import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { appendFileSync, existsSync, mkdirSync, readFileSync, readdirSync, realpathSync, renameSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve, basename } from 'node:path';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { redactText, sanitizeValue } from './diagnostics';

const runnerId = 'com.facebook.WebDriverAgentRunner.xctrunner';
const productId = 'com.facebook.WebDriverAgentRunner';
const version = '16.12.1';
const maxStartupMs = 300_000;
const maxStartupLogBytes = 100 * 1024 * 1024;
const maxStatusBytes = 65_536;
const maxProcessCommandMs = 2_000;
const maxRunnerCleanupMs = 10_000;
const maxOwnerBytes = 1_048_576;
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/iu;
const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms));

function redactedBounded(value: string | Buffer, maximum: number): string {
  const text = typeof value === 'string' ? value : value.toString('utf8');
  return Buffer.from(redactText(text)).subarray(0, maximum).toString('utf8');
}

const required = (key: string): string => {
  const value = process.env[key];
  if (!value) throw new Error(`XCTEST: missing ${key}`);
  return value;
};

function deadlineTimeout(deadline: number): number {
  const timeout = deadline - Date.now();
  if (!Number.isFinite(timeout) || timeout <= 0) throw new Error('XCTEST: startup deadline');
  return timeout;
}

function run(file: string, args: string[], timeout = 10_000): string {
  if (!Number.isFinite(timeout) || timeout <= 0) throw new Error('XCTEST: startup deadline');
  const result = spawnSync(file, args, { encoding: 'utf8', timeout, maxBuffer: 4_194_304 });
  if ((result.error as NodeJS.ErrnoException | undefined)?.code === 'ETIMEDOUT') throw new Error('XCTEST: startup deadline');
  if (result.error || result.status !== 0) throw new Error(`XCTEST: ${file} failed (${result.status}): ${result.stderr?.slice(0, 2048)}`);
  return result.stdout.trim();
}

function ownership(udid: string): void {
  if (!uuidPattern.test(udid) || readFileSync(required('MOBILE_DEVICE_OWNERSHIP_FILE'), 'utf8').trim() !== `ios:${udid}`) {
    throw new Error('XCTEST: simulator ownership mismatch');
  }
}

function portValue(key: string, fallback: number): number {
  const value = Number(process.env[key] || fallback);
  if (!Number.isInteger(value) || value < 1024 || value > 65535) throw new Error('XCTEST: invalid port');
  return value;
}

function listeners(port: number, timeout = maxProcessCommandMs, observed?: (status: number | null) => void): string[] {
  if (!Number.isInteger(port) || port < 1 || port > 65535 || timeout <= 0) throw new Error('XCTEST: startup deadline');
  const result = spawnSync('lsof', ['-nP', `-iTCP:${port}`, '-sTCP:LISTEN', '-t'], { encoding: 'utf8', timeout, maxBuffer: 65_536 });
  observed?.(result.status);
  if ((result.error as NodeJS.ErrnoException | undefined)?.code === 'ETIMEDOUT') throw new Error('XCTEST: startup deadline');
  if (result.error || (result.status !== 0 && result.status !== 1)) throw new Error('XCTEST: cannot inspect endpoint ownership');
  const values = [...new Set(result.stdout.trim().split(/\s+/u).filter(Boolean))];
  if (values.some(value => !/^[1-9]\d{0,9}$/u.test(value) || Number(value) > 2_147_483_647)) throw new Error('XCTEST: endpoint returned an invalid process identifier');
  return values;
}

function processExecutable(pid: string, deadline: number): string {
  const output = run('lsof', ['-nP', '-a', '-p', pid, '-d', 'txt', '-Fn'], deadlineTimeout(deadline));
  const executable = output.split(/\r?\n/u).find(line => line.startsWith('n'))?.slice(1);
  if (!executable || !executable.startsWith('/')) throw new Error('XCTEST: process executable is unavailable');
  return realpathSync(executable);
}

interface ProcessEvidence {
  pid: string;
  executable: string;
  command: string;
  birth: string;
}

function processEvidence(pid: string, deadline: number): ProcessEvidence {
  return {
    pid,
    executable: processExecutable(pid, deadline),
    command: run('ps', ['-p', pid, '-o', 'command='], deadlineTimeout(deadline)),
    birth: run('ps', ['-p', pid, '-o', 'lstart='], deadlineTimeout(deadline)),
  };
}

function commandContainsPath(command: string, path: string): boolean {
  const resolved = resolve(path);
  const variants = [resolved];
  if (resolved.startsWith('/private/')) variants.push(resolved.slice('/private'.length));
  if (resolved.startsWith('/var/')) variants.push(`/private${resolved}`);
  return variants.some(variant => command.includes(variant));
}

