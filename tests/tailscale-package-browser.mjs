import { createRequire } from 'node:module';
import { createHash } from 'node:crypto';
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { dirname, join } from 'node:path';

const require = createRequire(new URL('../frontend/package.json', import.meta.url));
const { chromium } = require('@playwright/test');
const input = JSON.parse(await new Promise((resolve, reject) => {
  let data = '';
  let bytes = 0;
  process.stdin.setEncoding('utf8');
  process.stdin.on('data', (chunk) => {
    bytes += Buffer.byteLength(chunk);
    if (bytes > 65536) {
      reject(new Error('browser input cap exceeded'));
      process.stdin.destroy();
      return;
    }
    data += chunk;
  });
  process.stdin.on('end', () => resolve(data));
  process.stdin.on('error', reject);
}));

const darwinFixture = input.fixture_platform === 'darwin-isolated-byo-v1';
if (input.fixture_platform !== undefined && !darwinFixture) throw new Error('unknown fixture platform');
if (darwinFixture && process.platform !== 'darwin') throw new Error('Darwin fixture requires Darwin');
const preservationCase = darwinFixture
  ? 'reprint_and_byo_restart_preserve_enrolled_device_credentials'
  : 'reprint_and_managed_restart_preserve_enrolled_device_credentials';
const enrollmentCases = [
  'dev_setup_link_imports_bare_wss_origin_in_extracted_frontend',
  'launcher_generated_setup_link_enrolls_real_controller_profile',
  'controller_reads_fake_inventory_and_sends_harmless_command',
  'second_persistent_profile_enrolls_reader_and_read_only_is_enforced',
];
const expectedCases = input.mode === 'enroll' ? enrollmentCases
  : ['reprint', 'restart'].includes(input.mode) ? [preservationCase] : [];
const cases = [];
const ownedContexts = new Set();
const darwinWorkerProfiles = new Set();
const darwinLaunchProfiles = new Set();
const darwinDevelopmentImports = new Set();
let unexpectedDarwinDestination = false;
let darwinLaunchUncertain = false;
let darwinStopping = false;
function completeCases() {
  return !darwinStopping && !darwinLaunchUncertain && !unexpectedDarwinDestination && expectedCases.length > 0 && cases.length === expectedCases.length
    && cases.every((entry, index) => entry.name === expectedCases[index] && entry.passed === true);
}
const record = (name, passed) => {
  if (unexpectedDarwinDestination) {
    throw Object.assign(new Error('unexpected owned browser destination'), { name: 'BrowserAssertionError' });
  }
  if (name !== expectedCases[cases.length]) {
    throw Object.assign(new Error('unexpected browser case order'), { name: 'BrowserAssertionError' });
  }
  cases.push({ name, passed: passed === true });
  // A failed case must not advance to invitation creation or another profile.
  if (passed !== true) {
    throw Object.assign(new Error('browser case failed'), { name: 'BrowserAssertionError' });
  }
};
const authKey = 'herdr_device_auth_v1';
const relaysKey = 'herdr_relays';
const deadline = 15000;

function authFrom(localStorageValue) {
  const state = JSON.parse(localStorageValue || 'null');
  if (!state || state.version !== 1 || !state.relays || typeof state.relays !== 'object') return null;
  return Object.values(state.relays).find((value) => value?.kind === 'credential') || null;
}

async function waitForCredential(page, role, profile = role) {
  try {
    await page.waitForFunction(({ key, expectedRole }) => {
      try {
        const state = JSON.parse(localStorage.getItem(key) || 'null');
        const entries = Object.values(state?.relays || {});
        return entries.some((entry) => entry?.kind === 'credential' && entry.role === expectedRole);
      } catch {
        return false;
      }
    }, { key: authKey, expectedRole: role }, { timeout: deadline });
  } catch (error) {
    await recordStorageSnapshot(profile, 'credential_wait_failed', page);
    throw error;
  }
  await recordStorageSnapshot(profile, 'credentialed', page);
  const stored = await page.evaluate((key) => localStorage.getItem(key), authKey);
  return authFrom(stored);
}

async function credential(page) {
  const stored = await page.evaluate((key) => localStorage.getItem(key), authKey);
  return authFrom(stored);
}

function fingerprint(value) {
  return createHash('sha256').update(JSON.stringify({
    id: value?.id,
    version: value?.version,
    secret: value?.secret,
    deviceId: value?.deviceId,
    role: value?.role,
  })).digest('hex');
}

const profileNames = new Set(['controller', 'reader']);
const storageCheckpoints = new Set(['after_navigation', 'credential_wait_failed', 'credentialed']);
const uiCheckpoints = new Set([
  'inventory_initial', 'agent_button_timeout', 'agent_button_disabled',
  'agent_button_ready', 'agent_click_failed', 'prompt_wait_failed', 'prompt_visible',
  'command_initial', 'command_fill_failed', 'command_prompt_filled',
  'command_send_failed', 'command_result', 'settings_navigated',
]);
const diagnosticCategories = new Set([
  'websocket', 'network', 'storage', 'tls', 'type_error', 'reference_error',
  'syntax_error', 'dom_exception', 'console_error', 'page_error', 'navigation_error',
]);
const profileDiagnostics = new Map();
let progressWriteQueue = Promise.resolve();

function classifyDiagnostic(message, errorName, fallback) {
  const sample = String(message ?? '').slice(0, 2048);
  const rules = [
    [/websocket|web socket/i, 'websocket'],
    [/certificate|tls|ssl|err_cert_/i, 'tls'],
    [/fetch|network|net::err_|failed to load resource/i, 'network'],
    [/localstorage|storage|quota/i, 'storage'],
    [/typeerror/i, 'type_error'],
    [/referenceerror/i, 'reference_error'],
    [/syntaxerror/i, 'syntax_error'],
  ];
  for (const [pattern, category] of rules) if (pattern.test(sample)) return category;
  if (errorName === 'TypeError') return 'type_error';
  if (errorName === 'ReferenceError') return 'reference_error';
  if (errorName === 'SyntaxError') return 'syntax_error';
  if (errorName === 'DOMException') return 'dom_exception';
  return fallback;
}

