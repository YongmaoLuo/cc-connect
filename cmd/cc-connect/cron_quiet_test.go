package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCronQuietCLI_EditIntervalIsInt pins the JSON type of the
// quiet_interval_mins edit value. The server's cron-edit validation
// (core/cron.go, UpdateJob) type-switches the decoded value and rejects
// anything that isn't a JSON number with "quiet_interval_mins must be a
// number", so shipping {"value":"30"} would make the field uneditable.
// Same class of bug as the old `silent` regression — see cron_edit_test.go.
func TestCronQuietCLI_EditIntervalIsInt(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		want      any
		wantError bool
	}{
		{"positive", "30", 30, false},
		{"zero disables", "0", 0, false},
		{"non-integer", "x", nil, true},
		{"float", "30.5", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCronEditValue("quiet_interval_mins", tt.input)
			if tt.wantError {
				if err == nil {
					t.Fatalf("parseCronEditValue(%q) = (%v, nil), want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCronEditValue(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseCronEditValue(%q) = %v (%T), want %v (%T)",
					tt.input, got, got, tt.want, tt.want)
			}
		})
	}
}

// TestCronQuietCLI_EditStringFields pins that quiet_hours / warm_prompt are
// forwarded verbatim. quiet_hours in particular is re-parsed by
// ParseQuietHours on the server, so any trimming or normalisation here would
// change which side of midnight the window is anchored on.
func TestCronQuietCLI_EditStringFields(t *testing.T) {
	tests := []struct {
		field string
		input string
	}{
		{"quiet_hours", "23:00-08:00"},
		{"quiet_hours", "08:00-08:00"},
		{"warm_prompt", "Reply OK only."},
		{"warm_prompt", "只回复 OK，不要调用工具"},
	}

	for _, tt := range tests {
		t.Run(tt.field+"/"+tt.input, func(t *testing.T) {
			got, err := parseCronEditValue(tt.field, tt.input)
			if err != nil {
				t.Fatalf("parseCronEditValue(%q, %q) unexpected error: %v", tt.field, tt.input, err)
			}
			if got != tt.input {
				t.Errorf("parseCronEditValue(%q, %q) = %v (%T), want the input verbatim",
					tt.field, tt.input, got, got)
			}
		})
	}
}

// TestCronQuietCLI_UsageDocumentsFlags is a help-text guard: both usage
// screens have to advertise the three quiet-hours knobs and show an example,
// otherwise the feature is only discoverable by reading the source.
func TestCronQuietCLI_UsageDocumentsFlags(t *testing.T) {
	addHelp := captureCronCLIStdout(t, printCronAddUsage)
	for _, want := range []string{
		"--quiet-hours",
		"--quiet-interval-mins",
		"--warm-prompt",
		`cc-connect cron add --cron "*/20 * * * *" --prompt "监控" --quiet-hours 23:00-08:00 --quiet-interval-mins 60`,
	} {
		if !strings.Contains(addHelp, want) {
			t.Errorf("printCronAddUsage does not document %q; got:\n%s", want, addHelp)
		}
	}

	editHelp := captureCronCLIStdout(t, printCronEditUsage)
	for _, want := range []string{
		"quiet_hours",
		"quiet_interval_mins",
		"warm_prompt",
		`cc-connect cron edit abc123 quiet_hours "23:00-08:00"`,
		"cc-connect cron edit abc123 quiet_interval_mins 60",
	} {
		if !strings.Contains(editHelp, want) {
			t.Errorf("printCronEditUsage does not document %q; got:\n%s", want, editHelp)
		}
	}
}

// ── `cron add` flag → /cron/add body ────────────────────────────
// runCronAdd calls os.Exit on bad input and writes to a unix socket, so
// both the happy path and the bad-input path are exercised by re-executing
// the test binary against a fake API socket.

const (
	cronCLIHelperEnv = "CC_CONNECT_CRON_CLI_HELPER"
	cronCLIArgsEnv   = "CC_CONNECT_CRON_CLI_ARGS"
)

// TestCronQuietCLI_HelperProcess is not a real test: it is the entry point the
// parent test re-execs. It calls runCronAdd for real, so os.Exit(1) on bad
// input is observable as the child process's exit code.
func TestCronQuietCLI_HelperProcess(t *testing.T) {
	if os.Getenv(cronCLIHelperEnv) != "1" {
		t.Skip("helper process — driven by TestCronQuietCLI_AddFlagsBody")
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(cronCLIArgsEnv)), &args); err != nil {
		t.Fatalf("decode %s: %v", cronCLIArgsEnv, err)
	}
	runCronAdd(args)
}

