import { createHash } from 'node:crypto';
import { mkdir, readFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { chromium, devices, webkit, type BrowserContext, type Page } from '../../../frontend/node_modules/playwright';
import { integrityFor, type BundleIdentity, type BundleSet } from '../support/artifacts';
import { writeSanitizedJson } from '../support/diagnostics';
import { startCommand, stopProcess } from '../support/process';
import { repositoryPath, repositoryRoot } from '../support/paths';

interface FixtureInfo {
  app_url: string;
  control_url: string;
  control_secret: string;
}

interface FixtureRequest {
  sequence: number;
  method: string;
  path: string;
  release: string;
  fault?: string;
  fault_id?: string;
  fault_generation?: string;
  status: number;
  content_type: string;
  cache_control: string;
  bytes: number;
  sha256: string;
}

interface FaultAction {
  action: string;
  id: string;
  generation: string;
  path: string;
  kind?: string;
  policy?: string;
  remaining: number;
  request_count: number;
}

interface FixtureState {
  invalidated: boolean;
  invalidation_reason: string;
  requests: FixtureRequest[];
  request_counts: Record<string, number>;
  fault_actions: FaultAction[];
  faults: Array<{ id: string; generation: string; path: string; kind: string; remaining: number }>;
}

interface ResourceObservation {
  path: string;
  sameOrigin: boolean;
  canonicalUrl: boolean;
  status: number;
  contentType: string;
  cacheControl: string;
  bytes?: number;
  sha256?: string;
  resourceType: string;
  fromServiceWorker: boolean;
}

interface Session {
  context: BrowserContext;
  appOrigin: string;
  page: Page;
  profile: string;
  resources: ResourceObservation[];
  pendingResources: Promise<void>[];
  consoleKinds: string[];
  navigationCount: number;
}

interface Options {
  bundleSet: BundleSet;
  info: FixtureInfo;
  workDir: string;
  resultDir: string;
  fixtureBinary: string;
}

const MAX_RESOURCE_BYTES = 8 * 1024 * 1024;
const POISON_HTML = '<!doctype html><script src="/herdr-bootstrap.js"></script>\n';
const POISON_JS = "export const fixturePoison = 'not the canonical module';";
const EXPECTED_CSP = "default-src 'self'; connect-src 'self' https: wss:; img-src 'self' blob: data:; media-src blob:; style-src 'self'; style-src-attr 'unsafe-inline'; script-src 'self'; worker-src 'self'; manifest-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'";
const engines = [
  { name: 'chromium' as const, browser: chromium, device: devices['Pixel 7'] },
  { name: 'webkit' as const, browser: webkit, device: devices['iPhone 15'] },
];

function option(name: string): string | undefined {
  const index = process.argv.indexOf(name);
  return index >= 0 ? process.argv[index + 1] : undefined;
}

function required(name: string): string {
  const value = option(name);
  if (!value) throw new Error(`PHONE_RECOVERY: missing ${name}`);
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
  throw new Error('PHONE_RECOVERY: fixture info was not published');
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
  if (!response.ok) throw new Error(`PHONE_RECOVERY: fixture control returned HTTP ${response.status}`);
  return text ? JSON.parse(text) : null;
}

function ensure(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(`PHONE_RECOVERY: ${message}`);
}

function appOrigin(info: FixtureInfo): string {
  return new URL(info.app_url).origin;
}

function canonicalResourceUrl(origin: string, path: string): string {
  const url = new URL(path, origin);
  ensure(url.origin === origin && url.pathname === path && !url.search && !url.hash,
    'resource descriptor did not contain a canonical same-origin path');
  return url.href;
}

function pathCount(state: FixtureState, path: string): number {
  return state.request_counts[path] || 0;
}

function sha256(value: Uint8Array | string): string {
  return createHash('sha256').update(value).digest('hex');
}

function observationFor(session: Session, path: string, from = 0): ResourceObservation | undefined {
  return [...session.resources.slice(from)].reverse().find((resource) => resource.path === path);
}

function matchingRequest(state: FixtureState, path: string, id: string, generation: string): FixtureRequest | undefined {
  return [...state.requests].reverse().find((request) => request.path === path
    && request.fault_id === id && request.fault_generation === generation);
}

async function waitForFault(
  info: FixtureInfo,
  path: string,
  kind: string,
  id: string,
  generation: string,
): Promise<{ state: FixtureState; request: FixtureRequest }> {
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    const state = await control(info, '/state') as FixtureState;
    ensure(!state.invalidated, `fixture invalidated: ${state.invalidation_reason || 'unknown reason'}`);
    const request = matchingRequest(state, path, id, generation);
    if (request?.fault === kind && request.status > 0) return { state, request };
    await new Promise((resolvePromise) => setTimeout(resolvePromise, 100));
  }
  throw new Error(`PHONE_RECOVERY: intended ${kind} response was not consumed at ${path}`);
}

async function openSession(engine: typeof engines[number], profile: string, appOrigin: string): Promise<Session> {
  await mkdir(resolve(profile, '..'), { recursive: true, mode: 0o700 });
  const context = await engine.browser.launchPersistentContext(profile, {
    ...engine.device,
    headless: true,
    ignoreHTTPSErrors: true,
    serviceWorkers: 'block',
  });
  const session: Session = {
    context,
    appOrigin,
    page: await context.newPage(),
    profile,
    resources: [],
    pendingResources: [],
    consoleKinds: [],
    navigationCount: 0,
  };
  attachPageEvidence(session);
  return session;
}

function attachPageEvidence(session: Session): void {
  session.page.on('response', (response) => {
    let resourceUrl: URL;
    try {
      resourceUrl = new URL(response.url());
    } catch {
      return;
    }
    const pathname = resourceUrl.pathname;
    const sameOrigin = resourceUrl.origin === session.appOrigin;
    const canonicalUrl = sameOrigin && !resourceUrl.search && !resourceUrl.hash;
    if (!pathname.startsWith('/assets/') && !pathname.startsWith('/builds/')) return;
    const pending = (async () => {
      const headers = response.headers();
      const observation: ResourceObservation = {
        path: pathname,
        sameOrigin,
        canonicalUrl,
        status: response.status(),
        contentType: headers['content-type'] || '',
        cacheControl: headers['cache-control'] || '',
        resourceType: response.request().resourceType(),
        fromServiceWorker: await response.fromServiceWorker(),
      };
      const length = Number(headers['content-length'] || 0);
      if (length > MAX_RESOURCE_BYTES) {
        session.resources.push(observation);
        return;
      }
      try {
        const body = await response.body();
        if (body.byteLength <= MAX_RESOURCE_BYTES) {
          observation.bytes = body.byteLength;
          observation.sha256 = sha256(body);
        }
      } catch {
        observation.bytes = undefined;
      }
      session.resources.push(observation);
    })();
    session.pendingResources.push(pending);
  });
  session.page.on('console', (message) => {
    const text = message.text().toLowerCase();
    const kind = text.includes('integrity') || text.includes('digest') ? 'integrity'
      : text.includes('mime') || text.includes('javascript') ? 'mime'
        : text.includes('failed to load') || text.includes('network') ? 'network' : 'other';
    if (!session.consoleKinds.includes(kind)) session.consoleKinds.push(kind);
  });
  session.page.on('framenavigated', (frame) => {
    if (frame === session.page.mainFrame()) session.navigationCount++;
  });
}

