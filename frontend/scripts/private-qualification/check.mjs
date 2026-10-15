import { readFileSync, existsSync, lstatSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
import vm from 'node:vm';
import { createRunScope, formatCompletionPointer, projectCompletionAccounting, reconcileInvocationAccounting, sha256, summarizeCommandAttempts } from './receipts.mjs';

if (process.argv.slice(2).join(' ') !== '--phase completion')
  throw new Error('Only --phase completion is supported');

const startedAt = performance.now();
const currentCaseCount = 79;
const currentVariantCount = 80;
const scope = createRunScope();
let result = 'failed';
let error;
let generated;
let priorHistory;
let testSummary;
let postRunAccounting;
let accountingAttempt;
let accountingStatus = 'unavailable';
let accountingErrors = [];
let controllerRecovery;
let completionPointer;
let stage = 'scope-begin';
let appendRevision06Invocation;
let collectPriorRevision06Invocations;
let generateRevision;
let recoverControllerGateInvocations;
const consoleLines = [];
let consoleBytes = 0;
const originalConsoleLog = console.log;

function recordConsole(args) {
  const line = args.map(value => typeof value === 'string' ? value : JSON.stringify(value)).join(' ');
  const bytes = Buffer.byteLength(line) + 1;
  if (consoleBytes + bytes > 1024 * 1024) throw new Error('Source test console receipt exceeded 1 MiB');
  consoleBytes += bytes;
  consoleLines.push(line);
}

const currentArtifactNames = [
  'BEGIN.json', 'failure.json', 'generation.json', 'controller-recovery.ndjson', 'parse-checks.ndjson', 'tests.mjs',
  'revision-06/source-manifest.json', 'revision-06/runner.derived.cjs', 'revision-06/runner-c1.patch',
  'revision-06/privateCompletion.cjs', 'revision-06/hosts.derived.js', 'revision-06/consumers.derived.js',
  'revision-06/jobDispatchers.derived.js', 'attempt-06.console.txt', 'attempt-06.ndjson',
  'attempt-06.summary.json', 'attempt-06.failures.ndjson', 'invocation-ledger.json', 'completion-result.json'
];

function currentArtifacts() {
  return currentArtifactNames.flatMap(path => {
    const pieces = path.split('/');
    let parent = scope.root;
    for (const piece of pieces.slice(0, -1)) {
      parent = join(parent, piece);
      let parentStat;
      try { parentStat = lstatSync(parent); }
      catch (error) { if (error.code === 'ENOENT') return []; throw error; }
      if (!parentStat.isDirectory() || parentStat.isSymbolicLink() || parentStat.uid !== process.getuid?.())
        throw new Error(`Current invocation receipt directory identity mismatch: ${path}`);
    }
    const fullPath = join(scope.root, path);
    if (!existsSync(fullPath)) return [];
    const stat = lstatSync(fullPath);
    if (!stat.isFile() || stat.isSymbolicLink() || stat.uid !== process.getuid?.())
      throw new Error(`Current invocation receipt identity mismatch: ${path}`);
    const bytes = readFileSync(fullPath);
    return [{ path, sha256: sha256(bytes), bytes: bytes.length }];
  });
}

function currentCaseOutcomes() {
  const summaryPath = join(scope.root, 'attempt-06.summary.json');
  const receiptPath = join(scope.root, 'attempt-06.ndjson');
  if (existsSync(summaryPath)) {
    const summary = JSON.parse(readFileSync(summaryPath, 'utf8'));
    const rows = existsSync(receiptPath) ? readFileSync(receiptPath, 'utf8').trimEnd().split('\n').filter(Boolean).map(JSON.parse) : [];
    const variantIds = rows.flatMap(row => Array.isArray(row.variants) ? row.variants : []);
    return { cases: summary.cases, passed: summary.passed, failed: summary.failed, caseIds: rows.map(row => row.id), failedIds: rows.filter(row => !row.passed).map(row => row.id), unexecutedCases: Math.max(0, currentCaseCount - summary.cases), ...(variantIds.length ? { variantIds, variantCount: variantIds.length } : {}) };
  }
  if (!existsSync(receiptPath)) return { cases: 0, passed: 0, failed: 0, caseIds: [], failedIds: [], unexecutedCases: currentCaseCount };
  const rows = readFileSync(receiptPath, 'utf8').trimEnd().split('\n').filter(Boolean).map(JSON.parse);
  const variantIds = rows.flatMap(row => Array.isArray(row.variants) ? row.variants : []);
  return { cases: rows.length, passed: rows.filter(row => row.passed).length, failed: rows.filter(row => !row.passed).length, caseIds: rows.map(row => row.id), failedIds: rows.filter(row => !row.passed).map(row => row.id), unexecutedCases: Math.max(0, currentCaseCount - rows.length), ...(variantIds.length ? { variantIds, variantCount: variantIds.length } : {}) };
}

function currentAllowanceProjection(generated) {
  const projection = generated?.causalAllowanceAmendment?.pointerRepairAmendment?.conditionalProjection;
  if (!projection) return null;
  return {
    replacementRootedInvocations: projection.rootedAfterNextCompletion,
    replacementTotalCommandAttempts: projection.commandAttemptsAfterNextCompletion,
    afterSubsequentControllerRootedInvocations: projection.rootedAfterSeparateControllerGate,
    afterSubsequentControllerTotalCommandAttempts: projection.commandAttemptsAfterSeparateControllerGate,
    notVerifiedTotals: projection.notVerifiedTotals,
    engineUsageInferred: projection.engineUsageInferred,
    capsRaised: projection.capsRaised,
    countersReset: projection.countersReset
  };
}

function writeInvocationLedger(status, cause) {
  if (!priorHistory) return null;
  const caseOutcomes = currentCaseOutcomes();
  const artifacts = currentArtifacts();
  const current = {
    runId: scope.id,
    root: scope.root,
    status,
    stage: status === 'passed' ? 'completion' : stage,
    cause: cause || null,
    expectedCases: currentCaseCount,
    expectedVariants: generated?.cases?.variants ?? null,
    caseOutcomes,
    artifacts
  };
  const all = [...priorHistory.invocations, current];
  const ledger = {
    schema: 1,
    command: 'bun frontend/scripts/private-qualification/check.mjs --phase completion',
    historicalRunIds: priorHistory.invocations.map(item => item.runId),
    priorTotals: priorHistory.totals,
    current,
    commandAttemptAccounting: summarizeCommandAttempts(all, priorHistory.commandAttemptAccounting.unrootedCommandEvents, priorHistory.commandAttemptAccounting.expectedControllerGateCount, priorHistory.controllerAssociations),
    cumulative: {
      invocations: all.length,
      passed: all.filter(item => item.status === 'passed').length,
      failed: all.filter(item => item.status === 'failed').length,
      recordedCases: all.reduce((sum, item) => sum + (item.caseOutcomes?.cases || 0), 0),
      passedCases: all.reduce((sum, item) => sum + (item.caseOutcomes?.passed || 0), 0),
      failedCases: all.reduce((sum, item) => sum + (item.caseOutcomes?.failed || 0), 0),
      unexecutedCases: all.reduce((sum, item) => sum + (item.unexecutedCases ?? item.caseOutcomes?.unexecutedCases ?? 0), 0),
      uniqueCaseIds: [...new Set(all.flatMap(item => item.caseOutcomes?.caseIds || []))].sort()
    }
  };
  scope.write('invocation-ledger.json', JSON.stringify(ledger, null, 2) + '\n');
  return ledger;
}

try {
  scope.begin({ phase: 'completion', nativeExecution: false, outputLimitBytes: 64 * 1024 * 1024 });
  stage = 'generation-module-initialization';
  const generator = await import('./generate.mjs');
  ({ appendRevision06Invocation, collectPriorRevision06Invocations, generateRevision, recoverControllerGateInvocations } = generator);
  stage = 'prior-history-collection';
  priorHistory = collectPriorRevision06Invocations();
  stage = 'controller-gate-recovery';
  controllerRecovery = recoverControllerGateInvocations(priorHistory, scope);
  priorHistory = collectPriorRevision06Invocations();
  stage = 'source-generation';
  generated = await generateRevision(scope, priorHistory, controllerRecovery);
  const revisionDirectory = join(scope.root, 'revision-06');
  const runner = readFileSync(join(revisionDirectory, 'runner.derived.cjs'), 'utf8');
  const support = readFileSync(join(revisionDirectory, 'privateCompletion.cjs'), 'utf8');
  const hosts = readFileSync(join(revisionDirectory, 'hosts.derived.js'), 'utf8');
  const consumers = readFileSync(join(revisionDirectory, 'consumers.derived.js'), 'utf8');
  const jobDispatchers = readFileSync(join(revisionDirectory, 'jobDispatchers.derived.js'), 'utf8');
  const multiplexer = readFileSync(join(revisionDirectory, 'multiplexer.derived.js'), 'utf8');
  const parseChecks = [];
  stage = 'source-parse-checks';
  for (const [name, source] of [
    ['runner.derived.cjs', runner],
    ['privateCompletion.cjs', support],
    ['hosts.derived.js', hosts],
    ['consumers.derived.js', consumers],
    ['jobDispatchers.derived.js', jobDispatchers],
    ['multiplexer.derived.js', multiplexer]
  ]) {
    new vm.Script(source, { filename: name });
    parseChecks.push({ name, status: 'passed', sha256: sha256(source), bytes: Buffer.byteLength(source) });
  }
  scope.write('parse-checks.ndjson', parseChecks.map(check => JSON.stringify(check)).join('\n') + '\n');
  const testPath = join(scope.root, 'tests.mjs');
  stage = 'source-test-execution';
  console.log = (...args) => recordConsole(args);
  await import(pathToFileURL(testPath).href + `?run=${scope.id}`);
  console.log = originalConsoleLog;
  const summaryPath = join(scope.root, 'attempt-06.summary.json');
  testSummary = JSON.parse(readFileSync(summaryPath, 'utf8'));
  const cases = readFileSync(join(scope.root, 'attempt-06.ndjson'), 'utf8').trimEnd().split('\n').map(JSON.parse);
  stage = 'case-ledger-validation';
  const expectedIds = Array.from({ length: currentCaseCount }, (_, index) => `C${String(index + 1).padStart(2, '0')}`);
  if (cases.length !== expectedIds.length || cases.some((test, index) => test.id !== expectedIds[index] || !test.passed || test.calls.native !== 0))
    throw new Error('Fresh C01-C79 actual-source receipt did not pass its bound case ledger');
  if (testSummary.cases !== currentCaseCount || testSummary.passed !== currentCaseCount || testSummary.failed !== 0)
    throw new Error('Fresh actual-source test summary is incomplete or failed');
  const c43 = cases.find(test => test.id === 'C43');
  if (c43.calls.retain !== 0 || c43.calls.fork !== 0)
    throw new Error('Strengthened C43 did not refuse retention and fork');
  const c54 = cases.find(test => test.id === 'C54');
  const c55 = cases.find(test => test.id === 'C55');
  const c68 = cases.find(test => test.id === 'C68');
  const c69 = cases.find(test => test.id === 'C69');
  const c70 = cases.find(test => test.id === 'C70');
  const c71 = cases.find(test => test.id === 'C71');
  const c76 = cases.find(test => test.id === 'C76');
  const c77 = cases.find(test => test.id === 'C77');
  const c78 = cases.find(test => test.id === 'C78');
  const c79 = cases.find(test => test.id === 'C79');
  if (c54.calls.retain !== 0 || c55.calls.retain !== 0 || c68.calls.fork !== 0 || c68.calls.retain !== 0 || c69.calls.retain !== 1 || !c70.passed || !c71.passed || c76.calls.fork !== 1 || c76.calls.retain !== 1 || c77.calls.fork !== 1 || c78.calls.native !== 0 || c79.calls.fork !== 0 || c79.calls.retain !== 1)
    throw new Error('Dispatcher refusal, incomplete stop, private sender, reporter ordering, admission deadline, or late retention violated its bounded behavior');
  const consoleText = consoleLines.join('\n') + '\n';
  scope.write('attempt-06.console.txt', consoleText);
  scope.write('attempt-06.failures.ndjson', '');
  stage = 'completion-pointer-preflight';
  const preflightInvocations = [...priorHistory.invocations, { runId: scope.id, status: 'passed' }];
  const preflightCommandAttempts = summarizeCommandAttempts(
    preflightInvocations,
    priorHistory.commandAttemptAccounting.unrootedCommandEvents,
    priorHistory.commandAttemptAccounting.expectedControllerGateCount,
    priorHistory.controllerAssociations
  );
  const preflightAccounting = projectCompletionAccounting({
    commandAttemptAccounting: preflightCommandAttempts,
    cumulativeRootedInvocations: preflightInvocations.length,
    ledgerAppend: { path: priorHistory.journal.path, sequence: priorHistory.journal.entries + 1, sha256: '0'.repeat(64), bytes: 64 * 1024 * 1024 },
    recoveryManifest: controllerRecovery?.manifestIdentity,
    recoveredRunIds: controllerRecovery?.recoveredRunIds ?? [],
    allowanceProjection: currentAllowanceProjection(generated)
  });
  const largestReceiptIdentity = path => ({ path, sha256: '0'.repeat(64), bytes: 64 * 1024 * 1024 });
  formatCompletionPointer({
    status: 'passed', runId: scope.id, root: scope.root, stage: 'completion',
    sourceTests: { cases: cases.length, passed: testSummary.passed, failed: testSummary.failed, variants: currentVariantCount },
    summary: largestReceiptIdentity('summary.json'),
    terminalReceipt: largestReceiptIdentity('completion-result.json'),
    caseReceipt: largestReceiptIdentity('attempt-06.ndjson'),
    caseSummary: largestReceiptIdentity('attempt-06.summary.json'),
    invocationLedger: largestReceiptIdentity('invocation-ledger.json'),
    accounting: preflightAccounting
  });
  stage = 'passed-receipt-finalization';
  const invocationLedger = writeInvocationLedger('passed', null);
  const receiptNames = ['BEGIN.json', 'generation.json', 'parse-checks.ndjson', 'revision-06/source-manifest.json', 'revision-06/runner-c1.patch', 'attempt-06.console.txt', 'attempt-06.ndjson', 'attempt-06.summary.json', 'attempt-06.failures.ndjson', 'invocation-ledger.json'];
  const receiptIdentities = receiptNames.map(path => {
    const bytes = readFileSync(join(scope.root, path));
    return { path, sha256: sha256(bytes), bytes: bytes.length };
  });
  const summaryReceipt = {
    status: 'passed',
    runId: scope.id,
    command: 'bun frontend/scripts/private-qualification/check.mjs --phase completion',
    sourceTests: { cases: cases.length, passed: testSummary.passed, failed: testSummary.failed, variants: currentVariantCount },
    controllerGateRecovery: controllerRecovery,
    causalAllowanceAmendment: generated.causalAllowanceAmendment,
    freshReceipts: receiptIdentities,
    revision06PriorInvocations: { path: 'invocation-ledger.json', ...priorHistory.totals },
    commandAttemptAccounting: invocationLedger.commandAttemptAccounting,
    baselineDifferences: {
      C43: { revision05ObservedRetainCalls: 1, currentExpectedRetainCalls: c43.calls.retain },
      C32: 'Expired parent deadline remains a no-stop case; C61 covers join cutoff earlier than parent deadline.'
    },
    sourceManifest: 'revision-06/source-manifest.json',
    originalFailureHistory: '/tmp/herdr-scroll-p1-output-retention-completion-v2/failure-history.json'
  };
  scope.write('completion-result.json', JSON.stringify(summaryReceipt, null, 2) + '\n');
  stage = 'completion';
  result = 'passed';
} catch (caught) {
  error = caught;
  console.log = originalConsoleLog;
  try {
    if (consoleLines.length && !existsSync(join(scope.root, 'attempt-06.console.txt')))
      scope.write('attempt-06.console.txt', consoleLines.join('\n') + '\n');
    if (testSummary?.failures && !existsSync(join(scope.root, 'attempt-06.failures.ndjson')))
      scope.write('attempt-06.failures.ndjson', testSummary.failures.map(failure => JSON.stringify(failure)).join('\n') + '\n');
    scope.write('failure.json', JSON.stringify({ runId: scope.id, status: 'failed', stage, message: String(caught), stack: caught?.stack || null }, null, 2) + '\n');
  } catch (receiptError) {
    error = new AggregateError([caught, receiptError], 'Completion failure and failure receipt could not both be recorded', { cause: caught });
  }
} finally {
  console.log = originalConsoleLog;
  let cumulativeInvocations = null;
  let commandAttemptAccounting = null;
  const accountingFailureDetails = [];
  const originalExecutionError = error;
  const captureAccountingFailure = (stageName, caught) => {
    accountingFailureDetails.push({ stage: stageName, message: String(caught), stack: caught?.stack || null });
    accountingErrors.push(caught);
  };
  try {
    if (priorHistory && !existsSync(join(scope.root, 'invocation-ledger.json'))) {
      try { writeInvocationLedger(result, error ? String(error) : 'Completion did not reach a passing terminal result'); }
      catch (ledgerError) { captureAccountingFailure('local-invocation-ledger', ledgerError); }
    }
    if (priorHistory && typeof appendRevision06Invocation === 'function' && typeof collectPriorRevision06Invocations === 'function' && accountingErrors.length === 0) {
      accountingAttempt = reconcileInvocationAccounting(
        () => appendRevision06Invocation({
          runId: scope.id,
          root: scope.root,
          status: result,
          stage: result === 'passed' ? 'completion' : stage,
          cause: error ? String(error) : null,
          expectedCases: currentCaseCount,
          expectedVariants: generated?.cases?.variants ?? null,
          caseOutcomes: currentCaseOutcomes(),
          artifacts: currentArtifacts()
        }),
        () => {
          const reconciled = collectPriorRevision06Invocations();
          if (!reconciled.invocations.some(item => item.runId === scope.id) || reconciled.totals.invocations !== priorHistory.invocations.length + 1)
            throw new Error('Post-run invocation accounting did not reconcile the fresh run');
          return reconciled;
        }
      );
      postRunAccounting = accountingAttempt.appendResult;
      cumulativeInvocations = accountingAttempt.reconciled;
      if (accountingAttempt.appendError) captureAccountingFailure('revision-06-invocation-append', accountingAttempt.appendError);
      if (accountingAttempt.reconciliationError) captureAccountingFailure('revision-06-invocation-reconciliation', accountingAttempt.reconciliationError);
    }
    accountingStatus = accountingAttempt?.status ?? (priorHistory ? 'incomplete' : 'unavailable');
    if (cumulativeInvocations) {
      commandAttemptAccounting = cumulativeInvocations.commandAttemptAccounting;
      if (commandAttemptAccounting?.unreconciledControllerGates > 0) {
        accountingStatus = 'incomplete';
        captureAccountingFailure('controller-association-reconciliation', new Error(`Unreconciled controller shell events remain: ${commandAttemptAccounting.unreconciledControllerGates}`));
      }
    }
    if (priorHistory && !accountingAttempt && accountingErrors.length === 0) {
      accountingStatus = 'incomplete';
      captureAccountingFailure('revision-06-invocation-accounting', new Error('Invocation accounting was not attempted'));
    }
    if (accountingErrors.length) {
      accountingStatus = 'incomplete';
      const causes = [error, ...accountingErrors].filter(Boolean);
      error = causes.length > 1 ? new AggregateError(causes, 'Completion execution and accounting failures are both retained', { cause: causes[0] }) : causes[0];
      if (result === 'passed') {
        result = 'failed';
        stage = accountingFailureDetails.at(-1)?.stage || 'post-run-accounting';
      }
      if (!existsSync(join(scope.root, 'failure.json'))) {
        try {
          scope.write('failure.json', JSON.stringify({
            runId: scope.id, status: 'failed', stage, executionStatus: testSummary ? 'passed' : 'not-passed',
            accountingStatus, message: error ? String(error) : 'Invocation accounting incomplete',
            accountingFailures: accountingFailureDetails
          }, null, 2) + '\n');
        } catch (failureReceiptError) {
          captureAccountingFailure('accounting-failure-receipt', failureReceiptError);
        }
      }
    }
    if (accountingErrors.length && !accountingFailureDetails.length)
      captureAccountingFailure('accounting-finalization', new Error('Accounting finalization failed without retained diagnostics'));
    const recoveredAssociations = cumulativeInvocations?.controllerAssociations ?? priorHistory?.controllerAssociations ?? [];
    const recoveryManifestIdentity = controllerRecovery?.manifestIdentity ?? cumulativeInvocations?.controllerRecoveryManifest?.manifestIdentity ?? null;
    scope.finalize({
      postRunAccounting,
      accountingAttempt: accountingAttempt ? {
        status: accountingAttempt.status,
        appendResult: accountingAttempt.appendResult,
        appendError: accountingAttempt.appendError ? String(accountingAttempt.appendError) : null,
        reconciliationError: accountingAttempt.reconciliationError ? String(accountingAttempt.reconciliationError) : null
      } : null,
      cumulativeInvocations: cumulativeInvocations?.totals ?? null,
      accountingStatus,
      accountingFailures: accountingFailureDetails,
      status: result,
      failedStage: result === 'passed' ? null : stage,
      elapsedMs: performance.now() - startedAt,
      sourceTests: testSummary ? { cases: testSummary.cases, passed: testSummary.passed, failed: testSummary.failed, variants: currentVariantCount } : null,
      controllerGateRecovery: controllerRecovery,
      recoveredControllerAssociations: recoveredAssociations.map(item => ({ occurrence: item.occurrence, runId: item.runId, classification: item.classification })),
      commandAttemptAccounting,
      originalExecutionError: originalExecutionError ? { message: String(originalExecutionError), stack: originalExecutionError.stack || null } : null,
      error: error ? { message: String(error), stack: error.stack || null } : null,
      generated: generated || null
    });
    const identity = path => {
      const fullPath = join(scope.root, path);
      if (!existsSync(fullPath)) return null;
      const bytes = readFileSync(fullPath);
      return { path, sha256: sha256(bytes), bytes: bytes.length };
    };
    completionPointer = formatCompletionPointer({
      status: result,
      runId: scope.id,
      root: scope.root,
      stage: result === 'passed' ? 'completion' : stage,
      sourceTests: testSummary ? { cases: testSummary.cases, passed: testSummary.passed, failed: testSummary.failed, variants: currentVariantCount } : null,
      summary: identity('summary.json'),
      terminalReceipt: identity(result === 'passed' ? 'completion-result.json' : 'failure.json'),
      caseReceipt: identity('attempt-06.ndjson'),
      caseSummary: identity('attempt-06.summary.json'),
      invocationLedger: identity('invocation-ledger.json'),
      accounting: projectCompletionAccounting({
        commandAttemptAccounting,
        cumulativeRootedInvocations: cumulativeInvocations?.totals?.invocations ?? null,
        ledgerAppend: postRunAccounting,
        recoveryManifest: recoveryManifestIdentity,
        recoveredRunIds: recoveredAssociations.map(item => item.runId),
        allowanceProjection: currentAllowanceProjection(generated),
        accountingStatus
      })
    });
  } catch (receiptError) {
    const causes = [error, receiptError].filter(Boolean);
    error = causes.length > 1 ? new AggregateError(causes, 'Completion failure and final accounting evidence are both retained', { cause: causes[0] }) : receiptError;
    result = 'failed';
    const fallbackFailure = {
      runId: scope.id, status: 'failed', stage: 'failure-summary-finalization', accountingStatus: 'incomplete',
      message: String(error), stack: error?.stack || null, accountingFailures: accountingFailureDetails
    };
    try {
      if (!existsSync(join(scope.root, 'summary.json'))) scope.write('summary.json', JSON.stringify(fallbackFailure, null, 2) + '\n');
      if (!existsSync(join(scope.root, 'failure.json'))) scope.write('failure.json', JSON.stringify(fallbackFailure, null, 2) + '\n');
      completionPointer = formatCompletionPointer({
        status: 'failed', runId: scope.id, root: scope.root, stage: fallbackFailure.stage,
        sourceTests: testSummary ? { cases: testSummary.cases, passed: testSummary.passed, failed: testSummary.failed, variants: currentVariantCount } : null,
        summary: existsSync(join(scope.root, 'summary.json')) ? { path: 'summary.json', sha256: sha256(readFileSync(join(scope.root, 'summary.json'))), bytes: lstatSync(join(scope.root, 'summary.json')).size } : null,
        terminalReceipt: existsSync(join(scope.root, 'failure.json')) ? { path: 'failure.json', sha256: sha256(readFileSync(join(scope.root, 'failure.json'))), bytes: lstatSync(join(scope.root, 'failure.json')).size } : null,
        caseReceipt: null, caseSummary: null, invocationLedger: null,
        accounting: projectCompletionAccounting({ commandAttemptAccounting: null, cumulativeRootedInvocations: null, ledgerAppend: postRunAccounting, recoveryManifest: null, recoveredRunIds: [], accountingStatus: 'incomplete' })
      });
    } catch (pointerError) {
      originalConsoleLog(JSON.stringify({ status: 'failed', runId: scope.id, root: scope.root, receiptError: String(receiptError), pointerError: String(pointerError) }));
    }
  }
}

if (result !== 'passed') {
  process.exitCode = 1;
  if (error) originalConsoleLog(`FAILED: ${error.stack || error}`);
}
if (completionPointer) originalConsoleLog(completionPointer);
