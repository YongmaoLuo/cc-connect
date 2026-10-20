//go:build webui_e2e

// Package e2e contains the browser-driven fixture for the cron quiet-hours
// web UI acceptance run.
//
// The fixture is not an assertion suite on its own. It boots a real
// ManagementServer that serves the built web/dist assets and the management
// API from the same origin (the web client calls the API with relative
// paths, so both have to live behind one listener), plus a CronScheduler
// backed by a throwaway store. It then hands the port and the token to the
// Playwright driver through files in a well-known directory and stays alive
// until the driver asks it to stop.
//
// Usage:
//
//	go test -tags=webui_e2e ./tests/e2e -run '^TestCronQuietWebUIFixture$' -timeout 15m
//
// The run ends when a "stop" file appears in the fixture directory, or after
// CC_CRON_E2E_MAX_WAIT (default 12m), whichever comes first.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const (
	// cronQuietWebUIDirEnv overrides the directory used for the handshake
	// files. The default is the path the verification doc refers to.
	cronQuietWebUIDirEnv = "CC_CRON_E2E_DIR"
	// cronQuietWebUIDefaultDir is where the port/token handshake lives.
	cronQuietWebUIDefaultDir = "/tmp/cc-connect-cron-e2e"
	// cronQuietWebUIToken is the management API token the driver logs in with.
	cronQuietWebUIToken = "e2e-token"
	// cronQuietWebUIProject is the single project registered with the engine.
	// The API resolves an empty project to the only registered engine, which
	// is what the web form does when the user leaves the select untouched.
	cronQuietWebUIProject = "e2e-project"
	// cronQuietWebUISessionKey is the session the fixture seeds. The cron API
	// rejects a job without a session key, and the web form can only offer the
	// sessions that the project already has, so the fixture has to create one
	// before the driver opens the form.
	cronQuietWebUISessionKey = "e2e:channel:user"
	// cronQuietWebUIMaxWaitEnv caps how long the fixture waits for the stop
	// file before shutting itself down.
	cronQuietWebUIMaxWaitEnv = "CC_CRON_E2E_MAX_WAIT"
)

// cronQuietWebUIPorts are the candidate ports, in preference order. When the
// first one is taken the fixture falls back to the next and records the port
// it actually bound in the handshake file.
var cronQuietWebUIPorts = []int{19820, 19821}

// cronQuietWebUIAgent is a minimal core.Agent. The fixture never runs a job:
// it only needs a project to exist so the web UI can list it and so the cron
// API can resolve a project name.
type cronQuietWebUIAgent struct{}

func (a *cronQuietWebUIAgent) Name() string { return "e2e-stub" }

func (a *cronQuietWebUIAgent) StartSession(ctx context.Context, sessionID string) (core.AgentSession, error) {
	return nil, fmt.Errorf("e2e stub agent cannot start sessions")
}

func (a *cronQuietWebUIAgent) ListSessions(ctx context.Context) ([]core.AgentSessionInfo, error) {
	return nil, nil
}

func (a *cronQuietWebUIAgent) Stop() error { return nil }

// cronQuietWebUIDir returns the directory holding the handshake files.
func cronQuietWebUIDir() string {
	if d := os.Getenv(cronQuietWebUIDirEnv); d != "" {
		return d
	}
	return cronQuietWebUIDefaultDir
}

