package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// preconditionRecorder collects the prompts the engine handed to the agent.
// The mutex keeps it safe against the goroutine the scheduler runs a job on.
type preconditionRecorder struct {
	mu      sync.Mutex
	prompts []string
}

func (r *preconditionRecorder) record(prompt string) {
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	r.mu.Unlock()
}

// count returns how many of the recorded prompts contain sub.
func (r *preconditionRecorder) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.prompts {
		if strings.Contains(p, sub) {
			n++
		}
	}
	return n
}

// preconditionRecordingSession is an AgentSession that records the prompt it
// receives and answers with a canned result. Every session owns its event
// channel, so two jobs running side by side cannot steal each other's result.
type preconditionRecordingSession struct {
	stubAgentSession
	events   chan Event
	recorder *preconditionRecorder
}

func newPreconditionRecordingSession(recorder *preconditionRecorder) *preconditionRecordingSession {
	return &preconditionRecordingSession{
		events:   make(chan Event, 1),
		recorder: recorder,
	}
}

func (s *preconditionRecordingSession) Send(prompt string, _ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.recorder.record(prompt)
	s.events <- Event{Type: EventResult, Content: "done", Done: true}
	return nil
}

func (s *preconditionRecordingSession) Events() <-chan Event     { return s.events }
func (s *preconditionRecordingSession) CurrentSessionID() string { return "precondition-session" }
func (s *preconditionRecordingSession) Alive() bool              { return true }

// preconditionRecordingAgent hands out a fresh recording session per call, so
// two jobs never share an event channel.
type preconditionRecordingAgent struct {
	recorder *preconditionRecorder
}

func (a *preconditionRecordingAgent) Name() string { return "stub" }
func (a *preconditionRecordingAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	return newPreconditionRecordingSession(a.recorder), nil
}
func (a *preconditionRecordingAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *preconditionRecordingAgent) Stop() error { return nil }

// preconditionCountingPlatform counts the cron executions the engine started.
// ExecuteCronJob resolves the cron reply target before it does anything else,
// so the counter is a direct "the engine was called" signal.
type preconditionCountingPlatform struct {
	stubPlatformEngine
	mu    sync.Mutex
	calls int
}

func (p *preconditionCountingPlatform) ReconstructReplyCtx(string) (any, error) {
	return "base-rctx", nil
}

func (p *preconditionCountingPlatform) ResolveCronReplyTarget(string, string) (string, any, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	return "discord:thread-fresh", "fresh-rctx", nil
}

func (p *preconditionCountingPlatform) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// newPreconditionScheduleFixture wires a scheduler over a throwaway store and
// an engine whose agent records the prompts it receives.
func newPreconditionScheduleFixture(t *testing.T) (*CronStore, *CronScheduler, *preconditionRecorder, *preconditionCountingPlatform) {
	t.Helper()
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	platform := &preconditionCountingPlatform{stubPlatformEngine: stubPlatformEngine{n: "discord"}}
	recorder := &preconditionRecorder{}
	agent := &preconditionRecordingAgent{recorder: recorder}

	cs := NewCronScheduler(store)
	e := NewEngine("test", agent, []Platform{platform}, "", LangEnglish)
	t.Cleanup(e.cancel)
	e.cronScheduler = cs
	cs.RegisterEngine("test", e)
	return store, cs, recorder, platform
}

// addDueJob stores a job and schedules it so that it is already due.
func addDueJob(t *testing.T, cs *CronScheduler, store *CronStore, job *CronJob, now time.Time) {
	t.Helper()
	if err := store.Add(job); err != nil {
		t.Fatal(err)
	}
	cs.entries[job.ID] = &cronEntry{
		jobID:    job.ID,
		schedule: mustParseStandardForTest(t, job.CronExpr),
		nextRun:  now.Add(-time.Hour),
	}
}