function runnerExecutableMatches(executable: string, product: string, udid: string, deadline: number, productBinaryHash?: string, evaluation?: RunnerMatchEvidence): boolean {
  try {
    const resolved = realpathSync(executable);
    const app = dirname(resolved);
    const container = dirname(app);
    const applicationDirectory = dirname(container);
    const expectedApplicationDirectory = `/Devices/${udid}/data/Containers/Bundle/Application`;
    const productPath = realpathSync(product);
    const pathShape: RunnerPathShape = {};
    const pathChecks: Array<[keyof RunnerPathShape, () => boolean, string]> = [
      ['applicationNameMatches', () => basename(app) === basename(productPath), 'runner-application-name'],
      ['containerUuid', () => uuidPattern.test(basename(container)), 'runner-container-uuid'],
      ['simulatorPath', () => applicationDirectory.endsWith(expectedApplicationDirectory), 'runner-simulator-path'],
      ['executableName', () => basename(resolved) === 'WebDriverAgentRunner-Runner', 'runner-executable-name'],
    ];
    for (const [key, check, stage] of pathChecks) {
      pathShape[key] = check();
      if (evaluation) evaluation.pathShape = {...pathShape};
      if (!pathShape[key]) {
        if (evaluation) { evaluation.failureStage = stage; evaluation.errorCategory = 'runner-path-mismatch'; }
        return false;
      }
    }
    let bundleIdentifier: string;
    try {
      bundleIdentifier = run('plutil', ['-extract', 'CFBundleIdentifier', 'raw', '-o', '-', join(app, 'Info.plist')], deadlineTimeout(deadline));
    } catch (error) {
      if (evaluation) { evaluation.bundleId = {status: 'error'}; evaluation.failureStage = 'runner-bundle-id-command'; evaluation.errorCategory = 'runner-command-error'; }
      throw error;
    }
    const bundleMatches = bundleIdentifier === runnerId;
    if (evaluation) evaluation.bundleId = {status: 'evaluated', matches: bundleMatches};
    if (!bundleMatches) {
      if (evaluation) { evaluation.failureStage = 'runner-bundle-id'; evaluation.errorCategory = 'runner-bundle-mismatch'; }
      return false;
    }
    if (evaluation) evaluation.executableHash = {status: 'not-evaluated', ...(productBinaryHash ? {expected: productBinaryHash} : {})};
    let actualHash: string;
    try {
      actualHash = binaryHash(resolved);
    } catch (error) {
      if (evaluation) { evaluation.executableHash = {status: 'error', ...(productBinaryHash ? {expected: productBinaryHash} : {})}; evaluation.failureStage = 'runner-executable-hash-command'; evaluation.errorCategory = 'runner-command-error'; }
      throw error;
    }
    let expectedHash: string;
    try {
      expectedHash = productBinaryHash || binaryHash(join(productPath, 'WebDriverAgentRunner-Runner'));
    } catch (error) {
      if (evaluation) {
        evaluation.executableHash = {status: 'error', actual: actualHash, ...(productBinaryHash ? {expected: productBinaryHash} : {})};
        evaluation.failureStage = 'runner-product-hash-command';
        evaluation.errorCategory = 'runner-command-error';
      }
      throw error;
    }
    const hashMatches = actualHash === expectedHash;
    if (evaluation) evaluation.executableHash = {status: 'evaluated', expected: expectedHash, actual: actualHash, matches: hashMatches};
    if (!hashMatches && evaluation) { evaluation.failureStage = 'runner-executable-hash'; evaluation.errorCategory = 'runner-hash-mismatch'; }
    return hashMatches;
  } catch (error) {
    if (error instanceof Error && error.message === 'XCTEST: startup deadline') throw error;
    if (evaluation && !evaluation.errorCategory) {
      evaluation.failureStage ||= 'runner-executable-observation';
      evaluation.errorCategory = 'runner-observation-error';
    }
    return false;
  }
}

class ReceiptValidationError extends Error {}

interface StatusResponse {
  statusCode: number;
  body: unknown;
  truncated?: boolean;
}

async function readStatus(url: string, timeout: number): Promise<StatusResponse> {
  if (timeout <= 0) throw new Error('XCTEST: startup deadline');
  const response = await fetch(`${url}/status`, { signal: AbortSignal.timeout(timeout), redirect: 'error' });
  const reader = response.body?.getReader();
  if (!reader) return { statusCode: response.status, body: null };
  const chunks: Uint8Array[] = [];
  let bytes = 0;
  let truncated = false;
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      const available = maxStatusBytes - bytes;
      if (available <= 0) {
        truncated = true;
        break;
      }
      chunks.push(value.subarray(0, available));
      bytes += Math.min(value.length, available);
      if (value.length > available) {
        truncated = true;
        break;
      }
    }
  } finally {
    await reader.cancel().catch(() => undefined);
  }
  const text = Buffer.concat(chunks).toString('utf8');
  let body: unknown;
  try { body = JSON.parse(text); }
  catch { body = { raw: text }; }
  return { statusCode: response.status, body, ...(truncated ? { truncated: true } : {}) };
}

export function validWdaStatus(status: unknown, runtime: string): boolean {
  const value = (status as { value?: { ready?: boolean; state?: string; build?: { version?: string; productBundleIdentifier?: string }; os?: { version?: string } } })?.value;
  return value?.ready === true && value.state === 'success' && value.build?.version === version
    && value.build.productBundleIdentifier === productId && value.os?.version === runtime;
}

export function selectXctestrun(root: string, product: string, deadline = Date.now() + 10_000): string {
  const files = readdirSync(root).filter(name => /^WebDriverAgentRunner_.*\.xctestrun$/u.test(name));
  if (files.length !== 1) throw new Error('XCTEST: ambiguous or missing xctestrun');
  const path = join(root, files[0]);
  const data = JSON.parse(run('plutil', ['-convert', 'json', '-o', '-', path], deadline - Date.now()));
  const keys = Object.keys(data).filter(key => key !== '__xctestrun_metadata__');
  const target = data.WebDriverAgentRunner;
  const productPath = realpathSync(product);
  if (keys.length !== 1 || keys[0] !== 'WebDriverAgentRunner' || target?.TestHostBundleIdentifier !== runnerId
    || target.TestBundlePath !== '__TESTHOST__/PlugIns/WebDriverAgentRunner.xctest'
    || target.TestHostPath !== '__TESTROOT__/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app'
    || realpathSync(join(root, 'Debug-iphonesimulator/WebDriverAgentRunner-Runner.app')) !== productPath
    || !existsSync(join(productPath, 'PlugIns/WebDriverAgentRunner.xctest'))) {
    throw new Error('XCTEST: unexpected test product');
  }
  if (run('plutil', ['-extract', 'CFBundleIdentifier', 'raw', '-o', '-', join(productPath, 'Info.plist')], deadline - Date.now()) !== runnerId) {
    throw new Error('XCTEST: runner identifier mismatch');
  }
  return path;
}

