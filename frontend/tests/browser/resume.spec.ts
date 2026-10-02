import { devices, expect, test, type Locator, type Page } from '@playwright/test';
import { epochSeed } from '../../scripts/analyze-resume-benchmarks.mjs';
import { observeDirectUpgrade, runEpoch, SCENARIOS, TRANSPORTS } from '../../scripts/run-resume-benchmarks.mjs';
import {
  fixtureRelay,
  resumeFixtureInit,
  resumeSensitiveMarkers,
  type FixtureConfig,
} from './resume-fixture.mjs';

interface RenderVerdict {
  slot: number;
  epoch: number;
  seq: number;
  path: string;
  verdict: string;
}

interface ExportedGroup {
  path: string;
  lifecycle: string;
  samples: number;
  in_progress: number;
  valid_attempts: number;
  fresh_on_time: number;
  non_completions: Record<string, number>;
  retries: { attempts: number; path_attempts: number };
  phase_p50_ms: Record<string, number>;
  unavailable: string[];
  not_applicable: string[];
  transport_eligible_ms: { n: number };
  direct: { attempted: number; promoted: number };
}

interface ExportedSummary {
  schema: string;
  deadline_ms: number;
  epochs: number;
  samples: number;
  unlock: { requested: number; unlocked: number; cancelled: number; unlock_ms: { n: number } };
  groups: ExportedGroup[];
}

interface Booted {
  page: Page;
  logs: string[];
  markers: string[];
}

async function boot(page: Page, config: FixtureConfig): Promise<Booted> {
  const logs: string[] = [];
  page.on('console', (message) => logs.push(message.text()));
  page.on('pageerror', (error) => logs.push(String(error)));
  await page.addInitScript(resumeFixtureInit, config);
  await page.goto('/');
  // The stable bootstrap document redirects to the build entry first.
  await page.waitForFunction(() => Boolean((window as any).__resumeFixture?.ready()));
  return { page, logs, markers: resumeSensitiveMarkers(config.relays) };
}

async function awaitFresh(page: Page, slots: number[], timeoutMs = 20_000): Promise<void> {
  const result = await page.evaluate(
    ({ wanted, timeout }) => (window as any).__resumeFixture.awaitFresh(wanted, timeout),
    { wanted: slots, timeout: timeoutMs },
  );
  expect(result).toHaveProperty('renderedAt');
}

async function fixture(page: Page, action: string, argument?: unknown): Promise<unknown> {
  return page.evaluate(
    ({ name, value }) => (window as any).__resumeFixture[name](value),
    { name: action, value: argument },
  );
}

async function openResumeTiming(page: Page): Promise<Locator> {
  await page.getByRole('button', { name: 'Settings' }).click();
  const card = page.getByRole('region', { name: 'Resume Timing' });
  await expect(card).toBeVisible();
  return card;
}

async function exportSummary(card: Locator): Promise<{ text: string; summary: ExportedSummary }> {
  await card.getByRole('button', { name: 'Export Redacted Summary' }).click();
  const field = card.getByLabel('Redacted resume summary');
  await expect(field).toBeVisible();
  const text = await field.inputValue();
  return { text, summary: JSON.parse(text) as ExportedSummary };
}