// TestCronPreconditionScheduleGate pins the scheduler-side gate: a job whose
// precondition passes runs exactly once, a job whose precondition blocks never
// reaches the model.
func TestCronPreconditionScheduleGate(t *testing.T) {
	store, cs, recorder, platform := newPreconditionScheduleFixture(t)

	now := time.Now()
	allowed := &CronJob{
		ID:           "allowed",
		Project:      "test",
		SessionKey:   "discord:channel-1:user-1",
		CronExpr:     "* * * * *",
		Prompt:       "allowed prompt",
		Enabled:      true,
		CreatedAt:    now,
		Precondition: "true",
	}
	blocked := &CronJob{
		ID:           "blocked",
		Project:      "test",
		SessionKey:   "discord:channel-2:user-2",
		CronExpr:     "* * * * *",
		Prompt:       "blocked prompt",
		Enabled:      true,
		CreatedAt:    now,
		Precondition: "exit 7",
	}
	addDueJob(t, cs, store, allowed, now)
	addDueJob(t, cs, store, blocked, now)

	cs.fireDueJobs()

	if got := recorder.count("allowed prompt"); got != 1 {
		t.Errorf("prompts containing %q = %d, want 1", "allowed prompt", got)
	}
	if got := recorder.count("blocked prompt"); got != 0 {
		t.Errorf("prompts containing %q = %d, want 0 (the precondition blocked the job)", "blocked prompt", got)
	}
	if got := platform.callCount(); got != 1 {
		t.Errorf("engine invocations = %d, want 1 (only the allowed job may run)", got)
	}
}

// TestCronPreconditionScheduleSkip pins what a blocked job must NOT do: it
// must not reach the model, must not refresh LastRun, and must still have its
// nextRun advanced so the scheduler does not spin on it.
func TestCronPreconditionScheduleSkip(t *testing.T) {
	store, cs, recorder, platform := newPreconditionScheduleFixture(t)

	now := time.Now()
	lastRun := now.Add(-time.Hour)
	blocked := &CronJob{
		ID:           "blocked",
		Project:      "test",
		SessionKey:   "discord:channel-1:user-1",
		CronExpr:     "* * * * *",
		Prompt:       "blocked prompt",
		Enabled:      true,
		CreatedAt:    now,
		LastRun:      lastRun,
		Precondition: "exit 1",
	}
	addDueJob(t, cs, store, blocked, now)

	cs.fireDueJobs()

	if got := platform.callCount(); got != 0 {
		t.Errorf("engine invocations = %d, want 0 (the precondition blocked the job)", got)
	}
	if got := recorder.count("blocked prompt"); got != 0 {
		t.Errorf("prompts containing %q = %d, want 0", "blocked prompt", got)
	}
	if got := store.Get("blocked").LastRun; !got.Equal(lastRun) {
		t.Errorf("LastRun = %v, want unchanged %v", got, lastRun)
	}
	if got := store.Get("blocked").LastError; got != "" {
		t.Errorf("LastError = %q, want empty (a skipped job is not a failed one)", got)
	}

	// A skipped job still has its schedule advanced, otherwise the scheduler
	// would spin on it.
	cs.mu.RLock()
	entry := cs.entries["blocked"]
	cs.mu.RUnlock()
	if entry == nil {
		t.Fatal("entry blocked missing after fireDueJobs")
	}
	if !entry.nextRun.After(now) {
		t.Errorf("nextRun = %v, want > %v", entry.nextRun, now)
	}
}