// cronQuietWebUIMaxWait returns how long the fixture stays alive waiting for
// the stop file.
func cronQuietWebUIMaxWait() time.Duration {
	if v := os.Getenv(cronQuietWebUIMaxWaitEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 12 * time.Minute
}

// cronQuietWebUIDist locates the built web/dist directory by walking up from
// the working directory (the package directory under `go test`).
func cronQuietWebUIDist(t *testing.T) string {
	t.Helper()
	start, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := start
	for {
		candidate := filepath.Join(dir, "web", "dist")
		if _, err := os.Stat(filepath.Join(candidate, "index.html")); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("web/dist/index.html not found at or above %s; run `pnpm build` in web/ first", start)
	return ""
}

// cronQuietWebUIPortFree reports whether the port can be bound right now.
func cronQuietWebUIPortFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// cronQuietWebUIProbe reports whether the management API answers on the port.
func cronQuietWebUIProbe(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/status?token=%s", port, cronQuietWebUIToken)
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// cronQuietWebUIStartServer boots the management server on the first port
// that both accepts a bind and comes up. It fails the test if none does.
// The configure hook runs before the listener opens, so the caller can wire
// the engine and the scheduler into the server.
func cronQuietWebUIStartServer(t *testing.T, configure func(*core.ManagementServer)) (int, *core.ManagementServer) {
	t.Helper()
	for _, port := range cronQuietWebUIPorts {
		if !cronQuietWebUIPortFree(port) {
			t.Logf("cron quiet-hours fixture: port %d is busy, trying the next one", port)
			continue
		}
		mgmt := core.NewManagementServer(port, cronQuietWebUIToken, nil)
		configure(mgmt)
		mgmt.Start()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if cronQuietWebUIProbe(port) {
				return port, mgmt
			}
			time.Sleep(150 * time.Millisecond)
		}
		mgmt.Stop()
		t.Logf("cron quiet-hours fixture: port %d did not become ready, trying the next one", port)
	}
	t.Fatalf("cron quiet-hours fixture: none of the ports %v could be bound", cronQuietWebUIPorts)
	return 0, nil
}

// cronQuietWebUIGet fetches a management API endpoint and returns the
// "data" object of the response envelope.
func cronQuietWebUIGet(port int, path string) (map[string]any, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/%s?token=%s", port, path, cronQuietWebUIToken)
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/v1/%s: status %d", path, resp.StatusCode)
	}
	var payload struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if !payload.OK {
		return nil, fmt.Errorf("GET /api/v1/%s returned ok=false", path)
	}
	return payload.Data, nil
}

// cronQuietWebUIListJobs reads the cron list through the management API, the
// same way the web client does. It doubles as the fixture self-check: the
// driver may only start once this answers.
func cronQuietWebUIListJobs(port int) ([]any, error) {
	data, err := cronQuietWebUIGet(port, "cron")
	if err != nil {
		return nil, err
	}
	jobs, _ := data["jobs"].([]any)
	return jobs, nil
}

// cronQuietWebUIListProjects reads the project list through the management
// API. The web form offers the projects in a select, and the cron API needs
// exactly one registered engine to resolve an empty project name.
func cronQuietWebUIListProjects(port int) ([]any, error) {
	data, err := cronQuietWebUIGet(port, "projects")
	if err != nil {
		return nil, err
	}
	projects, _ := data["projects"].([]any)
	return projects, nil
}

// cronQuietWebUIAddJob posts a cron job through the management API, the same
// way the web form does, and returns the created job.
func cronQuietWebUIAddJob(port int, body map[string]any) (map[string]any, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/cron?token=%s", port, cronQuietWebUIToken)
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("POST /api/v1/cron: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode created job: %w", err)
	}
	if !envelope.OK {
		return nil, fmt.Errorf("POST /api/v1/cron returned ok=false")
	}
	return envelope.Data, nil
}

// cronQuietWebUICreateSession seeds a session for the project. The web form
// offers the project's sessions in the session-key select, and the cron API
// rejects a job whose session key is empty, so the fixture needs at least
// one session to exist before the driver opens the form.
func cronQuietWebUICreateSession(port int, project, sessionKey string) error {
	payload, err := json.Marshal(map[string]any{"session_key": sessionKey})
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/projects/%s/sessions?token=%s", port, project, cronQuietWebUIToken)
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST /api/v1/projects/%s/sessions: status %d: %s", project, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// cronQuietWebUIListSessions reads the sessions of a project, the same way
// the web form's session-key select does.
func cronQuietWebUIListSessions(port int, project string) ([]any, error) {
	data, err := cronQuietWebUIGet(port, "projects/"+project+"/sessions")
	if err != nil {
		return nil, err
	}
	sessions, _ := data["sessions"].([]any)
	return sessions, nil
}

// cronQuietWebUIDeleteJob removes a cron job through the management API.
func cronQuietWebUIDeleteJob(port int, id string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/cron/%s?token=%s", port, id, cronQuietWebUIToken)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("DELETE /api/v1/cron/%s: status %d", id, resp.StatusCode)
	}
	return nil
}