async function expectNoSensitiveMarkers(booted: Booted, card: Locator, exported: string): Promise<void> {
  const visible = await card.innerText();
  for (const marker of booted.markers) {
    expect(visible, `resume timing UI leaked ${marker}`).not.toContain(marker);
    expect(exported, `resume summary export leaked ${marker}`).not.toContain(marker);
    for (const line of booted.logs) expect(line, `browser log leaked ${marker}`).not.toContain(marker);
  }
  expect(exported).not.toMatch(/wss?:\/\//);
  expect(exported).not.toMatch(/\b\d{1,3}(?:\.\d{1,3}){3}\b/);
}

function group(summary: ExportedSummary, path: string, lifecycle: string): ExportedGroup {
  const found = summary.groups.find((entry) => entry.path === path && entry.lifecycle === lifecycle);
  expect(found, `missing ${path} ${lifecycle} sample`).toBeTruthy();
  return found!;
}

test('measures a Cloudflare-labelled WSS cold launch and warm resume', async ({ page }) => {
  const booted = await boot(page, { relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })], seed: 11 });
  await awaitFresh(page, [1]);
  await fixture(page, 'hide');
  await fixture(page, 'show');
  await awaitFresh(page, [1]);

  const card = await openResumeTiming(page);
  await expect(card.getByRole('rowheader', { name: 'Cloudflare WSS' })).toHaveCount(2);
  await expect(card.getByText('cannot be observed by a web app')).toBeVisible();
  const { text, summary } = await exportSummary(card);
  expect(summary).toMatchObject({ schema: 'herdr-resume-summary/1', deadline_ms: 60_000 });
  const cold = group(summary, 'wss/cloudflare', 'cold-launch');
  expect(cold).toMatchObject({ valid_attempts: 1, fresh_on_time: 1 });
  // A cold launch cannot see the time before its first script ran.
  expect(cold.unavailable).toEqual(expect.arrayContaining(['os-wake-to-js', 'dns', 'tcp', 'tls']));
  for (const phase of ['dial', 'open', 'e2ee-hello', 'e2ee-server-hello', 'e2ee-confirm', 'authenticated', 'first-frame', 'inventory', 'rendered']) {
    expect(cold.phase_p50_ms, `cold launch is missing ${phase}`).toHaveProperty(phase);
  }
  expect(cold.phase_p50_ms).not.toHaveProperty('dns');
  const warm = group(summary, 'wss/cloudflare', 'warm');
  expect(warm).toMatchObject({ valid_attempts: 1, fresh_on_time: 1, retries: { attempts: 0 } });
  expect(warm.phase_p50_ms).toHaveProperty('probe');
  expect(warm.not_applicable).toEqual(expect.arrayContaining(['dial', 'open', 'authenticated']));
  await expectNoSensitiveMarkers(booted, card, text);
});

test('labels Tailscale WSS from the trusted descriptor, never from the hostname', async ({ page }) => {
  const booted = await boot(page, {
    relays: [
      fixtureRelay(1, 'wss', { ingress: 'tailscale-managed', host: 'relay-1-private-host.trycloudflare-lookalike.invalid' }),
      fixtureRelay(2, 'wss', { ingress: null, host: 'relay-2-private-host.tailnet-lookalike.ts.net' }),
    ],
    seed: 12,
  });
  await awaitFresh(page, [1, 2]);
  const card = await openResumeTiming(page);
  await expect(card.getByRole('rowheader', { name: 'Tailscale WSS (managed)' })).toHaveCount(1);
  await expect(card.getByRole('rowheader', { name: 'WSS, ingress not reported' })).toHaveCount(1);
  const { text, summary } = await exportSummary(card);
  expect(summary.groups.map((entry) => entry.path).sort()).toEqual(['wss/ingress-unknown', 'wss/tailscale-managed']);
  await expectNoSensitiveMarkers(booted, card, text);
});

test('records relayed gateway reconnects with their gateway handshake phases', async ({ page }) => {
  const booted = await boot(page, { relays: [fixtureRelay(1, 'hybrid')], forceRelay: true, seed: 13 });
  await awaitFresh(page, [1]);
  await fixture(page, 'hide');
  // Five minutes frozen: the half-open socket is a corpse, so resume redials.
  await fixture(page, 'killConnections');
  await fixture(page, 'freeze', 5 * 60_000);
  await fixture(page, 'show');
  await awaitFresh(page, [1]);

  const card = await openResumeTiming(page);
  await expect(card.getByRole('rowheader', { name: 'Gateway relayed' })).toHaveCount(2);
  const { text, summary } = await exportSummary(card);
  const cold = group(summary, 'gateway/relayed', 'cold-launch');
  for (const phase of ['dial', 'open', 'gateway-hello', 'gateway-proof', 'gateway-ready', 'e2ee-hello', 'authenticated', 'inventory', 'rendered']) {
    expect(cold.phase_p50_ms, `gateway cold launch is missing ${phase}`).toHaveProperty(phase);
  }
  const reconnect = group(summary, 'gateway/relayed', 'reconnect');
  expect(reconnect).toMatchObject({ valid_attempts: 1, fresh_on_time: 1 });
  expect(reconnect.retries.attempts).toBeGreaterThanOrEqual(1);
  expect(reconnect.direct.attempted).toBe(0);
  await expectNoSensitiveMarkers(booted, card, text);
});

