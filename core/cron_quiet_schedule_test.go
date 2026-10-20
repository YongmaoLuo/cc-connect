package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// atClock builds a local-time instant on a fixed day at the given clock time.
// Using time.Date (instead of Add on midnight) keeps the wall-clock time
// exact even on days where the local zone shifts by an hour.
func atClock(hour, minute int) time.Time {
	return time.Date(2026, 1, 15, hour, minute, 0, 0, time.Local)
}

// TestCronQuietSchedule_Decision covers the six combinations the scheduler
// has to distinguish when a job comes due:
//
//  1. outside the quiet window      → run normally
//  2. shell job in the window       → skip (no warm variant exists)
//  3. new-session-per-run in window → skip (a fresh session is too costly)
//  4. no warm interval in window    → skip
//  5. interval not elapsed yet      → skip
//  6. interval elapsed              → warm up
//
// plus the "never ran before" case, which warms up immediately.
func TestCronQuietSchedule_Decision(t *testing.T) {
	// An instant inside the 23:00-08:00 window.
	quietNow := atClock(23, 30)
	// An instant outside any quiet window.
	dayNow := atClock(12, 0)

	tests := []struct {
		name string
		now  time.Time
		job  CronJob
		want quietAction
	}{
		{
			name: "outside the quiet window the job runs normally",
			now:  dayNow,
			job:  CronJob{QuietHours: "23:00-08:00", QuietIntervalMins: 60, LastRun: dayNow.Add(-3 * time.Hour)},
			want: quietRun,
		},
		{
			name: "no quiet window configured the job runs normally",
			now:  quietNow,
			job:  CronJob{QuietIntervalMins: 60, LastRun: quietNow.Add(-3 * time.Hour)},
			want: quietRun,
		},
		{
			name: "shell job inside the quiet window is skipped",
			now:  quietNow,
			job:  CronJob{Exec: "echo hi", QuietHours: "23:00-08:00", QuietIntervalMins: 60, LastRun: quietNow.Add(-3 * time.Hour)},
			want: quietSkip,
		},
		{
			name: "new session per run inside the quiet window is skipped",
			now:  quietNow,
			job:  CronJob{SessionMode: "new_per_run", QuietHours: "23:00-08:00", QuietIntervalMins: 60, LastRun: quietNow.Add(-3 * time.Hour)},
			want: quietSkip,
		},
		{
			name: "zero warm interval inside the quiet window is skipped",
			now:  quietNow,
			job:  CronJob{QuietHours: "23:00-08:00", LastRun: quietNow.Add(-3 * time.Hour)},
			want: quietSkip,
		},
		{
			name: "warm interval not elapsed yet is skipped",
			now:  quietNow,
			job:  CronJob{QuietHours: "23:00-08:00", QuietIntervalMins: 60, LastRun: quietNow.Add(-30 * time.Minute)},
			want: quietSkip,
		},
		{
			name: "warm interval elapsed warms the job up",
			now:  quietNow,
			job:  CronJob{QuietHours: "23:00-08:00", QuietIntervalMins: 60, LastRun: quietNow.Add(-3 * time.Hour)},
			want: quietWarm,
		},
		{
			name: "job that never ran warms up immediately",
			now:  quietNow,
			job:  CronJob{QuietHours: "23:00-08:00", QuietIntervalMins: 60},
			want: quietWarm,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := tt.job
			if got := job.quietActionAt(tt.now); got != tt.want {
				t.Errorf("quietActionAt(%s) = %v, want %v", tt.now.Format("15:04"), got, tt.want)
			}
		})
	}
}