function incrementDiagnostic(profile, kind, category) {
  const diagnostics = profileDiagnostics.get(profile);
  if (!diagnostics || !diagnosticCategories.has(category)) return;
  const counts = diagnostics[kind];
  counts[category] = Math.min(16, (counts[category] || 0) + 1);
  void writeProgress();
}

function incrementWebSocket(profile, kind) {
  const diagnostics = profileDiagnostics.get(profile);
  if (!diagnostics || !['attempts', 'closed', 'errors'].includes(kind)) return;
  diagnostics.websockets[kind] = Math.min(16, diagnostics.websockets[kind] + 1);
  void writeProgress();
}

function observePage(profile, page) {
  const label = profileNames.has(profile) ? profile : 'controller';
  const diagnostics = {
    profile: label, storage: [], ui_snapshots: [], console_errors: {}, page_errors: {}, navigation_errors: {},
    websockets: { attempts: 0, closed: 0, errors: 0 },
  };
  profileDiagnostics.set(label, diagnostics);
  page.on('console', (message) => {
    if (message.type() !== 'error') return;
    let text = '';
    try { text = message.text().slice(0, 2048); } catch { /* category remains allowlisted */ }
    incrementDiagnostic(label, 'console_errors', classifyDiagnostic(text, '', 'console_error'));
  });
  page.on('pageerror', (error) => {
    let text = '';
    let name = '';
    try { text = String(error?.message ?? '').slice(0, 2048); } catch { /* category remains allowlisted */ }
    try { name = String(error?.name ?? '').slice(0, 64); } catch { /* category remains allowlisted */ }
    incrementDiagnostic(label, 'page_errors', classifyDiagnostic(text, name, 'page_error'));
  });
  page.on('websocket', (socket) => {
    if (darwinFixture) {
      try {
        const destination = new URL(socket.url());
        if (destination.origin !== input.origin.replace(/^https:/, 'wss:')
          || !['/', '/ws'].includes(destination.pathname) || destination.search || destination.hash) {
          unexpectedDarwinDestination = true;
        }
      } catch { unexpectedDarwinDestination = true; }
    }
    incrementWebSocket(label, 'attempts');
    socket.on('close', () => incrementWebSocket(label, 'closed'));
    socket.on('socketerror', () => incrementWebSocket(label, 'errors'));
  });
}

async function recordStorageSnapshot(profile, checkpoint, page) {
  const label = profileNames.has(profile) ? profile : 'controller';
  const diagnostics = profileDiagnostics.get(label);
  if (!diagnostics || !storageCheckpoints.has(checkpoint) || diagnostics.storage.length >= 8) return;
  let storage;
  try {
    storage = await page.evaluate(({ auth, relays }) => {
      const describe = (key) => {
        const raw = localStorage.getItem(key);
        if (raw === null) return { present: false, type: 'absent' };
        try {
          const value = JSON.parse(raw);
          return { present: true, type: value === null ? 'null' : Array.isArray(value) ? 'array' : typeof value };
        } catch {
          return { present: true, type: 'invalid_json' };
        }
      };
      try {
        return { available: true, device_auth: describe(auth), relays: describe(relays) };
      } catch {
        return {
          available: false,
          device_auth: { present: false, type: 'unavailable' },
          relays: { present: false, type: 'unavailable' },
        };
      }
    }, { auth: authKey, relays: relaysKey });
  } catch {
    storage = {
      available: false,
      device_auth: { present: false, type: 'unavailable' },
      relays: { present: false, type: 'unavailable' },
    };
  }
  diagnostics.storage.push({ checkpoint, ...storage });
  await writeProgress();
}

