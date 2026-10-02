#!/usr/bin/env node
/**
 * Hosted resume benchmark runner.
 *
 * Drives the shipped web bundle in Chromium and WebKit against the synthetic
 * in-page relay (tests/browser/resume-fixture.mjs) and records one outcome
 * for every attempted wake epoch. It is a separately named, bounded hosted
 * job, not a functional test, and it never touches a real relay, network,
 * device or account.
 *
 *   pilot   one variant (the current SHA); estimates variance and workload for
 *           planning a confirmatory experiment. It is never p95 acceptance.
 *   paired  matched baseline/candidate pairs with a seeded random order per
 *           pair, identical seeded conditions and a fresh browser context for
 *           every run, so neither variant inherits the other's state.
 *
 * The preregistration record (SHAs, strata, seeds, schedules, sample size,
 * endpoints, analysis and bounds) is written before the first epoch runs, and
 * its digest is bound into the evidence.
 *
 * The success endpoint is the first animation frame that paints the active
 * epoch's authenticated, ready, non-stale inventory, measured from the first
 * visible event (or from navigation start after an emulated discard) with a
 * fixed 60-second deadline. Anything else is a non-completion with a reason.
 */
import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { DEFAULT_BOUNDS, EVIDENCE_SCHEMA, epochSeed } from './analyze-resume-benchmarks.mjs';

export { epochSeed };
import { fixtureRelay, resumeFixtureInit, resumeSensitiveMarkers } from '../tests/browser/resume-fixture.mjs';

const FRONTEND = resolve(dirname(fileURLToPath(import.meta.url)), '..');
export const DEADLINE_MS = 60_000;
export const PILOT_MIN_EPOCHS = 30;
export const CONFIRMATORY_MIN_PAIRS = 400;
const WARMUP_TIMEOUT_MS = 30_000;
/** Idle time after the cold warm-up so no reply is still in flight at the wake. */
export const IDLE_BEFORE_HIDE_MS = 500;
/**
 * The separate, bounded window in which a gateway-direct epoch's direct
 * WebRTC upgrade is observed, from the same wake. It never changes the
 * primary 60-second completion deadline or outcome.
 */
export const DIRECT_UPGRADE_WINDOW_MS = 30_000;
const DEVICES = /** @type {const} */ ({ chromium: 'Pixel 7', webkit: 'iPhone 15' });

/**
 * Preregisterable workloads. `wake` is how the measured epoch starts: the
 * page becoming visible, a reload after an emulated discard, or a network
 * handoff reported while the page stays visible. `real_hidden_ms` is the
 * scripted real wait before that event (time hidden, or time on the silently
 * dead path before the handoff is reported); `frozen_wall_ms` is wall-clock
 * time that passes while the page is frozen.
 */
export const SCENARIOS = Object.freeze({
  'warm-short': {
    description: 'App switch: hidden 200-800 ms, connection kept, no frozen time.',
    wake: 'visible',
    real_hidden_ms: [200, 800],
    frozen_wall_ms: 0,
    connection: 'kept',
  },
  'hidden-30s': {
    description: 'Frozen page for 30 s with the connection still healthy (inside the keepalive freshness bound): the app probes and keeps it.',
    wake: 'visible',
    real_hidden_ms: [200, 400],
    frozen_wall_ms: 30_000,
    connection: 'kept',
  },
  'hidden-5m': {
    description: 'Frozen page for five minutes; the gateway/tunnel reaped the socket silently.',
    wake: 'visible',
    real_hidden_ms: [200, 400],
    frozen_wall_ms: 300_000,
    connection: 'half-open',
  },
  'hidden-long': {
    description: 'Frozen page for two hours, past the one-hour hidden keepalive bound; the socket was reaped silently.',
    wake: 'visible',
    real_hidden_ms: [200, 400],
    frozen_wall_ms: 7_200_000,
    connection: 'half-open',
  },
  'blackhole-restore': {
    description: 'Frozen 30 s with a half-open socket; new dials stall until the network returns 1-4 s after the wake, then connect on a 1/3/7/15/31 s SYN retransmission schedule; online fires at restoration.',
    wake: 'visible',
    real_hidden_ms: [200, 400],
    frozen_wall_ms: 30_000,
    connection: 'half-open',
    restore_after_wake_ms: [1_000, 4_000],
  },
  'network-change': {
    description: 'The page stays visible; the path goes silently half-open (a Wi-Fi/cellular handoff) and 200-400 ms later navigator.connection reports a change, which starts the measured epoch; online stays true.',
    wake: 'network-change',
    real_hidden_ms: [200, 400],
    frozen_wall_ms: 0,
    connection: 'half-open',
  },
  discard: {
    description: 'Page discarded while hidden for two minutes; reload with document.wasDiscarded. Measured from navigation start; OS wake-to-JS is unobservable.',
    wake: 'reload',
    real_hidden_ms: [200, 400],
    frozen_wall_ms: 120_000,
    connection: 'none (reloaded)',
  },
});

