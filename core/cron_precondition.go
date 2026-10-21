package core

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultPreconditionTimeout bounds a precondition command when the job
	// does not configure a timeout of its own.
	DefaultPreconditionTimeout = 10 * time.Second
	// MaxPreconditionTimeout is the hard ceiling for a precondition command.
	// A job may not ask for more than this, no matter what it stores.
	MaxPreconditionTimeout = 300 * time.Second
	// PreconditionOutputLimit caps how much of a precondition command's output
	// is kept. The output exists only to explain a decision in the log, so a
	// chatty command must not be able to bloat it.
	PreconditionOutputLimit = 4096
)

// preconditionWaitDelay is the grace period the executor gives the command's
// output pipes after the command itself was killed. Without it a grandchild
// that inherited the pipes (e.g. "sleep 60 &") could keep the executor
// blocked long past the timeout.
const preconditionWaitDelay = 200 * time.Millisecond

// PreconditionTimeoutDuration resolves the effective timeout of a job's
// precondition command. An unset (or nonsensical) timeout falls back to
// DefaultPreconditionTimeout, and anything above the ceiling is clamped to
// MaxPreconditionTimeout.
func PreconditionTimeoutDuration(job *CronJob) time.Duration {
	if job == nil || job.PreconditionTimeout <= 0 {
		return DefaultPreconditionTimeout
	}
	d := time.Duration(job.PreconditionTimeout) * time.Second
	if d > MaxPreconditionTimeout {
		return MaxPreconditionTimeout
	}
	return d
}

// preconditionEnv builds the CC_CRON_* variables handed to a precondition
// command. They are the only channel through which the command learns which job
// it guards and when that job last ran — the daemon's own environment knows
// nothing of either.
//
// A job that has never run reports CC_CRON_LAST_RUN=0 rather than an empty
// value, so a condition can branch on it without first testing for emptiness.
func preconditionEnv(job *CronJob, now time.Time) []string {
	lastRun := "0"
	if !job.LastRun.IsZero() {
		lastRun = strconv.FormatInt(job.LastRun.Unix(), 10)
	}
	return []string{
		"CC_CRON_JOB_ID=" + job.ID,
		"CC_CRON_JOB_DESC=" + job.Description,
		"CC_CRON_PROJECT=" + job.Project,
		"CC_CRON_SESSION_KEY=" + job.SessionKey,
		"CC_CRON_LAST_RUN=" + lastRun,
		"CC_CRON_NOW=" + strconv.FormatInt(now.Unix(), 10),
	}
}

// truncatePreconditionOutput caps a precondition command's output at
// PreconditionOutputLimit, marking anything dropped so a reader never mistakes
// a clipped log line for the whole story.
func truncatePreconditionOutput(s string) string {
	if len(s) <= PreconditionOutputLimit {
		return s
	}
	return s[:PreconditionOutputLimit] + "...(truncated)"
}

// runPrecondition runs the job's precondition command and reports whether the
// job may proceed.
//
// The command sees the job's identity and its last-run timestamp through the
// CC_CRON_* environment variables (see preconditionEnv), and its output comes
// back capped at PreconditionOutputLimit. The output is only ever logged — it
// never reaches a message or the job's stored result.
//
// The exit code decides:
//   - 0        → the gate passes, the job runs.
//   - 1 – 125  → the gate blocks, the job is skipped (the only skip path).
//   - 126/127  → the command could not be executed (not executable / not
//     found); fails open so a typo never silently blocks a job.
//   - signal   → the command was killed (typically by the timeout); fails open.
//
// A command that cannot even be started also fails open. An empty command is
// a no-op: no process is spawned and the job is allowed to run.
func runPrecondition(job *CronJob, now time.Time) (bool, int, string) {
	if job == nil || strings.TrimSpace(job.Precondition) == "" {
		return true, 0, ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), PreconditionTimeoutDuration(job))
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", job.Precondition)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", job.Precondition)
	}
	if job.WorkDir != "" {
		cmd.Dir = job.WorkDir
	}
	cmd.Env = append(os.Environ(), preconditionEnv(job, now)...)
	cmd.WaitDelay = preconditionWaitDelay

	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, 0, truncatePreconditionOutput(string(out))
	}

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if code >= 1 && code <= 125 {
			return false, code, truncatePreconditionOutput(string(out))
		}
		// 126/127 (cannot execute / not found) or a negative code (killed by
		// a signal, e.g. the timeout): fail open.
		slog.Warn("cron: precondition failed open",
			"id", job.ID, "exit_code", code, "at", now.Format(time.RFC3339),
			"output", truncateStr(truncatePreconditionOutput(string(out)), 200))
		return true, code, truncatePreconditionOutput(string(out))
	}

	// The command never ran (bad shell, permissions, ...): fail open.
	slog.Warn("cron: precondition could not run, failing open",
		"id", job.ID, "at", now.Format(time.RFC3339), "error", err)
	return true, -1, err.Error()
}