async function flushResources(session: Session): Promise<void> {
  const pending = session.pendingResources.splice(0);
  await Promise.allSettled(pending);
}

async function restartSession(engine: typeof engines[number], session: Session): Promise<Session> {
  await session.context.close();
  return openSession(engine, session.profile, session.appOrigin);
}

async function closeSession(session: Session | undefined): Promise<void> {
  await session?.context.close().catch(() => undefined);
}

async function runtimeDetails(page: Page, identity: BundleIdentity, expectedOrigin: string): Promise<{
  build: string;
  appMounted: boolean;
  documentOriginMatches: boolean;
  scriptCanonical: boolean;
  scriptPath: string;
  scriptIntegrity: string;
  styleCanonical: boolean;
  stylePath: string;
  cssAccepted: boolean;
  recoveryVisible: boolean;
}> {
  return page.evaluate(({ scriptPath, stylePath, origin }) => {
    const isCanonical = (value: string | null | undefined, path: string): boolean => {
      if (!value) return false;
      try {
        const url = new URL(value, location.href);
        return url.origin === origin && url.pathname === path && !url.search && !url.hash;
      } catch {
        return false;
      }
    };
    const script = [...document.querySelectorAll<HTMLScriptElement>('script[type="module"][src]')]
      .find((candidate) => isCanonical(candidate.src, scriptPath));
    const style = [...document.querySelectorAll<HTMLLinkElement>('link[rel="stylesheet"][href]')]
      .find((candidate) => isCanonical(candidate.href, stylePath));
    return {
      build: document.querySelector('[data-app-build]')?.getAttribute('data-app-build') || '',
      appMounted: Boolean(document.querySelector('#app')?.childNodes.length),
      documentOriginMatches: location.origin === origin,
      scriptCanonical: Boolean(script),
      scriptPath: script ? new URL(script.src, location.href).pathname : '',
      scriptIntegrity: script?.integrity || '',
      styleCanonical: Boolean(style),
      stylePath: style ? new URL(style.href, location.href).pathname : '',
      cssAccepted: document.documentElement.dataset.herdrCssReady === '1',
      recoveryVisible: Boolean(document.querySelector('#herdr-load-recovery')),
    };
  }, { scriptPath: identity.script, stylePath: identity.style, origin: expectedOrigin });
}

async function waitForApp(page: Page, identity: BundleIdentity, expectedOrigin: string): Promise<void> {
  await page.waitForFunction(({ build, script, style, origin }) => {
    const isCanonical = (value: string | null | undefined, path: string): boolean => {
      if (!value) return false;
      try {
        const url = new URL(value, location.href);
        return url.origin === origin && url.pathname === path && !url.search && !url.hash;
      } catch {
        return false;
      }
    };
    const app = document.querySelector('#app');
    const scriptElement = [...document.querySelectorAll<HTMLScriptElement>('script[type="module"][src]')]
      .find((candidate) => isCanonical(candidate.src, script));
    const styleElement = [...document.querySelectorAll<HTMLLinkElement>('link[rel="stylesheet"][href]')]
      .find((candidate) => isCanonical(candidate.href, style));
    return Boolean(location.origin === origin
      && app?.childNodes.length
      && document.querySelector('[data-app-build]')?.getAttribute('data-app-build') === build
      && scriptElement?.integrity
      && styleElement
      && document.documentElement.dataset.herdrCssReady === '1');
  }, { build: identity.build, script: identity.script, style: identity.style, origin: expectedOrigin }, { timeout: 30_000 });
}

async function waitForFailure(page: Page): Promise<void> {
  await page.getByRole('heading', { name: 'Herdr could not load' }).waitFor({ state: 'visible', timeout: 20_000 });
}

async function navigateCandidate(session: Session, info: FixtureInfo): Promise<{ status: number; contentType: string; cspIntact: boolean }> {
  const response = await session.page.goto(`${info.app_url}/index.html?herdr_reload=phone-recovery`, {
    waitUntil: 'domcontentloaded', timeout: 30_000,
  });
  const headers = response?.headers() || {};
  const policy = headers['content-security-policy'] || '';
  await flushResources(session);
  return {
    status: response?.status() || 0,
    contentType: headers['content-type'] || '',
    cspIntact: policy === EXPECTED_CSP,
  };
}

async function resourceTiming(page: Page, paths: string[], expectedOrigin: string): Promise<Array<{
  path: string;
  transferSize: number;
  encodedBodySize: number;
  decodedBodySize: number;
  duration: number;
}>> {
  return page.evaluate(({ targetPaths, origin }) => performance.getEntriesByType('resource')
    .flatMap((entry) => {
      try {
        const url = new URL(entry.name, location.href);
        if (url.origin !== origin || url.search || url.hash || !targetPaths.includes(url.pathname)) return [];
        const resource = entry as PerformanceResourceTiming;
        return [{
          path: url.pathname,
          transferSize: resource.transferSize,
          encodedBodySize: resource.encodedBodySize,
          decodedBodySize: resource.decodedBodySize,
          duration: Math.round(resource.duration),
        }];
      } catch {
        return [];
      }
    }), { targetPaths: paths, origin: expectedOrigin });
}