export const TRANSPORTS = Object.freeze({
  'wss-cloudflare': 'Direct WSS with a Cloudflare ingress descriptor',
  'wss-tailscale': 'Direct WSS with a managed Tailscale Serve ingress descriptor',
  'gateway-relayed': 'Community-gateway relayed path, direct upgrade disabled',
  'gateway-direct': 'Community-gateway path with the direct WebRTC upgrade; the measured wake starts once direct is promoted',
});

/**
 * The bounded default pilot. Every other workload and path is selectable
 * with --scenarios and --transports for a preregistered design, and each is
 * exercised by the hosted browser suite (tests/browser/resume.spec.ts).
 */
export const DEFAULT_PILOT_SCENARIOS = Object.freeze(['warm-short', 'hidden-5m', 'blackhole-restore', 'discard']);
export const DEFAULT_PILOT_TRANSPORTS = Object.freeze(['wss-cloudflare', 'gateway-relayed']);

/**
 * The synthetic relay for a transport. Hostnames never decide the label; the
 * ingress descriptor inside the authenticated session does.
 *
 * @param {string} transport
 */
export function relayForTransport(transport) {
  if (transport === 'wss-cloudflare') return fixtureRelay(1, 'wss', { ingress: 'cloudflare' });
  if (transport === 'wss-tailscale') return fixtureRelay(1, 'wss', { ingress: 'tailscale-managed' });
  if (transport === 'gateway-relayed' || transport === 'gateway-direct') return fixtureRelay(1, 'hybrid');
  throw new Error(`unknown transport ${transport}`);
}

export const NEGATIVE_CONTROLS = Object.freeze({
  'revoked-credential': 'Relay refuses the device credential: expect refusal, no fresh inventory, no redial loop.',
  'cancelled-unlock': 'Device verification is cancelled at launch: expect the lock to stay and no dial.',
  'permanent-outage': 'Network never returns during a 15 s window: expect no fresh inventory and no crash.',
  'ios-deferred-pairing': 'iOS Safari tab with a setup link: expect deferral to the Home Screen app and no dial.',
});

/**
 * The only objective infrastructure exclusions. Both are established outside
 * the page under test, before or independently of anything it does. A page
 * crash, a lost browser, a bundle that fails to load, or an app that never
 * renders its first inventory are attempted epochs that did not complete.
 */
export const HARNESS_INVALID_CRITERIA = Object.freeze([
  {
    id: 'server-unavailable',
    rule: 'The local static server refused or dropped an independent HTTP request made by the runner (a network error, not any HTTP status) right after the page failed to load.',
  },
  {
    id: 'browser-unavailable',
    rule: 'A fresh browser context or page could not be created, before the page under test was opened.',
  },
]);

/** Non-completion reasons the runner records for app or harness failures. */
export const NON_COMPLETION_OUTCOMES = Object.freeze([
  'deadline', 'late', 'load-failed', 'warmup-failed', 'page-crash', 'browser-disconnected', 'harness-error',
]);

/**
 * Deterministic PRNG (mulberry32).
 *
 * @param {number} seed
 * @returns {() => number}
 */