async function recordUISnapshot(profile, checkpoint, page) {
  const label = profileNames.has(profile) ? profile : 'controller';
  const diagnostics = profileDiagnostics.get(label);
  if (!diagnostics || !uiCheckpoints.has(checkpoint) || diagnostics.ui_snapshots.length >= 8) return;
  let summary;
  try {
    summary = await page.evaluate(() => {
      const cards = [...document.querySelectorAll('.agent-card')];
      const buttons = [...document.querySelectorAll('button.agent-open')];
      const tones = { danger: 0, warning: 0, success: 0, muted: 0 };
      for (const card of cards) {
        const dot = card.querySelector('.status-dot');
        for (const tone of Object.keys(tones)) {
          if (dot?.classList.contains(`status-${tone}`)) tones[tone] += 1;
        }
      }
      const prompt = document.querySelector('textarea[role="combobox"][aria-label="Prompt"]');
      const actionButtons = [...document.querySelectorAll('button[aria-label]')];
      const actionCounts = {
        send_prompt: actionButtons.filter((button) => button.getAttribute('aria-label') === 'Send prompt').length,
        submit_terminal_text: actionButtons.filter((button) => button.getAttribute('aria-label') === 'Submit terminal text').length,
        submitting_input: actionButtons.filter((button) => button.getAttribute('aria-label') === 'Submitting input').length,
      };
      const header = document.querySelector('header .status-dot[role="img"]');
      const headerLabel = header?.getAttribute('aria-label') || '';
      const relayMatch = headerLabel.match(/^(\d{1,4})\/(\d{1,4}) relays connected(?:; (\d{1,4}) agent inventory (unavailable|loading))?$/);
      let connectionState = 'unknown';
      let inventoryState = 'not_reported';
      let activeAgentStatus = 'not_applicable';
      let connectedRelays = 0;
      let configuredRelays = 0;
      if (relayMatch) {
        connectedRelays = Number(relayMatch[1]);
        configuredRelays = Number(relayMatch[2]);
        connectionState = connectedRelays === 0 ? 'disconnected'
          : connectedRelays >= configuredRelays ? 'connected' : 'partial';
        inventoryState = relayMatch[4] || 'ready';
      } else if (headerLabel.startsWith('Agent ')) {
        connectionState = 'active_agent';
        const status = headerLabel.slice('Agent '.length).toLowerCase();
        activeAgentStatus = ['idle', 'needs inspection', 'working', 'done'].includes(status) ? status : 'other';
      }
      const view = document.body.dataset.view || '';
      const views = new Set([
        'agents', 'terminal', 'history', 'settings', 'workspaces', 'launch',
        'activity', 'activity_detail', 'push', 'push_unavailable', 'notification',
      ]);
      const headerTone = ['danger', 'warning', 'success', 'muted']
        .find((tone) => header?.classList.contains(`status-${tone}`)) || 'unknown';
      const sendActionState = actionCounts.submitting_input ? 'submitting_input'
        : actionCounts.send_prompt ? 'send_prompt'
          : actionCounts.submit_terminal_text ? 'submit_terminal_text' : 'missing';
      return {
        view: views.has(view) ? view : 'other',
        prompt_inputs: prompt ? 1 : 0,
        enabled_prompt_inputs: prompt && !prompt.disabled ? 1 : 0,
        disabled_prompt_inputs: prompt?.disabled ? 1 : 0,
        send_prompt_buttons: actionCounts.send_prompt,
        submit_terminal_text_buttons: actionCounts.submit_terminal_text,
        submitting_input_buttons: actionCounts.submitting_input,
        send_action_state: sendActionState,
        agent_cards: cards.length,
        open_buttons: buttons.length,
        enabled_open_buttons: buttons.filter((button) => !button.disabled).length,
        disabled_open_buttons: buttons.filter((button) => button.disabled).length,
        stale_agent_cards: cards.filter((card) => card.classList.contains('stale')).length,
        status_tones: tones,
        header_tone: headerTone,
        connection_state: connectionState,
        inventory_state: inventoryState,
        connected_relays: connectedRelays,
        configured_relays: configuredRelays,
        active_agent_status: activeAgentStatus,
      };
    });
  } catch {
    summary = {
      view: 'other', prompt_inputs: 0, enabled_prompt_inputs: 0, disabled_prompt_inputs: 0,
      send_prompt_buttons: 0, submit_terminal_text_buttons: 0, submitting_input_buttons: 0,
      send_action_state: 'missing', agent_cards: 0, open_buttons: 0, enabled_open_buttons: 0,
      disabled_open_buttons: 0, stale_agent_cards: 0,
      status_tones: { danger: 0, warning: 0, success: 0, muted: 0 },
      header_tone: 'unknown', connection_state: 'unknown', inventory_state: 'not_reported',
      connected_relays: 0, configured_relays: 0, active_agent_status: 'not_applicable',
    };
  }
  diagnostics.ui_snapshots.push({ checkpoint, ...summary });
  await writeProgress();
}

