import { mkdirSync, lstatSync, statfsSync, writeFileSync, appendFileSync, readdirSync } from 'node:fs';
import { createHash, randomUUID } from 'node:crypto';
import { isAbsolute, resolve, sep } from 'node:path';

export const maxSourceBytes = 64 * 1024 * 1024;
const maxEntries = 256;
const maxDepth = 8;
const minimumHeadroom = maxSourceBytes * 2;

export function sha256(value) {
  return createHash('sha256').update(value).digest('hex');
}

export function validateBoundedEvidenceSnapshot(snapshot, expectedUid, maxBytes) {
  if (!snapshot || snapshot.isFile !== true || snapshot.isSymbolicLink !== false || snapshot.uid !== expectedUid || !Number.isSafeInteger(snapshot.size) || snapshot.size < 0 || !Number.isSafeInteger(snapshot.mode) || snapshot.mode < 0 || typeof snapshot.dev !== 'string' || typeof snapshot.ino !== 'string' || typeof snapshot.mtimeNs !== 'string' || typeof snapshot.ctimeNs !== 'string' || !Number.isSafeInteger(maxBytes) || maxBytes < 0)
    throw new Error('Bounded evidence identity mismatch');
  if (snapshot.size > maxBytes) throw new Error('Bounded evidence exceeds its configured byte limit');
  return snapshot;
}

export function validateBoundedEvidenceRead(before, after, bytesRead, expectedUid, maxBytes) {
  validateBoundedEvidenceSnapshot(before, expectedUid, maxBytes);
  validateBoundedEvidenceSnapshot(after, expectedUid, maxBytes);
  const stableFields = ['dev', 'ino', 'uid', 'mode', 'size', 'mtimeNs', 'ctimeNs', 'isFile', 'isSymbolicLink'];
  if (!Number.isSafeInteger(bytesRead) || bytesRead !== before.size || stableFields.some(field => before[field] !== after[field]))
    throw new Error('Bounded evidence changed while being read');
  return { size: before.size };
}

function invocationSummary(stdout, path) {
  const summaries = [];
  for (const line of stdout.split(/\r?\n/)) {
    if (!line.startsWith('{')) continue;
    try {
      const value = JSON.parse(line);
      if (value && /^[0-9a-f-]{36}$/.test(value.runId || '') && typeof value.root === 'string')
        summaries.push(value);
    } catch {}
  }
  if (summaries.length !== 1) throw new Error(`Completion shell result has an ambiguous outer receipt: ${path}`);
  return summaries[0];
}

export function extractCompletionGateShellResults(document, command, callsitePrefix, expectedCount = 2, maxEntries = 4096, maxOutputBytes = 10 * 1024 * 1024) {
  const completed = document?.journal?.completed ?? document?.completed;
  if (!completed || typeof completed !== 'object' || Array.isArray(completed))
    throw new Error('Recorded workflow journal has no completed shell-result map');
  if (typeof callsitePrefix !== 'string' || !/^shell\/callsite%3A\d+%3A\d+\/+$/.test(callsitePrefix) || !Number.isInteger(expectedCount) || expectedCount < 1)
    throw new Error('Recorded workflow shell callsite contract is malformed');
  const entries = Object.entries(completed);
  if (entries.length > maxEntries) throw new Error('Recorded workflow completion-entry limit exceeded');
  const candidates = entries.filter(([path]) => path.startsWith(callsitePrefix));
  if (candidates.length !== expectedCount) throw new Error(`Recorded workflow shell callsite count mismatch: ${candidates.length}`);
  const results = [];
  const occurrences = new Set();
  for (const [path, entry] of candidates) {
    const value = entry?.value;
    const occurrence = path.slice(callsitePrefix.length);
    if (entry?.path !== path || !/^occurrence%3A[1-9]\d*$/.test(occurrence) || occurrences.has(occurrence))
      throw new Error(`Recorded workflow shell callsite identity is malformed: ${path}`);
    occurrences.add(occurrence);
    const stderr = value?.stderr;
    if (typeof value?.stdout !== 'string' || Buffer.byteLength(value.stdout) > maxOutputBytes || typeof stderr !== 'string' || Buffer.byteLength(stderr) > maxOutputBytes || !Number.isInteger(value.exitCode))
      throw new Error(`Recorded completion shell result is malformed: ${path}`);
    results.push({
      path,
      occurrence,
      command,
      exitCode: value.exitCode,
      stdout: value.stdout,
      stdoutBytes: Buffer.byteLength(value.stdout),
      stdoutSHA256: sha256(value.stdout),
      stderr,
      stderrBytes: Buffer.byteLength(stderr),
      stderrSHA256: sha256(stderr),
      summary: invocationSummary(value.stdout, path)
    });
  }
  for (let index = 0; index < expectedCount; index += 1) {
    if (!occurrences.has(`occurrence%3A${index + 1}`)) throw new Error('Recorded workflow shell callsite occurrences are incomplete');
  }
  return results;
}

