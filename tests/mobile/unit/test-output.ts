import { androidLifecycleCases } from './android-retained-launch';
import {
  generateUnitTestShardManifest,
  validateUnitTestShardManifest,
  type UnitTestCase,
  type UnitTestInventory,
} from './test-shards';

const IOS_WRAPPER_NAME = 'iOS recorded publication, installation and navigation protocol regressions';
const MAX_CASES = 20_000;
const MAX_ACCOUNTING_LINES = 10_000;
const MAX_OUTPUT_ERRORS = 100;
const MAX_ACCOUNTING_LINE_LENGTH = 500;

type OutputEvent =
  | { kind: 'success'; line: string; caseId: string; caseName: string; scope: 'outer' | 'ios-inner'; occurrence: number; successKind: 'outer' | 'ios-inner' | 'ios-wrapper' }
  | { kind: 'diagnostic'; line: string; ownerId: string; ownerName: string; ownerScope: 'outer'; ownerOccurrence: number; ownerSequence: number; sourceFile: string };

type DiagnosticExpectation = Extract<OutputEvent, { kind: 'diagnostic' }> & { count: number };
export type UnitTestOutputDiagnostic = {
  line: string;
  stream: 'stdout' | 'stderr';
  lineNumber: number;
  ownerId?: string;
  ownerName?: string;
  ownerScope?: 'outer';
  ownerOccurrence?: number;
  ownerSequence?: number;
  sourceFile?: string;
};
export type UnitTestOutputSuccess = {
  line: string;
  title: string;
  stream: 'stdout' | 'stderr';
  lineNumber: number;
  caseId?: string;
  caseName?: string;
  scope?: 'outer' | 'ios-inner';
  occurrence?: number;
  successKind?: 'outer' | 'ios-inner' | 'ios-wrapper';
};
export type UnitTestOutputContract = {
  selection: string[];
  events: OutputEvent[];
  expectedSuccesses: Record<string, number>;
  expectedSuccessEvents: Array<Extract<OutputEvent, { kind: 'success' }>>;
  expectedDiagnostics: DiagnosticExpectation[];
};
export type UnitTestOutputAssessment = {
  expectedSuccesses: Record<string, number>;
  actualSuccesses: Record<string, number>;
  expectedSuccessEvents: UnitTestOutputContract['expectedSuccessEvents'];
  actualSuccessEvents: UnitTestOutputSuccess[];
  expectedDiagnostics: DiagnosticExpectation[];
  actualDiagnostics: UnitTestOutputDiagnostic[];
  diagnostics: { stdout: string[]; stderr: string[] };
  skips: string[];
  notOk: string[];
  unexpectedSuccesses: string[];
  missingSuccesses: string[];
  outputMissing: boolean;
  outputErrors: string[];
  accepted: boolean;
};

type SourceDiagnostic = { line: string; sourceFile: string };
type DiagnosticEmitter = (entry: UnitTestCase) => SourceDiagnostic[];

