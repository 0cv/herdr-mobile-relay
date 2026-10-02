import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { link, mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { stableTestIdentities } from './test-registry';
import {
  generateUnitTestShardManifest,
  validateUnitTestShardManifest,
  type UnitTestInventory,
  type UnitTestShardManifest,
} from './test-shards';

const wrapperName = 'iOS recorded publication, installation and navigation protocol regressions';
const cliPath = fileURLToPath(new URL('./test-shards.ts', import.meta.url));

type TestOutcome = void | string;

function inventoryWith(outerNames: readonly string[], innerNames: readonly string[] = ['iOS ordinary registration']): UnitTestInventory {
  const outer = stableTestIdentities('outer', [...outerNames, wrapperName]);
  const wrapper = outer.find(({ name }) => name === wrapperName)!;
  const inner = stableTestIdentities('ios-inner', innerNames).map((entry) => ({ ...entry, parentId: wrapper.id }));
  return { schema: 1, cases: [...outer, ...inner] };
}

function clone<T>(value: T): T {
  return structuredClone(value);
}

function rejectsManifest(
  manifest: UnitTestShardManifest,
  inventory: unknown,
  expected: RegExp,
): void {
  assert.throws(() => validateUnitTestShardManifest(manifest, inventory), expected);
}

function callCLI(args: string[], extraEnvironment: NodeJS.ProcessEnv = {}) {
  const environment: NodeJS.ProcessEnv = { ...process.env, BUN_OPTIONS: '--no-env-file' };
  delete environment.MOBILE_UNIT_FILTER;
  delete environment.IOS_TEST_FILTER;
  Object.assign(environment, extraEnvironment);
  return spawnSync(process.execPath, ['--no-env-file', cliPath, ...args], {
    encoding: 'utf8',
    env: environment,
    timeout: 15_000,
    maxBuffer: 1_000_000,
    stdio: 'pipe',
  });
}

export const testShardTests: Array<[string, () => Promise<TestOutcome>]> = [
  ['unit shard generation preserves scope, registration order, aggregate accounting and singleton rules', async () => {
    const source = inventoryWith([
      'alpha first', 'Foxtrot first', 'golf first', 'Mike first', 'november first', 'Zulu first',
      '3rd-party title', 'Repeated display title', 'Repeated display title',
      'Android environment snapshot CLI fixture', 'apple second', 'bravo second', 'charlie second', 'delta second', 'echo second', 'foxtrot second', 'golf second',
    ], [
      'iOS ordinary one', 'iOS ordinary two', 'iOS ordinary three', 'iOS ordinary four', 'iOS ordinary five',
      'iOS Plan13 hierarchy full install fixture one', 'iOS ordinary six', 'iOS ordinary seven',
      'iOS Plan13 hierarchy full install fixture two',
    ]);
    const first = generateUnitTestShardManifest(source);
    const second = generateUnitTestShardManifest(source);
    assert.deepEqual(first, second);
    assert.equal(first.inventory.registrationCount, source.cases.length);
    assert.equal(first.inventory.leafCount, first.inventory.outerLeafCount + first.inventory.iosLeafCount);
    const wrapper = source.cases.find(({ name }) => name === wrapperName)!;
    assert.equal(first.inventory.aggregateId, wrapper.id);
    assert.equal(first.aggregate.id, wrapper.id);
    assert.equal(first.inventory.outerLeafCount, source.cases.filter(({ scope }) => scope === 'outer').length - 1);
    assert.equal(first.inventory.iosLeafCount, source.cases.filter(({ scope }) => scope === 'ios-inner').length);
    assert.equal(first.shards.some(({ selection }) => selection.includes(wrapper.id)), false);
    assert.equal(validateUnitTestShardManifest(first, source), first);

    const selectedIds = first.shards.flatMap(({ selection }) => selection);
    const expectedLeaves = source.cases.filter(({ id }) => id !== wrapper.id).map(({ id }) => id);
    assert.equal(selectedIds.length, expectedLeaves.length);
    assert.equal(new Set(selectedIds).size, selectedIds.length);
    assert.deepEqual([...new Set(selectedIds)].sort(), [...new Set(expectedLeaves)].sort());
    const repeated = source.cases.filter(({ name }) => name === 'Repeated display title');
    assert.equal(repeated.length, 2);
    assert.notEqual(repeated[0]!.id, repeated[1]!.id);
    assert.ok(first.shards.some(({ group, selection }) => group === 'outer-n-z' && selection.includes(repeated[0]!.id)));
    assert.ok(first.shards.some(({ group, selection }) => group === 'outer-n-z' && selection.includes(repeated[1]!.id)));
    assert.ok(first.shards.some(({ group, selection }) => group === 'outer-a-f' && selection.some((id) => source.cases.find((entry) => entry.id === id)?.name === 'alpha first')));
    assert.ok(first.shards.some(({ group, selection }) => group === 'outer-g-m' && selection.some((id) => source.cases.find((entry) => entry.id === id)?.name === 'golf first')));
    assert.ok(first.shards.some(({ group, selection }) => group === 'outer-n-z' && selection.some((id) => source.cases.find((entry) => entry.id === id)?.name === 'november first')));
    assert.ok(first.shards.some(({ group, selection }) => group === 'outer-non-letter' && selection.some((id) => source.cases.find((entry) => entry.id === id)?.name === '3rd-party title')));
    assert.equal(first.shards.find(({ selection }) => selection.includes(wrapper.id)), undefined);
    const long = source.cases.find(({ name }) => name === 'Android environment snapshot CLI fixture')!;
    assert.equal(first.shards.filter(({ selection }) => selection.includes(long.id)).length, 1);
    assert.deepEqual(first.shards.find(({ selection }) => selection.includes(long.id))!.selection, [long.id]);

    const innerIds = new Set(source.cases.filter(({ scope }) => scope === 'ios-inner').map(({ id }) => id));
    const plan13 = source.cases.filter(({ scope, name }) => scope === 'ios-inner' && name.startsWith('iOS Plan13 hierarchy full install '));
    for (const entry of plan13) {
      const shard = first.shards.find(({ selection }) => selection.includes(entry.id))!;
      assert.equal(shard.group, 'ios-inner');
      assert.deepEqual(shard.selection, [entry.id]);
    }
    for (const shard of first.shards) {
      const names = shard.selection.map((id) => source.cases.find((entry) => entry.id === id)!);
      const indexes = names.map((entry) => source.cases.indexOf(entry));
      assert.ok(indexes.every((value, index) => index === 0 || value > indexes[index - 1]!));
      assert.ok(shard.selection.length <= (shard.group === 'ios-inner' ? 4 : 8));
      if (shard.group === 'ios-inner') assert.ok(shard.selection.every((id) => innerIds.has(id)));
    }
    assert.equal(first.shards.some(({ group, selection }) => group === 'outer-g-m' && selection.some((id) => innerIds.has(id))), false);
  }],
  ['unit shard generation and validation reject malformed, incomplete or mis-parented inventories', async () => {
    const source = inventoryWith(['alpha case', 'bravo case'], ['iOS child one', 'iOS child two']);
    const valid = generateUnitTestShardManifest(source);
    const rejectInventory = (inventory: unknown, expected: RegExp) => assert.throws(() => generateUnitTestShardManifest(inventory), expected);
    rejectInventory({ schema: 1, cases: [] }, /at least one registration/u);
    rejectInventory({ schema: 2, cases: source.cases }, /schema must be 1/u);
    rejectInventory({ schema: 1, cases: source.cases, source: 'unknown' }, /unexpected source/u);
    rejectInventory({ schema: 1, cases: [{ ...source.cases[0], extra: true }, ...source.cases.slice(1)] }, /unexpected extra/u);
    rejectInventory({ schema: 1, cases: source.cases.map((entry) => entry.id === source.cases[0]!.id ? { ...entry, scope: 'unknown' } : entry) }, /unrecognized scope/u);
    rejectInventory({ schema: 1, cases: source.cases.map((entry) => entry.id === source.cases[0]!.id ? { ...entry, id: 'outer:wrong:1' } : entry) }, /identity does not match/u);
    rejectInventory({ schema: 1, cases: source.cases.map((entry, index) => index === 1 ? { ...entry, id: source.cases[0]!.id } : entry) }, /duplicate registration ids/u);
    rejectInventory({ schema: 1, cases: source.cases.map((entry) => entry.scope === 'outer' && entry.name === wrapperName ? { ...entry, parentId: 'bad-parent' } : entry) }, /outer registrations cannot have a parent/u);
    rejectInventory({ schema: 1, cases: source.cases.map((entry) => entry.scope === 'ios-inner' ? { ...entry, parentId: 'not-the-wrapper' } : entry) }, /invalid parent id/u);
    rejectInventory({ schema: 1, cases: source.cases.map((entry) => entry.scope === 'ios-inner' ? { id: entry.id, name: entry.name, scope: entry.scope } : entry) }, /invalid parent id/u);
    rejectInventory({ schema: 1, cases: [...source.cases.slice(0, 2), source.cases.at(-1), ...source.cases.slice(2, -2), source.cases.at(-2)] }, /outer registrations must precede/u);
    rejectInventory({ schema: 1, cases: source.cases.filter(({ name }) => name !== wrapperName) }, /exactly one iOS aggregate wrapper/u);
    const outerNames = [...source.cases.filter(({ scope }) => scope === 'outer').map(({ name }) => name), wrapperName];
    const repeatedWrappers = stableTestIdentities('outer', outerNames);
    const firstWrapperId = repeatedWrappers.find(({ name }) => name === wrapperName)!.id;
    const nestedCases = stableTestIdentities('ios-inner', source.cases.filter(({ scope }) => scope === 'ios-inner').map(({ name }) => name))
      .map((entry) => ({ ...entry, parentId: firstWrapperId }));
    rejectInventory({ schema: 1, cases: [...repeatedWrappers, ...nestedCases] }, /exactly one iOS aggregate wrapper/u);
    rejectInventory({ schema: 1, cases: source.cases.filter(({ scope }) => scope !== 'ios-inner') }, /must contain nested iOS/u);
    rejectInventory({ schema: 1, cases: source.cases.map((entry) => entry.id === source.cases[0]!.id ? { ...entry, name: ' alpha case ' } : entry) }, /invalid name/u);
    rejectInventory({ schema: 1, cases: source.cases.map((entry) => entry.id === source.cases[0]!.id ? { ...entry, name: '' } : entry) }, /invalid name/u);

    const misplaced = clone(source);
    const nested = misplaced.cases.find(({ scope }) => scope === 'ios-inner')!;
    nested.parentId = source.cases.find(({ scope, name }) => scope === 'outer' && name !== wrapperName)?.id;
    rejectsManifest(valid, misplaced, /invalid parent id/u);
  }],
  ['unit shard validation rejects malformed selections, stale digests and inconsistent manifest metadata', async () => {
    const source = inventoryWith([
      'alpha one', 'alpha two', 'alpha three', 'alpha four', 'alpha five', 'alpha six', 'alpha seven', 'alpha eight', 'alpha nine',
      'Android production CLI single', 'november case', '3rd-party case',
    ], [
      'iOS child one', 'iOS child two', 'iOS child three', 'iOS child four', 'iOS child five',
      'iOS Plan13 hierarchy full install fixture',
    ]);
    const base = generateUnitTestShardManifest(source);
    const outer = source.cases.find(({ name }) => name === wrapperName)!;
    const mutate = (change: (manifest: UnitTestShardManifest) => void) => {
      const result = clone(base);
      change(result);
      return result;
    };
    rejectsManifest(mutate((manifest) => { manifest.shards = []; }), source, /nonempty shards/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[0]!.selection = []; }), source, /is empty/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[0]!.selection.push(manifest.shards[0]!.selection[0]!); }), source, /duplicate selection/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[0]!.selection[0] = 'unknown:selection:id'; }), source, /unknown or non-leaf/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[0]!.selection[0] = outer.id; }), source, /aggregate iOS wrapper/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[0]!.selection.pop(); }), source, /exactly cover/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[0]!.group = 'ios-inner'; }), source, /wrong shard group/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[0]!.group = 'unknown-group'; }), source, /unrecognized group/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[0]!.selection.reverse(); }), source, /registration order/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[1]!.selection.push(manifest.shards[0]!.selection[0]!); }), source, /duplicated across shards/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[1]!.id = manifest.shards[0]!.id; }), source, /shard id .* duplicated/u);
    rejectsManifest(mutate((manifest) => { manifest.shards[0]!.id = 'changed-shard-id'; }), source, /contains an unknown shard id/u);
    rejectsManifest(mutate((manifest) => { manifest.inventory.sha256 = '0'.repeat(64); }), source, /digest or identity counts/u);
    rejectsManifest(mutate((manifest) => { manifest.inventory.leafCount += 1; }), source, /digest or identity counts/u);
    rejectsManifest(mutate((manifest) => { (manifest.configuration as unknown as { outerMaxCasesPerShard: number }).outerMaxCasesPerShard = 99; }), source, /configuration is unsupported/u);
    rejectsManifest(mutate((manifest) => { manifest.aggregate.id = 'wrong-wrapper'; }), source, /aggregate wrapper metadata/u);
    rejectsManifest(mutate((manifest) => { (manifest as unknown as { schema: number }).schema = 2; }), source, /schema must be 1/u);
    rejectsManifest(mutate((manifest) => { (manifest as unknown as Record<string, unknown>).unknown = true; }), source, /unexpected unknown/u);
    rejectsManifest(mutate((manifest) => { manifest.batches = []; }), source, /nonempty batches/u);
    rejectsManifest(mutate((manifest) => { manifest.batches[0]!.shardIds = []; }), source, /batch .* is empty/u);
    rejectsManifest(mutate((manifest) => { manifest.batches[0]!.shardIds.push('unknown-shard'); }), source, /contains an unknown shard id/u);
    rejectsManifest(mutate((manifest) => { manifest.batches[0]!.shardIds.push(manifest.batches[0]!.shardIds[0]!); }), source, /duplicated across batches/u);
    rejectsManifest(mutate((manifest) => { manifest.batches[0]!.id = 'changed-batch'; }), source, /deterministic plan/u);

    const stale = inventoryWith([...source.cases.filter(({ scope, name }) => scope === 'outer' && name !== wrapperName).map(({ name }) => name), 'Zulu added after manifest']);
    rejectsManifest(base, stale, /digest or identity counts/u);
    const misparented = clone(source);
    const inner = misparented.cases.find(({ scope }) => scope === 'ios-inner')!;
    inner.parentId = 'outer:wrong-parent:1';
    rejectsManifest(base, misparented, /invalid parent id/u);
  }],
  ['unit shard batches stay below the matrix expansion limit and split deterministically', async () => {
    const outerNames = Array.from({ length: 2_049 }, (_value, index) => `A generated case ${String(index).padStart(4, '0')}`);
    const source = inventoryWith(outerNames);
    const first = generateUnitTestShardManifest(source);
    const second = generateUnitTestShardManifest(source);
    assert.deepEqual(first, second);
    assert.equal(first.batches.length, 2);
    assert.equal(first.batches[0]!.shardIds.length, 256);
    assert.equal(first.batches[1]!.shardIds.length, 2);
    assert.ok(first.batches.every(({ shardIds }) => shardIds.length > 0 && shardIds.length <= 256));
    const oversized = clone(first);
    oversized.batches = [{ id: 'batch-001', shardIds: oversized.shards.map(({ id }) => id) }];
    assert.throws(() => validateUnitTestShardManifest(oversized, source), /exceeds the matrix expansion limit/u);
  }],
  ['unit shard CLI requires inputs and provides repeatable generation, validation and explicit selections', async () => {
    const root = await mkdtemp(join(tmpdir(), 'herdr-unit-test-shards-'));
    const inventoryPath = join(root, 'inventory.json');
    const firstPath = join(root, 'first.json');
    const secondPath = join(root, 'second.json');
    const source = inventoryWith(['Android cheap shard control', 'iOS wrapper must remain aggregate'], ['iOS cheap nested shard control']);
    await writeFile(inventoryPath, `${JSON.stringify(source)}\n`);
    const originalInventory = await readFile(inventoryPath);
    const rejectOutputAlias = async (outputPath: string, expected: RegExp) => {
      const result = callCLI(['generate', '--inventory', inventoryPath, '--output', outputPath]);
      assert.equal(result.error, undefined);
      assert.equal(result.status, 2);
      assert.match(result.stderr, expected);
      assert.deepEqual(await readFile(inventoryPath), originalInventory);
    };
    try {
      const missing = callCLI([]);
      assert.equal(missing.error, undefined);
      assert.equal(missing.status, 2);
      assert.equal(missing.stdout, '');
      assert.match(missing.stderr, /usage:/u);
      const missingOutput = callCLI(['generate', '--inventory', inventoryPath]);
      assert.equal(missingOutput.status, 2);
      assert.match(missingOutput.stderr, /expected exactly/u);
      const absentInventory = callCLI(['generate', '--inventory', join(root, 'absent.json'), '--output', firstPath]);
      assert.equal(absentInventory.status, 2);
      assert.match(absentInventory.stderr, /cannot read/u);
      await rejectOutputAlias(inventoryPath, /output aliases the authoritative inventory/u);

      const symlinkAlias = join(root, 'inventory-symlink.json');
      await symlink(inventoryPath, symlinkAlias, 'file');
      await rejectOutputAlias(symlinkAlias, /symbolic link/u);

      const symlinkDirectory = join(root, 'directory-symlink');
      await symlink(root, symlinkDirectory, 'dir');
      await rejectOutputAlias(join(symlinkDirectory, 'inventory.json'), /output aliases the authoritative inventory/u);

      const hardlinkAlias = join(root, 'inventory-hardlink.json');
      await link(inventoryPath, hardlinkAlias);
      await rejectOutputAlias(hardlinkAlias, /output aliases the authoritative inventory/u);

      const first = callCLI(['generate', '--inventory', inventoryPath, '--output', firstPath]);
      const second = callCLI(['generate', '--inventory', inventoryPath, '--output', secondPath]);
      assert.equal(first.status, 0, first.stderr);
      assert.equal(second.status, 0, second.stderr);
      assert.equal(await readFile(firstPath, 'utf8'), await readFile(secondPath, 'utf8'));
      const existingManifest = await readFile(firstPath);
      const overwrite = callCLI(['generate', '--inventory', inventoryPath, '--output', firstPath]);
      assert.equal(overwrite.status, 2);
      assert.match(overwrite.stderr, /output already exists/u);
      assert.deepEqual(await readFile(firstPath), existingManifest);
      const validation = callCLI(['validate', '--inventory', inventoryPath, '--manifest', firstPath]);
      assert.equal(validation.status, 0, validation.stderr);
      assert.equal(JSON.parse(validation.stdout).valid, true);
      const manifest = JSON.parse(await readFile(firstPath, 'utf8')) as UnitTestShardManifest;
      const shard = manifest.shards.find(({ group }) => group === 'outer-a-f')!;
      const selection = callCLI(['select', '--inventory', inventoryPath, '--manifest', firstPath, '--shard-id', shard.id], {
        MOBILE_UNIT_FILTER: '(?!)', IOS_TEST_FILTER: '(?!)',
      });
      assert.equal(selection.status, 0, selection.stderr);
      assert.equal(selection.stdout.trim(), shard.selection.join(','));
      const unknownShard = callCLI(['select', '--inventory', inventoryPath, '--manifest', firstPath, '--shard-id', 'missing-shard']);
      assert.equal(unknownShard.status, 2);
      assert.match(unknownShard.stderr, /unknown shard id/u);
      const badManifestPath = join(root, 'bad.json');
      await writeFile(badManifestPath, JSON.stringify({ schema: 1, cases: [] }));
      const invalidManifest = callCLI(['validate', '--inventory', inventoryPath, '--manifest', badManifestPath]);
      assert.equal(invalidManifest.status, 2);
      assert.match(invalidManifest.stderr, /manifest keys are invalid/u);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }],
];