function requirePoisonResponse(
  id: string,
  generation: string,
  kind: 'immutable-html' | 'immutable-corrupt',
  path: string,
  request: FixtureRequest,
  browserResponse: ResourceObservation | undefined,
  identity: BundleIdentity,
): void {
  ensure(request.fault === kind && request.fault_id === id && request.fault_generation === generation,
    `${kind} consumption was not identified by fault ID and generation`);
  ensure(request.status === 200 && request.cache_control === 'public, max-age=31536000, immutable',
    `${kind} response did not use the intended HTTP 200 immutable policy`);
  ensure(request.sha256 && request.bytes > 0, `${kind} response has no bounded digest accounting`);
  ensure(browserResponse?.path === path && browserResponse.sameOrigin && browserResponse.canonicalUrl
    && browserResponse.status === 200,
  `${kind} response was not observed at its same-origin canonical URL by the browser`);
  ensure(browserResponse.sha256 === request.sha256,
    `${kind} server and browser response digests differ or browser bytes were unavailable`);
  ensure(browserResponse.cacheControl === request.cache_control,
    `${kind} browser response did not expose the immutable cache policy`);
  if (kind === 'immutable-html') {
    ensure(request.content_type.startsWith('text/html') && browserResponse.contentType.startsWith('text/html'),
      'immutable HTML poison did not have HTML MIME');
    ensure(request.sha256 === sha256(POISON_HTML) && request.bytes === Buffer.byteLength(POISON_HTML),
      'immutable HTML response differed from the recorded bootstrap bytes');
  } else {
    ensure(request.content_type.startsWith('text/javascript') && browserResponse.contentType.startsWith('text/javascript'),
      'corrupt JavaScript poison did not retain the JavaScript MIME');
    ensure(request.sha256 === sha256(POISON_JS) && request.sha256 !== identity.scriptSha256,
      'corrupt JavaScript response bytes were not wrong bytes');
  }
}

async function runCanonicalAndUnchangedCss(options: Options, engine: typeof engines[number]): Promise<Record<string, unknown>> {
  const baseline = options.bundleSet.baselines[0]!;
  const candidate = options.bundleSet.candidate;
  ensure(baseline.identity.style === candidate.identity.style
    && baseline.identity.styleSha256 === candidate.identity.styleSha256,
  'the phone-recovery fixture must use an explicitly unchanged stylesheet pair');
  const profile = join(options.workDir, 'profiles', `${engine.name}-canonical-unchanged-css`);
  let session = await openSession(engine, profile, appOrigin(options.info));
  let phase = 'baseline-navigation';
  try {
    await control(options.info, '/activate', 'POST', { release: 'old' });
    const firstDocument = await session.page.goto(options.info.app_url, { waitUntil: 'domcontentloaded', timeout: 30_000 });
    await waitForApp(session.page, baseline.identity, session.appOrigin);
    const baselineRuntime = await runtimeDetails(session.page, baseline.identity, session.appOrigin);
    phase = 'candidate-cutover';
    const styleCountBefore = pathCount(await control(options.info, '/state'), baseline.identity.style);
    const cleanCountsBefore = await control(options.info, '/state') as FixtureState;
    await control(options.info, '/activate', 'POST', { release: 'candidate' });
    const candidateDocument = await session.page.goto(`${options.info.app_url}/index.html`, { waitUntil: 'domcontentloaded', timeout: 30_000 });
    await waitForApp(session.page, candidate.identity, session.appOrigin);
    await flushResources(session);
    const candidateRuntime = await runtimeDetails(session.page, candidate.identity, session.appOrigin);
    const stateAfterCutover = await control(options.info, '/state') as FixtureState;
    const styleCountAfter = pathCount(stateAfterCutover, candidate.identity.style);
    const styleBrowserResponse = observationFor(session, candidate.identity.style);
    ensure(firstDocument?.status() === 200 || firstDocument?.status() === 307,
      'clean baseline startup did not return a successful document response');
    ensure(candidateDocument?.status() === 200 || candidateDocument?.status() === 307,
      'candidate startup did not return a successful document response');
    ensure(candidateRuntime.build === candidate.identity.build && candidateRuntime.appMounted && candidateRuntime.cssAccepted,
      'clean canonical candidate startup did not accept its module and stylesheet');
    ensure(candidateRuntime.scriptPath === candidate.identity.script && candidateRuntime.stylePath === candidate.identity.style,
      'canonical startup selected paths outside the emitted candidate descriptor');
    ensure(candidateRuntime.scriptIntegrity === integrityFor(candidate.identity.scriptSha256),
      'candidate module lost its descriptor-derived SRI value');
    ensure(styleBrowserResponse?.sha256 === candidate.identity.styleSha256,
      'the browser did not observe the unchanged candidate stylesheet bytes');
    phase = 'reusable-profile-restart';
    session = await restartSession(engine, session);
    const restartDocument = await session.page.goto(options.info.app_url, { waitUntil: 'domcontentloaded', timeout: 30_000 });
    await waitForApp(session.page, candidate.identity, session.appOrigin);
    await flushResources(session);
    const restartRuntime = await runtimeDetails(session.page, candidate.identity, session.appOrigin);
    ensure(restartRuntime.build === candidate.identity.build && restartRuntime.cssAccepted,
      'reusable profile restart did not load the candidate build and stylesheet');
    const stateAfterRestart = await control(options.info, '/state') as FixtureState;
    return {
      scenario: 'clean-canonical-startup-and-unchanged-css',
      engine: engine.name,
      cleanStartup: { status: firstDocument?.status() || 0, build: baselineRuntime.build, cssAccepted: baselineRuntime.cssAccepted },
      cutover: { status: candidateDocument?.status() || 0, build: candidateRuntime.build, scriptPath: candidateRuntime.scriptPath, stylePath: candidateRuntime.stylePath, cssAccepted: candidateRuntime.cssAccepted },
      stylesheet: {
        path: candidate.identity.style,
        sha256: candidate.identity.styleSha256,
        baselineCount: styleCountBefore,
        beforeCutoverCount: pathCount(cleanCountsBefore, candidate.identity.style),
        afterCutoverCount: styleCountAfter,
        response: styleBrowserResponse,
      },
      restart: { status: restartDocument?.status() || 0, build: restartRuntime.build, cssAccepted: restartRuntime.cssAccepted },
      serverRequests: stateAfterRestart.request_counts,
      browserResourceTiming: await resourceTiming(session.page, [candidate.identity.script, candidate.identity.style], session.appOrigin),
    };
  } catch (error) {
    await flushResources(session);
    const [state, runtime] = await Promise.all([
      control(options.info, '/state').catch(() => undefined) as Promise<FixtureState | undefined>,
      runtimeDetails(session.page, baseline.identity, session.appOrigin).catch(() => undefined),
    ]);
    const details = {
      phase,
      runtime,
      requestCounts: state?.request_counts,
      recentRequests: state?.requests.slice(-12).map((request) => ({
        path: request.path, release: request.release, status: request.status, contentType: request.content_type,
      })),
      browserResources: session.resources.slice(-12),
      browserConsoleKinds: session.consoleKinds,
    };
    throw new Error(`CANONICAL_SCENARIO: ${error instanceof Error ? error.message : String(error)} ${JSON.stringify(details)}`, { cause: error });
  } finally {
    await closeSession(session);
  }
}

