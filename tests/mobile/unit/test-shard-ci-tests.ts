import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, readFile, readdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { BoundedCommandError, boundedCommand, type BoundedCommandResult } from '../support/bounded-process';
import { redactText } from '../support/diagnostics';
import { repositoryRoot } from '../support/paths';
import { stableTestIdentities } from './test-registry';
import { createUnitTestOutputContract } from './test-output';
import {
  createUnitCiExecutionPlan,
  createUnitShardReceipt,
  executeUnitCiGroup,
  expectedSuccessMultiplicity,
  groupMatrixOutput,
  groupUnitTestShards,
  prepareUnitCiArtifacts,
  unitShardCommandArgs,
  validateUnitCiAggregate,
  validateUnitCiExecutionPlan,
  type UnitCiArtifactGroup,
  type UnitCiExecutionPlan,
  type UnitShardReceipt,
} from './test-shard-ci';
import { generateUnitTestShardManifest, type UnitTestInventory, type UnitTestShardManifest } from './test-shards';

const wrapperName = 'iOS recorded publication, installation and navigation protocol regressions';
const identity = { sourceSha: 'a'.repeat(40), runId: '123456', attempt: '2' };

type Fixture = { inventory: UnitTestInventory; manifest: UnitTestShardManifest; plan: UnitCiExecutionPlan };

function makeInventory(names = ['Android fixture alpha', 'Android fixture beta', wrapperName]): UnitTestInventory {
  const outer = stableTestIdentities('outer', names);
  const wrapper = outer.find(({ name }) => name === wrapperName)!;
  const inner = stableTestIdentities('ios-inner', ['iOS fixture inner one', 'iOS fixture inner two'])
    .map((entry) => ({ ...entry, parentId: wrapper.id }));
  return { schema: 1, cases: [...outer, ...inner] };
}