test('keeps the direct WebRTC upgrade on its own timeline', async ({ page }) => {
  const booted = await boot(page, { relays: [fixtureRelay(1, 'hybrid')], direct: true, seed: 14 });
  await awaitFresh(page, [1]);
  // The direct session authenticates on its own; promotion follows its first message.
  await expect.poll(async () => ((await fixture(page, 'stats')) as { directHandshakes: number }).directHandshakes).toBeGreaterThanOrEqual(1);
  await page.waitForTimeout(250);
  await fixture(page, 'hide');
  await fixture(page, 'show');
  await awaitFresh(page, [1]);

  const card = await openResumeTiming(page);
  const { text, summary } = await exportSummary(card);
  // First inventory arrived relayed; the upgrade is reported separately.
  const cold = group(summary, 'gateway/relayed', 'cold-launch');
  expect(cold.direct.attempted).toBeGreaterThanOrEqual(1);
  expect(cold.direct.promoted).toBe(1);
  expect(cold.phase_p50_ms).not.toHaveProperty('offer');
  // Once promoted, a warm resume is served by the direct path.
  const warm = group(summary, 'gateway/direct', 'warm');
  expect(warm).toMatchObject({ valid_attempts: 1, fresh_on_time: 1 });
  await expect(card.getByRole('rowheader', { name: 'Gateway direct (WebRTC)' })).toHaveCount(1);
  await expectNoSensitiveMarkers(booted, card, text);
});

test('reports a revoked credential as a refusal, never a fast resume', async ({ page }) => {
  const booted = await boot(page, { relays: [fixtureRelay(1, 'wss', { revoked: true })], seed: 15 });
  await expect(page.getByText('This setup link has expired or was already used').first()).toBeVisible();
  await page.waitForTimeout(500);
  const stats = (await fixture(page, 'stats')) as { dials: number; handshakes: number };
  expect(stats).toMatchObject({ handshakes: 0 });
  expect(stats.dials).toBe(1);

  const card = await openResumeTiming(page);
  const { text, summary } = await exportSummary(card);
  const refused = group(summary, 'wss/ingress-unknown', 'cold-launch');
  expect(refused).toMatchObject({ valid_attempts: 1, fresh_on_time: 0, non_completions: { 'auth-rejected': 1 } });
  await expectNoSensitiveMarkers(booted, card, text);
});

test('splits a scripted device unlock from network work', async ({ page }) => {
  const booted = await boot(page, {
    relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })],
    lock: { delayMs: 400, cancel: false },
    seed: 16,
  });
  await awaitFresh(page, [1]);
  const card = await openResumeTiming(page);
  const { text, summary } = await exportSummary(card);
  expect(summary.unlock).toMatchObject({ requested: 1, unlocked: 1, cancelled: 0, unlock_ms: { n: 1 } });
  expect(group(summary, 'wss/cloudflare', 'cold-launch').transport_eligible_ms.n).toBe(1);
  await expectNoSensitiveMarkers(booted, card, text);
});

/**
 * Waits until the cold connection's own refresh has been answered and its
 * reply delivered, as the benchmark runner does before hiding. The in-app
 * metric cannot tell a reply already in flight at the wake from the answer to
 * the post-wake probe (docs/resume.md), so tests that assert in-app outcomes
 * start the wake from a quiet connection.
 */
async function quiesce(page: Page): Promise<void> {
  await expect.poll(async () => ((await fixture(page, 'stats')) as { refreshes: number }).refreshes).toBeGreaterThanOrEqual(1);
  await page.waitForTimeout(500);
}

async function renderLog(page: Page): Promise<RenderVerdict[]> {
  return (await fixture(page, 'renderLog')) as RenderVerdict[];
}

async function awaitTimeout(page: Page, timeoutMs: number): Promise<void> {
  const result = await page.evaluate(
    ({ timeout }) => (window as any).__resumeFixture.awaitFresh([1], timeout),
    { timeout: timeoutMs },
  );
  expect(result).toEqual({ timedOut: true });
}

test('does not count a workspace-only refresh as fresh agents', async ({ page }) => {
  const booted = await boot(page, {
    relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })],
    faults: { workspaceOnly: true },
    seed: 18,
  });
  await awaitFresh(page, [1]);
  await quiesce(page);
  await fixture(page, 'hide');
  await fixture(page, 'show');
  // The refresh renders a new workspace label, but no post-wake agents.
  await expect(page.getByText('fixture-ws-1-2').first()).toBeVisible();
  await awaitTimeout(page, 3_000);
  expect((await renderLog(page)).filter((entry) => entry.epoch === 2)).toEqual([]);

  const card = await openResumeTiming(page);
  const { text, summary } = await exportSummary(card);
  expect(group(summary, 'wss/cloudflare', 'warm')).toMatchObject({ samples: 1, fresh_on_time: 0 });
  await expectNoSensitiveMarkers(booted, card, text);
});

