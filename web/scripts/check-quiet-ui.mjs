#!/usr/bin/env node
// Web build gate for the quiet-hours fields.
//
// The quiet-hours configuration is only useful if it survives the whole
// round trip: the HTTP API (core/api.go, core/management.go) speaks
// `quiet_hours` / `quiet_interval_mins` / `warm_prompt`, and the Web UI has
// to speak exactly the same names when it edits a job or renders its card.
//
// This script is the gate for that contract:
//
//   1. api/cron.ts declares the three fields on CronJob.
//   2. pages/Cron/CronList.tsx reads them, edits them and PATCHes them.
//   3. The i18n locales en + zh carry the three new labels (every other
//      locale falls back to en, see src/i18n/index.ts).
//   4. `pnpm build` still succeeds.
//
// A failed assertion exits 1 immediately and the exception is not swallowed.

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');

const FIELDS = ['quiet_hours', 'quiet_interval_mins', 'warm_prompt'];
const I18N_KEYS = ['quietHours', 'quietInterval', 'warmPrompt'];

let failures = 0;

function check(ok, message) {
  if (ok) {
    console.log(`  ok   ${message}`);
    return;
  }
  console.error(`  FAIL ${message}`);
  failures++;
}

function read(rel) {
  return readFileSync(join(webRoot, rel), 'utf8');
}

// ── 1. api/cron.ts ────────────────────────────────────────────
const apiSrc = read('src/api/cron.ts');
const apiInterface = apiSrc.slice(apiSrc.indexOf('export interface CronJob'));
for (const field of FIELDS) {
  check(apiInterface.includes(field), `api/cron.ts: CronJob declares ${field}`);
}

// ── 2. pages/Cron/CronList.tsx ────────────────────────────────
const listSrc = read('src/pages/Cron/CronList.tsx');
for (const field of FIELDS) {
  check(listSrc.includes(field), `CronList.tsx: handles ${field}`);
}
for (const key of I18N_KEYS) {
  check(listSrc.includes(`cron.${key}`), `CronList.tsx: uses cron.${key}`);
}
// The interval is a JSON number on the wire, never a string.
check(
  /updates\.quiet_interval_mins\s*=/.test(listSrc) && !/updates\.quiet_interval_mins\s*=\s*form\./.test(listSrc),
  'CronList.tsx: quiet_interval_mins is PATCHed as a number',
);

if (failures > 0) {
  console.error(`\nFAIL quiet-ui (${failures} assertion(s) failed)`);
  process.exit(1);
}

// ── 3. i18n locales ───────────────────────────────────────────
for (const locale of ['en', 'zh']) {
  let data;
  try {
    data = JSON.parse(read(`src/i18n/locales/${locale}.json`));
  } catch (e) {
    console.error(`  FAIL locales/${locale}.json is not valid JSON: ${e.message}`);
    failures++;
    continue;
  }
  const cron = data?.cron ?? {};
  for (const key of I18N_KEYS) {
    check(typeof cron[key] === 'string' && cron[key].length > 0, `locales/${locale}.json: cron.${key} exists`);
  }
}

if (failures > 0) {
  console.error(`\nFAIL quiet-ui (${failures} assertion(s) failed)`);
  process.exit(1);
}

// ── 4. production build ───────────────────────────────────────
console.log('  ...  running pnpm build');
const build = spawnSync('pnpm', ['build'], { cwd: webRoot, stdio: 'inherit' });
if (build.error) {
  console.error(`  FAIL pnpm build could not start: ${build.error.message}`);
  process.exit(1);
}
if (build.status !== 0) {
  console.error(`  FAIL pnpm build exited with ${build.status}`);
  process.exit(1);
}

console.log('PASS quiet-ui');