async function openProfile(profile, path, url, developmentImport = false) {
  if (darwinStopping) throw new Error('owned browser shutdown has started');
  if (darwinFixture && developmentImport) darwinDevelopmentImports.add(profile);
  await mkdir(path, { recursive: true, mode: 0o700 });
  if (darwinStopping) throw new Error('owned browser shutdown has started');
  if (darwinFixture) darwinLaunchUncertain = true;
  const context = await chromium.launchPersistentContext(path, {
    timeout: deadline,
    headless: true,
    ...(darwinFixture ? { channel: 'chromium', chromiumSandbox: true, args: ['--enable-automation'] } : {}),
    viewport: { width: 412, height: 915 },
    deviceScaleFactor: 1,
    serviceWorkers: 'allow',
    // TLS verification stays enabled. Linux uses private HOME NSS;
    // Darwin uses only each owned profile's ServerCertificate database.
  });
  ownedContexts.add(context);
  if (darwinFixture) darwinLaunchUncertain = false;
  if (darwinStopping) throw new Error('owned browser shutdown has started');
  if (darwinFixture) {
    // Constrain routed owned-page requests, not OS/browser-wide egress.
    // Service-worker-owned interceptions are not attested by this route.
    // Continue real requests; never fulfill TLS, responses, or worker content.
    await context.route('**/*', async (route) => {
      let allowed = false;
      try { allowed = new URL(route.request().url()).origin === input.origin; } catch { /* refuse */ }
      if (allowed) await route.continue();
      else {
        unexpectedDarwinDestination = true;
        await route.abort('blockedbyclient');
      }
    });
    if (developmentImport) {
      await context.routeWebSocket('**/*', (webSocket) => {
        let allowed = false;
        try { allowed = new URL(webSocket.url()).origin === input.origin.replace(/^https:/, 'wss:'); } catch { /* refuse */ }
        if (!allowed) unexpectedDarwinDestination = true;
        // Import-only check: refuse, never fulfill or proxy, this one-use dial.
        // This context is closed before launcher enrollment. All authenticated
        // journeys use a new context with native, unrouted browser WSS/TLS.
        webSocket.close({ code: 1008, reason: allowed ? 'development fragment import only' : 'owned destination refused' });
      });
    }
  }
  context.setDefaultTimeout(deadline);
  context.setDefaultNavigationTimeout(deadline);
  const page = context.pages()[0] || await context.newPage();
  observePage(profile, page);
  if (darwinFixture && profile === 'reader' && input.mode === 'enroll') {
    await page.addInitScript(installReaderMutationProbe);
  }
  try {
    if (darwinFixture) {
      const session = await context.newCDPSession(page);
      const info = await session.send('Browser.getVersion');
      const { arguments: argv } = await session.send('Browser.getBrowserCommandLine');
      const pinned = require(join(dirname(require.resolve('playwright-core')),
        'lib/server/chromium/chromiumSwitches.js')).chromiumSwitches();
      const expected = [...pinned, '--enable-unsafe-swiftshader', '--headless', '--hide-scrollbars', '--mute-audio',
        '--blink-settings=primaryHoverType=2,availableHoverTypes=2,primaryPointerType=4,availablePointerTypes=4',
        '--enable-automation', '--user-data-dir=' + path, '--remote-debugging-pipe', 'about:blank'];
      if (info.product !== 'Chrome/151.0.7922.34' || argv.length !== expected.length + 1
        || !expected.every((argument, index) => argv[index + 1] === argument)
        || !argv.includes('--use-mock-keychain')
        || argv.some((argument) => /no-sandbox|ignore-certificate|insecure-localhost|test-root/.test(argument))) {
        throw new Error('owned profile browser identity or launch arguments differ');
      }
      await session.detach();
      darwinLaunchProfiles.add(profile);
      // Bootstrap only a genuinely empty profile, through the shipped Settings
      // UI, before importing either setup fragment. Its Stop Push Notifications
      // flow registers the root via legacyPushEndpoint but cannot subscribe push
      // with zero relay connections. No harness worker registration is used.
      await page.goto(input.origin + '/', { waitUntil: 'domcontentloaded', timeout: deadline });
      const hasRoot = await page.evaluate(async () => Boolean(
        (await navigator.serviceWorker.getRegistration())?.active));
      if (!hasRoot) {
        const empty = await page.evaluate(({ auth, relays }) => {
          const configured = JSON.parse(localStorage.getItem(relays) || '[]');
          const devices = JSON.parse(localStorage.getItem(auth) || 'null');
          return Array.isArray(configured) && configured.length === 0
            && Object.keys(devices?.relays || {}).length === 0;
        }, { auth: authKey, relays: relaysKey });
        if (!empty) throw new Error('worker bootstrap requires an empty private profile');
        await context.grantPermissions(['notifications'], { origin: input.origin });
        // Mount again so the shipped pushPreferences store observes the origin-
        // scoped permission. pushOptedIn defaults true with a granted permission;
        // clicking a stale Enable button would actually toggle OFF, not ON.
        await page.reload({ waitUntil: 'domcontentloaded', timeout: deadline });
        await page.getByRole('navigation', { name: 'Application' })
          .getByRole('button', { name: /^Settings/ }).click();
        // Genuine opt-out initializes the root worker while there are no relays,
        // before any connection callback can start an external subscription.
        await page.getByRole('button', { name: 'Stop Push Notifications', exact: true }).click();
        await page.getByRole('button', { name: 'Enable Push Notifications', exact: true })
          .waitFor({ state: 'visible', timeout: deadline });
        await context.clearPermissions();
      }
      await page.waitForFunction(async () => {
        if (!globalThis.isSecureContext || !navigator.serviceWorker) return false;
        const registration = await navigator.serviceWorker.getRegistration();
        const worker = registration?.active;
        if (!worker || worker.state !== 'activated') return false;
        const script = new URL(worker.scriptURL);
        return registration.scope === location.origin + '/'
          && script.origin === location.origin && script.pathname === '/sw.js';
      }, undefined, { timeout: deadline });
    }
    await page.goto(url, { waitUntil: 'domcontentloaded', timeout: deadline });
    if (darwinFixture) {
      await page.waitForFunction(async () => {
        const registration = await navigator.serviceWorker.getRegistration();
        const active = registration?.active;
        const controller = navigator.serviceWorker.controller;
        if (!isSecureContext || !active || active.state !== 'activated' || !controller) return false;
        const script = new URL(active.scriptURL);
        return registration.scope === location.origin + '/' && script.origin === location.origin
          && script.pathname === '/sw.js' && controller.scriptURL === active.scriptURL
          && localStorage.getItem('herdr_push_enabled') === 'false';
      }, undefined, { timeout: deadline });
      darwinWorkerProfiles.add(profile);
    }
  } catch (error) {
    let text = '';
    let name = '';
    try { text = String(error?.message ?? '').slice(0, 2048); } catch { /* category remains allowlisted */ }
    try { name = String(error?.name ?? '').slice(0, 64); } catch { /* category remains allowlisted */ }
    incrementDiagnostic(profile, 'navigation_errors', classifyDiagnostic(text, name, 'navigation_error'));
    await writeProgress();
    throw error;
  }
  await recordStorageSnapshot(profile, 'after_navigation', page);
  return { context, page };
}

async function closeContextBounded(context, end = Date.now() + 5000) {
  if (!context || !ownedContexts.has(context)) return;
  let timer;
  try {
    await Promise.race([
      context.close(),
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error('owned browser context closure timed out')), Math.max(0, end - Date.now()));
      }),
    ]);
    ownedContexts.delete(context);
  } finally {
    clearTimeout(timer);
  }
}

async function closeOwnedContexts(end = Date.now() + 5000) {
  const results = await Promise.allSettled([...ownedContexts].map((context) => closeContextBounded(context, end)));
  if (results.some((entry) => entry.status !== 'fulfilled') || ownedContexts.size !== 0) {
    throw new Error('owned browser cleanup failed or remains uncertain');
  }
}

async function waitForAgent(page) {
  await page.getByRole('button', { name: /^Open / }).first().waitFor({ state: 'visible', timeout: deadline });
}