const RETAINED_RESPONSE_OPERATIONS = ['identity', 'completion', 'agent', 'dialog'];
const RETAINED_RESPONSE_FAULTS = ['http', 'session', 'transport'];
const RETAINED_SHAPE_OPERATIONS = ['contexts', 'handles', 'window', 'url', 'proof'];
const RETAINED_SHAPE_FAULTS = ['missing', 'null', 'object', 'number', 'array-item', 'empty-array'];
const INSPECTION_CHANGE_SCENARIOS = [
  'kind', 'original-pid', 'original-start', 'original-boot', 'kernel-mode', 'kernel-source', 'kernel-digest',
  'kernel-bounds', 'kernel-size', 'kernel-boot', 'kernel-time', 'kernel-snapshot', 'kernel-cache-time',
  'original-namespace', 'original-session', 'native-pid', 'native-start', 'native-boot', 'native-namespace',
  'chrome-main', 'native-provider', 'current-document', 'current-provider', 'current-window', 'second', 'unknown',
  'iframe', 'background-page', 'malformed-url', 'duplicate-observation', 'missing', 'duplicate', 'malformed',
  'replaced', 'association-pid', 'association-connection', 'association-order', 'association-time', 'stale-bound',
  'endpoint-drift', 'forward-drift', 'document-scope', 'zero-candidate', 'association-final-count',
];
const INSPECTION_EXTRA_SCENARIOS = [
  'route-error', 'route-timeout', 'overlap', 'fresh-navigation', 'service-worker', 'mutation-postcheck',
  'parent-admission', 'identity-document-race', 'completion-document-race', 'preference-native-race',
  'agent-native-race', 'composer-native-race', 'completion-parent-admission', 'completion-parent-expiry',
  'failure-parent-admission', 'failure-parent-expiry', 'composer-type-native-race', 'candidate-navigation-during-switch',
  'confirmation-publication',
];
const RETAINED_LAUNCH_SCENARIOS = [
  'boot', 'kernel-boot-acquisition', 'kernel-disabled', 'kernel-unreadable', 'kernel-missing', 'kernel-malformed',
  'kernel-duplicate', 'kernel-conflicting', 'kernel-enabled-absent', 'kernel-disabled-fields', 'kernel-pid-duplicate',
  'kernel-nspid-duplicate', 'kernel-nested', 'kernel-same-number-nested', 'healthy', 'pid-before', 'start-before',
  'selected-window-loss', 'pid', 'start-time', 'missing-process', 'dead-process', 'missing-owner', 'replaced-session',
  'missing-current', 'missing-handles', 'missing-context', 'refusal', 'timeout', 'malformed', 'wrong-id',
  'wrong-component', 'wrong-scope', 'wrong-mac', 'ambiguous-shortcut', 'uncertain-launch', 'wrong-origin',
  'stale-browser', 'wrong-provider', 'ambiguous-document', 'late-document', 'zero-candidate',
  'still-browser-confirmation', 'recorded-two-page',
];
const TEARDOWN_SCENARIOS = [
  'held-successful-delete', 'held-timeout-delete', 'held-resolved-fatal-delete', 'warm-inspection', 'termination-home',
  'setup-settings-post', 'setup-chrome-post', 'setup-settings-timeout', 'setup-before-settings', 'setup-overlap',
  'setup-synchronous-stop', 'cold-replacement-post',
];
const CERTIFICATE_TEARDOWN_SCENARIOS = [
  'push-success-after-stop', 'push-failure-after-stop', 'scan-success-after-stop',
  'scan-failure-after-stop', 'push-failure-before-stop',
];
const WARM_ENVIRONMENT_SCENARIOS = [
  'healthy-warm', 'early-isolated-signal9-only', 'unknown-stop-only',
  'stable-binary-gms-components-only', 'signal9-stop-components-combined', 'package-identity-replacement-only',
];
const SOCKET_FAULTS = ['search', 'browser', 'other', 'origin', 'document', 'native-failure', 'missing', 'empty-valid'];
const APPIUM_WORKFLOW_FILES = ['.github/actions/mobile-device-run/action.yml', '.github/workflows/mobile-ci.yml'];

function emitter(line: string, sourceFile: string): SourceDiagnostic {
  return { line, sourceFile };
}

