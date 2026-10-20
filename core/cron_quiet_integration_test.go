package core

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newQuietIntegrationServer wires a management server with a cron scheduler
// backed by a throwaway store. A test can then follow a job from the HTTP
// request all the way to the scheduling decision: API → store → decision.
func newQuietIntegrationServer(t *testing.T) (*httptest.Server, *CronStore) {
	t.Helper()
	mgmt, ts, e := testManagementServer(t, "tok")
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := NewCronScheduler(store)
	cs.RegisterEngine("test-project", e)
	mgmt.SetCronScheduler(cs)
	return ts, store
}

// postQuietJob creates a cron job through the management API and returns the
// created job.
func postQuietJob(t *testing.T, ts *httptest.Server, body map[string]any) CronJob {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	body["project"] = "test-project"
	body["session_key"] = "test:chan:user"
	body["cron_expr"] = "0 9 * * *"
	body["prompt"] = "hello"
	r := mgmtPost(t, ts.URL+"/api/v1/cron", "tok", body)
	if !r.OK {
		t.Fatalf("cron add failed: %s", r.Error)
	}
	var job CronJob
	if err := json.Unmarshal(r.Data, &job); err != nil {
		t.Fatalf("unmarshal cron job: %v", err)
	}
	return job
}

// TestCronQuietIntegration_EndToEnd follows a job through the whole chain:
// the quiet-hours fields are accepted by the API, land in the store, come
// back over the wire, and finally drive the scheduling decision.
//
// The PATCH step sends the window and the interval in a single request on a
// job that has neither yet, and repeats it: Go randomizes map iteration, so
// a random application order would fail about half of the runs.
func TestCronQuietIntegration_EndToEnd(t *testing.T) {
	ts, store := newQuietIntegrationServer(t)

	// ── POST ────────────────────────────────────────────────────────────
	job := postQuietJob(t, ts, map[string]any{
		"quiet_hours":         "23:00-08:00",
		"quiet_interval_mins": 60,
	})
	if job.QuietHours != "23:00-08:00" {
		t.Errorf("POST response QuietHours = %q, want %q", job.QuietHours, "23:00-08:00")
	}
	if job.QuietIntervalMins != 60 {
		t.Errorf("POST response QuietIntervalMins = %d, want 60", job.QuietIntervalMins)
	}
	stored := store.Get(job.ID)
	if stored == nil {
		t.Fatal("job not found in store after POST")
	}
	if stored.QuietHours != "23:00-08:00" || stored.QuietIntervalMins != 60 {
		t.Errorf("stored quiet fields = (%q, %d), want (%q, 60)",
			stored.QuietHours, stored.QuietIntervalMins, "23:00-08:00")
	}

	// ── PATCH ───────────────────────────────────────────────────────────
	// Both quiet fields in one request, on a job that has neither yet.
	for i := 0; i < 20; i++ {
		plain := postQuietJob(t, ts, nil)

		patch := mgmtPatch(t, ts.URL+"/api/v1/cron/"+plain.ID, "tok", map[string]any{
			"quiet_hours":         "23:00-08:00",
			"quiet_interval_mins": 60,
		})
		if !patch.OK {
			t.Fatalf("iteration %d: cron patch failed: %s", i, patch.Error)
		}

		var patched CronJob
		if err := json.Unmarshal(patch.Data, &patched); err != nil {
			t.Fatalf("iteration %d: unmarshal patched job: %v", i, err)
		}
		if patched.QuietHours != "23:00-08:00" || patched.QuietIntervalMins != 60 {
			t.Fatalf("iteration %d: patched quiet fields = (%q, %d), want (%q, 60)",
				i, patched.QuietHours, patched.QuietIntervalMins, "23:00-08:00")
		}

		patchedStored := store.Get(plain.ID)
		if patchedStored == nil {
			t.Fatalf("iteration %d: job not found in store after PATCH", i)
		}
		if patchedStored.QuietHours != "23:00-08:00" || patchedStored.QuietIntervalMins != 60 {
			t.Fatalf("iteration %d: stored quiet fields = (%q, %d), want (%q, 60)",
				i, patchedStored.QuietHours, patchedStored.QuietIntervalMins, "23:00-08:00")
		}

		// The configuration that reached the store is what the scheduler
		// decides on: inside the window the job warms up.
		if got := patchedStored.quietActionAt(atClock(23, 30)); got != quietWarm {
			t.Fatalf("iteration %d: quietActionAt(23:30) = %v, want %v", i, got, quietWarm)
		}
	}

	// ── GET ─────────────────────────────────────────────────────────────
	list := mgmtGet(t, ts.URL+"/api/v1/cron", "tok")
	if !list.OK {
		t.Fatalf("cron list failed: %s", list.Error)
	}
	var listed struct {
		Jobs []CronJob `json:"jobs"`
	}
	if err := json.Unmarshal(list.Data, &listed); err != nil {
		t.Fatalf("unmarshal cron list: %v", err)
	}
	var fetched *CronJob
	for i := range listed.Jobs {
		if listed.Jobs[i].ID == job.ID {
			fetched = &listed.Jobs[i]
			break
		}
	}
	if fetched == nil {
		t.Fatalf("job %s not found in the cron list", job.ID)
	}
	if fetched.QuietHours != "23:00-08:00" {
		t.Errorf("GET QuietHours = %q, want %q", fetched.QuietHours, "23:00-08:00")
	}
	if fetched.QuietIntervalMins != 60 {
		t.Errorf("GET QuietIntervalMins = %d, want 60", fetched.QuietIntervalMins)
	}
	for _, want := range []string{`"quiet_hours":"23:00-08:00"`, `"quiet_interval_mins":60`} {
		if !strings.Contains(string(list.Data), want) {
			t.Errorf("cron list payload should contain %s, got: %s", want, string(list.Data))
		}
	}

	// ── decision ────────────────────────────────────────────────────────
	// The job that came in through the API decides exactly as it was
	// configured: warm inside the window, normal outside it.
	final := store.Get(job.ID)
	if final == nil {
		t.Fatal("job not found in store")
	}
	if final.QuietHours != "23:00-08:00" {
		t.Errorf("final QuietHours = %q, want %q", final.QuietHours, "23:00-08:00")
	}
	if got := final.quietActionAt(atClock(23, 30)); got != quietWarm {
		t.Errorf("quietActionAt(23:30) = %v, want %v", got, quietWarm)
	}
	if got := final.quietActionAt(atClock(12, 0)); got != quietRun {
		t.Errorf("quietActionAt(12:00) = %v, want %v", got, quietRun)
	}
}