type RunnerIdentity = Pick<ProcessEvidence, 'pid' | 'executable' | 'birth'>;
type ListenerEvidence = Partial<ProcessEvidence> & Pick<ProcessEvidence, 'pid' | 'executable'>;

interface ListenerCommandStatus {
  port: number;
  status: number | null;
}

type ListenerEndpointStatus = 'not-evaluated' | 'evaluated' | 'error';
type AssociationStatus = 'not-evaluated' | 'match' | 'mismatch';

interface ListenerEndpointSnapshot {
  status: ListenerEndpointStatus;
  pids?: string[];
  errorCategory?: string;
}

interface ListenerSnapshot {
  wda: ListenerEndpointSnapshot;
  mjpeg: ListenerEndpointSnapshot;
  evidence: ListenerEvidence[];
  commands: ListenerCommandStatus[];
}

type RunnerPathShape = Partial<Record<'applicationNameMatches' | 'containerUuid' | 'simulatorPath' | 'executableName', boolean>>;

interface RunnerMatchEvidence {
  pathShape?: RunnerPathShape;
  bundleId?: { status: 'not-evaluated' | 'evaluated' | 'error'; matches?: boolean };
  executableHash?: { status: 'not-evaluated' | 'evaluated' | 'error'; expected?: string; actual?: string; matches?: boolean };
  failureStage?: string;
  errorCategory?: string;
}

type ListenerEndpointDiagnostic =
  | { status: 'not-evaluated' }
  | { status: 'error'; errorCategory: string }
  | { status: 'evaluated'; count: number; pids: string[] };

interface ListenerValidation {
  checkedAt: string;
  endpoints: {
    wda: ListenerEndpointDiagnostic;
    mjpeg: ListenerEndpointDiagnostic;
  };
  commandStatus: ListenerCommandStatus[];
  runnerEvidencePresent: 'not-evaluated' | 'present' | 'absent';
  runnerAssociation: AssociationStatus;
  mjpegAssociation: AssociationStatus;
  bundleId: { status: 'not-evaluated' | 'evaluated' | 'error'; matches?: boolean };
  executableHash: { status: 'not-evaluated' | 'evaluated' | 'error'; expected?: string; actual?: string; matches?: boolean };
  cachedProductExecutableHash?: string;
  cachedProductExecutableHashAt?: string;
  pathShape?: RunnerMatchEvidence['pathShape'];
  failureStage: string;
  errorCategory: string;
}

interface Owner {
  udid: string;
  pid?: number;
  command?: string[];
  runnerPid?: string;
  runnerExecutable?: string;
  receipt?: string;
  installReceipt?: string;
  listenerEvidence?: ListenerEvidence[];
  listenerValidation?: ListenerValidation;
  cachedProductExecutableHash?: string;
  cachedProductExecutableHashAt?: string;
  pidBirth?: string;
  runnerBirth?: string;
  startedAt: string;
  ready: boolean;
  endedAt?: string;
  exitCode?: number | null;
  signal?: string | null;
  error?: string;
  receiptError?: string;
  url: string;
  product: string;
  xctestrun?: string;
  xctestrunHash?: string;
  status?: StatusResponse;
  logBytes?: number;
  logTruncated?: boolean;
}

function ownerContent(owner: Owner, sanitized: boolean): string {
  const content = `${JSON.stringify(sanitized ? sanitizeValue(owner) : owner, null, 2)}\n`;
  if (Buffer.byteLength(content) > maxOwnerBytes) throw new Error('XCTEST: owner evidence limit');
  return content;
}

function writeOwner(root: string, owner: Owner): void {
  const privateNext = join(root, 'owner-private.next.json');
  writeFileSync(privateNext, ownerContent(owner, false), { mode: 0o600 });
  renameSync(privateNext, join(root, 'owner-private.json'));
  const next = join(root, 'owner.next.json');
  writeFileSync(next, ownerContent(owner, true), { mode: 0o600 });
  renameSync(next, join(root, 'owner.json'));
}

function binaryHash(path: string): string {
  return createHash('sha256').update(readFileSync(path)).digest('hex');
}

const diagnosticPids = (values: string[]): string[] => values.slice(0, 16);

function endpointDiagnostic(endpoint: ListenerEndpointSnapshot): ListenerEndpointDiagnostic {
  if (endpoint.status === 'evaluated') {
    const pids = endpoint.pids || [];
    return {status: 'evaluated', count: pids.length, pids: diagnosticPids(pids)};
  }
  return endpoint.status === 'error'
    ? {status: 'error', errorCategory: endpoint.errorCategory || 'command-error'}
    : {status: 'not-evaluated'};
}

function listenerValidation(
  snapshot: ListenerSnapshot,
  commands: ListenerCommandStatus[],
  runnerAssociation: ListenerValidation['runnerAssociation'],
  mjpegAssociation: ListenerValidation['mjpegAssociation'],
  evaluation: RunnerMatchEvidence,
  failureStage: string,
  errorCategory: string,
  cachedProductExecutableHash?: string,
  cachedProductExecutableHashAt?: string,
): ListenerValidation {
  const runnerPid = snapshot.wda.status === 'evaluated' ? snapshot.wda.pids?.[0] : undefined;
  return {
    checkedAt: new Date().toISOString(),
    endpoints: {wda: endpointDiagnostic(snapshot.wda), mjpeg: endpointDiagnostic(snapshot.mjpeg)},
    commandStatus: commands.slice(0, 4),
    runnerEvidencePresent: runnerPid
      ? snapshot.evidence.length === 0 ? 'not-evaluated' : snapshot.evidence.some(entry => entry.pid === runnerPid) ? 'present' : 'absent'
      : 'not-evaluated',
    runnerAssociation,
    mjpegAssociation,
    bundleId: evaluation.bundleId || {status: 'not-evaluated'},
    executableHash: evaluation.executableHash || {status: 'not-evaluated'},
    ...(cachedProductExecutableHash ? {cachedProductExecutableHash} : {}),
    ...(cachedProductExecutableHashAt ? {cachedProductExecutableHashAt} : {}),
    ...(evaluation.pathShape ? {pathShape: evaluation.pathShape} : {}),
    failureStage,
    errorCategory,
  };
}