function invocationFingerprint(record) {
  const outcomes = record.caseOutcomes;
  const artifacts = (record.artifacts || []).map(item => [item.path, item.sha256, item.bytes]).sort((a, b) => a[0].localeCompare(b[0]));
  return JSON.stringify({
    runId: record.runId,
    root: record.root,
    status: record.status,
    stage: record.stage,
    expectedCases: record.expectedCases,
    expectedVariants: record.expectedVariants ?? null,
    caseOutcomes: outcomes,
    artifacts
  });
}

export function invocationRecordFingerprint(record) {
  return sha256(invocationFingerprint(record));
}

function caseOutcomeIdentity(outcomes) {
  return JSON.stringify([
    outcomes?.cases,
    outcomes?.passed,
    outcomes?.failed,
    outcomes?.caseIds,
    outcomes?.failedIds,
    outcomes?.unexecutedCases,
    outcomes?.variantIds ?? [],
    outcomes?.variantCount ?? null
  ]);
}

export function assessRecoveredInvocationLedger(ledger, expected) {
  if (ledger === null)
    return { status: 'historically-absent', omission: 'No invocation-ledger.json existed for this retained completion attempt; the new append-only record is derived from validated run receipts.' };
  const current = ledger?.current;
  const prior = ledger?.priorTotals;
  const cumulative = ledger?.cumulative;
  const variantOmitted = current?.expectedVariants === undefined || current?.expectedVariants === null;
  if (variantOmitted && !Number.isInteger(expected.expectedVariants))
    throw new Error(`Retained invocation ledger omitted variants without a validating receipt: ${expected.runId}`);
  if (ledger.schema !== 1 || ledger.command !== expected.command || !current || current.runId !== expected.runId || current.root !== expected.root || current.status !== expected.status || current.stage !== expected.stage || current.expectedCases !== expected.expectedCases || (!variantOmitted && current.expectedVariants !== expected.expectedVariants) || caseOutcomeIdentity(current.caseOutcomes) !== caseOutcomeIdentity(expected.caseOutcomes) || !Array.isArray(current.artifacts) || !Array.isArray(ledger.historicalRunIds) || new Set(ledger.historicalRunIds).size !== ledger.historicalRunIds.length || ledger.historicalRunIds.includes(expected.runId) || !prior || !cumulative || prior.invocations !== ledger.historicalRunIds.length || prior.passed + prior.failed !== prior.invocations || cumulative.invocations !== prior.invocations + 1 || cumulative.passed !== prior.passed + (expected.status === 'passed' ? 1 : 0) || cumulative.failed !== prior.failed + (expected.status === 'failed' ? 1 : 0) || cumulative.recordedCases !== prior.recordedCases + expected.caseOutcomes.cases || cumulative.passedCases !== prior.passedCases + expected.caseOutcomes.passed || cumulative.failedCases !== prior.failedCases + expected.caseOutcomes.failed || cumulative.unexecutedCases !== prior.unexecutedCases + expected.caseOutcomes.unexecutedCases)
    throw new Error(`Retained invocation ledger conflicts with validated run evidence: ${expected.runId}`);
  const identities = new Map(expected.artifacts.map(item => [item.path, item]));
  const seenPaths = new Set();
  for (const identity of current.artifacts) {
    const actual = identities.get(identity?.path);
    if (seenPaths.has(identity?.path) || !actual || actual.sha256 !== identity.sha256 || actual.bytes !== identity.bytes)
      throw new Error(`Retained invocation ledger artifact identity mismatch: ${expected.runId}/${identity?.path}`);
    seenPaths.add(identity.path);
  }
  const expectedIds = [...new Set([...(prior.uniqueCaseIds || []), ...expected.caseOutcomes.caseIds])].sort();
  if (JSON.stringify(cumulative.uniqueCaseIds) !== JSON.stringify(expectedIds))
    throw new Error(`Retained invocation ledger case-ID totals mismatch: ${expected.runId}`);
  return variantOmitted
    ? { status: 'validated-with-omissions', omission: 'Historical invocation ledger omitted expectedVariants; the value is derived from the independently validated completion/source-manifest receipts.' }
    : { status: 'validated', omission: null };
}

