# Cron Precondition — End-to-End Verification

Acceptance run for the cron **precondition gate** in the cc-connect management
API: **HTTP request → management API → cron store → read back → PATCH → read
back again**.

Upstream decision points covered:

- arch decision 8 — the E2E runs as an independent `go test` binary against a
  throwaway store on a fixed port, and never touches `~/.cc-connect`
- arch decision 9 — this verification document is written to disk
- test decision 5 — the E2E assertions are the evidence for the precondition
  fields surviving the whole round trip

- **Date:** 2026-10-20
- **Branch:** `feat/cron-precondition`
- **Result:** 13 assertion groups, 13 PASS, 0 FAIL (+ 3 environment checks,
  3 PASS)

---

## 1. How the run works

**No browser is involved.** This run does **not** use Playwright, does **not**
use Puppeteer, and does **not** start any browser process. This repository
carries no frontend test dependency, so "user input" is simulated with plain
HTTP requests against the real management API — the same management server
code the web admin talks to. The browser-based counterpart of this acceptance
is [`cron-quiet-hours-e2e.md`](cron-quiet-hours-e2e.md); this document is its
HTTP-only counterpart.

```
┌────────────────────────────────────────────────────────────┐
│ go test binary (tests/e2e/cron_precondition_e2e_test.go)   │
│                                                            │
│   core.NewCronStore(t.TempDir())   ← throwaway store       │
│   core.NewCronScheduler(store)                            │
│   core.NewManagementServer(:19820)  ← real HTTP listener   │
│                                                            │
│   POST /api/v1/cron ─► GET /api/v1/cron ─► PATCH ─► GET     │
└────────────────────────────────────────────────────────────┘
```

### 1.1 Start method

The fixture **starts inside the test process** — there is nothing to launch
beforehand and no separate server to keep alive:

| Item | Value |
|---|---|
| Entry point | `TestCronPreconditionE2E` in `tests/e2e/cron_precondition_e2e_test.go` |
| Build tag | `webui_e2e` |
| Server | `core.NewManagementServer` started in-process by the test |
| Ports | `19820`, falling back to `19821` when `19820` is busy (the helper logs `port 19820 is busy, trying the next one`) |
| Recorded run | bound `19820` (`management api started port=19820`) |
| Store | `core.NewCronStore(t.TempDir())` — a throwaway directory, see §3 |
| Handshake dir | `CC_CRON_E2E_DIR`, default `/tmp/cc-connect-cron-e2e` |

**About `CC_CRON_E2E_DIR`:** it is the handshake directory shared by the
`tests/e2e` fixture family. The sibling browser fixture
(`TestCronQuietWebUIFixture`) writes `port`, `token` and `stop` files into it
and hands the port/token to an external driver. `TestCronPreconditionE2E`
does **not** need that handshake — it drives the API from inside the same
process — but the environment variable and its default still apply to the
fixture family, and §2.2 below uses it to replay the request sequence with
`curl` against the repo's own management-API fixture.

### 1.2 Command

```bash
go test -tags=webui_e2e ./tests/e2e -run '^TestCronPreconditionE2E$' -count=1 -v
```

`-run` is required: the sibling file `cron_quiet_webui_fixture_test.go` holds
a fixture that stays alive until a driver drops a stop file (or 12 minutes
elapse), so running the whole package is not a way to run this test.

Recorded output of the acceptance run:

```
=== RUN   TestCronPreconditionE2E
2026/10/20 22:52:28 INFO cron: scheduler started jobs=0
2026/10/20 22:52:28 INFO management api started port=19820
--- PASS: TestCronPreconditionE2E (0.03s)
PASS
ok  	github.com/chenhg5/cc-connect/tests/e2e	0.198s
```

Exit code: `0`.

---

## 2. The request sequence

### 2.1 What the E2E issues

