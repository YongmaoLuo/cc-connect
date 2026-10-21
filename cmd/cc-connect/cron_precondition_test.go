package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// preconditionSampleCmd is the command used throughout these tests. It mixes
// single quotes, double quotes, a shell substitution and an environment
// variable, so it catches any re-quoting or trimming the CLI might do.
const preconditionSampleCmd = `test "$(stat -f %m /tmp/state.md)" -gt "$CC_CRON_LAST_RUN"`

// TestCronPreconditionCLIAdd pins the wire contract of the two new add
// flags: with them set the body carries precondition / precondition_timeout
// verbatim, and without them the keys are absent entirely — a job created by
// the new CLI must stay byte-identical to one created by an older CLI so old
// daemons and old jobs.json keep working.
func TestCronPreconditionCLIAdd(t *testing.T) {
	tests := []struct {
		name     string
		flags    []string
		wantKeys map[string]any
		absent   []string
	}{
		{
			name:  "both flags reach the daemon verbatim",
			flags: []string{"--precondition", preconditionSampleCmd, "--precondition-timeout", "30"},
			wantKeys: map[string]any{
				"precondition":         preconditionSampleCmd,
				"precondition_timeout": float64(30),
			},
		},
		{
			name:   "no precondition flags",
			flags:  nil,
			absent: []string{"precondition", "precondition_timeout"},
		},
		{
			name:   "empty precondition is not sent",
			flags:  []string{"--precondition", ""},
			absent: []string{"precondition"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, bodyPath := startFakeCronAPI(t)

			args := append([]string{
				"--cron", "*/20 * * * *",
				"--project", "demo",
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

// TestCronPreconditionCLIAddBodyUnchanged pins the no-flag request body to
// the exact bytes an older CLI produced. The precondition keys are omitted
// rather than sent as zero values, so a job created without the flags stays
// byte-identical to one created before the feature existed.
func TestCronPreconditionCLIAddBodyUnchanged(t *testing.T) {
	dir, bodyPath := startFakeCronAPI(t)

	code, stdout, stderr := runCronAddSubprocess(t, []string{
		"--cron", "*/20 * * * *",
		"--project", "demo",
		"--session-key", "feishu:1",
		"--prompt", "监控",
		"--data-dir", dir,
	})
	if code != 0 {
		t.Fatalf("runCronAdd exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	raw, err := os.ReadFile(bodyPath)
	if err != nil {
		t.Fatalf("read captured body: %v", err)
	}
	wantRaw, err := json.Marshal(map[string]any{
		"project":     "demo",
		"session_key": "feishu:1",
		"cron_expr":   "*/20 * * * *",
		"prompt":      "监控",
		"exec":        "",
		"description": "",
	})
	if err != nil {
		t.Fatalf("marshal expected body: %v", err)
	}
	if string(raw) != string(wantRaw) {
		t.Errorf("body without the precondition flags is not byte-identical to the old CLI\n got: %s\nwant: %s", raw, wantRaw)
	}
}

// TestCronPreconditionCLIAddInvalidTimeout pins the CLI-side guard: a
// non-integer --precondition-timeout must fail loudly before anything is sent
// to the daemon, with exit code 1 and a message naming the flag.
func TestCronPreconditionCLIAddInvalidTimeout(t *testing.T) {
	dir, bodyPath := startFakeCronAPI(t)

	code, _, stderr := runCronAddSubprocess(t, []string{
		"--cron", "*/20 * * * *",
		"--session-key", "feishu:1",
		"--prompt", "监控",
		"--data-dir", dir,
		"--precondition", preconditionSampleCmd,
		"--precondition-timeout", "x",
	})

	if code != 1 {
		t.Fatalf("exit code = %d, want 1\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "--precondition-timeout") {
		t.Errorf("stderr does not name the offending flag; got: %s", stderr)
	}
	if _, err := os.Stat(bodyPath); err == nil {
		t.Errorf("a request was sent to the daemon despite the invalid flag")
	}
}

// TestCronPreconditionCLIEdit pins the wire types of the two fields
// `cron edit` accepts. The server's UpdateJob (core/cron.go) type-switches the
// decoded value and only accepts a JSON number for precondition_timeout — a
// string falls through to the reflection fallback, which cannot set an int
// field, and returns "unknown or invalid field: precondition_timeout".
// precondition is the mirror case: it must stay a verbatim string, since the
// command runs through a shell and any trimming or re-quoting here would change
// what the daemon executes.
func TestCronPreconditionCLIEdit(t *testing.T) {
	t.Run("precondition_timeout is sent as a number", func(t *testing.T) {
		tests := []struct {
			name      string
			input     string
			want      any
			wantError bool
		}{
			{"positive", "30", 30, false},
			{"zero uses the default", "0", 0, false},
			{"non-integer", "x", nil, true},
			{"float", "30.5", nil, true},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, err := parseCronEditValue("precondition_timeout", tt.input)
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
	})

	t.Run("precondition is forwarded verbatim", func(t *testing.T) {
		// Leading/trailing spaces and both quote styles must survive: the
		// command is executed by a shell, so "exit 1 " != "exit 1" in
		// practice and `test "$(...)"` depends on its inner double quotes.
		for _, input := range []string{
			"exit 1",
			"  exit 1  ",
			preconditionSampleCmd,
			`echo 'single' "double" | wc -c`,
		} {
			got, err := parseCronEditValue("precondition", input)
			if err != nil {
				t.Fatalf("parseCronEditValue(%q, %q) unexpected error: %v", "precondition", input, err)
			}
			gotStr, ok := got.(string)
			if !ok {
				t.Fatalf("parseCronEditValue(%q, %q) = %v (%T), want a string", "precondition", input, got, got)
			}
			if gotStr != input {
				t.Errorf("parseCronEditValue(%q, %q) = %q, want the input verbatim", "precondition", input, gotStr)
			}
		}
	})

	t.Run("other fields keep their existing types", func(t *testing.T) {
		cases := []struct {
			field string
			input string
			want  any
		}{
			{"timeout_mins", "60", 60},
			{"quiet_interval_mins", "60", 60},
			{"enabled", "false", false},
			{"silent", "true", true},
			{"cron_expr", "0 9 * * *", "0 9 * * *"},
			{"quiet_hours", "23:00-08:00", "23:00-08:00"},
		}
		for _, c := range cases {
			got, err := parseCronEditValue(c.field, c.input)
			if err != nil {
				t.Fatalf("parseCronEditValue(%q, %q) unexpected error: %v", c.field, c.input, err)
			}
			if got != c.want {
				t.Errorf("parseCronEditValue(%q, %q) = %v (%T), want %v (%T)",
					c.field, c.input, got, got, c.want, c.want)
			}
		}
	})

	t.Run("edit usage documents both fields", func(t *testing.T) {
		help := captureCronCLIStdout(t, printCronEditUsage)
		for _, want := range []string{
			"precondition",
			"precondition_timeout",
			"cc-connect cron edit abc123 precondition_timeout 30",
		} {
			if !strings.Contains(help, want) {
				t.Errorf("printCronEditUsage does not document %q; got:\n%s", want, help)
			}
		}
	})
}

// TestCronPreconditionCLIUsage is a help-text guard: the add usage screen has
// to advertise both flags and show the example, otherwise the feature is only
// discoverable by reading the source.
func TestCronPreconditionCLIUsage(t *testing.T) {
	help := captureCronCLIStdout(t, printCronAddUsage)
	for _, want := range []string{
		"--precondition <cmd>",
		"--precondition-timeout <n>",
		`--precondition 'test "$(stat -f %m /tmp/state.md)" -gt "$CC_CRON_LAST_RUN"'`,
	} {
		if !strings.Contains(help, want) {
			t.Errorf("printCronAddUsage does not document %q; got:\n%s", want, help)
		}
	}
}