// TestCronQuietIntegration_LegacyFile checks that a jobs.json written before
// the quiet-hours fields existed still loads, still runs normally, and is
// still written back in the old format — the feature is opt-in, so a job
// without the fields must behave exactly as before.
func TestCronQuietIntegration_LegacyFile(t *testing.T) {
	dir := t.TempDir()
	cronsDir := filepath.Join(dir, "crons")
	if err := os.MkdirAll(cronsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const legacy = `[{
  "id": "cron_legacy",
  "project": "test-project",
  "session_key": "test:chan:user",
  "cron_expr": "0 9 * * *",
  "prompt": "hello",
  "description": "legacy job",
  "enabled": true,
  "created_at": "2026-01-01T00:00:00Z"
}]`
	jobsPath := filepath.Join(cronsDir, "jobs.json")
	if err := os.WriteFile(jobsPath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	jobs := store.List()
	if len(jobs) != 1 {
		t.Fatalf("loaded %d jobs, want 1", len(jobs))
	}
	job := jobs[0]
	if job.ID != "cron_legacy" {
		t.Errorf("loaded job ID = %q, want %q", job.ID, "cron_legacy")
	}
	if job.QuietHours != "" || job.QuietIntervalMins != 0 || job.WarmPrompt != "" {
		t.Errorf("legacy job quiet fields = (%q, %d, %q), want all empty",
			job.QuietHours, job.QuietIntervalMins, job.WarmPrompt)
	}

	// Without a window the job always runs normally — at any hour of the day.
	for _, now := range []time.Time{atClock(23, 30), atClock(3, 0), atClock(12, 0)} {
		if got := job.quietActionAt(now); got != quietRun {
			t.Errorf("legacy job quietActionAt(%s) = %v, want %v",
				now.Format("15:04"), got, quietRun)
		}
	}

	// The store writes the file back in the old format: no quiet-hours keys
	// are invented for a job that never configured them.
	if !store.Update(job.ID, "description", "legacy job touched") {
		t.Fatal("store update of the legacy job failed")
	}
	rewritten, err := os.ReadFile(jobsPath)
	if err != nil {
		t.Fatalf("read rewritten jobs.json: %v", err)
	}
	for _, key := range []string{"quiet_hours", "quiet_interval_mins", "warm_prompt"} {
		if strings.Contains(string(rewritten), key) {
			t.Errorf("rewritten jobs.json should not contain %q, got: %s", key, string(rewritten))
		}
	}

	// Reloading the rewritten file yields the same job, still without quiet
	// configuration and still always running normally.
	reloaded, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	back := reloaded.Get("cron_legacy")
	if back == nil {
		t.Fatal("legacy job not found after reloading the rewritten file")
	}
	if back.QuietHours != "" || back.QuietIntervalMins != 0 || back.WarmPrompt != "" {
		t.Errorf("reloaded job quiet fields = (%q, %d, %q), want all empty",
			back.QuietHours, back.QuietIntervalMins, back.WarmPrompt)
	}
	if got := back.quietActionAt(atClock(23, 30)); got != quietRun {
		t.Errorf("reloaded job quietActionAt(23:30) = %v, want %v", got, quietRun)
	}
}
