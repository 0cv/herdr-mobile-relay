import { createHash } from 'node:crypto';
import { lstat, readFile, realpath, stat, writeFile } from 'node:fs/promises';
import { basename, dirname, join, resolve } from 'node:path';
import { isDeepStrictEqual } from 'node:util';
import { stableTestIdentities, type TestCaseIdentity } from './test-registry';

export type UnitTestCase = TestCaseIdentity & { parentId?: string };
export type UnitTestInventory = { schema: 1; cases: UnitTestCase[] };
export type UnitTestShard = { id: string; group: string; selection: string[] };
export type UnitTestShardManifest = {
  schema: 1;
  configuration: typeof GROUPING_CONFIGURATION;
  inventory: {
    sha256: string;
    registrationCount: number;
    outerLeafCount: number;
    iosLeafCount: number;
    leafCount: number;
    aggregateId: string;
  };
  aggregate: {
    id: string;
    scope: 'outer';
    childScope: 'ios-inner';
    accounting: 'all nested leaf IDs exactly once; wrapper is not a shard leaf';
  };
  shards: UnitTestShard[];
  batches: Array<{ id: string; shardIds: string[] }>;
};

const IOS_WRAPPER_NAME = 'iOS recorded publication, installation and navigation protocol regressions';
const PLAN13_SINGLETON_PREFIX = 'iOS Plan13 hierarchy full install ';
const OUTER_SINGLETON_PREFIXES = [
  'Android environment snapshot CLI ',
  'Android environment snapshot persists ',
  'Android production CLI retained initial and warm relaunch ',
  'the workflow provenance adapter ',
  'unit runner listing and selection interface ',
  'unit runner explicit selection matches default filtered execution',
] as const;
const GROUPING_CONFIGURATION = {
  algorithm: 'unit-test-shards-v1',
  outerPartition: 'ASCII-letter A-F, G-M, N-Z, and non-letter; exclude the iOS aggregate wrapper',
  outerMaxCasesPerShard: 8,
  outerSingletonPrefixes: OUTER_SINGLETON_PREFIXES,
  iosInnerMaxCasesPerShard: 4,
  iosInnerSingletonPrefix: PLAN13_SINGLETON_PREFIX,
  matrixExpansionLimit: 256,
  runtimeEvidence: 'unmeasured',
} as const;
const OUTER_GROUPS = [
  { id: 'outer-a-f', accepts: (letter: string | undefined) => letter !== undefined && letter >= 'A' && letter <= 'F' },
  { id: 'outer-g-m', accepts: (letter: string | undefined) => letter !== undefined && letter >= 'G' && letter <= 'M' },
  { id: 'outer-n-z', accepts: (letter: string | undefined) => letter !== undefined && letter >= 'N' && letter <= 'Z' },
  { id: 'outer-non-letter', accepts: (letter: string | undefined) => letter === undefined },
] as const;
const RECOGNIZED_GROUPS = new Set([...OUTER_GROUPS.map(({ id }) => id), 'ios-inner']);

function invalid(message: string): never {
  throw new Error(`UNIT_SHARDS: ${message}`);
}