async function runImmutableDocumentRetry(
  options: Options,
  engine: typeof engines[number],
  kind: 'immutable-html' | 'immutable-corrupt',
): Promise<Record<string, unknown>> {
  const baseline = options.bundleSet.baselines[0]!;
  const candidate = options.bundleSet.candidate;
  const path = candidate.identity.script;
  const id = `${engine.name}-${kind}`;
  const generation = `${id}-generation-1`;
  const profile = join(options.workDir, 'profiles', `${engine.name}-${kind}-retry`);
  let session = await openSession(engine, profile, appOrigin(options.info));
  try {
    await control(options.info, '/activate', 'POST', { release: 'old' });
    const beforeBaseline = await control(options.info, '/state') as FixtureState;
    await session.page.goto(options.info.app_url, { waitUntil: 'domcontentloaded', timeout: 30_000 });
    await waitForApp(session.page, baseline.identity, session.appOrigin);
    const beforePoison = await control(options.info, '/state') as FixtureState;
    ensure(pathCount(beforePoison, path) === pathCount(beforeBaseline, path), 'candidate module was requested before the poison was armed');
    const poisonEventOffset = session.resources.length;
    await control(options.info, '/fault', 'POST', {
      id, generation, method: 'GET', path, kind, remaining: -1, lifetime_ms: 120_000,
    });
    await control(options.info, '/activate', 'POST', { release: 'candidate' });
    const documentResult = await navigateCandidate(session, options.info);
    await waitForFailure(session.page);
    const { state: consumedState, request } = await waitForFault(options.info, path, kind, id, generation);
    await flushResources(session);
    const poisonResponse = observationFor(session, path, poisonEventOffset);
    requirePoisonResponse(id, generation, kind, path, request, poisonResponse, candidate.identity);
    const failedRuntime = await runtimeDetails(session.page, candidate.identity, session.appOrigin);
    ensure(failedRuntime.scriptPath === candidate.identity.script,
      'the failure did not arise from the canonical descriptor-selected script');
    ensure(failedRuntime.scriptIntegrity === integrityFor(candidate.identity.scriptSha256),
      'script integrity metadata was not intact when poisoning was consumed');
    ensure(!failedRuntime.appMounted && failedRuntime.recoveryVisible,
      'wrong resource bytes were accepted instead of showing the load failure');
    ensure(documentResult.cspIntact, 'the fixture document did not retain its same-origin CSP policy');
    if (kind === 'immutable-corrupt') {
      ensure(session.consoleKinds.includes('integrity'), 'the browser did not report the corrupt bytes as an integrity rejection');
    }
    const countBeforeRetry = pathCount(consumedState, path);
    await control(options.info, '/fault/clear', 'POST', { id, generation });
    const clearedState = await control(options.info, '/state') as FixtureState;
    ensure(clearedState.fault_actions.some((action) => action.action === 'fault_cleared'
      && action.id === id && action.generation === generation), 'explicit server repair was not accounted separately from exhaustion');
    const retryEventOffset = session.resources.length;
    const navigation = session.page.waitForNavigation({ waitUntil: 'domcontentloaded', timeout: 20_000 }).catch(() => null);
    await session.page.getByRole('button', { name: 'Try again' }).click({ timeout: 10_000 });
    await navigation;
    await waitForFailure(session.page);
    await flushResources(session);
    const stateAfterRetry = await control(options.info, '/state') as FixtureState;
    const countAfterRetry = pathCount(stateAfterRetry, path);
    const retryRuntime = await runtimeDetails(session.page, candidate.identity, session.appOrigin);
    const retryResponse = observationFor(session, path, retryEventOffset);
    const retryTiming = await resourceTiming(session.page, [path], session.appOrigin);
    const integrityRejectionObserved = session.consoleKinds.includes('integrity');
    let retryAssessment = 'inconclusive';
    if (countAfterRetry > countBeforeRetry && retryRuntime.build === candidate.identity.build && retryRuntime.appMounted) {
      retryAssessment = 'server-refetch-recovered';
    } else if (countAfterRetry === countBeforeRetry && retryResponse?.sha256 === request.sha256 && !retryRuntime.appMounted) {
      retryAssessment = 'browser-reused-poison';
    } else if (countAfterRetry === countBeforeRetry && retryTiming.some((entry) => entry.path === path && entry.transferSize === 0)) {
      retryAssessment = 'browser-cache-source-observed-but-payload-uncertain';
    } else if (countAfterRetry > countBeforeRetry) {
      retryAssessment = 'server-refetch-did-not-recover';
    }
    session = await restartSession(engine, session);
    const restartBefore = await control(options.info, '/state') as FixtureState;
    const restartCountBefore = pathCount(restartBefore, path);
    const restartDocument = await session.page.goto(options.info.app_url, { waitUntil: 'domcontentloaded', timeout: 30_000 }).catch(() => null);
    await session.page.waitForFunction(() => Boolean(document.querySelector('#app')?.childNodes.length
      || document.querySelector('#herdr-load-recovery')), undefined, { timeout: 20_000 }).catch(() => undefined);
    await flushResources(session);
    const restartRuntime = await runtimeDetails(session.page, candidate.identity, session.appOrigin);
    const restartAfter = await control(options.info, '/state') as FixtureState;
    return {
      scenario: `${kind}-document-only-retry`,
      engine: engine.name,
      resourcePath: path,
      fault: { id, generation, kind, consumed: true, requestCountBefore: pathCount(beforePoison, path), requestCountAfterPoison: countBeforeRetry, response: request },
      browserPoisonResponse: poisonResponse,
      integrityAndPolicy: {
        intactSRI: failedRuntime.scriptIntegrity === integrityFor(candidate.identity.scriptSha256),
        cspIntact: documentResult.cspIntact,
        integrityRejectionObserved,
        failedRuntime,
      },
      repairAction: clearedState.fault_actions.filter((action) => action.id === id && action.generation === generation),
      documentRetry: {
        requestCountBefore: countBeforeRetry,
        requestCountAfter: countAfterRetry,
        assessment: retryAssessment,
        runtime: retryRuntime,
        browserResponse: retryResponse,
        resourceTiming: retryTiming,
      },
      profileRestart: {
        documentStatus: restartDocument?.status() || 0,
        requestCountBefore: restartCountBefore,
        requestCountAfter: pathCount(restartAfter, path),
        runtime: restartRuntime,
        resourceTiming: await resourceTiming(session.page, [path], session.appOrigin),
      },
      limitation: retryAssessment === 'inconclusive'
        ? 'The browser did not expose enough payload/cache-source evidence to infer reuse from origin counts.'
        : undefined,
    };
  } finally {
    await closeSession(session);
  }
}