export function summarizeCommandAttempts(invocations, unrootedEvents, expectedControllerGates, controllerAssociations = []) {
  if (!Array.isArray(invocations) || !Array.isArray(unrootedEvents) || !Number.isInteger(expectedControllerGates) || expectedControllerGates < 0 || !Array.isArray(controllerAssociations))
    throw new Error('Command-attempt accounting inputs are incomplete');
  const associatedRunIds = new Set();
  const eventIds = new Set();
  if (controllerAssociations.length) validateControllerRecoveryAssociations(controllerAssociations, invocations, controllerAssociations.length);
  for (const association of controllerAssociations) {
    if (associatedRunIds.has(association.runId) || eventIds.has(association.eventIdentity))
      throw new Error('Command-attempt controller association is duplicate or unaccounted');
    associatedRunIds.add(association.runId);
    eventIds.add(association.eventIdentity);
  }
  const reconciledControllerGates = controllerAssociations.length;
  if (reconciledControllerGates > expectedControllerGates)
    throw new Error('Command-attempt accounting contains excess recovered controller gates');
  return {
    rootedInvocations: invocations.length,
    unrootedCommandEvents: unrootedEvents,
    unrootedFailedAttempts: unrootedEvents.filter(item => item.status === 'failed').length,
    unrootedUnexecutedCaseSlots: unrootedEvents.reduce((sum, item) => sum + (item.unexecutedCaseSlots || 0), 0),
    reconciledControllerGates,
    unreconciledControllerGates: expectedControllerGates - reconciledControllerGates,
    reconciledCommandAttempts: invocations.length + unrootedEvents.length,
    expectedControllerGateCount: expectedControllerGates
  };
}

export function controllerShellAssociationIdentity({ occurrence, runId, root, status, exitCode, stdoutSHA256, stdoutBytes }) {
  if (!/^occurrence%3A[1-9]\d*$/.test(occurrence || '') || !/^[0-9a-f-]{36}$/.test(runId || '') || root !== `/tmp/herdr-retention-v3-${runId}` || !['passed', 'failed'].includes(status) || !Number.isInteger(exitCode) || !/^[0-9a-f]{64}$/.test(stdoutSHA256 || '') || !Number.isSafeInteger(stdoutBytes) || stdoutBytes < 0 || stdoutBytes > 10 * 1024 * 1024)
    throw new Error('Controller shell association identity is invalid');
  return sha256(JSON.stringify({ occurrence, runId, root, status, exitCode, stdoutSHA256, stdoutBytes }));
}