// TestCronPreconditionScheduleQuiet pins that the precondition gate belongs to
// the normal run path only. Inside the quiet-hours window a due tick is either
// skipped or warmed up, and neither of them may run the precondition:
//
//   - a skipped tick must be completely side-effect free — not even the
//     precondition's own side effects may happen;
//   - a warm tick (the keep-alive ping) must run even when the precondition
//     would block, because the condition guards the real work, not the ping.
//
// The third job is the control: it fires outside the quiet window, so its
// precondition does run. It proves two things at once — that the precondition
// is effective on the run path (the job is blocked) and that the "touch"
// command used by the skipped job really creates a file, so the skipped
// job's assertion below is not vacuous.
func TestCronPreconditionScheduleQuiet(t *testing.T) {
	store, cs, recorder, _ := newPreconditionScheduleFixture(t)

	now := time.Now()
	window := quietWindowAround(now)

	skipSideEffect := filepath.Join(t.TempDir(), "quiet-skip-side-effect")
	controlSideEffect := filepath.Join(t.TempDir(), "control-run-side-effect")

	skipped := &CronJob{
		ID:                "quiet-skip",
		Project:           "test",
		SessionKey:        "discord:channel-1:user-1",
		CronExpr:          "* * * * *",
		Prompt:            "skipped real prompt",
		Enabled:           true,
		CreatedAt:         now,
		QuietHours:        window,
		QuietIntervalMins: 0, // no warm interval inside the window → skip
		Precondition:      "touch '" + skipSideEffect + "'; exit 1",
	}
	warmed := &CronJob{
		ID:                "quiet-warm",
		Project:           "test",
		SessionKey:        "discord:channel-2:user-2",
		CronExpr:          "* * * * *",
		Prompt:            "warmed real prompt",
		Enabled:           true,
		CreatedAt:         now,
		QuietHours:        window,
		QuietIntervalMins: 60,
		LastRun:           now.Add(-3 * time.Hour), // warm interval elapsed → warm
		WarmPrompt:        "warm ping",
		Precondition:      "exit 1",
	}
	control := &CronJob{
		ID:           "control-run",
		Project:      "test",
		SessionKey:   "discord:channel-3:user-3",
		CronExpr:     "* * * * *",
		Prompt:       "control real prompt",
		Enabled:      true,
		CreatedAt:    now,
		Precondition: "touch '" + controlSideEffect + "'; exit 1",
	}
	addDueJob(t, cs, store, skipped, now)
	addDueJob(t, cs, store, warmed, now)
	addDueJob(t, cs, store, control, now)

	cs.fireDueJobs()

	// Control: outside the quiet window the precondition runs, so its side
	// effect exists and the job itself is blocked.
	if _, err := os.Stat(controlSideEffect); err != nil {
		t.Fatalf("control precondition side effect %q missing (stat err = %v); the precondition did not run on the run path, so the assertions below would be vacuous",
			controlSideEffect, err)
	}
	if got := recorder.count("control real prompt"); got != 0 {
		t.Errorf("prompts containing %q = %d, want 0 (the precondition blocked the job)", "control real prompt", got)
	}

	// The skipped tick must not have run the precondition: no side effect.
	if _, err := os.Stat(skipSideEffect); !os.IsNotExist(err) {
		t.Errorf("precondition side effect %q exists (stat err = %v), want a skipped tick to have no side effects at all",
			skipSideEffect, err)
	}
	// The skipped tick must not have reached the model either.
	if got := recorder.count("skipped real prompt"); got != 0 {
		t.Errorf("prompts containing %q = %d, want 0 (a skipped tick must not run)", "skipped real prompt", got)
	}
	// A skipped tick leaves the job's bookkeeping alone.
	if got := store.Get("quiet-skip").LastRun; !got.IsZero() {
		t.Errorf("skipped job LastRun = %v, want zero (a skipped tick is not a run)", got)
	}
	if got := store.Get("quiet-skip").LastError; got != "" {
		t.Errorf("skipped job LastError = %q, want empty (a skipped tick is not a failed one)", got)
	}

	// The warm tick runs even though its precondition would block it: the
	// keep-alive ping must not be gated by a condition that guards the real
	// work.
	if got := recorder.count("warm ping"); got != 1 {
		t.Errorf("prompts containing %q = %d, want 1 (a warm run must not be gated by the precondition)", "warm ping", got)
	}
	// The warm run uses the warm prompt, not the real one.
	if got := recorder.count("warmed real prompt"); got != 0 {
		t.Errorf("prompts containing %q = %d, want 0 (the warm run must use the warm prompt)", "warmed real prompt", got)
	}
}