// TestCronQuietSchedule_MidnightBoundary pins the window semantics: the
// window is [start, end), so the start minute belongs to the window while
// the end minute does not.
func TestCronQuietSchedule_MidnightBoundary(t *testing.T) {
	job := &CronJob{QuietHours: "23:00-08:00", QuietIntervalMins: 60}

	tests := []struct {
		name string
		now  time.Time
		want quietAction
	}{
		{name: "window start belongs to the window", now: atClock(23, 0), want: quietWarm},
		{name: "minute before window end belongs to the window", now: atClock(7, 59), want: quietWarm},
		{name: "window end is outside the window", now: atClock(8, 0), want: quietRun},
		{name: "midday is outside the window", now: atClock(12, 0), want: quietRun},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := job.quietActionAt(tt.now); got != tt.want {
				t.Errorf("quietActionAt(%s) = %v, want %v", tt.now.Format("15:04"), got, tt.want)
			}
		})
	}
}

// TestCronQuietWarmRun_Snapshot checks the warm-up snapshot: it carries the
// warm prompt (default or custom), is muted, keeps the job identity so the
// store can refresh LastRun, and never mutates the original job.
func TestCronQuietWarmRun_Snapshot(t *testing.T) {
	job := &CronJob{
		ID:     "warm1",
		Prompt: "do the real work",
	}

	snap := job.warmSnapshot()
	if snap == job {
		t.Fatal("warmSnapshot() returned the original job, want a copy")
	}
	if snap.Prompt != DefaultWarmPrompt {
		t.Errorf("snapshot Prompt = %q, want %q", snap.Prompt, DefaultWarmPrompt)
	}
	if !snap.Mute {
		t.Error("snapshot Mute = false, want true")
	}
	if snap.ID != job.ID {
		t.Errorf("snapshot ID = %q, want %q (the store must refresh the original job's LastRun)", snap.ID, job.ID)
	}
	if snap.SessionKey != job.SessionKey {
		t.Errorf("snapshot SessionKey = %q, want %q", snap.SessionKey, job.SessionKey)
	}

	// The original job must be untouched.
	if job.Prompt != "do the real work" {
		t.Errorf("original Prompt = %q, want %q", job.Prompt, "do the real work")
	}
	if job.Mute {
		t.Error("original Mute = true, want false")
	}

	// A custom warm prompt wins over the default one.
	custom := &CronJob{ID: "warm2", Prompt: "real work", WarmPrompt: "Reply OK only."}
	customSnap := custom.warmSnapshot()
	if customSnap.Prompt != "Reply OK only." {
		t.Errorf("snapshot Prompt = %q, want %q", customSnap.Prompt, "Reply OK only.")
	}
	if customSnap.Mute != true {
		t.Error("snapshot Mute = false, want true")
	}
	if custom.Prompt != "real work" {
		t.Errorf("original Prompt = %q, want %q", custom.Prompt, "real work")
	}
	if custom.WarmPrompt != "Reply OK only." {
		t.Errorf("original WarmPrompt = %q, want %q", custom.WarmPrompt, "Reply OK only.")
	}

	// Mutating the snapshot must not leak back into the original job.
	customSnap.Prompt = "mutated"
	customSnap.Mute = false
	customSnap.LastRun = time.Now()
	if custom.Prompt != "real work" || custom.Mute || !custom.LastRun.IsZero() {
		t.Errorf("original job mutated through the snapshot: prompt=%q mute=%t lastRun=%v",
			custom.Prompt, custom.Mute, custom.LastRun)
	}
}

// quietWindowAround returns a "HH:MM-HH:MM" spec whose window contains the
// given instant, so scheduler-level tests can exercise the quiet path no
// matter what the wall clock says. The window is two hours wide on both
// sides, which keeps the current minute inside it even if the clock ticks
// over a minute boundary while the test runs.
func quietWindowAround(now time.Time) string {
	start := now.Add(-2 * time.Hour)
	end := now.Add(2 * time.Hour)
	return fmt.Sprintf("%02d:%02d-%02d:%02d", start.Hour(), start.Minute(), end.Hour(), end.Minute())
}