async function fetchCanonicalResource(
  page: Page,
  path: string,
  cache: 'default' | 'reload',
  expectedOrigin: string,
  expectedSha256: string,
): Promise<{
  status: number;
  contentType: string;
  cacheControl: string;
  bytes: number;
  sha256: string;
  sameOrigin: boolean;
  exactCanonicalUrl: boolean;
  redirected: boolean;
  matchesDescriptor: boolean;
  oversized: boolean;
  timedOut: boolean;
}> {
  const expectedUrl = canonicalResourceUrl(expectedOrigin, path);
  return page.evaluate(async ({ resourceUrl, origin, cacheMode, maxBytes, timeoutMs, expectedDigest }) => {
    const controller = new AbortController();
    let response: Response | undefined;
    let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
    let timedOut = false;
    let timer = 0;
    const timeout = new Promise<never>((_resolve, reject) => {
      timer = window.setTimeout(() => {
        timedOut = true;
        controller.abort();
        reject(new Error('bounded resource probe timed out'));
      }, timeoutMs);
    });
    const probe = async () => {
      response = await fetch(resourceUrl, {
        cache: cacheMode,
        credentials: 'same-origin',
        redirect: 'follow',
        signal: controller.signal,
      });
      const contentType = response.headers.get('content-type') || '';
      const cacheControl = response.headers.get('cache-control') || '';
      const finalUrl = new URL(response.url);
      const sameOrigin = finalUrl.origin === origin;
      const exactCanonicalUrl = sameOrigin && response.url === resourceUrl
        && !finalUrl.search && !finalUrl.hash;
      const base = {
        status: response.status,
        contentType,
        cacheControl,
        sameOrigin,
        exactCanonicalUrl,
        redirected: response.redirected,
      };
      if (!exactCanonicalUrl) {
        await response.body?.cancel().catch(() => undefined);
        return { ...base, bytes: 0, sha256: '', matchesDescriptor: false, oversized: false, timedOut: false };
      }
      const contentLength = Number(response.headers.get('content-length') || 0);
      if (Number.isFinite(contentLength) && contentLength > maxBytes) {
        await response.body?.cancel().catch(() => undefined);
        return { ...base, bytes: contentLength, sha256: '', matchesDescriptor: false, oversized: true, timedOut: false };
      }
      if (!response.body) {
        return { ...base, bytes: 0, sha256: '', matchesDescriptor: false, oversized: false, timedOut: false };
      }
      reader = response.body.getReader();
      const chunks: Uint8Array[] = [];
      let bytes = 0;
      while (true) {
        const chunk = await reader.read();
        if (chunk.done) break;
        bytes += chunk.value.byteLength;
        if (bytes > maxBytes) {
          await reader.cancel().catch(() => undefined);
          return { ...base, bytes, sha256: '', matchesDescriptor: false, oversized: true, timedOut: false };
        }
        chunks.push(chunk.value);
      }
      const body = new Uint8Array(bytes);
      let offset = 0;
      for (const chunk of chunks) {
        body.set(chunk, offset);
        offset += chunk.byteLength;
      }
      const digest = new Uint8Array(await crypto.subtle.digest('SHA-256', body));
      const sha256 = [...digest].map((value) => value.toString(16).padStart(2, '0')).join('');
      const matchesDescriptor = response.status === 200
        && contentType.toLowerCase().startsWith('text/javascript')
        && sha256 === expectedDigest;
      return { ...base, bytes, sha256, matchesDescriptor, oversized: false, timedOut: false };
    };
    try {
      return await Promise.race([probe(), timeout]);
    } catch {
      await reader?.cancel().catch(() => undefined);
      return {
        status: response?.status || 0,
        contentType: response?.headers.get('content-type') || '',
        cacheControl: response?.headers.get('cache-control') || '',
        bytes: 0,
        sha256: '',
        sameOrigin: response ? new URL(response.url).origin === origin : false,
        exactCanonicalUrl: response?.url === resourceUrl,
        redirected: response?.redirected || false,
        matchesDescriptor: false,
        oversized: false,
        timedOut,
      };
    } finally {
      window.clearTimeout(timer);
    }
  }, {
    resourceUrl: expectedUrl,
    origin: expectedOrigin,
    cacheMode: cache,
    maxBytes: MAX_RESOURCE_BYTES,
    timeoutMs: 10_000,
    expectedDigest: expectedSha256,
  });
}

async function canonicalNavigationObservation(
  session: Session,
  options: Options,
  identity: BundleIdentity,
): Promise<Record<string, unknown>> {
  const pathCountBefore = pathCount(await control(options.info, '/state'), identity.script);
  const responseOffset = session.resources.length;
  const response = await session.page.goto(`${options.info.app_url}/index.html?herdr_reload=phone-recovery`, {
    waitUntil: 'domcontentloaded', timeout: 30_000,
  }).catch(() => null);
  await session.page.waitForFunction(() => Boolean(document.querySelector('#app')?.childNodes.length
    || document.querySelector('#herdr-load-recovery')), undefined, { timeout: 20_000 }).catch(() => undefined);
  await flushResources(session);
  const runtime = await runtimeDetails(session.page, identity, session.appOrigin);
  const browserScript = observationFor(session, identity.script, responseOffset);
  const browserStyle = observationFor(session, identity.style, responseOffset);
  const stateAfter = await control(options.info, '/state') as FixtureState;
  const documentSameOrigin = new URL(session.page.url()).origin === session.appOrigin;
  const scriptResponseCanonical = browserScript?.sameOrigin === true && browserScript.canonicalUrl === true;
  const stylesheetResponseCanonical = browserStyle?.sameOrigin === true && browserStyle.canonicalUrl === true;
  const mountedCorrectBuild = documentSameOrigin && runtime.documentOriginMatches && runtime.scriptCanonical
    && scriptResponseCanonical && runtime.appMounted && runtime.build === identity.build && runtime.scriptPath === identity.script;
  const stylesheetAccepted = documentSameOrigin && runtime.styleCanonical && stylesheetResponseCanonical
    && runtime.cssAccepted && runtime.stylePath === identity.style;
  return {
    documentStatus: response?.status() || 0,
    documentSameOrigin,
    scriptResponseCanonical,
    stylesheetResponseCanonical,
    scriptCountBefore: pathCountBefore,
    scriptCountAfter: pathCount(stateAfter, identity.script),
    mountedCorrectBuild,
    stylesheetAccepted,
    runtime,
    browserScript,
    browserStyle,
  };
}