async function supervise(): Promise<void> {
  const root = required('IOS_XCTEST_STATE_DIR');
  const udid = required('IOS_SIMULATOR_UDID');
  const productInput = process.env.IOS_WDA_PREBUILT_PATH || '';
  const requestedPort = process.env.IOS_WDA_PORT || '8100';
  const owner: Owner = {
    udid,
    product: productInput,
    url: `http://127.0.0.1:${requestedPort}`,
    startedAt: new Date().toISOString(),
    ready: false,
  };
  let stop = false;
  let deadline = 0;
  let port = 0;
  let mjpeg = 0;
  let product = '';
  let productBinaryHash = '';
  let child: ChildProcess | undefined;
  let exited: Promise<void> | undefined;
  let childEnded = false;
  const save = (): void => {
    try {
      writeOwner(root, owner);
    } catch {
      owner.error ||= 'XCTEST: owner evidence write failed';
      stop = true;
    }
  };
  const remaining = (): number => {
    const value = deadline - Date.now();
    if (value <= 0) throw new Error('XCTEST: startup deadline');
    return value;
  };
  const onSignal = (): void => { stop = true; };
  process.on('SIGTERM', onSignal);
  process.on('SIGINT', onSignal);
  const log = join(root, 'wda-preflight-private.log');
  const publicLog = join(root, 'wda-preflight.log');
  let logBytes = 0;
  const retain = (chunk: Buffer): void => {
    const available = Math.max(0, maxStartupLogBytes - logBytes);
    const retained = chunk.subarray(0, available);
    if (retained.length) {
      try {
        appendFileSync(log, retained, { mode: 0o600 });
        logBytes += retained.length;
      } catch {
        owner.error ||= 'XCTEST: startup log write failed';
        stop = true;
      }
    }
    if (retained.length !== chunk.length) {
      owner.logTruncated = true;
      owner.error ||= 'XCTEST: log output limit';
      stop = true;
    }
    owner.logBytes = logBytes;
  };
  const receipt = (): string => {
    const installed = realpathSync(run('xcrun', ['simctl', 'get_app_container', udid, runnerId, 'app'], Math.min(10_000, remaining())));
    owner.receipt = installed;
    writeFileSync(join(root, 'installed-Info.plist'), readFileSync(join(installed, 'Info.plist')), { mode: 0o600 });
    const applicationDirectory = dirname(dirname(installed));
    const expectedApplicationDirectory = `/Devices/${udid}/data/Containers/Bundle/Application`;
    if (!uuidPattern.test(basename(dirname(installed))) || basename(installed) !== basename(product)
      || !applicationDirectory.endsWith(expectedApplicationDirectory)) {
      throw new Error('XCTEST: registration receipt mismatch');
    }
    if (run('plutil', ['-extract', 'CFBundleIdentifier', 'raw', '-o', '-', join(installed, 'Info.plist')], Math.min(10_000, remaining())) !== runnerId
      || binaryHash(join(installed, 'WebDriverAgentRunner-Runner')) !== binaryHash(join(product, 'WebDriverAgentRunner-Runner'))) {
      throw new Error('XCTEST: registration receipt mismatch');
    }
    return installed;
  };
  const listenerCommandErrorCategory = (error: unknown): string => {
    if (error instanceof Error && error.message === 'XCTEST: startup deadline') return 'timeout';
    if (error instanceof Error && error.message.includes('invalid process identifier')) return 'invalid-process-id';
    return 'command-error';
  };
  const persistListenerCommandFailure = (snapshot: ListenerSnapshot, failureStage: string, error: unknown): void => {
    const category = listenerCommandErrorCategory(error);
    const diagnosticCategory = category === 'timeout' ? 'listener-command-timeout'
      : category === 'invalid-process-id' ? 'listener-invalid-process-id' : 'listener-command-error';
    owner.listenerValidation = listenerValidation(snapshot, snapshot.commands, 'not-evaluated', 'not-evaluated', {}, failureStage,
      diagnosticCategory, owner.cachedProductExecutableHash, owner.cachedProductExecutableHashAt);
    save();
  };
  const inspectListeners = (inspectionDeadline: number): ListenerSnapshot => {
    const commands: ListenerCommandStatus[] = [];
    let wdaEndpoint: ListenerEndpointSnapshot;
    try {
      wdaEndpoint = {status: 'evaluated', pids: listeners(port, deadlineTimeout(inspectionDeadline), status => commands.push({port, status}))};
    } catch (error) {
      wdaEndpoint = {status: 'error', errorCategory: listenerCommandErrorCategory(error)};
      persistListenerCommandFailure({wda: wdaEndpoint, mjpeg: {status: 'not-evaluated'}, evidence: [], commands}, 'wda-listener-command', error);
      throw error;
    }
    let mjpegEndpoint: ListenerEndpointSnapshot;
    try {
      mjpegEndpoint = {status: 'evaluated', pids: listeners(mjpeg, deadlineTimeout(inspectionDeadline), status => commands.push({port: mjpeg, status}))};
    } catch (error) {
      mjpegEndpoint = {status: 'error', errorCategory: listenerCommandErrorCategory(error)};
      persistListenerCommandFailure({wda: wdaEndpoint, mjpeg: mjpegEndpoint, evidence: [], commands}, 'mjpeg-listener-command', error);
      throw error;
    }
    const pids = wdaEndpoint.pids || [];
    const mjpegPids = mjpegEndpoint.pids || [];
    const evidence: ListenerEvidence[] = [];
    for (const pid of [...new Set([...pids, ...mjpegPids])]) {
      try {
        evidence.push(processEvidence(pid, inspectionDeadline));
      } catch (error) {
        evidence.push({ pid, executable: '[unavailable]' });
        owner.listenerEvidence = evidence;
        save();
        if (error instanceof Error && error.message === 'XCTEST: startup deadline') {
          owner.listenerValidation = listenerValidation({wda: wdaEndpoint, mjpeg: mjpegEndpoint, evidence, commands}, commands, 'not-evaluated', 'not-evaluated', {}, 'listener-process-evidence', 'listener-process-timeout', owner.cachedProductExecutableHash, owner.cachedProductExecutableHashAt);
          save();
          throw error;
        }
      }
    }
    owner.listenerEvidence = evidence;
    save();
    return {wda: wdaEndpoint, mjpeg: mjpegEndpoint, evidence, commands};
  };
  const listenerPids = (): string[] => [...new Set([...listeners(port), ...listeners(mjpeg)])];
  const ownedListeners = (pids: string[]): boolean => {
    if (!owner.runnerPid || !owner.runnerBirth || !owner.runnerExecutable || pids.length === 0 || pids.some(pid => pid !== owner.runnerPid)) return false;
    try {
      const inspectionDeadline = Date.now() + maxProcessCommandMs * 3;
      const evidence = processEvidence(owner.runnerPid, inspectionDeadline);
      return evidence.birth === owner.runnerBirth && evidence.executable === owner.runnerExecutable
        && runnerExecutableMatches(evidence.executable, owner.product, udid, inspectionDeadline, productBinaryHash);
    } catch {
      return false;
    }
  };
  const validateListenerSnapshot = (snapshot: ListenerSnapshot, expected: RunnerIdentity | undefined, inspectionDeadline: number): RunnerIdentity => {
    const wdaPids = snapshot.wda.status === 'evaluated' ? snapshot.wda.pids || [] : [];
    const mjpegPids = snapshot.mjpeg.status === 'evaluated' ? snapshot.mjpeg.pids || [] : [];
    const runner = snapshot.wda.status === 'evaluated' ? snapshot.evidence.find(entry => entry.pid === wdaPids[0]) : undefined;
    const admittedRunner = runner as ListenerEvidence | undefined;
    const evaluation: RunnerMatchEvidence = {};
    let runnerAssociation: ListenerValidation['runnerAssociation'] = 'not-evaluated';
    let mjpegAssociation: ListenerValidation['mjpegAssociation'] = 'not-evaluated';
    const persist = (observedRunnerAssociation: ListenerValidation['runnerAssociation'], observedMjpegAssociation: ListenerValidation['mjpegAssociation'], failureStage: string, errorCategory: string): void => {
      owner.listenerValidation = listenerValidation(snapshot, snapshot.commands, observedRunnerAssociation, observedMjpegAssociation, evaluation, failureStage, errorCategory, owner.cachedProductExecutableHash, owner.cachedProductExecutableHashAt);
      save();
    };
    const record = (observedRunnerAssociation: ListenerValidation['runnerAssociation'], observedMjpegAssociation: ListenerValidation['mjpegAssociation'], failureStage: string, errorCategory: string): never => {
      persist(observedRunnerAssociation, observedMjpegAssociation, failureStage, errorCategory);
      throw new Error(failureStage.startsWith('managed-') ? 'XCTEST: managed WDA listener changed' : 'XCTEST: unknown WDA listener');
    };
    if (expected) {
      if (wdaPids.length !== 1) record('mismatch', mjpegAssociation, 'managed-wda-pid-count', 'managed-listener-identity-mismatch');
      if (!admittedRunner) record('mismatch', mjpegAssociation, 'managed-wda-runner-evidence', 'managed-listener-identity-mismatch');
      if (admittedRunner!.pid !== expected.pid) record('mismatch', mjpegAssociation, 'managed-wda-pid', 'managed-listener-identity-mismatch');
      if (admittedRunner!.executable !== expected.executable) record('mismatch', mjpegAssociation, 'managed-wda-executable', 'managed-listener-identity-mismatch');
      if (admittedRunner!.birth !== expected.birth) record('mismatch', mjpegAssociation, 'managed-wda-birth', 'managed-listener-identity-mismatch');
      runnerAssociation = 'match';
      if (mjpegPids.length > 1) record(runnerAssociation, 'mismatch', 'managed-mjpeg-pid-count', 'managed-listener-identity-mismatch');
      if (mjpegPids.some(pid => pid !== expected.pid)) record(runnerAssociation, 'mismatch', 'managed-mjpeg-pid', 'managed-listener-identity-mismatch');
      mjpegAssociation = 'match';
    }
    if (snapshot.wda.status !== 'evaluated' || wdaPids.length !== 1) record(expected ? 'mismatch' : 'not-evaluated', mjpegAssociation, 'wda-pid-count', 'listener-endpoint-cardinality');
    if (!admittedRunner) record(expected ? 'mismatch' : 'not-evaluated', mjpegAssociation, 'wda-runner-evidence', 'listener-process-evidence');
    if (admittedRunner!.executable === '[unavailable]') record(expected ? 'mismatch' : 'not-evaluated', mjpegAssociation, 'wda-executable-evidence', 'listener-process-evidence');
    if (!admittedRunner!.birth) record(expected ? 'mismatch' : 'not-evaluated', mjpegAssociation, 'wda-birth-evidence', 'listener-process-evidence');
    let executableMatch = false;
    try {
      executableMatch = runnerExecutableMatches(admittedRunner!.executable, product, udid, inspectionDeadline, productBinaryHash, evaluation);
    } catch (error) {
      if (error instanceof Error && error.message === 'XCTEST: startup deadline') {
        persist(runnerAssociation, mjpegAssociation, evaluation.failureStage || 'runner-executable-deadline', evaluation.errorCategory || 'listener-deadline');
        throw error;
      }
      record(runnerAssociation, mjpegAssociation, evaluation.failureStage || 'runner-executable-observation', evaluation.errorCategory || 'runner-command-error');
    }
    if (!executableMatch) record(runnerAssociation, mjpegAssociation, evaluation.failureStage || 'runner-executable-observation', evaluation.errorCategory || 'runner-identity-mismatch');
    if (mjpegPids.length > 1) record(runnerAssociation, 'mismatch', 'mjpeg-pid-count', 'listener-endpoint-cardinality');
    if (mjpegPids.some(pid => pid !== admittedRunner!.pid)) record(runnerAssociation, 'mismatch', 'mjpeg-pid-association', 'listener-endpoint-association');
    if (mjpegAssociation === 'not-evaluated') mjpegAssociation = 'match';
    persist(runnerAssociation, mjpegAssociation, 'validated', 'none');
    return { pid: admittedRunner!.pid, executable: admittedRunner!.executable, birth: admittedRunner!.birth! };
  };
  const stopChild = async (): Promise<void> => {
    if (!child || childEnded) return;
    child.kill('SIGTERM');
    await Promise.race([exited, sleep(10_000)]);
    if (!childEnded) child.kill('SIGKILL');
    await Promise.race([exited, sleep(2_000)]);
    if (!childEnded) owner.error ||= 'XCTEST: child cleanup did not settle';
  };
  const waitForOwnedListenersToExit = async (): Promise<void> => {
    const limit = Date.now() + maxRunnerCleanupMs;
    while (Date.now() < limit) {
      const pids = listenerPids();
      if (!pids.length) return;
      if (!ownedListeners(pids)) throw new Error('XCTEST: unknown WDA listener remained during cleanup');
      await sleep(100);
    }
    throw new Error('XCTEST: owned WDA listener cleanup deadline');
  };

  try {
    deadline = Number(readFileSync(join(root, 'deadline'), 'utf8'));
    if (!Number.isSafeInteger(deadline) || deadline <= Date.now() || deadline > Date.now() + maxStartupMs) {
      throw new Error('XCTEST: invalid startup deadline');
    }
    port = portValue('IOS_WDA_PORT', 8100);
    mjpeg = portValue('IOS_WDA_MJPEG_PORT', 9100);
    if (port === mjpeg) throw new Error('XCTEST: WDA and MJPEG ports must differ');
    owner.url = `http://127.0.0.1:${port}`;
    product = realpathSync(required('IOS_WDA_PREBUILT_PATH'));
    owner.product = product;
    productBinaryHash = binaryHash(join(product, 'WebDriverAgentRunner-Runner'));
    owner.cachedProductExecutableHash = productBinaryHash;
    owner.cachedProductExecutableHashAt = new Date().toISOString();
    save();
    ownership(udid);
    if (listeners(port, Math.min(maxProcessCommandMs, remaining())).length || listeners(mjpeg, Math.min(maxProcessCommandMs, remaining())).length) {
      throw new Error('XCTEST: occupied endpoint');
    }
    const xctestrun = selectXctestrun(required('IOS_WDA_BOOTSTRAP_PATH'), product, deadline);
    const packageRoot = dirname(required('IOS_WDA_AGENT_PATH'));
    const packageVersion = JSON.parse(readFileSync(join(packageRoot, 'package.json'), 'utf8')).version;
    if (packageVersion !== version) throw new Error('XCTEST: unpinned WDA version');
    const data = JSON.parse(run('plutil', ['-convert', 'json', '-o', '-', xctestrun], remaining()));
    data.WebDriverAgentRunner.EnvironmentVariables = {
      ...data.WebDriverAgentRunner.EnvironmentVariables,
      USE_PORT: String(port),
      USE_IP: '127.0.0.1',
      MJPEG_SERVER_PORT: String(mjpeg),
      WDA_PRODUCT_BUNDLE_IDENTIFIER: productId,
    };
    writeFileSync(xctestrun, JSON.stringify(data));
    run('plutil', ['-convert', 'xml1', xctestrun], remaining());
    owner.xctestrun = realpathSync(xctestrun);
    owner.xctestrunHash = binaryHash(owner.xctestrun);
    writeFileSync(join(root, 'selected.xctestrun'), readFileSync(owner.xctestrun), { mode: 0o600 });
    run('xcrun', ['simctl', 'install', udid, product], remaining());
    owner.receipt = receipt();
    owner.installReceipt = owner.receipt;
    writeFileSync(join(root, 'installed-Info.plist'), readFileSync(join(owner.receipt, 'Info.plist')), { mode: 0o600 });
    owner.command = ['xcodebuild', 'test-without-building', '-xctestrun', owner.xctestrun, '-destination', `id=${udid}`];
    child = spawn(owner.command[0], owner.command.slice(1), { stdio: ['ignore', 'pipe', 'pipe'] });
    if (!child.pid) throw new Error('XCTEST: xcodebuild did not provide a process identifier');
    owner.pid = child.pid;
    child.stdout?.on('data', retain);
    child.stderr?.on('data', retain);
    exited = new Promise(resolve => {
      child!.on('error', error => { owner.error ||= `XCTEST: ${error.message}`; });
      child!.on('close', (code, signal) => {
        childEnded = true;
        owner.exitCode = code;
        owner.signal = signal;
        save();
        resolve();
      });
    });
    try { owner.pidBirth = run('ps', ['-p', String(owner.pid), '-o', 'lstart='], Math.min(maxProcessCommandMs, remaining())); } catch (error) {
      owner.error ||= error instanceof Error ? error.message : String(error);
    }
    save();
    let frozenRunner: RunnerIdentity | undefined;
    while (!stop && !childEnded && !existsSync(join(root, 'stop'))) {
      remaining();
      try {
        const initial = inspectListeners(deadline);
        const initialWdaPids = initial.wda.status === 'evaluated' ? initial.wda.pids || [] : [];
        const initialMjpegPids = initial.mjpeg.status === 'evaluated' ? initial.mjpeg.pids || [] : [];
        if (!initialWdaPids.length && !initialMjpegPids.length) {
          if (frozenRunner) {
            owner.listenerValidation = listenerValidation(initial, initial.commands, 'not-evaluated', 'not-evaluated', {},
              'managed-listener-endpoints-empty', 'managed-listener-endpoint-disappearance', owner.cachedProductExecutableHash, owner.cachedProductExecutableHashAt);
            save();
            throw new Error('XCTEST: managed WDA listener changed');
          }
          await sleep(Math.min(250, remaining()));
          continue;
        }
        let candidate = validateListenerSnapshot(initial, frozenRunner, deadline);
        if (!frozenRunner) {
          try {
            owner.receipt = receipt();
          } catch (error) {
            owner.receiptError = error instanceof Error ? error.message : String(error);
            save();
            throw new ReceiptValidationError(`XCTEST: registration receipt validation failed (${owner.receiptError})`);
          }
          const afterReceipt = inspectListeners(deadline);
          candidate = validateListenerSnapshot(afterReceipt, candidate, deadline);
          frozenRunner = candidate;
          owner.runnerPid = candidate.pid;
          owner.runnerExecutable = candidate.executable;
          owner.runnerBirth = candidate.birth;
          save();
        }
        let status: StatusResponse | undefined;
        try {
          status = await readStatus(owner.url, Math.min(2_000, remaining()));
          owner.status = status;
          try {
            writeFileSync(join(root, 'wda-status.json'), redactedBounded(JSON.stringify(sanitizeValue({ checkedAt: new Date().toISOString(), ...status })) + '\n', maxStatusBytes), { mode: 0o600 });
          } catch {
            throw new Error('XCTEST: WDA status evidence write failed');
          }
          save();
        } finally {
          const afterStatus = inspectListeners(deadline);
          validateListenerSnapshot(afterStatus, frozenRunner, deadline);
        }
        if (status && status.statusCode === 200 && !status.truncated && validWdaStatus(status.body, required('IOS_PLATFORM_VERSION'))) {
          remaining();
          if (childEnded) throw new Error('XCTEST: runner exited during readiness');
          owner.ready = true;
          save();
          break;
        }
      } catch (error) {
        if (error instanceof ReceiptValidationError || (error instanceof Error && error.message.startsWith('XCTEST:'))) throw error;
      }
      await sleep(Math.min(250, remaining()));
    }
    if (!owner.ready) throw new Error(`XCTEST: runner exited before readiness (${owner.exitCode ?? 'unknown'})`);
    const lifetimeDeadline = Date.now() + 60 * 60_000;
    while (!stop && !childEnded && !existsSync(join(root, 'stop')) && Date.now() < lifetimeDeadline) await sleep(250);
    if (childEnded) owner.error ||= 'XCTEST: runner exited during scenario';
    if (!stop && !childEnded && !existsSync(join(root, 'stop'))) owner.error ||= 'XCTEST: scenario owner lifetime limit';
  } catch (error) {
    owner.error ||= error instanceof Error ? error.message : String(error);
  } finally {
    owner.ready = false;
    save();
    let ownedListenerObserved = false;
    try {
      ownership(udid);
      const pids = listenerPids();
      if (pids.length) {
        if (!ownedListeners(pids)) owner.error ||= 'XCTEST: refusing unknown WDA listener cleanup';
        else {
          ownedListenerObserved = true;
          run('xcrun', ['simctl', 'terminate', udid, runnerId]);
        }
      }
    } catch (error) {
      owner.error ||= error instanceof Error ? error.message : String(error);
    }
    await stopChild();
    if (ownedListenerObserved) {
      try { await waitForOwnedListenersToExit(); }
      catch (error) { owner.error ||= error instanceof Error ? error.message : String(error); }
    }
    try {
      ownership(udid);
      const result = spawnSync('xcrun', ['simctl', 'spawn', udid, 'log', 'show', '--last', '10m', '--style', 'compact', '--predicate', 'process CONTAINS[c] "WebDriverAgent" OR process == "xctest"'], {
        encoding: 'utf8', timeout: 10_000, maxBuffer: 16_777_216,
      });
      writeFileSync(join(root, 'ios-wda-system.log'), redactedBounded(`${result.stdout || ''}\n${result.stderr || ''}`, 16_777_216), { mode: 0o600 });
    } catch (error) {
      owner.error ||= error instanceof Error ? error.message : String(error);
    }
    try {
      if (existsSync(log)) writeFileSync(publicLog, redactedBounded(readFileSync(log, 'utf8'), maxStartupLogBytes), { mode: 0o600 });
    } catch {
      owner.error ||= 'XCTEST: startup log finalization failed';
    }
    owner.endedAt = new Date().toISOString();
    save();
    process.off('SIGTERM', onSignal);
    process.off('SIGINT', onSignal);
  }
}

