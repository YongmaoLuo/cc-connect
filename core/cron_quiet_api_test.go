package core

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newQuietTestAPIServer wires an APIServer with a cron scheduler backed by a
// throwaway store, so a test can inspect what the HTTP layer persisted.
func newQuietTestAPIServer(t *testing.T) (*APIServer, *CronStore) {
	t.Helper()
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scheduler := NewCronScheduler(store)
	engine := NewEngine("test", &stubAgent{}, []Platform{&stubMediaPlatform{stubPlatformEngine: stubPlatformEngine{n: "test"}}}, "", LangEnglish)
	t.Cleanup(engine.cancel)
	engine.cronScheduler = scheduler
	scheduler.RegisterEngine("test", engine)
	return &APIServer{engines: map[string]*Engine{"test": engine}, cron: scheduler}, store
}

// mgmtRequestWithStatus performs an authenticated management API call and
// returns the HTTP status code together with the decoded response.
func mgmtRequestWithStatus(t *testing.T, method, url, token string, body any) (int, mgmtResponse) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, _ := http.NewRequest(method, url, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var r mgmtResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, r
}

func assertCronQuietFields(t *testing.T, where string, job CronJob) {
	t.Helper()
	if job.QuietHours != "23:00-08:00" {
		t.Errorf("%s QuietHours = %q, want %q", where, job.QuietHours, "23:00-08:00")
	}
	if job.QuietIntervalMins != 60 {
		t.Errorf("%s QuietIntervalMins = %d, want 60", where, job.QuietIntervalMins)
	}
	if job.WarmPrompt != "Reply OK only." {
		t.Errorf("%s WarmPrompt = %q, want %q", where, job.WarmPrompt, "Reply OK only.")
	}
}

// assertCronQuietJSON checks the wire format: the quiet-hours fields are
// exposed under their JSON names, not dropped by the HTTP layer.
func assertCronQuietJSON(t *testing.T, where, payload string) {
	t.Helper()
	for _, want := range []string{
		`"quiet_hours":"23:00-08:00"`,
		`"quiet_interval_mins":60`,
		`"warm_prompt":"Reply OK only."`,
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("%s payload should contain %s, got: %s", where, want, payload)
		}
	}
}