// Serialized into only the Darwin reader's enrollment page. Replace one real
// pane-read payload BEFORE native AES-GCM encryption, using the UI's existing
// authenticated session, sequence queue and native Chromium WSS connection.
// No credentials, keys, handshake, ciphertext or server response are invented;
// decryption and TLS verification remain native and responses pass unchanged.
function installReaderMutationProbe() {
  const subtle = crypto.subtle;
  const nativeEncrypt = subtle.encrypt;
  const nativeDecrypt = subtle.decrypt;
  const encoder = new TextEncoder();
  const decoder = new TextDecoder();
  const requestId = 'package-reader-refusal-' + crypto.randomUUID();
  let authenticatedReader = false;
  let armed = false;
  let attempted = false;
  let encrypted = false;
  let responseSeen = false;
  let refused = false;
  const sessionTraffic = (algorithm, direction) => {
    if (algorithm?.name !== 'AES-GCM' || !algorithm.additionalData) return false;
    const source = algorithm.additionalData;
    const bytes = ArrayBuffer.isView(source)
      ? new Uint8Array(source.buffer, source.byteOffset, source.byteLength) : new Uint8Array(source);
    const prefix = encoder.encode(`herdr-e2ee-v2 ${direction}`);
    return bytes.length === prefix.length + 9 && bytes[prefix.length] === 0
      && prefix.every((byte, index) => bytes[index] === byte);
  };
  subtle.encrypt = async function (algorithm, key, data) {
    if (!armed || attempted || !authenticatedReader || !sessionTraffic(algorithm, 'c2s')) {
      return Reflect.apply(nativeEncrypt, subtle, [algorithm, key, data]);
    }
    let message;
    try { message = JSON.parse(decoder.decode(data)); } catch { /* leave other payloads unchanged */ }
    if (message?.type !== 'read_pane' || message.pane_id !== 'workspace:agent'
      || message.target?.pane_id !== message.pane_id || !message.server_session_id
      || message.target?.server_session_id !== message.server_session_id) {
      return Reflect.apply(nativeEncrypt, subtle, [algorithm, key, data]);
    }
    attempted = true; // One attempt only, even if encryption or delivery fails.
    const mutation = {
      type: 'submit_prompt', protocol: 3, request_id: requestId,
      pane_id: message.pane_id, target: message.target, server_session_id: message.server_session_id,
      text: 'package acceptance reader refusal harmless ping',
    };
    const ciphertext = await Reflect.apply(nativeEncrypt, subtle,
      [algorithm, key, encoder.encode(JSON.stringify(mutation))]);
    encrypted = true;
    return ciphertext;
  };
  subtle.decrypt = async function (algorithm, key, data) {
    // A failed native authentication tag never produces an observation.
    const plaintext = await Reflect.apply(nativeDecrypt, subtle, [algorithm, key, data]);
    if (!sessionTraffic(algorithm, 's2c')) return plaintext;
    let message;
    try { message = JSON.parse(decoder.decode(plaintext)); } catch { return plaintext; }
    if (message?.type === 'e2ee_server_finish' && message.version === 2) {
      authenticatedReader = message.role === 'reader';
    }
    if (encrypted && message?.request_id === requestId) {
      responseSeen = true;
      refused = message.type === 'error' && message.error?.code === 'reader_denied'
        && message.error?.args?.operation === 'submit_prompt';
      subtle.encrypt = nativeEncrypt;
      subtle.decrypt = nativeDecrypt;
    }
    return plaintext;
  };
  Object.defineProperty(globalThis, '__herdrReaderMutationProbe', {
    value: Object.freeze({
      arm() {
        if (!authenticatedReader || armed || attempted) return false;
        armed = true;
        return true;
      },
      status() { return { authenticatedReader, attempted, encrypted, responseSeen, refused }; },
    }),
  });
}

async function darwinReaderMutationRefused(page) {
  // Reopening the genuine terminal UI causes a fresh read on the SAME enrolled
  // connection. Its exact target becomes the one-shot harmless mutation above;
  // do not enable the reader's disabled composer or insert a stored credential.
  await page.getByRole('button', { name: 'Back', exact: true }).click();
  await waitForAgent(page);
  const armed = await page.evaluate(() => globalThis.__herdrReaderMutationProbe?.arm() === true);
  if (!armed) throw new Error('reader mutation probe lacks an authenticated reader session');
  await page.getByRole('button', { name: /^Open / }).first().click();
  await page.waitForFunction(() => globalThis.__herdrReaderMutationProbe?.status().responseSeen === true,
    undefined, { timeout: deadline });
  return page.evaluate(() => {
    const status = globalThis.__herdrReaderMutationProbe.status();
    return status.authenticatedReader && status.attempted && status.encrypted && status.refused;
  });
}

async function openFixtureAgent(page, profile) {
  const button = page.getByRole('button', { name: /^Open / }).first();
  await recordUISnapshot(profile, 'inventory_initial', page);
  try {
    await button.waitFor({ state: 'visible', timeout: deadline });
  } catch (error) {
    await recordUISnapshot(profile, 'agent_button_timeout', page);
    throw error;
  }
  if (!await button.isEnabled()) {
    await recordUISnapshot(profile, 'agent_button_disabled', page);
    try {
      await page.waitForFunction(() => {
        const candidate = document.querySelector('button.agent-open');
        return candidate instanceof HTMLButtonElement && !candidate.disabled;
      }, null, { timeout: deadline });
    } catch (error) {
      await recordUISnapshot(profile, 'agent_button_disabled', page);
      throw error;
    }
  }
  await recordUISnapshot(profile, 'agent_button_ready', page);
  try {
    await button.click();
  } catch (error) {
    await recordUISnapshot(profile, 'agent_click_failed', page);
    throw error;
  }
  try {
    await page.getByRole('combobox', { name: 'Prompt' }).waitFor({ state: 'visible', timeout: deadline });
  } catch (error) {
    await recordUISnapshot(profile, 'prompt_wait_failed', page);
    throw error;
  }
  await recordUISnapshot(profile, 'prompt_visible', page);
  if (darwinFixture) {
    // Attribute the live fixture read to this page as well as checking socket
    // counts. Another authenticated profile's polling cannot render its data
    // here, and a stored credential alone cannot satisfy this assertion.
    await page.getByRole('log', { name: 'Agent terminal output' })
      .filter({ hasText: 'Harmless fixture output' })
      .waitFor({ state: 'visible', timeout: deadline });
  }
}

async function operationKinds(path) {
  let content = '';
  try { content = await readFile(path, 'utf8'); } catch { return []; }
  return content.split('\n').filter(Boolean).flatMap((line) => {
    try { return [JSON.parse(line).argv?.slice(0, 2).join(' ') || '']; } catch { return []; }
  });
}

async function operationRecords(path) {
  let content = '';
  try { content = await readFile(path, 'utf8'); } catch { return []; }
  return content.split('\n').filter(Boolean).flatMap((line) => {
    try { return [JSON.parse(line)]; } catch { return []; }
  });
}

async function operationCount(path, expected) {
  return (await operationKinds(path)).filter((kind) => kind === expected).length;
}

async function socketOperationCount(path, method, outcome = 'succeeded') {
  try {
    const operations = JSON.parse(await readFile(path, 'utf8'));
    if (!Array.isArray(operations)) return 0;
    return operations.reduce((count, item) => count + (
      item?.method === method && item?.outcome === outcome && Number.isInteger(item?.count)
        ? item.count : 0
    ), 0);
  } catch {
    return 0;
  }
}

