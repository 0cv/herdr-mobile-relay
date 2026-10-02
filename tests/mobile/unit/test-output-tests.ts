import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { boundedCommand } from '../support/bounded-process';
import { repositoryRoot } from '../support/paths';
import { stableTestIdentities } from './test-registry';
import {
  createUnitTestOutputContract,
  UNIT_TEST_STANDALONE_DIAGNOSTICS,
  unitTestDiagnosticOwnerNames,
  validateUnitTestOutput,
} from './test-output';
import type { UnitTestInventory } from './test-shards';

type OutputFixture = { inventory: UnitTestInventory; outerIds: Map<string, string[]>; innerIds: string[] };
const wrapperName = 'iOS recorded publication, installation and navigation protocol regressions';

function makeInventory(names: string[]): OutputFixture {
  const outer = stableTestIdentities('outer', [...names, wrapperName]);
  const wrapper = outer.find(({ name }) => name === wrapperName)!;
  const inner = stableTestIdentities('ios-inner', ['iOS nested one', 'iOS nested two'])
    .map((entry) => ({ ...entry, parentId: wrapper.id }));
  const outerIds = new Map<string, string[]>();
  for (const entry of outer) outerIds.set(entry.name, [...(outerIds.get(entry.name) || []), entry.id]);
  return { inventory: { schema: 1, cases: [...outer, ...inner] }, outerIds, innerIds: inner.map(({ id }) => id) };
}

function outputFor(inventory: UnitTestInventory, selection: readonly string[]): string {
  return `${createUnitTestOutputContract(inventory, selection).events.map(({ line }) => line).join('\n')}\n`;
}

function selected(fixture: OutputFixture, name: string, occurrence = 0): string {
  const id = fixture.outerIds.get(name)?.[occurrence];
  assert.ok(id, `missing fixture registration ${name} occurrence ${occurrence + 1}`);
  return id;
}

async function authoritativeInventory(): Promise<UnitTestInventory> {
  const root = await mkdtemp(join(tmpdir(), 'unit-output-listing-'));
  const sentinel = join(root, 'body-ran');
  const previous = new Map(['BUN_OPTIONS', 'GOTOOLCHAIN', 'MOBILE_UNIT_FILTER', 'IOS_TEST_FILTER', 'MOBILE_UNIT_REGISTRATION_SENTINEL']
    .map((key) => [key, process.env[key]]));
  process.env.BUN_OPTIONS = '--no-env-file';
  process.env.GOTOOLCHAIN = 'local';
  process.env.MOBILE_UNIT_REGISTRATION_SENTINEL = sentinel;
  delete process.env.MOBILE_UNIT_FILTER;
  delete process.env.IOS_TEST_FILTER;
  try {
    const result = await boundedCommand(process.execPath, ['--no-env-file', join(repositoryRoot, 'tests/mobile/unit/run.ts'), '--list-tests'], 20_000, {
      maxBytes: 4 * 1024 * 1024,
      cleanupReservationMs: 1_000,
      label: 'unit output contract source listing',
    });
    assert.equal(result.code, 0);
    assert.equal(result.timedOut, false);
    assert.equal(result.outputLimitExceeded, false);
    assert.equal(result.ownedProcessesExited, true);
    assert.equal(result.stdioClosed, true);
    assert.equal(result.stderr, '');
    const listing = JSON.parse(result.stdout) as UnitTestInventory;
    assert.equal(listing.schema, 1);
    assert.equal(listing.cases.length > 0, true);
    assert.equal((await import('node:fs')).existsSync(sentinel), false);
    return listing;
  } finally {
    for (const [key, value] of previous) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
    await rm(root, { recursive: true, force: true });
  }
}