const diagnosticEmitters = new Map<string, DiagnosticEmitter>([
  ['Android retained post-attachment and response-shape refusals', () => [
    ...RETAINED_RESPONSE_OPERATIONS.flatMap((operation) => RETAINED_RESPONSE_FAULTS.map((fault) =>
      emitter(`PASS retained response ${operation}/${fault}`, 'android-retained-responses.ts'))),
    ...RETAINED_SHAPE_OPERATIONS.flatMap((operation) => RETAINED_SHAPE_FAULTS.map((fault) =>
      emitter(`PASS retained response ${operation}/${fault}`, 'android-retained-responses.ts'))),
  ]],
  ['Android retained passive inspection decoder and ownership regressions', () =>
    [...INSPECTION_CHANGE_SCENARIOS, ...INSPECTION_EXTRA_SCENARIOS].map((scenario) =>
      emitter(`PASS retained inspection ${scenario}`, 'android-retained-inspection.ts'))],
  ['Android retained signed launch and explicit lifecycle regressions', () => RETAINED_LAUNCH_SCENARIOS.map((scenario) =>
    emitter(`PASS retained ${scenario.startsWith('kernel-') && scenario !== 'kernel-disabled' ? 'kernel acquisition' : 'signed launch'} ${scenario}`, 'android-retained-launch.ts'))],
  ['Android initial launch retains bootstrap and verifies readiness after signed launch', () =>
    [emitter('PASS retained signed launch recorded-two-page', 'android-retained-launch.ts')]],
  ['Android installed attachment selects the owned standalone window instead of a browser window', () =>
    [emitter('PASS retained signed launch recorded-two-page', 'android-retained-launch.ts')]],
  ...Object.entries(androidLifecycleCases).map(([group, names]): [string, DiagnosticEmitter] => [
    `Android production CLI retained initial and warm relaunch emit no fabricated close evidence or planned operations: ${group}`, () => {
      const lines: SourceDiagnostic[] = [];
      for (const name of names) {
        if (name === 'concurrent-cold-consumers') {
          for (const variant of TEARDOWN_SCENARIOS) {
            if (variant === 'setup-settings-post') {
              lines.push(...CERTIFICATE_TEARDOWN_SCENARIOS.map((certificate) =>
                emitter(`PASS SM56 certificate admission ${certificate}`, 'android-retained-launch.ts')));
            }
            lines.push(emitter(`PASS SM56 lifecycle concurrent-cold-consumers/${variant}`, 'android-retained-launch.ts'));
          }
        }
        lines.push(emitter(`PASS SM56 lifecycle ${group}/${name}`, 'android-environment.ts'));
      }
      return lines;
    }]),
  ['Android production CLI SM56 warm lifecycle keeps independent and combined signal, stop and component drift fatal', () =>
    WARM_ENVIRONMENT_SCENARIOS.map((name) => emitter(`PASS SM56 environment ${name}`, 'android-environment.ts'))],
  ['Android CI gates both local Appium launch paths', () => [
    ...APPIUM_WORKFLOW_FILES.map((filename) => emitter(
      `PASS ${filename}: scoped installation, startup refusal and bounded Android integrity artifact routing`,
      'android-appium-ci.ts',
    )),
    emitter('PASS launch wrapper verifies immediately before exec of the same local Appium', 'android-appium-ci.ts'),
  ]],
  ['Android socket metadata preserves fresh native and selected document ownership', () =>
    SOCKET_FAULTS.map((fault) => emitter(`PASS retained socket ownership ${fault}`, 'android-socket.ts'))],
]);

export const unitTestDiagnosticOwnerNames = [...diagnosticEmitters.keys()];

function invalid(message: string): never {
  throw new Error(`UNIT_TEST_OUTPUT: ${message}`);
}

export function hasForbiddenUnitTestOutputCharacter(value: string): boolean {
  for (const character of value) {
    const code = character.codePointAt(0)!;
    if (code <= 0x1f || (code >= 0x7f && code <= 0x9f) || code === 0x2028 || code === 0x2029) return true;
  }
  return false;
}

function caseOccurrences(inventory: UnitTestInventory): Map<string, number> {
  const countsByTitle = new Map<string, number>();
  const occurrences = new Map<string, number>();
  for (const entry of inventory.cases) {
    const key = `${entry.scope}\0${entry.name}`;
    const occurrence = (countsByTitle.get(key) || 0) + 1;
    countsByTitle.set(key, occurrence);
    occurrences.set(entry.id, occurrence);
  }
  return occurrences;
}

function validatedInventory(input: UnitTestInventory): UnitTestInventory {
  if (!input || !Array.isArray(input.cases) || input.cases.length > MAX_CASES) invalid('inventory is absent or exceeds the supported case limit');
  const manifest = generateUnitTestShardManifest(input);
  validateUnitTestShardManifest(manifest, input);
  return input;
}

function diagnosticEvents(inventory: UnitTestInventory): Map<string, SourceDiagnostic[]> {
  const occurrences = caseOccurrences(inventory);
  const events = new Map<string, SourceDiagnostic[]>();
  for (const entry of inventory.cases) {
    if (entry.scope !== 'outer') continue;
    const emit = diagnosticEmitters.get(entry.name);
    if (!emit) continue;
    const occurrence = occurrences.get(entry.id)!;
    if (occurrence !== 1) invalid(`diagnostic emitter ${entry.name} has an unsupported repeated title occurrence`);
    events.set(entry.id, emit(entry));
  }
  return events;
}