// TestCronQuietAPI_AddPersistsFields checks that POST /cron/add carries the
// quiet-hours fields into the persisted job instead of dropping them.
func TestCronQuietAPI_AddPersistsFields(t *testing.T) {
	api, store := newQuietTestAPIServer(t)

	body, err := json.Marshal(map[string]any{
		"project":             "test",
		"session_key":         "test:chan:user",
		"cron_expr":           "0 9 * * *",
		"prompt":              "hello",
		"quiet_hours":         "23:00-08:00",
		"quiet_interval_mins": 60,
		"warm_prompt":         "Reply OK only.",
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/cron/add", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	api.handleCronAdd(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var job CronJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	assertCronQuietFields(t, "response", job)
	assertCronQuietJSON(t, "response", rec.Body.String())

	stored := store.Get(job.ID)
	if stored == nil {
		t.Fatal("job not found in store")
	}
	assertCronQuietFields(t, "store", *stored)
}

// TestCronQuietAPI_MgmtPostPersistsFields checks that the management API's
// POST /api/v1/cron keeps the quiet-hours fields too.
func TestCronQuietAPI_MgmtPostPersistsFields(t *testing.T) {
	mgmt, ts, e := testManagementServer(t, "tok")
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := NewCronScheduler(store)
	cs.RegisterEngine("test-project", e)
	mgmt.SetCronScheduler(cs)

	r := mgmtPost(t, ts.URL+"/api/v1/cron", "tok", map[string]any{
		"project":             "test-project",
		"session_key":         "test:chan:user",
		"cron_expr":           "0 9 * * *",
		"prompt":              "hello",
		"quiet_hours":         "23:00-08:00",
		"quiet_interval_mins": 60,
		"warm_prompt":         "Reply OK only.",
	})
	if !r.OK {
		t.Fatalf("cron add failed: %s", r.Error)
	}

	var job CronJob
	if err := json.Unmarshal(r.Data, &job); err != nil {
		t.Fatalf("unmarshal cron job: %v", err)
	}
	assertCronQuietFields(t, "response", job)
	assertCronQuietJSON(t, "response", string(r.Data))

	stored := store.Get(job.ID)
	if stored == nil {
		t.Fatal("job not found in store")
	}
	assertCronQuietFields(t, "store", *stored)
}

// TestCronQuietAPI_PatchOrderIndependent checks that a PATCH setting the
// window and the interval in one request always succeeds and always lands on
// the same configuration, whatever order the JSON object lists the fields in.
// Go randomizes map iteration, so the request is repeated: a random
// application order would fail about half of the runs.
func TestCronQuietAPI_PatchOrderIndependent(t *testing.T) {
	mgmt, ts, e := testManagementServer(t, "tok")
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := NewCronScheduler(store)
	cs.RegisterEngine("test-project", e)
	mgmt.SetCronScheduler(cs)

	for i := 0; i < 25; i++ {
		r := mgmtPost(t, ts.URL+"/api/v1/cron", "tok", map[string]any{
			"project":     "test-project",
			"session_key": "test:chan:user",
			"cron_expr":   "0 9 * * *",
			"prompt":      "hello",
		})
		if !r.OK {
			t.Fatalf("iteration %d: cron add failed: %s", i, r.Error)
		}
		var job CronJob
		if err := json.Unmarshal(r.Data, &job); err != nil {
			t.Fatalf("iteration %d: unmarshal cron job: %v", i, err)
		}

		r = mgmtPatch(t, ts.URL+"/api/v1/cron/"+job.ID, "tok", map[string]any{
			"quiet_hours":         "23:00-08:00",
			"quiet_interval_mins": 60,
		})
		if !r.OK {
			t.Fatalf("iteration %d: cron patch failed: %s", i, r.Error)
		}

		var updated CronJob
		if err := json.Unmarshal(r.Data, &updated); err != nil {
			t.Fatalf("iteration %d: unmarshal patched job: %v", i, err)
		}
		if updated.QuietHours != "23:00-08:00" || updated.QuietIntervalMins != 60 {
			t.Fatalf("iteration %d: patched job = (%q, %d), want (%q, 60)",
				i, updated.QuietHours, updated.QuietIntervalMins, "23:00-08:00")
		}
		for _, want := range []string{`"quiet_hours":"23:00-08:00"`, `"quiet_interval_mins":60`} {
			if !strings.Contains(string(r.Data), want) {
				t.Fatalf("iteration %d: patch payload should contain %s, got: %s",
					i, want, string(r.Data))
			}
		}

		stored := store.Get(job.ID)
		if stored == nil {
			t.Fatalf("iteration %d: job not found in store", i)
		}
		if stored.QuietHours != "23:00-08:00" || stored.QuietIntervalMins != 60 {
			t.Fatalf("iteration %d: stored job = (%q, %d), want (%q, 60)",
				i, stored.QuietHours, stored.QuietIntervalMins, "23:00-08:00")
		}
	}
}

// TestCronQuietAPI_PatchFieldOrder pins the order in which a PATCH applies
// its fields: the quiet-hours window first, then the quiet interval, then the
// warm prompt, then the remaining fields in a stable alphabetical order. The
// order must not depend on Go's randomized map iteration.
func TestCronQuietAPI_PatchFieldOrder(t *testing.T) {
	got := cronPatchOrder(map[string]any{
		"enabled":             true,
		"warm_prompt":         "ping",
		"quiet_interval_mins": 60,
		"quiet_hours":         "23:00-08:00",
		"cron_expr":           "0 9 * * *",
	})
	want := []string{"quiet_hours", "quiet_interval_mins", "warm_prompt", "cron_expr", "enabled"}
	if len(got) != len(want) {
		t.Fatalf("cronPatchOrder() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cronPatchOrder() = %v, want %v", got, want)
		}
	}
}

// TestCronQuietAPI_RejectsInvalid checks that a malformed quiet-hours window
// and a quiet interval without a window are rejected with 400 and a reason,
// on both the add and the patch path.
func TestCronQuietAPI_RejectsInvalid(t *testing.T) {
	api, _ := newQuietTestAPIServer(t)

	t.Run("add with malformed quiet hours", func(t *testing.T) {
		body, err := json.Marshal(map[string]any{
			"project":     "test",
			"session_key": "test:chan:user",
			"cron_expr":   "0 9 * * *",
			"prompt":      "hello",
			"quiet_hours": "25:00-09:00",
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/cron/add", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		api.handleCronAdd(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "quiet_hours") {
			t.Errorf("body should explain the invalid quiet_hours, got: %s", rec.Body.String())
		}
	})

	t.Run("add with interval but no window", func(t *testing.T) {
		body, err := json.Marshal(map[string]any{
			"project":             "test",
			"session_key":         "test:chan:user",
			"cron_expr":           "0 9 * * *",
			"prompt":              "hello",
			"quiet_interval_mins": 60,
		})
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/cron/add", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		api.handleCronAdd(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "quiet_hours") {
			t.Errorf("body should explain the missing quiet_hours, got: %s", rec.Body.String())
		}
	})

	mgmt, ts, e := testManagementServer(t, "tok")
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := NewCronScheduler(store)
	cs.RegisterEngine("test-project", e)
	mgmt.SetCronScheduler(cs)

	// addJob creates a plain job without quiet configuration.
	addJob := func(t *testing.T) string {
		t.Helper()
		r := mgmtPost(t, ts.URL+"/api/v1/cron", "tok", map[string]any{
			"project":     "test-project",
			"session_key": "test:chan:user",
			"cron_expr":   "0 9 * * *",
			"prompt":      "hello",
		})
		if !r.OK {
			t.Fatalf("cron add failed: %s", r.Error)
		}
		var job CronJob
		if err := json.Unmarshal(r.Data, &job); err != nil {
			t.Fatalf("unmarshal cron job: %v", err)
		}
		return job.ID
	}

	t.Run("management post with malformed quiet hours", func(t *testing.T) {
		status, r := mgmtRequestWithStatus(t, http.MethodPost, ts.URL+"/api/v1/cron", "tok", map[string]any{
			"project":     "test-project",
			"session_key": "test:chan:user",
			"cron_expr":   "0 9 * * *",
			"prompt":      "hello",
			"quiet_hours": "25:00-09:00",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; error=%s", status, r.Error)
		}
		if !strings.Contains(r.Error, "quiet_hours") {
			t.Errorf("error should explain the invalid quiet_hours, got: %s", r.Error)
		}
	})

	t.Run("patch with malformed quiet hours", func(t *testing.T) {
		id := addJob(t)
		status, r := mgmtRequestWithStatus(t, http.MethodPatch, ts.URL+"/api/v1/cron/"+id, "tok", map[string]any{
			"quiet_hours": "25:00-09:00",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; error=%s", status, r.Error)
		}
		if !strings.Contains(r.Error, "quiet_hours") {
			t.Errorf("error should explain the invalid quiet_hours, got: %s", r.Error)
		}
		if got := store.Get(id).QuietHours; got != "" {
			t.Errorf("stored QuietHours = %q after rejected patch, want empty", got)
		}
	})

	t.Run("patch with interval but no window", func(t *testing.T) {
		id := addJob(t)
		status, r := mgmtRequestWithStatus(t, http.MethodPatch, ts.URL+"/api/v1/cron/"+id, "tok", map[string]any{
			"quiet_interval_mins": 60,
		})
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; error=%s", status, r.Error)
		}
		if !strings.Contains(r.Error, "quiet_hours") {
			t.Errorf("error should explain the missing quiet_hours, got: %s", r.Error)
		}
		if got := store.Get(id).QuietIntervalMins; got != 0 {
			t.Errorf("stored QuietIntervalMins = %d after rejected patch, want 0", got)
		}
	})
}
