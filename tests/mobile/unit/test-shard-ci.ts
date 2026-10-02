import { createHash } from 'node:crypto';
import { lstat, mkdir, readFile, readdir, realpath, writeFile } from 'node:fs/promises';
import { basename, dirname, join, resolve } from 'node:path';
import { isDeepStrictEqual } from 'node:util';
import { BoundedCommandError, boundedCommand, type BoundedCommandOptions, type BoundedCommandResult } from '../support/bounded-process';
import { PhaseBudget } from '../support/budget';
import { generateUnitTestShardManifest, validateUnitTestShardManifest, type UnitTestInventory, type UnitTestShard, type UnitTestShardManifest } from './test-shards';
import { createUnitTestOutputContract, expectedUnitTestSuccessMultiplicity, hasForbiddenUnitTestOutputCharacter, validateUnitTestOutput, type UnitTestOutputAssessment } from './test-output';

const GROUP_SHARD_LIMIT = 4;
const GROUP_MATRIX_LIMIT = 256;
const GROUP_DEADLINE_MS = 900_000;
const GROUP_TIMESTAMP_TOLERANCE_MS = 1_000;
const SHARD_HARD_DEADLINE_MS = 180_000;
const SHARD_RECEIPT_OVERHEAD_LIMIT_MS = 10_000;
const SHARD_EXECUTION_LIMIT_MS = 175_000;
const SHARD_OUTPUT_LIMIT_BYTES = 1 * 1024 * 1024;
const JSON_INPUT_LIMIT_BYTES = 20 * 1024 * 1024;
const UNIT_RUNNER = 'tests/mobile/unit/run.ts';
const HASH_PATTERN = /^[0-9a-f]{64}$/u;
const SOURCE_SHA_PATTERN = /^[0-9a-f]{40}$/u;
const POSITIVE_INTEGER_PATTERN = /^[1-9][0-9]*$/u;
const GROUP_ID_PATTERN = /^group-[0-9]{3}$/u;

type RunIdentity = { sourceSha: string; runId: string; attempt: string };
export type UnitCiGroup = { id: string; shardIds: string[] };
export type UnitCiExecutionPlan = RunIdentity & {
  schema: 1;
  inventoryDigest: string;
  inventoryFileSha256: string;
  manifestFileSha256: string;
  shardCount: number;
  groups: UnitCiGroup[];
  sha256: string;
};
export type UnitShardCommandResult = Omit<BoundedCommandResult, 'stdout' | 'stderr'> & {
  stdout: string;
  stderr: string;
  storedStdoutBytes: number;
  storedStderrBytes: number;
  storedStdoutSha256: string;
  storedStderrSha256: string;
};
type UnitShardCommandInput = {
  binary: string;
  args: string[];
  result: BoundedCommandResult | null;
  rejected?: boolean;
  launchError?: string;
};
export type UnitShardReceipt = {
  schema: 1;
  groupId: string;
  shardId: string;
  selectedIds: string[];
  sourceSha: string;
  runId: string;
  attempt: string;
  inventoryDigest: string;
  inventoryFileSha256: string;
  manifestFileSha256: string;
  planSha256: string;
  startedAt: string;
  durationMs: number;
  command: {
    binary: string;
    args: string[];
    rejected: boolean;
    result: UnitShardCommandResult | null;
    launchError?: string;
  };
  accounting: UnitTestOutputAssessment;
};
export type UnitCiGroupReceipt = RunIdentity & {
  schema: 1;
  groupId: string;
  inventoryDigest: string;
  inventoryFileSha256: string;
  manifestFileSha256: string;
  planSha256: string;
  shardIds: string[];
  attemptedShardIds: string[];
  receiptDigests: Array<{ shardId: string; sha256: string }>;
  startedAt: string;
  completedAt: string;
  durationMs: number;
  result: 'success' | 'failure';
};

export type UnitCiArtifactGroup = {
  artifactName: string;
  group: unknown;
  receipts: Array<{ receipt: unknown; sha256: string }>;
};

export type UnitCiAggregateInput = {
  inventory: unknown;
  manifest: unknown;
  plan: unknown;
  artifactGroups: UnitCiArtifactGroup[];
  identity: RunIdentity;
  preparationResult: string;
  groupsResult: string;
};

export type UnitCiAggregateResult = { accepted: boolean; errors: string[]; groups: number; shards: number };

type CommandRunner = (binary: string, args: string[], timeoutMs: number, options: BoundedCommandOptions) => Promise<BoundedCommandResult>;

function invalid(message: string): never {
  throw new Error(`UNIT_SHARD_CI: ${message}`);
}