export function reconcileControllerGateAssociations(candidates, knownRecords, expectedAssociations) {
  if (!Array.isArray(candidates) || !Array.isArray(knownRecords) || !Number.isInteger(expectedAssociations) || candidates.length !== expectedAssociations)
    throw new Error('Controller shell association count mismatch');
  const known = new Map(knownRecords.map(record => [record.runId, record]));
  if (known.size !== knownRecords.length) throw new Error('Known invocation identities are not unique');
  const seenEvents = new Set();
  const seenRuns = new Set();
  const seenOccurrences = new Set();
  const associations = candidates.map(candidate => {
    const record = candidate.invocation;
    const eventIdentity = controllerShellAssociationIdentity(candidate);
    if (!record || record.provenance?.associationId !== eventIdentity || record.runId !== candidate.runId || record.root !== candidate.root || record.status !== candidate.status || seenEvents.has(eventIdentity) || seenRuns.has(record.runId) || seenOccurrences.has(candidate.occurrence))
      throw new Error('Controller shell results contain a repeated or conflicting outer invocation identity');
    seenEvents.add(eventIdentity);
    seenRuns.add(record.runId);
    seenOccurrences.add(candidate.occurrence);
    const fingerprint = invocationRecordFingerprint(record);
    const existing = known.get(record.runId);
    if (existing && invocationRecordFingerprint(existing) !== fingerprint)
      throw new Error(`Controller shell invocation conflicts with retained accounting: ${record.runId}`);
    if (existing?.provenance?.associationId && existing.provenance.associationId !== eventIdentity)
      throw new Error(`Controller shell association conflicts with retained provenance: ${record.runId}`);
    const classification = existing
      ? existing.retainedInInvocationJournal ? 'known-invocation-journal' : 'known-fixed-history'
      : 'new-root';
    return {
      occurrence: candidate.occurrence,
      path: candidate.path,
      eventIdentity,
      runId: record.runId,
      root: record.root,
      status: record.status,
      exitCode: candidate.exitCode,
      stdoutSHA256: candidate.stdoutSHA256,
      stdoutBytes: candidate.stdoutBytes,
      invocationFingerprint: fingerprint,
      classification,
      invocation: record
    };
  });
  if (Array.from({ length: expectedAssociations }, (_, index) => `occurrence%3A${index + 1}`).some(occurrence => !seenOccurrences.has(occurrence)))
    throw new Error('Controller shell association omitted a required occurrence');
  return { associations, newRecords: associations.filter(item => item.classification === 'new-root').map(item => item.invocation) };
}

export function validateControllerRecoveryAssociations(associations, knownRecords, expectedAssociations) {
  if (!Array.isArray(associations) || associations.length !== expectedAssociations || !Array.isArray(knownRecords))
    throw new Error('Controller recovery association manifest is incomplete');
  const known = new Map(knownRecords.map(record => [record.runId, record]));
  if (known.size !== knownRecords.length) throw new Error('Known invocation identities are not unique');
  const seenEvents = new Set();
  const seenRuns = new Set();
  const seenOccurrences = new Set();
  for (const association of associations) {
    const eventIdentity = controllerShellAssociationIdentity(association);
    const record = known.get(association.runId);
    if (!record || record.root !== association.root || record.status !== association.status || invocationRecordFingerprint(record) !== association.invocationFingerprint || eventIdentity !== association.eventIdentity || seenEvents.has(eventIdentity) || seenRuns.has(association.runId) || seenOccurrences.has(association.occurrence))
      throw new Error(`Controller recovery association does not match a unique retained invocation: ${association.runId}`);
    if (!['known-fixed-history', 'known-invocation-journal', 'new-root-appended'].includes(association.classification))
      throw new Error(`Controller recovery association classification is invalid: ${association.runId}`);
    if ((association.classification === 'known-fixed-history' && record.retainedInInvocationJournal) || (association.classification === 'known-invocation-journal' && !record.retainedInInvocationJournal) || (association.classification === 'new-root-appended' && (!record.retainedInInvocationJournal || record.provenance?.associationId !== eventIdentity)))
      throw new Error(`Controller recovery association provenance mismatch: ${association.runId}`);
    if (record.provenance?.associationId && record.provenance.associationId !== eventIdentity)
      throw new Error(`Controller recovery association conflicts with invocation provenance: ${association.runId}`);
    seenEvents.add(eventIdentity);
    seenRuns.add(association.runId);
    seenOccurrences.add(association.occurrence);
  }
  if (Array.from({ length: expectedAssociations }, (_, index) => `occurrence%3A${index + 1}`).some(occurrence => !seenOccurrences.has(occurrence)))
    throw new Error('Controller recovery manifest omitted a required shell occurrence');
  return associations;
}