export function createUnitTestOutputContract(input: UnitTestInventory, selection: readonly string[]): UnitTestOutputContract {
  const inventory = validatedInventory(input);
  if (!Array.isArray(selection) || !selection.length) invalid('selection must contain at least one leaf id');
  const selectedIds = new Set<string>();
  const casesById = new Map(inventory.cases.map((entry) => [entry.id, entry]));
  const manifest = generateUnitTestShardManifest(inventory);
  const wrapper = casesById.get(manifest.aggregate.id);
  if (!wrapper || wrapper.name !== IOS_WRAPPER_NAME || wrapper.scope !== 'outer') invalid('inventory has no authoritative iOS wrapper');
  for (const id of selection) {
    if (typeof id !== 'string' || !id || selectedIds.has(id)) invalid('selection contains an invalid or duplicate id');
    const entry = casesById.get(id);
    if (!entry || id === wrapper.id) invalid(`selection contains an unknown or non-leaf id ${id}`);
    if (entry.scope === 'ios-inner' && !entry.name.startsWith('iOS ')) invalid(`nested iOS test ${id} has an unsupported display title`);
    selectedIds.add(id);
  }

  const occurrences = caseOccurrences(inventory);
  const ownerEvents = diagnosticEvents(inventory);
  const selectedInner = inventory.cases.filter((entry) => entry.scope === 'ios-inner' && selectedIds.has(entry.id));
  const events: OutputEvent[] = [];
  const appendSuccess = (entry: UnitTestCase, successKind: Extract<OutputEvent, { kind: 'success' }>['successKind'], line: string) => {
    events.push({
      kind: 'success', line, caseId: entry.id, caseName: entry.name, scope: entry.scope,
      occurrence: occurrences.get(entry.id)!, successKind,
    });
  };
  for (const entry of inventory.cases) {
    if (entry.scope !== 'outer') continue;
    if (entry.id === wrapper.id) {
      if (!selectedInner.length) continue;
      for (const inner of selectedInner) {
        appendSuccess(inner, 'ios-inner', `ok - ${inner.name}`);
      }
      appendSuccess(entry, 'ios-wrapper', `ok - ${entry.name}`);
      continue;
    }
    if (!selectedIds.has(entry.id)) continue;
    for (const [index, diagnostic] of (ownerEvents.get(entry.id) || []).entries()) {
      events.push({
        kind: 'diagnostic', line: diagnostic.line, ownerId: entry.id, ownerName: entry.name,
        ownerScope: 'outer', ownerOccurrence: occurrences.get(entry.id)!, ownerSequence: index + 1,
        sourceFile: diagnostic.sourceFile,
      });
    }
    appendSuccess(entry, 'outer', `ok - ${entry.name}`);
  }

  const expectedSuccessEvents = events.filter((event): event is Extract<OutputEvent, { kind: 'success' }> => event.kind === 'success');
  const expectedDiagnostics = events.filter((event): event is Extract<OutputEvent, { kind: 'diagnostic' }> => event.kind === 'diagnostic')
    .map((event) => ({ ...event, count: 1 }));
  const successCounts = new Map<string, number>();
  for (const event of expectedSuccessEvents) successCounts.set(event.caseName, (successCounts.get(event.caseName) || 0) + 1);
  return {
    selection: [...selection], events, expectedSuccessEvents,
    expectedSuccesses: Object.fromEntries(successCounts), expectedDiagnostics,
  };
}

function addOutputError(errors: string[], message: string): void {
  if (errors.length < MAX_OUTPUT_ERRORS) errors.push(message);
}

