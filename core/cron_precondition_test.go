package core

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCronPreconditionValidation pins the bounds validateCronJob enforces on
// the precondition fields: the timeout window and the command length cap.
// A timeout > 0 with an empty command is deliberately legal — it is a no-op,
// and rejecting it would make the two fields impossible to set independently.
func TestCronPreconditionValidation(t *testing.T) {
	cases := []struct {
		name    string
		job     CronJob
		wantErr bool
	}{
		{
			name:    "negative timeout",
			job:     CronJob{Precondition: "true", PreconditionTimeout: -1},
			wantErr: true,
		},
		{
			name:    "zero timeout uses default",
			job:     CronJob{Precondition: "true", PreconditionTimeout: 0},
			wantErr: false,
		},
		{
			name:    "max timeout",
			job:     CronJob{Precondition: "true", PreconditionTimeout: 300},
			wantErr: false,
		},
		{
			name:    "timeout above max",
			job:     CronJob{Precondition: "true", PreconditionTimeout: 301},
			wantErr: true,
		},
		{
			name:    "command at length cap",
			job:     CronJob{Precondition: strings.Repeat("a", 4096)},
			wantErr: false,
		},
		{
			name:    "command above length cap",
			job:     CronJob{Precondition: strings.Repeat("a", 4097)},
			wantErr: true,
		},
		{
			name:    "timeout without command is a no-op",
			job:     CronJob{Precondition: "", PreconditionTimeout: 10},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := tc.job
			j.SessionKey = "test:1:1"
			err := validateCronJob(&j)
			if tc.wantErr && err == nil {
				t.Fatalf("validateCronJob() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("validateCronJob() = %v, want nil", err)
			}
		})
	}
}