export function reuseControllerRecoveryManifest(manifest, knownRecords, expectedAssociations) {
  if (manifest === null || manifest === undefined) return null;
  if (!Array.isArray(manifest.associations) || !Array.isArray(manifest.recoveredRunIds) || !Array.isArray(manifest.newlyAppendedRunIds) || JSON.stringify(manifest.recoveredRunIds) !== JSON.stringify(manifest.associations.map(item => item.runId)) || JSON.stringify(manifest.newlyAppendedRunIds) !== JSON.stringify(manifest.associations.filter(item => item.classification === 'new-root-appended').map(item => item.runId)))
    throw new Error('Stored controller recovery manifest identities are inconsistent');
  const identity = manifest.manifestIdentity;
  if (!identity || typeof identity.path !== 'string' || !identity.path || identity.path.length > 180 || identity.path.startsWith('/') || identity.path.split(/[\\/]/).includes('..') || !/^[0-9a-f]{64}$/.test(identity.sha256 || '') || !Number.isSafeInteger(identity.bytes) || identity.bytes < 0 || identity.bytes > 131072)
    throw new Error('Stored controller recovery manifest identity is invalid');
  validateControllerRecoveryAssociations(manifest.associations, knownRecords, expectedAssociations);
  return {
    associations: manifest.associations,
    recoveredRunIds: manifest.recoveredRunIds,
    newlyAppendedRunIds: manifest.newlyAppendedRunIds,
    manifestIdentity: identity,
    rootedInvocationCount: knownRecords.length,
    appendRecords: []
  };
}

const preTestStages = new Set(['scope-begin', 'generation-module-initialization', 'prior-history-collection', 'controller-gate-recovery', 'source-generation', 'source-parse-checks']);

export function validatePriorAttemptTestEvidence({ record, failure, completion, caseReceipt, caseSummary, testConsole, testFailures, expectedCases }) {
  if (record.status === 'failed' && preTestStages.has(record.stage)) {
    const outcomes = record.caseOutcomes;
    const historicalStageOmission = record.stage === 'source-generation' && failure?.stage === undefined;
    if (failure?.runId !== record.runId || failure.status !== 'failed' || (failure.stage !== record.stage && !historicalStageOmission) || completion || caseReceipt || caseSummary || testConsole || testFailures || outcomes?.cases !== 0 || outcomes?.passed !== 0 || outcomes?.failed !== 0 || outcomes?.unexecutedCases !== expectedCases || (outcomes.caseIds || []).length || (outcomes.failedIds || []).length)
      throw new Error(`Pre-test failure receipts disagree with invocation accounting: ${record.runId}`);
    return { execution: 'not-started', stageEvidence: historicalStageOmission ? 'historical-stage-only' : 'matched' };
  }
  if (!caseReceipt) throw new Error(`Executed prior invocation lacks its case receipt: ${record.runId}`);
  return { execution: 'started', stageEvidence: failure?.stage === undefined ? 'stage-not-recorded' : failure.stage === record.stage ? 'matched' : 'mismatch' };
}

export function reconcileInvocationAccounting(appendInvocation, collectInvocations) {
  let appendResult = null;
  let appendError = null;
  let reconciled = null;
  let reconciliationError = null;
  if (typeof appendInvocation === 'function') {
    try { appendResult = appendInvocation(); }
    catch (error) { appendError = error; }
  }
  if (appendResult && typeof collectInvocations === 'function') {
    try { reconciled = collectInvocations(); }
    catch (error) { reconciliationError = error; }
  }
  const status = appendError || reconciliationError ? 'incomplete' : reconciled ? 'available' : 'unavailable';
  return { appendResult, appendError, reconciled, reconciliationError, status };
}

export function deriveRecoveredInvocationRecord(invocation, evidenceSHA256, provenance) {
  if (!invocation || !/^[0-9a-f-]{36}$/.test(invocation.runId || '') || invocation.root !== `/tmp/herdr-retention-v3-${invocation.runId}` || !['passed', 'failed'].includes(invocation.status) || !Number.isInteger(invocation.expectedCases) || !invocation.caseOutcomes || !Array.isArray(invocation.artifacts) || !/^[0-9a-f]{64}$/.test(evidenceSHA256 || '') || !provenance?.invocationLedger)
    throw new Error('Recovered invocation accounting evidence is incomplete');
  return {
    runId: invocation.runId,
    root: invocation.root,
    status: invocation.status,
    stage: invocation.stage,
    cause: invocation.cause,
    expectedCases: invocation.expectedCases,
    expectedVariants: invocation.expectedVariants ?? null,
    caseOutcomes: invocation.caseOutcomes,
    artifacts: invocation.artifacts,
    evidenceSHA256,
    provenance
  };
}