async function runRepairPolicyExperiment(
  options: Options,
  engine: typeof engines[number],
  policy: 'no-cache' | 'no-store',
): Promise<Record<string, unknown>> {
  const baseline = options.bundleSet.baselines[0]!;
  const candidate = options.bundleSet.candidate;
  const path = candidate.identity.script;
  const id = `${engine.name}-repair-${policy}`;
  const generation = `${id}-generation-1`;
  const profile = join(options.workDir, 'profiles', `${engine.name}-repair-${policy}`);
  let session = await openSession(engine, profile, appOrigin(options.info));
  try {
    await control(options.info, '/activate', 'POST', { release: 'old' });
    await session.page.goto(options.info.app_url, { waitUntil: 'domcontentloaded', timeout: 30_000 });
    await waitForApp(session.page, baseline.identity, session.appOrigin);
    const poisonEventOffset = session.resources.length;
    await control(options.info, '/fault', 'POST', {
      id, generation, method: 'GET', path, kind: 'immutable-corrupt', remaining: -1, lifetime_ms: 120_000,
    });
    await control(options.info, '/activate', 'POST', { release: 'candidate' });
    const documentResult = await navigateCandidate(session, options.info);
    await waitForFailure(session.page);
    const consumed = await waitForFault(options.info, path, 'immutable-corrupt', id, generation);
    await flushResources(session);
    const poisonedResponse = observationFor(session, path, poisonEventOffset);
    requirePoisonResponse(id, generation, 'immutable-corrupt', path, consumed.request, poisonedResponse, candidate.identity);
    const failureRuntime = await runtimeDetails(session.page, candidate.identity, session.appOrigin);
    ensure(!failureRuntime.appMounted && failureRuntime.recoveryVisible && documentResult.cspIntact,
      'repair experiment did not begin from an intact rejected poisoned module');
    const repairBefore = await control(options.info, '/state') as FixtureState;
    await control(options.info, '/fault/repair', 'POST', { id, generation, response_policy: policy });
    const repaired = await control(options.info, '/state') as FixtureState;
    const repairEvent = repaired.fault_actions.find((action) => action.action === 'fault_repaired'
      && action.id === id && action.generation === generation);
    ensure(repairEvent?.policy === policy, 'explicit repair and response policy were not recorded');

    const defaultBefore = await control(options.info, '/state') as FixtureState;
    const defaultFetch = await fetchCanonicalResource(
      session.page, path, 'default', session.appOrigin, candidate.identity.scriptSha256,
    );
    const defaultAfter = await control(options.info, '/state') as FixtureState;
    const defaultServerRequests = defaultAfter.requests.filter((request) => request.path === path
      && request.sequence > Math.max(...defaultBefore.requests.map((request) => request.sequence), 0));

    const reloadBefore = await control(options.info, '/state') as FixtureState;
    const reloadFetch = await fetchCanonicalResource(
      session.page, path, 'reload', session.appOrigin, candidate.identity.scriptSha256,
    );
    const reloadAfter = await control(options.info, '/state') as FixtureState;
    const reloadServerRequests = reloadAfter.requests.filter((request) => request.path === path
      && request.sequence > Math.max(...reloadBefore.requests.map((request) => request.sequence), 0));
    for (const request of [...defaultServerRequests, ...reloadServerRequests]) {
      ensure(request.cache_control === policy,
        `repaired ${policy} response lost its fixture-scoped cache policy`);
    }
    for (const [name, probe] of [['default', defaultFetch], ['reload', reloadFetch]] as const) {
      ensure(probe.sameOrigin && probe.exactCanonicalUrl && !probe.redirected
        && !probe.oversized && !probe.timedOut,
      `${name} cache probe left the same-origin canonical URL or its bounded response limits`);
    }

    const canonical = await canonicalNavigationObservation(session, options, candidate.identity);
    session = await restartSession(engine, session);
    const afterRestart = await canonicalNavigationObservation(session, options, candidate.identity);
    const effective = reloadFetch.matchesDescriptor
      && canonical.mountedCorrectBuild === true && canonical.stylesheetAccepted === true
      && afterRestart.mountedCorrectBuild === true && afterRestart.stylesheetAccepted === true;
    return {
      scenario: 'bounded-cache-repair-experiment',
      engine: engine.name,
      policy,
      resourcePath: path,
      poison: { consumed: true, requestCount: pathCount(consumed.state, path), response: consumed.request, browserResponse: poisonedResponse },
      repairAction: repairEvent,
      repairCounts: { before: pathCount(repairBefore, path), after: pathCount(repaired, path) },
      probes: {
        default: {
          order: 1,
          requestCountBefore: pathCount(defaultBefore, path),
          requestCountAfter: pathCount(defaultAfter, path),
          response: defaultFetch,
          serverResponses: defaultServerRequests,
        },
        reload: {
          order: 2,
          requestCountBefore: pathCount(reloadBefore, path),
          requestCountAfter: pathCount(reloadAfter, path),
          response: reloadFetch,
          serverResponses: reloadServerRequests,
        },
      },
      canonicalNavigation: canonical,
      orderlyProfileRestart: afterRestart,
      recoveryDesignGate: effective ? 'canonical module and stylesheet accepted before and after restart' : `TEST-ONLY fetch did not establish canonical module and stylesheet acceptance; reload matched descriptor: ${reloadFetch.matchesDescriptor}; production recovery remains unqualified`,
      repairEffective: effective,
    };
  } finally {
    await closeSession(session);
  }
}

