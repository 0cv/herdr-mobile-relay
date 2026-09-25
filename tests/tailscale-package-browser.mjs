import { createRequire } from 'node:module';
import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
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
const deadline = 45000;

function authFrom(localStorageValue) {
  const state = JSON.parse(localStorageValue || 'null');
  if (!state || state.version !== 1 || !state.relays || typeof state.relays !== 'object') return null;
  return Object.values(state.relays).find((value) => value?.kind === 'credential') || null;
}

async function waitForCredential(page, role) {
  await page.waitForFunction(({ key, expectedRole }) => {
    try {
      const state = JSON.parse(localStorage.getItem(key) || 'null');
      const entries = Object.values(state?.relays || {});
      return entries.some((entry) => entry?.kind === 'credential' && entry.role === expectedRole);
    } catch {
      return false;
    }
  }, { key: authKey, expectedRole: role }, { timeout: deadline });
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

async function openProfile(path, url) {
  await mkdir(path, { recursive: true, mode: 0o700 });
  const context = await chromium.launchPersistentContext(path, {
    headless: true,
    viewport: { width: 412, height: 915 },
    deviceScaleFactor: 1,
    serviceWorkers: 'allow',
    // No ignoreHTTPSErrors and no certificate-ignore launch flags: the
    // disposable container trusts the fixture CA using its ordinary CA store.
  });
  const page = context.pages()[0] || await context.newPage();
  await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 30000 });
  return { context, page };
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

async function initialEnrollment() {
  const profiles = input.profiles;
  const controllerPath = join(profiles, 'controller');
  const readerPath = join(profiles, 'reader');
  let controller;
  let reader;
  try {
    stage = 'controller_enrollment';
    controller = await openProfile(controllerPath, input.setup_url);
    const controllerCredential = await waitForCredential(controller.page, 'controller');
    record('launcher_generated_setup_link_enrolls_real_controller_profile', controllerCredential?.role === 'controller');

    const controllerReadBaseline = await operationCount(input.fake_herdr_operations, 'pane read');
    stage = 'controller_inventory';
    await openFixtureAgent(controller.page);
    const readWorked = await waitForOperation(input.fake_herdr_operations, 'pane read', controllerReadBaseline + 1)
      && await waitForSuccessfulOperation(input.fake_herdr_operations, 'pane read');
    stage = 'controller_command';
    const prompt = controller.page.getByRole('textbox', { name: 'Prompt' });
    await prompt.fill('package acceptance harmless ping');
    await controller.page.getByRole('button', { name: 'Send prompt' }).click();
    const commandRecorded = await waitForSuccessfulOperation(input.fake_herdr_operations, 'agent prompt');
    const kinds = await operationKinds(input.fake_herdr_operations);
    const commandWorked = commandRecorded && kinds.includes('agent prompt');
    record('controller_reads_fake_inventory_and_sends_harmless_command', readWorked && commandWorked);

    await controller.page.getByRole('button', { name: /Settings/ }).click();
    await controller.page.getByRole('heading', { name: 'Devices' }).waitFor({ state: 'visible', timeout: deadline });
    stage = 'reader_invitation';
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
    stage = 'reader_enrollment';
    reader = await openProfile(readerPath, readerSetupURL);
    const readerCredential = await waitForCredential(reader.page, 'reader');
    stage = 'reader_read_only';
    const readerReadBaseline = await operationCount(input.fake_herdr_operations, 'pane read');
    await openFixtureAgent(reader.page);
    const readerRead = await waitForOperation(input.fake_herdr_operations, 'pane read', readerReadBaseline + 1)
      && await waitForSuccessfulOperation(input.fake_herdr_operations, 'pane read');
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

    stage = 'credential_preservation';
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
    await reader?.context.close().catch(() => {});
    await controller?.context.close().catch(() => {});
  }
}

async function preserveExistingProfiles() {
  const profiles = input.profiles;
  stage = 'credential_preservation';
  const before = JSON.parse(await readFile(join(profiles, '.credential-fingerprints'), 'utf8'));
  let controller;
  let reader;
  try {
    controller = await openProfile(join(profiles, 'controller'), input.origin + '/');
    reader = await openProfile(join(profiles, 'reader'), input.origin + '/');
    const controllerCredential = await waitForCredential(controller.page, 'controller');
    const readerCredential = await waitForCredential(reader.page, 'reader');
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
    await reader?.context.close().catch(() => {});
    await controller?.context.close().catch(() => {});
  }
}

let stage = 'browser_runner';
let result = { mode: input.mode, result: 'fail', passed_cases: [] };
try {
  if (input.mode === 'enroll') {
    stage = 'controller_enrollment';
    const values = await initialEnrollment();
    result = { mode: input.mode, ...values, result: cases.length === 3 && cases.every((entry) => entry.passed) ? 'pass' : 'fail', passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name) };
  } else if (input.mode === 'reprint' || input.mode === 'restart') {
    stage = 'credential_preservation';
    const values = await preserveExistingProfiles();
    result = { mode: input.mode, ...values, result: cases.length === 1 && cases.every((entry) => entry.passed) ? 'pass' : 'fail', passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name) };
  } else throw new Error('unknown browser acceptance mode');
  if (result.result === 'pass') stage = 'browser_complete';
  else result.exception_type = 'BrowserAssertionError';
} catch (error) {
  const type = error && typeof error === 'object' && 'constructor' in error && typeof error.constructor?.name === 'string'
    ? error.constructor.name : 'BrowserError';
  result = { mode: input.mode, result: 'fail', passed_cases: cases.filter((entry) => entry.passed).map((entry) => entry.name), exception_type: /^[A-Za-z][A-Za-z0-9]{0,47}$/.test(type) ? type : 'BrowserError' };
}
process.stdout.write(JSON.stringify({ ...result, stage }) + '\n');
process.exitCode = result.result === 'pass' ? 0 : 1;
