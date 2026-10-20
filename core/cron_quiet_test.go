package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCronQuietModel_Parse(t *testing.T) {
	tests := []struct {
		name       string
		spec       string
		wantStart  int
		wantEnd    int
		wantActive bool
		wantErr    bool
	}{
		{name: "empty spec disables quiet hours", spec: "", wantStart: 0, wantEnd: 0, wantActive: false},
		{name: "blank spec disables quiet hours", spec: "   ", wantStart: 0, wantEnd: 0, wantActive: false},
		{name: "daytime window", spec: "09:00-17:00", wantStart: 540, wantEnd: 1020, wantActive: true},
		{name: "window crossing midnight", spec: "23:00-08:00", wantStart: 1380, wantEnd: 480, wantActive: true},
		{name: "equal bounds disable quiet hours", spec: "08:00-08:00", wantStart: 480, wantEnd: 480, wantActive: false},
		{name: "hour out of range", spec: "25:00-09:00", wantErr: true},
		{name: "minute out of range", spec: "08:70-09:00", wantErr: true},
		{name: "missing separator", spec: "23:00", wantErr: true},
		{name: "too many separators", spec: "08:00-09:00-10:00", wantErr: true},
		{name: "garbage", spec: "not-a-window", wantErr: true},
		{name: "empty bound", spec: "-09:00", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, active, err := ParseQuietHours(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseQuietHours(%q) = (%d, %d, %t, nil), want error", tt.spec, start, end, active)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseQuietHours(%q) unexpected error: %v", tt.spec, err)
			}
			if start != tt.wantStart || end != tt.wantEnd || active != tt.wantActive {
				t.Errorf("ParseQuietHours(%q) = (%d, %d, %t), want (%d, %d, %t)",
					tt.spec, start, end, active, tt.wantStart, tt.wantEnd, tt.wantActive)
			}
		})
	}
}