test('does not count stale post-wake agents as fresh', async ({ page }) => {
  const booted = await boot(page, {
    relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })],
    faults: { stale: true },
    seed: 19,
  });
  await awaitFresh(page, [1]);
  await quiesce(page);
  await fixture(page, 'hide');
  await fixture(page, 'show');
  // The stale agents do render; the endpoint rejects them.
  await expect.poll(async () => (await renderLog(page)).some((entry) => entry.epoch === 2 && entry.verdict === 'not-authoritative')).toBe(true);
  await awaitTimeout(page, 3_000);
  expect((await renderLog(page)).some((entry) => entry.epoch === 2 && entry.verdict === 'counted')).toBe(false);

  const card = await openResumeTiming(page);
  const { text, summary } = await exportSummary(card);
  expect(group(summary, 'wss/cloudflare', 'warm')).toMatchObject({ samples: 1, fresh_on_time: 0 });
  await expectNoSensitiveMarkers(booted, card, text);
});

test('counts a fresh snapshot that replaces another before it paints', async ({ page }) => {
  // A reconnect delivers its initial snapshot and, moments later, the answer
  // to the app's own refresh; the second can replace the first before the
  // frame paints. The endpoint must count what the frame actually shows.
  await boot(page, {
    relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })],
    faults: { burst: true },
    seed: 21,
  });
  await awaitFresh(page, [1]);
  await quiesce(page);
  await fixture(page, 'hide');
  await fixture(page, 'show');
  await awaitFresh(page, [1], 5_000);
  const counted = (await renderLog(page)).filter((entry) => entry.epoch === 2 && entry.verdict === 'counted');
  expect(counted).toHaveLength(1);
});

test('does not count agents delivered on a path the relay is abandoning', async ({ page }) => {
  await boot(page, {
    relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })],
    faults: { abandonFirst: true },
    seed: 20,
  });
  await awaitFresh(page, [1]);
  await quiesce(page);
  const before = (await fixture(page, 'stats')) as { handshakes: number };
  await fixture(page, 'hide');
  await fixture(page, 'show');
  const result = (await page.evaluate(
    () => (window as any).__resumeFixture.measure([1], 20_000),
  )) as { renderedAt?: number; wakeAt: number };
  expect(result.renderedAt).toBeDefined();
  // Success waited for the reconnect that follows the abandoned path.
  expect(result.renderedAt! - result.wakeAt).toBeGreaterThanOrEqual(900);
  const log = (await renderLog(page)).filter((entry) => entry.epoch === 2);
  const rejected = log.find((entry) => entry.verdict === 'inactive-path');
  const counted = log.find((entry) => entry.verdict === 'counted');
  expect(rejected).toBeDefined();
  expect(counted).toBeDefined();
  expect(counted!.seq).toBeGreaterThan(rejected!.seq);
  const after = (await fixture(page, 'stats')) as { handshakes: number };
  expect(after.handshakes).toBe(before.handshakes + 1);
});

test('counts only the path the app is using when a direct promotion lands before the paint', async ({ page }) => {
  await boot(page, {
    relays: [fixtureRelay(1, 'hybrid')],
    direct: true,
    faults: { holdPaintUntilDirect: true },
    seed: 22,
  });
  await awaitFresh(page, [1]);
  await expect.poll(() => fixture(page, 'currentPath', 1)).toBe('webrtc');
  await quiesce(page);
  // Five frozen minutes on a dead path: the wake redials the gateway, whose
  // fresh agents render first; the direct upgrade is promoted before their
  // paint, so they are no longer on the path the app uses.
  await fixture(page, 'hide');
  await fixture(page, 'killConnections');
  await fixture(page, 'freeze', 5 * 60_000);
  await fixture(page, 'show');
  await awaitFresh(page, [1], 20_000);
  const log = (await renderLog(page)).filter((entry) => entry.epoch === 2);
  const counted = log.filter((entry) => entry.verdict === 'counted');
  expect(counted).toHaveLength(1);
  expect(counted[0].path).toBe('webrtc');
  expect(log.some((entry) => entry.path === 'gateway' && entry.verdict === 'retired-before-paint:not-current-path')).toBe(true);
  expect(log.every((entry) => entry.path !== 'gateway' || entry.verdict !== 'counted')).toBe(true);
});

