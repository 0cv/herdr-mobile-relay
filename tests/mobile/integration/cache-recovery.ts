import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { chromium, type Browser } from '../../../frontend/node_modules/playwright';
import { type BundleSet } from '../support/artifacts';
import { command, startCommand, stopProcess } from '../support/process';
import { repositoryPath, repositoryRoot } from '../support/paths';

interface FixtureInfo {
  app_url: string;
  setup_urls: string[];
  control_url: string;
  control_secret: string;
}

function option(name: string): string | undefined {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

function required(name: string): string {
  const value = option(name);
  if (!value) throw new Error(`missing ${name}`);
  return value;
}

async function readJson<T>(filename: string): Promise<T> {
  return JSON.parse(await readFile(filename, 'utf8')) as T;
}

async function waitForInfo(filename: string): Promise<FixtureInfo> {
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    try {
      return await readJson<FixtureInfo>(filename);
    } catch {
      await new Promise((resolvePromise) => setTimeout(resolvePromise, 100));
    }
  }
  throw new Error('CACHE_RECOVERY: fixture info was not published');
}

async function control(info: FixtureInfo, path: string, method = 'GET', body?: unknown): Promise<any> {
  const response = await fetch(`${info.control_url}${path}`, {
    method,
    headers: {
      'X-Herdr-Fixture-Secret': info.control_secret,
      ...(body === undefined ? {} : { 'content-type': 'application/json' }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(10_000),
  });
  const text = await response.text();
  if (!response.ok) throw new Error(`CACHE_RECOVERY: fixture control ${response.status}: ${text.slice(0, 300)}`);
  return text ? JSON.parse(text) : null;
}

async function waitForFault(info: FixtureInfo, path: string): Promise<void> {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    const state = await control(info, '/state');
    if (state.requests.some((request: { path: string; fault?: string }) => request.path === path && request.fault === 'missing')) return;
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 100));
  }
  throw new Error(`CACHE_RECOVERY: missing stylesheet fault was not consumed for ${path}`);
}

