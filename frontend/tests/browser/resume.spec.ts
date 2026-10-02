import { expect, test, type Locator, type Page } from '@playwright/test';
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
  verdict: string;
}

interface ExportedGroup {
  path: string;
  lifecycle: string;
  samples: number;
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
