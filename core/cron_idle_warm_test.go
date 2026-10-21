package core

import (
	"testing"
	"time"
)

// idleWarmCase describes one idle warm-up scenario: what the precondition
// reports, how long ago the job last ran, and how the job is configured.
type idleWarmCase struct {
	name         string
	precondition string
	lastRun      func(now time.Time) time.Time // nil = the job never ran
	sessionMode  string
	exec         string
	quietMins    int
	wantRuns     int // how many times the engine ran the job
	wantWarmRuns int // how many of those runs were muted warm-ups
}

// TestCronIdleWarm drives the scheduler the way the daemon does — through
// fireDueJobs, the production path — and pins the three-way behaviour of the
// idle warm-up:
//
//   - a precondition that reports an update (exit 0) runs the job normally,
//     with the job's own prompt and unmuted;
//   - a precondition that reports no change (exit 1) skips the job, but a
//     muted warm-up ping still fires once the idle interval has elapsed since
//     the last run;
//   - a job that cannot be warmed up (shell job, fresh session per run,
//     interval off) is simply skipped.
//
// The warm-up and the real run are told apart by what the engine observed:
// the warm-up carries the warm prompt and is muted, so it never resolves a
// reply target and never writes to the chat; the real run carries the job's
// prompt and does both.
func TestCronIdleWarm(t *testing.T) {
	now := time.Now()

	cases := []idleWarmCase{
		{
			name:         "never ran: the first blocked tick warms up",
			precondition: "exit 1",
			quietMins:    120,
			wantRuns:     1,
			wantWarmRuns: 1,
		},
		{
			name:         "ran 30 minutes ago: the idle interval has not elapsed",
			precondition: "exit 1",
			lastRun:      func(now time.Time) time.Time { return now.Add(-30 * time.Minute) },
			quietMins:    120,
			wantRuns:     0,
		},
		{
			name:         "idle for 3 hours: warms up",
			precondition: "exit 1",
			lastRun:      func(now time.Time) time.Time { return now.Add(-3 * time.Hour) },
			quietMins:    120,
			wantRuns:     1,
			wantWarmRuns: 1,
		},
		{
			name:         "the precondition reports an update: the job runs normally",
			precondition: "exit 0",
			lastRun:      func(now time.Time) time.Time { return now.Add(-3 * time.Hour) },
			quietMins:    120,
			wantRuns:     1,
		},
		{
			name:         "a fresh session per run is never warmed up",
			precondition: "exit 1",
			lastRun:      func(now time.Time) time.Time { return now.Add(-3 * time.Hour) },
			sessionMode:  "new_per_run",
			quietMins:    120,
			wantRuns:     0,
		},
		{
			name:         "a shell job has no warm variant",
			precondition: "exit 1",
			lastRun:      func(now time.Time) time.Time { return now.Add(-3 * time.Hour) },
			exec:         "echo hi",
			quietMins:    120,
			wantRuns:     0,
		},
		{
			name:         "an interval of zero turns the feature off",
			precondition: "exit 1",
			lastRun:      func(now time.Time) time.Time { return now.Add(-3 * time.Hour) },
			quietMins:    0,
			wantRuns:     0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, cs, recorder, platform := newPreconditionScheduleFixture(t)

			job := &CronJob{
				ID:                "idle-warm",
				Project:           "test",
				SessionKey:        "discord:channel-1:user-1",
				CronExpr:          "* * * * *",
				Prompt:            "real work",
				Exec:              tc.exec,
				Enabled:           true,
				CreatedAt:         now,
				SessionMode:       tc.sessionMode,
				QuietIntervalMins: tc.quietMins,
				Precondition:      tc.precondition,
			}
			if tc.lastRun != nil {
				job.LastRun = tc.lastRun(now)
			}
			// The store keeps the very same pointer, so the previous
			// timestamp has to be captured before the run refreshes it.
			lastRunBefore := job.LastRun
			addDueJob(t, cs, store, job, now)

			cs.fireDueJobs()

			// count("") matches every recorded prompt: strings.Contains
			// always succeeds on the empty substring, so it is the total
			// number of times the engine reached the model.
			total := waitForIdleWarmRuns(recorder, tc.wantRuns)
			wantRealRuns := tc.wantRuns - tc.wantWarmRuns

			if total != tc.wantRuns {
				t.Errorf("engine runs = %d, want %d", total, tc.wantRuns)
			}
			if got := recorder.count("real work"); got != wantRealRuns {
				t.Errorf("prompts containing the job prompt = %d, want %d", got, wantRealRuns)
			}
			if got := recorder.count(DefaultWarmPrompt); got != tc.wantWarmRuns {
				t.Errorf("prompts containing the warm prompt = %d, want %d", got, tc.wantWarmRuns)
			}

			// A muted run never resolves a reply target (the engine skips
			// that for a muted job) and never writes to the chat; an
			// unmuted run does both.
			if wantRealRuns == 1 {
				if platform.callCount() == 0 {
					t.Error("an unmuted run must resolve a cron reply target")
				}
				if len(platform.getSent()) == 0 {
					t.Error("an unmuted run must write to the chat")
				}
			} else {
				if platform.callCount() != 0 {
					t.Errorf("cron reply target resolutions = %d, want 0 (a muted warm-up resolves none)", platform.callCount())
				}
				if sent := platform.getSent(); len(sent) != 0 {
					t.Errorf("chat messages = %#v, want none (a muted warm-up writes nothing)", sent)
				}
			}

			// A run refreshes LastRun — that is what rate-limits the next
			// warm-up — while a skipped job leaves it alone.
			gotLastRun := store.Get("idle-warm").LastRun
			if tc.wantRuns == 0 {
				if !gotLastRun.Equal(lastRunBefore) {
					t.Errorf("LastRun = %v, want unchanged %v (a skipped job is not a run)", gotLastRun, lastRunBefore)
				}
			} else if !gotLastRun.After(lastRunBefore) {
				t.Errorf("LastRun = %v, want a refreshed timestamp after %v", gotLastRun, lastRunBefore)
			}
		})
	}
}