export const testOutputTests: Array<[string, () => Promise<void>]> = [
  ['unit output source catalog is deterministic, owner-bound and body-free for the authoritative listing', async () => {
    const inventory = await authoritativeInventory();
    const wrapper = inventory.cases.find(({ name, scope }) => name === wrapperName && scope === 'outer')!;
    const selection = inventory.cases.filter(({ id }) => id !== wrapper.id).map(({ id }) => id);
    const first = createUnitTestOutputContract(inventory, selection);
    const second = createUnitTestOutputContract(inventory, selection);
    assert.deepEqual(first, second);
    assert.equal(first.expectedSuccessEvents.length, selection.length + 1);
    assert.equal(first.expectedSuccessEvents.filter(({ successKind }) => successKind === 'ios-wrapper').length, 1);
    assert.equal(first.expectedSuccessEvents.filter(({ successKind }) => successKind === 'ios-inner').length,
      inventory.cases.filter(({ scope }) => scope === 'ios-inner').length);
    for (const name of unitTestDiagnosticOwnerNames) {
      const owners = inventory.cases.filter(({ name: actual, scope }) => actual === name && scope === 'outer');
      assert.equal(owners.length, 1, `source emitter ${name} must resolve to one authoritative case`);
      const diagnostics = first.expectedDiagnostics.filter(({ ownerId }) => ownerId === owners[0]!.id);
      assert.ok(diagnostics.length > 0, `source emitter ${name} was omitted from the output contract`);
      assert.ok(diagnostics.every(({ ownerName, ownerOccurrence, ownerScope }) =>
        ownerName === name && ownerOccurrence === 1 && ownerScope === 'outer'));
    }
    assert.ok(first.expectedDiagnostics.every(({ ownerId }) => selection.includes(ownerId)));
    assert.equal(first.expectedDiagnostics.some(({ line }) => UNIT_TEST_STANDALONE_DIAGNOSTICS.includes(line as typeof UNIT_TEST_STANDALONE_DIAGNOSTICS[number])), false);
    assert.equal(validateUnitTestOutput(inventory, selection, outputFor(inventory, selection), '').accepted, true);
  }],
  ['unit output contract accounts exact response and inspection source loops and rejects missing, duplicate, unknown and unselected diagnostics', async () => {
    const fixture = makeInventory([
      'Android retained post-attachment and response-shape refusals',
      'Android retained passive inspection decoder and ownership regressions',
      'Android fixture without source diagnostics',
    ]);
    const responseId = selected(fixture, 'Android retained post-attachment and response-shape refusals');
    const inspectionId = selected(fixture, 'Android retained passive inspection decoder and ownership regressions');
    const response = createUnitTestOutputContract(fixture.inventory, [responseId]);
    assert.equal(response.expectedDiagnostics.length, 42);
    assert.deepEqual(response.expectedDiagnostics.slice(0, 3).map(({ line }) => line), [
      'PASS retained response identity/http',
      'PASS retained response identity/session',
      'PASS retained response identity/transport',
    ]);
    assert.equal(validateUnitTestOutput(fixture.inventory, [responseId], outputFor(fixture.inventory, [responseId]), '').accepted, true);
    const inspection = createUnitTestOutputContract(fixture.inventory, [inspectionId]);
    assert.equal(inspection.expectedDiagnostics.length, 63);
    assert.equal(validateUnitTestOutput(fixture.inventory, [inspectionId], outputFor(fixture.inventory, [inspectionId]), '').accepted, true);
    const cleanId = selected(fixture, 'Android fixture without source diagnostics');
    const clean = outputFor(fixture.inventory, [cleanId]);
    assert.equal(validateUnitTestOutput(fixture.inventory, [cleanId], clean, '').accepted, true);
    for (const changed of [
      clean.replace('\n', '\nPASS retained response identity/http\n'),
      clean.replace('\n', '\nPASS arbitrary diagnostic\n'),
    ]) assert.equal(validateUnitTestOutput(fixture.inventory, [cleanId], changed, '').accepted, false);
    const removed = outputFor(fixture.inventory, [responseId]).replace('PASS retained response identity/http\n', '');
    const duplicated = outputFor(fixture.inventory, [responseId]).replace('PASS retained response identity/http\n', 'PASS retained response identity/http\nPASS retained response identity/http\n');
    const renamed = outputFor(fixture.inventory, [responseId]).replace('PASS retained response identity/http\n', 'PASS retained response identity/other\n');
    const unknown = outputFor(fixture.inventory, [responseId]).replace('PASS retained response identity/http\n', 'PASS invented diagnostic\n');
    for (const changed of [removed, duplicated, renamed, unknown]) {
      assert.equal(validateUnitTestOutput(fixture.inventory, [responseId], changed, '').accepted, false);
    }
  }],
  ['unit output contract limits retained launch messages to selected outer call scopes', async () => {
    const fixture = makeInventory([
      'Android initial launch retains bootstrap and verifies readiness after signed launch',
      'Android installed attachment selects the owned standalone window instead of a browser window',
      'Android retained signed launch and explicit lifecycle regressions',
      'Android unrelated selection',
    ]);
    const initial = selected(fixture, 'Android initial launch retains bootstrap and verifies readiness after signed launch');
    const attachment = selected(fixture, 'Android installed attachment selects the owned standalone window instead of a browser window');
    const whole = selected(fixture, 'Android retained signed launch and explicit lifecycle regressions');
    const unrelated = selected(fixture, 'Android unrelated selection');
    const exactLine = 'PASS retained signed launch recorded-two-page';
    for (const id of [initial, attachment]) {
      const contract = createUnitTestOutputContract(fixture.inventory, [id]);
      assert.deepEqual(contract.expectedDiagnostics.map(({ line }) => line), [exactLine]);
      assert.equal(validateUnitTestOutput(fixture.inventory, [id], outputFor(fixture.inventory, [id]), '').accepted, true);
    }
    const full = createUnitTestOutputContract(fixture.inventory, [whole]);
    assert.ok(full.expectedDiagnostics.some(({ line }) => line === 'PASS retained kernel acquisition kernel-boot-acquisition'));
    assert.ok(full.expectedDiagnostics.some(({ line }) => line === exactLine));
    assert.equal(validateUnitTestOutput(fixture.inventory, [unrelated], `${outputFor(fixture.inventory, [unrelated])}${exactLine}\n`, '').accepted, false);
  }],
  ['unit output contract requires nested iOS successes before exactly one wrapper success', async () => {
    const fixture = makeInventory(['Android unrelated selection']);
    const inner = fixture.innerIds[0]!;
    const contract = createUnitTestOutputContract(fixture.inventory, [inner]);
    assert.deepEqual(contract.expectedSuccessEvents.map(({ line, successKind }) => [line, successKind]), [
      ['ok - iOS nested one', 'ios-inner'],
      [`ok - ${wrapperName}`, 'ios-wrapper'],
    ]);
    const healthy = outputFor(fixture.inventory, [inner]);
    assert.equal(validateUnitTestOutput(fixture.inventory, [inner], healthy, '').accepted, true);
    for (const changed of [
      'ok - iOS nested one\n',
      `ok - ${wrapperName}\nok - iOS nested one\n`,
      `ok - iOS nested one\nok - iOS nested one\nok - ${wrapperName}\n`,
    ]) assert.equal(validateUnitTestOutput(fixture.inventory, [inner], changed, '').accepted, false);
    assert.equal(validateUnitTestOutput(fixture.inventory, [inner], '', healthy).accepted, false);
    assert.equal(validateUnitTestOutput(fixture.inventory, [selected(fixture, 'iOS recorded publication, installation and navigation protocol regressions')], healthy, '').accepted, false);
  }],
  ['unit output contract preserves stream identity and rejects failure, skip and unknown stderr even with all successes', async () => {
    const fixture = makeInventory(['Android fixture']);
    const id = selected(fixture, 'Android fixture');
    const healthy = outputFor(fixture.inventory, [id]);
    const mutations = [
      { stdout: '', stderr: healthy },
      { stdout: healthy, stderr: 'unknown stderr\n' },
      { stdout: healthy, stderr: '\n' },
      { stdout: `${healthy}PASS unknown\n`, stderr: '' },
      { stdout: healthy.replace('\n', '\nnot ok - Android fixture: failure\n'), stderr: '' },
      { stdout: healthy.replace('\n', ' # SKIP fixture\n'), stderr: '' },
    ];
    for (const mutation of mutations) {
      const assessment = validateUnitTestOutput(fixture.inventory, [id], mutation.stdout, mutation.stderr);
      assert.equal(assessment.accepted, false);
      assert.ok(assessment.outputErrors.length > 0);
    }
    const newlineOnlyStderr = validateUnitTestOutput(fixture.inventory, [id], healthy, '\n');
    assert.ok(newlineOnlyStderr.outputErrors.some((error) => error.includes('stderr contains an empty line')));
  }],
  ['unit output contract rejects reordering, partial lines, controls and empty selections while normalizing only CRLF', async () => {
    const fixture = makeInventory(['Android retained post-attachment and response-shape refusals']);
    const id = selected(fixture, 'Android retained post-attachment and response-shape refusals');
    const healthy = outputFor(fixture.inventory, [id]);
    const crlf = healthy.replaceAll('\n', '\r\n');
    assert.equal(validateUnitTestOutput(fixture.inventory, [id], crlf, '').accepted, true);
    const events = createUnitTestOutputContract(fixture.inventory, [id]).events.map(({ line }) => line);
    const reordered = `${events[1]}\n${events[0]}\n${events.slice(2).join('\n')}\n`;
    const truncated = healthy.slice(0, -1);
    const control = healthy.replace('identity/http', 'identity/ht\u0000tp');
    const bareCarriageReturn = healthy.replace('\n', '\r');
    for (const changed of [reordered, truncated, control, bareCarriageReturn]) {
      assert.equal(validateUnitTestOutput(fixture.inventory, [id], changed, '').accepted, false);
    }
    assert.equal(validateUnitTestOutput(fixture.inventory, [], '', '').accepted, false);
    assert.equal(validateUnitTestOutput(fixture.inventory, [id, id], healthy, '').accepted, false);
    const excessive = validateUnitTestOutput(fixture.inventory, [id], '\n'.repeat(10_001), '');
    assert.equal(excessive.accepted, false);
    assert.ok(excessive.outputErrors.length <= 100);
    assert.ok(excessive.outputErrors.some((error) => error.includes('accounting line limit')));
  }],
  ['unit output contract handles repeated titles, skip-like titles and __proto__ as exact selected identity occurrences', async () => {
    const fixture = makeInventory(['Repeated title', 'Repeated title', 'literal # SKIP phrase', '__proto__']);
    const repeatedOne = selected(fixture, 'Repeated title', 0);
    const repeatedTwo = selected(fixture, 'Repeated title', 1);
    const protoId = selected(fixture, '__proto__');
    const skipLikeId = selected(fixture, 'literal # SKIP phrase');
    assert.match(repeatedOne, /:1$/u);
    assert.match(repeatedTwo, /:2$/u);
    const firstContract = createUnitTestOutputContract(fixture.inventory, [repeatedOne]);
    assert.equal(firstContract.expectedSuccesses['Repeated title'], 1);
    assert.equal(firstContract.expectedSuccessEvents[0]!.caseId, repeatedOne);
    assert.equal(firstContract.expectedSuccessEvents[0]!.occurrence, 1);
    const firstAssessment = validateUnitTestOutput(fixture.inventory, [repeatedOne], outputFor(fixture.inventory, [repeatedOne]), '');
    assert.equal(firstAssessment.accepted, true);
    assert.equal(firstAssessment.actualSuccessEvents[0]!.occurrence, 1);
    const secondContract = createUnitTestOutputContract(fixture.inventory, [repeatedTwo]);
    assert.equal(secondContract.expectedSuccesses['Repeated title'], 1);
    assert.equal(secondContract.expectedSuccessEvents[0]!.caseId, repeatedTwo);
    assert.equal(secondContract.expectedSuccessEvents[0]!.occurrence, 2);
    assert.equal(validateUnitTestOutput(fixture.inventory, [repeatedTwo], outputFor(fixture.inventory, [repeatedTwo]), '').accepted, true);
    const bothAssessment = validateUnitTestOutput(fixture.inventory, [repeatedOne, repeatedTwo], outputFor(fixture.inventory, [repeatedOne, repeatedTwo]), '');
    assert.equal(bothAssessment.accepted, true);
    assert.deepEqual(bothAssessment.actualSuccessEvents.map(({ occurrence }) => occurrence), [1, 2]);
    assert.equal(validateUnitTestOutput(fixture.inventory, [skipLikeId], outputFor(fixture.inventory, [skipLikeId]), '').accepted, true);
    const protoContract = createUnitTestOutputContract(fixture.inventory, [protoId]);
    assert.equal(Object.hasOwn(protoContract.expectedSuccesses, '__proto__'), true);
    assert.equal(protoContract.expectedSuccesses['__proto__'], 1);
    assert.equal(validateUnitTestOutput(fixture.inventory, [protoId], outputFor(fixture.inventory, [protoId]), '').accepted, true);
  }],
  ['unit runner-only output rejects standalone diagnostic summaries and known source lines without their owner', async () => {
    const fixture = makeInventory(['Android attachment command refusals latch without discovery retry']);
    const id = selected(fixture, 'Android attachment command refusals latch without discovery retry');
    const expected = outputFor(fixture.inventory, [id]);
    for (const line of UNIT_TEST_STANDALONE_DIAGNOSTICS) {
      assert.equal(validateUnitTestOutput(fixture.inventory, [id], `${expected}${line}\n`, '').accepted, false);
    }
  }],
];