async function runStaleEntryScenario(options: Options, engine: typeof engines[number]): Promise<Record<string, unknown>> {
  const baseline = options.bundleSet.baselines[0]!;
  const candidate = options.bundleSet.candidate;
  const profile = join(options.workDir, 'profiles', `${engine.name}-stale-entry`);
  const session = await openSession(engine, profile, appOrigin(options.info));
  try {
    await control(options.info, '/activate', 'POST', { release: 'old' });
    await session.page.goto(options.info.app_url, { waitUntil: 'domcontentloaded', timeout: 30_000 });
    await waitForApp(session.page, baseline.identity, session.appOrigin);
    const oldEntry = baseline.identity.entry;
    const oldScript = baseline.identity.script;
    const before = await control(options.info, '/state') as FixtureState;
    ensure(pathCount(before, oldScript) > 0, 'baseline entry did not consume its descriptor-derived script before cutover');
    await control(options.info, '/activate', 'POST', { release: 'candidate' });
    const staleResponse = await session.page.goto(new URL(oldEntry, options.info.app_url).href, {
      waitUntil: 'domcontentloaded', timeout: 30_000,
    });
    const staleHeaders = staleResponse?.headers() || {};
    const stalePage = await session.page.evaluate(() => ({
      title: document.title,
      heading: document.querySelector('h1')?.textContent || '',
      hasApp: Boolean(document.querySelector('#app')),
      hasBuild: Boolean(document.querySelector('[data-app-build]')),
      hasScripts: document.querySelectorAll('script').length > 0,
    }));
    const oldResource = await session.page.evaluate(async (path) => {
      const response = await fetch(path, { cache: 'reload', redirect: 'follow' });
      return {
        status: response.status,
        contentType: response.headers.get('content-type') || '',
        cacheControl: response.headers.get('cache-control') || '',
        sameOrigin: new URL(response.url).origin === location.origin,
      };
    }, oldScript);
    const after = await control(options.info, '/state') as FixtureState;
    const oldScriptRequest = [...after.requests].reverse().find((request) => request.path === oldScript);
    ensure(staleResponse?.status() === 404 && staleHeaders['cache-control'] === 'no-store',
      'stale entry did not resolve to the expected non-storing 404 after cutover');
    ensure(!stalePage.hasApp && !stalePage.hasBuild,
      `stale entry executed application content after a 404: ${JSON.stringify(stalePage)}`);
    ensure(oldResource.status === 404 && oldResource.cacheControl === 'no-store' && oldResource.sameOrigin,
      'removed baseline resource did not produce a same-origin non-storing 404');
    ensure(oldScriptRequest?.status === 404 && oldScriptRequest.cache_control === 'no-store',
      'fixture did not record the removed baseline resource response');
    return {
      scenario: 'stale-entry-after-cutover',
      engine: engine.name,
      baselineEntry: oldEntry,
      baselineScript: oldScript,
      candidateEntry: candidate.identity.entry,
      candidateScript: candidate.identity.script,
      before: { entryRequests: pathCount(before, oldEntry), scriptRequests: pathCount(before, oldScript) },
      after: { entryRequests: pathCount(after, oldEntry), scriptRequests: pathCount(after, oldScript) },
      staleEntryResponse: { status: staleResponse?.status() || 0, contentType: staleHeaders['content-type'] || '', cacheControl: staleHeaders['cache-control'] || '', document: stalePage },
      removedResourceResponse: oldResource,
      serverResourceRecord: oldScriptRequest,
    };
  } finally {
    await closeSession(session);
  }
}

async function runPersistentOfflineScenario(options: Options, engine: typeof engines[number]): Promise<Record<string, unknown>> {
  const baseline = options.bundleSet.baselines[0]!;
  const candidate = options.bundleSet.candidate;
  const path = candidate.identity.script;
  const id = `${engine.name}-persistent-offline`;
  const generation = `${id}-generation-1`;
  const profile = join(options.workDir, 'profiles', `${engine.name}-persistent-offline`);
  const session = await openSession(engine, profile, appOrigin(options.info));
  try {
    await control(options.info, '/activate', 'POST', { release: 'old' });
    await session.page.goto(options.info.app_url, { waitUntil: 'domcontentloaded', timeout: 30_000 });
    await waitForApp(session.page, baseline.identity, session.appOrigin);
    await control(options.info, '/fault', 'POST', {
      id, generation, method: 'GET', path, kind: 'missing', remaining: -1, lifetime_ms: 120_000,
    });
    await control(options.info, '/activate', 'POST', { release: 'candidate' });
    await navigateCandidate(session, options.info);
    await waitForFailure(session.page);
    const first = await waitForFault(options.info, path, 'missing', id, generation);
    const onlineAttempts = [first.request];
    for (let attempt = 0; attempt < 2; attempt++) {
      const previousCount = pathCount(await control(options.info, '/state'), path);
      const navigation = session.page.waitForNavigation({ waitUntil: 'domcontentloaded', timeout: 20_000 }).catch(() => null);
      await session.page.getByRole('button', { name: 'Try again' }).click({ timeout: 10_000 });
      await navigation;
      await waitForFailure(session.page);
      const state = await control(options.info, '/state') as FixtureState;
      ensure(pathCount(state, path) > previousCount, 'explicit persistent-fault retry did not make its bounded asset request');
      onlineAttempts.push([...state.requests].reverse().find((request) => request.path === path && request.fault_id === id)!);
    }
    const beforeIdle = await control(options.info, '/state') as FixtureState;
    await session.page.waitForTimeout(1_000);
    const afterIdle = await control(options.info, '/state') as FixtureState;
    ensure(pathCount(afterIdle, path) === pathCount(beforeIdle, path), 'persistent failure caused an automatic retry without user action');
    const beforeOffline = pathCount(afterIdle, path);
    await session.context.setOffline(true);
    const offlineAttempt = session.page.waitForNavigation({ waitUntil: 'domcontentloaded', timeout: 10_000 }).catch(() => null);
    await session.page.getByRole('button', { name: 'Try again' }).click({ timeout: 10_000 }).catch(() => undefined);
    await offlineAttempt;
    await session.context.setOffline(false);
    const afterOffline = await control(options.info, '/state') as FixtureState;
    ensure(pathCount(afterOffline, path) === beforeOffline, 'offline attempt unexpectedly reached the fixture origin');
    const activeFault = afterOffline.faults.find((fault) => fault.id === id && fault.generation === generation);
    ensure(activeFault?.remaining === -1, 'persistent fault did not remain explicitly active and time-bounded');
    await control(options.info, '/fault/clear', 'POST', { id, generation });
    const repaired = await control(options.info, '/state') as FixtureState;
    const repairedPage = await session.page.goto(options.info.app_url, { waitUntil: 'domcontentloaded', timeout: 30_000 }).catch(() => null);
    await session.page.waitForFunction(() => Boolean(document.querySelector('#app')?.childNodes.length
      || document.querySelector('#herdr-load-recovery')), undefined, { timeout: 20_000 }).catch(() => undefined);
    const finalRuntime = await runtimeDetails(session.page, candidate.identity, session.appOrigin);
    const actions = repaired.fault_actions.filter((action) => action.id === id && action.generation === generation);
    ensure(actions.some((action) => action.action === 'fault_cleared')
      && !actions.some((action) => action.action === 'fault_exhausted'),
    'persistent fault clear was not distinguished from finite exhaustion');
    return {
      scenario: 'bounded-persistent-fault-and-offline-attempts',
      engine: engine.name,
      resourcePath: path,
      fault: { id, generation, kind: 'missing', remaining: -1, lifetimeMs: 120_000 },
      onlineAttempts: onlineAttempts.map((request) => ({ status: request.status, cacheControl: request.cache_control, fault: request.fault, requestCount: request.sequence })),
      idle: { requestCountBefore: pathCount(beforeIdle, path), requestCountAfter: pathCount(afterIdle, path) },
      offline: { requestCountBefore: beforeOffline, requestCountAfter: pathCount(afterOffline, path), userTriggered: true },
      clearAction: actions,
      recoveryAfterRepair: { documentStatus: repairedPage?.status() || 0, build: finalRuntime.build, appMounted: finalRuntime.appMounted, cssAccepted: finalRuntime.cssAccepted },
      faultRequestCount: pathCount(repaired, path),
    };
  } finally {
    await closeSession(session);
  }
}