test('counts in-app freshness only once the agent list shows it', async ({ page }) => {
  await boot(page, { relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })], seed: 23 });
  await awaitFresh(page, [1]);
  await quiesce(page);
  // Settings replaces the agent list: a warm wake's fresh snapshot arrives
  // but is not rendered anywhere.
  const card = await openResumeTiming(page);
  await fixture(page, 'hide');
  await fixture(page, 'show');
  await expect.poll(async () => ((await fixture(page, 'stats')) as { refreshes: number }).refreshes).toBeGreaterThanOrEqual(2);
  await page.waitForTimeout(500);
  const pending = (await exportSummary(card)).summary;
  expect(group(pending, 'wss/cloudflare', 'warm')).toMatchObject({ samples: 1, in_progress: 1, fresh_on_time: 0 });
  // Back on the agent list, the next frame shows the fresh inventory.
  await page.getByRole('button', { name: 'Settings' }).click();
  await expect(page.getByRole('main', { name: 'Agents' })).toBeVisible();
  await page.waitForTimeout(250);
  const shown = (await exportSummary(await openResumeTiming(page))).summary;
  expect(group(shown, 'wss/cloudflare', 'warm')).toMatchObject({ samples: 1, in_progress: 0, fresh_on_time: 1 });
});

/**
 * Every preregisterable workload and path runs through the benchmark
 * runner's own epoch code, one seeded epoch each, so the hosted suite proves
 * the selectable strata work, not only the bounded default pilot.
 */
test.describe('benchmark runner workloads', () => {
  for (const transport of Object.keys(TRANSPORTS)) {
    for (const scenario of Object.keys(SCENARIOS) as Array<keyof typeof SCENARIOS>) {
      test(`${transport} ${scenario} completes fresh through the runner`, async ({ browser, browserName, baseURL }) => {
        test.setTimeout(90_000);
        const id = `${browserName}/${transport}/${scenario}`;
        const stratum = {
          id,
          browser: browserName,
          device: browserName === 'webkit' ? 'iPhone 15' : 'Pixel 7',
          transport,
          scenario,
        };
        const record = await runEpoch(browser, devices, { origin: String(baseURL), stratum, seed: epochSeed(20_261_002, id, 0) });
        expect(record, JSON.stringify({ outcome: record.outcome, error: record.harness_error })).toMatchObject({
          harness_invalid: null,
          outcome: 'fresh',
          wake_document: SCENARIOS[scenario].wake === 'reload' ? 'reloaded' : 'same',
        });
        expect(record.time_to_fresh_ms).toBeGreaterThanOrEqual(0);
        expect(record.time_to_fresh_ms).toBeLessThanOrEqual(60_000);
        if (SCENARIOS[scenario].connection === 'kept') {
          // The live connection answered the post-wake probe.
          expect(record).toMatchObject({ dials: 0, handshakes: 0, direct_handshakes: 0 });
          expect(record.refreshes).toBeGreaterThanOrEqual(1);
        } else {
          // The dead path was replaced inside the epoch.
          expect(record.dials).toBeGreaterThanOrEqual(1);
          expect(record.handshakes).toBeGreaterThanOrEqual(1);
        }
        if (scenario === 'network-change') expect(record.time_to_fresh_ms).toBeGreaterThanOrEqual(1_500);
        const upgrade = record.direct_upgrade as Record<string, any>;
        if (transport !== 'gateway-direct') {
          expect(upgrade).toEqual({ outcome: 'not-applicable' });
        } else if (SCENARIOS[scenario].connection === 'kept') {
          // The promoted direct session survived the wake.
          expect(upgrade).toMatchObject({ outcome: 'stayed-direct', attempts: 0, promoted_ms: null });
        } else {
          // A new direct session was negotiated after the reconnect.
          expect(upgrade).toMatchObject({ outcome: 'promoted', window_ms: 30_000, refused: 0 });
          expect(upgrade.attempts).toBeGreaterThanOrEqual(1);
          const steps = [upgrade.offer_ms, upgrade.answer_ms, upgrade.open_ms, upgrade.authenticated_ms, upgrade.promoted_ms];
          for (const step of steps) expect(typeof step).toBe('number');
          expect([...steps].sort((left, right) => left - right)).toEqual(steps);
          expect(upgrade.promoted_ms).toBeLessThanOrEqual(30_000);
        }
      });
    }
  }
});