async function waitForSocketOperation(path, method, minimumCount = 1) {
  const end = Date.now() + deadline;
  while (Date.now() < end) {
    if (await socketOperationCount(path, method) >= minimumCount) return true;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  return false;
}

async function waitForOperation(path, expected, minimumCount = 1) {
  const end = Date.now() + deadline;
  while (Date.now() < end) {
    if (await operationCount(path, expected) >= minimumCount) return true;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  return false;
}

async function waitForSuccessfulOperation(path, expected) {
  const end = Date.now() + deadline;
  while (Date.now() < end) {
    const records = await operationRecords(path);
    if (records.some((record) => record.argv?.slice(0, 2).join(' ') === expected && record.outcome === 'succeeded')) return true;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  return false;
}

let stage = 'browser_runner';
const progressStages = new Set([
  'browser_runner', 'controller_enrollment', 'controller_inventory', 'controller_command',
  'controller_settings', 'controller_settings_devices', 'reader_invitation', 'reader_enrollment', 'reader_read_only',
  'credential_preservation', 'browser_complete',
]);

function profileEvidence() {
  return [...profileDiagnostics.values()].map((profile) => ({
    profile: profile.profile,
    storage: profile.storage.slice(0, 8),
    ui_snapshots: profile.ui_snapshots.slice(0, 8),
    console_errors: { ...profile.console_errors },
    page_errors: { ...profile.page_errors },
    navigation_errors: { ...profile.navigation_errors },
    websockets: { ...profile.websockets },
  }));
}

function writeProgress() {
  if (typeof input.progress_path !== 'string') return Promise.resolve();
  const progress = {
    mode: ['enroll', 'reprint', 'restart'].includes(input.mode) ? input.mode : 'other',
    stage: progressStages.has(stage) ? stage : 'browser_runner',
    passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name),
    profiles: profileEvidence(),
  };
  const temporary = `${input.progress_path}.tmp`;
  progressWriteQueue = progressWriteQueue.catch(() => {}).then(async () => {
    try {
      await writeFile(temporary, JSON.stringify(progress), { mode: 0o600 });
      await rename(temporary, input.progress_path);
    } catch {
      // Progress is optional, bounded diagnostic evidence; it never gates the flow.
    }
  });
  return progressWriteQueue;
}

const browserExceptionTypes = new Set([
  'BrowserScriptMissing', 'BrowserProtocolError', 'BrowserAssertionError', 'BrowserBudgetTimeout',
  'BrowserError', 'TimeoutExpired', 'TimeoutError', 'StrictLocatorError', 'Error', 'TypeError', 'ReferenceError',
  'SyntaxError', 'RangeError', 'DOMException', 'TargetClosedError', 'ProtocolError', 'PageClosedError',
]);

function safeExceptionType(error) {
  let name = '';
  let message = '';
  try { name = String(error?.name ?? error?.constructor?.name ?? '').slice(0, 64); } catch { /* allowlisted fallback */ }
  try { message = String(error?.message ?? '').slice(0, 2048); } catch { /* allowlisted fallback */ }
  if (/strict mode violation/i.test(message)) return 'StrictLocatorError';
  if (/timeout|timed out|exceeded.{0,32}time/i.test(message)) return 'TimeoutError';
  return browserExceptionTypes.has(name) ? name : 'BrowserError';
}

let resultEmitted = false;
async function emitBrowserResult(values = {}, error = null) {
  if (resultEmitted) return;
  const passed = completeCases();
  const safeResult = {
    mode: ['enroll', 'reprint', 'restart'].includes(input.mode) ? input.mode : 'other',
    result: error ? 'fail' : passed ? 'pass' : 'fail',
    passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name),
    stage: progressStages.has(stage) ? stage : 'browser_runner',
    profiles: profileEvidence(),
    owned_contexts_closed: ownedContexts.size === 0 && !darwinLaunchUncertain,
    ...(darwinFixture ? {
      fixture_platform: 'darwin-isolated-byo-v1',
      app_worker_verified: darwinWorkerProfiles.has('controller') && darwinWorkerProfiles.has('reader'),
      notification_delivery_disabled: darwinWorkerProfiles.has('controller') && darwinWorkerProfiles.has('reader'),
      launch_arguments_verified: darwinLaunchProfiles.has('controller') && darwinLaunchProfiles.has('reader'),
    } : {}),
  };
  for (const key of [
    'controller_enrolled', 'controller_read', 'controller_command', 'reader_enrolled',
    'reader_read', 'reader_mutation_denied', 'credentials_preserved',
  ]) {
    if (typeof values?.[key] === 'boolean') safeResult[key] = values[key];
  }
  if (error) safeResult.exception_type = safeExceptionType(error);
  else if (!passed) safeResult.exception_type = 'BrowserAssertionError';
  const encoded = JSON.stringify(safeResult) + '\n';
  if (Buffer.byteLength(encoded) > 65536) throw new Error('browser result cap exceeded');
  resultEmitted = true;
  await new Promise((resolve) => process.stdout.write(encoded, resolve));
  await writeProgress();
}

async function setStage(value) {
  stage = progressStages.has(value) ? value : 'browser_runner';
  await writeProgress();
}

async function initialEnrollment() {
  const profiles = input.profiles;
  const controllerPath = join(profiles, 'controller');
  const readerPath = join(profiles, 'reader');
  let controller;
  let reader;
  try {
    await setStage('controller_enrollment');
    controller = await openProfile('controller', controllerPath, input.dev_setup_url, darwinFixture);
    await controller.page.waitForFunction(({ key, expected }) => {
      try {
        const relays = JSON.parse(localStorage.getItem(key) || '[]');
        return Array.isArray(relays) && relays.some((relay) => relay?.url === expected && relay?.token?.length === 32);
      } catch {
        return false;
      }
    }, { key: relaysKey, expected: input.dev_relay_origin }, { timeout: deadline });
    if (darwinFixture && await controller.page.evaluate((key) => {
      const state = JSON.parse(localStorage.getItem(key) || 'null');
      return Object.values(state?.relays || {}).some((entry) => entry?.kind === 'credential');
    }, authKey)) throw new Error('development import unexpectedly consumed bootstrap enrollment');
    record('dev_setup_link_imports_bare_wss_origin_in_extracted_frontend', true);
    await controller.page.evaluate(({ darwin, auth, relays }) => {
      if (darwin) {
        // Preserve the notification opt-out created by the genuine UI. Clearing
        // it would restore pushOptedIn's default after development-link import.
        localStorage.removeItem(auth);
        localStorage.removeItem(relays);
      } else localStorage.clear();
      sessionStorage.clear();
    }, { darwin: darwinFixture, auth: authKey, relays: relaysKey });
    if (darwinFixture) {
      // Dispose of the import-only interception entirely. The same persistent
      // profile retains its genuine notification opt-out and root worker, but
      // the launcher link now enrolls over native Chromium WSS with real TLS.
      await closeContextBounded(controller.context);
      darwinDevelopmentImports.delete('controller');
      controller = await openProfile('controller', controllerPath, input.setup_url);
    } else {
      await controller.page.goto(input.setup_url, { waitUntil: 'domcontentloaded', timeout: deadline });
    }
    await recordStorageSnapshot('controller', 'after_navigation', controller.page);
    const controllerCredential = await waitForCredential(controller.page, 'controller', 'controller');
    record('launcher_generated_setup_link_enrolls_real_controller_profile', controllerCredential?.role === 'controller');

    const controllerReadBaseline = await socketOperationCount(input.herdr_socket_operations, 'pane.read');
    await setStage('controller_inventory');
    await openFixtureAgent(controller.page, 'controller');
    const readWorked = await waitForSocketOperation(input.herdr_socket_operations, 'pane.read', controllerReadBaseline + 1);
    await setStage('controller_command');
    await recordUISnapshot('controller', 'command_initial', controller.page);
    const prompt = controller.page.getByRole('combobox', { name: 'Prompt' });
    try {
      await prompt.fill('package acceptance harmless ping');
    } catch (error) {
      await recordUISnapshot('controller', 'command_fill_failed', controller.page);
      throw error;
    }
    await recordUISnapshot('controller', 'command_prompt_filled', controller.page);
    try {
      await controller.page.getByRole('button', { name: 'Send prompt' }).click();
    } catch (error) {
      await recordUISnapshot('controller', 'command_send_failed', controller.page);
      throw error;
    }
    const commandRecorded = await waitForSuccessfulOperation(input.fake_herdr_operations, 'agent prompt');
    await recordUISnapshot('controller', 'command_result', controller.page);
    const kinds = await operationKinds(input.fake_herdr_operations);
    const commandWorked = commandRecorded && kinds.includes('agent prompt');
    record('controller_reads_fake_inventory_and_sends_harmless_command', readWorked && commandWorked);

    await setStage('controller_settings');
    await controller.page.getByRole('navigation', { name: 'Application' })
      .getByRole('button', { name: /^Settings/ }).click();
    await setStage('controller_settings_devices');
    await recordUISnapshot('controller', 'settings_navigated', controller.page);
    await controller.page.getByRole('heading', { name: 'Devices', exact: true, level: 3 })
      .waitFor({ state: 'visible', timeout: deadline });
    await setStage('reader_invitation');
    await controller.page.getByRole('button', { name: 'Invite Device', exact: true }).click();
    const inviteDialog = controller.page.getByRole('dialog', { name: 'Invite Device' });
    await inviteDialog.getByLabel('Device name').fill('Package reader');
    await inviteDialog.getByLabel('Role').selectOption('reader');
    await inviteDialog.getByRole('button', { name: 'Create Invitation' }).click();
    const invitation = controller.page.getByLabel('One-use invitation link');
    await invitation.waitFor({ state: 'visible', timeout: deadline });
    const readerSetupURL = await invitation.inputValue();
    const invitationParams = new URLSearchParams(new URL(readerSetupURL).hash.slice(1));
    if (!readerSetupURL.startsWith(input.origin + '/')
      || !/^[A-Za-z0-9_-]{43}$/.test(invitationParams.get('setup') || '')
      || !/^[A-Za-z0-9_-]{16,128}$/.test(invitationParams.get('invite') || '')
      || Number(invitationParams.get('invite_version')) < 1
      || !Number.isFinite(Number(invitationParams.get('invite_expires')))
      || !invitationParams.get('relay')?.startsWith('wss://')) {
      throw new Error('live controller UI did not produce the source-format one-use invitation fragment');
    }
    await setStage('reader_enrollment');
    reader = await openProfile('reader', readerPath, readerSetupURL);
    const readerCredential = await waitForCredential(reader.page, 'reader', 'reader');
    await setStage('reader_read_only');
    const readerReadBaseline = await socketOperationCount(input.herdr_socket_operations, 'pane.read');
    await openFixtureAgent(reader.page, 'reader');
    const readerRead = await waitForSocketOperation(input.herdr_socket_operations, 'pane.read', readerReadBaseline + 1);
    const readerPrompt = reader.page.getByRole('combobox', { name: 'Prompt' });
    const denied = await readerPrompt.isDisabled();
    const before = await operationCount(input.fake_herdr_operations, 'agent prompt');
    const relayDenied = !darwinFixture || await darwinReaderMutationRefused(reader.page);
    if (!darwinFixture && !denied) {
      await readerPrompt.fill('must not execute from reader');
      await reader.page.getByRole('button', { name: 'Send prompt' }).click().catch(() => {});
    }
    await reader.page.waitForTimeout(250);
    const after = await operationCount(input.fake_herdr_operations, 'agent prompt');
    const readerMutationDenied = denied && relayDenied && before === after;
    record('second_persistent_profile_enrolls_reader_and_read_only_is_enforced', readerCredential?.role === 'reader' && readerRead && readerMutationDenied);

    await setStage('credential_preservation');
    const controllerNow = await credential(controller.page);
    const readerNow = await credential(reader.page);
    await mkdir(profiles, { recursive: true, mode: 0o700 });
    await writeFile(join(profiles, '.credential-fingerprints'), JSON.stringify({
      controller: fingerprint(controllerNow),
      reader: fingerprint(readerNow),
    }), { mode: 0o600 });
    const values = {
      controller_enrolled: controllerCredential?.role === 'controller',
      controller_read: readWorked,
      controller_command: (await operationKinds(input.fake_herdr_operations)).includes('agent prompt'),
      reader_enrolled: readerCredential?.role === 'reader',
      reader_read: readerRead,
      reader_mutation_denied: readerMutationDenied,
      credentials_preserved: true,
    };
    return values;
  } finally {
    // Includes contexts whose page navigation failed before openProfile returned.
    await closeOwnedContexts();
  }
}

async function preserveExistingProfiles() {
  const profiles = input.profiles;
  await setStage('credential_preservation');
  const before = JSON.parse(await readFile(join(profiles, '.credential-fingerprints'), 'utf8'));
  let controller;
  let reader;
  try {
    controller = await openProfile('controller', join(profiles, 'controller'), input.origin + '/');
    reader = await openProfile('reader', join(profiles, 'reader'), input.origin + '/');
    const controllerCredential = await waitForCredential(controller.page, 'controller', 'controller');
    const readerCredential = await waitForCredential(reader.page, 'reader', 'reader');
    const same = fingerprint(controllerCredential) === before.controller && fingerprint(readerCredential) === before.reader;
    await waitForAgent(controller.page);
    await waitForAgent(reader.page);
    let controllerRead = false;
    let readerRead = false;
    if (darwinFixture) {
      // A stored credential and cached agent card do not prove the restarted
      // backend authenticated this browser. Require new fixture socket reads.
      let baseline = await socketOperationCount(input.herdr_socket_operations, 'pane.read');
      await openFixtureAgent(controller.page, 'controller');
      controllerRead = await waitForSocketOperation(input.herdr_socket_operations, 'pane.read', baseline + 1);
      // Terminal polling is periodic. Retire the controller before measuring
      // the reader so its background reads cannot satisfy the reader's case.
      await closeContextBounded(controller.context);
      baseline = await socketOperationCount(input.herdr_socket_operations, 'pane.read');
      await openFixtureAgent(reader.page, 'reader');
      readerRead = await waitForSocketOperation(input.herdr_socket_operations, 'pane.read', baseline + 1);
    }
    record(preservationCase, same && (!darwinFixture || (controllerRead && readerRead)));
    const values = {
      controller_enrolled: true,
      reader_enrolled: true,
      credentials_preserved: same,
      ...(darwinFixture ? { controller_read: controllerRead, reader_read: readerRead } : {}),
    };
    return values;
  } finally {
    await closeOwnedContexts();
  }
}

async function stopDarwinBrowser(exceptionType) {
  if (darwinStopping) return;
  darwinStopping = true;
  // Python's owned-child INT wait is three seconds. Give the two contexts one
  // shared two-second close window, not two renewed per-context deadlines.
  // Hard interruption cannot prove closure; the parent retains uncertainty.
  const hardStop = setTimeout(() => process.exit(1), 2800);
  try {
    await closeOwnedContexts(Date.now() + 2000);
  } catch { /* ownedContexts/launch uncertainty remain in the failed result */ }
  try {
    await emitBrowserResult({}, Object.assign(new Error('owned browser interrupted'), { name: exceptionType }));
  } finally {
    clearTimeout(hardStop);
    process.exit(1);
  }
}
if (darwinFixture) {
  for (const name of ['SIGINT', 'SIGTERM']) {
    process.on(name, () => { void stopDarwinBrowser('BrowserAssertionError'); });
  }
}

const jsDeadlineTimer = setTimeout(() => {
  if (darwinFixture) {
    void stopDarwinBrowser('BrowserBudgetTimeout');
    return;
  }
  if (resultEmitted) {
    process.exit(1);
    return;
  }
  const timeoutRecord = {
    mode: ['enroll', 'reprint', 'restart'].includes(input.mode) ? input.mode : 'other',
    result: 'fail',
    passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name),
    exception_type: 'BrowserBudgetTimeout',
    stage: progressStages.has(stage) ? stage : 'browser_runner',
    profiles: profileEvidence(),
    // The hard deadline cannot attest asynchronous closure/reaping on either OS.
    owned_contexts_closed: false,
    ...(darwinFixture ? { fixture_platform: 'darwin-isolated-byo-v1' } : {}),
  };
  resultEmitted = true;
  const encoded = JSON.stringify(timeoutRecord) + '\n';
  if (Buffer.byteLength(encoded) > 65536) {
    process.exit(1);
    return;
  }
  process.stdout.write(encoded, () => process.exit(1));
}, darwinFixture ? 145000 : 150000);

let result = { mode: input.mode, result: 'fail', passed_cases: [] };
try {
  if (input.mode === 'enroll') {
    await setStage('controller_enrollment');
    const values = await initialEnrollment();
    result = { mode: input.mode, ...values, result: completeCases() ? 'pass' : 'fail', passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name) };
  } else if (input.mode === 'reprint' || input.mode === 'restart') {
    await setStage('credential_preservation');
    const values = await preserveExistingProfiles();
    result = { mode: input.mode, ...values, result: completeCases() ? 'pass' : 'fail', passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name) };
  } else throw new Error('unknown browser acceptance mode');
  if (result.result === 'pass') await setStage('browser_complete');
  else result.exception_type = 'BrowserAssertionError';
} catch (error) {
  result = {
    mode: input.mode,
    result: 'fail',
    passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name),
    exception_type: safeExceptionType(error),
  };
}
if (!resultEmitted) {
  const error = result.result === 'pass' ? null
    : Object.assign(new Error('browser acceptance failed'), { name: result.exception_type || 'BrowserAssertionError' });
  await emitBrowserResult(result, error);
}
clearTimeout(jsDeadlineTimer);
process.exitCode = result.result === 'pass' ? 0 : 1;