// TestCronQuietSchedule_FireDueJobs exercises the scheduler path: a job that
// is skipped during quiet hours must not reach the model but must still have
// its nextRun advanced, and a job that is due for a warm-up must be executed
// with the warm prompt and without any chat output.
func TestCronQuietSchedule_FireDueJobs(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	platform := &stubCronReplyTargetPlatform{
		stubPlatformEngine: stubPlatformEngine{n: "discord"},
	}
	agentSession := newResultAgentSession("warm done")
	agent := &resultAgent{session: agentSession}

	cs := NewCronScheduler(store)
	e := NewEngine("test", agent, []Platform{platform}, "", LangEnglish)
	defer e.cancel()
	e.cronScheduler = cs
	cs.RegisterEngine("test", e)

	now := time.Now()
	window := quietWindowAround(now)

	skipped := &CronJob{
		ID:                "skipme",
		Project:           "test",
		SessionKey:        "discord:channel-1:user-1",
		CronExpr:          "* * * * *",
		Prompt:            "do the real work",
		Enabled:           true,
		CreatedAt:         now,
		QuietHours:        window,
		QuietIntervalMins: 60,
		LastRun:           now.Add(-30 * time.Minute),
	}
	warmed := &CronJob{
		ID:                "warmme",
		Project:           "test",
		SessionKey:        "discord:channel-2:user-2",
		CronExpr:          "* * * * *",
		Prompt:            "do the real work",
		Enabled:           true,
		CreatedAt:         now,
		QuietHours:        window,
		QuietIntervalMins: 60,
		LastRun:           now.Add(-3 * time.Hour),
	}
	for _, job := range []*CronJob{skipped, warmed} {
		if err := store.Add(job); err != nil {
			t.Fatal(err)
		}
		cs.entries[job.ID] = &cronEntry{
			jobID:    job.ID,
			schedule: mustParseStandardForTest(t, job.CronExpr),
			nextRun:  now.Add(-time.Hour),
		}
	}

	cs.fireDueJobs()

	// The skipped job must not have reached the model and must not have had
	// its LastRun refreshed.
	if got := store.Get("skipme").LastRun; !got.Equal(skipped.LastRun) {
		t.Errorf("skipped job LastRun = %v, want unchanged %v", got, skipped.LastRun)
	}
	for _, prompt := range agentSession.sentPrompts {
		if strings.Contains(prompt, "do the real work") {
			t.Errorf("skipped job reached the model with %q", prompt)
		}
	}

	// The skipped job must still have its nextRun advanced, otherwise the
	// scheduler would spin on it.
	cs.mu.RLock()
	skipEntry := cs.entries["skipme"]
	cs.mu.RUnlock()
	if skipEntry == nil {
		t.Fatal("entry skipme missing after fireDueJobs")
	}
	if !skipEntry.nextRun.After(now) {
		t.Errorf("skipped job nextRun = %v, want > %v", skipEntry.nextRun, now)
	}

	// The warmed job must have been executed with the warm prompt.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(agentSession.sentPrompts) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(agentSession.sentPrompts) != 1 {
		t.Fatalf("agent prompts = %#v, want exactly one warm prompt", agentSession.sentPrompts)
	}
	if !strings.Contains(agentSession.sentPrompts[0], DefaultWarmPrompt) {
		t.Errorf("agent prompt = %q, want it to contain %q", agentSession.sentPrompts[0], DefaultWarmPrompt)
	}
	if strings.Contains(agentSession.sentPrompts[0], "do the real work") {
		t.Errorf("agent prompt = %q, want the warm prompt instead of the job prompt", agentSession.sentPrompts[0])
	}

	// A warm run is muted: no chat output at all.
	if sent := platform.getSent(); len(sent) != 0 {
		t.Errorf("platform messages = %#v, want none (warm run is muted)", sent)
	}

	// The warm run refreshes LastRun, which is what rate-limits the next
	// warm-up inside the quiet window.
	if got := store.Get("warmme").LastRun; got.Before(warmed.LastRun) {
		t.Errorf("warmed job LastRun = %v, want a refreshed timestamp after %v", got, warmed.LastRun)
	}
}