test.describe('benchmark runner direct upgrade outcomes', () => {
  const stratum = (browserName: string) => ({
    id: `${browserName}/gateway-direct/hidden-5m`,
    browser: browserName,
    device: browserName === 'webkit' ? 'iPhone 15' : 'Pixel 7',
    transport: 'gateway-direct',
    scenario: 'hidden-5m',
  });

  test('reports a delayed upgrade separately from the primary completion', async ({ browser, browserName, baseURL }) => {
    test.setTimeout(90_000);
    const run = stratum(browserName);
    const record = await runEpoch(browser, devices, {
      origin: String(baseURL), stratum: run, seed: epochSeed(20_261_003, run.id, 0), faults: { directDelayMs: 3_000 },
    });
    expect(record).toMatchObject({ harness_invalid: null, outcome: 'fresh' });
    const upgrade = record.direct_upgrade as Record<string, number | string | null>;
    expect(upgrade).toMatchObject({ outcome: 'promoted', refused: 0 });
    expect(upgrade.promoted_ms).toBeGreaterThanOrEqual(3_000);
    // The relayed path served fresh agents first; the primary time is not the upgrade's.
    expect(record.time_to_fresh_ms).toBeLessThan(3_000);
    expect(record.time_to_fresh_ms).toBeLessThan(Number(upgrade.promoted_ms));
  });

  test('reports a refused upgrade as not promoted while the primary resume completes', async ({ browser, browserName, baseURL }) => {
    test.setTimeout(90_000);
    const run = stratum(browserName);
    const record = await runEpoch(browser, devices, {
      origin: String(baseURL), stratum: run, seed: epochSeed(20_261_004, run.id, 0), faults: { directRefuse: true },
    });
    expect(record).toMatchObject({ harness_invalid: null, outcome: 'fresh' });
    expect(record.time_to_fresh_ms).toBeLessThan(5_000);
    const upgrade = record.direct_upgrade as Record<string, number | string | null>;
    expect(upgrade).toMatchObject({ outcome: 'not-promoted', promoted_ms: null, open_ms: null, window_ms: 30_000 });
    expect(upgrade.attempts).toBeGreaterThanOrEqual(1);
    expect(upgrade.refused).toBeGreaterThanOrEqual(1);
    // An attempt may still be negotiating when the window closes.
    expect(upgrade.refused).toBeLessThanOrEqual(Number(upgrade.attempts));
  });
});

test.describe('direct upgrade window', () => {
  interface RawTimeline {
    events: Array<{ kind: string; at_ms: number }>;
    window_closed: boolean;
    kept_direct: boolean | null;
  }
  const none = { attempts: 0, refused: 0, offer_ms: null, answer_ms: null, open_ms: null, authenticated_ms: null, promoted_ms: null };

  async function directBoot(page: Page, seed: number): Promise<void> {
    // A one-second window keeps these checks short; preregistered runs use 30 s.
    await boot(page, { relays: [fixtureRelay(1, 'hybrid')], direct: true, directWindowMs: 1_000, seed });
    await awaitFresh(page, [1]);
    await expect.poll(() => fixture(page, 'currentPath', 1)).toBe('webrtc');
    await page.waitForTimeout(500);
  }

  test('ignores an upgrade that only happens after its window, even when the primary resume finishes later', async ({ page }) => {
    test.setTimeout(60_000);
    await directBoot(page, 27);
    await fixture(page, 'hide');
    // Silently half-open while hidden: the app only finds out from its 2 s
    // post-wake probe, then reconnects and upgrades again after the window.
    await fixture(page, 'killConnections');
    await fixture(page, 'show');
    const primary = await page.evaluate(() => (window as any).__resumeFixture.measure([1], 60_000)) as { renderedAt?: number; wakeAt: number };
    expect(typeof primary.renderedAt).toBe('number');
    expect(Number(primary.renderedAt) - primary.wakeAt).toBeGreaterThan(1_000);
    await expect.poll(() => fixture(page, 'currentPath', 1), { timeout: 20_000 }).toBe('webrtc');
    const raw = (await fixture(page, 'directTimeline', 1)) as RawTimeline;
    expect(raw).toMatchObject({ window_closed: true, kept_direct: false });
    // A complete new upgrade happened, all of it after the window closed.
    for (const kind of ['peer', 'offer', 'answer', 'open', 'authenticated', 'promoted']) {
      expect(raw.events.some((event) => event.kind === kind), kind).toBe(true);
    }
    expect(raw.events.every((event) => event.at_ms > 1_000)).toBe(true);
    expect(await observeDirectUpgrade(page, 1_000)).toEqual({ outcome: 'not-attempted', window_ms: 1_000, ...none });
  });

  test('keeps a direct session that outlived its window as stayed-direct even if it dies afterwards', async ({ page }) => {
    await directBoot(page, 28);
    await fixture(page, 'hide');
    await fixture(page, 'show');
    await awaitFresh(page, [1]);
    await expect.poll(async () => ((await fixture(page, 'directTimeline', 1)) as RawTimeline).window_closed).toBe(true);
    expect(await fixture(page, 'directTimeline', 1)).toMatchObject({ kept_direct: true });
    // The direct path dies after the window closed and before the record is read.
    await fixture(page, 'killConnections');
    expect(await fixture(page, 'currentPath', 1)).toBeNull();
    expect(await observeDirectUpgrade(page, 1_000)).toEqual({ outcome: 'stayed-direct', window_ms: 1_000, ...none });
  });
});