function record(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function requireExactKeys(value: Record<string, unknown>, required: readonly string[], optional: readonly string[], label: string): void {
  const allowed = new Set([...required, ...optional]);
  const missing = required.filter((key) => !(key in value));
  const unexpected = Object.keys(value).filter((key) => !allowed.has(key));
  if (missing.length || unexpected.length) {
    invalid(`${label} keys are invalid${missing.length ? `; missing ${missing.join(', ')}` : ''}${unexpected.length ? `; unexpected ${unexpected.join(', ')}` : ''}`);
  }
}

function normalizeInventory(input: unknown): UnitTestInventory {
  if (!record(input)) invalid('inventory must be an object');
  requireExactKeys(input, ['schema', 'cases'], [], 'inventory');
  if (input.schema !== 1) invalid('inventory schema must be 1');
  if (!Array.isArray(input.cases) || input.cases.length === 0) invalid('inventory must contain at least one registration');

  const cases = input.cases.map((entry, index): UnitTestCase => {
    if (!record(entry)) invalid(`inventory case ${index} must be an object`);
    requireExactKeys(entry, ['id', 'name', 'scope'], ['parentId'], `inventory case ${index}`);
    if (typeof entry.id !== 'string' || !entry.id) invalid(`inventory case ${index} has an invalid id`);
    if (typeof entry.name !== 'string' || !entry.name.trim() || entry.name !== entry.name.trim() || /[\0\r\n]/u.test(entry.name)) {
      invalid(`inventory case ${index} has an invalid name`);
    }
    if (entry.scope !== 'outer' && entry.scope !== 'ios-inner') invalid(`inventory case ${index} has an unrecognized scope`);
    if ('parentId' in entry && (typeof entry.parentId !== 'string' || !entry.parentId)) {
      invalid(`inventory case ${index} has an invalid parent id`);
    }
    return {
      id: entry.id,
      name: entry.name,
      scope: entry.scope,
      ...('parentId' in entry ? { parentId: entry.parentId as string } : {}),
    };
  });

  const ids = cases.map(({ id }) => id);
  if (new Set(ids).size !== ids.length) invalid('inventory contains duplicate registration ids');
  let foundInner = false;
  for (const entry of cases) {
    if (entry.scope === 'ios-inner') foundInner = true;
    else if (foundInner) invalid('outer registrations must precede nested iOS registrations');
  }

  for (const scope of ['outer', 'ios-inner'] as const) {
    const selected = cases.filter((entry) => entry.scope === scope);
    const expected = stableTestIdentities(scope, selected.map(({ name }) => name));
    for (const [index, entry] of selected.entries()) {
      if (entry.id !== expected[index]!.id) invalid(`${scope} registration identity does not match its name and registration order`);
      if (scope === 'outer' && 'parentId' in entry) invalid('outer registrations cannot have a parent id');
    }
  }

  const outer = cases.filter((entry) => entry.scope === 'outer');
  const nested = cases.filter((entry) => entry.scope === 'ios-inner');
  const wrappers = outer.filter(({ name }) => name === IOS_WRAPPER_NAME);
  if (wrappers.length !== 1) invalid('inventory must contain exactly one iOS aggregate wrapper');
  if (!nested.length) invalid('inventory must contain nested iOS registrations');
  const wrapperId = wrappers[0]!.id;
  for (const entry of nested) {
    if (entry.parentId !== wrapperId) invalid(`nested iOS registration ${entry.id} has an invalid parent id`);
  }
  return { schema: 1, cases };
}

function canonicalInventory(inventory: UnitTestInventory): string {
  return JSON.stringify({
    schema: 1,
    cases: inventory.cases.map(({ id, name, scope, parentId }) => ({
      id,
      name,
      scope,
      ...(parentId === undefined ? {} : { parentId }),
    })),
  });
}

export function unitTestInventoryDigest(input: unknown): string {
  const inventory = normalizeInventory(input);
  return createHash('sha256').update(canonicalInventory(inventory)).digest('hex');
}

function wrapperCase(inventory: UnitTestInventory): UnitTestCase {
  return inventory.cases.find(({ scope, name }) => scope === 'outer' && name === IOS_WRAPPER_NAME)!;
}

function letterGroup(name: string): string | undefined {
  const first = name.match(/^[A-Za-z]/u)?.[0];
  return first?.toUpperCase();
}

function outerGroup(caseEntry: UnitTestCase): string {
  const letter = letterGroup(caseEntry.name);
  return OUTER_GROUPS.find(({ accepts }) => accepts(letter))!.id;
}

function shardId(group: string, selection: readonly string[], ordinal: number): string {
  const digest = createHash('sha256').update(`${group}\0${selection.join('\0')}`).digest('hex').slice(0, 12);
  return `${group}-${String(ordinal).padStart(3, '0')}-${digest}`;
}

function createShards(inventory: UnitTestInventory): UnitTestShard[] {
  const wrapperId = wrapperCase(inventory).id;
  const shards: UnitTestShard[] = [];
  const ordinals = new Map<string, number>();
  const append = (group: string, selection: string[]) => {
    const ordinal = (ordinals.get(group) || 0) + 1;
    ordinals.set(group, ordinal);
    shards.push({ id: shardId(group, selection, ordinal), group, selection });
  };

  const outer = inventory.cases.filter(({ scope, id }) => scope === 'outer' && id !== wrapperId);
  for (const { id: group } of OUTER_GROUPS) {
    let pending: UnitTestCase[] = [];
    const flush = () => {
      if (!pending.length) return;
      append(group, pending.map(({ id }) => id));
      pending = [];
    };
    for (const entry of outer) {
      if (outerGroup(entry) !== group) continue;
      const singleton = OUTER_SINGLETON_PREFIXES.some((prefix) => entry.name.startsWith(prefix));
      if (singleton) {
        flush();
        append(group, [entry.id]);
        continue;
      }
      pending.push(entry);
      if (pending.length === GROUPING_CONFIGURATION.outerMaxCasesPerShard) flush();
    }
    flush();
  }

  let pendingIOS: UnitTestCase[] = [];
  const flushIOS = () => {
    if (!pendingIOS.length) return;
    append('ios-inner', pendingIOS.map(({ id }) => id));
    pendingIOS = [];
  };
  for (const entry of inventory.cases.filter(({ scope }) => scope === 'ios-inner')) {
    if (entry.name.startsWith(PLAN13_SINGLETON_PREFIX)) {
      flushIOS();
      append('ios-inner', [entry.id]);
      continue;
    }
    pendingIOS.push(entry);
    if (pendingIOS.length === GROUPING_CONFIGURATION.iosInnerMaxCasesPerShard) flushIOS();
  }
  flushIOS();
  return shards;
}

function makeBatches(shards: readonly UnitTestShard[]): Array<{ id: string; shardIds: string[] }> {
  const batches: Array<{ id: string; shardIds: string[] }> = [];
  for (let offset = 0; offset < shards.length; offset += GROUPING_CONFIGURATION.matrixExpansionLimit) {
    const ordinal = batches.length + 1;
    batches.push({
      id: `batch-${String(ordinal).padStart(3, '0')}`,
      shardIds: shards.slice(offset, offset + GROUPING_CONFIGURATION.matrixExpansionLimit).map(({ id }) => id),
    });
  }
  return batches;
}

export function generateUnitTestShardManifest(input: unknown): UnitTestShardManifest {
  const inventory = normalizeInventory(input);
  const wrapper = wrapperCase(inventory);
  const shards = createShards(inventory);
  if (!shards.length) invalid('generation produced no nonempty shards');
  const outerLeafCount = inventory.cases.filter(({ scope, id }) => scope === 'outer' && id !== wrapper.id).length;
  const iosLeafCount = inventory.cases.filter(({ scope }) => scope === 'ios-inner').length;
  return {
    schema: 1,
    configuration: structuredClone(GROUPING_CONFIGURATION),
    inventory: {
      sha256: createHash('sha256').update(canonicalInventory(inventory)).digest('hex'),
      registrationCount: inventory.cases.length,
      outerLeafCount,
      iosLeafCount,
      leafCount: outerLeafCount + iosLeafCount,
      aggregateId: wrapper.id,
    },
    aggregate: {
      id: wrapper.id,
      scope: 'outer',
      childScope: 'ios-inner',
      accounting: 'all nested leaf IDs exactly once; wrapper is not a shard leaf',
    },
    shards,
    batches: makeBatches(shards),
  };
}

function validateManifestShape(input: unknown): asserts input is UnitTestShardManifest {
  if (!record(input)) invalid('manifest must be an object');
  requireExactKeys(input, ['schema', 'configuration', 'inventory', 'aggregate', 'shards', 'batches'], [], 'manifest');
  if (input.schema !== 1) invalid('manifest schema must be 1');
  if (!isDeepStrictEqual(input.configuration, GROUPING_CONFIGURATION)) invalid('manifest grouping configuration is unsupported or conflicting');
  if (!record(input.inventory)) invalid('manifest inventory descriptor must be an object');
  requireExactKeys(input.inventory, ['sha256', 'registrationCount', 'outerLeafCount', 'iosLeafCount', 'leafCount', 'aggregateId'], [], 'manifest inventory descriptor');
  if (!record(input.aggregate)) invalid('manifest aggregate descriptor must be an object');
  requireExactKeys(input.aggregate, ['id', 'scope', 'childScope', 'accounting'], [], 'manifest aggregate descriptor');
  if (!Array.isArray(input.shards) || input.shards.length === 0) invalid('manifest must contain nonempty shards');
  if (!Array.isArray(input.batches) || input.batches.length === 0) invalid('manifest must contain nonempty batches');
}

export function validateUnitTestShardManifest(manifestInput: unknown, authoritativeInventoryInput: unknown): UnitTestShardManifest {
  const inventory = normalizeInventory(authoritativeInventoryInput);
  validateManifestShape(manifestInput);
  const manifest = manifestInput;
  const expected = generateUnitTestShardManifest(inventory);
  if (!isDeepStrictEqual(manifest.inventory, expected.inventory)) invalid('manifest inventory digest or identity counts do not match the authoritative inventory');
  if (!isDeepStrictEqual(manifest.aggregate, expected.aggregate)) invalid('manifest aggregate wrapper metadata does not match the authoritative inventory');

  const registrations = new Map(inventory.cases.map((entry, index) => [entry.id, { entry, index }]));
  const wrapperId = expected.aggregate.id;
  const selected = new Set<string>();
  const shardIds = new Set<string>();
  const leafIds = new Set(inventory.cases.filter(({ id }) => id !== wrapperId).map(({ id }) => id));
  for (const [index, shardValue] of manifest.shards.entries()) {
    if (!record(shardValue)) invalid(`shard ${index} must be an object`);
    requireExactKeys(shardValue, ['id', 'group', 'selection'], [], `shard ${index}`);
    if (typeof shardValue.id !== 'string' || !shardValue.id) invalid(`shard ${index} has an invalid id`);
    if (shardIds.has(shardValue.id)) invalid(`shard id ${shardValue.id} is duplicated`);
    shardIds.add(shardValue.id);
    if (typeof shardValue.group !== 'string' || !RECOGNIZED_GROUPS.has(shardValue.group)) invalid(`shard ${index} has an unrecognized group`);
    if (!Array.isArray(shardValue.selection) || shardValue.selection.length === 0) invalid(`shard ${shardValue.id} is empty`);
    const local = new Set<string>();
    let priorIndex = -1;
    for (const id of shardValue.selection) {
      if (typeof id !== 'string' || !id) invalid(`shard ${shardValue.id} contains an invalid selection`);
      if (local.has(id)) invalid(`shard ${shardValue.id} contains a duplicate selection`);
      local.add(id);
      if (id === wrapperId) invalid('the aggregate iOS wrapper cannot be scheduled as a leaf');
      const registration = registrations.get(id);
      if (!registration || !leafIds.has(id)) invalid(`shard ${shardValue.id} selects an unknown or non-leaf id`);
      if (selected.has(id)) invalid(`leaf id ${id} is duplicated across shards`);
      selected.add(id);
      if (registration.entry.scope === 'ios-inner' && registration.entry.parentId !== wrapperId) invalid(`nested leaf id ${id} has an invalid parent`);
      const expectedGroup = registration.entry.scope === 'ios-inner' ? 'ios-inner' : outerGroup(registration.entry);
      if (shardValue.group !== expectedGroup) invalid(`leaf id ${id} is assigned to the wrong shard group`);
      if (registration.index <= priorIndex) invalid(`shard ${shardValue.id} selection is not in registration order`);
      priorIndex = registration.index;
    }
    const nestedSingleton = shardValue.selection.some((id) => registrations.get(id)!.entry.name.startsWith(PLAN13_SINGLETON_PREFIX));
    const outerSingleton = shardValue.selection.some((id) => OUTER_SINGLETON_PREFIXES.some((prefix) => registrations.get(id)!.entry.name.startsWith(prefix)));
    const limit = shardValue.group === 'ios-inner' ? GROUPING_CONFIGURATION.iosInnerMaxCasesPerShard : GROUPING_CONFIGURATION.outerMaxCasesPerShard;
    if (shardValue.selection.length > limit) invalid(`shard ${shardValue.id} exceeds its configured case limit`);
    if ((nestedSingleton || outerSingleton) && shardValue.selection.length !== 1) invalid(`shard ${shardValue.id} combines a singleton case with other leaves`);
  }
  if (selected.size !== leafIds.size || [...leafIds].some((id) => !selected.has(id))) invalid('shard selections do not exactly cover all authoritative leaf IDs');

  const batched = new Set<string>();
  const batchIds = new Set<string>();
  for (const [index, batchValue] of manifest.batches.entries()) {
    if (!record(batchValue)) invalid(`batch ${index} must be an object`);
    requireExactKeys(batchValue, ['id', 'shardIds'], [], `batch ${index}`);
    if (typeof batchValue.id !== 'string' || !batchValue.id) invalid(`batch ${index} has an invalid id`);
    if (batchIds.has(batchValue.id)) invalid(`batch id ${batchValue.id} is duplicated`);
    batchIds.add(batchValue.id);
    if (!Array.isArray(batchValue.shardIds) || batchValue.shardIds.length === 0) invalid(`batch ${batchValue.id} is empty`);
    if (batchValue.shardIds.length > GROUPING_CONFIGURATION.matrixExpansionLimit) invalid(`batch ${batchValue.id} exceeds the matrix expansion limit`);
    for (const id of batchValue.shardIds) {
      if (typeof id !== 'string' || !shardIds.has(id)) invalid(`batch ${batchValue.id} contains an unknown shard id`);
      if (batched.has(id)) invalid(`shard ${id} is duplicated across batches`);
      batched.add(id);
    }
  }
  if (batched.size !== shardIds.size || [...shardIds].some((id) => !batched.has(id))) invalid('batches do not exactly cover all shard IDs');
  if (!isDeepStrictEqual(manifest, expected)) invalid('manifest is not the deterministic plan for the authoritative inventory');
  return manifest;
}

async function assertSafeManifestOutput(inventoryPath: string, outputPath: string): Promise<void> {
  const resolvedInventory = resolve(inventoryPath);
  const resolvedOutput = resolve(outputPath);
  if (resolvedInventory === resolvedOutput) invalid('output aliases the authoritative inventory');

  const inventoryRealPath = await realpath(inventoryPath);
  const inventoryStat = await stat(inventoryRealPath);
  let outputDirectory: string;
  try {
    outputDirectory = await realpath(dirname(resolvedOutput));
  } catch (error) {
    invalid(`cannot resolve output directory: ${error instanceof Error ? error.message : String(error)}`);
  }
  if (join(outputDirectory!, basename(resolvedOutput)) === inventoryRealPath) invalid('output aliases the authoritative inventory');

  try {
    const outputStat = await lstat(resolvedOutput);
    if (outputStat.isSymbolicLink()) invalid('output must not be a symbolic link');
    if (outputStat.dev === inventoryStat.dev && outputStat.ino === inventoryStat.ino) invalid('output aliases the authoritative inventory');
    invalid('output already exists; refusing to overwrite');
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error;
  }
}

type CommandOptions = Record<string, string>;

function parseCommandOptions(args: string[], allowed: readonly string[]): CommandOptions {
  if (args.length !== allowed.length * 2) invalid(`expected exactly ${allowed.map((option) => `--${option}`).join(', ')}`);
  const options: CommandOptions = {};
  for (let index = 0; index < args.length; index += 2) {
    const flag = args[index]!;
    const value = args[index + 1]!;
    if (!flag.startsWith('--') || !allowed.includes(flag.slice(2))) invalid(`unexpected option ${flag}`);
    const key = flag.slice(2);
    if (key in options) invalid(`option --${key} is duplicated`);
    if (!value || value.startsWith('--')) invalid(`option --${key} requires a value`);
    options[key] = value;
  }
  if (allowed.some((key) => !(key in options))) invalid(`required option missing: ${allowed.filter((key) => !(key in options)).map((key) => `--${key}`).join(', ')}`);
  return options;
}

async function readJson(path: string): Promise<unknown> {
  let source: string;
  try {
    source = await readFile(path, 'utf8');
  } catch (error) {
    invalid(`cannot read ${path}: ${error instanceof Error ? error.message : String(error)}`);
  }
  try {
    return JSON.parse(source!);
  } catch (error) {
    invalid(`cannot parse ${path}: ${error instanceof Error ? error.message : String(error)}`);
  }
}

async function runCLI(args: string[]): Promise<void> {
  const action = args[0];
  if (action === 'generate') {
    const options = parseCommandOptions(args.slice(1), ['inventory', 'output']);
    const inventory = await readJson(options.inventory!);
    const manifest = generateUnitTestShardManifest(inventory);
    await assertSafeManifestOutput(options.inventory!, options.output!);
    await writeFile(options.output!, `${JSON.stringify(manifest, null, 2)}\n`, { flag: 'wx' });
    process.stdout.write(`${JSON.stringify({ generated: options.output, shards: manifest.shards.length, batches: manifest.batches.length, inventorySha256: manifest.inventory.sha256 })}\n`);
    return;
  }
  if (action === 'validate') {
    const options = parseCommandOptions(args.slice(1), ['inventory', 'manifest']);
    const inventory = await readJson(options.inventory!);
    const manifest = validateUnitTestShardManifest(await readJson(options.manifest!), inventory);
    process.stdout.write(`${JSON.stringify({ valid: true, inventorySha256: manifest.inventory.sha256, registrationCount: manifest.inventory.registrationCount, leafCount: manifest.inventory.leafCount, shardCount: manifest.shards.length, batchCount: manifest.batches.length })}\n`);
    return;
  }
  if (action === 'select') {
    const options = parseCommandOptions(args.slice(1), ['inventory', 'manifest', 'shard-id']);
    const inventory = await readJson(options.inventory!);
    const manifest = validateUnitTestShardManifest(await readJson(options.manifest!), inventory);
    const shard = manifest.shards.find(({ id }) => id === options['shard-id']);
    if (!shard) invalid(`unknown shard id ${options['shard-id']}`);
    process.stdout.write(`${shard.selection.join(',')}\n`);
    return;
  }
  invalid('usage: test-shards.ts <generate --inventory FILE --output FILE | validate --inventory FILE --manifest FILE | select --inventory FILE --manifest FILE --shard-id ID>');
}

if (import.meta.main) {
  try {
    await runCLI(process.argv.slice(2));
  } catch (error) {
    process.stderr.write(`test shards: ${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 2;
  }
}
