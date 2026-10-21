//go:build webui_e2e

// Package e2e contains the E2E fixture for the cron precondition gate.
//
// The core unit tests already cover the precondition pass-through of the
// management API, but they live inside the core package and call the handler
// directly. This file proves the same thing from the outside: a real HTTP
// server on a real port, a real cron store, and the values as an API client
// sees them — create, read back, patch, read back again, and compare every
// field against what the store persisted.
//
// The store is a throwaway directory (t.TempDir), so the run never reads or
// writes ~/.cc-connect, and no daemon or production binary is started. The
// only process involved is this test binary.
//
// Usage:
//
//	go test -tags=webui_e2e ./tests/e2e -run '^TestCronPreconditionE2E$' -v
//
// The -run filter matters: the sibling file cron_quiet_webui_fixture_test.go
// holds a browser fixture that stays alive until a driver drops a stop file
// (or 12 minutes elapse), so running the whole package is not a way to run
// this test.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const (
	// cronPreconditionCommand is the gate the job carries. It is never
	// executed by this test — the job's schedule cannot fire during the run
	// — it only has to survive the round trip through the API and the store.
	cronPreconditionCommand = "exit 0"
	// cronPreconditionTimeout is the timeout the job is created with.
	cronPreconditionTimeout = 15
	// cronPreconditionPatchedTimeout is the timeout the PATCH writes.
	cronPreconditionPatchedTimeout = 30
)

// cronPreconditionFields are the job fields the E2E compares field by field
// between the API response and the persisted job. Both sides are JSON, so the
// comparison is apples to apples.
var cronPreconditionFields = []string{
	"id",
	"project",
	"session_key",
	"cron_expr",
	"prompt",
	"description",
	"enabled",
	"precondition",
	"precondition_timeout",
}

// cronPreconditionPatchJob PATCHes a cron job through the management API and
// returns the "data" object of the response envelope.
func cronPreconditionPatchJob(port int, id string, body map[string]any) (map[string]any, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/v1/cron/%s?token=%s", port, id, cronQuietWebUIToken)
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PATCH /api/v1/cron/%s: status %d: %s", id, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var envelope struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode patched job: %w", err)
	}
	if !envelope.OK {
		return nil, fmt.Errorf("PATCH /api/v1/cron/%s returned ok=false", id)
	}
	return envelope.Data, nil
}

// cronPreconditionJobFromList reads the cron list through the management API
// and returns the job with the given id. It is the read-back path: the
// management API has no GET for a single job, so the client reads the list
// and picks its own job out of it — the same way the web UI does.
func cronPreconditionJobFromList(port int, id string) (map[string]any, error) {
	data, err := cronQuietWebUIGet(port, "cron")
	if err != nil {
		return nil, err
	}
	jobs, _ := data["jobs"].([]any)
	for _, entry := range jobs {
		job, _ := entry.(map[string]any)
		if got, _ := job["id"].(string); got == id {
			return job, nil
		}
	}
	return nil, fmt.Errorf("job %s not found in the cron list", id)
}

// cronPreconditionInt reads an integer field out of a decoded JSON object.
// JSON numbers decode to float64, so both shapes are accepted.
func cronPreconditionInt(t *testing.T, obj map[string]any, field string) int {
	t.Helper()
	switch v := obj[field].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case nil:
		t.Fatalf("field %q missing from %v", field, obj)
	default:
		t.Fatalf("field %q = %v (%T), want a number", field, obj[field], obj[field])
	}
	return 0
}

// cronPreconditionPersistedJob reads the job with the given id straight out of
// the store's JSON file. The in-memory store is what the API reads from; this
// is what survived a save, which is the claim the E2E exists to prove.
func cronPreconditionPersistedJob(t *testing.T, dataDir, id string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dataDir, "crons", "jobs.json"))
	if err != nil {
		t.Fatalf("read persisted jobs: %v", err)
	}
	var jobs []map[string]any
	if err := json.Unmarshal(raw, &jobs); err != nil {
		t.Fatalf("decode persisted jobs: %v", err)
	}
	for _, job := range jobs {
		if got, _ := job["id"].(string); got == id {
			return job
		}
	}
	t.Fatalf("job %s not found in the persisted file", id)
	return nil
}