// TestCronQuietCLI_AddFlagsBody pins the wire contract of the three new add
// flags: with them set the body carries quiet_hours / quiet_interval_mins /
// warm_prompt, and without them the keys are absent entirely (zero-value
// compatibility — old jobs.json and old daemons must keep working).
func TestCronQuietCLI_AddFlagsBody(t *testing.T) {
	tests := []struct {
		name     string
		flags    []string
		wantKeys map[string]any
		absent   []string
	}{
		{
			name:  "all three flags",
			flags: []string{"--quiet-hours", "23:00-08:00", "--quiet-interval-mins", "60", "--warm-prompt", "Reply OK only."},
			wantKeys: map[string]any{
				"quiet_hours":         "23:00-08:00",
				"quiet_interval_mins": float64(60),
				"warm_prompt":         "Reply OK only.",
			},
		},
		{
			name:     "no quiet flags",
			flags:    nil,
			absent:   []string{"quiet_hours", "quiet_interval_mins", "warm_prompt"},
			wantKeys: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, bodyPath := startFakeCronAPI(t)

			args := append([]string{
				"--cron", "*/20 * * * *",
				"--session-key", "feishu:1",
				"--prompt", "监控",
				"--data-dir", dir,
			}, tt.flags...)
			code, stdout, stderr := runCronAddSubprocess(t, args)
			if code != 0 {
				t.Fatalf("runCronAdd exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}

			raw, err := os.ReadFile(bodyPath)
			if err != nil {
				t.Fatalf("read captured body: %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("captured body is not JSON: %v\n%s", err, raw)
			}
			for _, key := range tt.absent {
				if v, ok := body[key]; ok {
					t.Errorf("body must omit %q when the flag is absent, got %#v", key, v)
				}
			}
			for key, want := range tt.wantKeys {
				got, ok := body[key]
				if !ok {
					t.Errorf("body missing %q; got keys %v", key, keysOf(body))
					continue
				}
				if got != want {
					t.Errorf("body[%q] = %#v (%T), want %#v (%T)", key, got, got, want, want)
				}
			}
		})
	}
}

// TestCronQuietCLI_AddBadIntervalExits pins the CLI-side guard: a
// non-integer --quiet-interval-mins must fail loudly before anything is sent
// to the daemon, with exit code 1 and a message naming the flag.
func TestCronQuietCLI_AddBadIntervalExits(t *testing.T) {
	dir, bodyPath := startFakeCronAPI(t)

	code, _, stderr := runCronAddSubprocess(t, []string{
		"--cron", "*/20 * * * *",
		"--session-key", "feishu:1",
		"--prompt", "监控",
		"--data-dir", dir,
		"--quiet-hours", "23:00-08:00",
		"--quiet-interval-mins", "x",
	})

	if code != 1 {
		t.Fatalf("exit code = %d, want 1\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "--quiet-interval-mins") {
		t.Errorf("stderr does not name the offending flag; got: %s", stderr)
	}
	if _, err := os.Stat(bodyPath); err == nil {
		t.Errorf("a request was sent to the daemon despite the invalid flag")
	}
}

// ── helpers ─────────────────────────────────────────────────────

func captureCronCLIStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
	return string(out)
}

// startFakeCronAPI serves one /cron/add request on a unix socket laid out the
// way resolveSocketPath expects (<data-dir>/run/api.sock) and records the
// request body at <data-dir>/request.json. Returns (dataDir, bodyPath).
func startFakeCronAPI(t *testing.T) (dataDir, bodyPath string) {
	t.Helper()
	// macOS caps sun_path at ~104 bytes, and its TMPDIR is already ~50
	// characters — t.TempDir()'s "<base>/TestName<sub>/001/run/api.sock"
	// blows past it with "bind: invalid argument". Fall back to /tmp so the
	// socket path stays short.
	base := os.TempDir()
	if runtime.GOOS != "windows" && len(base) > 40 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "ccq")
	if err != nil {
		t.Fatalf("mkdir temp data dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	runDir := filepath.Join(dir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run dir: %v", err)
	}
	bodyPath = filepath.Join(dir, "request.json")

	ln, err := net.Listen("unix", filepath.Join(runDir, "api.sock"))
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if err := os.WriteFile(bodyPath, raw, 0o644); err != nil {
			t.Errorf("record request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"job1","cron_expr":"*/20 * * * *","prompt":"p"}`)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return dir, bodyPath
}

func runCronAddSubprocess(t *testing.T, args []string) (code int, stdout, stderr string) {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encode args: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestCronQuietCLI_HelperProcess", "-test.v=false")
	cmd.Env = append(os.Environ(),
		cronCLIHelperEnv+"=1",
		cronCLIArgsEnv+"="+string(encoded),
	)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	stdout, stderr = outBuf.String(), errBuf.String()

	switch e := err.(type) {
	case nil:
		return 0, stdout, stderr
	case *exec.ExitError:
		return e.ExitCode(), stdout, stderr
	default:
		t.Fatalf("run helper process: %v\nstderr: %s", err, stderr)
		return 0, stdout, stderr
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