export function seededRandom(seed) {
  let value = seed >>> 0;
  return () => {
    value = (value + 0x6d2b79f5) >>> 0;
    let t = value;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/**
 * Scripted conditions for one seed: hidden time, and the restoration delay
 * for the outage scenario.
 *
 * @param {number} seed
 * @param {keyof typeof SCENARIOS} scenario
 */
export function epochSchedule(seed, scenario) {
  const random = seededRandom(seed);
  const spec = SCENARIOS[scenario];
  const between = (/** @type {readonly number[]} */ range) => range[0] + Math.floor(random() * (range[1] - range[0] + 1));
  return {
    hiddenMs: between(spec.real_hidden_ms),
    frozenMs: spec.frozen_wall_ms,
    restoreAfterMs: 'restore_after_wake_ms' in spec ? between(spec.restore_after_wake_ms) : null,
    fixtureSeed: Math.floor(random() * 1_000_000_000) + 1,
  };
}

/**
 * Seeded, independent order for each matched pair.
 *
 * @param {number} seed
 * @returns {Array<'baseline' | 'candidate'>}
 */
export function pairOrder(seed) {
  return seededRandom(seed ^ 0x5bd1e995)() < 0.5 ? ['baseline', 'candidate'] : ['candidate', 'baseline'];
}

/**
 * @param {{
 *   design: 'pilot' | 'paired';
 *   browsers: Array<keyof typeof DEVICES>;
 *   transports: Array<keyof typeof TRANSPORTS>;
 *   scenarios: Array<keyof typeof SCENARIOS>;
 *   samples: number;
 *   baseSeed: number;
 *   candidateSha: string;
 *   baselineSha: string;
 *   targets?: string[];
 *   negativeTrials?: number;
 * }} options
 */
export function buildPreregistration(options) {
  if (options.design === 'pilot' && options.samples < PILOT_MIN_EPOCHS) {
    throw new Error(`a pilot needs at least ${PILOT_MIN_EPOCHS} attempted epochs per stratum`);
  }
  if (options.design === 'paired') {
    if (options.samples < CONFIRMATORY_MIN_PAIRS) {
      throw new Error(`a confirmatory experiment needs at least ${CONFIRMATORY_MIN_PAIRS} matched pairs per stratum`);
    }
    if (!options.baselineSha || !options.candidateSha || options.baselineSha === options.candidateSha) {
      throw new Error('a paired experiment needs distinct baseline and candidate SHAs');
    }
  }
  const targets = new Set(options.targets || []);
  const strata = [];
  for (const browser of options.browsers) {
    for (const transport of options.transports) {
      for (const scenario of options.scenarios) {
        const id = `${browser}/${transport}/${scenario}`;
        strata.push({
          id,
          browser,
          device: DEVICES[browser],
          transport,
          scenario,
          role: options.design === 'pilot' ? 'pilot' : targets.has(id) ? 'acceptance-target' : 'regression',
        });
      }
    }
  }
  const family = options.design === 'paired'
    ? strata.reduce((total, stratum) => total + (stratum.role === 'regression' ? 3 : 2), 0)
    : 2;
  return {
    schema: 'herdr-resume-preregistration/1',
    design: options.design,
    candidate_sha: options.candidateSha,
    baseline_sha: options.design === 'pilot' ? options.candidateSha : options.baselineSha,
    deadline_ms: DEADLINE_MS,
    sample_size_per_stratum: options.samples,
    sample_unit: options.design === 'pilot' ? 'attempted wake epoch' : 'matched baseline/candidate pair',
    browsers: [...options.browsers],
    variants: options.design === 'paired' ? ['baseline', 'candidate'] : ['baseline'],
    strata,
    seeds: { base_seed: options.baseSeed, derivation: 'fnv1a32(base_seed:stratum:index) for each epoch or pair' },
    order: options.design === 'paired' ? 'seeded independent random order per pair' : 'single variant',
    state_reset: 'fresh browser context, storage and synthetic relay for every run',
    idle_before_hide_ms: IDLE_BEFORE_HIDE_MS,
    direct_upgrade_window_ms: DIRECT_UPGRADE_WINDOW_MS,
    hide_sequence: 'hide, then kill the socket (and blackhole new dials), then advance the frozen wall clock, then wait the real hidden time',
    scenarios: SCENARIOS,
    transports: TRANSPORTS,
    network_restoration_schedule: {
      scenario: 'blackhole-restore',
      restore_after_wake_ms: SCENARIOS['blackhole-restore'].restore_after_wake_ms,
      retransmit_schedule_ms: [1_000, 3_000, 7_000, 15_000, 31_000],
      online_event_at_restore: true,
    },
    unlock_schedule: {
      positive_strata: 'device verification disabled; no unlock in the measured path',
      negative_control: 'cancelled-unlock: the scripted authenticator rejects after 300 ms',
    },
    endpoints: {
      primary: 'on-time completion: the first frame that paints an agent card (not a workspace label) naming the active epoch\'s agents from a snapshot the relay sent with ready, non-stale inventory, on a session that is still the live authenticated path, with no unlock dialog covering it, within 60 s of the first visible event (navigation start after a discard)',
      time_to_fresh: 'all valid attempts; non-completions right-censored at 60 s with their reason',
      supplementary: [
        'success-only p50/p95',
        'dials, handshakes and bytes per epoch',
        'hidden dials and bytes',
        `direct WebRTC upgrade outcome and relay-side timeline within ${DIRECT_UPGRADE_WINDOW_MS / 1_000} s of the wake (gateway-direct strata; synthetic DataChannel, not real ICE), separate from the primary endpoint`,
      ],
      not_measured: ['first-known render (no last-known view before B2)', 'OS wake-to-JS, DNS, TCP, TLS', 'real ICE, radio or NAT behaviour'],
    },
    analysis: {
      method: options.design === 'pilot'
        ? 'descriptive: Wilson 95% for completion, percentile bootstrap 95% for all-attempt quantiles, planning for a later confirmatory design'
        : 'Newcombe paired difference for reliability; paired block bootstrap for quantile ratios; Bonferroni family-wise control; one-sided 95%',
      bootstrap_resamples: 2_000,
      bootstrap_seed: options.baseSeed,
      planned_family_size: family,
    },
    bounds: { ...DEFAULT_BOUNDS },
    harness_invalid_criteria: HARNESS_INVALID_CRITERIA,
    non_completion_outcomes: NON_COMPLETION_OUTCOMES,
    replacement_limit: 2,
    negative_controls: Object.entries(NEGATIVE_CONTROLS).map(([id, expectation]) => ({
      id,
      expectation,
      trials_per_browser: options.negativeTrials ?? 3,
      counts_as_latency_success: false,
    })),
  };
}

/** @param {unknown} value */
export function digest(value) {
  return createHash('sha256').update(JSON.stringify(value)).digest('hex');
}

/**
 * Starts the repository's static release server for one bundle.
 *
 * @param {string} root
 * @param {number} port
 */
async function startServer(root, port) {
  const child = spawn(process.execPath, [join(FRONTEND, 'scripts/browser-server.mjs'), root], {
    env: { ...process.env, PORT: String(port) },
    stdio: ['ignore', 'ignore', 'inherit'],
  });
  const origin = `http://127.0.0.1:${port}`;
  for (let attempt = 0; attempt < 100; attempt += 1) {
    try {
      const response = await fetch(`${origin}/version.json`);
      if (response.ok) return { origin, stop: () => child.kill() };
    } catch {
      // Not listening yet.
    }
    await new Promise((resolveDelay) => setTimeout(resolveDelay, 100));
  }
  child.kill();
  throw new Error(`static server for ${root} did not start`);
}

/**
 * @param {import('@playwright/test').Page} page
 * @param {string} action
 * @param {unknown} [argument]
 */
function call(page, action, argument) {
  return page.evaluate(
    ({ name, value }) => /** @type {any} */ (window).__resumeFixture[name](value),
    { name: action, value: argument },
  );
}

/**
 * Waits until the app document (not the stable bootstrap redirect in front of
 * it) is running the fixture. Waiting survives that redirect.
 *
 * @param {import('@playwright/test').Page} page
 * @param {number} timeout
 */
function appDocument(page, timeout) {
  return page.waitForFunction(
    () => Boolean(/** @type {any} */ (window).__resumeFixture?.ready()),
    null,
    { timeout },
  );
}

/**
 * Maps a harness exception to a fixed vocabulary, so evidence never carries
 * raw error text.
 *
 * @param {unknown} error
 */
export function harnessErrorClass(error) {
  const message = error instanceof Error ? error.message : '';
  if (/Timeout/i.test(message)) return 'timeout';
  if (/Execution context was destroyed|navigat/i.test(message)) return 'context-destroyed';
  if (/closed|detached/i.test(message)) return 'target-closed';
  return 'other';
}

/**
 * @param {import('@playwright/test').Page} page
 * @param {number} deadline
 */
function measure(page, deadline) {
  return page.evaluate(
    (limit) => /** @type {any} */ (window).__resumeFixture.measure([1], limit),
    deadline,
  );
}

/**
 * Whether the local static server still answers at all. Any HTTP status means
 * it is up, so a failed page load is then the bundle's own failure.
 *
 * @param {string} origin
 */
async function serverReachable(origin) {
  try {
    await fetch(`${origin}/`, { signal: AbortSignal.timeout(5_000) });
    return true;
  } catch {
    return false;
  }
}

/**
 * @param {Record<string, number>} after
 * @param {Record<string, number>} before
 */
function delta(after, before) {
  return {
    dials: after.dials - before.dials,
    handshakes: after.handshakes - before.handshakes,
    refreshes: after.refreshes - before.refreshes,
    bytes: after.bytes - before.bytes,
    hidden_dials: after.hiddenDials - before.hiddenDials,
    hidden_bytes: after.hiddenBytes - before.hiddenBytes,
    direct_handshakes: after.directHandshakes - before.directHandshakes,
  };
}

/**
 * Runs one measured wake epoch in a fresh context.
 *
 * @param {import('@playwright/test').Browser} browser
 * @param {Record<string, any>} devices
 * @param {{ origin: string; stratum: Record<string, any>; seed: number; faults?: import('../tests/browser/resume-fixture.mjs').FixtureFaults }} run
 *   `faults` is for the hosted browser suite only; preregistered runs never set it.
 */
export async function runEpoch(browser, devices, run) {
  const { stratum, seed } = run;
  const schedule = epochSchedule(seed, stratum.scenario);
  const spec = SCENARIOS[/** @type {keyof typeof SCENARIOS} */ (stratum.scenario)];
  const relay = relayForTransport(stratum.transport);
  /** @type {Record<string, any>} */
  const record = {
    outcome: null,
    time_to_fresh_ms: null,
    harness_invalid: null,
    wake_document: spec.wake === 'reload' ? 'reloaded' : 'same',
    first_known_render_ms: null,
    direct_upgrade: { outcome: 'not-applicable' },
  };
  /** @type {import('@playwright/test').BrowserContext | undefined} */
  let context;
  /** @type {import('@playwright/test').Page} */
  let page;
  try {
    context = await browser.newContext({ ...devices[stratum.device], baseURL: run.origin, serviceWorkers: 'block' });
    page = await context.newPage();
  } catch {
    // Nothing of the page under test has run yet: an infrastructure failure.
    await context?.close().catch(() => {});
    record.harness_invalid = 'browser-unavailable';
    return record;
  }
  let crashed = false;
  page.on('crash', () => { crashed = true; });
  // Page and browser failures from here on are attempted epochs that did not
  // complete; they are never excluded.
  const failure = () => (crashed ? 'page-crash' : browser.isConnected() ? null : 'browser-disconnected');
  try {
    await page.addInitScript(resumeFixtureInit, {
      relays: [relay],
      forceRelay: stratum.transport === 'gateway-relayed',
      direct: stratum.transport === 'gateway-direct',
      connectionEvents: spec.wake === 'network-change',
      seed: schedule.fixtureSeed,
      latencyMs: /** @type {[number, number]} */ ([8, 30]),
      faults: run.faults,
    });
    const response = await page.goto('/', { waitUntil: 'load', timeout: WARMUP_TIMEOUT_MS }).catch(() => null);
    if (!response || !response.ok()) {
      const failed = failure();
      if (failed) record.outcome = failed;
      else if (!(await serverReachable(run.origin))) record.harness_invalid = 'server-unavailable';
      else record.outcome = 'load-failed';
      return record;
    }
    const warm = await appDocument(page, WARMUP_TIMEOUT_MS)
      .then(() => page.evaluate(
        (timeout) => /** @type {any} */ (window).__resumeFixture.awaitFresh([1], timeout),
        WARMUP_TIMEOUT_MS,
      ))
      .catch(() => null);
    if (!warm || !('renderedAt' in warm)) {
      // The app never showed its first fresh inventory: a failed attempt.
      record.outcome = failure() ?? 'warmup-failed';
      return record;
    }
    if (stratum.transport === 'gateway-direct') {
      // The measured wake starts from the promoted direct path.
      const promoted = await page.waitForFunction(
        () => /** @type {any} */ (window).__resumeFixture.currentPath(1) === 'webrtc',
        null,
        { timeout: WARMUP_TIMEOUT_MS },
      ).then(() => true, () => false);
      if (!promoted) {
        record.outcome = failure() ?? 'warmup-failed';
        return record;
      }
    }
    await page.waitForTimeout(IDLE_BEFORE_HIDE_MS);
    const before = /** @type {Record<string, number>} */ (await call(page, 'stats'));
    let baseline = before;
    if (spec.wake === 'network-change') {
      // Still visible: the path dies silently, then the handoff is reported.
      await call(page, 'killConnections');
      await page.waitForTimeout(schedule.hiddenMs);
      await call(page, 'networkChange');
    } else {
      await call(page, 'hide');
      // The socket dies first, then time passes: nothing can arrive after the
      // frozen interval begins and make a dead path look recently active.
      if (spec.connection !== 'kept') await call(page, 'killConnections');
      if (stratum.scenario === 'blackhole-restore') await call(page, 'blackhole');
      if (schedule.frozenMs) await call(page, 'freeze', schedule.frozenMs);
      await page.waitForTimeout(schedule.hiddenMs);
      if (spec.wake === 'reload') {
        await call(page, 'prepareDiscard');
        await page.reload({ waitUntil: 'commit', timeout: WARMUP_TIMEOUT_MS });
        await appDocument(page, DEADLINE_MS);
        baseline = { dials: 0, handshakes: 0, directHandshakes: 0, refreshes: 0, bytes: 0, hiddenDials: 0, hiddenBytes: 0 };
      } else {
        await call(page, 'show');
        if (stratum.scenario === 'blackhole-restore' && schedule.restoreAfterMs !== null) {
          await page.waitForTimeout(schedule.restoreAfterMs);
          await call(page, 'restore', true);
        }
      }
    }
    const result = /** @type {{ renderedAt?: number; wakeAt: number }} */ (await measure(page, DEADLINE_MS));
    Object.assign(record, delta(/** @type {Record<string, number>} */ (await call(page, 'stats')), baseline));
    if (typeof result.renderedAt === 'number') {
      const elapsed = Math.round(result.renderedAt - result.wakeAt);
      record.outcome = elapsed <= DEADLINE_MS ? 'fresh' : 'late';
      record.time_to_fresh_ms = elapsed;
    } else {
      record.outcome = 'deadline';
    }
    if (stratum.transport === 'gateway-direct') {
      // Observed after the primary outcome is final; its failure never changes it.
      record.direct_upgrade = await observeDirectUpgrade(page, result.wakeAt)
        .catch(() => ({ outcome: 'unobserved', window_ms: DIRECT_UPGRADE_WINDOW_MS }));
    }
  } catch (error) {
    const failed = failure();
    if (failed) record.outcome = failed;
    else {
      // Any other harness exception inside the measured epoch is a failure.
      record.outcome = 'harness-error';
      record.harness_error = harnessErrorClass(error);
    }
  } finally {
    await context.close().catch(() => {});
  }
  return record;
}

/**
 * Waits, within the separate upgrade window from the wake, for the direct
 * WebRTC path to be promoted, and records the relay-side timeline (numbers
 * only). A wake that kept its earlier direct session is `stayed-direct`; one
 * that never tried is `not-attempted`; tries without promotion inside the
 * window are `not-promoted`. The caller records `unobserved` if the page
 * could not be read.
 *
 * @param {import('@playwright/test').Page} page
 * @param {number} wakeAt
 */
export async function observeDirectUpgrade(page, wakeAt) {
  const end = wakeAt + DIRECT_UPGRADE_WINDOW_MS;
  await page.waitForFunction((limit) => {
    const timeline = /** @type {any} */ (window).__resumeFixture.directTimeline(1);
    return timeline.promoted_ms !== null || timeline.stayed_direct || performance.now() >= limit;
  }, end, { polling: 100, timeout: DIRECT_UPGRADE_WINDOW_MS + 5_000 }).catch(() => {});
  const timeline = /** @type {Record<string, unknown>} */ (await call(page, 'directTimeline', 1));
  /** @param {string} key */
  const number = (key) => {
    const value = timeline[key];
    return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null;
  };
  const promotedAt = number('promoted_ms');
  const promoted = promotedAt !== null && promotedAt <= DIRECT_UPGRADE_WINDOW_MS;
  const attempts = number('attempts') ?? 0;
  const outcome = promoted
    ? 'promoted'
    : timeline.stayed_direct === true ? 'stayed-direct' : attempts > 0 ? 'not-promoted' : 'not-attempted';
  return {
    outcome,
    window_ms: DIRECT_UPGRADE_WINDOW_MS,
    attempts,
    refused: number('refused') ?? 0,
    offer_ms: number('offer_ms'),
    answer_ms: number('answer_ms'),
    open_ms: number('open_ms'),
    authenticated_ms: number('authenticated_ms'),
    promoted_ms: promoted ? promotedAt : null,
  };
}

/**
 * @param {import('@playwright/test').Browser} browser
 * @param {Record<string, any>} devices
 * @param {string} origin
 * @param {'chromium' | 'webkit'} name
 * @param {string} control
 * @param {number} trial
 * @param {'baseline' | 'candidate'} variant
 */
async function runNegativeControl(browser, devices, origin, name, control, trial, variant) {
  const result = { control, browser: name, variant, trial, expected: '', observed: '', safety: 'fail' };
  /** @type {import('@playwright/test').BrowserContext | undefined} */
  let context;
  try {
    context = await browser.newContext({ ...devices[DEVICES[name]], baseURL: origin, serviceWorkers: 'block' });
    const page = await context.newPage();
    if (control === 'revoked-credential') {
      result.expected = 'refusal without fresh inventory or redial loop';
      await page.addInitScript(resumeFixtureInit, { relays: [fixtureRelay(1, 'wss', { revoked: true })], seed: trial + 1 });
      await page.goto('/');
      await appDocument(page, WARMUP_TIMEOUT_MS);
      await page.waitForTimeout(5_000);
      const stats = /** @type {Record<string, number>} */ (await call(page, 'stats'));
      const rendered = await call(page, 'hasRendered', 1);
      const refused = await page.getByText('This setup link has expired or was already used').count();
      result.observed = `${rendered ? 'fresh-render' : 'no-render'}, ${refused ? 'refusal shown' : 'no refusal'}, dials=${stats.dials}, handshakes=${stats.handshakes}`;
      result.safety = !rendered && refused > 0 && stats.handshakes === 0 && stats.dials <= 2 ? 'pass' : 'fail';
    } else if (control === 'cancelled-unlock') {
      result.expected = 'locked, no dial, no fresh inventory';
      await page.addInitScript(resumeFixtureInit, {
        relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })],
        lock: { delayMs: 300, cancel: true },
        seed: trial + 1,
      });
      await page.goto('/');
      await appDocument(page, WARMUP_TIMEOUT_MS);
      await page.waitForTimeout(3_000);
      const stats = /** @type {Record<string, number>} */ (await call(page, 'stats'));
      const rendered = await call(page, 'hasRendered', 1);
      const locked = await page.locator('#unlock-dialog').count();
      result.observed = `${locked ? 'locked' : 'unlocked'}, ${rendered ? 'fresh-render' : 'no-render'}, dials=${stats.dials}`;
      result.safety = locked > 0 && !rendered && stats.dials === 0 ? 'pass' : 'fail';
    } else if (control === 'permanent-outage') {
      result.expected = 'no fresh inventory and no crash during a 15 s outage';
      await page.addInitScript(resumeFixtureInit, {
        relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })],
        blackholeAtStart: true,
        seed: trial + 1,
      });
      await page.goto('/');
      await appDocument(page, WARMUP_TIMEOUT_MS);
      await page.waitForTimeout(15_000);
      const stats = /** @type {Record<string, number>} */ (await call(page, 'stats'));
      const rendered = await call(page, 'hasRendered', 1);
      result.observed = `${rendered ? 'fresh-render' : 'no-render'}, dials=${stats.dials}, handshakes=${stats.handshakes}`;
      result.safety = !rendered && stats.handshakes === 0 ? 'pass' : 'fail';
    } else if (control === 'ios-deferred-pairing') {
      result.expected = 'deferred to the Home Screen app with no dial';
      await page.addInitScript(resumeFixtureInit, { relays: [], iosTab: true, seed: trial + 1 });
      await page.goto('/#setup=0123456789abcdef0123456789abcdef&label=Fixture&relay=wss%3A%2F%2Frelay-deferred-private.example');
      await appDocument(page, WARMUP_TIMEOUT_MS);
      await page.waitForTimeout(2_000);
      const stats = /** @type {Record<string, number>} */ (await call(page, 'stats'));
      const deferred = await page.getByText('Add Herdr to the iPhone or iPad Home Screen').count();
      result.observed = `${deferred ? 'deferred' : 'not deferred'}, dials=${stats.dials}`;
      result.safety = deferred > 0 && stats.dials === 0 ? 'pass' : 'fail';
    }
  } catch {
    // A control that cannot be observed is not a safe control.
    result.observed = 'harness error';
    result.safety = 'fail';
  } finally {
    await context?.close().catch(() => {});
  }
  return result;
}