export async function managedWdaCapabilities(udid: string): Promise<Record<string, unknown>> {
  const root = required('IOS_XCTEST_STATE_DIR');
  const owner: Owner = JSON.parse(readFileSync(join(root, 'owner-private.json'), 'utf8'));
  ownership(udid);
  const product = realpathSync(required('IOS_WDA_PREBUILT_PATH'));
  const productBinaryHash = binaryHash(join(product, 'WebDriverAgentRunner-Runner'));
  if (!owner.ready || owner.endedAt || owner.udid !== udid || !owner.pid || !owner.runnerPid || !owner.runnerExecutable || !owner.receipt || !owner.pidBirth || !owner.runnerBirth
    || !owner.xctestrun || !owner.xctestrunHash || owner.product !== product || !/^http:\/\/127\.0\.0\.1:[1-9]\d{3,4}$/u.test(owner.url)
    || binaryHash(owner.xctestrun) !== owner.xctestrunHash) {
    throw new Error('XCTEST: missing live managed owner');
  }
  const inspectionDeadline = Date.now() + maxProcessCommandMs * 3;
  const command = run('ps', ['-p', String(owner.pid), '-o', 'command='], deadlineTimeout(inspectionDeadline));
  const birth = run('ps', ['-p', String(owner.pid), '-o', 'lstart='], deadlineTimeout(inspectionDeadline));
  if (!command.includes(`-destination id=${udid}`) || !commandContainsPath(command, owner.xctestrun)
    || !command.includes('test-without-building') || birth !== owner.pidBirth) {
    throw new Error('XCTEST: managed owner changed');
  }
  const pids = listeners(Number(new URL(owner.url).port), deadlineTimeout(inspectionDeadline));
  const mjpegPids = listeners(portValue('IOS_WDA_MJPEG_PORT', 9100), deadlineTimeout(inspectionDeadline));
  if (pids.length !== 1 || pids[0] !== owner.runnerPid || mjpegPids.length > 1 || mjpegPids.some(pid => pid !== owner.runnerPid)) {
    throw new Error('XCTEST: managed WDA listener changed');
  }
  const listener = processEvidence(owner.runnerPid, inspectionDeadline);
  if (listener.birth !== owner.runnerBirth || listener.executable !== owner.runnerExecutable
    || !runnerExecutableMatches(listener.executable, product, udid, inspectionDeadline, productBinaryHash)) {
    throw new Error('XCTEST: managed WDA executable changed');
  }
  const status = await readStatus(owner.url, 2_000);
  if (status.statusCode !== 200 || status.truncated || !validWdaStatus(status.body, required('IOS_PLATFORM_VERSION'))) {
    throw new Error('XCTEST: managed WDA not ready');
  }
  const afterStatusDeadline = Date.now() + maxProcessCommandMs * 3;
  const afterStatusPids = listeners(Number(new URL(owner.url).port), deadlineTimeout(afterStatusDeadline));
  const afterStatusMjpegPids = listeners(portValue('IOS_WDA_MJPEG_PORT', 9100), deadlineTimeout(afterStatusDeadline));
  if (afterStatusPids.length !== 1 || afterStatusPids[0] !== owner.runnerPid || afterStatusMjpegPids.length > 1
    || afterStatusMjpegPids.some(pid => pid !== owner.runnerPid)) {
    throw new Error('XCTEST: managed WDA listener changed');
  }
  const afterStatusListener = processEvidence(owner.runnerPid, afterStatusDeadline);
  if (afterStatusListener.birth !== owner.runnerBirth || afterStatusListener.executable !== owner.runnerExecutable
    || !runnerExecutableMatches(afterStatusListener.executable, product, udid, afterStatusDeadline, productBinaryHash)) {
    throw new Error('XCTEST: managed WDA executable changed');
  }
  return { 'appium:webDriverAgentUrl': owner.url };
}