export function deduplicateInvocationRecords(records, knownRecords) {
  const known = new Map(knownRecords.map(record => [record.runId, record]));
  const seen = new Map();
  const missing = [];
  const duplicates = [];
  for (const record of records) {
    if (!/^[0-9a-f-]{36}$/.test(record.runId || '') || record.root !== `/tmp/herdr-retention-v3-${record.runId}`)
      throw new Error('Recovered invocation identity does not match its namespace');
    const fingerprint = invocationFingerprint(record);
    const previous = seen.get(record.runId);
    if (previous !== undefined) {
      if (previous !== fingerprint) throw new Error(`Conflicting recovered invocation duplicate: ${record.runId}`);
      duplicates.push(record.runId);
      continue;
    }
    seen.set(record.runId, fingerprint);
    const existing = known.get(record.runId);
    if (existing) {
      if (invocationFingerprint(existing) !== fingerprint)
        throw new Error(`Recovered invocation conflicts with retained accounting: ${record.runId}`);
      duplicates.push(record.runId);
      continue;
    }
    missing.push(record);
  }
  return { missing, duplicates };
}

function compactReceiptIdentity(identity) {
  if (!identity) return null;
  if (typeof identity.path !== 'string' || identity.path.length > 180 || !/^[0-9a-f]{64}$/.test(identity.sha256 || '') || !Number.isSafeInteger(identity.bytes) || identity.bytes < 0 || identity.bytes > maxSourceBytes)
    throw new Error('Completion pointer receipt identity is invalid');
  return { path: identity.path, sha256: identity.sha256, bytes: identity.bytes };
}

function compactAppendIdentity(identity) {
  if (!identity) return null;
  if (!/^[0-9a-f]{64}$/.test(identity.sha256 || '') || !Number.isSafeInteger(identity.sequence) || identity.sequence < 1 || !Number.isSafeInteger(identity.bytes) || identity.bytes < 0 || identity.bytes > maxSourceBytes || typeof identity.path !== 'string' || identity.path.length > 180)
    throw new Error('Completion pointer append identity is invalid');
  return { path: identity.path, sequence: identity.sequence, sha256: identity.sha256, bytes: identity.bytes };
}

function compactProjection(projection) {
  if (!projection) return null;
  const fields = [
    'rootedInvocationsBeforeRecovery', 'unrootedFailedCommandEvents', 'unreconciledControllerGates',
    'replacementRootedInvocations', 'replacementTotalCommandAttempts',
    'afterSubsequentControllerRootedInvocations', 'afterSubsequentControllerTotalCommandAttempts',
    'newRootsFromControllerAssociations', 'rootedAfterControllerAssociation', 'nextCompletionExecutionAddsRoot',
    'rootedBeforeControllerAssociations', 'rootedAfterControllerAssociations', 'commandAttemptsAfterControllerAssociations',
    'rootedAfterNextCompletion', 'commandAttemptsAfterNextCompletion', 'separateControllerGateAddsRoot',
    'rootedAfterSeparateControllerGate', 'commandAttemptsAfterSeparateControllerGate'
  ];
  const compact = Object.fromEntries(fields.filter(key => Number.isSafeInteger(projection[key])).map(key => [key, projection[key]]));
  if (projection.notVerifiedTotals === true) compact.notVerifiedTotals = true;
  return Object.keys(compact).length ? compact : null;
}

function validateRecoveredRunIds(runIds) {
  if (!Array.isArray(runIds) || runIds.length > 4 || runIds.some(runId => !/^[0-9a-f-]{36}$/.test(runId)) || new Set(runIds).size !== runIds.length)
    throw new Error('Completion pointer recovered-run identity limit exceeded');
  return runIds;
}