function cleanFailure(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error);
  return message.replace(/https?:\/\/[^\s"')]+/giu, (value) => {
    try {
      const parsed = new URL(value);
      return `${parsed.origin}${parsed.pathname}`;
    } catch {
      return '[URL]';
    }
  }).replace(/[\r\n\t]+/gu, ' ').slice(0, 2_000);
}

async function main(): Promise<void> {
  const bundleSetFile = repositoryPath(required('--bundle-set'));
  const workDir = resolve(required('--work'));
  const resultDir = resolve(required('--output'));
  const fixtureBinary = resolve(required('--fixture'));
  await mkdir(workDir, { recursive: true, mode: 0o700 });
  await mkdir(resultDir, { recursive: true, mode: 0o700 });
  const bundleDirectory = resolve(bundleSetFile, '..');
  const rawBundleSet = await readJson<BundleSet>(bundleSetFile);
  const bundleSet: BundleSet = {
    ...rawBundleSet,
    candidate: { ...rawBundleSet.candidate, root: resolve(bundleDirectory, rawBundleSet.candidate.root) },
    baselines: rawBundleSet.baselines.map((baseline) => ({ ...baseline, root: resolve(bundleDirectory, baseline.root) })),
  };
  ensure(bundleSet.baselines.length === 1, 'phone-recovery fixture must name exactly one generated baseline');
  ensure(bundleSet.candidate.identity.script !== bundleSet.baselines[0]!.identity.script,
    'synthetic baseline and candidate must emit separate canonical script paths');
  const infoFile = join(workDir, 'fixture-info.json');
  const fixtureDataDir = join(workDir, 'fixture-runtime');
  const fixtureProcess = startCommand(fixtureBinary, [
    '-old-root', bundleSet.baselines[0]!.root,
    '-candidate-root', bundleSet.candidate.root,
    '-run-dir', fixtureDataDir,
    '-info-file', infoFile,
  ], () => undefined, {
    cwd: repositoryRoot,
    env: {
      PATH: process.env.PATH || '',
      HOME: join(workDir, 'home'),
      XDG_CONFIG_HOME: join(workDir, 'config'),
      XDG_CACHE_HOME: join(workDir, 'cache'),
      XDG_DATA_HOME: join(workDir, 'data'),
      LANG: 'C',
      LC_ALL: 'C',
    },
  });
  const options: Options = { bundleSet, info: { app_url: '', control_url: '', control_secret: '' }, workDir, resultDir, fixtureBinary };
  const results: Record<string, unknown>[] = [];
  let failure: string | undefined;
  try {
    options.info = await waitForInfo(infoFile);
    for (const engine of engines) {
      results.push(await runCanonicalAndUnchangedCss(options, engine));
    }
    for (const engine of engines) {
      results.push(await runImmutableDocumentRetry(options, engine, 'immutable-html'));
      results.push(await runImmutableDocumentRetry(options, engine, 'immutable-corrupt'));
    }
    for (const engine of engines) {
      results.push(await runStaleEntryScenario(options, engine));
      results.push(await runPersistentOfflineScenario(options, engine));
    }
    for (const engine of engines) {
      results.push(await runRepairPolicyExperiment(options, engine, 'no-cache'));
      results.push(await runRepairPolicyExperiment(options, engine, 'no-store'));
    }
  } catch (error) {
    failure = cleanFailure(error);
  } finally {
    if (options.info.control_url) await control(options.info, '/shutdown', 'POST').catch(() => undefined);
    await stopProcess(fixtureProcess);
    await writeSanitizedJson(join(resultDir, 'result.json'), {
      schema: 1,
      result: failure ? 'failed' : 'passed',
      engines: engines.map((engine) => engine.name),
      scenarios: results,
      failure,
      cacheRepairDesignGate: results.some((result) => result.repairEffective === false)
        ? 'At least one TEST-ONLY fetch policy did not establish successful canonical module and stylesheet use.' : 'All recorded repaired-response variants reached the candidate build and stylesheet after restart, or did not run.',
      evidenceScope: 'Private synthetic bundles and reusable Playwright profiles. Server counts establish origin requests; browser response digests and resource timing establish client observations only where exposed.',
    });
  }
  if (failure) {
    process.stderr.write(`PHONE_RECOVERY: ${failure}\nEvidence: ${join(resultDir, 'result.json')}\nPrivate test data preserved at ${workDir}\n`);
    process.exitCode = 1;
    return;
  }
  process.stdout.write(`Phone recovery scenarios passed. Sanitized evidence: ${join(resultDir, 'result.json')}\n`);
}

main().catch(async (error: unknown) => {
  const failure = cleanFailure(error);
  const output = option('--output');
  if (output) {
    await mkdir(resolve(output), { recursive: true, mode: 0o700 }).catch(() => undefined);
    await writeSanitizedJson(join(resolve(output), 'result.json'), {
      schema: 1, result: 'failed', scenarios: [], failure,
      evidenceScope: 'Failure before browser scenario output was available.',
    }).catch(() => undefined);
  }
  process.stderr.write(`PHONE_RECOVERY: ${failure}\n`);
  process.exitCode = 1;
});