// waitForIdleWarmRuns waits until the engine has recorded the expected number
// of prompts, and returns how many were recorded. The engine runs the agent
// in a goroutine, so a run is not necessarily over when fireDueJobs returns;
// when no run is expected the helper settles first, so a late prompt cannot
// slip past the assertion.
func waitForIdleWarmRuns(recorder *preconditionRecorder, want int) int {
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := recorder.count("")
		if got >= want {
			if want == 0 {
				// Nothing is expected: give a late prompt the chance to
				// show up before the caller asserts that there was none.
				time.Sleep(100 * time.Millisecond)
				return recorder.count("")
			}
			return got
		}
		if time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCronIdleWarmDue covers the decision itself: which jobs are eligible for
// an idle warm-up, and when the interval has elapsed.
func TestCronIdleWarmDue(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name string
		job  *CronJob
		want bool
	}{
		{
			name: "a job that never ran is due immediately",
			job:  &CronJob{QuietIntervalMins: 120},
			want: true,
		},
		{
			name: "the interval has elapsed",
			job:  &CronJob{QuietIntervalMins: 120, LastRun: now.Add(-3 * time.Hour)},
			want: true,
		},
		{
			name: "the interval has not elapsed yet",
			job:  &CronJob{QuietIntervalMins: 120, LastRun: now.Add(-30 * time.Minute)},
			want: false,
		},
		{
			name: "the interval is exactly elapsed",
			job:  &CronJob{QuietIntervalMins: 120, LastRun: now.Add(-120 * time.Minute)},
			want: true,
		},
		{
			name: "a shell job has no warm variant",
			job:  &CronJob{Exec: "echo hi", QuietIntervalMins: 120, LastRun: now.Add(-3 * time.Hour)},
			want: false,
		},
		{
			name: "a fresh session per run is too expensive to ping",
			job:  &CronJob{SessionMode: "new_per_run", QuietIntervalMins: 120, LastRun: now.Add(-3 * time.Hour)},
			want: false,
		},
		{
			name: "an interval of zero turns the feature off",
			job:  &CronJob{QuietIntervalMins: 0, LastRun: now.Add(-3 * time.Hour)},
			want: false,
		},
		{
			name: "a nil job is not due",
			job:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.job.idleWarmDue(now); got != tt.want {
				t.Errorf("idleWarmDue() = %t, want %t", got, tt.want)
			}
		})
	}
}