export function projectCompletionAccounting({ commandAttemptAccounting, cumulativeRootedInvocations, ledgerAppend, recoveryManifest, recoveredRunIds = [], allowanceProjection, accountingStatus }) {
  if (!['available', 'incomplete', 'unavailable'].includes(accountingStatus ?? 'available'))
    throw new Error('Completion pointer accounting status is invalid');
  if (!commandAttemptAccounting) {
    if (cumulativeRootedInvocations !== null && cumulativeRootedInvocations !== undefined)
      throw new Error('Completion pointer cannot report totals without reconciled accounting');
    return {
      available: false,
      status: accountingStatus === 'incomplete' ? 'incomplete' : 'unavailable',
      unavailableReason: accountingStatus === 'incomplete' ? 'accounting-incomplete' : 'accounting-unavailable',
      recoveredRunIds: validateRecoveredRunIds(recoveredRunIds),
      ledgerAppend: compactAppendIdentity(ledgerAppend),
      recoveryManifest: compactReceiptIdentity(recoveryManifest)
    };
  }
  if (accountingStatus === 'unavailable') throw new Error('Completion pointer cannot mark reconciled accounting unavailable');
  const events = commandAttemptAccounting.unrootedCommandEvents;
  if (!Array.isArray(events) || events.length > 4)
    throw new Error('Completion pointer unrooted-event limit exceeded');
  validateRecoveredRunIds(recoveredRunIds);
  const unrootedEventIds = events.map(event => {
    if (typeof event?.identity !== 'string' || event.identity.length > 128 || event.status !== 'failed')
      throw new Error('Completion pointer unrooted-event identity is invalid');
    return event.identity;
  });
  const scalarNames = [
    'rootedInvocations', 'reconciledCommandAttempts', 'unrootedFailedAttempts', 'unrootedUnexecutedCaseSlots',
    'reconciledControllerGates', 'unreconciledControllerGates', 'expectedControllerGateCount'
  ];
  const scalars = Object.fromEntries(scalarNames.map(key => {
    const value = commandAttemptAccounting[key];
    if (!Number.isSafeInteger(value) || value < 0) throw new Error(`Completion pointer accounting field is invalid: ${key}`);
    return [key, value];
  }));
  if (cumulativeRootedInvocations !== null && cumulativeRootedInvocations !== undefined && (!Number.isSafeInteger(cumulativeRootedInvocations) || cumulativeRootedInvocations < 0))
    throw new Error('Completion pointer cumulative invocation count is invalid');
  return {
    available: true,
    status: accountingStatus ?? 'available',
    ...scalars,
    cumulativeRootedInvocations: cumulativeRootedInvocations ?? null,
    recoveredRunIds: validateRecoveredRunIds(recoveredRunIds),
    unrootedEventIds,
    ledgerAppend: compactAppendIdentity(ledgerAppend),
    recoveryManifest: compactReceiptIdentity(recoveryManifest),
    allowanceProjection: compactProjection(allowanceProjection)
  };
}

export function formatCompletionPointer({ status, runId, root, stage, sourceTests, summary, terminalReceipt, caseReceipt, caseSummary, invocationLedger, accounting }) {
  const pointer = {
    type: 'private-qualification-completion',
    status,
    runId,
    root,
    stage,
    sourceTests,
    summary,
    terminalReceipt,
    caseReceipt,
    caseSummary,
    invocationLedger,
    accounting
  };
  const output = JSON.stringify(pointer);
  if (Buffer.byteLength(output) > 3000) throw new Error('Compact completion pointer exceeded 3000 bytes');
  return output;
}

export function freshSpace(path = '/tmp') {
  const stat = statfsSync(path);
  const availableBytes = Number(stat.bavail) * Number(stat.bsize);
  if (!Number.isFinite(availableBytes) || availableBytes < minimumHeadroom)
    throw new Error('Fresh source-generation headroom is below the bounded forecast');
  return { path, availableBytes, blockSize: Number(stat.bsize), observedAt: new Date().toISOString() };
}