function record(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function sha256(value: string | Buffer): string {
  return createHash('sha256').update(value).digest('hex');
}

function jsonText(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function validateIdentity(identity: RunIdentity): void {
  if (!SOURCE_SHA_PATTERN.test(identity.sourceSha)) invalid('source SHA must be a full lowercase Git SHA');
  if (!POSITIVE_INTEGER_PATTERN.test(identity.runId)) invalid('run id must be a positive decimal integer');
  if (!POSITIVE_INTEGER_PATTERN.test(identity.attempt)) invalid('run attempt must be a positive decimal integer');
}

function canonicalPlan(value: Omit<UnitCiExecutionPlan, 'sha256'>): string {
  return JSON.stringify(value);
}

export function groupUnitTestShards(shards: readonly UnitTestShard[], maximumGroups = GROUP_MATRIX_LIMIT): UnitCiGroup[] {
  if (!Array.isArray(shards) || !shards.length) invalid('execution plan cannot be empty');
  if (!Number.isSafeInteger(maximumGroups) || maximumGroups < 1) invalid('matrix limit must be a positive integer');
  const ids = new Set<string>();
  const groups: UnitCiGroup[] = [];
  for (let offset = 0; offset < shards.length; offset += GROUP_SHARD_LIMIT) {
    const shardIds = shards.slice(offset, offset + GROUP_SHARD_LIMIT).map((shard) => {
      if (!shard || typeof shard.id !== 'string' || !shard.id || ids.has(shard.id)) invalid('shard ids must be nonempty and unique');
      ids.add(shard.id);
      if (!Array.isArray(shard.selection) || !shard.selection.length) invalid(`shard ${shard.id} has an empty selection`);
      return shard.id;
    });
    groups.push({ id: `group-${String(groups.length + 1).padStart(3, '0')}`, shardIds });
  }
  if (groups.length > maximumGroups) invalid(`execution matrix has ${groups.length} groups, exceeding ${maximumGroups}`);
  return groups;
}

export function createUnitCiExecutionPlan(
  manifest: UnitTestShardManifest,
  identity: RunIdentity,
  inventoryFileSha256: string,
  manifestFileSha256: string,
  maximumGroups = GROUP_MATRIX_LIMIT,
): UnitCiExecutionPlan {
  validateIdentity(identity);
  if (!HASH_PATTERN.test(inventoryFileSha256) || !HASH_PATTERN.test(manifestFileSha256)) invalid('inventory and manifest file digests must be SHA256 values');
  const groups = groupUnitTestShards(manifest.shards, maximumGroups);
  const body = {
    schema: 1 as const,
    ...identity,
    inventoryDigest: manifest.inventory.sha256,
    inventoryFileSha256,
    manifestFileSha256,
    shardCount: manifest.shards.length,
    groups,
  };
  return { ...body, sha256: sha256(canonicalPlan(body)) };
}

export function validateUnitCiExecutionPlan(
  planInput: unknown,
  manifest: UnitTestShardManifest,
  identity: RunIdentity,
  inventoryFileSha256: string,
  manifestFileSha256: string,
): UnitCiExecutionPlan {
  if (!record(planInput)) invalid('execution plan must be an object');
  const expected = createUnitCiExecutionPlan(manifest, identity, inventoryFileSha256, manifestFileSha256);
  if (!isDeepStrictEqual(planInput, expected)) invalid('execution plan differs from the authoritative source-bound plan');
  return planInput as UnitCiExecutionPlan;
}

export function groupMatrixOutput(plan: UnitCiExecutionPlan): string {
  return JSON.stringify({ include: plan.groups.map(({ id }) => ({ group_id: id })) });
}

export function expectedSuccessMultiplicity(
  inventory: UnitTestInventory,
  manifest: UnitTestShardManifest,
  selection: readonly string[],
): Record<string, number> {
  validateUnitTestShardManifest(manifest, inventory);
  return expectedUnitTestSuccessMultiplicity(inventory, selection);
}

function successfulCommandLifecycle(result: BoundedCommandResult | null, rejected: boolean): boolean {
  return Boolean(result && !rejected && result.code === 0 && result.signal == null && result.launchError == null
    && result.timedOut === false && result.outputLimitExceeded === false && result.aborted === false
    && Number.isSafeInteger(result.durationMs) && result.durationMs >= 0 && result.durationMs <= SHARD_HARD_DEADLINE_MS
    && Number.isSafeInteger(result.targetPID) && result.targetPID! > 0
    && result.childExited === true && result.targetCloseObserved === true
    && result.targetStdoutNaturalEnd === true && result.targetStdoutCloseObserved === true
    && result.targetStderrNaturalEnd === true && result.targetStderrCloseObserved === true
    && result.managerStdoutNaturalEnd === true && result.managerStdoutCloseObserved === true
    && result.managerStderrNaturalEnd === true && result.managerStderrCloseObserved === true
    && result.callerStdoutNaturalEnd === true && result.callerStdoutCloseObserved === true
    && result.callerStderrNaturalEnd === true && result.callerStderrCloseObserved === true
    && result.targetOutputRelayHealthy === true && result.ownedProcessesExited === true && result.stdioClosed === true);
}

function testOutputAccounting(
  inventory: UnitTestInventory,
  selection: readonly string[],
  stdout: string,
  stderr: string,
  result: BoundedCommandResult | null,
  rejected = false,
): UnitShardReceipt['accounting'] {
  const output = validateUnitTestOutput(inventory, selection, stdout, stderr);
  const outputMissing = !result || result.stdoutBytes + result.stderrBytes === 0 || output.outputMissing;
  const accepted = successfulCommandLifecycle(result, rejected) && !outputMissing && output.accepted;
  return { ...output, outputMissing, accepted };
}

function outputMarker(base: string, expectedLines: ReadonlySet<string>): string {
  let marker = base;
  while (expectedLines.has(marker)) marker += '!';
  return marker;
}

function sanitizeCommandOutput(value: string, expectedLines: ReadonlySet<string>): string {
  if (!value) return '';
  const invalidMarker = outputMarker('[REDACTED INVALID OUTPUT]', expectedLines);
  const unexpectedMarker = outputMarker('[REDACTED UNEXPECTED OUTPUT]', expectedLines);
  const failureMarker = outputMarker('not ok - [REDACTED FAILURE OUTPUT]', expectedLines);
  const skipMarker = outputMarker('ok - [REDACTED SKIP OUTPUT] # SKIP', expectedLines);
  return value.replace(/\r\n/gu, '\n').split(/(?<=\n)/u).map((fragment) => {
    const newline = fragment.endsWith('\n') ? '\n' : '';
    const line = newline ? fragment.slice(0, -1) : fragment;
    if (expectedLines.has(line)) return `${line}${newline}`;
    if (line.startsWith('not ok - ')) return `${failureMarker}${newline}`;
    if (line.startsWith('ok - ') && line.slice('ok - '.length).includes(' # SKIP')) return `${skipMarker}${newline}`;
    if (hasForbiddenUnitTestOutputCharacter(line)) return `${invalidMarker}${newline}`;
    return `${unexpectedMarker}${newline}`;
  }).join('');
}

function sanitizeLaunchError(value: string): string {
  return value ? '[REDACTED LAUNCH ERROR]' : value;
}

function receiptIdentity(identity: RunIdentity, plan: UnitCiExecutionPlan) {
  return {
    sourceSha: identity.sourceSha,
    runId: identity.runId,
    attempt: identity.attempt,
    inventoryDigest: plan.inventoryDigest,
    inventoryFileSha256: plan.inventoryFileSha256,
    manifestFileSha256: plan.manifestFileSha256,
    planSha256: plan.sha256,
  };
}

export function createUnitShardReceipt(
  inventory: UnitTestInventory,
  manifest: UnitTestShardManifest,
  plan: UnitCiExecutionPlan,
  groupId: string,
  shard: UnitTestShard,
  command: UnitShardCommandInput,
  startedAt: string,
  durationMs: number,
): UnitShardReceipt {
  const outputContract = createUnitTestOutputContract(inventory, shard.selection);
  const expectedLines = new Set(outputContract.events.map(({ line }) => line));
  const stdout = sanitizeCommandOutput(command.result?.stdout || '', expectedLines);
  const stderr = sanitizeCommandOutput(command.result?.stderr || '', expectedLines);
  const result = command.result ? {
    ...command.result,
    ...(command.result.launchError ? { launchError: sanitizeLaunchError(command.result.launchError) } : {}),
    stdout,
    stderr,
    stdoutBytes: Buffer.byteLength(stdout),
    stderrBytes: Buffer.byteLength(stderr),
    stdoutRetainedBytes: Buffer.byteLength(stdout),
    stderrRetainedBytes: Buffer.byteLength(stderr),
    stdoutSha256: sha256(stdout),
    stderrSha256: sha256(stderr),
    storedStdoutBytes: Buffer.byteLength(stdout),
    storedStderrBytes: Buffer.byteLength(stderr),
    storedStdoutSha256: sha256(stdout),
    storedStderrSha256: sha256(stderr),
  } : null;
  return {
    schema: 1,
    groupId,
    shardId: shard.id,
    selectedIds: [...shard.selection],
    ...receiptIdentity(plan, plan),
    startedAt,
    durationMs,
    command: {
      binary: command.binary,
      args: [...command.args],
      rejected: command.rejected === true,
      result,
      ...(command.launchError ? { launchError: sanitizeLaunchError(command.launchError) } : {}),
    },
    accounting: testOutputAccounting(inventory, shard.selection, stdout, stderr, result, command.rejected === true),
  };
}

export function unitShardCommandArgs(selection: readonly string[]): string[] {
  return ['--no-env-file', UNIT_RUNNER, '--select-tests', selection.join(',')];
}

async function withUnitCommandEnvironment<T>(run: () => Promise<T>): Promise<T> {
  const keys = ['BUN_OPTIONS', 'GOTOOLCHAIN', 'MOBILE_UNIT_FILTER', 'IOS_TEST_FILTER'] as const;
  const previous = new Map(keys.map((key) => [key, process.env[key]]));
  process.env.BUN_OPTIONS = '--no-env-file';
  process.env.GOTOOLCHAIN = 'local';
  delete process.env.MOBILE_UNIT_FILTER;
  delete process.env.IOS_TEST_FILTER;
  try {
    return await run();
  } finally {
    for (const key of keys) {
      const value = previous.get(key);
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  }
}

async function runAuthoritativeInventory(runner: CommandRunner = boundedCommand, budget?: PhaseBudget): Promise<{ inventory: UnitTestInventory; stdout: string }> {
  if (!process.versions.bun) invalid('authoritative listing must run under Bun');
  const result = await withUnitCommandEnvironment(() => runner(
    process.execPath,
    ['--no-env-file', UNIT_RUNNER, '--list-tests'],
    30_000,
    { maxBytes: 4 * 1024 * 1024, cleanupReservationMs: 1_000, label: 'authoritative unit test listing', budget },
  ));
  if (result.code !== 0 || result.timedOut || !result.ownedProcessesExited || !result.stdioClosed) invalid('authoritative test listing did not complete successfully');
  let inventory: unknown;
  try { inventory = JSON.parse(result.stdout); } catch { invalid('authoritative test listing is not valid JSON'); }
  if (!record(inventory) || inventory.schema !== 1 || !Array.isArray(inventory.cases)) invalid('authoritative test listing has an unsupported shape');
  return { inventory: inventory as UnitTestInventory, stdout: result.stdout };
}

async function safeReadJson(filename: string): Promise<{ value: unknown; bytes: Buffer }> {
  const fileStat = await lstat(filename);
  if (!fileStat.isFile() || fileStat.isSymbolicLink() || fileStat.size > JSON_INPUT_LIMIT_BYTES) invalid(`input file is not a bounded regular file: ${filename}`);
  const bytes = await readFile(filename);
  try { return { value: JSON.parse(bytes.toString('utf8')), bytes }; } catch { invalid(`input file is not valid JSON: ${filename}`); }
}

async function assertNewDirectory(directory: string, forbiddenDirectories: readonly string[] = []): Promise<void> {
  const target = resolve(directory);
  if (!directory || !directory.startsWith('/') || target !== directory) invalid('output directory path must be absolute and canonical');
  const parent = dirname(target);
  const parentStat = await lstat(parent);
  if (!parentStat.isDirectory() || parentStat.isSymbolicLink()) invalid('output parent must be a real directory');
  const physicalTarget = join(await realpath(parent), basename(target));
  for (const forbidden of forbiddenDirectories) {
    const protectedPath = await realpath(forbidden);
    if (physicalTarget === protectedPath || physicalTarget.startsWith(`${protectedPath}/`)) invalid('output directory aliases a protected input');
  }
  await mkdir(target, { mode: 0o700 });
}

async function writeExclusiveJson(filename: string, value: unknown): Promise<Buffer> {
  const content = Buffer.from(jsonText(value));
  if (content.byteLength > JSON_INPUT_LIMIT_BYTES) invalid(`output file exceeds ${JSON_INPUT_LIMIT_BYTES} bytes`);
  await writeFile(filename, content, { flag: 'wx', mode: 0o600 });
  return content;
}

export async function prepareUnitCiArtifacts(
  artifactDirectory: string,
  identity: RunIdentity,
  runner: CommandRunner = boundedCommand,
): Promise<{ plan: UnitCiExecutionPlan; matrix: string }> {
  validateIdentity(identity);
  const { inventory } = await runAuthoritativeInventory(runner);
  const manifest = validateUnitTestShardManifest(generateUnitTestShardManifest(inventory), inventory);
  const inventoryBytes = Buffer.from(jsonText(inventory));
  const manifestBytes = Buffer.from(jsonText(manifest));
  const plan = createUnitCiExecutionPlan(manifest, identity, sha256(inventoryBytes), sha256(manifestBytes));
  await assertNewDirectory(artifactDirectory);
  await writeExclusiveJson(join(artifactDirectory, 'inventory.json'), inventory);
  await writeExclusiveJson(join(artifactDirectory, 'manifest.json'), manifest);
  await writeExclusiveJson(join(artifactDirectory, 'plan.json'), plan);
  return { plan, matrix: groupMatrixOutput(plan) };
}

async function loadPreparedDirectory(
  preparedDirectory: string,
  identity: RunIdentity,
  runner: CommandRunner,
  budget?: PhaseBudget,
): Promise<{ inventory: UnitTestInventory; manifest: UnitTestShardManifest; plan: UnitCiExecutionPlan }> {
  const { inventory: actualInventory } = await runAuthoritativeInventory(runner, budget);
  const generated = validateUnitTestShardManifest(generateUnitTestShardManifest(actualInventory), actualInventory);
  const inventoryBytes = Buffer.from(jsonText(actualInventory));
  const manifestBytes = Buffer.from(jsonText(generated));
  const rootStat = await lstat(preparedDirectory);
  if (!rootStat.isDirectory() || rootStat.isSymbolicLink()) invalid('prepared artifact root must be a real directory');
  const rootReal = await realpath(preparedDirectory);
  const entries = await readdir(rootReal);
  if (!isDeepStrictEqual(entries.sort(), ['inventory.json', 'manifest.json', 'plan.json'])) invalid('prepared artifact file set is incomplete or unexpected');
  const storedInventory = await safeReadJson(join(rootReal, 'inventory.json'));
  const storedManifest = await safeReadJson(join(rootReal, 'manifest.json'));
  const storedPlan = await safeReadJson(join(rootReal, 'plan.json'));
  if (!isDeepStrictEqual(storedInventory.value, actualInventory)) invalid('prepared inventory differs from the authoritative source listing');
  const validatedManifest = validateUnitTestShardManifest(storedManifest.value, actualInventory);
  if (!isDeepStrictEqual(validatedManifest, generated)) invalid('prepared manifest differs from the validated deterministic manifest');
  if (sha256(inventoryBytes) !== sha256(storedInventory.bytes) || sha256(manifestBytes) !== sha256(storedManifest.bytes)) {
    invalid('prepared inventory or manifest file digest is stale');
  }
  const plan = validateUnitCiExecutionPlan(storedPlan.value, generated, identity, sha256(inventoryBytes), sha256(manifestBytes));
  return { inventory: actualInventory, manifest: generated, plan };
}

function getGroup(plan: UnitCiExecutionPlan, groupId: string): UnitCiGroup {
  if (!GROUP_ID_PATTERN.test(groupId)) invalid('group id has an invalid format');
  const group = plan.groups.find(({ id }) => id === groupId);
  if (!group) invalid(`group ${groupId} is not in the prepared matrix`);
  if (!group.shardIds.length || group.shardIds.length > GROUP_SHARD_LIMIT) invalid(`group ${groupId} has an invalid shard count`);
  return group;
}

function isoTimestamp(value: string): boolean {
  return typeof value === 'string' && Number.isFinite(Date.parse(value)) && new Date(value).toISOString() === value;
}

function receiptWithinGroupWindow(receipt: unknown, groupStarted: number, groupCompleted: number): boolean {
  if (!record(receipt) || typeof receipt.startedAt !== 'string' || !isoTimestamp(receipt.startedAt)
    || typeof receipt.durationMs !== 'number' || !Number.isSafeInteger(receipt.durationMs)
    || receipt.durationMs < 0 || receipt.durationMs > SHARD_HARD_DEADLINE_MS) return false;
  const started = Date.parse(receipt.startedAt);
  const completed = started + receipt.durationMs;
  return started >= groupStarted - GROUP_TIMESTAMP_TOLERANCE_MS
    && started <= groupCompleted + GROUP_TIMESTAMP_TOLERANCE_MS
    && completed <= groupCompleted + GROUP_TIMESTAMP_TOLERANCE_MS;
}

function acceptableCommandResult(receipt: UnitShardReceipt, shard: UnitTestShard, inventory: UnitTestInventory): boolean {
  if (receipt.command.args.length !== 4 || !isDeepStrictEqual(receipt.command.args, unitShardCommandArgs(shard.selection))) return false;
  if (receipt.command.binary !== process.execPath || !basename(receipt.command.binary).startsWith('bun')) return false;
  if (!isoTimestamp(receipt.startedAt) || !Number.isSafeInteger(receipt.durationMs) || receipt.durationMs < 0 || receipt.durationMs > SHARD_HARD_DEADLINE_MS) return false;
  const result = receipt.command.result;
  if (result) {
    const required = ['aborted', 'callerStderrCloseObserved', 'callerStderrNaturalEnd', 'callerStdoutCloseObserved', 'callerStdoutNaturalEnd', 'childExited', 'code', 'durationMs', 'managerStderrCloseObserved', 'managerStderrNaturalEnd', 'managerStdoutCloseObserved', 'managerStdoutNaturalEnd', 'outputLimitExceeded', 'ownedProcessesExited', 'stdioClosed', 'stderr', 'stderrBytes', 'stderrRetainedBytes', 'stderrSha256', 'storedStderrBytes', 'storedStderrSha256', 'storedStdoutBytes', 'storedStdoutSha256', 'stdout', 'stdoutBytes', 'stdoutRetainedBytes', 'stdoutSha256', 'targetCloseObserved', 'targetOutputRelayHealthy', 'targetStderrCloseObserved', 'targetStderrNaturalEnd', 'targetStdoutCloseObserved', 'targetStdoutNaturalEnd', 'timedOut'];
    const allowed = new Set([...required, 'launchError', 'signal', 'targetPID']);
    const keys = Object.keys(result);
    if (required.some((key) => !keys.includes(key)) || keys.some((key) => !allowed.has(key))) return false;
    if (typeof result.stdout !== 'string' || typeof result.stderr !== 'string'
      || !Number.isSafeInteger(result.stdoutBytes) || !Number.isSafeInteger(result.stderrBytes)
      || !Number.isSafeInteger(result.stdoutRetainedBytes) || !Number.isSafeInteger(result.stderrRetainedBytes)
      || !Number.isSafeInteger(result.storedStdoutBytes) || !Number.isSafeInteger(result.storedStderrBytes)
      || result.storedStdoutBytes !== Buffer.byteLength(result.stdout) || result.storedStderrBytes !== Buffer.byteLength(result.stderr)
      || result.storedStdoutSha256 !== sha256(result.stdout) || result.storedStderrSha256 !== sha256(result.stderr)
      || !Number.isSafeInteger(result.code) || !Number.isSafeInteger(result.durationMs)
      || typeof result.timedOut !== 'boolean' || typeof result.outputLimitExceeded !== 'boolean' || typeof result.aborted !== 'boolean'
      || typeof result.childExited !== 'boolean' || typeof result.targetCloseObserved !== 'boolean'
      || typeof result.ownedProcessesExited !== 'boolean' || typeof result.stdioClosed !== 'boolean'
      || !HASH_PATTERN.test(result.stdoutSha256) || !HASH_PATTERN.test(result.stderrSha256)) return false;
  }
  if (result && (result.stdoutBytes !== Buffer.byteLength(result.stdout) || result.stderrBytes !== Buffer.byteLength(result.stderr)
    || result.stdoutRetainedBytes !== Buffer.byteLength(result.stdout) || result.stderrRetainedBytes !== Buffer.byteLength(result.stderr)
    || result.stdoutBytes + result.stderrBytes > SHARD_OUTPUT_LIMIT_BYTES
    || result.stdoutSha256 !== sha256(result.stdout) || result.stderrSha256 !== sha256(result.stderr)
    || result.durationMs > receipt.durationMs || receipt.durationMs - result.durationMs > SHARD_RECEIPT_OVERHEAD_LIMIT_MS)) return false;
  const accounting = testOutputAccounting(
    inventory,
    shard.selection,
    receipt.command.result?.stdout || '',
    receipt.command.result?.stderr || '',
    receipt.command.result,
    receipt.command.rejected,
  );
  if (!isDeepStrictEqual(receipt.accounting, accounting)) return false;
  return accounting.accepted && receipt.command.result !== null && !receipt.command.launchError && !receipt.command.rejected;
}

function validateReceipt(
  input: unknown,
  groupId: string,
  shard: UnitTestShard,
  inventory: UnitTestInventory,
  manifest: UnitTestShardManifest,
  plan: UnitCiExecutionPlan,
): input is UnitShardReceipt {
  if (!record(input)) return false;
  const expected = {
    schema: 1,
    groupId,
    shardId: shard.id,
    selectedIds: shard.selection,
    ...receiptIdentity(plan, plan),
  };
  for (const [key, value] of Object.entries(expected)) if (!isDeepStrictEqual(input[key], value)) return false;
  if (!isDeepStrictEqual(Object.keys(input).sort(), ['accounting', 'attempt', 'command', 'durationMs', 'groupId', 'inventoryDigest', 'inventoryFileSha256', 'manifestFileSha256', 'planSha256', 'runId', 'schema', 'selectedIds', 'shardId', 'sourceSha', 'startedAt'].sort())) return false;
  const receipt = input as unknown as UnitShardReceipt;
  if (!record(receipt.command) || !record(receipt.accounting) || !Array.isArray(receipt.selectedIds)) return false;
  const commandKeys = Object.keys(receipt.command).sort();
  if (!isDeepStrictEqual(commandKeys, Object.hasOwn(receipt.command, 'launchError')
    ? ['args', 'binary', 'launchError', 'rejected', 'result'] : ['args', 'binary', 'rejected', 'result'])) return false;
  if (typeof receipt.command.binary !== 'string' || !Array.isArray(receipt.command.args)
    || receipt.command.args.some((value) => typeof value !== 'string') || typeof receipt.command.rejected !== 'boolean'
    || (Object.hasOwn(receipt.command, 'launchError') && typeof receipt.command.launchError !== 'string')) return false;
  if (receipt.command.result !== null && (!record(receipt.command.result) || typeof receipt.command.result.stdout !== 'string'
    || typeof receipt.command.result.stderr !== 'string' || Buffer.byteLength(receipt.command.result.stdout) > SHARD_OUTPUT_LIMIT_BYTES
    || Buffer.byteLength(receipt.command.result.stderr) > SHARD_OUTPUT_LIMIT_BYTES)) return false;
  return acceptableCommandResult(receipt, shard, inventory);
}

function groupArtifactName(groupId: string, attempt: string): string {
  return `unit-ci-${groupId}-try-${attempt}`;
}

function validateGroupRecord(
  group: unknown,
  artifact: UnitCiArtifactGroup,
  expectedGroup: UnitCiGroup,
  inventory: UnitTestInventory,
  manifest: UnitTestShardManifest,
  plan: UnitCiExecutionPlan,
): string[] {
  const errors: string[] = [];
  if (!record(group)) return ['group artifact has no group receipt'];
  const expectedGroupKeys = ['attempt', 'attemptedShardIds', 'completedAt', 'durationMs', 'groupId', 'inventoryDigest', 'inventoryFileSha256', 'manifestFileSha256', 'planSha256', 'receiptDigests', 'result', 'runId', 'schema', 'shardIds', 'sourceSha', 'startedAt'];
  if (!isDeepStrictEqual(Object.keys(group).sort(), expectedGroupKeys.sort())) errors.push(`group ${expectedGroup.id} has unexpected receipt fields`);
  if (artifact.artifactName !== groupArtifactName(expectedGroup.id, plan.attempt)) errors.push(`unexpected group artifact ${artifact.artifactName}`);
  const identity = receiptIdentity(plan, plan);
  for (const [key, value] of Object.entries({
    schema: 1,
    ...identity,
    groupId: expectedGroup.id,
    shardIds: expectedGroup.shardIds,
  })) if (!isDeepStrictEqual(group[key], value)) errors.push(`group ${expectedGroup.id} has mismatched ${key}`);
  if (!Array.isArray(group.attemptedShardIds) || !isDeepStrictEqual(group.attemptedShardIds, artifact.receipts.map(({ receipt }) => record(receipt) ? receipt.shardId : undefined))) {
    errors.push(`group ${expectedGroup.id} does not exactly enumerate its shard receipt files`);
  }
  if (group.result !== 'success') errors.push(`group ${expectedGroup.id} did not report success`);
  const groupStarted = typeof group.startedAt === 'string' && isoTimestamp(group.startedAt) ? Date.parse(group.startedAt) : NaN;
  const groupCompleted = typeof group.completedAt === 'string' && isoTimestamp(group.completedAt) ? Date.parse(group.completedAt) : NaN;
  const groupDuration = typeof group.durationMs === 'number' ? group.durationMs : NaN;
  const wallDuration = groupCompleted - groupStarted;
  if (!Number.isFinite(groupStarted) || !Number.isFinite(groupCompleted) || wallDuration < 0
    || !Number.isSafeInteger(groupDuration) || groupDuration < 0 || groupDuration > GROUP_DEADLINE_MS
    || wallDuration > GROUP_DEADLINE_MS + GROUP_TIMESTAMP_TOLERANCE_MS
    || Math.abs(wallDuration - groupDuration) > GROUP_TIMESTAMP_TOLERANCE_MS) {
    errors.push(`group ${expectedGroup.id} has invalid bounded timing evidence`);
  }
  const expectedDigests = artifact.receipts.map(({ receipt, sha256: digest }) => ({
    shardId: record(receipt) && typeof receipt.shardId === 'string' ? receipt.shardId : '',
    sha256: digest,
  }));
  if (!isDeepStrictEqual(group.receiptDigests, expectedDigests)) errors.push(`group ${expectedGroup.id} receipt digest set is invalid`);
  const expectedShards = new Map(manifest.shards.filter(({ id }) => expectedGroup.shardIds.includes(id)).map((shard) => [shard.id, shard]));
  const seen = new Set<string>();
  for (const { receipt } of artifact.receipts) {
    const shardId = record(receipt) && typeof receipt.shardId === 'string' ? receipt.shardId : '';
    const shard = expectedShards.get(shardId);
    if (!shard) {
      errors.push(`group ${expectedGroup.id} contains an unexpected shard receipt ${shardId || '(invalid)'}`);
      continue;
    }
    if (seen.has(shardId)) {
      errors.push(`group ${expectedGroup.id} contains a duplicate shard receipt ${shardId}`);
      continue;
    }
    seen.add(shardId);
    if (!receiptWithinGroupWindow(receipt, groupStarted, groupCompleted)) errors.push(`shard ${shardId} timing falls outside group ${expectedGroup.id}`);
    if (!validateReceipt(receipt, expectedGroup.id, shard, inventory, manifest, plan)) errors.push(`shard ${shardId} receipt did not pass outcome and output accounting`);
  }
  for (const shardId of expectedGroup.shardIds) if (!seen.has(shardId)) errors.push(`group ${expectedGroup.id} is missing shard receipt ${shardId}`);
  return errors;
}

export function validateUnitCiAggregate(input: UnitCiAggregateInput): UnitCiAggregateResult {
  const errors: string[] = [];
  let manifest: UnitTestShardManifest;
  let plan: UnitCiExecutionPlan;
  try {
    validateIdentity(input.identity);
    manifest = validateUnitTestShardManifest(input.manifest, input.inventory);
    const inventoryBytes = Buffer.from(jsonText(input.inventory));
    const manifestBytes = Buffer.from(jsonText(manifest));
    plan = validateUnitCiExecutionPlan(input.plan, manifest, input.identity, sha256(inventoryBytes), sha256(manifestBytes));
  } catch (error) {
    return { accepted: false, errors: [error instanceof Error ? error.message : String(error)], groups: 0, shards: 0 };
  }
  if (input.preparationResult !== 'success') errors.push(`preparation prerequisite result is ${input.preparationResult}`);
  if (input.groupsResult !== 'success') errors.push(`execution matrix prerequisite result is ${input.groupsResult}`);
  const expectedGroups = new Map(plan.groups.map((group) => [group.id, group]));
  const observedGroups = new Map<string, UnitCiArtifactGroup[]>();
  for (const artifact of input.artifactGroups) {
    const prefix = `unit-ci-`;
    const suffix = `-try-${plan.attempt}`;
    const artifactGroupId = artifact.artifactName.startsWith(prefix) && artifact.artifactName.endsWith(suffix)
      ? artifact.artifactName.slice(prefix.length, -suffix.length) : '';
    if (!expectedGroups.has(artifactGroupId)) {
      errors.push(`unexpected group artifact ${artifact.artifactName}`);
      continue;
    }
    const values = observedGroups.get(artifactGroupId) || [];
    values.push(artifact);
    observedGroups.set(artifactGroupId, values);
  }
  for (const [groupId, expectedGroup] of expectedGroups) {
    const artifacts = observedGroups.get(groupId) || [];
    if (artifacts.length !== 1) {
      errors.push(`expected exactly one artifact for group ${groupId}, found ${artifacts.length}`);
      continue;
    }
    errors.push(...validateGroupRecord(artifacts[0]!.group, artifacts[0]!, expectedGroup, input.inventory as UnitTestInventory, manifest, plan));
  }
  return { accepted: errors.length === 0, errors, groups: plan.groups.length, shards: plan.shardCount };
}

async function readArtifactDirectory(directory: string): Promise<{
  inventory: unknown;
  inventoryBytes: Buffer;
  manifest: unknown;
  manifestBytes: Buffer;
  plan: unknown;
  planBytes: Buffer;
}> {
  const directoryStat = await lstat(directory);
  if (!directoryStat.isDirectory() || directoryStat.isSymbolicLink()) invalid('preparation artifact is not a real directory');
  const root = await realpath(directory);
  const rootEntries = await readdir(root);
  if (!isDeepStrictEqual(rootEntries.sort(), ['inventory.json', 'manifest.json', 'plan.json'])) invalid('preparation artifact file set is incomplete or unexpected');
  const inventory = await safeReadJson(join(root, 'inventory.json'));
  const manifest = await safeReadJson(join(root, 'manifest.json'));
  const plan = await safeReadJson(join(root, 'plan.json'));
  return {
    inventory: inventory.value,
    inventoryBytes: inventory.bytes,
    manifest: manifest.value,
    manifestBytes: manifest.bytes,
    plan: plan.value,
    planBytes: plan.bytes,
  };
}

async function readReceiptArtifact(root: string, artifactName: string, expectedGroup: UnitCiGroup): Promise<UnitCiArtifactGroup> {
  if (!/^unit-ci-group-[a-z0-9-]+-try-[1-9][0-9]*$/u.test(artifactName)) invalid(`artifact name is invalid: ${artifactName}`);
  const directory = join(root, artifactName);
  const directoryStat = await lstat(directory);
  if (!directoryStat.isDirectory() || directoryStat.isSymbolicLink()) invalid(`group artifact is not a real directory: ${artifactName}`);
  const actualDirectory = await realpath(directory);
  if (dirname(actualDirectory) !== await realpath(root)) invalid(`group artifact path escapes its root: ${artifactName}`);
  const expectedNames = ['group.json', ...expectedGroup.shardIds.map((id) => `receipt-${id}.json`)].sort();
  const actualNames = (await readdir(actualDirectory)).sort();
  if (!isDeepStrictEqual(actualNames, expectedNames)) invalid(`group artifact ${artifactName} has missing or unexpected receipt files`);
  const group = (await safeReadJson(join(actualDirectory, 'group.json'))).value;
  const receipts: Array<{ receipt: unknown; sha256: string }> = [];
  for (const shardId of expectedGroup.shardIds) {
    const { value, bytes } = await safeReadJson(join(actualDirectory, `receipt-${shardId}.json`));
    receipts.push({ receipt: value, sha256: sha256(bytes) });
  }
  return { artifactName, group, receipts };
}

async function aggregateArtifacts(
  artifactRoot: string,
  identity: RunIdentity,
  preparationResult: string,
  groupsResult: string,
  runner: CommandRunner,
): Promise<UnitCiAggregateResult> {
  const { inventory: authoritativeInventory } = await runAuthoritativeInventory(runner);
  const manifest = validateUnitTestShardManifest(generateUnitTestShardManifest(authoritativeInventory), authoritativeInventory);
  const inventoryBytes = Buffer.from(jsonText(authoritativeInventory));
  const manifestBytes = Buffer.from(jsonText(manifest));
  const plan = createUnitCiExecutionPlan(manifest, identity, sha256(inventoryBytes), sha256(manifestBytes));
  let artifactInventory: unknown;
  let artifactManifest: unknown;
  let artifactPlan: unknown;
  const rootStat = await lstat(artifactRoot).catch(() => undefined);
  if (!rootStat || !rootStat.isDirectory() || rootStat.isSymbolicLink()) {
    return validateUnitCiAggregate({ inventory: authoritativeInventory, manifest, plan, artifactGroups: [], identity, preparationResult, groupsResult });
  }
  const root = await realpath(artifactRoot);
  const rootEntries = await readdir(root);
  const expectedNames = [
    `unit-ci-plan-try-${identity.attempt}`,
    ...plan.groups.map(({ id }) => groupArtifactName(id, identity.attempt)),
  ].sort();
  const unexpected = rootEntries.filter((name) => !expectedNames.includes(name));
  if (unexpected.length) invalid(`artifact download contains unexpected artifacts: ${unexpected.join(', ')}`);
  try {
    const prep = await readArtifactDirectory(join(root, `unit-ci-plan-try-${identity.attempt}`));
    artifactInventory = prep.inventory;
    artifactManifest = prep.manifest;
    artifactPlan = prep.plan;
    if (sha256(prep.inventoryBytes) !== sha256(inventoryBytes) || sha256(prep.manifestBytes) !== sha256(manifestBytes)) {
      invalid('preparation artifact inventory or manifest bytes are tampered');
    }
    if (sha256(prep.planBytes) !== sha256(jsonText(plan))) invalid('preparation artifact execution plan bytes are tampered');
  } catch (error) {
    const failure = error instanceof Error ? error.message : String(error);
    return { accepted: false, errors: [failure], groups: plan.groups.length, shards: plan.shardCount };
  }
  if (!isDeepStrictEqual(artifactInventory, authoritativeInventory)) invalid('downloaded inventory is stale or tampered');
  const validatedManifest = validateUnitTestShardManifest(artifactManifest, authoritativeInventory);
  const validatedPlan = validateUnitCiExecutionPlan(artifactPlan, validatedManifest, identity, sha256(inventoryBytes), sha256(manifestBytes));
  const artifacts: UnitCiArtifactGroup[] = [];
  for (const group of validatedPlan.groups) {
    const name = groupArtifactName(group.id, identity.attempt);
    if (!rootEntries.includes(name)) continue;
    artifacts.push(await readReceiptArtifact(root, name, group));
  }
  return validateUnitCiAggregate({
    inventory: authoritativeInventory,
    manifest: validatedManifest,
    plan: validatedPlan,
    artifactGroups: artifacts,
    identity,
    preparationResult,
    groupsResult,
  });
}

export async function executeUnitCiGroup(
  preparedDirectory: string,
  groupId: string,
  receiptDirectory: string,
  identity: RunIdentity,
  runner: CommandRunner = boundedCommand,
): Promise<{ group: UnitCiGroupReceipt; accepted: boolean }> {
  const groupStart = performance.now();
  const startedAt = new Date().toISOString();
  const budget = new PhaseBudget(`mobile unit ${groupId}`, { timeoutMs: GROUP_DEADLINE_MS, recoveryLimit: 0 });
  const { inventory, manifest, plan } = await loadPreparedDirectory(preparedDirectory, identity, runner, budget);
  const group = getGroup(plan, groupId);
  await assertNewDirectory(receiptDirectory, [preparedDirectory]);
  const receiptDigests: UnitCiGroupReceipt['receiptDigests'] = [];
  const attemptedShardIds: string[] = [];
  let accepted = true;
  for (const shardId of group.shardIds) {
    const shard = manifest.shards.find(({ id }) => id === shardId);
    if (!shard) invalid(`prepared group ${groupId} selected unknown shard ${shardId}`);
    const shardStartedAt = new Date().toISOString();
    const shardStart = performance.now();
    const args = unitShardCommandArgs(shard.selection);
    let command: UnitShardCommandInput;
    attemptedShardIds.push(shard.id);
    try {
      const result = await withUnitCommandEnvironment(() => runner(process.execPath, args, SHARD_HARD_DEADLINE_MS, {
        budget,
        label: `unit shard ${shard.id}`,
        maxBytes: SHARD_OUTPUT_LIMIT_BYTES,
        cleanupReservationMs: SHARD_HARD_DEADLINE_MS - SHARD_EXECUTION_LIMIT_MS,
      }));
      command = { binary: process.execPath, args, result };
    } catch (error) {
      if (error instanceof BoundedCommandError) command = { binary: process.execPath, args, result: error.result, rejected: true };
      else command = { binary: process.execPath, args, result: null, launchError: error instanceof Error ? error.message : String(error) };
    }
    const receipt = createUnitShardReceipt(inventory, manifest, plan, groupId, shard, command, shardStartedAt, Math.ceil(performance.now() - shardStart));
    const filename = join(receiptDirectory, `receipt-${shard.id}.json`);
    const bytes = await writeExclusiveJson(filename, receipt);
    receiptDigests.push({ shardId: shard.id, sha256: sha256(bytes) });
    accepted &&= receipt.accounting.accepted && !command.launchError;
  }
  const groupReceipt: UnitCiGroupReceipt = {
    schema: 1,
    ...receiptIdentity(plan, plan),
    groupId,
    shardIds: [...group.shardIds],
    attemptedShardIds,
    receiptDigests,
    startedAt,
    completedAt: new Date().toISOString(),
    durationMs: Math.ceil(performance.now() - groupStart),
    result: accepted && isDeepStrictEqual(attemptedShardIds, group.shardIds) ? 'success' : 'failure',
  };
  await writeExclusiveJson(join(receiptDirectory, 'group.json'), groupReceipt);
  return { group: groupReceipt, accepted: groupReceipt.result === 'success' };
}

function parseOptions(args: string[], allowed: readonly string[]): Record<string, string> {
  if (args.length !== allowed.length * 2) invalid(`expected exactly ${allowed.map((key) => `--${key}`).join(', ')}`);
  const result: Record<string, string> = {};
  for (let index = 0; index < args.length; index += 2) {
    const option = args[index]!;
    const value = args[index + 1]!;
    const key = option.startsWith('--') ? option.slice(2) : '';
    if (!allowed.includes(key) || key in result) invalid(`unexpected or duplicate option ${option}`);
    if (!value || value.startsWith('--') || value.includes('\0')) invalid(`option ${option} requires a safe value`);
    result[key] = value;
  }
  for (const key of allowed) if (!Object.hasOwn(result, key)) invalid(`required option --${key} is missing`);
  return result;
}

function runIdentityFrom(options: Record<string, string>): RunIdentity {
  const identity = { sourceSha: options['source-sha']!, runId: options['run-id']!, attempt: options.attempt! };
  validateIdentity(identity);
  return identity;
}

async function runCli(args: string[]): Promise<number> {
  const action = args[0];
  if (action === 'prepare') {
    const options = parseOptions(args.slice(1), ['artifact-dir', 'source-sha', 'run-id', 'attempt']);
    const { matrix } = await prepareUnitCiArtifacts(options['artifact-dir']!, runIdentityFrom(options));
    process.stdout.write(`matrix=${matrix}\n`);
    return 0;
  }
  if (action === 'execute') {
    const options = parseOptions(args.slice(1), ['prepared-dir', 'group-id', 'receipt-dir', 'source-sha', 'run-id', 'attempt']);
    const result = await executeUnitCiGroup(options['prepared-dir']!, options['group-id']!, options['receipt-dir']!, runIdentityFrom(options));
    process.stdout.write(`${JSON.stringify({ group: result.group.groupId, shards: result.group.attemptedShardIds.length, result: result.group.result })}\n`);
    return result.accepted ? 0 : 1;
  }
  if (action === 'aggregate') {
    const options = parseOptions(args.slice(1), ['artifact-dir', 'source-sha', 'run-id', 'attempt', 'prepare-result', 'groups-result']);
    const result = await aggregateArtifacts(options['artifact-dir']!, runIdentityFrom(options), options['prepare-result']!, options['groups-result']!, boundedCommand);
    process.stdout.write(`${JSON.stringify(result)}\n`);
    return result.accepted ? 0 : 1;
  }
  invalid('usage: test-shard-ci.ts <prepare | execute | aggregate>');
}

if (import.meta.main) {
  try {
    process.exitCode = await runCli(process.argv.slice(2));
  } catch (error) {
    process.stderr.write(`unit shard CI: ${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 2;
  }
}