// TestCronQuietWebUIFixture starts the browser fixture for the cron
// quiet-hours web UI acceptance run. The assertions themselves are performed
// by the Playwright driver against the live server; this test owns the
// server lifecycle and the handshake.
func TestCronQuietWebUIFixture(t *testing.T) {
	dir := cronQuietWebUIDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create fixture dir: %v", err)
	}
	// Drop stale handshake files so the driver can never read a port or a
	// stop flag left over from an earlier run.
	for _, name := range []string{"port", "token", "stop"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove stale %s: %v", name, err)
		}
	}

	// Serve the built frontend from disk so a fresh `pnpm build` is picked up
	// without recompiling the Go binary.
	dist := cronQuietWebUIDist(t)
	core.RegisterWebAssets(os.DirFS(dist))
	t.Logf("cron quiet-hours fixture: serving web assets from %s", dist)

	store, err := core.NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("create cron store: %v", err)
	}
	engine := core.NewEngine(cronQuietWebUIProject, &cronQuietWebUIAgent{}, nil, "", core.LangEnglish)
	scheduler := core.NewCronScheduler(store)
	scheduler.RegisterEngine(cronQuietWebUIProject, engine)
	if err := scheduler.Start(); err != nil {
		t.Fatalf("start cron scheduler: %v", err)
	}
	defer scheduler.Stop()

	port, mgmt := cronQuietWebUIStartServer(t, func(m *core.ManagementServer) {
		m.SetCronScheduler(scheduler)
		m.RegisterEngine(cronQuietWebUIProject, engine)
	})
	defer mgmt.Stop()

	// Self-check: the endpoints the driver relies on must already answer.
	jobs, err := cronQuietWebUIListJobs(port)
	if err != nil {
		t.Fatalf("cron list self-check failed: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("cron list self-check: got %d jobs, want 0 in a fresh store", len(jobs))
	}
	// The web form's project select is fed by /api/v1/projects, and the cron
	// API resolves an empty project to the single registered engine. Both
	// need the engine to be registered with the management server.
	projects, err := cronQuietWebUIListProjects(port)
	if err != nil {
		t.Fatalf("project list self-check failed: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("project list self-check: got %d projects, want exactly 1", len(projects))
	}
	first, _ := projects[0].(map[string]any)
	if name, _ := first["name"].(string); name != cronQuietWebUIProject {
		t.Fatalf("project list self-check: project name = %q, want %q", name, cronQuietWebUIProject)
	}

	// The browser is about to create a job with quiet hours through the same
	// endpoint. Prove the path works before handing over, so a wiring fault
	// surfaces here instead of as an unexplained UI timeout.
	if err := cronQuietWebUICreateSession(port, cronQuietWebUIProject, cronQuietWebUISessionKey); err != nil {
		t.Fatalf("session seed self-check failed: %v", err)
	}
	sessions, err := cronQuietWebUIListSessions(port, cronQuietWebUIProject)
	if err != nil {
		t.Fatalf("session list self-check failed: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatalf("session list self-check: got 0 sessions, want at least 1")
	}
	probe, err := cronQuietWebUIAddJob(port, map[string]any{
		"cron_expr":           "0 9 * * *",
		"prompt":              "self-check",
		"description":         "fixture self-check",
		"session_key":         cronQuietWebUISessionKey,
		"quiet_hours":         "23:00-08:00",
		"quiet_interval_mins": 60,
	})
	if err != nil {
		t.Fatalf("job create self-check failed: %v", err)
	}
	probeID, _ := probe["id"].(string)
	if probeID == "" {
		t.Fatalf("job create self-check: no id in %v", probe)
	}
	if hours, _ := probe["quiet_hours"].(string); hours != "23:00-08:00" {
		t.Fatalf("job create self-check: quiet_hours = %q, want %q", hours, "23:00-08:00")
	}
	if err := cronQuietWebUIDeleteJob(port, probeID); err != nil {
		t.Fatalf("job delete self-check failed: %v", err)
	}

	// Handshake: the driver polls for these two files.
	if err := os.WriteFile(filepath.Join(dir, "port"), []byte(strconv.Itoa(port)), 0o644); err != nil {
		t.Fatalf("write port file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte(cronQuietWebUIToken), 0o644); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	t.Logf("cron quiet-hours fixture: ready on port %d (token %q), handshake in %s", port, cronQuietWebUIToken, dir)

	// Stay alive until the driver drops the stop file.
	stopFile := filepath.Join(dir, "stop")
	deadline := time.Now().Add(cronQuietWebUIMaxWait())
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopFile); err == nil {
			t.Log("cron quiet-hours fixture: stop file seen, shutting down")
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Log("cron quiet-hours fixture: max wait reached, shutting down")
}