/** Counts active change listeners on the agent rail's wide-screen media query. */
async function trackRailMedia(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const native = window.matchMedia.bind(window);
    let active = 0;
    Object.defineProperty(window, '__railMediaListeners', { configurable: true, get: () => active });
    window.matchMedia = (query: string) => {
      const list = native(query);
      if (query !== '(min-width: 900px)') return list;
      const add = list.addEventListener.bind(list);
      const remove = list.removeEventListener.bind(list);
      const listeners = new Set<unknown>();
      list.addEventListener = ((type: string, listener: EventListenerOrEventListenerObject, options?: AddEventListenerOptions) => {
        if (type === 'change' && !listeners.has(listener)) {
          listeners.add(listener);
          active += 1;
        }
        add(type, listener, options);
      }) as typeof list.addEventListener;
      list.removeEventListener = ((type: string, listener: EventListenerOrEventListenerObject, options?: EventListenerOptions) => {
        if (type === 'change' && listeners.delete(listener)) active -= 1;
        remove(type, listener, options);
      }) as typeof list.removeEventListener;
      return list;
    };
  });
}

async function railListeners(page: Page): Promise<number> {
  return page.evaluate(() => (window as any).__railMediaListeners as number);
}

async function refreshes(page: Page): Promise<number> {
  return ((await fixture(page, 'stats')) as { refreshes: number }).refreshes;
}

async function openTerminal(page: Page): Promise<Locator> {
  await page.locator('article.agent-card .agent-open').first().click();
  const rail = page.locator('aside.agent-rail');
  await expect(rail).toHaveCount(1);
  return rail;
}

/** Settings, then Back to the terminal, then Back to the agent list. */
async function backToAgents(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.locator('aside.agent-rail')).toHaveCount(1);
  await page.getByRole('button', { name: 'Back' }).click();
  await expect(page.getByRole('main', { name: 'Agents' })).toBeVisible();
}