async function main(): Promise<void> {
  const mode = process.argv[2];
  const root = required('IOS_XCTEST_STATE_DIR');
  if (mode === 'supervise') {
    await supervise();
    return;
  }
  if (mode === 'start') {
    mkdirSync(root, { recursive: false, mode: 0o700 });
    const deadline = Date.now() + maxStartupMs;
    writeFileSync(join(root, 'deadline'), String(deadline), { mode: 0o600 });
    const owner: Owner = {
      udid: required('IOS_SIMULATOR_UDID'),
      product: process.env.IOS_WDA_PREBUILT_PATH || '',
      url: `http://127.0.0.1:${process.env.IOS_WDA_PORT || '8100'}`,
      startedAt: new Date().toISOString(),
      ready: false,
    };
    writeOwner(root, owner);
    const child = spawn(process.execPath, [fileURLToPath(import.meta.url), 'supervise'], { detached: true, stdio: 'ignore', env: process.env });
    let spawnError: Error | undefined;
    child.on('error', error => {
      spawnError = error;
      try {
        const current: Owner = JSON.parse(readFileSync(join(root, 'owner.json'), 'utf8'));
        current.error ||= `XCTEST: ${error.message}`;
        current.endedAt ||= new Date().toISOString();
        writeOwner(root, current);
      } catch (error) {
        spawnError ||= error instanceof Error ? error : new Error(String(error));
      }
    });
    child.unref();
    while (Date.now() < deadline) {
      if (spawnError) throw spawnError;
      if (existsSync(join(root, 'owner.json'))) {
        const current: Owner = JSON.parse(readFileSync(join(root, 'owner.json'), 'utf8'));
        if (current.ready) return;
        if (current.endedAt) throw new Error(current.error || 'XCTEST: startup exited');
      }
      await sleep(100);
    }
    writeFileSync(join(root, 'stop'), 'startup deadline', { mode: 0o600 });
    throw new Error('XCTEST: startup deadline');
  }
  if (mode === 'stop') {
    if (!existsSync(root)) return;
    ownership(required('IOS_SIMULATOR_UDID'));
    const privateOwnerPath = join(root, 'owner-private.json');
    const ownerPath = existsSync(privateOwnerPath) ? privateOwnerPath : join(root, 'owner.json');
    if (!existsSync(ownerPath)) throw new Error('XCTEST: missing owner state');
    const readOwner = (): Owner => JSON.parse(readFileSync(ownerPath, 'utf8'));
    if (!readOwner().endedAt) writeFileSync(join(root, 'stop'), 'owner cleanup', { mode: 0o600 });
    const deadline = Date.now() + 30_000;
    while (Date.now() < deadline) {
      if (existsSync(ownerPath)) {
        const current = readOwner();
        if (current.endedAt) {
          if (current.error) throw new Error(current.error);
          return;
        }
      }
      await sleep(100);
    }
    throw new Error('XCTEST: supervisor cleanup deadline');
  }
  throw new Error('XCTEST: expected start, stop or supervise');
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main().catch(error => {
    console.error(redactText(error instanceof Error ? error.message : String(error)));
    process.exitCode = 1;
  });
}