function parseLines(stream: string, label: string, errors: string[]): string[] {
  if (typeof stream !== 'string') {
    addOutputError(errors, `${label} is not text`);
    return [];
  }
  const normalized = stream.replace(/\r\n/gu, '\n');
  if (normalized.includes('\r')) addOutputError(errors, `${label} contains a bare carriage return`);
  if (normalized && !normalized.endsWith('\n')) addOutputError(errors, `${label} ends with an incomplete line`);
  if (!normalized) return [];
  const content = normalized.endsWith('\n') ? normalized.slice(0, -1) : normalized;
  const lines = content.split('\n');
  if (lines.length > MAX_ACCOUNTING_LINES) addOutputError(errors, `${label} exceeds the diagnostic accounting line limit`);
  const retained = lines.slice(0, MAX_ACCOUNTING_LINES);
  for (const [index, line] of retained.entries()) {
    if (!line) addOutputError(errors, `${label} contains an empty line at ${index + 1}`);
    if (hasForbiddenUnitTestOutputCharacter(line)) {
      addOutputError(errors, `${label} contains a control character at line ${index + 1}`);
    }
  }
  return retained;
}

function boundedLine(line: string): string {
  return line.length <= MAX_ACCOUNTING_LINE_LENGTH ? line : `${line.slice(0, MAX_ACCOUNTING_LINE_LENGTH)}...[line length ${line.length}]`;
}

function successTitle(line: string): string | undefined {
  if (!line.startsWith('ok - ')) return undefined;
  const title = line.slice('ok - '.length);
  return title.includes(' # SKIP') ? undefined : title;
}

function skipTitle(line: string): string | undefined {
  if (!line.startsWith('ok - ')) return undefined;
  const title = line.slice('ok - '.length);
  const marker = title.indexOf(' # SKIP');
  return marker < 0 ? undefined : title.slice(0, marker);
}

function failureTitle(line: string): string | undefined {
  if (!line.startsWith('not ok - ')) return undefined;
  const title = line.slice('not ok - '.length);
  const separator = title.indexOf(':');
  return separator < 0 ? title : title.slice(0, separator);
}

