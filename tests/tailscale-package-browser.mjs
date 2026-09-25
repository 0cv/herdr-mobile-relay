import { createRequire } from 'node:module';
import { createHash } from 'node:crypto';
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import { join } from 'node:path';

const require = createRequire('/workspace/frontend/package.json');
const { chromium } = require('@playwright/test');
const input = JSON.parse(await new Promise((resolve, reject) => {
  let data = '';
  process.stdin.setEncoding('utf8');
  process.stdin.on('data', (chunk) => { data += chunk; });
  process.stdin.on('end', () => resolve(data));
  process.stdin.on('error', reject);
}));

const cases = [];
const record = (name, passed) => cases.push({ name, passed: Boolean(passed) });
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
const diagnosticCategories = new Set([
  'websocket', 'network', 'storage', 'tls', 'type_error', 'reference_error',
  'syntax_error', 'dom_exception', 'console_error', 'page_error',
]);
const profileDiagnostics = new Map();
let progressWriteQueue = Promise.resolve();

function classifyDiagnostic(message, errorName, fallback) {
  const sample = String(message ?? '').slice(0, 2048);
  const rules = [
    [/websocket|web socket/i, 'websocket'],
    [/fetch|network|net::err_|failed to load resource/i, 'network'],
    [/localstorage|storage|quota/i, 'storage'],
    [/certificate|tls|ssl|err_cert_/i, 'tls'],
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
    profile: label, storage: [], console_errors: {}, page_errors: {},
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

async function openProfile(profile, path, url) {
  await mkdir(path, { recursive: true, mode: 0o700 });
  const context = await chromium.launchPersistentContext(path, {
    timeout: deadline,
    headless: true,
    viewport: { width: 412, height: 915 },
    deviceScaleFactor: 1,
    serviceWorkers: 'allow',
    // No ignoreHTTPSErrors and no certificate-ignore launch flags: the
    // disposable container trusts the fixture CA using its ordinary CA store.
  });
  context.setDefaultTimeout(deadline);
  context.setDefaultNavigationTimeout(deadline);
  const page = context.pages()[0] || await context.newPage();
  observePage(profile, page);
  await page.goto(url, { waitUntil: 'domcontentloaded', timeout: deadline });
  await recordStorageSnapshot(profile, 'after_navigation', page);
  return { context, page };
}

async function closeContextBounded(context) {
  if (!context) return;
  let timer;
  const timedOut = await Promise.race([
    context.close().then(() => false, () => false),
    new Promise((resolve) => { timer = setTimeout(() => resolve(true), 5000); }),
  ]);
  clearTimeout(timer);
  if (!timedOut) return;
  const browser = context.browser();
  if (!browser) return;
  let browserTimer;
  await Promise.race([
    browser.close().catch(() => {}),
    new Promise((resolve) => { browserTimer = setTimeout(resolve, 2500); }),
  ]);
  clearTimeout(browserTimer);
}

async function waitForAgent(page) {
  await page.getByRole('button', { name: /^Open / }).first().waitFor({ state: 'visible', timeout: deadline });
}

async function openFixtureAgent(page) {
  await waitForAgent(page);
  await page.getByRole('button', { name: /^Open / }).first().click();
  await page.getByRole('textbox', { name: 'Prompt' }).waitFor({ state: 'visible', timeout: deadline });
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
  'reader_invitation', 'reader_enrollment', 'reader_read_only', 'credential_preservation', 'browser_complete',
]);

function profileEvidence() {
  return [...profileDiagnostics.values()].map((profile) => ({
    profile: profile.profile,
    storage: profile.storage.slice(0, 8),
    console_errors: { ...profile.console_errors },
    page_errors: { ...profile.page_errors },
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
    controller = await openProfile('controller', controllerPath, input.setup_url);
    const controllerCredential = await waitForCredential(controller.page, 'controller', 'controller');
    record('launcher_generated_setup_link_enrolls_real_controller_profile', controllerCredential?.role === 'controller');

    const controllerReadBaseline = await socketOperationCount(input.herdr_socket_operations, 'pane.read');
    await setStage('controller_inventory');
    await openFixtureAgent(controller.page);
    const readWorked = await waitForSocketOperation(input.herdr_socket_operations, 'pane.read', controllerReadBaseline + 1);
    await setStage('controller_command');
    const prompt = controller.page.getByRole('textbox', { name: 'Prompt' });
    await prompt.fill('package acceptance harmless ping');
    await controller.page.getByRole('button', { name: 'Send prompt' }).click();
    const commandRecorded = await waitForSuccessfulOperation(input.fake_herdr_operations, 'agent prompt');
    const kinds = await operationKinds(input.fake_herdr_operations);
    const commandWorked = commandRecorded && kinds.includes('agent prompt');
    record('controller_reads_fake_inventory_and_sends_harmless_command', readWorked && commandWorked);

    await controller.page.getByRole('button', { name: /Settings/ }).click();
    await controller.page.getByRole('heading', { name: 'Devices' }).waitFor({ state: 'visible', timeout: deadline });
    await setStage('reader_invitation');
    await controller.page.getByRole('button', { name: 'Invite Device' }).click();
    await controller.page.getByLabel('Device name').fill('Package reader');
    await controller.page.getByLabel('Role').selectOption('reader');
    await controller.page.getByRole('button', { name: 'Create Invitation' }).click();
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
    await openFixtureAgent(reader.page);
    const readerRead = await waitForSocketOperation(input.herdr_socket_operations, 'pane.read', readerReadBaseline + 1);
    const readerPrompt = reader.page.getByRole('textbox', { name: 'Prompt' });
    const denied = await readerPrompt.isDisabled();
    const before = await operationCount(input.fake_herdr_operations, 'agent prompt');
    if (!denied) {
      await readerPrompt.fill('must not execute from reader');
      await reader.page.getByRole('button', { name: 'Send prompt' }).click().catch(() => {});
    }
    await reader.page.waitForTimeout(250);
    const after = await operationCount(input.fake_herdr_operations, 'agent prompt');
    record('second_persistent_profile_enrolls_reader_and_read_only_is_enforced', readerCredential?.role === 'reader' && readerRead && denied && before === after);

    await setStage('credential_preservation');
    const controllerNow = await credential(controller.page);
    const readerNow = await credential(reader.page);
    await mkdir(profiles, { recursive: true, mode: 0o700 });
    await writeFile(join(profiles, '.credential-fingerprints'), JSON.stringify({
      controller: fingerprint(controllerNow),
      reader: fingerprint(readerNow),
    }), { mode: 0o600 });
    return {
      controller_enrolled: controllerCredential?.role === 'controller',
      controller_read: readWorked,
      controller_command: (await operationKinds(input.fake_herdr_operations)).includes('agent prompt'),
      reader_enrolled: readerCredential?.role === 'reader',
      reader_read: readerRead,
      reader_mutation_denied: denied && before === after,
      credentials_preserved: true,
    };
  } finally {
    await closeContextBounded(reader?.context);
    await closeContextBounded(controller?.context);
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
    record('reprint_and_managed_restart_preserve_enrolled_device_credentials', same);
    return {
      controller_enrolled: true,
      reader_enrolled: true,
      credentials_preserved: same,
    };
  } finally {
    await closeContextBounded(reader?.context);
    await closeContextBounded(controller?.context);
  }
}

const jsDeadlineTimer = setTimeout(() => {
  const timeoutRecord = {
    mode: ['enroll', 'reprint', 'restart'].includes(input.mode) ? input.mode : 'other',
    result: 'fail',
    passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name),
    exception_type: 'BrowserBudgetTimeout',
    stage: progressStages.has(stage) ? stage : 'browser_runner',
    profiles: profileEvidence(),
  };
  process.stdout.write(JSON.stringify(timeoutRecord) + '\n', () => process.exit(1));
}, 150000);

let result = { mode: input.mode, result: 'fail', passed_cases: [] };
try {
  if (input.mode === 'enroll') {
    await setStage('controller_enrollment');
    const values = await initialEnrollment();
    result = { mode: input.mode, ...values, result: cases.length === 3 && cases.every((entry) => entry.passed) ? 'pass' : 'fail', passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name) };
  } else if (input.mode === 'reprint' || input.mode === 'restart') {
    await setStage('credential_preservation');
    const values = await preserveExistingProfiles();
    result = { mode: input.mode, ...values, result: cases.length === 1 && cases.every((entry) => entry.passed) ? 'pass' : 'fail', passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name) };
  } else throw new Error('unknown browser acceptance mode');
  if (result.result === 'pass') await setStage('browser_complete');
  else result.exception_type = 'BrowserAssertionError';
} catch (error) {
  const type = error && typeof error === 'object' && 'constructor' in error && typeof error.constructor?.name === 'string'
    ? error.constructor.name : 'BrowserError';
  result = { mode: input.mode, result: 'fail', passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name), exception_type: /^[A-Za-z][A-Za-z0-9]{0,47}$/.test(type) ? type : 'BrowserError' };
}
clearTimeout(jsDeadlineTimer);
await writeProgress();
process.stdout.write(JSON.stringify({ ...result, stage, profiles: profileEvidence() }) + '\n');
process.exitCode = result.result === 'pass' ? 0 : 1;
