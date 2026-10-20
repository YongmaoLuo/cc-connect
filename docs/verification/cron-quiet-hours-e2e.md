# Cron Quiet Hours — Web UI End-to-End Verification

Acceptance run for the quiet-hours editing chain in the cc-connect web admin:
**browser form → management API → cron store → card re-render**.

Upstream decision points covered:

- arch decision 6 — Web UI visibility (the quiet fields are editable and visible in the web admin)
- test decision 2 — part of the post-completion gate

- **Date:** 2026-10-20
- **Branch:** `feat/cron-quiet-hours`
- **Result:** 11 assertions, 11 PASS, 0 FAIL

---

## 1. How the run works

The acceptance is driven by a Go fixture that boots the real stack, plus a
Playwright driver that performs the assertions in a real browser. The fixture
serves the built frontend **and** the management API from the same origin,
because the web client calls the API with relative paths (`/api/v1/...`).

```
┌─────────────────────────────┐        ┌──────────────────────────────┐
│ fixture (go test)           │        │ Playwright driver (browser)  │
│  ManagementServer :19820    │◄──────►│  drives the real web admin   │
│   ├─ /api/v1/* (cron, …)    │  HTTP  │                              │
│   └─ /  → web/dist (SPA)    │        │                              │
│  CronScheduler + CronStore  │        │                              │
└─────────────────────────────┘        └──────────────────────────────┘
```

### 1.1 Start the fixture

```bash
cd web && pnpm build && cd ..
go test -count=1 -tags=webui_e2e ./tests/e2e -run '^TestCronQuietWebUIFixture$' -timeout 15m &
```

`-count=1` is required: without it Go replays the cached result of a previous
run instead of starting the server, and the handshake files never appear.

The fixture writes the handshake files the driver polls for (≤120s):

| File | Content |
|---|---|
| `/tmp/cc-connect-cron-e2e/port` | the port it actually bound (`19820`, or `19821` if `19820` was busy) |
| `/tmp/cc-connect-cron-e2e/token` | `e2e-token` |

The run ends when the driver drops `/tmp/cc-connect-cron-e2e/stop`, or after
`CC_CRON_E2E_MAX_WAIT` (default 12m).

### 1.2 Drive the browser

1. `browser_navigate` → `http://localhost:19820/login?token=e2e-token`
2. Click the **Cron** nav item, wait for the job-card area
3. Click **Add**, fill in the form, save
4. Assert the card shows the quiet window and the interval → screenshot `01-saved.png`
5. Open the card, change the interval, save, assert the card re-renders → screenshot `02-edited.png`
6. `browser_evaluate` → `fetch('/api/v1/cron')`, cross-check the payload against the rendered card

---

## 2. Assertion checklist

Every line is one assertion made against the **running** stack in a real
browser. The result column is what the run actually produced.

| # | Assertion | Evidence | Result | Screenshot |
|---|---|---|---|---|
| 1 | `GET /login?token=e2e-token` logs in without a manual token entry and lands on `/` | page URL after auto-login: `http://localhost:19820/` | **PASS** | — |
| 2 | The Cron nav item is present and navigates to `/cron` | page URL after click: `http://localhost:19820/cron` | **PASS** | — |
| 3 | The Cron page renders the job list (empty state before the first job) | `暂无定时任务` visible | **PASS** | — |
| 4 | The Add form opens and exposes the quiet-hours and quiet-interval fields | modal `添加任务` with `静默时段` and `静默间隔（分钟）` inputs | **PASS** | — |
| 5 | The project select is offered and the session-key select populates from it | project `e2e-project` selected → `e2e:channel:user` appears as a session option | **PASS** | — |
| 6 | Saving a job with quiet hours `23:00-08:00` and interval `60` persists it | card shows badge `23:00-08:00` and row `静默间隔（分钟）` = `60` | **PASS** | `01-saved.png` |
| 7 | The saved quiet window is echoed on the card next to the schedule badge | card badge row: `每天 9:00` · `23:00-08:00` · `跟随项目默认` | **PASS** | `01-saved.png` |
| 8 | Opening the card pre-fills the edit form with the stored quiet values | edit form `静默时段` = `23:00-08:00`, `静默间隔（分钟）` = `60` | **PASS** | — |
| 9 | Editing the interval to `30` and saving re-renders the card with `30` | card row `静默间隔（分钟）` = `30`, window still `23:00-08:00` | **PASS** | `02-edited.png` |
| 10 | `GET /api/v1/cron` returns the same quiet fields the card shows | API `quiet_hours` = `23:00-08:00`, `quiet_interval_mins` = `30`; card text contains `23:00-08:00` and `30` | **PASS** | — |
| 11 | The quiet fields are persisted to the cron store on disk | `…/crons/jobs.json` contains `"quiet_hours": "23:00-08:00"`, `"quiet_interval_mins": 30` | **PASS** | — |

### 2.1 Payload cross-check (assertion 10, verbatim)

`browser_evaluate` → `fetch('/api/v1/cron')` on the live page:

```json
{
  "httpStatus": 200,
  "ok": true,
  "jobCount": 1,
  "api": {
    "id": "3f17873b",
    "project": "e2e-project",
    "session_key": "e2e:channel:user",
    "cron_expr": "0 9 * * *",
    "description": "e2e-quiet-hours-job",
    "prompt": "quiet hours acceptance prompt",
    "quiet_hours": "23:00-08:00",
    "quiet_interval_mins": 30
  },
  "cardText": "e2e-quiet-hours-job\n每天 9:00\n23:00-08:00\n跟随项目默认\n项目\ne2e-project\n提示词\nquiet hours acceptance prompt\n静默时段\n23:00-08:00\n静默间隔（分钟）\n30\n上次运行\n1/1/1 08:05:43",
  "cardShowsWindow": true,
  "cardShowsInterval30": true
}
```

The card text contains the same window and the same interval as the API
payload — the UI is not rendering a local copy that drifted from the server.

---

## 3. Screenshots

| File | Shows |
|---|---|
| [`screenshots/cron-quiet-hours/01-saved.png`](screenshots/cron-quiet-hours/01-saved.png) | The job card right after saving: quiet-hours badge `23:00-08:00`, detail row `静默时段 23:00-08:00`, `静默间隔（分钟） 60` |
| [`screenshots/cron-quiet-hours/02-edited.png`](screenshots/cron-quiet-hours/02-edited.png) | The same card after editing the interval to `30`: `静默间隔（分钟） 30`, window unchanged at `23:00-08:00` |

The UI language is the browser's (`zh` in this run); the asserted **values**
(`23:00-08:00`, `60`, `30`) are language-independent.

---

## 4. Notes for whoever runs this next

Three things about the fixture are not obvious from the code and cost time
during this run:

1. **The session key is mandatory.** `core.validateCronJob` rejects a job
   whose `session_key` is empty, and the web form can only offer the sessions
   the project already has. The fixture therefore seeds one session
   (`e2e:channel:user`) through `POST /api/v1/projects/{project}/sessions`
   before the driver opens the form. Without it, the Add form submits a job
   the API rejects with `session_key is required`.
2. **The project must be selected before the session key is offered.** The
   form only fetches the project's sessions once `form.project` is set, so the
   session-key select is empty until the project is picked. This is a UI
   dependency, not a fixture artefact — the driver selects the project first.
3. **Port fallback is exercised.** When `19820` is already taken the fixture
   binds `19821` and writes the port it actually used into
   `/tmp/cc-connect-cron-e2e/port`. The driver must read the port from that
   file rather than hard-coding `19820`. (This happened for real during the
   run: a leftover fixture from an earlier attempt still held `19820`, the
   fixture logged `port 19820 is busy, trying the next one` and bound
   `19821`.)

The fixture also self-checks before handing over: it asserts that
`/api/v1/projects` lists the project, that `/api/v1/cron` is reachable, and
that a job with quiet hours can be created and deleted through the same
endpoint the browser uses. A wiring fault therefore fails the `go test` run
instead of surfacing as an unexplained UI timeout. Those self-checks earned
their keep: they caught the missing engine registration and the missing
session seed during this run, before the browser was involved.

### 4.1 The Add/Edit modal needs a tall viewport (UX observation)

`web/src/components/ui/Modal.tsx` renders the dialog as
`fixed inset-0 … items-center` with no `max-height` and no internal scroll.
The cron form has enough fields that at a 900px-tall viewport the dialog
overflows the screen, and the **Save** button ends up outside the viewport —
a real click on it is impossible, not merely awkward. The driver therefore
resizes to `1400×1600` for the form interaction and back to `1100×700` for
the screenshots.

This is a genuine UI finding, not a defect in the quiet-hours feature: the
same overflow exists for any cron job form, quiet fields or not. It is
recorded here rather than fixed, because the quiet-hours UI was already
merged in task 5 and widening the modal's scope belongs to a separate
change.

### 4.2 Environment note (not a product defect)

The Playwright MCP server in this environment is pinned to a Chromium build
(`chromium-1232`) that is not installed; only `chromium-1243` is present. The
run was made possible by aliasing the installed bundle at the pinned path:

```bash
CACHE="$HOME/Library/Caches/ms-playwright"
mkdir -p "$CACHE/chromium-1232/chrome-mac"
ln -s "$CACHE/chromium-1243/chrome-mac-arm64/Google Chrome for Testing.app" \
      "$CACHE/chromium-1232/chrome-mac/Google Chrome for Testing.app"
```

This touches only the local Playwright browser cache — no settings file and
no repository file. On a machine where the pinned build is installed the
alias is unnecessary.

---

## 5. Reproducing

```bash
cd web && pnpm build && cd ..
go test -count=1 -tags=webui_e2e ./tests/e2e -run '^TestCronQuietWebUIFixture$' -timeout 15m &
# wait for the handshake
until [ -f /tmp/cc-connect-cron-e2e/port ] && [ -f /tmp/cc-connect-cron-e2e/token ]; do sleep 1; done
PORT=$(cat /tmp/cc-connect-cron-e2e/port)
TOKEN=$(cat /tmp/cc-connect-cron-e2e/token)
# … drive the browser against http://localhost:$PORT/login?token=$TOKEN …
# stop the fixture
touch /tmp/cc-connect-cron-e2e/stop
```