// TestCronPreconditionLegacyJSON verifies that a jobs.json written before the
// precondition fields existed still loads: both fields fall back to their zero
// values and the job remains valid, so behavior is unchanged for existing jobs.
func TestCronPreconditionLegacyJSON(t *testing.T) {
	dir := t.TempDir()
	cronsDir := filepath.Join(dir, "crons")
	if err := os.MkdirAll(cronsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `[{"id":"legacy1","project":"p","session_key":"test:1:1","cron_expr":"0 6 * * *","prompt":"x","enabled":true}]`
	if err := os.WriteFile(filepath.Join(cronsDir, "jobs.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	j := store.Get("legacy1")
	if j == nil {
		t.Fatal("Get(legacy1) = nil, want the job loaded from jobs.json")
	}
	if j.Precondition != "" {
		t.Errorf("legacy JSON: Precondition = %q, want empty", j.Precondition)
	}
	if j.PreconditionTimeout != 0 {
		t.Errorf("legacy JSON: PreconditionTimeout = %d, want 0", j.PreconditionTimeout)
	}
	if err := validateCronJob(j); err != nil {
		t.Errorf("legacy JSON: validateCronJob() = %v, want nil", err)
	}
}

// TestCronPreconditionRun pins the exit-code semantics of the precondition
// gate: exit 0 lets the job run, exit 1-125 is the only path that skips it,
// and every other outcome (126/127, a kill by signal, a command that cannot
// start) fails open so a broken gate never silently blocks a job.
func TestCronPreconditionRun(t *testing.T) {
	now := time.Now()

	t.Run("exit 0 allows", func(t *testing.T) {
		job := &CronJob{Precondition: "true"}
		allow, code, out := runPrecondition(job, now)
		t.Logf("precondition %q -> allow=%v exitCode=%d output=%q", job.Precondition, allow, code, out)
		if !allow {
			t.Errorf("allow = false, want true")
		}
		if code != 0 {
			t.Errorf("exitCode = %d, want 0", code)
		}
	})

	t.Run("exit 3 skips", func(t *testing.T) {
		job := &CronJob{Precondition: "exit 3"}
		allow, code, out := runPrecondition(job, now)
		t.Logf("precondition %q -> allow=%v exitCode=%d output=%q", job.Precondition, allow, code, out)
		if allow {
			t.Errorf("allow = true, want false")
		}
		if code != 3 {
			t.Errorf("exitCode = %d, want 3", code)
		}
	})

	t.Run("timeout fails open", func(t *testing.T) {
		job := &CronJob{Precondition: "sleep 2", PreconditionTimeout: 1}
		start := time.Now()
		allow, code, out := runPrecondition(job, now)
		elapsed := time.Since(start)
		t.Logf("precondition %q (timeout=%ds) -> allow=%v exitCode=%d elapsed=%v output=%q",
			job.Precondition, job.PreconditionTimeout, allow, code, elapsed, out)
		if !allow {
			t.Errorf("allow = false, want true (timeout must fail open)")
		}
		if elapsed >= 2*time.Second {
			t.Errorf("elapsed = %v, want < 2s (the 1s timeout must cut the command short)", elapsed)
		}
	})

	t.Run("command not found fails open with 127", func(t *testing.T) {
		job := &CronJob{Precondition: "/nonexistent/x"}
		allow, code, out := runPrecondition(job, now)
		t.Logf("precondition %q -> allow=%v exitCode=%d output=%q", job.Precondition, allow, code, out)
		if !allow {
			t.Errorf("allow = false, want true")
		}
		if code != 127 {
			t.Errorf("exitCode = %d, want 127", code)
		}
	})

	t.Run("empty command is a no-op", func(t *testing.T) {
		job := &CronJob{Precondition: "   "}
		allow, code, out := runPrecondition(job, now)
		if !allow {
			t.Errorf("allow = false, want true")
		}
		if code != 0 {
			t.Errorf("exitCode = %d, want 0", code)
		}
		if out != "" {
			t.Errorf("output = %q, want empty", out)
		}
	})

	t.Run("nil job is a no-op", func(t *testing.T) {
		allow, code, out := runPrecondition(nil, now)
		if !allow {
			t.Errorf("allow = false, want true")
		}
		if code != 0 {
			t.Errorf("exitCode = %d, want 0", code)
		}
		if out != "" {
			t.Errorf("output = %q, want empty", out)
		}
	})

	t.Run("workdir is honored when set", func(t *testing.T) {
		dir := t.TempDir()
		job := &CronJob{Precondition: "pwd", WorkDir: dir}
		allow, code, out := runPrecondition(job, now)
		t.Logf("precondition %q (work_dir=%s) -> allow=%v exitCode=%d output=%q", job.Precondition, dir, allow, code, out)
		if !allow || code != 0 {
			t.Errorf("allow=%v exitCode=%d, want true/0", allow, code)
		}
		want, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("pwd = %q, want %q", got, want)
		}
	})

	t.Run("empty workdir inherits the current directory", func(t *testing.T) {
		job := &CronJob{Precondition: "pwd"}
		allow, code, out := runPrecondition(job, now)
		t.Logf("precondition %q (no work_dir) -> allow=%v exitCode=%d output=%q", job.Precondition, allow, code, out)
		if !allow || code != 0 {
			t.Errorf("allow=%v exitCode=%d, want true/0", allow, code)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		want, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("pwd = %q, want %q (the daemon's current directory)", got, want)
		}
	})
}

// TestCronPreconditionEnv pins the CC_CRON_* variables the executor injects
// into the precondition command's environment, and the cap it puts on the
// command's output.
//
// The variables are the only channel through which a condition learns which
// job it guards and when that job last ran — the daemon's own environment
// knows nothing of either. The output cap keeps a chatty condition from
// bloating the log line it is reported in.
func TestCronPreconditionEnv(t *testing.T) {
	now := time.Now()

	t.Run("zero LastRun exports 0", func(t *testing.T) {
		job := &CronJob{ID: "j0", Precondition: `printf %s "$CC_CRON_LAST_RUN"`}
		allow, code, out := runPrecondition(job, now)
		if !allow || code != 0 {
			t.Fatalf("allow=%v exitCode=%d, want true/0", allow, code)
		}
		if got := strings.TrimSpace(out); got != "0" {
			t.Errorf("CC_CRON_LAST_RUN = %q, want %q (a job that never ran)", got, "0")
		}
	})

	t.Run("LastRun exports unix seconds", func(t *testing.T) {
		last := now.Add(-90 * time.Second)
		job := &CronJob{ID: "j1", LastRun: last, Precondition: `printf %s "$CC_CRON_LAST_RUN"`}
		allow, code, out := runPrecondition(job, now)
		if !allow || code != 0 {
			t.Fatalf("allow=%v exitCode=%d, want true/0", allow, code)
		}
		want := strconv.FormatInt(last.Unix(), 10)
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("CC_CRON_LAST_RUN = %q, want %q (the last run, 90s ago)", got, want)
		}
	})

	t.Run("identity variables are exported", func(t *testing.T) {
		job := &CronJob{
			ID:           "j2",
			Description:  "nightly sweep",
			Project:      "proj",
			SessionKey:   "feishu:1:2",
			Precondition: `printf "%s|%s|%s|%s|%s" "$CC_CRON_JOB_ID" "$CC_CRON_JOB_DESC" "$CC_CRON_PROJECT" "$CC_CRON_SESSION_KEY" "$CC_CRON_NOW"`,
		}
		allow, code, out := runPrecondition(job, now)
		if !allow || code != 0 {
			t.Fatalf("allow=%v exitCode=%d, want true/0", allow, code)
		}
		want := "j2|nightly sweep|proj|feishu:1:2|" + strconv.FormatInt(now.Unix(), 10)
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("env dump = %q, want %q", got, want)
		}
	})

	t.Run("5000 bytes of output is truncated", func(t *testing.T) {
		// 5000 'a' bytes, no trailing newline: the cap must cut the tail off
		// and say so.
		job := &CronJob{
			ID:           "j3",
			Precondition: `awk 'BEGIN{s="";while(length(s)<5000)s=s "a";printf "%s",s}'`,
		}
		allow, code, out := runPrecondition(job, now)
		if !allow || code != 0 {
			t.Fatalf("allow=%v exitCode=%d, want true/0", allow, code)
		}
		const suffix = "...(truncated)"
		if want := PreconditionOutputLimit + len(suffix); len(out) != want {
			t.Fatalf("len(output) = %d, want %d (5000 bytes capped at the limit + %q)", len(out), want, suffix)
		}
		if !strings.HasSuffix(out, suffix) {
			t.Errorf("output = %q, want the %q suffix", out, suffix)
		}
		// The kept prefix is the head of the command's output, not padding.
		if got := strings.Trim(out[:PreconditionOutputLimit], "a"); got != "" {
			t.Errorf("kept prefix contains %q, want only the command's own bytes", got)
		}
	})

	t.Run("truncatePreconditionOutput bounds", func(t *testing.T) {
		if got := truncatePreconditionOutput(""); got != "" {
			t.Errorf("truncatePreconditionOutput(\"\") = %q, want empty", got)
		}
		atLimit := strings.Repeat("a", PreconditionOutputLimit)
		if got := truncatePreconditionOutput(atLimit); got != atLimit {
			t.Errorf("output at the limit = %d bytes, want it passed through unchanged", len(got))
		}
		over := strings.Repeat("a", 5000)
		got := truncatePreconditionOutput(over)
		if want := over[:PreconditionOutputLimit] + "...(truncated)"; got != want {
			t.Errorf("truncatePreconditionOutput(5000 bytes) = %d bytes, want %d", len(got), len(want))
		}
	})

	t.Run("preconditionEnv holds all six variables", func(t *testing.T) {
		last := now.Add(-90 * time.Second)
		job := &CronJob{
			ID: "j4", Description: "d", Project: "p", SessionKey: "s", LastRun: last,
		}
		want := []string{
			"CC_CRON_JOB_ID=j4",
			"CC_CRON_JOB_DESC=d",
			"CC_CRON_PROJECT=p",
			"CC_CRON_SESSION_KEY=s",
			"CC_CRON_LAST_RUN=" + strconv.FormatInt(last.Unix(), 10),
			"CC_CRON_NOW=" + strconv.FormatInt(now.Unix(), 10),
		}
		for _, kv := range want {
			if !slices.Contains(preconditionEnv(job, now), kv) {
				t.Errorf("preconditionEnv() missing %q, got %v", kv, preconditionEnv(job, now))
			}
		}
	})
}

// TestPreconditionTimeoutDuration pins how the configured timeout is
// resolved: unset falls back to the default, an explicit value is honoured,
// and anything above the cap is clamped to the maximum.
func TestPreconditionTimeoutDuration(t *testing.T) {
	cases := []struct {
		name string
		job  *CronJob
		want time.Duration
	}{
		{"nil job", nil, DefaultPreconditionTimeout},
		{"unset uses default", &CronJob{}, DefaultPreconditionTimeout},
		{"negative uses default", &CronJob{PreconditionTimeout: -5}, DefaultPreconditionTimeout},
		{"explicit timeout", &CronJob{PreconditionTimeout: 30}, 30 * time.Second},
		{"above cap is clamped", &CronJob{PreconditionTimeout: 9999}, MaxPreconditionTimeout},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PreconditionTimeoutDuration(tc.job); got != tc.want {
				t.Errorf("PreconditionTimeoutDuration() = %v, want %v", got, tc.want)
			}
		})
	}
}