/** @param {string[]} argv */
export function parseArguments(argv) {
  /** @type {Record<string, string>} */
  const values = {};
  for (let index = 0; index < argv.length; index += 1) {
    const arg = argv[index];
    if (!arg.startsWith('--')) continue;
    const next = argv[index + 1];
    if (next === undefined || next.startsWith('--')) values[arg.slice(2)] = 'true';
    else {
      values[arg.slice(2)] = next;
      index += 1;
    }
  }
  const list = (/** @type {string | undefined} */ value, /** @type {string[]} */ fallback) => (value ? value.split(',').filter(Boolean) : fallback);
  const design = values.design === 'paired' ? 'paired' : 'pilot';
  return {
    design: /** @type {'pilot' | 'paired'} */ (design),
    outDir: resolve(values['out-dir'] || 'resume-benchmark'),
    webRoot: resolve(values['web-root'] || join(FRONTEND, '..', 'web')),
    baselineRoot: values['baseline-root'] ? resolve(values['baseline-root']) : '',
    candidateRoot: values['candidate-root'] ? resolve(values['candidate-root']) : '',
    baselineSha: values['baseline-sha'] || '',
    candidateSha: values['candidate-sha'] || process.env.GITHUB_SHA || '',
    browsers: /** @type {Array<keyof typeof DEVICES>} */ (list(values.browsers, ['chromium', 'webkit'])),
    transports: /** @type {Array<keyof typeof TRANSPORTS>} */ (list(values.transports, [...DEFAULT_PILOT_TRANSPORTS])),
    scenarios: /** @type {Array<keyof typeof SCENARIOS>} */ (list(values.scenarios, [...DEFAULT_PILOT_SCENARIOS])),
    samples: Number(values.samples) || (design === 'paired' ? CONFIRMATORY_MIN_PAIRS : PILOT_MIN_EPOCHS),
    baseSeed: Number(values.seed) || 20_261_001,
    targets: list(values.targets, []),
    negativeTrials: Number(values['negative-trials']) || 3,
  };
}