// TestCronPreconditionE2E drives the precondition fields of a cron job through
// the management API and checks that they survive the whole round trip:
//
//   - POST /api/v1/cron creates the job with a precondition command and a
//     timeout, and both come back;
//   - reading the job back shows the same values;
//   - PATCH /api/v1/cron/{id} changes the timeout and leaves the command
//     alone;
//   - reading the job back again shows the patched timeout;
//   - the store and the API agree on every field;
//   - the values are on disk, not just in memory.
func TestCronPreconditionE2E(t *testing.T) {
	// A throwaway store: the run must never touch ~/.cc-connect.
	dataDir := t.TempDir()
	store, err := core.NewCronStore(dataDir)
	if err != nil {
		t.Fatalf("create cron store: %v", err)
	}
	engine := core.NewEngine(cronQuietWebUIProject, &cronQuietWebUIAgent{}, nil, "", core.LangEnglish)
	scheduler := core.NewCronScheduler(store)
	scheduler.RegisterEngine(cronQuietWebUIProject, engine)
	if err := scheduler.Start(); err != nil {
		t.Fatalf("start cron scheduler: %v", err)
	}
	// Stop reclaims the scheduler's run loop; without it the goroutine would
	// outlive the test.
	defer scheduler.Stop()

	port, mgmt := cronQuietWebUIStartServer(t, func(m *core.ManagementServer) {
		m.SetCronScheduler(scheduler)
		m.RegisterEngine(cronQuietWebUIProject, engine)
	})
	// Stop closes the HTTP server; without it the listener would outlive the
	// test and hold the port.
	defer mgmt.Stop()

	// Create the job with both precondition fields. The schedule is one that
	// never comes round (1 January, 03:00), so the scheduler cannot fire the
	// job while the test runs — the stub agent would fail to start a session
	// and the run would depend on the wall clock.
	created, err := cronQuietWebUIAddJob(port, map[string]any{
		"cron_expr":            "0 3 1 1 *",
		"prompt":               "precondition e2e",
		"project":              cronQuietWebUIProject,
		"session_key":          cronQuietWebUISessionKey,
		"precondition":         cronPreconditionCommand,
		"precondition_timeout": cronPreconditionTimeout,
	})
	if err != nil {
		t.Fatalf("create cron job: %v", err)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("created job has no id: %v", created)
	}
	if got, _ := created["precondition"].(string); got != cronPreconditionCommand {
		t.Errorf("created precondition = %q, want %q", got, cronPreconditionCommand)
	}
	if got := cronPreconditionInt(t, created, "precondition_timeout"); got != cronPreconditionTimeout {
		t.Errorf("created precondition_timeout = %d, want %d", got, cronPreconditionTimeout)
	}

	// Read the job back: the values must still be there.
	readBack, err := cronPreconditionJobFromList(port, id)
	if err != nil {
		t.Fatalf("read the job back: %v", err)
	}
	if got, _ := readBack["precondition"].(string); got != cronPreconditionCommand {
		t.Errorf("read-back precondition = %q, want %q", got, cronPreconditionCommand)
	}
	if got := cronPreconditionInt(t, readBack, "precondition_timeout"); got != cronPreconditionTimeout {
		t.Errorf("read-back precondition_timeout = %d, want %d", got, cronPreconditionTimeout)
	}

	// Patch the timeout only: the command keeps its value.
	patched, err := cronPreconditionPatchJob(port, id, map[string]any{
		"precondition_timeout": cronPreconditionPatchedTimeout,
	})
	if err != nil {
		t.Fatalf("patch cron job: %v", err)
	}
	if got := cronPreconditionInt(t, patched, "precondition_timeout"); got != cronPreconditionPatchedTimeout {
		t.Errorf("patched precondition_timeout = %d, want %d", got, cronPreconditionPatchedTimeout)
	}
	if got, _ := patched["precondition"].(string); got != cronPreconditionCommand {
		t.Errorf("patched precondition = %q, want %q (unchanged)", got, cronPreconditionCommand)
	}

	// Read the patched job back through a second, independent request.
	afterPatch, err := cronPreconditionJobFromList(port, id)
	if err != nil {
		t.Fatalf("read the patched job back: %v", err)
	}
	if got := cronPreconditionInt(t, afterPatch, "precondition_timeout"); got != cronPreconditionPatchedTimeout {
		t.Errorf("read-back precondition_timeout = %d, want %d", got, cronPreconditionPatchedTimeout)
	}
	if got, _ := afterPatch["precondition"].(string); got != cronPreconditionCommand {
		t.Errorf("read-back precondition = %q, want %q (unchanged)", got, cronPreconditionCommand)
	}

	// The store and the API must agree on every field.
	stored := store.Get(id)
	if stored == nil {
		t.Fatal("job not found in the store")
	}
	storeJob, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal stored job: %v", err)
	}
	var storeMap map[string]any
	if err := json.Unmarshal(storeJob, &storeMap); err != nil {
		t.Fatalf("decode stored job: %v", err)
	}
	for _, field := range cronPreconditionFields {
		apiValue, apiOK := afterPatch[field]
		storeValue, storeOK := storeMap[field]
		if apiOK != storeOK {
			t.Errorf("field %q: API presence = %v, store presence = %v", field, apiOK, storeOK)
			continue
		}
		if !reflect.DeepEqual(apiValue, storeValue) {
			t.Errorf("field %q: API = %v, store = %v", field, apiValue, storeValue)
		}
	}
	if got := stored.PreconditionTimeout; got != cronPreconditionPatchedTimeout {
		t.Errorf("store precondition_timeout = %d, want %d", got, cronPreconditionPatchedTimeout)
	}
	if got := stored.Precondition; got != cronPreconditionCommand {
		t.Errorf("store precondition = %q, want %q", got, cronPreconditionCommand)
	}

	// And the values must be on disk, not only in memory.
	persisted := cronPreconditionPersistedJob(t, dataDir, id)
	if got, _ := persisted["precondition"].(string); got != cronPreconditionCommand {
		t.Errorf("persisted precondition = %q, want %q", got, cronPreconditionCommand)
	}
	if got := cronPreconditionInt(t, persisted, "precondition_timeout"); got != cronPreconditionPatchedTimeout {
		t.Errorf("persisted precondition_timeout = %d, want %d", got, cronPreconditionPatchedTimeout)
	}
}