The E2E performs the four requests below against the real management API on
`127.0.0.1:19820` (all requests carry the management token as
`?token=<token>`; the token is the fixture's `e2e-token`):

| # | Request | Body | Expected |
|---|---|---|---|
| 1 | `POST /api/v1/cron` | `{"cron_expr":"0 3 1 1 *","prompt":"precondition e2e","project":"e2e-project","session_key":"e2e:channel:user","precondition":"exit 0","precondition_timeout":15}` | response `data.precondition` = `exit 0`, `data.precondition_timeout` = `15` |
| 2 | `GET /api/v1/cron` | — | the job appears in `data.jobs` with the same two values |
| 3 | `PATCH /api/v1/cron/{id}` | `{"precondition_timeout":30}` | response `data.precondition_timeout` = `30`, `data.precondition` still `exit 0` |
| 4 | `GET /api/v1/cron` | — | the job now reads back with `precondition_timeout` = `30` |

`cron_expr` is `0 3 1 1 *` (1 January, 03:00) on purpose: the schedule never
comes round, so the scheduler cannot fire the job while the test runs and the
result cannot depend on the wall clock.

### 2.2 Wire transcript

`TestCronPreconditionE2E` drives those four requests from inside the test
process, so there is no server left listening afterwards to point a `curl` at.
To capture the same sequence **on the wire**, it was replayed with `curl`
against the repo's own management-API fixture
(`TestCronQuietWebUIFixture`), which serves the very same
`core.ManagementServer` + `core.CronScheduler` + `core.CronStore` stack and
publishes its port and token through the `CC_CRON_E2E_DIR` handshake:

```bash
# start the fixture (background) and read the handshake
CC_CRON_E2E_DIR=/tmp/cc-connect-cron-e2e \
  go test -tags=webui_e2e ./tests/e2e -run '^TestCronQuietWebUIFixture$' -count=1 -timeout 15m -v &
until [ -f /tmp/cc-connect-cron-e2e/port ] && [ -f /tmp/cc-connect-cron-e2e/token ]; do sleep 1; done
PORT=$(cat /tmp/cc-connect-cron-e2e/port); TOKEN=$(cat /tmp/cc-connect-cron-e2e/token)

# 1. create
curl -sS -X POST "http://127.0.0.1:$PORT/api/v1/cron?token=$TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"cron_expr":"0 3 1 1 *","prompt":"precondition e2e","project":"e2e-project","session_key":"e2e:channel:user","precondition":"exit 0","precondition_timeout":15}'

# 2. read back
curl -sS "http://127.0.0.1:$PORT/api/v1/cron?token=$TOKEN"

# 3. patch the timeout only
curl -sS -X PATCH "http://127.0.0.1:$PORT/api/v1/cron/$ID?token=$TOKEN" \
  -H 'Content-Type: application/json' -d '{"precondition_timeout":30}'

# 4. read back again
curl -sS "http://127.0.0.1:$PORT/api/v1/cron?token=$TOKEN"
```

**Step 1 — `POST /api/v1/cron`** (response, verbatim):

```json
{
    "data": {
        "id": "c5343303",
        "project": "e2e-project",
        "session_key": "e2e:channel:user",
        "cron_expr": "0 3 1 1 *",
        "prompt": "precondition e2e",
        "description": "",
        "enabled": true,
        "created_at": "2026-10-20T22:51:54.249183+08:00",
        "last_run": "0001-01-01T00:00:00Z",
        "precondition": "exit 0",
        "precondition_timeout": 15
    },
    "ok": true
}
```

**Step 2 — `GET /api/v1/cron`**, the job picked out of `data.jobs`:

```json
{
  "id": "c5343303",
  "project": "e2e-project",
  "session_key": "e2e:channel:user",
  "cron_expr": "0 3 1 1 *",
  "prompt": "precondition e2e",
  "description": "",
  "enabled": true,
  "created_at": "2026-10-20T22:51:54.249183+08:00",
  "last_run": "0001-01-01T00:00:00Z",
  "precondition": "exit 0",
  "precondition_timeout": 15
}
```

**Step 3 — `PATCH /api/v1/cron/c5343303`** with `{"precondition_timeout":30}`:

```json
{
  "id": "c5343303",
  "project": "e2e-project",
  "session_key": "e2e:channel:user",
  "cron_expr": "0 3 1 1 *",
  "prompt": "precondition e2e",
  "description": "",
  "enabled": true,
  "created_at": "2026-10-20T22:51:54.249183+08:00",
  "last_run": "0001-01-01T00:00:00Z",
  "precondition": "exit 0",
  "precondition_timeout": 30
}
```

**Step 4 — `GET /api/v1/cron`** again: identical to step 3, i.e. the patched
timeout is what a later client reads.

The `precondition` command is untouched by the PATCH in every reading, and
`precondition_timeout` moves `15 → 30` and stays there.

---

## 3. The temporary store, and the proof that `~/.cc-connect` was not touched

### 3.1 Where the store lives

The E2E builds its store with `core.NewCronStore(t.TempDir())`, so the data
lands in a throwaway directory that the Go test framework deletes when the
test ends. `core.CronStore` writes to `<dataDir>/crons/jobs.json` and nothing
else.

Go's `testing.T.TempDir()` creates its base directory with
`os.MkdirTemp(os.Getenv("GOTMPDIR"), <test name>)`, so pointing `GOTMPDIR` at
a scratch directory makes the store path of a run observable. The recorded
acceptance run used `GOTMPDIR=/tmp/cc-e2e-gotmp` and the store path was:

```
/tmp/cc-e2e-gotmp/TestCronPreconditionE2E256139500/001
```

`001` is the per-test sequence number, `TestCronPreconditionE2E*` is the test
name, and the middle number is random per run — the base is `$GOTMPDIR`
(without `GOTMPDIR` the base is the OS temp directory).

To capture the path, poll the scratch directory while the test runs and keep
the last copy of `jobs.json`:

```python
import os, time
gotmp, keep = "/tmp/cc-e2e-gotmp", "/tmp/cc-e2e-keep"
while True:
    for entry in os.listdir(gotmp):
        if entry.startswith("TestCronPreconditionE2E"):
            store = os.path.join(gotmp, entry, "001")
            jobs = os.path.join(store, "crons", "jobs.json")
            try:
                data = open(jobs, "rb").read()
            except OSError:
                continue
            if data:
                open(os.path.join(keep, "jobs.json"), "wb").write(data)
                open(os.path.join(keep, "store-path.txt"), "w").write(store + "\n")
    time.sleep(0.0005)
```

The captured `crons/jobs.json` of that run — the state the test left on disk,
after the PATCH:

```json
[
  {
    "id": "447c6e9d",
    "project": "e2e-project",
    "session_key": "e2e:channel:user",
    "cron_expr": "0 3 1 1 *",
    "prompt": "precondition e2e",
    "description": "",
    "enabled": true,
    "created_at": "2026-10-20T22:52:28.434452+08:00",
    "last_run": "0001-01-01T00:00:00Z",
    "precondition": "exit 0",
    "precondition_timeout": 30
  }
]
```

The same job is in the temporary store, in the in-memory store, and in the
API responses — with the patched timeout `30` and the command `exit 0`.

### 3.2 `~/.cc-connect` is untouched

The whole `~/.cc-connect` tree was snapshotted immediately before and
immediately after the acceptance run:

```bash
ls -la "$HOME/.cc-connect" > /tmp/cc-conn-ls-before.txt
find "$HOME/.cc-connect" -not -path '*/logs/*' -not -name logs \
  -exec stat -f "%m %N" {} \; | sort > /tmp/cc-conn-tree-before.txt
shasum -a 256 "$HOME/.cc-connect/crons/jobs.json"

go test -tags=webui_e2e ./tests/e2e -run '^TestCronPreconditionE2E$' -count=1 -v

ls -la "$HOME/.cc-connect" > /tmp/cc-conn-ls-after.txt
find "$HOME/.cc-connect" -not -path '*/logs/*' -not -name logs \
  -exec stat -f "%m %N" {} \; | sort > /tmp/cc-conn-tree-after.txt
shasum -a 256 "$HOME/.cc-connect/crons/jobs.json"

diff /tmp/cc-conn-ls-before.txt  /tmp/cc-conn-ls-after.txt
diff /tmp/cc-conn-tree-before.txt /tmp/cc-conn-tree-after.txt
```

Recorded result:

```
--- diff of the whole ~/.cc-connect tree (path + mtime) ---
IDENTICAL: no path added, removed or re-stamped

jobs.json sha256 (before): 1e22c1b733e2326960a1163c8f2a944adbbf86d677b1cb5178c9ffb394b416dc
jobs.json sha256 (after) : 1e22c1b733e2326960a1163c8f2a944adbbf86d677b1cb5178c9ffb394b416dc
```

No `mtime` in `ls -la ~/.cc-connect` changed, no path was added or removed,
and the real cron store is byte-identical before and after. The data of the
run exists only in the temporary store of §3.1.

Cross-check — the E2E job is not in the real store:

```bash
grep -c "precondition e2e" "$HOME/.cc-connect/crons/jobs.json"   # → 0
grep -c "447c6e9d"         "$HOME/.cc-connect/crons/jobs.json"   # → 0
```

---

## 4. Assertion checklist

Every line is one assertion the E2E makes against the running stack. "Where"
points at the code that makes it.

| # | Assertion | Where | Result |
|---|---|---|---|
| 1 | `POST` response carries `precondition` = `exit 0` | `cron_precondition_e2e_test.go:217` | **PASS** |
| 2 | `POST` response carries `precondition_timeout` = `15` | `cron_precondition_e2e_test.go:220` | **PASS** |
| 3 | `GET` read-back carries `precondition` = `exit 0` | `cron_precondition_e2e_test.go:229` | **PASS** |
| 4 | `GET` read-back carries `precondition_timeout` = `15` | `cron_precondition_e2e_test.go:232` | **PASS** |
| 5 | `PATCH` response carries `precondition_timeout` = `30` | `cron_precondition_e2e_test.go:243` | **PASS** |
| 6 | `PATCH` response leaves `precondition` = `exit 0` | `cron_precondition_e2e_test.go:246` | **PASS** |
| 7 | second `GET` read-back carries `precondition_timeout` = `30` | `cron_precondition_e2e_test.go:255` | **PASS** |
| 8 | second `GET` read-back leaves `precondition` = `exit 0` | `cron_precondition_e2e_test.go:258` | **PASS** |
| 9 | `store.Get(id)` and the API read-back agree on 9 fields (`id`, `project`, `session_key`, `cron_expr`, `prompt`, `description`, `enabled`, `precondition`, `precondition_timeout`) | `cron_precondition_e2e_test.go:275` | **PASS** |
| 10 | `store.Get(id).PreconditionTimeout` = `30` | `cron_precondition_e2e_test.go:286` | **PASS** |
| 11 | `store.Get(id).Precondition` = `exit 0` | `cron_precondition_e2e_test.go:289` | **PASS** |
| 12 | the on-disk `crons/jobs.json` carries `precondition` = `exit 0` | `cron_precondition_e2e_test.go:295` | **PASS** |
| 13 | the on-disk `crons/jobs.json` carries `precondition_timeout` = `30` | `cron_precondition_e2e_test.go:298` | **PASS** |

### 4.1 Environment checks

| # | Assertion | Evidence | Result |
|---|---|---|---|
| E1 | The E2E store is a throwaway directory, not `~/.cc-connect` | store path `/tmp/cc-e2e-gotmp/TestCronPreconditionE2E256139500/001` (§3.1) | **PASS** |
| E2 | `~/.cc-connect` is unchanged by the run | `diff` of the `ls -la` listing and of the recursive path+mtime tree: identical; `jobs.json` sha256 unchanged (§3.2) | **PASS** |
| E3 | No browser, no Playwright, no Puppeteer | the run is a single `go test` process; the only client is the Go HTTP client inside the test, plus `curl` for the transcript in §2.2 | **PASS** |

---

## 5. Notes for whoever runs this next

1. **The live daemon on this machine also writes `~/.cc-connect`.** A running
   `cc-connect` daemon persists *its own* cron store, and one of its jobs
   (`2-57/5 * * * *`) fired at `22:52:00` during the session — that is what
   changed `~/.cc-connect/crons/jobs.json` outside the test window. The
   §3.2 comparison is therefore taken in a window around the test only, and
   the attribution is done by content, not only by mtime: the daemon's 14
   jobs carry no `precondition` field, and the E2E's job id and prompt appear
   in neither the file nor its hash. If your machine has no daemon running,
   the mtime comparison is unambiguous.
2. **Stale handshake files fool a `curl` driver.** The fixture deletes the
   `port`/`token`/`stop` files at the *start* of the test, so a wait loop that
   polls for them can pick up the leftovers of an earlier run and issue its
   first request before the server is up (`Couldn't connect to 127.0.0.1 port
   19820`). Delete the handshake directory before starting the fixture, and
   wait for `GET /api/v1/status` to answer rather than for the file to exist.
3. **Only the precondition fields are exercised here.** The gate itself
   (running the command, honouring the timeout, skipping the job when it
   fails) is covered by the unit tests in `core/cron_precondition_test.go`;
   this acceptance is about the fields surviving the API round trip.
4. **The `cron_expr` is a date that never comes round** (`0 3 1 1 *`). Keep it
   that way — a job that fires during the run would make the result depend on
   the wall clock and on the stub agent.

---

## 6. Reproducing

```bash
# the acceptance itself
go test -tags=webui_e2e ./tests/e2e -run '^TestCronPreconditionE2E$' -count=1 -v
# expect: --- PASS: TestCronPreconditionE2E

# the untouched check
ls -la "$HOME/.cc-connect" > /tmp/cc-conn-ls-before.txt
find "$HOME/.cc-connect" -not -path '*/logs/*' -not -name logs \
  -exec stat -f "%m %N" {} \; | sort > /tmp/cc-conn-tree-before.txt
go test -tags=webui_e2e ./tests/e2e -run '^TestCronPreconditionE2E$' -count=1 -v
ls -la "$HOME/.cc-connect" > /tmp/cc-conn-ls-after.txt
find "$HOME/.cc-connect" -not -path '*/logs/*' -not -name logs \
  -exec stat -f "%m %N" {} \; | sort > /tmp/cc-conn-tree-after.txt
diff /tmp/cc-conn-ls-before.txt  /tmp/cc-conn-ls-after.txt
diff /tmp/cc-conn-tree-before.txt /tmp/cc-conn-tree-after.txt
# expect: no differences

# the wire transcript of §2.2 (optional; needs the sibling fixture)
rm -f /tmp/cc-connect-cron-e2e/port /tmp/cc-connect-cron-e2e/token /tmp/cc-connect-cron-e2e/stop
CC_CRON_E2E_DIR=/tmp/cc-connect-cron-e2e \
  go test -tags=webui_e2e ./tests/e2e -run '^TestCronQuietWebUIFixture$' -count=1 -timeout 15m -v &
until [ -f /tmp/cc-connect-cron-e2e/port ] && [ -f /tmp/cc-connect-cron-e2e/token ]; do sleep 1; done
PORT=$(cat /tmp/cc-connect-cron-e2e/port); TOKEN=$(cat /tmp/cc-connect-cron-e2e/token)
curl -sS "http://127.0.0.1:$PORT/api/v1/status?token=$TOKEN"   # wait for this to answer
# … the four requests of §2.2 …
touch /tmp/cc-connect-cron-e2e/stop                            # stop the fixture
```