async function main() {
  const options = parseArguments(process.argv.slice(2));
  for (const browser of options.browsers) if (!(browser in DEVICES)) throw new Error(`unknown browser ${browser}`);
  for (const transport of options.transports) if (!(transport in TRANSPORTS)) throw new Error(`unknown transport ${transport}`);
  for (const scenario of options.scenarios) if (!(scenario in SCENARIOS)) throw new Error(`unknown scenario ${scenario}`);
  if (!/^[0-9a-f]{40}$/.test(options.candidateSha)) throw new Error('--candidate-sha (or GITHUB_SHA) must be a full commit SHA');
  const preregistration = buildPreregistration(options);
  await mkdir(options.outDir, { recursive: true });
  const registrationDigest = digest(preregistration);
  await writeFile(join(options.outDir, 'preregistration.json'), `${JSON.stringify(preregistration, null, 2)}\n`);

  const roots = options.design === 'paired'
    ? { baseline: options.baselineRoot, candidate: options.candidateRoot }
    : { baseline: options.webRoot };
  if (options.design === 'paired' && (!roots.baseline || !roots.candidate)) {
    throw new Error('a paired experiment needs --baseline-root and --candidate-root');
  }
  /** @type {Record<string, { origin: string; stop: () => void }>} */
  const servers = {};
  /** @type {Record<string, string>} */
  const builds = {};
  let port = 4_310;
  for (const [variant, root] of Object.entries(roots)) {
    if (!root) continue;
    servers[variant] = await startServer(root, port);
    port += 1;
    builds[variant] = JSON.parse(await readFile(join(root, 'version.json'), 'utf8')).build;
  }

  const { chromium, devices, webkit } = await import('@playwright/test');
  /** @type {Record<string, any>[]} */
  const attempts = [];
  /** @type {Record<string, any>[]} */
  const negativeControls = [];
  const environment = { node: process.version, platform: process.platform, arch: process.arch, browsers: /** @type {Record<string, string>} */ ({}) };
  const startedAt = Date.now();
  try {
    await Promise.all(options.browsers.map(async (name) => {
      const launch = () => (name === 'chromium' ? chromium : webkit).launch();
      let browser = await launch();
      environment.browsers[name] = browser.version();
      // A lost browser is recorded against the epoch it ended; the next epoch
      // gets a fresh one rather than inheriting the failure.
      const ready = async () => {
        if (!browser.isConnected()) browser = await launch();
        return browser;
      };
      try {
        for (const stratum of preregistration.strata.filter((entry) => entry.browser === name)) {
          for (let index = 0; index < options.samples; index += 1) {
            const seed = epochSeed(options.baseSeed, stratum.id, index);
            const variants = options.design === 'paired' ? pairOrder(seed) : /** @type {const} */ (['baseline']);
            for (let replacement = 0; replacement <= preregistration.replacement_limit; replacement += 1) {
              /** @type {Record<string, any>[]} */
              const round = [];
              for (const variant of variants) {
                const record = await runEpoch(await ready(), devices, { origin: servers[variant].origin, stratum, seed });
                round.push({
                  stratum: stratum.id,
                  browser: name,
                  transport: stratum.transport,
                  scenario: stratum.scenario,
                  variant,
                  pair: index,
                  seed,
                  order: `${variants[0]}-first`,
                  replacement,
                  ...record,
                });
              }
              attempts.push(...round);
              // Only an objective, preregistered infrastructure failure earns a
              // same-seed replacement, and it replaces the whole pair.
              if (!round.some((entry) => entry.harness_invalid)) break;
            }
          }
          console.error(`${stratum.id}: ${attempts.filter((entry) => entry.stratum === stratum.id).length} attempts recorded`);
        }
        // Every variant must refuse what it should refuse: a candidate is never
        // vouched for by the baseline's safety controls.
        for (const variant of /** @type {Array<'baseline' | 'candidate'>} */ (preregistration.variants)) {
          for (const control of Object.keys(NEGATIVE_CONTROLS)) {
            for (let trial = 0; trial < options.negativeTrials; trial += 1) {
              negativeControls.push(await runNegativeControl(
                await ready(), devices, servers[variant].origin, name, control, trial, variant,
              ));
            }
          }
        }
      } finally {
        await browser.close().catch(() => {});
      }
    }));
  } finally {
    for (const server of Object.values(servers)) server.stop();
  }

  const evidence = {
    schema: EVIDENCE_SCHEMA,
    design: options.design,
    preregistration,
    preregistration_sha256: registrationDigest,
    builds,
    environment,
    duration_ms: Date.now() - startedAt,
    attempts,
    negative_controls: negativeControls,
  };
  const serialized = JSON.stringify(evidence, null, 2);
  // Evidence is uploaded as an artifact: refuse to write anything that could
  // identify a relay, credential, path or content, even synthetic ones.
  const relays = Object.keys(TRANSPORTS).map((transport) => relayForTransport(transport));
  for (const marker of resumeSensitiveMarkers(relays)) {
    if (serialized.includes(marker)) throw new Error('benchmark evidence contains a sensitive fixture marker');
  }
  await writeFile(join(options.outDir, 'evidence.json'), `${serialized}\n`);
  console.error(`wrote ${attempts.length} attempted epochs and ${negativeControls.length} negative-control trials`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  await main();
}