export function validateUnitTestOutput(
  inventory: UnitTestInventory,
  selection: readonly string[],
  stdout: string,
  stderr: string,
): UnitTestOutputAssessment {
  const errors: string[] = [];
  let contract: UnitTestOutputContract;
  try {
    contract = createUnitTestOutputContract(inventory, selection);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    return {
      expectedSuccesses: {}, actualSuccesses: {}, expectedSuccessEvents: [], actualSuccessEvents: [],
      expectedDiagnostics: [], actualDiagnostics: [], diagnostics: { stdout: [], stderr: [] }, skips: [], notOk: [],
      unexpectedSuccesses: [], missingSuccesses: [], outputMissing: !stdout && !stderr,
      outputErrors: [message], accepted: false,
    };
  }
  const stdoutLines = parseLines(stdout, 'stdout', errors);
  const stderrLines = parseLines(stderr, 'stderr', errors);
  const actualCounts = new Map<string, number>();
  const unexpectedSuccesses = new Set<string>();
  const skips: string[] = [];
  const notOk: string[] = [];
  const diagnostics = { stdout: [] as string[], stderr: [] as string[] };
  const actualDiagnostics: UnitTestOutputDiagnostic[] = [];
  const actualSuccessEvents: UnitTestOutputSuccess[] = [];
  const expectedOutput = contract.events;
  const collect = (stream: 'stdout' | 'stderr', lines: readonly string[]) => {
    for (const [index, line] of lines.entries()) {
      const expectedAtIndex = stream === 'stdout' ? expectedOutput[index] : undefined;
      const expectedSuccess = expectedAtIndex?.kind === 'success' && expectedAtIndex.line === line ? expectedAtIndex : undefined;
      const title = expectedSuccess?.caseName || successTitle(line);
      const skipped = expectedSuccess ? undefined : skipTitle(line);
      const failed = failureTitle(line);
      if (skipped !== undefined) {
        skips.push(skipped);
        diagnostics[stream].push(boundedLine(line));
        continue;
      }
      if (title !== undefined) {
        if (stream === 'stdout') actualCounts.set(title, (actualCounts.get(title) || 0) + 1);
        actualSuccessEvents.push({
          line: boundedLine(line), title: boundedLine(title), stream, lineNumber: index + 1,
          ...(expectedSuccess ? {
            caseId: expectedSuccess.caseId, caseName: expectedSuccess.caseName,
            scope: expectedSuccess.scope, occurrence: expectedSuccess.occurrence,
            successKind: expectedSuccess.successKind,
          } : {}),
        });
        if (!Object.hasOwn(contract.expectedSuccesses, title)) unexpectedSuccesses.add(title);
        continue;
      }
      if (failed !== undefined) notOk.push(boundedLine(failed));
      diagnostics[stream].push(boundedLine(line));
      const matched = stream === 'stdout' ? expectedOutput[index] : undefined;
      const expectedDiagnostic = matched?.kind === 'diagnostic' && matched.line === line ? matched : undefined;
      actualDiagnostics.push({
        line: boundedLine(line), stream, lineNumber: index + 1,
        ...(expectedDiagnostic ? {
          ownerId: expectedDiagnostic.ownerId, ownerName: expectedDiagnostic.ownerName,
          ownerScope: expectedDiagnostic.ownerScope, ownerOccurrence: expectedDiagnostic.ownerOccurrence,
          ownerSequence: expectedDiagnostic.ownerSequence, sourceFile: expectedDiagnostic.sourceFile,
        } : {}),
      });
    }
  };
  collect('stdout', stdoutLines);
  collect('stderr', stderrLines);
  if (stderrLines.length) addOutputError(errors, 'stderr contains output not emitted by a healthy selected test');
  if (stdoutLines.length !== expectedOutput.length) {
    addOutputError(errors, `stdout has ${stdoutLines.length} lines; source-derived selection requires ${expectedOutput.length}`);
  }
  const compared = Math.min(stdoutLines.length, expectedOutput.length);
  for (let index = 0; index < compared; index++) {
    if (stdoutLines[index] !== expectedOutput[index]!.line) {
      addOutputError(errors, `stdout line ${index + 1} differs from source-derived ${expectedOutput[index]!.kind} output`);
    }
  }
  for (const event of actualSuccessEvents) {
    if (event.stream === 'stderr') unexpectedSuccesses.add(event.title);
  }
  const missingSuccesses = Object.entries(contract.expectedSuccesses)
    .filter(([title, count]) => actualCounts.get(title) !== count)
    .map(([title]) => title);
  for (const [title, count] of actualCounts) {
    const expectedCount = Object.hasOwn(contract.expectedSuccesses, title) ? contract.expectedSuccesses[title]! : 0;
    if (expectedCount === 0 || count > expectedCount) unexpectedSuccesses.add(title);
  }
  if (skips.length) addOutputError(errors, 'selected output contains a skip marker');
  if (notOk.length) addOutputError(errors, 'selected output contains a failure diagnostic');
  if (unexpectedSuccesses.size) addOutputError(errors, 'selected output contains an unexpected or misplaced success line');
  if (missingSuccesses.length) addOutputError(errors, 'selected output is missing one or more required successes');
  const outputMissing = !stdout.length && !stderr.length;
  if (outputMissing) addOutputError(errors, 'selected output is empty');
  return {
    expectedSuccesses: contract.expectedSuccesses,
    actualSuccesses: Object.fromEntries(actualCounts),
    expectedSuccessEvents: contract.expectedSuccessEvents,
    actualSuccessEvents,
    expectedDiagnostics: contract.expectedDiagnostics,
    actualDiagnostics,
    diagnostics,
    skips,
    notOk,
    unexpectedSuccesses: [...unexpectedSuccesses],
    missingSuccesses,
    outputMissing,
    outputErrors: [...new Set(errors)],
    accepted: errors.length === 0,
  };
}

export function expectedUnitTestSuccessMultiplicity(
  inventory: UnitTestInventory,
  selection: readonly string[],
): Record<string, number> {
  return createUnitTestOutputContract(inventory, selection).expectedSuccesses;
}

export function expectedUnitTestDiagnostics(
  inventory: UnitTestInventory,
  selection: readonly string[],
): DiagnosticExpectation[] {
  return createUnitTestOutputContract(inventory, selection).expectedDiagnostics;
}

export const UNIT_TEST_STANDALONE_DIAGNOSTICS = [
  'Android attachment refusal regressions passed (35 cases)',
  'Host protocol replays are not native qualification. Picker-ready UI is synthetic, not recorded.',
] as const;