async function main(): Promise<void> {
  const bundleSetFile = repositoryPath(required('--bundle-set'));
  const bundleSetDirectory = resolve(bundleSetFile, '..');
  const source = await readJson<BundleSet>(bundleSetFile);
  const bundleSet: BundleSet = {
    ...source,
    candidate: { ...source.candidate, root: resolve(bundleSetDirectory, source.candidate.root) },
    baselines: source.baselines.map((bundle) => ({ ...bundle, root: resolve(bundleSetDirectory, bundle.root) })),
  };
  const baseline = bundleSet.baselines[0];
  if (!baseline) throw new Error('CACHE_RECOVERY: bundle set has no baseline');
  if (baseline.identity.style === bundleSet.candidate.identity.style
    || baseline.identity.styleSha256 === bundleSet.candidate.identity.styleSha256) {
    throw new Error('CACHE_RECOVERY: baseline and candidate stylesheet identities must differ');
  }
  const privateDir = await mkdtemp(join(tmpdir(), 'herdr-mobile-cache-recovery-'));
  const infoFile = join(privateDir, 'fixture-info.json');
  const suppliedBinary = option('--fixture');
  const binary = suppliedBinary || join(privateDir, 'fixture-bin');
  if (!suppliedBinary) {
    try {
      await command('go', ['build', '-o', binary, './tests/mobile/fixture'], 300_000, { cwd: repositoryRoot });
    } catch (error) {
      await rm(privateDir, { recursive: true, force: true });
      throw error;
    }
  }
  const args = ['-old-root', baseline.root, '-candidate-root', bundleSet.candidate.root, '-run-dir', join(privateDir, 'fixture'), '-info-file', infoFile];
  const fixture = startCommand(binary, args, undefined, {
    cwd: repositoryRoot,
    env: {
      PATH: process.env.PATH || '',
      HOME: join(privateDir, 'home'),
      XDG_CONFIG_HOME: join(privateDir, 'config'),
      XDG_CACHE_HOME: join(privateDir, 'cache'),
      XDG_DATA_HOME: join(privateDir, 'data'),
      LANG: 'C',
      LC_ALL: 'C',
    },
  });
  let info: FixtureInfo | undefined;
  let browser: Browser | undefined;
  try {
    info = await waitForInfo(infoFile);
    browser = await chromium.launch({ headless: true });
    const context = await browser.newContext({ ignoreHTTPSErrors: true });
    const page = await context.newPage();
    await page.goto(info.app_url, { waitUntil: 'networkidle' });
    await page.waitForFunction(() => Boolean(document.querySelector('#app')?.childNodes.length));
    await page.goto(info.setup_urls[0], { waitUntil: 'domcontentloaded' });
    await page.locator('button.agent-open[aria-label="Open mobile-ci on alpha"]').waitFor({ timeout: 30_000 });
    await page.evaluate(({ version, assets, build }) => {
      sessionStorage.setItem('herdr_update_progress', JSON.stringify({
        targetVersion: version,
        relayIds: [],
        startedRelayIds: [],
        relayStartedAt: {},
        appRelayId: '',
        phoneAppRequired: true,
        phoneTarget: { version, assets, build },
        phoneState: 'loading',
        phoneAcknowledged: false,
        phoneReloadAttempts: 0,
        phoneError: '',
        errors: {},
        startedAt: Date.now(),
      }));
    }, {
      version: bundleSet.candidate.identity.version,
      assets: bundleSet.candidate.identity.assets,
      build: bundleSet.candidate.identity.build,
    });
    await control(info, '/activate', 'POST', { release: 'candidate' });
    await control(info, '/fault', 'POST', {
      method: 'GET', path: bundleSet.candidate.identity.style, kind: 'missing', remaining: 2,
    });
    await page.goto(`${info.app_url}/index.html?herdr_reload=cache-recovery`, { waitUntil: 'domcontentloaded' });
    await page.getByRole('heading', { name: 'Herdr could not load' }).waitFor();
    await waitForFault(info, bundleSet.candidate.identity.style);
    await page.waitForFunction(() => JSON.parse(sessionStorage.getItem('herdr_update_progress') || '{}').phoneState === 'failed');
    const failedPlan = await page.evaluate(() => JSON.parse(sessionStorage.getItem('herdr_update_progress') || '{}')) as { phoneAcknowledged?: boolean; phoneState?: string };
    if (failedPlan.phoneAcknowledged === true || failedPlan.phoneState !== 'failed') {
      throw new Error(`CACHE_RECOVERY: failed plan was not incomplete: ${JSON.stringify(failedPlan)}`);
    }
    await page.getByRole('button', { name: 'Try again' }).click();
    await page.waitForFunction(() => {
      const plan = JSON.parse(sessionStorage.getItem('herdr_update_progress') || '{}');
      return plan.phoneAcknowledged === true
        && plan.phoneState === 'loaded'
        && document.documentElement.dataset.herdrCssReady === '1'
        && Boolean(document.querySelector('#app')?.childNodes.length);
    }, undefined, { timeout: 30_000 });
    const targetRuntime = await page.evaluate(() => ({
      pathname: location.pathname,
      script: document.querySelector<HTMLScriptElement>('script[type="module"][src*="/assets/app"]')?.src || '',
      style: document.querySelector<HTMLLinkElement>('link[rel="stylesheet"][href*="/assets/app"]')?.href || '',
      build: document.querySelector('[data-app-build]')?.getAttribute('data-app-build') || '',
    }));
    if (!targetRuntime.script.endsWith(bundleSet.candidate.identity.script)
      || !targetRuntime.style.endsWith(bundleSet.candidate.identity.style)
      || targetRuntime.build !== bundleSet.candidate.identity.build) {
      throw new Error(`CACHE_RECOVERY: target runtime identity mismatch: ${JSON.stringify(targetRuntime)}`);
    }
    const state = await control(info, '/state');
    const fault = state.requests.find((request: { path: string; fault?: string }) => request.path === bundleSet.candidate.identity.style && request.fault === 'missing');
    if (!fault) throw new Error('CACHE_RECOVERY: consumed stylesheet fault was not recorded');
    const updateDialog = page.locator('#update-progress-dialog');
    await updateDialog.getByRole('button', { name: 'Close', exact: true }).click();
    await updateDialog.waitFor({ state: 'hidden' });
    await page.locator('button[aria-label^="Settings"]').click();
    await page.locator('#settings-title').waitFor();
    await page.getByRole('button', { name: 'Back', exact: true }).click();
    await page.getByRole('main', { name: 'Agents' }).waitFor();
    await page.getByRole('button', { name: 'Open mobile-ci on alpha', exact: true }).click();
    await page.getByRole('combobox', { name: 'Prompt', exact: true }).waitFor();
    await context.close();
  } finally {
    await browser?.close();
    if (info) await control(info, '/shutdown', 'POST').catch(() => undefined);
    await stopProcess(fixture);
    await rm(privateDir, { recursive: true, force: true });
  }
}

main().catch((error: unknown) => {
  process.stderr.write(`${error instanceof Error ? error.stack || error.message : String(error)}\n`);
  process.exitCode = 1;
});