func TestCronQuietModel_Validate(t *testing.T) {
	tests := []struct {
		name    string
		job     CronJob
		wantErr string
	}{
		{
			name:    "interval without quiet hours is rejected",
			job:     CronJob{SessionKey: "feishu:1", QuietIntervalMins: 5},
			wantErr: "quiet_hours is required when quiet_interval_mins > 0",
		},
		{
			name:    "negative interval is rejected",
			job:     CronJob{SessionKey: "feishu:1", QuietIntervalMins: -1},
			wantErr: "quiet_interval_mins must be >= 0",
		},
		{
			name:    "malformed quiet hours is rejected",
			job:     CronJob{SessionKey: "feishu:1", QuietHours: "25:00-09:00"},
			wantErr: "quiet_hours",
		},
		{
			name: "no quiet configuration is valid",
			job:  CronJob{SessionKey: "feishu:1"},
		},
		{
			name: "zero interval without quiet hours is valid",
			job:  CronJob{SessionKey: "feishu:1", QuietIntervalMins: 0},
		},
		{
			name: "interval with quiet hours is valid",
			job:  CronJob{SessionKey: "feishu:1", QuietHours: "23:00-08:00", QuietIntervalMins: 5},
		},
		{
			name: "quiet hours without interval is valid",
			job:  CronJob{SessionKey: "feishu:1", QuietHours: "23:00-08:00"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCronJob(&tt.job)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateCronJob() unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateCronJob() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validateCronJob() error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestCronQuietModel_UpdateFields(t *testing.T) {
	job := &CronJob{ID: "j1", SessionKey: "feishu:1"}

	if err := updateJobField(job, "quiet_hours", "23:00-08:00"); err != nil {
		t.Fatalf("updateJobField(quiet_hours) error: %v", err)
	}
	if job.QuietHours != "23:00-08:00" {
		t.Errorf("QuietHours = %q, want %q", job.QuietHours, "23:00-08:00")
	}

	if err := updateJobField(job, "quiet_interval_mins", float64(15)); err != nil {
		t.Fatalf("updateJobField(quiet_interval_mins, float64) error: %v", err)
	}
	if job.QuietIntervalMins != 15 {
		t.Errorf("QuietIntervalMins = %d, want 15", job.QuietIntervalMins)
	}

	if err := updateJobField(job, "quiet_interval_mins", 30); err != nil {
		t.Fatalf("updateJobField(quiet_interval_mins, int) error: %v", err)
	}
	if job.QuietIntervalMins != 30 {
		t.Errorf("QuietIntervalMins = %d, want 30", job.QuietIntervalMins)
	}

	if err := updateJobField(job, "warm_prompt", "Reply OK only."); err != nil {
		t.Fatalf("updateJobField(warm_prompt) error: %v", err)
	}
	if job.WarmPrompt != "Reply OK only." {
		t.Errorf("WarmPrompt = %q, want %q", job.WarmPrompt, "Reply OK only.")
	}

	// The scheduler-level update path must accept the same fields and
	// persist them, and must reject invalid values.
	dir := t.TempDir()
	store, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cs := NewCronScheduler(store)
	if err := cs.AddJob(&CronJob{
		ID:         "j2",
		Project:    "proj",
		SessionKey: "feishu:1",
		CronExpr:   "0 9 * * *",
		Prompt:     "hello",
		Enabled:    false,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := cs.UpdateJob("j2", "quiet_hours", "23:00-08:00"); err != nil {
		t.Fatalf("UpdateJob(quiet_hours) error: %v", err)
	}
	if got := store.Get("j2").QuietHours; got != "23:00-08:00" {
		t.Errorf("stored QuietHours = %q, want %q", got, "23:00-08:00")
	}

	if err := cs.UpdateJob("j2", "quiet_interval_mins", float64(10)); err != nil {
		t.Fatalf("UpdateJob(quiet_interval_mins) error: %v", err)
	}
	if got := store.Get("j2").QuietIntervalMins; got != 10 {
		t.Errorf("stored QuietIntervalMins = %d, want 10", got)
	}

	if err := cs.UpdateJob("j2", "warm_prompt", "ping"); err != nil {
		t.Fatalf("UpdateJob(warm_prompt) error: %v", err)
	}
	if got := store.Get("j2").WarmPrompt; got != "ping" {
		t.Errorf("stored WarmPrompt = %q, want %q", got, "ping")
	}

	if err := cs.UpdateJob("j2", "quiet_interval_mins", float64(-1)); err == nil {
		t.Error("UpdateJob(quiet_interval_mins, -1) = nil, want error")
	}
	if err := cs.UpdateJob("j2", "quiet_hours", "25:00-09:00"); err == nil {
		t.Error("UpdateJob(quiet_hours, malformed) = nil, want error")
	}
	if got := store.Get("j2").QuietIntervalMins; got != 10 {
		t.Errorf("stored QuietIntervalMins = %d after rejected update, want 10", got)
	}
}

func TestCronQuietModel_LegacyJSON(t *testing.T) {
	legacy := `{
	  "id": "legacy1",
	  "project": "proj",
	  "session_key": "feishu:1",
	  "cron_expr": "0 9 * * *",
	  "prompt": "hello",
	  "description": "legacy job",
	  "enabled": true,
	  "created_at": "2026-01-02T03:04:05Z"
	}`

	var job CronJob
	if err := json.Unmarshal([]byte(legacy), &job); err != nil {
		t.Fatalf("unmarshal legacy job: %v", err)
	}
	if job.QuietHours != "" {
		t.Errorf("legacy QuietHours = %q, want empty", job.QuietHours)
	}
	if job.QuietIntervalMins != 0 {
		t.Errorf("legacy QuietIntervalMins = %d, want 0", job.QuietIntervalMins)
	}
	if job.WarmPrompt != "" {
		t.Errorf("legacy WarmPrompt = %q, want empty", job.WarmPrompt)
	}

	// A legacy jobs.json on disk must load with zero-valued quiet fields.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "crons"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "crons", "jobs.json")
	if err := os.WriteFile(path, []byte("["+legacy+"]"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded := store.Get("legacy1")
	if loaded == nil {
		t.Fatal("legacy job not loaded from jobs.json")
	}
	if loaded.QuietHours != "" || loaded.QuietIntervalMins != 0 || loaded.WarmPrompt != "" {
		t.Errorf("legacy job quiet fields = (%q, %d, %q), want zero values",
			loaded.QuietHours, loaded.QuietIntervalMins, loaded.WarmPrompt)
	}

	// New jobs with quiet fields must round-trip through JSON.
	withQuiet := CronJob{
		ID:                "new1",
		SessionKey:        "feishu:1",
		QuietHours:        "23:00-08:00",
		QuietIntervalMins: 5,
		WarmPrompt:        DefaultWarmPrompt,
	}
	data, err := json.Marshal(withQuiet)
	if err != nil {
		t.Fatal(err)
	}
	var back CronJob
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.QuietHours != withQuiet.QuietHours ||
		back.QuietIntervalMins != withQuiet.QuietIntervalMins ||
		back.WarmPrompt != withQuiet.WarmPrompt {
		t.Errorf("round-trip = (%q, %d, %q), want (%q, %d, %q)",
			back.QuietHours, back.QuietIntervalMins, back.WarmPrompt,
			withQuiet.QuietHours, withQuiet.QuietIntervalMins, withQuiet.WarmPrompt)
	}

	if DefaultWarmPrompt == "" {
		t.Error("DefaultWarmPrompt must not be empty")
	}
}

// TestCronQuietUpdate_CombinedValidation covers the combination check in
// CronScheduler.UpdateJob. The socket API (`handleCronEdit`) calls UpdateJob
// directly, so without a combined check it could persist a job that
// validateCronJob — and therefore /cron/add — would reject.
func TestCronQuietUpdate_CombinedValidation(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := NewCronScheduler(store)

	newJob := func(t *testing.T, id string, quietHours string, interval int) {
		t.Helper()
		if err := cs.AddJob(&CronJob{
			ID:                id,
			Project:           "proj",
			SessionKey:        "feishu:1",
			CronExpr:          "0 9 * * *",
			Prompt:            "hello",
			Enabled:           false,
			QuietHours:        quietHours,
			QuietIntervalMins: interval,
			CreatedAt:         time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 1. An interval without a window is rejected, and nothing is
	// half-written: the stored interval keeps its previous value.
	t.Run("interval without quiet_hours is rejected", func(t *testing.T) {
		newJob(t, "no-window", "", 0)

		err := cs.UpdateJob("no-window", "quiet_interval_mins", 60)
		if err == nil {
			t.Fatal("UpdateJob(quiet_interval_mins=60) on a job without quiet_hours = nil, want error")
		}
		if !strings.Contains(err.Error(), "quiet_hours is required when quiet_interval_mins > 0") {
			t.Errorf("UpdateJob error = %q, want it to mention quiet_hours being required", err)
		}
		if got := store.Get("no-window").QuietIntervalMins; got != 0 {
			t.Errorf("stored QuietIntervalMins = %d after rejected update, want 0", got)
		}
	})

	// 2. The same update succeeds once the job carries a window.
	t.Run("interval with quiet_hours is accepted", func(t *testing.T) {
		newJob(t, "with-window", "00:00-08:00", 0)

		if err := cs.UpdateJob("with-window", "quiet_interval_mins", 60); err != nil {
			t.Fatalf("UpdateJob(quiet_interval_mins=60) with quiet_hours set: %v", err)
		}
		if got := store.Get("with-window").QuietIntervalMins; got != 60 {
			t.Errorf("stored QuietIntervalMins = %d, want 60", got)
		}
	})

	// 3. Clearing the window while an interval is set would strand the
	// interval, so it is rejected.
	t.Run("clearing quiet_hours with an interval is rejected", func(t *testing.T) {
		newJob(t, "has-interval", "00:00-08:00", 60)

		err := cs.UpdateJob("has-interval", "quiet_hours", "")
		if err == nil {
			t.Fatal("UpdateJob(quiet_hours=\"\") on a job with an interval = nil, want error")
		}
		if !strings.Contains(err.Error(), "quiet_hours is required when quiet_interval_mins > 0") {
			t.Errorf("UpdateJob error = %q, want it to mention quiet_hours being required", err)
		}
		if got := store.Get("has-interval").QuietIntervalMins; got != 60 {
			t.Errorf("stored QuietIntervalMins = %d after rejected update, want 60", got)
		}
		if got := store.Get("has-interval").QuietHours; got != "00:00-08:00" {
			t.Errorf("stored QuietHours = %q after rejected update, want %q", got, "00:00-08:00")
		}
	})

	// 4. Setting a window on a job with no quiet configuration is fine: the
	// interval stays 0, so the pair is still valid.
	t.Run("quiet_hours alone is accepted", func(t *testing.T) {
		newJob(t, "hours-only", "", 0)

		if err := cs.UpdateJob("hours-only", "quiet_hours", "00:00-08:00"); err != nil {
			t.Fatalf("UpdateJob(quiet_hours) on a job with no quiet config: %v", err)
		}
		got := store.Get("hours-only")
		if got.QuietHours != "00:00-08:00" {
			t.Errorf("stored QuietHours = %q, want %q", got.QuietHours, "00:00-08:00")
		}
		if got.QuietIntervalMins != 0 {
			t.Errorf("stored QuietIntervalMins = %d, want 0", got.QuietIntervalMins)
		}
	})
}