function jsonText(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function hash(value: string): string {
  return createHash('sha256').update(value).digest('hex');
}

async function fromRepositoryRoot<T>(operation: () => Promise<T>): Promise<T> {
  const previous = process.cwd();
  process.chdir(repositoryRoot);
  try {
    return await operation();
  } finally {
    process.chdir(previous);
  }
}

function fixture(inventory = makeInventory()): Fixture {
  const manifest = generateUnitTestShardManifest(inventory);
  const plan = createUnitCiExecutionPlan(manifest, identity, hash(jsonText(inventory)), hash(jsonText(manifest)));
  return { inventory, manifest, plan };
}

function resultFor(stdout: string, options: Partial<BoundedCommandResult> = {}): BoundedCommandResult {
  const stderr = options.stderr || '';
  return {
    stdout, stderr, code: 0, durationMs: 5, timedOut: false, outputLimitExceeded: false, aborted: false,
    childExited: true, targetPID: 123, targetCloseObserved: true,
    targetStdoutNaturalEnd: true, targetStdoutCloseObserved: true, targetStderrNaturalEnd: true, targetStderrCloseObserved: true,
    managerStdoutNaturalEnd: true, managerStdoutCloseObserved: true, managerStderrNaturalEnd: true, managerStderrCloseObserved: true,
    callerStdoutNaturalEnd: true, callerStdoutCloseObserved: true, callerStderrNaturalEnd: true, callerStderrCloseObserved: true,
    targetOutputRelayHealthy: true, ownedProcessesExited: true, stdioClosed: true,
    stdoutBytes: Buffer.byteLength(stdout), stderrBytes: Buffer.byteLength(stderr),
    stdoutRetainedBytes: Buffer.byteLength(stdout), stderrRetainedBytes: Buffer.byteLength(stderr),
    stdoutSha256: hash(stdout), stderrSha256: hash(stderr), ...options,
  };
}

function shardOutput(inventory: UnitTestInventory, shardSelection: readonly string[]): string {
  return `${createUnitTestOutputContract(inventory, shardSelection).events.map(({ line }) => line).join('\n')}\n`;
}

function makeGroupArtifacts(current: Fixture): UnitCiArtifactGroup[] {
  return current.plan.groups.map((group) => {
    const receipts = group.shardIds.map((shardId) => {
      const shard = current.manifest.shards.find(({ id }) => id === shardId)!;
      const output = shardOutput(current.inventory, shard.selection);
      const receipt = createUnitShardReceipt(
        current.inventory,
        current.manifest,
        current.plan,
        group.id,
        shard,
        { binary: process.execPath, args: ['--no-env-file', 'tests/mobile/unit/run.ts', '--select-tests', shard.selection.join(',')], result: resultFor(output) },
        '2026-01-01T00:00:00.000Z',
        5,
      );
      return { receipt, sha256: hash(jsonText(receipt)) };
    });
    return {
      artifactName: `unit-ci-${group.id}-try-${current.plan.attempt}`,
      group: {
        schema: 1,
        sourceSha: current.plan.sourceSha,
        runId: current.plan.runId,
        attempt: current.plan.attempt,
        inventoryDigest: current.plan.inventoryDigest,
        inventoryFileSha256: current.plan.inventoryFileSha256,
        manifestFileSha256: current.plan.manifestFileSha256,
        planSha256: current.plan.sha256,
        groupId: group.id,
        shardIds: group.shardIds,
        attemptedShardIds: group.shardIds,
        receiptDigests: receipts.map(({ receipt, sha256: digest }) => ({ shardId: receipt.shardId, sha256: digest })),
        startedAt: '2026-01-01T00:00:00.000Z',
        completedAt: '2026-01-01T00:00:00.005Z',
        durationMs: 5,
        result: 'success',
      },
      receipts,
    };
  });
}

function aggregateInput(current: Fixture, artifactGroups = makeGroupArtifacts(current)) {
  return {
    inventory: current.inventory,
    manifest: current.manifest,
    plan: current.plan,
    artifactGroups,
    identity,
    preparationResult: 'success',
    groupsResult: 'success',
  };
}

function replaceGroupReceipt(artifacts: UnitCiArtifactGroup[], groupIndex: number, receiptIndex: number, mutate: (receipt: any) => void): UnitCiArtifactGroup[] {
  const changed = structuredClone(artifacts);
  const receipt = changed[groupIndex]!.receipts[receiptIndex]!.receipt as any;
  mutate(receipt);
  changed[groupIndex]!.receipts[receiptIndex]!.sha256 = hash(jsonText(receipt));
  const group = changed[groupIndex]!.group as any;
  group.receiptDigests[receiptIndex]!.sha256 = changed[groupIndex]!.receipts[receiptIndex]!.sha256;
  return changed;
}

function syntheticListingRunner(
  inventory: UnitTestInventory,
  outcome: 'success' | 'nonzero' | 'timeout' | 'cancel' | 'skip' | 'rejected-success-shaped' = 'success',
  onSelection?: () => void,
) {
  let firstShard = true;
  return async (binary: string, args: string[], _timeout: number): Promise<BoundedCommandResult> => {
    if (args.includes('--list-tests')) return resultFor(`${JSON.stringify(inventory)}\n`);
    onSelection?.();
    const selected = args.at(-1)!.split(',');
    let stdout = shardOutput(inventory, selected);
    const options: Partial<BoundedCommandResult> = {};
    if (firstShard) {
      firstShard = false;
      if (outcome === 'nonzero') options.code = 9;
      if (outcome === 'timeout') { options.code = 1; options.timedOut = true; }
      if (outcome === 'cancel') { options.code = 1; options.aborted = true; }
      if (outcome === 'skip') {
        const skipped = inventory.cases.find(({ id }) => id === selected[0])!;
        stdout = stdout.replace(`ok - ${skipped.name}\n`, `ok - ${skipped.name} # SKIP fixture skip\n`);
      }
    }
    const result = resultFor(stdout, { ...options, durationMs: 0 });
    if (firstShard === false && outcome === 'rejected-success-shaped' && args.includes('--select-tests')) {
      throw new BoundedCommandError(binary, args, result);
    }
    return result;
  };
}

export const testShardCiTests: Array<[string, () => Promise<void>]> = [
  ['unit CI run and attempt identities require positive decimal values while accepting local numeric IDs', async () => {
    const current = fixture();
    const local = { ...identity, runId: '1', attempt: '1' };
    assert.equal(createUnitCiExecutionPlan(current.manifest, local, current.plan.inventoryFileSha256, current.plan.manifestFileSha256).runId, '1');
    for (const invalid of ['local', '0', '-1', '1.0', '+1', '01', ' 1']) {
      assert.throws(() => createUnitCiExecutionPlan(current.manifest, { ...identity, runId: invalid }, current.plan.inventoryFileSha256, current.plan.manifestFileSha256), /run id must be a positive decimal integer/u, invalid);
      assert.throws(() => createUnitCiExecutionPlan(current.manifest, { ...identity, attempt: invalid }, current.plan.inventoryFileSha256, current.plan.manifestFileSha256), /run attempt must be a positive decimal integer/u, invalid);
    }
    assert.throws(() => createUnitCiExecutionPlan(current.manifest, { ...identity, sourceSha: 'g'.repeat(40) }, current.plan.inventoryFileSha256, current.plan.manifestFileSha256), /full lowercase Git SHA/u);
  }],
  ['unit CI plan groups every authoritative shard once within four leaves and matrix limits', async () => {
    const shards = Array.from({ length: 19 }, (_value, index) => ({ id: `shard-${index + 1}`, group: 'outer-a-f', selection: [`leaf-${index + 1}`] }));
    const groups = groupUnitTestShards(shards);
    assert.deepEqual(groups.map(({ shardIds }) => shardIds.length), [4, 4, 4, 4, 3]);
    assert.equal(new Set(groups.map(({ id }) => id)).size, groups.length);
    assert.deepEqual(groups.flatMap(({ shardIds }) => shardIds), shards.map(({ id }) => id));
    assert.throws(() => groupUnitTestShards([]), /cannot be empty/u);
    assert.throws(() => groupUnitTestShards(Array.from({ length: 1_025 }, (_value, index) => ({ id: `s-${index}`, group: 'outer-a-f', selection: [`l-${index}`] }))), /exceeding 256/u);
    assert.throws(() => groupUnitTestShards([{ id: 'same', group: 'outer-a-f', selection: ['one'] }, { id: 'same', group: 'outer-a-f', selection: ['two'] }]), /unique/u);
  }],
  ['unit CI matrix and plan are deterministic and reject source, digest, run, attempt and selection drift', async () => {
    const current = fixture();
    const clone = structuredClone(current.plan);
    assert.deepEqual(current.plan, createUnitCiExecutionPlan(current.manifest, identity, current.plan.inventoryFileSha256, current.plan.manifestFileSha256));
    assert.equal(groupMatrixOutput(current.plan), JSON.stringify({ include: current.plan.groups.map(({ id }) => ({ group_id: id })) }));
    assert.equal(validateUnitCiExecutionPlan(current.plan, current.manifest, identity, current.plan.inventoryFileSha256, current.plan.manifestFileSha256).sha256, current.plan.sha256);
    for (const mutate of [
      (plan: any) => { plan.sourceSha = 'b'.repeat(40); },
      (plan: any) => { plan.runId = '654321'; },
      (plan: any) => { plan.attempt = '3'; },
      (plan: any) => { plan.inventoryDigest = '0'.repeat(64); },
      (plan: any) => { plan.manifestFileSha256 = '1'.repeat(64); },
      (plan: any) => { plan.groups[0].shardIds.pop(); },
      (plan: any) => { plan.groups[0].shardIds = []; },
      (plan: any) => { plan.groups[0].id = '../bad'; },
      (plan: any) => { plan.groups.push(structuredClone(plan.groups[0])); },
    ]) {
      const changed = structuredClone(clone);
      mutate(changed);
      assert.throws(() => validateUnitCiExecutionPlan(changed, current.manifest, identity, current.plan.inventoryFileSha256, current.plan.manifestFileSha256), /authoritative source-bound plan/u);
    }
    assert.throws(() => createUnitCiExecutionPlan(current.manifest, { ...identity, sourceSha: 'short' }, current.plan.inventoryFileSha256, current.plan.manifestFileSha256), /full lowercase Git SHA/u);
  }],
  ['unit CI preparation rejects more than 256 groups instead of truncating the matrix', async () => {
    const shards = Array.from({ length: 1_025 }, (_value, index) => ({ id: `shard-${index}`, group: 'outer-a-f', selection: [`leaf-${index}`] }));
    assert.throws(() => groupUnitTestShards(shards), /exceeding 256/u);
  }],
  ['unit CI aggregate accepts complete current artifacts and exact selected iOS wrapper multiplicity', async () => {
    const current = fixture();
    const result = validateUnitCiAggregate(aggregateInput(current));
    assert.deepEqual(result, { accepted: true, errors: [], groups: current.plan.groups.length, shards: current.plan.shardCount });
    const innerShard = current.manifest.shards.find(({ group }) => group === 'ios-inner')!;
    assert.deepEqual(expectedSuccessMultiplicity(current.inventory, current.manifest, innerShard.selection), {
      'iOS fixture inner one': 1,
      'iOS fixture inner two': 1,
      [wrapperName]: 1,
    });
  }],
  ['unit CI aggregate rejects missing, duplicate, extra groups and exact-set shard omissions or duplicates', async () => {
    const current = fixture();
    const baseline = makeGroupArtifacts(current);
    const missing = validateUnitCiAggregate(aggregateInput(current, baseline.slice(1)));
    assert.equal(missing.accepted, false);
    assert.ok(missing.errors.some((error) => error.includes('exactly one artifact')));
    const duplicate = validateUnitCiAggregate(aggregateInput(current, [...baseline, structuredClone(baseline[0]!) ]));
    assert.ok(duplicate.errors.some((error) => error.includes('exactly one artifact')));
    const extra = validateUnitCiAggregate(aggregateInput(current, [...baseline, { ...structuredClone(baseline[0]!), artifactName: 'unit-ci-group-999-try-2' }]));
    assert.ok(extra.errors.some((error) => error.includes('unexpected group artifact')));
    const missingShard = structuredClone(baseline);
    missingShard[0]!.receipts.pop();
    assert.ok(validateUnitCiAggregate(aggregateInput(current, missingShard)).errors.some((error) => error.includes('missing shard receipt')));
    const duplicateShard = structuredClone(baseline);
    duplicateShard[0]!.receipts.push(structuredClone(duplicateShard[0]!.receipts[0]!));
    assert.ok(validateUnitCiAggregate(aggregateInput(current, duplicateShard)).errors.some((error) => error.includes('receipt files')));
  }],
  ['unit CI aggregate rejects altered commit, run, attempt and inventory or manifest digests', async () => {
    const current = fixture();
    for (const mutate of [
      (plan: any) => { plan.sourceSha = 'b'.repeat(40); },
      (plan: any) => { plan.runId = '7654321'; },
      (plan: any) => { plan.attempt = '5'; },
      (plan: any) => { plan.inventoryFileSha256 = '0'.repeat(64); },
      (plan: any) => { plan.manifestFileSha256 = '1'.repeat(64); },
    ]) {
      const altered = structuredClone(current.plan);
      mutate(altered);
      assert.equal(validateUnitCiAggregate({ ...aggregateInput(current), plan: altered }).accepted, false);
    }
    const badReceipt = replaceGroupReceipt(makeGroupArtifacts(current), 0, 0, (receipt) => { receipt.sourceSha = 'b'.repeat(40); });
    assert.equal(validateUnitCiAggregate(aggregateInput(current, badReceipt)).accepted, false);
  }],
  ['unit CI aggregate rejects inconsistent group and shard timing windows', async () => {
    const current = fixture();
    const baseline = makeGroupArtifacts(current);
    const lateCompletion = structuredClone(baseline);
    (lateCompletion[0]!.group as any).completedAt = '2026-01-02T00:00:00.000Z';
    assert.equal(validateUnitCiAggregate(aggregateInput(current, lateCompletion)).accepted, false);
    const inconsistentDuration = structuredClone(baseline);
    (inconsistentDuration[0]!.group as any).durationMs = 60_000;
    assert.equal(validateUnitCiAggregate(aggregateInput(current, inconsistentDuration)).accepted, false);
    const shardStartedBeforeGroup = replaceGroupReceipt(baseline, 0, 0, (receipt) => { receipt.startedAt = '2025-12-31T23:00:00.000Z'; });
    assert.equal(validateUnitCiAggregate(aggregateInput(current, shardStartedBeforeGroup)).accepted, false);
    const shardCompletedAfterGroup = replaceGroupReceipt(baseline, 0, 0, (receipt) => { receipt.durationMs = 5_000; });
    assert.equal(validateUnitCiAggregate(aggregateInput(current, shardCompletedAfterGroup)).accepted, false);
    const commandOutlastsReceipt = replaceGroupReceipt(baseline, 0, 0, (receipt) => { receipt.command.result.durationMs = 170_000; });
    assert.equal(validateUnitCiAggregate(aggregateInput(current, commandOutlastsReceipt)).accepted, false);
  }],
  ['unit CI aggregate fails closed on preparation or matrix failure, cancellation and skipped groups', async () => {
    const current = fixture();
    for (const result of ['failure', 'cancelled', 'skipped', 'unknown']) {
      assert.equal(validateUnitCiAggregate({ ...aggregateInput(current), preparationResult: result }).accepted, false);
      assert.equal(validateUnitCiAggregate({ ...aggregateInput(current), groupsResult: result }).accepted, false);
    }
  }],
  ['unit CI aggregation rejects every failed process state and artifact success with failing tests', async () => {
    const current = fixture();
    const baseline = makeGroupArtifacts(current);
    for (const change of [
      (receipt: any) => { receipt.command.rejected = true; },
      (receipt: any) => { receipt.command.result.code = 4; },
      (receipt: any) => { receipt.command.result.timedOut = true; },
      (receipt: any) => { receipt.command.result.aborted = true; },
      (receipt: any) => { receipt.command.result.signal = 'SIGTERM'; },
      (receipt: any) => { receipt.command.result.ownedProcessesExited = false; },
      (receipt: any) => { receipt.command.result.targetCloseObserved = false; },
      (receipt: any) => { receipt.command.result.outputLimitExceeded = true; },
      (receipt: any) => { receipt.command.result.targetOutputRelayHealthy = false; },
      (receipt: any) => { receipt.command.result.stdoutSha256 = '0'.repeat(64); },
      (receipt: any) => { receipt.command.result.stderrSha256 = '0'.repeat(64); },
      (receipt: any) => { receipt.command.result.durationMs = 180_001; },
      (receipt: any) => { receipt.command.result.durationMs = 170_000; },
      (receipt: any) => { receipt.command.binary = '/tmp/bun-malicious'; },
      (receipt: any) => { receipt.command.result.stdout = ''; receipt.command.result.stdoutBytes = 0; },
      (receipt: any) => { receipt.command.result.stdout = receipt.command.result.stdout.replace('ok - Android fixture alpha', 'not ok - Android fixture alpha'); },
      (receipt: any) => { receipt.command.result.stdout = receipt.command.result.stdout.replace('ok - Android fixture alpha', 'ok - Android fixture alpha # SKIP fixture'); },
      (receipt: any) => { receipt.command.result.stdout += 'ok - unexpected test\n'; },
    ]) {
      const changed = replaceGroupReceipt(baseline, 0, 0, change);
      assert.equal(validateUnitCiAggregate(aggregateInput(current, changed)).accepted, false);
    }
    for (const field of [
      'childExited', 'targetCloseObserved', 'targetStdoutNaturalEnd', 'targetStdoutCloseObserved', 'targetStderrNaturalEnd', 'targetStderrCloseObserved',
      'managerStdoutNaturalEnd', 'managerStdoutCloseObserved', 'managerStderrNaturalEnd', 'managerStderrCloseObserved',
      'callerStdoutNaturalEnd', 'callerStdoutCloseObserved', 'callerStderrNaturalEnd', 'callerStderrCloseObserved',
      'targetOutputRelayHealthy', 'ownedProcessesExited', 'stdioClosed',
    ]) {
      const changed = replaceGroupReceipt(baseline, 0, 0, (receipt) => { receipt.command.result[field] = false; });
      assert.equal(validateUnitCiAggregate(aggregateInput(current, changed)).accepted, false, `${field} lifecycle evidence`);
    }
  }],
  ['unit CI adapter sends real outer and nested ids through the P01 explicit-selection interface', async () => {
    const listing = await fromRepositoryRoot(() => boundedCommand(process.execPath, ['--no-env-file', 'tests/mobile/unit/run.ts', '--list-tests'], 15_000, {
      maxBytes: 4 * 1024 * 1024,
      cleanupReservationMs: 1_000,
    }));
    const inventory = JSON.parse(listing.stdout) as UnitTestInventory;
    const manifest = generateUnitTestShardManifest(inventory);
    const sourceFile = Buffer.from(jsonText(inventory));
    const manifestFile = Buffer.from(jsonText(manifest));
    const plan = createUnitCiExecutionPlan(manifest, identity, hash(sourceFile.toString()), hash(manifestFile.toString()));
    const selected = [
      inventory.cases.find(({ scope, name }) => scope === 'outer' && name === 'unit runner registration sentinel')!,
      inventory.cases.find(({ scope, name }) => scope === 'ios-inner' && name === 'iOS recorded initial publication waits passively for the same page before actual document binding')!,
    ];
    assert.ok(selected.every(Boolean));
    for (const entry of selected) {
      const shard = manifest.shards.find(({ selection }) => selection.includes(entry.id))!;
      const groupId = plan.groups.find(({ shardIds }) => shardIds.includes(shard.id))!.id;
      const args = unitShardCommandArgs([entry.id]);
      const command = await fromRepositoryRoot(() => boundedCommand(process.execPath, args, 15_000, { maxBytes: 2 * 1024 * 1024, cleanupReservationMs: 1_000 }));
      const receipt = createUnitShardReceipt(inventory, manifest, plan, groupId, { ...shard, selection: [entry.id] },
        { binary: process.execPath, args, result: command }, new Date().toISOString(), command.durationMs);
      assert.equal(receipt.accounting.accepted, true, entry.name);
      assert.deepEqual(receipt.accounting.missingSuccesses, []);
      assert.deepEqual(receipt.accounting.unexpectedSuccesses, []);
    }
  }],
  ['unit CI receipts preserve a bounded redaction-sensitive authoritative success title', async () => {
    const listing = await fromRepositoryRoot(() => boundedCommand(process.execPath, ['--no-env-file', 'tests/mobile/unit/run.ts', '--list-tests'], 15_000, {
      maxBytes: 4 * 1024 * 1024,
      cleanupReservationMs: 1_000,
    }));
    const inventory = JSON.parse(listing.stdout) as UnitTestInventory;
    const entry = inventory.cases.find(({ name }) => redactText(`ok - ${name}`) !== `ok - ${name}`);
    assert.ok(entry, 'authoritative listing has a success title changed by generic secret redaction');
    const line = `ok - ${entry.name}\n`;
    assert.notEqual(redactText(line), line);
    const manifest = generateUnitTestShardManifest(inventory);
    const plan = createUnitCiExecutionPlan(manifest, identity, hash(jsonText(inventory)), hash(jsonText(manifest)));
    const shard = manifest.shards.find(({ selection }) => selection.includes(entry.id))!;
    const groupId = plan.groups.find(({ shardIds }) => shardIds.includes(shard.id))!.id;
    const args = unitShardCommandArgs([entry.id]);
    const startedAt = new Date().toISOString();
    const command = await fromRepositoryRoot(() => boundedCommand(process.execPath, args, 60_000, { maxBytes: 2 * 1024 * 1024, cleanupReservationMs: 3_000 }));
    assert.equal(command.code, 0);
    assert.ok(command.stdout.includes(line));
    const receipt = createUnitShardReceipt(inventory, manifest, plan, groupId, { ...shard, selection: [entry.id] },
      { binary: process.execPath, args, result: command }, startedAt, command.durationMs);
    assert.ok(receipt.command.result?.stdout.includes(line));
    assert.equal(receipt.accounting.actualSuccesses[entry.name], 1);
    assert.deepEqual(receipt.accounting.missingSuccesses, []);
    assert.equal(receipt.accounting.accepted, true);
  }],
  ['unit CI output accounting retains source-owned diagnostics and rejects arbitrary supplementary lines', async () => {
    const inventory = makeInventory(['Android retained post-attachment and response-shape refusals', wrapperName]);
    const current = fixture(inventory);
    const owner = inventory.cases.find(({ name }) => name === 'Android retained post-attachment and response-shape refusals')!;
    const shard = current.manifest.shards.find(({ selection }) => selection.includes(owner.id))!;
    const group = current.plan.groups.find(({ shardIds }) => shardIds.includes(shard.id))!;
    const output = shardOutput(inventory, shard.selection);
    const receipt = createUnitShardReceipt(inventory, current.manifest, current.plan, group.id, shard,
      { binary: process.execPath, args: unitShardCommandArgs(shard.selection), result: resultFor(output) },
      '2026-01-01T00:00:00.000Z', 5);
    assert.equal(receipt.accounting.expectedDiagnostics.length, 42);
    assert.equal(receipt.accounting.actualDiagnostics.length, 42);
    assert.equal(receipt.accounting.actualDiagnostics[0]!.ownerId, owner.id);
    assert.equal(receipt.accounting.actualDiagnostics[0]!.sourceFile, 'android-retained-responses.ts');
    assert.equal(receipt.accounting.accepted, true);
    const alteredOutput = `${output}fixture diagnostic only\n`;
    const altered = createUnitShardReceipt(inventory, current.manifest, current.plan, group.id, shard,
      { binary: process.execPath, args: unitShardCommandArgs(shard.selection), result: resultFor(alteredOutput) },
      '2026-01-01T00:00:00.000Z', 5);
    assert.deepEqual(altered.accounting.diagnostics.stdout.slice(-1), ['[REDACTED UNEXPECTED OUTPUT]']);
    assert.equal(altered.accounting.accepted, false);
    assert.ok(altered.accounting.outputErrors.length > 0);
    const newlineOnlyStderr = createUnitShardReceipt(inventory, current.manifest, current.plan, group.id, shard,
      {
        binary: process.execPath,
        args: unitShardCommandArgs(shard.selection),
        result: resultFor(output, { stderr: '\n', stderrBytes: 1, stderrRetainedBytes: 1, stderrSha256: hash('\n') }),
      }, '2026-01-01T00:00:00.000Z', 5);
    assert.equal(newlineOnlyStderr.command.result!.stdout, output);
    assert.equal(newlineOnlyStderr.command.result!.stderr, '[REDACTED UNEXPECTED OUTPUT]\n');
    assert.equal(newlineOnlyStderr.accounting.diagnostics.stderr[0], '[REDACTED UNEXPECTED OUTPUT]');
    assert.ok(newlineOnlyStderr.accounting.actualDiagnostics.some(({ line, stream }) =>
      line === '[REDACTED UNEXPECTED OUTPUT]' && stream === 'stderr'));
    assert.equal(newlineOnlyStderr.accounting.accepted, false);
    assert.ok(newlineOnlyStderr.accounting.outputErrors.some((error) => error.includes('stderr contains output')));
  }],
  ['unit CI aggregate recomputes diagnostic accounting after digest-consistent output tampering', async () => {
    const inventory = makeInventory(['Android retained post-attachment and response-shape refusals', wrapperName]);
    const current = fixture(inventory);
    const owner = inventory.cases.find(({ name }) => name === 'Android retained post-attachment and response-shape refusals')!;
    const shard = current.manifest.shards.find(({ selection }) => selection.includes(owner.id))!;
    const groupIndex = current.plan.groups.findIndex(({ shardIds }) => shardIds.includes(shard.id));
    const group = current.plan.groups[groupIndex]!;
    const receiptIndex = group.shardIds.indexOf(shard.id);
    const baseline = makeGroupArtifacts(current);
    assert.equal(validateUnitCiAggregate(aggregateInput(current, baseline)).accepted, true);
    const receipt = baseline[groupIndex]!.receipts[receiptIndex]!.receipt as UnitShardReceipt;
    const stdout = receipt.command.result!.stdout;
    const mutations = [
      stdout.replace('PASS retained response identity/http\n', ''),
      stdout.replace('PASS retained response identity/http\n', 'PASS retained response identity/http\nPASS retained response identity/http\n'),
      stdout.replace('PASS retained response identity/http\n', 'PASS retained response identity/http\nPASS unknown output\n'),
    ];
    for (const changedOutput of mutations) {
      const changedReceipt = createUnitShardReceipt(inventory, current.manifest, current.plan, group.id, shard,
        { binary: process.execPath, args: unitShardCommandArgs(shard.selection), result: resultFor(changedOutput) },
        receipt.startedAt, receipt.durationMs);
      assert.equal(changedReceipt.accounting.accepted, false);
      const changedArtifacts = structuredClone(baseline);
      const digest = hash(jsonText(changedReceipt));
      changedArtifacts[groupIndex]!.receipts[receiptIndex] = { receipt: changedReceipt, sha256: digest };
      (changedArtifacts[groupIndex]!.group as any).receiptDigests[receiptIndex]!.sha256 = digest;
      const aggregate = validateUnitCiAggregate(aggregateInput(current, changedArtifacts));
      assert.equal(aggregate.accepted, false);
      assert.ok(aggregate.errors.some((error) => error.includes('output accounting')));
    }
    const alteredAccounting = structuredClone(baseline);
    const forgedReceipt = alteredAccounting[groupIndex]!.receipts[receiptIndex]!.receipt as any;
    forgedReceipt.accounting.expectedDiagnostics[0].line = 'PASS forged source diagnostic';
    const forgedDigest = hash(jsonText(forgedReceipt));
    alteredAccounting[groupIndex]!.receipts[receiptIndex]!.sha256 = forgedDigest;
    (alteredAccounting[groupIndex]!.group as any).receiptDigests[receiptIndex]!.sha256 = forgedDigest;
    const forgedAggregate = validateUnitCiAggregate(aggregateInput(current, alteredAccounting));
    assert.equal(forgedAggregate.accepted, false);
    assert.ok(forgedAggregate.errors.some((error) => error.includes('output accounting')));
  }],
  ['unit CI sanitizer preserves exact output and redacts unknown lines and launch errors without weakening accounting', async () => {
    const inventory = makeInventory(['case 10[REDACTED]', wrapperName]);
    const current = fixture(inventory);
    const owner = inventory.cases.find(({ name }) => name === 'case 10[REDACTED]')!;
    const shard = current.manifest.shards.find(({ selection }) => selection.includes(owner.id))!;
    const group = current.plan.groups.find(({ shardIds }) => shardIds.includes(shard.id))!;
    const exactOutput = `ok - ${owner.name}\n`;
    const exactReceipt = createUnitShardReceipt(inventory, current.manifest, current.plan, group.id, shard,
      { binary: process.execPath, args: unitShardCommandArgs(shard.selection), result: resultFor(exactOutput) }, '2026-01-01T00:00:00.000Z', 5);
    assert.equal(exactReceipt.accounting.accepted, true);
    assert.equal(exactReceipt.command.result!.stdout, exactOutput);

    const secretTitle = 'A'.repeat(43);
    const titleReceipt = createUnitShardReceipt(inventory, current.manifest, current.plan, group.id, shard,
      { binary: process.execPath, args: unitShardCommandArgs(shard.selection), result: resultFor(`ok - case ${secretTitle}\n`) }, '2026-01-01T00:00:00.000Z', 5);
    assert.equal(titleReceipt.command.result!.stdout.includes(secretTitle), false);
    assert.equal(titleReceipt.command.result!.stdout, '[REDACTED UNEXPECTED OUTPUT]\n');
    assert.equal(titleReceipt.accounting.accepted, false);

    const unknownMarker = '[REDACTED UNEXPECTED OUTPUT]';
    const unknownOutputs = [
      { text: 'PASSWORD=short-secret\n', values: ['short-secret'], lines: [unknownMarker] },
      { text: 'API_TOKEN=api-token-fixture\n', values: ['api-token-fixture'], lines: [unknownMarker] },
      { text: 'CLIENT_SECRET: "quoted-fixture-value"\n', values: ['quoted-fixture-value'], lines: [unknownMarker] },
      { text: '{"password":"compact-json-fixture"}\n', values: ['compact-json-fixture'], lines: [unknownMarker] },
      {
        text: '{\n  "password":\n  "multiline-json-fixture"\n}\n',
        values: ['multiline-json-fixture'], lines: [unknownMarker, unknownMarker, unknownMarker, unknownMarker],
      },
      {
        text: '{"message":"{\\"password\\":\\"nested-escaped-fixture\\"}"}\n',
        values: ['nested-escaped-fixture'], lines: [unknownMarker],
      },
    ];
    for (const unknown of unknownOutputs) {
      const receipt = createUnitShardReceipt(inventory, current.manifest, current.plan, group.id, shard,
        { binary: process.execPath, args: unitShardCommandArgs(shard.selection), result: resultFor(`${exactOutput}${unknown.text}`) },
        '2026-01-01T00:00:00.000Z', 5);
      assert.equal(receipt.command.result!.stdout, `${exactOutput}${unknown.lines.join('\n')}\n`);
      const serializedReceipt = JSON.stringify(receipt);
      for (const value of unknown.values) assert.equal(serializedReceipt.includes(value), false);
      assert.deepEqual(receipt.accounting.diagnostics.stdout, unknown.lines);
      assert.deepEqual(receipt.accounting.actualDiagnostics.map(({ line }) => line), unknown.lines);
      assert.equal(receipt.accounting.accepted, false);
    }

    const failureReceipt = createUnitShardReceipt(inventory, current.manifest, current.plan, group.id, shard,
      { binary: process.execPath, args: unitShardCommandArgs(shard.selection), result: resultFor('not ok - case: failure-fixture-value\n') },
      '2026-01-01T00:00:00.000Z', 5);
    assert.equal(failureReceipt.command.result!.stdout, 'not ok - [REDACTED FAILURE OUTPUT]\n');
    assert.deepEqual(failureReceipt.accounting.notOk, ['[REDACTED FAILURE OUTPUT]']);
    assert.equal(JSON.stringify(failureReceipt).includes('failure-fixture-value'), false);
    assert.equal(failureReceipt.accounting.accepted, false);

    const skipReceipt = createUnitShardReceipt(inventory, current.manifest, current.plan, group.id, shard,
      { binary: process.execPath, args: unitShardCommandArgs(shard.selection), result: resultFor('ok - case # SKIP skip-fixture-value\n') },
      '2026-01-01T00:00:00.000Z', 5);
    assert.equal(skipReceipt.command.result!.stdout, 'ok - [REDACTED SKIP OUTPUT] # SKIP\n');
    assert.deepEqual(skipReceipt.accounting.skips, ['[REDACTED SKIP OUTPUT]']);
    assert.equal(JSON.stringify(skipReceipt).includes('skip-fixture-value'), false);
    assert.equal(skipReceipt.accounting.accepted, false);

    const launchErrorValue = 'launch-error-fixture-value';
    const launchErrorReceipt = createUnitShardReceipt(inventory, current.manifest, current.plan, group.id, shard, {
      binary: process.execPath,
      args: unitShardCommandArgs(shard.selection),
      rejected: true,
      launchError: `launcher failed: {"password":"${launchErrorValue}"}`,
      result: resultFor(exactOutput, { launchError: `process failed:\n  "password":\n  "${launchErrorValue}"` }),
    }, '2026-01-01T00:00:00.000Z', 5);
    assert.equal(launchErrorReceipt.command.launchError, '[REDACTED LAUNCH ERROR]');
    assert.equal(launchErrorReceipt.command.result!.launchError, '[REDACTED LAUNCH ERROR]');
    assert.equal(launchErrorReceipt.command.rejected, true);
    assert.equal(JSON.stringify(launchErrorReceipt).includes(launchErrorValue), false);
    assert.equal(launchErrorReceipt.accounting.accepted, false);

    const collisionInventory = makeInventory(['PASSWORD=[REDACTED]', wrapperName]);
    const collision = fixture(collisionInventory);
    const collisionOwner = collisionInventory.cases.find(({ name }) => name === 'PASSWORD=[REDACTED]')!;
    const collisionShard = collision.manifest.shards.find(({ selection }) => selection.includes(collisionOwner.id))!;
    const collisionGroup = collision.plan.groups.find(({ shardIds }) => shardIds.includes(collisionShard.id))!;
    const collisionReceipt = createUnitShardReceipt(collisionInventory, collision.manifest, collision.plan, collisionGroup.id, collisionShard,
      { binary: process.execPath, args: unitShardCommandArgs(collisionShard.selection), result: resultFor('ok - PASSWORD=collision-fixture-value\n') },
      '2026-01-01T00:00:00.000Z', 5);
    assert.equal(collisionReceipt.command.result!.stdout, '[REDACTED UNEXPECTED OUTPUT]\n');
    assert.equal(JSON.stringify(collisionReceipt).includes('collision-fixture-value'), false);
    assert.deepEqual(collisionReceipt.accounting.missingSuccesses, ['PASSWORD=[REDACTED]']);
    assert.equal(collisionReceipt.accounting.accepted, false);

    const skipCollisionInventory = makeInventory(['[REDACTED SKIP OUTPUT] # SKIP', wrapperName]);
    const skipCollision = fixture(skipCollisionInventory);
    const skipCollisionOwner = skipCollisionInventory.cases.find(({ name }) => name === '[REDACTED SKIP OUTPUT] # SKIP')!;
    const skipCollisionShard = skipCollision.manifest.shards.find(({ selection }) => selection.includes(skipCollisionOwner.id))!;
    const skipCollisionGroup = skipCollision.plan.groups.find(({ shardIds }) => shardIds.includes(skipCollisionShard.id))!;
    const skipCollisionReceipt = createUnitShardReceipt(skipCollisionInventory, skipCollision.manifest, skipCollision.plan,
      skipCollisionGroup.id, skipCollisionShard,
      { binary: process.execPath, args: unitShardCommandArgs(skipCollisionShard.selection), result: resultFor('ok - unknown # SKIP collision-fixture-value\n') },
      '2026-01-01T00:00:00.000Z', 5);
    assert.equal(skipCollisionReceipt.command.result!.stdout, 'ok - [REDACTED SKIP OUTPUT] # SKIP!\n');
    assert.deepEqual(skipCollisionReceipt.accounting.skips, ['[REDACTED SKIP OUTPUT]']);
    assert.equal(JSON.stringify(skipCollisionReceipt).includes('collision-fixture-value'), false);
    assert.equal(skipCollisionReceipt.accounting.accepted, false);
  }],
  ['unit CI output accounting counts the valid __proto__ title as an own exact-match key', async () => {
    const outer = stableTestIdentities('outer', ['__proto__', wrapperName]);
    const wrapper = outer.find(({ name }) => name === wrapperName)!;
    const inner = stableTestIdentities('ios-inner', ['iOS fixture inner one', 'iOS fixture inner two'])
      .map((entry) => ({ ...entry, parentId: wrapper.id }));
    const current = fixture({ schema: 1, cases: [...outer, ...inner] });
    const protoCase = current.inventory.cases.find(({ name }) => name === '__proto__')!;
    const shard = current.manifest.shards.find(({ selection }) => selection.includes(protoCase.id))!;
    const groupIndex = current.plan.groups.findIndex(({ shardIds }) => shardIds.includes(shard.id));
    const groupId = current.plan.groups[groupIndex]!.id;
    const receiptIndex = current.plan.groups[groupIndex]!.shardIds.indexOf(shard.id);
    const args = unitShardCommandArgs(shard.selection);
    const selectedReceipt = createUnitShardReceipt(current.inventory, current.manifest, current.plan, groupId, shard,
      { binary: process.execPath, args, result: resultFor('ok - __proto__\n') }, '2026-01-01T00:00:00.000Z', 5);
    assert.equal(Object.hasOwn(selectedReceipt.accounting.expectedSuccesses, '__proto__'), true);
    assert.equal(Object.hasOwn(selectedReceipt.accounting.actualSuccesses, '__proto__'), true);
    assert.deepEqual(selectedReceipt.accounting.missingSuccesses, []);
    assert.equal(selectedReceipt.accounting.accepted, true);

    const missingReceipt = createUnitShardReceipt(current.inventory, current.manifest, current.plan, groupId, shard,
      { binary: process.execPath, args, result: resultFor('fixture diagnostic only\n') }, '2026-01-01T00:00:00.000Z', 5);
    assert.deepEqual(missingReceipt.accounting.missingSuccesses, ['__proto__']);
    assert.equal(missingReceipt.accounting.outputMissing, false);
    assert.equal(missingReceipt.accounting.accepted, false);
    const artifacts = makeGroupArtifacts(current);
    const changed = structuredClone(artifacts);
    const digest = hash(jsonText(missingReceipt));
    changed[groupIndex]!.receipts[receiptIndex] = { receipt: missingReceipt, sha256: digest };
    (changed[groupIndex]!.group as any).receiptDigests[receiptIndex]!.sha256 = digest;
    assert.equal(validateUnitCiAggregate(aggregateInput(current, changed)).accepted, false);
  }],
  ['unit CI preparation and synthetic shard adapters write fresh source-bound receipts for zero nonzero timeout cancellation and skip', async () => {
    const inventory = makeInventory();
    const root = await mkdtemp(join(tmpdir(), 'herdr-unit-ci-adapter-'));
    try {
      for (const outcome of ['success', 'nonzero', 'timeout', 'cancel', 'skip', 'rejected-success-shaped'] as const) {
        const prepared = join(root, `prepared-${outcome}`);
        const receipts = join(root, `receipts-${outcome}`);
        const runner = syntheticListingRunner(inventory, outcome);
        const prep = await prepareUnitCiArtifacts(prepared, identity, runner);
        assert.equal(JSON.parse(prep.matrix).include.length, prep.plan.groups.length);
        const groupId = prep.plan.groups[0]!.id;
        const result = await executeUnitCiGroup(prepared, groupId, receipts, identity, runner);
        const shardFiles = (await readdir(receipts)).filter((name) => name.startsWith('receipt-'));
        assert.equal(shardFiles.length, prep.plan.groups[0]!.shardIds.length);
        const firstShardId = prep.plan.groups[0]!.shardIds[0]!;
        const raw = JSON.parse(await readFile(join(receipts, `receipt-${firstShardId}.json`), 'utf8')) as UnitShardReceipt;
        assert.equal(raw.sourceSha, identity.sourceSha);
        assert.equal(raw.planSha256, prep.plan.sha256);
        assert.equal(raw.accounting.accepted, outcome === 'success', `${outcome} receipt acceptance`);
        assert.equal(raw.command.rejected, outcome === 'rejected-success-shaped', `${outcome} rejection evidence`);
        if (outcome === 'rejected-success-shaped') assert.match(raw.command.result!.stdout, /ok - Android fixture alpha/u);
        assert.equal(result.accepted, outcome === 'success', `${outcome} group acceptance`);
        const groupReceipt = JSON.parse(await readFile(join(receipts, 'group.json'), 'utf8'));
        assert.equal(groupReceipt.result, outcome === 'success' ? 'success' : 'failure');
      }
      await assert.rejects(() => prepareUnitCiArtifacts(join(root, 'prepared-success'), identity, syntheticListingRunner(inventory)), /already exists|EEXIST/u);
    } finally {
      await rm(root, { recursive: true, force: true });
    }
  }],
  ['unit CI execution clears selector overrides and rejects invalid group selection', async () => {
    const inventory = makeInventory();
    const root = await mkdtemp(join(tmpdir(), 'herdr-unit-ci-selectors-'));
    const oldMobile = process.env.MOBILE_UNIT_FILTER;
    const oldIOS = process.env.IOS_TEST_FILTER;
    process.env.MOBILE_UNIT_FILTER = '(?!)';
    process.env.IOS_TEST_FILTER = '(?!)';
    try {
      const prepared = join(root, 'prepared');
      const receipts = join(root, 'receipts');
      const runner = syntheticListingRunner(inventory, 'success', () => {
        assert.equal(process.env.MOBILE_UNIT_FILTER, undefined);
        assert.equal(process.env.IOS_TEST_FILTER, undefined);
        assert.equal(process.env.BUN_OPTIONS, '--no-env-file');
        assert.equal(process.env.GOTOOLCHAIN, 'local');
      });
      const prep = await prepareUnitCiArtifacts(prepared, identity, runner);
      await executeUnitCiGroup(prepared, prep.plan.groups[0]!.id, receipts, identity, runner);
      assert.equal(process.env.MOBILE_UNIT_FILTER, '(?!)');
      assert.equal(process.env.IOS_TEST_FILTER, '(?!)');
      await assert.rejects(() => executeUnitCiGroup(prepared, '../escape', join(root, 'bad'), identity, runner), /invalid format/u);
      await assert.rejects(() => executeUnitCiGroup(prepared, prep.plan.groups[0]!.id, prepared, identity, runner), /aliases a protected input/u);
      await assert.rejects(() => executeUnitCiGroup(prepared, prep.plan.groups[0]!.id, `${root}/nested/../escaped`, identity, runner), /absolute and canonical/u);
      assert.equal(await readdir(root).then((entries) => entries.includes('bad')), false);
      assert.equal(await readdir(root).then((entries) => entries.includes('escaped')), false);
    } finally {
      if (oldMobile === undefined) delete process.env.MOBILE_UNIT_FILTER;
      else process.env.MOBILE_UNIT_FILTER = oldMobile;
      if (oldIOS === undefined) delete process.env.IOS_TEST_FILTER;
      else process.env.IOS_TEST_FILTER = oldIOS;
      await rm(root, { recursive: true, force: true });
    }
  }],
  ['bounded unit CI adapter process records actual output and reaps a timed-out fixture', async () => {
    const output = await boundedCommand(process.execPath, ['--no-env-file', '-e', 'process.stdout.write("fixture-stdout");process.stderr.write("fixture-stderr")'], 5_000, {
      maxBytes: 1_024,
      cleanupReservationMs: 500,
    });
    assert.equal(output.stdout, 'fixture-stdout');
    assert.equal(output.stderr, 'fixture-stderr');
    assert.equal(output.code, 0);
    assert.equal(output.ownedProcessesExited, true);
    assert.equal(output.stdioClosed, true);
    await assert.rejects(
      () => boundedCommand(process.execPath, ['--no-env-file', '-e', 'setInterval(()=>{},1000)'], 3_000, { maxBytes: 1_024, cleanupReservationMs: 1_000 }),
      (error: unknown) => error instanceof BoundedCommandError && error.result.timedOut && error.result.ownedProcessesExited,
    );
  }],
];