export function createRunScope() {
  const space = freshSpace('/tmp');
  const id = randomUUID();
  const root = `/tmp/herdr-retention-v3-${id}`;
  mkdirSync(root, { mode: 0o700 });
  const rootStat = lstatSync(root);
  if (!rootStat.isDirectory() || rootStat.isSymbolicLink() || rootStat.uid !== process.getuid?.() || (rootStat.mode & 0o077) !== 0)
    throw new Error('Fresh source-output namespace identity mismatch');
  const ensureOwnedRoot = () => {
    const current = lstatSync(root);
    if (!current.isDirectory() || current.isSymbolicLink() || current.uid !== process.getuid?.() || current.dev !== rootStat.dev || current.ino !== rootStat.ino)
      throw new Error('Source-output namespace changed identity');
  };
  const resolveInside = name => {
    if (typeof name !== 'string' || !name || isAbsolute(name) || name.split(/[\\/]/).some(part => !part || part === '.' || part === '..'))
      throw new Error('Invalid private receipt path');
    const path = resolve(root, name);
    if (!path.startsWith(root + sep)) throw new Error('Receipt escaped its private namespace');
    return path;
  };
  const measure = extraBytes => {
    ensureOwnedRoot();
    let logicalBytes = 0;
    let allocatedBytes = 0;
    let entries = 0;
    const inspect = (directory, depth) => {
      if (depth > maxDepth) throw new Error('Source-output depth limit exceeded');
      for (const name of readdirSync(directory)) {
        if (++entries > maxEntries) throw new Error('Source-output entry limit exceeded');
        const path = `${directory}${sep}${name}`;
        const stat = lstatSync(path);
        if (stat.isSymbolicLink()) throw new Error('Source-output symlink refused');
        if (stat.isDirectory()) inspect(path, depth + 1);
        else if (stat.isFile()) {
          logicalBytes += stat.size;
          allocatedBytes += Number(stat.blocks || Math.ceil(stat.size / 4096)) * 512;
        } else throw new Error('Source-output file type refused');
        if (logicalBytes + extraBytes > maxSourceBytes || allocatedBytes + extraBytes > maxSourceBytes)
          throw new Error('64 MiB source-output envelope exceeded');
      }
    };
    inspect(root, 0);
    const availableBytes = freshSpace(root).availableBytes;
    if (availableBytes < extraBytes) throw new Error('Source-output headroom became uncertain');
    return { logicalBytes, allocatedBytes, entries, availableBytes };
  };
  const parentFor = name => {
    const path = resolveInside(name);
    const parent = path.slice(0, path.lastIndexOf(sep));
    ensureOwnedRoot();
    const parentStat = lstatSync(parent);
    if (!parentStat.isDirectory() || parentStat.isSymbolicLink() || parentStat.uid !== rootStat.uid)
      throw new Error('Receipt parent identity mismatch');
    return path;
  };
  const write = (name, value) => {
    const bytes = Buffer.isBuffer(value) ? value : Buffer.from(String(value));
    const path = parentFor(name);
    measure(bytes.length);
    writeFileSync(path, bytes, { flag: 'wx', mode: 0o600 });
    return measure(0);
  };
  const append = (name, value) => {
    const bytes = Buffer.isBuffer(value) ? value : Buffer.from(String(value));
    const path = parentFor(name);
    const current = lstatSync(path);
    if (!current.isFile() || current.isSymbolicLink() || current.uid !== rootStat.uid)
      throw new Error('Append receipt identity mismatch');
    measure(bytes.length);
    appendFileSync(path, bytes);
    return measure(0);
  };
  const mkdir = name => {
    const path = resolveInside(name);
    const parent = path.slice(0, path.lastIndexOf(sep));
    const parentStat = lstatSync(parent);
    if (!parentStat.isDirectory() || parentStat.isSymbolicLink() || parentStat.uid !== rootStat.uid)
      throw new Error('Source-output directory parent mismatch');
    mkdirSync(path, { mode: 0o700 });
    const created = lstatSync(path);
    if (!created.isDirectory() || created.isSymbolicLink() || created.uid !== rootStat.uid)
      throw new Error('Source-output directory identity mismatch');
  };
  const begin = fields => write('BEGIN.json', JSON.stringify({ schema: 1, runId: id, root, sourceSpace: space, ...fields }, null, 2) + '\n');
  const finalize = fields => {
    const inventory = measure(0);
    const summary = { schema: 1, runId: id, root, inventory, ...fields };
    write('summary.json', JSON.stringify(summary, null, 2) + '\n');
    return summary;
  };
  return { id, root, space, write, append, mkdir, measure, begin, finalize };
}
