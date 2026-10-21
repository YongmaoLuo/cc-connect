package core

import (
	"fmt"
	"os"
	"path/filepath"
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

// TestCronQuietSchedule_Decision covers the combinations the scheduler has
// to distinguish when a job comes due:
//
//  1. outside the quiet window       → run normally
//  2. no window configured           → run normally
//  3. inside the window, interval elapsed (plain, shell or new-per-run) → run
//  4. inside the window, interval not elapsed yet                   → skip
//  5. inside the window, no interval configured                     → skip
//
// plus the "never ran before" case, which runs immediately.
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
			name: "shell job inside the quiet window is rate limited like any other",
			now:  quietNow,
			job:  CronJob{Exec: "echo hi", QuietHours: "23:00-08:00", QuietIntervalMins: 60, LastRun: quietNow.Add(-3 * time.Hour)},
			want: quietRun,
		},
		{
			name: "shell job inside the quiet window is skipped while the interval is pending",
			now:  quietNow,
			job:  CronJob{Exec: "echo hi", QuietHours: "23:00-08:00", QuietIntervalMins: 60, LastRun: quietNow.Add(-30 * time.Minute)},
			want: quietSkip,
		},
		{
			name: "new session per run inside the quiet window is rate limited like any other",
			now:  quietNow,
			job:  CronJob{SessionMode: "new_per_run", QuietHours: "23:00-08:00", QuietIntervalMins: 60, LastRun: quietNow.Add(-3 * time.Hour)},
			want: quietRun,
		},
		{
			name: "new session per run inside the quiet window is skipped while the interval is pending",
			now:  quietNow,
			job:  CronJob{SessionMode: "new_per_run", QuietHours: "23:00-08:00", QuietIntervalMins: 60, LastRun: quietNow.Add(-30 * time.Minute)},
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
			name: "warm interval elapsed lets the job run",
			now:  quietNow,
			job:  CronJob{QuietHours: "23:00-08:00", QuietIntervalMins: 60, LastRun: quietNow.Add(-3 * time.Hour)},
			want: quietRun,
		},
		{
			name: "job that never ran runs immediately",
			now:  quietNow,
			job:  CronJob{QuietHours: "23:00-08:00", QuietIntervalMins: 60},
			want: quietRun,
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
		{name: "window start belongs to the window", now: atClock(23, 0), want: quietRun},
		{name: "minute before window end belongs to the window", now: atClock(7, 59), want: quietRun},
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

// quietWindowAway returns a "HH:MM-HH:MM" spec whose window provably does not
// contain the given instant: it opens ten hours later and stays open for four.
func quietWindowAway(now time.Time) string {
	start := now.Add(10 * time.Hour)
	end := now.Add(14 * time.Hour)
	return fmt.Sprintf("%02d:%02d-%02d:%02d", start.Hour(), start.Minute(), end.Hour(), end.Minute())
}

// TestCronQuietSchedule_FireDueJobs exercises the scheduler path: a job that
// is rate-limited inside the quiet-hours window must not reach the model but
// must still have its nextRun advanced, and a job whose tick survives the
// rate limit must go through the normal decision — a blocked precondition
// turns it into a muted warm-up ping.
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
	// The interval has elapsed, so the tick is let through — and the
	// precondition then blocks it, which is the only path that still pings.
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
		Precondition:      "exit 1",
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

// quietRatelimitCase describes one scheduler-level scenario for the
// quiet-hours rate limit: how the job is configured, and what the engine must
// have been allowed to do with the tick.
type quietRatelimitCase struct {
	name string
	// window is the quiet_hours spec. Empty means "the window contains now";
	// windowAway picks a window that provably does not.
	window     string
	windowAway bool

	// interval is QuietIntervalMins; lastRun is when the job last ran
	// (nil = it never ran).
	interval int
	lastRun  func(now time.Time) time.Time

	// precond is the job's gate: "" leaves the job ungated, "allow" gates it
	// with a passing command, and "block" gates it with a command that exits
	// 1. Both leave a marker file behind, so "the gate ran" is observable
	// whichever way it decided.
	precond string

	// exec turns the job into a shell job whose command leaves a marker file
	// behind, so "the command ran" is directly observable.
	exec bool

	wantRealRuns int  // prompts carrying the job's own prompt
	wantWarmRuns int  // prompts carrying DefaultWarmPrompt
	wantChat     bool // the run wrote to the chat (i.e. it was not muted)
	wantPrecond  bool // the precondition executed
	wantExec     bool // the shell command executed
	wantLastRun  bool // LastRun advanced
}

// runQuietRatelimitCase drives one case through fireDueJobs — the production
// path the daemon takes — and asserts what the engine observed.
func runQuietRatelimitCase(t *testing.T, tc quietRatelimitCase, now time.Time) {
	t.Helper()
	store, cs, recorder, platform := newPreconditionScheduleFixture(t)

	dir := t.TempDir()
	precondMarker := filepath.Join(dir, "precondition-ran")
	execMarker := filepath.Join(dir, "exec-ran")

	job := &CronJob{
		ID:                "ratelimit",
		Project:           "test",
		SessionKey:        "discord:channel-1:user-1",
		CronExpr:          "* * * * *",
		Prompt:            "real work",
		Enabled:           true,
		CreatedAt:         now,
		QuietHours:        tc.window,
		QuietIntervalMins: tc.interval,
	}
	if job.QuietHours == "" {
		job.QuietHours = quietWindowAround(now)
	}
	if tc.windowAway {
		job.QuietHours = quietWindowAway(now)
	}
	if tc.lastRun != nil {
		job.LastRun = tc.lastRun(now)
	}
	switch tc.precond {
	case "allow":
		job.Precondition = fmt.Sprintf("touch %q; exit 0", precondMarker)
	case "block":
		job.Precondition = fmt.Sprintf("test -f %q || (touch %q; exit 1)", precondMarker, precondMarker)
	}
	if tc.exec {
		job.Exec = fmt.Sprintf("touch %q", execMarker)
		job.Prompt = ""
	}
	lastRunBefore := job.LastRun

	addDueJob(t, cs, store, job, now)

	cs.fireDueJobs()

	// count("") matches every recorded prompt: strings.Contains always
	// succeeds on the empty substring, so it is the number of times the
	// engine reached the model.
	wantRuns := tc.wantRealRuns + tc.wantWarmRuns
	if got := waitForIdleWarmRuns(recorder, wantRuns); got != wantRuns {
		t.Errorf("engine runs = %d, want %d", got, wantRuns)
	}
	if got := recorder.count("real work"); got != tc.wantRealRuns {
		t.Errorf("prompts carrying the job prompt = %d, want %d", got, tc.wantRealRuns)
	}
	if got := recorder.count(DefaultWarmPrompt); got != tc.wantWarmRuns {
		t.Errorf("prompts carrying the warm prompt = %d, want %d", got, tc.wantWarmRuns)
	}

	// A muted run writes nothing to the chat; an unmuted one does.
	if sent := platform.getSent(); tc.wantChat != (len(sent) > 0) {
		t.Errorf("chat messages = %#v, want chat output: %t", sent, tc.wantChat)
	}

	// The marker files make "the gate ran" and "the command ran" observable
	// without trusting a log line.
	if got := fileExists(precondMarker); got != tc.wantPrecond {
		t.Errorf("precondition executed = %t, want %t", got, tc.wantPrecond)
	}
	if got := fileExists(execMarker); got != tc.wantExec {
		t.Errorf("shell command executed = %t, want %t", got, tc.wantExec)
	}

	// A run refreshes LastRun — that is what rate-limits the next tick —
	// while a dropped tick leaves it alone.
	gotLastRun := store.Get("ratelimit").LastRun
	if tc.wantLastRun {
		if !gotLastRun.After(lastRunBefore) {
			t.Errorf("LastRun = %v, want a refreshed timestamp after %v", gotLastRun, lastRunBefore)
		}
	} else if !gotLastRun.Equal(lastRunBefore) {
		t.Errorf("LastRun = %v, want unchanged %v (a dropped tick is not a run)", gotLastRun, lastRunBefore)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestCronQuietSchedule_RateLimitedWindow pins the new quiet-hours semantics
// end to end, through fireDueJobs: the window no longer suppresses the job,
// it rate-limits it to one tick per quiet_interval_mins.
func TestCronQuietSchedule_RateLimitedWindow(t *testing.T) {
	elapsed := func(now time.Time) time.Time { return now.Add(-3 * time.Hour) }
	pending := func(now time.Time) time.Time { return now.Add(-30 * time.Minute) }

	cases := []quietRatelimitCase{
		{
			name: "in the window, interval elapsed, precondition allows: the real job runs",
			// The tick survives the rate limit, the gate lets it through, so
			// this is an ordinary unmuted run — job prompt, chat output.
			interval:     60,
			lastRun:      elapsed,
			precond:      "allow",
			wantRealRuns: 1,
			wantChat:     true,
			wantPrecond:  true,
			wantLastRun:  true,
		},
		{
			name: "in the window, interval elapsed, precondition blocks: a muted warm ping runs",
			// Same rate-limited tick, but the gate reports no change — the
			// only remaining warm-up path.
			interval:     60,
			lastRun:      elapsed,
			precond:      "block",
			wantWarmRuns: 1,
			wantPrecond:  true,
			wantLastRun:  true,
		},
		{
			name: "in the window, interval not elapsed: nothing runs, not even the precondition",
			// The dropped tick must be side-effect free: the gate never
			// executes, so the marker it would have left never appears.
			interval:    60,
			lastRun:     pending,
			precond:     "block",
			wantPrecond: false,
			wantLastRun: false,
		},
		{
			name: "in the window, shell job, interval elapsed: the command runs",
			// Shell jobs used to be skipped outright inside the window.
			interval:    60,
			lastRun:     elapsed,
			exec:        true,
			wantChat:    true,
			wantExec:    true,
			wantLastRun: true,
		},
		{
			name:        "in the window, shell job, interval not elapsed: the command does not run",
			interval:    60,
			lastRun:     pending,
			exec:        true,
			wantPrecond: false,
			wantLastRun: false,
		},
		{
			name: "in the window with no interval configured: skipped however long ago it ran",
			// quiet_interval_mins = 0 keeps the old semantics: a window on
			// its own holds the job back for the whole window.
			interval:    0,
			lastRun:     func(now time.Time) time.Time { return now.Add(-10 * 24 * time.Hour) },
			precond:     "block",
			wantPrecond: false,
			wantLastRun: false,
		},
		{
			name: "outside the window, interval not elapsed: the job still runs",
			// Regression guard: the rate limit must not leak outside the
			// window.
			windowAway:   true,
			interval:     60,
			lastRun:      pending,
			wantRealRuns: 1,
			wantChat:     true,
			wantLastRun:  true,
		},
	}

	now := time.Now()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runQuietRatelimitCase(t, tc, now)
		})
	}
}