test.describe('in-app freshness needs a visible inventory view', () => {
  test('does not count inventory behind a phone terminal whose agent rail is hidden', async ({ page }) => {
    await trackRailMedia(page);
    const booted = await boot(page, { relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })], seed: 24 });
    await awaitFresh(page, [1]);
    await quiesce(page);
    const rail = await openTerminal(page);
    // Mounted beside the terminal, but CSS-hidden at phone width.
    await expect(rail).toBeHidden();
    expect(await rail.evaluate((element) => getComputedStyle(element).display)).toBe('none');
    expect(await railListeners(page)).toBe(1);
    const asked = await refreshes(page);
    await fixture(page, 'hide');
    await fixture(page, 'show');
    await expect.poll(() => refreshes(page)).toBeGreaterThan(asked);
    await page.waitForTimeout(500);
    const pending = await exportSummary(await openResumeTiming(page));
    expect(group(pending.summary, 'wss/cloudflare', 'warm')).toMatchObject({ samples: 1, in_progress: 1, fresh_on_time: 0 });
    expect(group(pending.summary, 'wss/cloudflare', 'warm').phase_p50_ms).not.toHaveProperty('rendered');

    // The agent list's first visible frame completes it, inside the deadline.
    await backToAgents(page);
    expect(await railListeners(page)).toBe(0);
    await page.waitForTimeout(250);
    const card = await openResumeTiming(page);
    const shown = await exportSummary(card);
    expect(group(shown.summary, 'wss/cloudflare', 'warm')).toMatchObject({ samples: 1, in_progress: 0, fresh_on_time: 1 });
    await expectNoSensitiveMarkers(booted, card, shown.text);
  });

  test('counts inventory shown by a visible desktop agent rail beside the terminal', async ({ page }) => {
    await page.setViewportSize({ width: 1_200, height: 900 });
    await trackRailMedia(page);
    const booted = await boot(page, { relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })], seed: 25 });
    await awaitFresh(page, [1]);
    await quiesce(page);
    const rail = await openTerminal(page);
    await expect(rail).toBeVisible();
    expect(await railListeners(page)).toBe(1);
    const asked = await refreshes(page);
    await fixture(page, 'hide');
    await fixture(page, 'show');
    await expect.poll(() => refreshes(page)).toBeGreaterThan(asked);
    await page.waitForTimeout(500);
    // Settings shows no inventory: completion must already have happened on the rail.
    const card = await openResumeTiming(page);
    const shown = await exportSummary(card);
    expect(group(shown.summary, 'wss/cloudflare', 'warm')).toMatchObject({ samples: 1, in_progress: 0, fresh_on_time: 1 });
    expect(await railListeners(page)).toBe(0);
    await expectNoSensitiveMarkers(booted, card, shown.text);
  });

  test('does not count a desktop rail hidden by a resize before the fresh frame', async ({ page }) => {
    await page.setViewportSize({ width: 1_200, height: 900 });
    await trackRailMedia(page);
    const booted = await boot(page, {
      relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })],
      // The post-wake answer is held well inside the app's 2 s probe timeout.
      faults: { delayRefreshMs: 1_200 },
      seed: 26,
    });
    await awaitFresh(page, [1]);
    await quiesce(page);
    const rail = await openTerminal(page);
    await expect(rail).toBeVisible();
    const asked = await refreshes(page);
    await fixture(page, 'hide');
    await fixture(page, 'show');
    // The window narrows before the fresh snapshot arrives and paints.
    await page.setViewportSize({ width: 412, height: 839 });
    await expect(rail).toBeHidden();
    expect(await railListeners(page)).toBe(1);
    await expect.poll(() => refreshes(page)).toBeGreaterThan(asked);
    await page.waitForTimeout(1_700);
    const pending = await exportSummary(await openResumeTiming(page));
    expect(group(pending.summary, 'wss/cloudflare', 'warm')).toMatchObject({ samples: 1, in_progress: 1, fresh_on_time: 0 });

    await backToAgents(page);
    expect(await railListeners(page)).toBe(0);
    await page.waitForTimeout(250);
    const card = await openResumeTiming(page);
    const shown = await exportSummary(card);
    expect(group(shown.summary, 'wss/cloudflare', 'warm')).toMatchObject({ samples: 1, in_progress: 0, fresh_on_time: 1 });
    await expectNoSensitiveMarkers(booted, card, shown.text);
  });
});

test('stops and clears measurement when the user opts out', async ({ page }) => {
  await boot(page, { relays: [fixtureRelay(1, 'wss', { ingress: 'cloudflare' })], seed: 17 });
  await awaitFresh(page, [1]);
  const card = await openResumeTiming(page);
  await expect(card.getByRole('rowheader', { name: 'Cloudflare WSS' })).toHaveCount(1);
  const toggle = card.getByRole('switch', { name: 'Measure Resume Timing' });
  await expect(toggle).toBeChecked();
  await toggle.uncheck();
  await expect(card.getByRole('status').filter({ hasText: 'Resume timing is off, and recorded timings were cleared.' })).toBeVisible();
  await expect(card.getByRole('rowheader')).toHaveCount(0);
  await expect(card.getByRole('button', { name: 'Export Redacted Summary' })).toBeDisabled();
  expect(await page.evaluate(() => localStorage.getItem('herdr_resume_metrics'))).toBe('off');
  await toggle.check();
  await expect(card.getByRole('status').filter({ hasText: 'Resume timing is on.' })).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem('herdr_resume_metrics'))).toBeNull();
});
