package core

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newPreconditionTestAPIServer wires an APIServer with a cron scheduler backed
// by a throwaway store, so the test can inspect what the local HTTP API
// persisted.
func newPreconditionTestAPIServer(t *testing.T) (*APIServer, *CronStore) {
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

// postCronAdd sends body to POST /cron/add and returns the recorder.
func postCronAdd(t *testing.T, api *APIServer, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/cron/add", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	api.handleCronAdd(rec, req)
	return rec
}

// TestCronPreconditionAPILocalAdd checks that the local Unix-socket API
// accepts the precondition fields on POST /cron/add and persists them:
// both the response payload and the stored job must carry the values.
func TestCronPreconditionAPILocalAdd(t *testing.T) {
	api, store := newPreconditionTestAPIServer(t)

	rec := postCronAdd(t, api, map[string]any{
		"project":              "test",
		"session_key":          "test:chan:user",
		"cron_expr":            "0 9 * * *",
		"prompt":               "hello",
		"precondition":         "test -f marker.txt",
		"precondition_timeout": 30,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var job CronJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	if job.Precondition != "test -f marker.txt" {
		t.Errorf("response Precondition = %q, want %q", job.Precondition, "test -f marker.txt")
	}
	if job.PreconditionTimeout != 30 {
		t.Errorf("response PreconditionTimeout = %d, want 30", job.PreconditionTimeout)
	}

	stored := store.Get(job.ID)
	if stored == nil {
		t.Fatal("job not found in store")
	}
	if stored.Precondition != "test -f marker.txt" {
		t.Errorf("store Precondition = %q, want %q", stored.Precondition, "test -f marker.txt")
	}
	if stored.PreconditionTimeout != 30 {
		t.Errorf("store PreconditionTimeout = %d, want 30", stored.PreconditionTimeout)
	}
}

// TestCronPreconditionAPILocalAddDefaults checks the omitted case: a request
// without the precondition fields behaves exactly like a pre-existing one —
// both fields land on their zero values.
func TestCronPreconditionAPILocalAddDefaults(t *testing.T) {
	api, store := newPreconditionTestAPIServer(t)

	rec := postCronAdd(t, api, map[string]any{
		"project":     "test",
		"session_key": "test:chan:user",
		"cron_expr":   "0 9 * * *",
		"prompt":      "hello",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var job CronJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	if job.Precondition != "" {
		t.Errorf("response Precondition = %q, want empty", job.Precondition)
	}
	if job.PreconditionTimeout != 0 {
		t.Errorf("response PreconditionTimeout = %d, want 0", job.PreconditionTimeout)
	}

	stored := store.Get(job.ID)
	if stored == nil {
		t.Fatal("job not found in store")
	}
	if stored.Precondition != "" {
		t.Errorf("store Precondition = %q, want empty", stored.Precondition)
	}
	if stored.PreconditionTimeout != 0 {
		t.Errorf("store PreconditionTimeout = %d, want 0", stored.PreconditionTimeout)
	}
}

// TestCronPreconditionAPILocalAddRejectsInvalid checks that precondition
// values rejected by validateCronJob are reported as a 400 by the local API
// and that nothing is persisted.
func TestCronPreconditionAPILocalAddRejectsInvalid(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "timeout above the cap",
			body: map[string]any{
				"precondition":         "true",
				"precondition_timeout": 301,
			},
		},
		{
			name: "negative timeout",
			body: map[string]any{
				"precondition":         "true",
				"precondition_timeout": -1,
			},
		},
		{
			name: "command above the length cap",
			body: map[string]any{
				"precondition":         strings.Repeat("a", 4097),
				"precondition_timeout": 30,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, store := newPreconditionTestAPIServer(t)

			body := map[string]any{
				"project":     "test",
				"session_key": "test:chan:user",
				"cron_expr":   "0 9 * * *",
				"prompt":      "hello",
			}
			for k, v := range tc.body {
				body[k] = v
			}

			rec := postCronAdd(t, api, body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if jobs := store.List(); len(jobs) != 0 {
				t.Errorf("store has %d jobs, want 0 (nothing persisted)", len(jobs))
			}
		})
	}
}

// TestCronPreconditionUpdateJob checks that `cron edit` (CronScheduler.UpdateJob)
// can set both precondition fields. Two value shapes must be accepted: the int
// the CLI passes natively and the float64 a JSON-decoded body carries. An
// out-of-range timeout must be rejected before it reaches the store, and an
// empty precondition must be accepted as "clear the gate".
func TestCronPreconditionUpdateJob(t *testing.T) {
	api, store := newPreconditionTestAPIServer(t)

	rec := postCronAdd(t, api, map[string]any{
		"project":     "test",
		"session_key": "test:chan:user",
		"cron_expr":   "0 9 * * *",
		"prompt":      "hello",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var job CronJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}

	// Both fields, int values (the shape the CLI sends).
	if err := api.cron.UpdateJob(job.ID, "precondition", "test -f marker.txt"); err != nil {
		t.Fatalf("UpdateJob(precondition): %v", err)
	}
	if err := api.cron.UpdateJob(job.ID, "precondition_timeout", 30); err != nil {
		t.Fatalf("UpdateJob(precondition_timeout, 30): %v", err)
	}
	stored := store.Get(job.ID)
	if stored == nil {
		t.Fatal("job not found in store")
	}
	if stored.Precondition != "test -f marker.txt" {
		t.Errorf("store Precondition = %q, want %q", stored.Precondition, "test -f marker.txt")
	}
	if stored.PreconditionTimeout != 30 {
		t.Errorf("store PreconditionTimeout = %d, want 30", stored.PreconditionTimeout)
	}

	// An out-of-range timeout is refused, and the stored job keeps its value.
	if err := api.cron.UpdateJob(job.ID, "precondition_timeout", 301); err == nil {
		t.Error("UpdateJob(precondition_timeout, 301) = nil, want error")
	}
	if got := store.Get(job.ID).PreconditionTimeout; got != 30 {
		t.Errorf("store PreconditionTimeout after rejected update = %d, want 30 (unchanged)", got)
	}

	// float64, the shape a JSON-decoded body carries.
	if err := api.cron.UpdateJob(job.ID, "precondition_timeout", float64(45)); err != nil {
		t.Fatalf("UpdateJob(precondition_timeout, float64(45)): %v", err)
	}
	if got := store.Get(job.ID).PreconditionTimeout; got != 45 {
		t.Errorf("store PreconditionTimeout = %d, want 45", got)
	}

	// An empty precondition is legal and means "no gate".
	if err := api.cron.UpdateJob(job.ID, "precondition", ""); err != nil {
		t.Fatalf("UpdateJob(precondition, \"\"): %v", err)
	}
	if got := store.Get(job.ID).Precondition; got != "" {
		t.Errorf("store Precondition = %q, want empty", got)
	}
}

// TestCronPreconditionAPIPatch checks the management API's cron endpoints for
// the precondition fields:
//
//   - POST /api/v1/cron must carry the fields into the stored job instead of
//     dropping them;
//   - PATCH /api/v1/cron/{id} must apply the precondition command before its
//     timeout, so a multi-field PATCH always lands in the same state;
//   - a PATCH carrying only the timeout leaves the command untouched;
//   - an out-of-range timeout is refused with 400 and is not persisted.
func TestCronPreconditionAPIPatch(t *testing.T) {
	mgmt, ts, e := testManagementServer(t, "tok")
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := NewCronScheduler(store)
	cs.RegisterEngine("test-project", e)
	mgmt.SetCronScheduler(cs)

	// POST: both fields must reach the stored job.
	r := mgmtPost(t, ts.URL+"/api/v1/cron", "tok", map[string]any{
		"project":              "test-project",
		"session_key":          "test:chan:user",
		"cron_expr":            "0 9 * * *",
		"prompt":               "hello",
		"precondition":         "test -f marker.txt",
		"precondition_timeout": 30,
	})
	if !r.OK {
		t.Fatalf("cron add failed: %s", r.Error)
	}
	var job CronJob
	if err := json.Unmarshal(r.Data, &job); err != nil {
		t.Fatalf("unmarshal cron job: %v", err)
	}
	if job.Precondition != "test -f marker.txt" {
		t.Errorf("response Precondition = %q, want %q", job.Precondition, "test -f marker.txt")
	}
	if job.PreconditionTimeout != 30 {
		t.Errorf("response PreconditionTimeout = %d, want 30", job.PreconditionTimeout)
	}
	stored := store.Get(job.ID)
	if stored == nil {
		t.Fatal("job not found in store")
	}
	if stored.Precondition != "test -f marker.txt" {
		t.Errorf("store Precondition = %q, want %q", stored.Precondition, "test -f marker.txt")
	}
	if stored.PreconditionTimeout != 30 {
		t.Errorf("store PreconditionTimeout = %d, want 30", stored.PreconditionTimeout)
	}

	// PATCH with only the timeout: the command keeps its value.
	status, resp := mgmtRequestWithStatus(t, http.MethodPatch, ts.URL+"/api/v1/cron/"+job.ID, "tok", map[string]any{
		"precondition_timeout": 45,
	})
	if status != http.StatusOK {
		t.Fatalf("patch status = %d, want 200; error=%s", status, resp.Error)
	}
	var updated CronJob
	if err := json.Unmarshal(resp.Data, &updated); err != nil {
		t.Fatalf("unmarshal patched job: %v", err)
	}
	if updated.PreconditionTimeout != 45 {
		t.Errorf("patched PreconditionTimeout = %d, want 45", updated.PreconditionTimeout)
	}
	if updated.Precondition != "test -f marker.txt" {
		t.Errorf("patched Precondition = %q, want %q (unchanged)", updated.Precondition, "test -f marker.txt")
	}
	stored = store.Get(job.ID)
	if stored == nil {
		t.Fatal("job not found in store after patch")
	}
	if stored.PreconditionTimeout != 45 {
		t.Errorf("store PreconditionTimeout = %d, want 45", stored.PreconditionTimeout)
	}
	if stored.Precondition != "test -f marker.txt" {
		t.Errorf("store Precondition = %q, want %q (unchanged)", stored.Precondition, "test -f marker.txt")
	}

	// PATCH with an out-of-range timeout: 400, and the stored value stands.
	status, resp = mgmtRequestWithStatus(t, http.MethodPatch, ts.URL+"/api/v1/cron/"+job.ID, "tok", map[string]any{
		"precondition_timeout": 301,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("patch status = %d, want 400; error=%s", status, resp.Error)
	}
	if got := store.Get(job.ID).PreconditionTimeout; got != 45 {
		t.Errorf("store PreconditionTimeout after rejected patch = %d, want 45 (unchanged)", got)
	}

	// PATCH with both fields: the command is applied before its timeout, so
	// both land and the job stays consistent.
	status, resp = mgmtRequestWithStatus(t, http.MethodPatch, ts.URL+"/api/v1/cron/"+job.ID, "tok", map[string]any{
		"precondition":         "test -f other.txt",
		"precondition_timeout": 60,
	})
	if status != http.StatusOK {
		t.Fatalf("patch status = %d, want 200; error=%s", status, resp.Error)
	}
	stored = store.Get(job.ID)
	if stored == nil {
		t.Fatal("job not found in store after two-field patch")
	}
	if stored.Precondition != "test -f other.txt" {
		t.Errorf("store Precondition = %q, want %q", stored.Precondition, "test -f other.txt")
	}
	if stored.PreconditionTimeout != 60 {
		t.Errorf("store PreconditionTimeout = %d, want 60", stored.PreconditionTimeout)
	}

	// The application order is pinned: the command comes before its timeout.
	got := cronPatchOrder(map[string]any{
		"precondition":         "test -f marker.txt",
		"precondition_timeout": 30,
		"quiet_hours":          "23:00-08:00",
	})
	want := []string{"precondition", "quiet_hours", "precondition_timeout"}
	if len(got) != len(want) {
		t.Fatalf("cronPatchOrder() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cronPatchOrder() = %v, want %v", got, want)
		}
	}
}
