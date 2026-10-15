import { readFileSync, lstatSync, mkdirSync, writeFileSync, appendFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { deriveRet2Runner, makeUnifiedPatch, countPatchLines } from './runner-edits.mjs';
import { assessRecoveredInvocationLedger, controllerShellAssociationIdentity, deriveRecoveredInvocationRecord, extractCompletionGateShellResults, invocationRecordFingerprint, maxSourceBytes, reconcileControllerGateAssociations, reuseControllerRecoveryManifest, sha256, summarizeCommandAttempts, validateBoundedEvidenceRead, validateBoundedEvidenceSnapshot, validateControllerRecoveryAssociations, validatePriorAttemptTestEvidence } from './receipts.mjs';

const baselineContract = JSON.parse(readFileSync(join(dirname(fileURLToPath(import.meta.url)), 'baseline.json')));

const sourceDirectory = dirname(fileURLToPath(import.meta.url));
const repositoryRoot = resolve(sourceDirectory, '../../..');

function readBytes(path) {
  return readFileSync(path);
}

function lineCount(value) {
  const text = Buffer.isBuffer(value) ? value.toString('utf8') : String(value);
  return text.length === 0 ? 0 : text.split('\n').length - (text.endsWith('\n') ? 1 : 0);
}

function section(text, start, end) {
  const begin = text.indexOf(start);
  if (begin < 0) throw new Error(`Missing source section: ${start}`);
  const finish = text.indexOf(end, begin);
  if (finish < 0) throw new Error(`Missing source section end: ${end}`);
  return text.slice(begin, finish);
}

function applyUnifiedPatch(original, patch) {
  const source = original === '' ? [] : original.trimEnd().split('\n');
  const diff = patch.trimEnd().split('\n');
  if (!diff[0]?.startsWith('--- ') || !diff[1]?.startsWith('+++ '))
    throw new Error('Expected unified patch headers');
  const result = [];
  let cursor = 0;
  for (let i = 2; i < diff.length;) {
    const match = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/.exec(diff[i++]);
    if (!match) throw new Error('Invalid unified patch hunk');
    const oldStart = Number(match[1]) === 0 ? 0 : Number(match[1]) - 1;
    if (oldStart < cursor) throw new Error('Overlapping unified patch hunks');
    result.push(...source.slice(cursor, oldStart));
    cursor = oldStart;
    let oldCount = 0;
    let newCount = 0;
    while (i < diff.length && !diff[i].startsWith('@@ ')) {
      const line = diff[i++];
      if (![' ', '-', '+'].includes(line[0])) throw new Error('Invalid unified patch line');
      if (line[0] !== '+') {
        if (source[cursor++] !== line.slice(1)) throw new Error('Unified patch context mismatch');
        ++oldCount;
      }
      if (line[0] !== '-') {
        result.push(line.slice(1));
        ++newCount;
      }
    }
    if (oldCount !== Number(match[2] ?? 1) || newCount !== Number(match[4] ?? 1))
      throw new Error('Unified patch line count mismatch');
  }
  result.push(...source.slice(cursor));
  return result.length ? result.join('\n') + '\n' : '';
}

function differenceCounts(before, after) {
  const left = before.trimEnd().split('\n');
  const right = after.trimEnd().split('\n');
  const previous = new Uint32Array(right.length + 1);
  const current = new Uint32Array(right.length + 1);
  for (let i = 1; i <= left.length; ++i) {
    current[0] = 0;
    for (let j = 1; j <= right.length; ++j)
      current[j] = left[i - 1] === right[j - 1] ? previous[j - 1] + 1 : Math.max(previous[j], current[j - 1]);
    previous.set(current);
  }
  const common = previous[right.length];
  return { added: right.length - common, deleted: left.length - common, common };
}

function inputIdentity(relativePath) {
  const bytes = readBytes(join(repositoryRoot, relativePath));
  return { path: relativePath, sha256: sha256(bytes), bytes: bytes.length, lines: lineCount(bytes) };
}

function readAndBindInputs() {
  if (baselineContract.repositoryRoot !== repositoryRoot)
    throw new Error('Private qualification checkout root does not match its source contract');
  const actualSources = baselineContract.installedSources.map(expected => {
    const path = join(repositoryRoot, expected.path);
    const bytes = readBytes(path);
    const digest = sha256(bytes);
    if (digest !== expected.sha256) throw new Error(`Installed source identity mismatch: ${expected.path}`);
    return { path, sha256: digest, bytes: bytes.length };
  });
  const installedPackage = JSON.parse(readBytes(join(repositoryRoot, 'frontend/node_modules/@playwright/test/package.json')));
  if (installedPackage.version !== baselineContract.selectedVersion) throw new Error('Selected @playwright/test version mismatch');
  const frontendPackageBytes = readBytes(join(repositoryRoot, 'frontend/package.json'));
  const frontendPackage = JSON.parse(frontendPackageBytes);
  if (frontendPackage.devDependencies?.[baselineContract.selectedPackage] !== baselineContract.selectedVersion)
    throw new Error('Frontend does not select the installed private runner version');
  actualSources.push({ path: join(repositoryRoot, 'frontend/package.json'), sha256: sha256(frontendPackageBytes), bytes: frontendPackageBytes.length });
  const manifestPath = join(baselineContract.revision05.root, 'revision-05/source-manifest.json');
  const oldManifest = JSON.parse(readBytes(manifestPath));
  if (oldManifest.selected !== '@playwright/test/cli.js' || oldManifest.version !== baselineContract.selectedVersion)
    throw new Error('Revision-05 selected runner mismatch');
  for (const source of oldManifest.identities) {
    const actual = actualSources.find(candidate => candidate.path === source.path);
    if (!actual || actual.sha256 !== source.sha256 || actual.bytes !== source.bytes)
      throw new Error(`Revision-05 source identity mismatch: ${source.path}`);
  }
  const originalRunner = readBytes(actualSources[0].path).toString('utf8');
  const baselinePatchBytes = readBytes(join(baselineContract.revision05.root, baselineContract.revision05.runnerPatch.path));
  if (sha256(baselinePatchBytes) !== baselineContract.revision05.runnerPatch.sha256)
    throw new Error('Immutable revision-05 runner patch identity mismatch');
  const revision05 = applyUnifiedPatch(originalRunner, baselinePatchBytes.toString('utf8'));
  if (sha256(revision05) !== baselineContract.revision05.runnerDerived.sha256)
    throw new Error('Revision-05 reconstruction does not match its immutable derived runner');
  const oldDerivedBytes = readBytes(join(baselineContract.revision05.root, baselineContract.revision05.runnerDerived.path));
  if (!oldDerivedBytes.equals(Buffer.from(revision05))) throw new Error('Revision-05 reconstruction is not byte-exact');
  const support = readBytes(join(sourceDirectory, 'privateCompletion.cjs'));
  const oldSupport = readBytes(join(baselineContract.revision05.root, baselineContract.revision05.support.path));
  if (sha256(support) !== baselineContract.revision05.support.sha256 || !support.equals(oldSupport))
    throw new Error('Repository completion support differs from immutable revision-05');
  const historicalTests = readBytes(join(baselineContract.revision05.root, baselineContract.revision05.tests.path));
  if (sha256(historicalTests) !== baselineContract.revision05.tests.sha256)
    throw new Error('Immutable revision-05 test input identity mismatch');
  const receipt = readBytes(join(baselineContract.revision05.root, baselineContract.revision05.attempt05Receipt.path));
  if (sha256(receipt) !== baselineContract.revision05.attempt05Receipt.sha256)
    throw new Error('Immutable revision-05 53-case receipt identity mismatch');
  const oldSummary = JSON.parse(readBytes(join(baselineContract.revision05.root, baselineContract.revision05.attempt05Summary.path)));
  if (sha256(readBytes(join(baselineContract.revision05.root, baselineContract.revision05.attempt05Summary.path))) !== baselineContract.revision05.attempt05Summary.sha256 || oldSummary.cases !== 53 || oldSummary.passed !== 53 || oldSummary.failed !== 0)
    throw new Error('Immutable revision-05 53-case summary identity mismatch');
  const historicalCases = receipt.toString('utf8').trimEnd().split('\n').map(JSON.parse);
  if (historicalCases.length !== 53 || historicalCases.some((test, index) => test.id !== `C${String(index + 1).padStart(2, '0')}` || !test.passed))
    throw new Error('Revision-05 receipt no longer matches C01-C53');
  const case43 = historicalCases.find(test => test.id === 'C43');
  if (case43?.calls?.retain !== 1) throw new Error('Revision-05 C43 retain=1 observation changed');
  const failureHistory = readBytes(join(baselineContract.revision05.root, baselineContract.revision05.failureHistory.path));
  if (sha256(failureHistory) !== baselineContract.revision05.failureHistory.sha256)
    throw new Error('Historical failure ledger identity mismatch');
  const inputs = baselineContract.repositoryInputs.map(inputIdentity);
  return { actualSources, originalRunner, baselinePatchBytes, revision05, support, historicalTests, historicalCases, inputs };
}

const priorArtifactNames = [
  'BEGIN.json', 'failure.json', 'completion-result.json', 'generation.json', 'controller-recovery.ndjson', 'parse-checks.ndjson',
  'tests.mjs', 'attempt-06.console.txt', 'attempt-06.ndjson', 'attempt-06.summary.json',
  'attempt-06.failures.ndjson', 'invocation-ledger.json', 'summary.json', 'revision-06/source-manifest.json',
  'revision-06/runner.derived.cjs', 'revision-06/runner-c1.patch', 'revision-06/privateCompletion.cjs',
  'revision-06/hosts.derived.js', 'revision-06/consumers.derived.js', 'revision-06/jobDispatchers.derived.js'
];

function statOwnedFile(path) {
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.uid !== process.getuid?.())
    throw new Error(`Prior invocation receipt identity mismatch: ${path}`);
  return stat;
}

function optionalPriorReceipt(root, name, maxBytes = maxSourceBytes) {
  const pieces = name.split('/');
  let parent = root;
  for (const piece of pieces.slice(0, -1)) {
    parent = join(parent, piece);
    let stat;
    try { stat = lstatSync(parent); }
    catch (error) { if (error.code === 'ENOENT') return null; throw error; }
    if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== process.getuid?.())
      throw new Error(`Prior invocation directory identity mismatch: ${parent}`);
  }
  let stat;
  try { stat = lstatSync(join(root, name)); }
  catch (error) { if (error.code === 'ENOENT') return null; throw error; }
  statOwnedFile(join(root, name));
  if (stat.size > maxBytes) throw new Error(`Prior invocation receipt byte limit exceeded: ${root}/${name}`);
  const bytes = readBytes(join(root, name));
  if (bytes.length !== stat.size) throw new Error(`Prior invocation receipt changed while being read: ${root}/${name}`);
  return { name, bytes, identity: { path: name, sha256: sha256(bytes), bytes: bytes.length } };
}

function invocationJournalConfig() {
  const config = baselineContract.revision06InvocationJournal;
  if (!config || !/^[0-9a-f-]{36}$/.test(config.programId) || config.root !== `/tmp/herdr-retention-v3-accounting-${config.programId}` || config.manifest !== 'JOURNAL.json' || config.records !== 'invocations.ndjson' || config.recoveryManifest !== 'controller-recovery.json' || config.maxEntries !== 128 || config.maxBytes !== 4194304)
    throw new Error('Revision-06 invocation journal contract is invalid');
  return config;
}

function ownedDirectory(path, label) {
  const stat = lstatSync(path);
  if (!stat.isDirectory() || stat.isSymbolicLink() || stat.uid !== process.getuid?.() || (stat.mode & 0o077) !== 0)
    throw new Error(`${label} identity mismatch`);
  return stat;
}

function ownedJournalFile(path) {
  const stat = lstatSync(path);
  if (!stat.isFile() || stat.isSymbolicLink() || stat.uid !== process.getuid?.() || (stat.mode & 0o077) !== 0)
    throw new Error('Revision-06 invocation journal file identity mismatch');
  return stat;
}

function readInvocationJournalRows() {
  const config = invocationJournalConfig();
  let rootStat;
  try { rootStat = ownedDirectory(config.root, 'Revision-06 invocation journal directory'); }
  catch (error) { if (error.code === 'ENOENT') return []; throw error; }
  const manifestPath = join(config.root, config.manifest);
  const recordsPath = join(config.root, config.records);
  ownedJournalFile(manifestPath);
  const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
  if (manifest.schema !== 1 || manifest.programId !== config.programId || manifest.root !== config.root || manifest.records !== config.records || manifest.maxEntries !== config.maxEntries || manifest.maxBytes !== config.maxBytes)
    throw new Error('Revision-06 invocation journal manifest mismatch');
  let recordsStat;
  try { recordsStat = ownedJournalFile(recordsPath); }
  catch (error) { if (error.code === 'ENOENT') return []; throw error; }
  if (recordsStat.size > config.maxBytes) throw new Error('Revision-06 invocation journal byte limit exceeded');
  const text = readFileSync(recordsPath, 'utf8');
  if (text && !text.endsWith('\n')) throw new Error('Revision-06 invocation journal has an incomplete final record');
  const rows = text ? text.trimEnd().split('\n').map(JSON.parse) : [];
  if (rows.length > config.maxEntries || lstatSync(config.root).ino !== rootStat.ino || lstatSync(config.root).dev !== rootStat.dev)
    throw new Error('Revision-06 invocation journal changed identity or exceeded its entry limit');
  const ids = new Set();
  for (const row of rows) {
    if (row.schema !== 1 || row.programId !== config.programId || !/^[0-9a-f-]{36}$/.test(row.runId) || row.root !== `/tmp/herdr-retention-v3-${row.runId}` || !['passed', 'failed'].includes(row.status) || !Array.isArray(row.artifacts) || ids.has(row.runId))
      throw new Error('Revision-06 invocation journal record is invalid');
    ids.add(row.runId);
  }
  return rows;
}

function ensureInvocationJournal() {
  const config = invocationJournalConfig();
  let rootStat;
  try { rootStat = ownedDirectory(config.root, 'Revision-06 invocation journal directory'); }
  catch (error) {
    if (error.code !== 'ENOENT') throw error;
    mkdirSync(config.root, { mode: 0o700 });
    rootStat = ownedDirectory(config.root, 'Revision-06 invocation journal directory');
    const manifest = { schema: 1, programId: config.programId, root: config.root, records: config.records, maxEntries: config.maxEntries, maxBytes: config.maxBytes };
    writeFileSync(join(config.root, config.manifest), JSON.stringify(manifest, null, 2) + '\n', { flag: 'wx', mode: 0o600 });
    writeFileSync(join(config.root, config.records), '', { flag: 'wx', mode: 0o600 });
  }
  if (!rootStat) throw new Error('Revision-06 invocation journal was not initialized');
  readInvocationJournalRows();
  return config;
}

export function appendRevision06Invocation(record) {
  if (!record || !/^[0-9a-f-]{36}$/.test(record.runId) || record.root !== `/tmp/herdr-retention-v3-${record.runId}` || !['passed', 'failed'].includes(record.status) || !Number.isInteger(record.expectedCases) || !record.caseOutcomes || !Array.isArray(record.artifacts))
    throw new Error('Revision-06 invocation record is incomplete');
  const config = ensureInvocationJournal();
  const prior = readInvocationJournalRows();
  if (prior.length >= config.maxEntries || prior.some(item => item.runId === record.runId))
    throw new Error('Revision-06 invocation journal refused duplicate or over-limit append');
  if (record.artifacts.some(item => !item || typeof item.path !== 'string' || !/^[0-9a-f]{64}$/.test(item.sha256) || !Number.isInteger(item.bytes) || item.bytes < 0))
    throw new Error('Revision-06 invocation record is incomplete');
  const row = { schema: 1, programId: config.programId, ...record };
  const text = JSON.stringify(row) + '\n';
  const path = join(config.root, config.records);
  const stat = ownedJournalFile(path);
  if (stat.size + Buffer.byteLength(text) > config.maxBytes) throw new Error('Revision-06 invocation journal byte limit exceeded');
  appendFileSync(path, text);
  const rows = readInvocationJournalRows();
  if (rows.length !== prior.length + 1 || rows.at(-1).runId !== record.runId)
    throw new Error('Revision-06 invocation journal append could not be verified');
  return { root: config.root, path: config.records, sequence: rows.length, sha256: sha256(text), bytes: Buffer.byteLength(text) };
}

function controllerGateRecoveryConfig() {
  const config = baselineContract.controllerGateRecovery;
  const amendment = config?.evidenceBoundAmendment;
  if (!config || config.programId !== invocationJournalConfig().programId || config.command !== 'bun frontend/scripts/private-qualification/check.mjs --phase completion' || config.shellCallsitePrefix !== 'shell/callsite%3A842%3A865/' || config.expectedShellResults !== config.expectedControllerAssociations || !config.journalPath.endsWith(`/runs/${config.programId}/journal.json`) || !config.workflowPath.endsWith(`/runs/${config.programId}/workflow.js`) || !config.taskStatePath.endsWith(`/runs/${config.programId}/orchestration/tasks/T001/steps/0004.json`) || config.maxJournalBytes !== 16777216 || config.maxWorkflowBytes !== 262144 || config.maxTaskStateBytes !== 8388608 || config.maxEntries !== 4096 || config.maxShellOutputBytes !== 10485760 || config.expectedControllerAssociations !== 2 || config.expectedShellResults !== 2 || amendment?.recordedBeforeNextCompletionCommand !== true || amendment.previousMaxJournalBytes !== 8388608 || amendment.suppliedObservedJournalBytes !== 8622774 || amendment.suppliedExcessBytesOverPreviousLimit !== 234166 || amendment.failedInvocationId !== 'fa914510-0111-41c8-84fb-a875aff079f9' || amendment.failedStage !== 'controller-gate-recovery' || amendment.observationSource !== '/Users/christophe.vidal/.pi/workflows/projects/herdr-mobile-relay-8c5d7f53c701/sessions/01a0fd10-fe99-772d-b993-10054f3b02b5/runs/1ad22fd0-dd89-45d5-be90-cc685cbe62a9/orchestration/program/recovery-6.json' || !amendment.reason?.includes('implementation-selected') || amendment.newMaxJournalBytes !== config.maxJournalBytes || amendment.nextReadObservation !== null || amendment.recordActualBytesAndSHA256OnSuccessfulRead !== true || amendment.automaticWidening !== false)
    throw new Error('Controller completion-gate recovery contract is invalid');
  return config;
}

function evidenceSnapshot(stat) {
  return {
    isFile: stat.isFile(),
    isSymbolicLink: stat.isSymbolicLink(),
    uid: Number(stat.uid),
    mode: Number(stat.mode),
    size: Number(stat.size),
    dev: String(stat.dev),
    ino: String(stat.ino),
    mtimeNs: String(stat.mtimeNs),
    ctimeNs: String(stat.ctimeNs)
  };
}

function readBoundedEvidence(path, limit, label) {
  const before = evidenceSnapshot(lstatSync(path, { bigint: true }));
  try { validateBoundedEvidenceSnapshot(before, process.getuid?.(), limit); }
  catch (error) { throw new Error(`${label} evidence identity or size limit mismatch: ${error.message}`, { cause: error }); }
  const bytes = readBytes(path);
  const after = evidenceSnapshot(lstatSync(path, { bigint: true }));
  try { validateBoundedEvidenceRead(before, after, bytes.length, process.getuid?.(), limit); }
  catch (error) { throw new Error(`${label} evidence changed while being read: ${error.message}`, { cause: error }); }
  return { bytes, sha256: sha256(bytes), size: bytes.length };
}

function readControllerRecoveryManifest(config, knownInvocations) {
  const journalConfig = invocationJournalConfig();
  const path = join(journalConfig.root, journalConfig.recoveryManifest);
  let stat;
  try { stat = lstatSync(path); }
  catch (error) { if (error.code === 'ENOENT') return null; throw error; }
  if (!stat.isFile() || stat.isSymbolicLink() || stat.uid !== process.getuid?.() || (stat.mode & 0o077) !== 0 || stat.size > 131072)
    throw new Error('Controller recovery manifest identity or size limit mismatch');
  const bytes = readBytes(path);
  if (bytes.length !== stat.size) throw new Error('Controller recovery manifest changed while being read');
  const manifest = JSON.parse(bytes.toString('utf8'));
  if (manifest.schema !== 2 || manifest.programId !== config.programId || manifest.command !== config.command || manifest.journalPath !== config.journalPath || manifest.expectedControllerAssociations !== config.expectedControllerAssociations || manifest.shellCallsitePrefix !== config.shellCallsitePrefix || manifest.expectedShellResults !== config.expectedShellResults || !/^[0-9a-f]{64}$/.test(manifest.journalSHA256 || '') || !Number.isSafeInteger(manifest.journalBytes) || manifest.journalBytes < 0 || manifest.journalBytes > config.maxJournalBytes || !Array.isArray(manifest.associations) || !Array.isArray(manifest.recoveredRunIds) || !Array.isArray(manifest.newlyAppendedRunIds) || manifest.associations.length !== config.expectedControllerAssociations || JSON.stringify(manifest.recoveredRunIds) !== JSON.stringify(manifest.associations.map(item => item.runId)) || JSON.stringify(manifest.newlyAppendedRunIds) !== JSON.stringify(manifest.associations.filter(item => item.classification === 'new-root-appended').map(item => item.runId)))
    throw new Error('Controller recovery manifest does not match its source contract');
  validateControllerRecoveryAssociations(manifest.associations, knownInvocations, config.expectedControllerAssociations);
  return { ...manifest, manifestPath: path, manifestIdentity: { path: journalConfig.recoveryManifest, sha256: sha256(bytes), bytes: bytes.length } };
}

function validateRecoveredRun(shellResult, provenance, bound) {
  const summary = shellResult.summary;
  if (summary.root !== `/tmp/herdr-retention-v3-${summary.runId}` || !['passed', 'failed'].includes(summary.status) || (summary.status === 'passed' && shellResult.exitCode !== 0))
    throw new Error(`Controller shell result has an invalid top-level invocation identity: ${shellResult.path}`);
  if (summary.status === 'failed' && shellResult.exitCode === 0)
    throw new Error(`Failed controller invocation has a successful shell status: ${summary.runId}`);
  ownedDirectory(summary.root, `Controller invocation namespace ${summary.runId}`);
  const storedSummaryArtifact = optionalPriorReceipt(summary.root, 'summary.json');
  const storedSummary = storedSummaryArtifact ? JSON.parse(storedSummaryArtifact.bytes.toString('utf8')) : null;
  if (storedSummary?.runId !== summary.runId || storedSummary.root !== summary.root || storedSummary.status !== summary.status)
    throw new Error(`Controller shell identity does not match its retained summary receipt: ${summary.runId}`);
  const generationSummary = summary.generated ?? storedSummary.generated;
  const failureArtifact = optionalPriorReceipt(summary.root, 'failure.json');
  const completionArtifact = optionalPriorReceipt(summary.root, 'completion-result.json');
  const ledgerArtifact = optionalPriorReceipt(summary.root, 'invocation-ledger.json');
  const failure = failureArtifact ? JSON.parse(failureArtifact.bytes.toString('utf8')) : null;
  const completion = completionArtifact ? JSON.parse(completionArtifact.bytes.toString('utf8')) : null;
  const ledger = ledgerArtifact ? JSON.parse(ledgerArtifact.bytes.toString('utf8')) : null;
  const expectedCases = generationSummary?.cases?.executed ?? completion?.sourceTests?.cases ?? ledger?.current?.expectedCases ?? summary.sourceTests?.cases ?? storedSummary.sourceTests?.cases;
  if (!Number.isInteger(expectedCases)) throw new Error(`Controller invocation does not retain an authoritative case-count identity: ${summary.runId}`);
  const expectedVariants = generationSummary?.cases?.variants ?? completion?.sourceTests?.variants ?? ledger?.current?.expectedVariants ?? summary.sourceTests?.variants ?? storedSummary.sourceTests?.variants ?? null;
  const stage = summary.status === 'passed' ? 'completion' : failure?.stage ?? (generationSummary ? 'case-ledger-validation' : 'source-generation');
  const inspected = inspectPriorInvocation({
    runId: summary.runId,
    root: summary.root,
    status: summary.status,
    stage,
    expectedCases,
    expectedVariants,
    cause: summary.error?.message ?? storedSummary.error?.message ?? null
  }, false);
  const artifacts = inspected.invocation.artifacts;
  const byName = new Map(artifacts.map(item => [item.path, item]));
  for (const key of ['summary', 'terminalReceipt', 'caseReceipt', 'caseSummary', 'invocationLedger']) {
    const identity = summary[key];
    if (identity && (!byName.has(identity.path) || byName.get(identity.path).sha256 !== identity.sha256 || byName.get(identity.path).bytes !== identity.bytes))
      throw new Error(`Controller stdout pointer identity mismatch: ${summary.runId}/${identity.path}`);
  }
  const ledgerAssessment = assessRecoveredInvocationLedger(ledger, {
    command: provenance.command,
    runId: summary.runId,
    root: summary.root,
    status: summary.status,
    stage,
    expectedCases: inspected.invocation.expectedCases,
    expectedVariants: inspected.invocation.expectedVariants,
    caseOutcomes: inspected.invocation.caseOutcomes,
    artifacts
  });
  const readArtifact = name => {
    const identity = byName.get(name);
    if (!identity) return null;
    return JSON.parse(readBytes(join(summary.root, name)).toString('utf8'));
  };
  const begin = readArtifact('BEGIN.json');
  if (begin?.phase !== 'completion' || begin.nativeExecution !== false || begin.root !== summary.root || storedSummary?.root !== summary.root || storedSummary.runId !== summary.runId || storedSummary.status !== summary.status)
    throw new Error(`Controller shell identity does not match its retained run receipts: ${summary.runId}`);
  if (summary.sourceTests && storedSummary.sourceTests && JSON.stringify(summary.sourceTests) !== JSON.stringify(storedSummary.sourceTests))
    throw new Error(`Controller stdout test totals disagree with its summary receipt: ${summary.runId}`);
  if (summary.generated && JSON.stringify(summary.generated) !== JSON.stringify(storedSummary.generated))
    throw new Error(`Controller stdout generation disagrees with its summary receipt: ${summary.runId}`);
  const receipt = byName.get('attempt-06.ndjson');
  if (receipt) {
    const rows = readBytes(join(summary.root, 'attempt-06.ndjson')).toString('utf8').trimEnd().split('\n').filter(Boolean).map(JSON.parse);
    if (rows.some(row => row.calls?.native !== 0)) throw new Error(`Controller invocation executed a native adapter: ${summary.runId}`);
    if (summary.status === 'passed' && rows.some(row => !row.passed)) throw new Error(`Passing controller invocation contains a failed case: ${summary.runId}`);
  }
  if (generationSummary) {
    const generation = readArtifact('generation.json');
    const sourceManifest = readArtifact('revision-06/source-manifest.json');
    const runner = byName.get('revision-06/runner.derived.cjs');
    const patch = byName.get('revision-06/runner-c1.patch');
    if (!generation || !sourceManifest || !runner || !patch || !Array.isArray(generation.generated) || !Array.isArray(generationSummary.generated) || JSON.stringify(generation.generated) !== JSON.stringify(generationSummary.generated))
      throw new Error(`Controller stdout generation identity disagrees with its retained source receipts: ${summary.runId}`);
    for (const identity of generation.generated) {
      if (typeof identity.path !== 'string' || identity.path.startsWith('/') || identity.path.split('/').includes('..'))
        throw new Error(`Controller generation receipt path is invalid: ${summary.runId}`);
      const actual = byName.get(identity.path);
      if (!actual || actual.sha256 !== identity.sha256 || actual.bytes !== identity.bytes)
        throw new Error(`Controller generated source identity mismatch: ${summary.runId}/${identity.path}`);
    }
    const runnerBytes = readBytes(join(summary.root, 'revision-06/runner.derived.cjs'));
    const patchBytes = readBytes(join(summary.root, 'revision-06/runner-c1.patch'));
    const supportBytes = readBytes(join(summary.root, 'revision-06/privateCompletion.cjs'));
    const expectedCaseIds = Array.from({ length: expectedCases }, (_, index) => `C${String(index + 1).padStart(2, '0')}`);
    const sourceLedger = sourceManifest.caseLedger;
    const oldInputs = sourceManifest.repositoryInputs;
    if (sourceManifest.selected !== '@playwright/test/cli.js' || sourceManifest.version !== baselineContract.selectedVersion || JSON.stringify(sourceManifest.identities) !== JSON.stringify(bound.actualSources) || sourceManifest.revision05?.runnerPatchSHA256 !== sha256(bound.baselinePatchBytes) || sourceManifest.revision05?.runnerSHA256 !== baselineContract.revision05.runnerDerived.sha256 || sourceManifest.revision05?.supportSHA256 !== baselineContract.revision05.support.sha256 || sourceManifest.revision05?.testsSHA256 !== baselineContract.revision05.tests.sha256 || sourceManifest.revision05?.attempt05ReceiptSHA256 !== baselineContract.revision05.attempt05Receipt.sha256 || sourceManifest.revision05?.c43ObservedRetainCalls !== 1 || sourceManifest.derived?.sha256 !== sha256(runnerBytes) || sourceManifest.derived?.bytes !== runnerBytes.length || sourceManifest.support?.sha256 !== sha256(supportBytes) || sourceManifest.support?.bytes !== supportBytes.length || sourceManifest.c1?.exactIncrementPatchSHA256 !== sha256(patchBytes) || sourceManifest.c1?.incrementalVendorAdded !== generation.vendorLines?.c1Added || sourceManifest.c1?.incrementalVendorDeleted !== generation.vendorLines?.c1Deleted || sourceManifest.c1?.fullVendorAdded !== generation.vendorLines?.originalToCandidateAdded || sourceManifest.c1?.fullVendorDeleted !== generation.vendorLines?.originalToCandidateDeleted || JSON.stringify(sourceLedger?.historicalIds) !== JSON.stringify(Array.from({ length: 53 }, (_, index) => `C${String(index + 1).padStart(2, '0')}`)) || JSON.stringify(oldInputs?.map(item => [item.path, item.sha256, item.bytes, item.lines])) !== JSON.stringify(generation.sourceCounts?.map(item => [item.path, item.sha256, item.bytes, item.lines])) || sourceLedger?.executedIds?.length !== expectedCases || JSON.stringify(sourceLedger.executedIds) !== JSON.stringify(expectedCaseIds) || sourceLedger.newCaseCount !== expectedCases - 53 || generation.cases?.executed !== expectedCases || generation.cases?.variants !== expectedCases + 1 || JSON.stringify(generation.cases.IDs) !== JSON.stringify(expectedCaseIds) || applyUnifiedPatch(bound.revision05, patchBytes.toString('utf8')) !== runnerBytes.toString('utf8'))
      throw new Error(`Controller candidate does not reconstruct from the immutable revision-05 source: ${summary.runId}`);
  }
  if (summary.status === 'passed') {
    if (completion?.runId !== summary.runId || completion.status !== 'passed' || completion.command !== provenance.command || completion.sourceTests?.cases !== inspected.invocation.caseOutcomes.cases || completion.sourceTests?.passed !== inspected.invocation.caseOutcomes.passed || completion.sourceTests?.failed !== inspected.invocation.caseOutcomes.failed)
      throw new Error(`Controller completion receipt disagrees with its shell result: ${summary.runId}`);
    for (const identity of completion.freshReceipts || []) {
      const actual = byName.get(identity.path);
      if (!actual || actual.sha256 !== identity.sha256 || actual.bytes !== identity.bytes)
        throw new Error(`Controller completion receipt identity mismatch: ${summary.runId}/${identity.path}`);
    }
  }
  return deriveRecoveredInvocationRecord(inspected.invocation, shellResult.stdoutSHA256, { ...provenance, invocationLedger: ledgerAssessment });
}

function validatePreReplacementSnapshot(knownHistory) {
  const before = baselineContract.causalAllowanceAmendment?.revision06BeforeReplacement;
  const historicalIds = [...baselineContract.revision06PriorInvocations.map(item => item.runId), before?.includesFailedRun, before?.preservedPriorSuccess];
  if (!before || historicalIds.some(id => typeof id !== 'string') || new Set(historicalIds).size !== before.invocations)
    throw new Error('Historical pre-replacement snapshot contract is incomplete');
  const known = new Map(knownHistory.invocations.map(item => [item.runId, item]));
  const historical = historicalIds.map(id => known.get(id));
  if (historical.some(item => !item) || historical.filter(item => item.status === 'passed').length !== before.passed || historical.filter(item => item.status === 'failed').length !== before.failed || historical.reduce((sum, item) => sum + item.caseOutcomes.cases, 0) !== before.recordedCases || historical.reduce((sum, item) => sum + item.unexecutedCases, 0) !== before.unexecutedCases || known.get(before.includesFailedRun)?.status !== 'failed' || known.get(before.preservedPriorSuccess)?.status !== 'passed')
    throw new Error('Historical pre-replacement subset does not match its retained snapshot');
  return historical;
}

function controllerRecoveryDiagnostic(scope, associations) {
  if (!scope || typeof scope.write !== 'function') throw new Error('Controller recovery requires the fresh invocation scope');
  const rows = associations.map(association => ({
    occurrence: association.occurrence,
    eventIdentity: association.eventIdentity,
    runId: association.runId,
    root: association.root,
    status: association.status,
    stdoutSHA256: association.stdoutSHA256,
    stdoutBytes: association.stdoutBytes,
    classification: association.classification,
    currentClassification: association.currentClassification ?? null,
    invocationFingerprint: association.invocationFingerprint
  }));
  scope.write('controller-recovery.ndjson', rows.map(row => JSON.stringify(row)).join('\n') + '\n');
  return rows;
}

function controllerManifestAssociation(association) {
  return {
    occurrence: association.occurrence,
    path: association.path,
    eventIdentity: association.eventIdentity,
    runId: association.runId,
    root: association.root,
    status: association.status,
    exitCode: association.exitCode,
    stdoutSHA256: association.stdoutSHA256,
    stdoutBytes: association.stdoutBytes,
    invocationFingerprint: association.invocationFingerprint,
    classification: association.classification === 'new-root' ? 'new-root-appended' : association.classification
  };
}

export function recoverControllerGateInvocations(knownHistory, scope) {
  const config = controllerGateRecoveryConfig();
  validatePreReplacementSnapshot(knownHistory);
  const reusedManifest = reuseControllerRecoveryManifest(knownHistory.controllerRecoveryManifest, knownHistory.invocations, config.expectedControllerAssociations);
  if (reusedManifest) {
    const diagnostic = reusedManifest.associations.map(association => ({
      ...association,
      currentClassification: association.classification === 'new-root-appended' ? 'known-invocation-journal' : association.classification
    }));
    controllerRecoveryDiagnostic(scope, diagnostic);
    return {
      ...knownHistory.controllerRecoveryManifest,
      associations: reusedManifest.associations,
      alreadyRecovered: true,
      appendedRunIdsThisInvocation: [],
      rootedInvocationCount: reusedManifest.rootedInvocationCount
    };
  }
  const journal = readBoundedEvidence(config.journalPath, config.maxJournalBytes, 'Workflow journal');
  const workflow = readBoundedEvidence(config.workflowPath, config.maxWorkflowBytes, 'Workflow source');
  const taskState = readBoundedEvidence(config.taskStatePath, config.maxTaskStateBytes, 'T001 step state');
  const workflowText = workflow.bytes.toString('utf8');
  if (!workflowText.includes('async function runVerificationGates(state)') || !workflowText.includes('for (let index = 0; index < state.config.verificationCommands.length; index += 1)') || !workflowText.includes('const command = state.config.verificationCommands[index];') || !workflowText.includes('result = await shell(command, options)') || !workflowText.includes('(result.stdout + "\\n" + result.stderr).slice(-6000)'))
    throw new Error('Retained workflow source does not match the recorded controller-gate path');
  const task = JSON.parse(taskState.bytes.toString('utf8'));
  const controllerVerification = task.controllerVerification;
  if (!Array.isArray(controllerVerification) || controllerVerification.length !== 1 || !controllerVerification.some(item => item.name === config.command && item.status === 'passed' && typeof item.evidence === 'string' && item.evidence.startsWith('Exit code: 0\n')))
    throw new Error('T001 step does not retain the configured completion-gate result');
  const workflowJournal = JSON.parse(journal.bytes.toString('utf8'));
  const recordedRunId = workflowJournal.originRunId ?? workflowJournal.programId ?? workflowJournal.runId;
  if (recordedRunId !== undefined && recordedRunId !== config.programId)
    throw new Error('Workflow journal program identity mismatch');
  const shellResults = extractCompletionGateShellResults(workflowJournal, config.command, config.shellCallsitePrefix, config.expectedShellResults, config.maxEntries, config.maxShellOutputBytes);
  const controllerEvidenceMatches = shellResults.some(result => controllerVerification[0].evidence === `Exit code: ${result.exitCode}\n${(result.stdout + "\n" + result.stderr).slice(-6000)}`);
  if (!controllerEvidenceMatches) throw new Error('T001 verification evidence does not match a retained controller shell result');
  const provenanceBase = {
    kind: 'controller-shell-recovery',
    sourcePath: config.journalPath,
    journalSHA256: journal.sha256,
    workflowPath: config.workflowPath,
    workflowSHA256: workflow.sha256,
    taskStatePath: config.taskStatePath,
    taskStateSHA256: taskState.sha256,
    command: config.command,
    shellCallsitePrefix: config.shellCallsitePrefix
  };
  const bound = readAndBindInputs();
  const candidates = shellResults.map(shellResult => {
    const event = {
      occurrence: shellResult.occurrence,
      runId: shellResult.summary.runId,
      root: shellResult.summary.root,
      status: shellResult.summary.status,
      exitCode: shellResult.exitCode,
      stdoutSHA256: shellResult.stdoutSHA256,
      stdoutBytes: shellResult.stdoutBytes
    };
    const associationId = controllerShellAssociationIdentity(event);
    const invocation = validateRecoveredRun(shellResult, {
      ...provenanceBase,
      associationId,
      completedPath: shellResult.path,
      shellExitCode: shellResult.exitCode,
      stdoutBytes: shellResult.stdoutBytes,
      stdoutSHA256: shellResult.stdoutSHA256,
      stderrSHA256: shellResult.stderrSHA256,
      stderrBytes: shellResult.stderrBytes
    }, bound);
    return { ...event, path: shellResult.path, invocation };
  });
  const reconciled = reconcileControllerGateAssociations(candidates, knownHistory.invocations, config.expectedControllerAssociations);
  const manifestAssociations = reconciled.associations.map(controllerManifestAssociation);
  controllerRecoveryDiagnostic(scope, manifestAssociations);
  for (const record of reconciled.newRecords) appendRevision06Invocation(record);
  const manifest = {
    schema: 2,
    programId: config.programId,
    command: config.command,
    shellCallsitePrefix: config.shellCallsitePrefix,
    expectedShellResults: config.expectedShellResults,
    expectedControllerAssociations: config.expectedControllerAssociations,
    journalPath: config.journalPath,
    journalSHA256: journal.sha256,
    journalBytes: journal.size,
    workflowPath: config.workflowPath,
    workflowSHA256: workflow.sha256,
    taskStatePath: config.taskStatePath,
    taskStateSHA256: taskState.sha256,
    associations: manifestAssociations,
    recoveredRunIds: manifestAssociations.map(item => item.runId),
    newlyAppendedRunIds: manifestAssociations.filter(item => item.classification === 'new-root-appended').map(item => item.runId)
  };
  const journalConfig = invocationJournalConfig();
  const manifestPath = join(journalConfig.root, journalConfig.recoveryManifest);
  writeFileSync(manifestPath, JSON.stringify(manifest, null, 2) + '\n', { flag: 'wx', mode: 0o600 });
  const bytes = readBytes(manifestPath);
  return {
    ...manifest,
    manifestPath,
    manifestIdentity: { path: journalConfig.recoveryManifest, sha256: sha256(bytes), bytes: bytes.length },
    alreadyRecovered: false,
    appendedRunIdsThisInvocation: manifest.newlyAppendedRunIds,
    rootedInvocationCount: knownHistory.totals.invocations + reconciled.newRecords.length
  };
}

function inspectPriorInvocation(record, fromJournal) {
  if (!/^[0-9a-f-]{36}$/.test(record.runId) || record.root !== `/tmp/herdr-retention-v3-${record.runId}`)
    throw new Error('Prior invocation namespace does not match its run ID');
  const rootStat = ownedDirectory(record.root, `Prior invocation namespace ${record.runId}`);
  if (priorArtifactNames.length > 32) throw new Error('Prior invocation artifact-entry limit exceeded');
  const artifacts = [];
  let totalBytes = 0;
  for (const name of priorArtifactNames) {
    const artifact = optionalPriorReceipt(record.root, name, maxSourceBytes - totalBytes);
    if (!artifact) continue;
    totalBytes += artifact.identity.bytes;
    artifacts.push(artifact);
  }
  const byName = new Map(artifacts.map(artifact => [artifact.name, artifact]));
  const json = name => byName.has(name) ? JSON.parse(byName.get(name).bytes.toString('utf8')) : null;
  const begin = json('BEGIN.json');
  const summary = json('summary.json');
  const failure = json('failure.json');
  const completion = json('completion-result.json');
  const caseSummary = json('attempt-06.summary.json');
  const caseReceipt = byName.get('attempt-06.ndjson');
  if (begin?.runId !== record.runId || (begin.root && begin.root !== record.root) || (summary && (summary.runId !== record.runId || (summary.root && summary.root !== record.root) || summary.status !== record.status)) || (!fromJournal && !summary))
    throw new Error(`Prior invocation status receipt mismatch: ${record.runId}`);
  if (record.status === 'passed' && (failure || completion?.runId !== record.runId || completion.status !== 'passed' || !caseSummary))
    throw new Error(`Prior passing invocation lacks its completion receipts: ${record.runId}`);
  if (record.status === 'failed' && (failure?.runId !== record.runId || failure.status !== 'failed' || completion))
    throw new Error(`Prior failed invocation lacks its failure receipt: ${record.runId}`);
  if (fromJournal) {
    for (const identity of record.artifacts) {
      const actual = byName.get(identity.path)?.identity;
      if (!actual || actual.sha256 !== identity.sha256 || actual.bytes !== identity.bytes)
        throw new Error(`Invocation journal receipt identity mismatch: ${record.runId}/${identity.path}`);
    }
  }
  const rows = caseReceipt ? caseReceipt.bytes.toString('utf8').trimEnd().split('\n').filter(Boolean).map(JSON.parse) : [];
  const expectedCases = record.expectedCases ?? baselineContract.revision06PriorCaseCount;
  if (!Number.isInteger(expectedCases) || ![67, 69, 70, 71, 74, 75, 78, 79].includes(expectedCases) || rows.length > expectedCases || rows.some((row, index) => row.id !== `C${String(index + 1).padStart(2, '0')}`))
    throw new Error(`Prior case receipt identity mismatch: ${record.runId}`);
  if (caseSummary && (caseSummary.cases !== rows.length || caseSummary.passed !== rows.filter(row => row.passed).length || caseSummary.failed !== rows.filter(row => !row.passed).length))
    throw new Error(`Prior case receipt totals do not match its summary: ${record.runId}`);
  const variantIds = rows.flatMap(row => Array.isArray(row.variants) ? row.variants : []);
  const outcomes = {
    cases: rows.length,
    passed: rows.filter(row => row.passed).length,
    failed: rows.filter(row => !row.passed).length,
    caseIds: rows.map(row => row.id),
    failedIds: rows.filter(row => !row.passed).map(row => row.id),
    unexecutedCases: expectedCases - rows.length,
    ...(variantIds.length ? { variantIds, variantCount: variantIds.length } : {})
  };
  if (caseSummary && caseSummary.variants !== undefined && caseSummary.variants !== variantIds.length)
    throw new Error(`Prior case variant totals do not match its summary: ${record.runId}`);
  if (fromJournal && JSON.stringify(record.caseOutcomes) !== JSON.stringify(outcomes))
    throw new Error(`Invocation journal case totals mismatch: ${record.runId}`);
  const testEvidence = validatePriorAttemptTestEvidence({
    record: { ...record, caseOutcomes: outcomes },
    failure,
    completion,
    caseReceipt: Boolean(caseReceipt),
    caseSummary: Boolean(caseSummary),
    testConsole: byName.has('attempt-06.console.txt'),
    testFailures: byName.has('attempt-06.failures.ndjson'),
    expectedCases
  });
  return {
    invocation: {
      runId: record.runId,
      root: record.root,
      status: record.status,
      stage: record.stage,
      cause: failure?.message || record.cause || null,
      expectedCases,
      expectedVariants: completion?.sourceTests?.variants ?? summary?.generated?.cases?.variants ?? record.expectedVariants ?? caseSummary?.variants ?? null,
      unexecutedCases: expectedCases - rows.length,
      caseOutcomes: outcomes,
      testExecution: testEvidence.execution,
      stageEvidence: testEvidence.stageEvidence,
      retainedInInvocationJournal: fromJournal,
      artifacts: artifacts.map(artifact => artifact.identity),
      provenance: record.provenance ?? null
    },
    totalBytes
  };
}

export function collectPriorRevision06Invocations() {
  const expected = baselineContract.revision06PriorInvocations;
  if (!Array.isArray(expected) || expected.length !== 15 || new Set(expected.map(item => item.runId)).size !== expected.length || expected.filter(item => item.status === 'passed').length !== 7 || expected.filter(item => item.status === 'failed').length !== 8)
    throw new Error('Revision-06 prior invocation ledger is incomplete');
  const journalRows = readInvocationJournalRows();
  const fixedIds = new Set(expected.map(item => item.runId));
  if (journalRows.some(row => fixedIds.has(row.runId))) throw new Error('Revision-06 journal duplicates a fixed historical invocation');
  let totalBytes = 0;
  const invocations = [...expected.map(record => ({ record, fromJournal: false })), ...journalRows.map(record => ({ record, fromJournal: true }))].map(({ record, fromJournal }) => {
    const inspected = inspectPriorInvocation(record, fromJournal);
    totalBytes += inspected.totalBytes;
    if (totalBytes > maxSourceBytes) throw new Error('Prior invocation receipt read envelope exceeded');
    return inspected.invocation;
  });
  const amendment = baselineContract.causalAllowanceAmendment;
  const unrootedEvents = [amendment?.subsequentCausalAmendment?.unrootedCommandEvent, amendment?.latestCausalAmendment?.additionalUnrootedCommandEvent].filter(Boolean);
  const recoveryManifest = readControllerRecoveryManifest(controllerGateRecoveryConfig(), invocations);
  const controllerAssociations = recoveryManifest?.associations ?? [];
  const commandAttempts = summarizeCommandAttempts(invocations, unrootedEvents, baselineContract.controllerGateRecovery.expectedControllerAssociations, controllerAssociations);
  return {
    invocations,
    journal: { path: baselineContract.revision06InvocationJournal.records, entries: journalRows.length, maxEntries: baselineContract.revision06InvocationJournal.maxEntries },
    controllerAssociations,
    controllerRecoveryManifest: recoveryManifest,
    commandAttemptAccounting: commandAttempts,
    totals: {
      invocations: invocations.length,
      passed: invocations.filter(item => item.status === 'passed').length,
      failed: invocations.filter(item => item.status === 'failed').length,
      recordedCases: invocations.reduce((sum, item) => sum + (item.caseOutcomes?.cases || 0), 0),
      passedCases: invocations.reduce((sum, item) => sum + (item.caseOutcomes?.passed || 0), 0),
      failedCases: invocations.reduce((sum, item) => sum + (item.caseOutcomes?.failed || 0), 0),
      unexecutedCases: invocations.reduce((sum, item) => sum + item.unexecutedCases, 0),
      uniqueCaseIds: [...new Set(invocations.flatMap(item => item.caseOutcomes.caseIds))].sort(),
      receiptBytesRead: totalBytes
    }
  };
}

export async function generateRevision(scope, priorHistory = collectPriorRevision06Invocations(), controllerRecovery = null) {
  const allowance = baselineContract.causalAllowanceAmendment;
  const historicalCounters = allowance?.historicalStep9Counters;
  const currentCounters = allowance?.recovery5CausalAmendment?.currentControllerConsumption;
  const recovery5Amendment = allowance?.recovery5CausalAmendment;
  const recovery6Amendment = allowance?.recovery6CausalAmendment;
  const review3Amendment = allowance?.review3CausalAmendment;
  const continuationAmendment = allowance?.continuationCausalAmendment;
  const review4Amendment = allowance?.review4CausalAmendment;
  const previousProjection = allowance?.projection;
  const amendment = allowance?.subsequentCausalAmendment;
  const event = amendment?.unrootedCommandEvent;
  const latestAmendment = allowance?.latestCausalAmendment;
  const additionalEvent = latestAmendment?.additionalUnrootedCommandEvent;
  const correctedProjection = amendment?.projection;
  const latestProjection = latestAmendment?.projection;
  const validHistoricalCounters = historicalCounters?.taskStep === 9 && historicalCounters.workerTurns === 5 && historicalCounters.reviews === 6 && historicalCounters.reviewLimit === 12 && historicalCounters.repairRounds === 2 && historicalCounters.verificationFailures === 2 && historicalCounters.interventions === 1 && historicalCounters.audits === 0 && historicalCounters.auditLimit === 2 && historicalCounters.engineUsageEvidence === 'unknown-not-inferred' && historicalCounters.countersReset === false && historicalCounters.capsRaised === false && historicalCounters.sourceClassification?.includes('Historical') === true;
  const validCurrentCounters = currentCounters?.taskStep === 13 && currentCounters.workerTurns === 7 && currentCounters.reviews === 6 && currentCounters.reviewLimit === 12 && currentCounters.repairRounds === 2 && currentCounters.verificationFailures === 4 && currentCounters.interventions === 1 && currentCounters.audits === 0 && currentCounters.auditLimit === 2 && currentCounters.engineUsageEvidence === 'unknown-not-inferred' && currentCounters.countersReset === false && currentCounters.capsRaised === false && currentCounters.sourceClassification?.includes('current T001 consumption') === true;
  const validRecovery5Projection = recovery5Amendment?.recordedBeforeNextCompletionCommand === true && recovery5Amendment.expectedControllerAssociations === 2 && recovery5Amendment.retainedRevision06Snapshot?.rootedInvocations === 18 && recovery5Amendment.retainedRevision06Snapshot?.passed === 8 && recovery5Amendment.retainedRevision06Snapshot?.failed === 10 && recovery5Amendment.retainedRevision06Snapshot?.recordedCases === 936 && recovery5Amendment.retainedRevision06Snapshot?.unexecutedCases === 300 && recovery5Amendment.retainedRevision06Snapshot?.unrootedFailedCommandEvents === 2 && recovery5Amendment.retainedRevision06Snapshot?.unrootedUnexecutedCaseSlots === 150 && recovery5Amendment.conditionalProjection?.newRootsFromControllerAssociations === 1 && recovery5Amendment.conditionalProjection?.rootedAfterControllerAssociation === 19 && recovery5Amendment.conditionalProjection?.rootedAfterNextCompletion === 20 && recovery5Amendment.conditionalProjection?.commandAttemptsAfterNextCompletion === 22 && recovery5Amendment.conditionalProjection?.rootedAfterSeparateControllerGate === 21 && recovery5Amendment.conditionalProjection?.commandAttemptsAfterSeparateControllerGate === 23 && recovery5Amendment.conditionalProjection?.notVerifiedTotals === true && recovery5Amendment.additionalVariants === 0;
  const validRecovery6Consumption = recovery6Amendment?.recordedBeforeNextCompletionCommand === true && recovery6Amendment.currentControllerConsumption?.taskStep === 15 && recovery6Amendment.currentControllerConsumption?.workerTurns === 8 && recovery6Amendment.currentControllerConsumption?.reviews === 6 && recovery6Amendment.currentControllerConsumption?.reviewLimit === 12 && recovery6Amendment.currentControllerConsumption?.repairRounds === 2 && recovery6Amendment.currentControllerConsumption?.verificationFailures === 5 && recovery6Amendment.currentControllerConsumption?.interventions === 1 && recovery6Amendment.currentControllerConsumption?.audits === 0 && recovery6Amendment.currentControllerConsumption?.engineUsageEvidence === 'unknown-not-inferred' && recovery6Amendment.currentControllerConsumption?.countersReset === false && recovery6Amendment.currentControllerConsumption?.capsRaised === false && recovery6Amendment.currentControllerConsumption?.sourcePath === recovery6Amendment.sourcePath && recovery6Amendment.currentControllerConsumption?.sourceClassification?.includes('no engine usage is inferred') === true;
  const validRecovery6Projection = recovery6Amendment?.expectedControllerAssociations === 2 && recovery6Amendment.additionalCaseIds?.length === 0 && recovery6Amendment.additionalVariants === 0 && recovery6Amendment?.retainedRevision06Snapshot?.rootedInvocations === 19 && recovery6Amendment.retainedRevision06Snapshot?.passed === 8 && recovery6Amendment.retainedRevision06Snapshot?.failed === 11 && recovery6Amendment.retainedRevision06Snapshot?.recordedCases === 936 && recovery6Amendment.retainedRevision06Snapshot?.unexecutedCases === 375 && recovery6Amendment.retainedRevision06Snapshot?.unrootedFailedCommandEvents === 2 && recovery6Amendment.retainedRevision06Snapshot?.unrootedUnexecutedCaseSlots === 150 && recovery6Amendment.conditionalProjection?.rootedBeforeControllerAssociations === 19 && recovery6Amendment.conditionalProjection?.newRootsFromControllerAssociations === 1 && recovery6Amendment.conditionalProjection?.rootedAfterControllerAssociations === 20 && recovery6Amendment.conditionalProjection?.commandAttemptsAfterControllerAssociations === 22 && recovery6Amendment.conditionalProjection?.rootedAfterNextCompletion === 21 && recovery6Amendment.conditionalProjection?.commandAttemptsAfterNextCompletion === 23 && recovery6Amendment.conditionalProjection?.rootedAfterSeparateControllerGate === 22 && recovery6Amendment.conditionalProjection?.commandAttemptsAfterSeparateControllerGate === 24 && recovery6Amendment.conditionalProjection?.notVerifiedTotals === true && recovery6Amendment.additionalVariants === 0;
  const validReview3Amendment = review3Amendment?.recordedBeforeRepairExecution === true && review3Amendment.sourceClassification?.includes('step 18') === true && review3Amendment.reports?.length === 3 && review3Amendment.reviewAccounting?.workerTurn === 9 && review3Amendment.reviewAccounting?.reviewsConsumed === 9 && review3Amendment.reviewAccounting?.reviewLimit === 12 && review3Amendment.reviewAccounting?.auditsConsumed === 0 && review3Amendment.reviewAccounting?.blockerCount === 0 && review3Amendment.accountingAssessments?.length === 3 && review3Amendment.accountingAssessments.every(item => ['F006', 'F007', 'F008'].every(id => item[id] === 'resolved')) && review3Amendment.reports?.[0]?.verdict === 'clean' && review3Amendment.reports?.[1]?.verdict === 'clean' && review3Amendment.reports?.[2]?.verdict === 'findings' && review3Amendment.reviewAccounting?.engineUsageEvidence === 'unknown-not-inferred' && review3Amendment.reviewAccounting?.countersReset === false && review3Amendment.reviewAccounting?.capsRaised === false && review3Amendment.priorObservedRun === 'a0870891-de61-44e0-b349-34a03d90bf8f' && review3Amendment.separateControllerRun === '35236f06-4d33-409d-964a-e89b1bc70d42' && JSON.stringify(review3Amendment.additionalCaseIds) === JSON.stringify(['C76', 'C77', 'C78']) && review3Amendment.additionalVariants === 3 && review3Amendment.projectedCases === 78 && review3Amendment.projectedVariants === 79 && review3Amendment.priorObservedAccounting?.rootedInvocations === 22 && review3Amendment.priorObservedAccounting?.commandAttempts === 24 && review3Amendment.conditionalProjection?.notVerifiedTotals === true && review3Amendment.conditionalProjection?.engineUsageInferred === false && review3Amendment.conditionalProjection?.capsRaised === false && review3Amendment.conditionalProjection?.countersReset === false && review3Amendment.findings?.map(item => item.id).join(',') === 'F009,F010,F011';
  const validContinuationAmendment = continuationAmendment?.recordedBeforeReplacementExecution === true && continuationAmendment.sourceClassification?.includes('step 20') === true && continuationAmendment.sourcePath?.endsWith('/orchestration/program/recovery-7.json') === true && continuationAmendment.preservedFailure?.runId === '9d120cce-a0fe-42f4-bec0-163cf73199d1' && continuationAmendment.preservedFailure?.stage === 'case-ledger-validation' && continuationAmendment.preservedFailure?.cases === 76 && continuationAmendment.preservedFailure?.passed === 75 && continuationAmendment.preservedFailure?.failed === 1 && continuationAmendment.preservedFailure?.failedCase === 'C76' && continuationAmendment.preservedFailure?.failedCaseDurationMs === 1002.55 && JSON.stringify(continuationAmendment.preservedFailure?.unexecutedCaseIds) === JSON.stringify(['C77', 'C78']) && continuationAmendment.suppliedAccounting?.rootedInvocations === 23 && continuationAmendment.suppliedAccounting?.passedInvocations === 11 && continuationAmendment.suppliedAccounting?.failedInvocations === 12 && continuationAmendment.suppliedAccounting?.recordedOutcomes === 1232 && continuationAmendment.suppliedAccounting?.failedCases === 45 && continuationAmendment.suppliedAccounting?.unexecutedRootedSlots === 377 && continuationAmendment.suppliedAccounting?.unrootedFailedAttempts === 2 && continuationAmendment.suppliedAccounting?.unrootedUnexecutedSlots === 150 && continuationAmendment.suppliedAccounting?.revision05AttemptsSeparate === true && continuationAmendment.consumption?.taskStep === 19 && continuationAmendment.consumption?.workerTurns === 10 && continuationAmendment.consumption?.reviews === 9 && continuationAmendment.consumption?.reviewLimit === 12 && continuationAmendment.consumption?.repairs === 3 && continuationAmendment.consumption?.verificationFailures === 6 && continuationAmendment.consumption?.interventions === 1 && continuationAmendment.consumption?.audits === 0 && continuationAmendment.consumption?.engineUsageEvidence === 'unknown-not-inferred' && continuationAmendment.consumption?.countersReset === false && continuationAmendment.consumption?.capsRaised === false && continuationAmendment.preservedPriorProjection?.rootedAfterNextCompletion === 23 && continuationAmendment.preservedPriorProjection?.commandAttemptsAfterNextCompletion === 25 && continuationAmendment.preservedPriorProjection?.rootedAfterSeparateControllerGate === 24 && continuationAmendment.preservedPriorProjection?.commandAttemptsAfterSeparateControllerGate === 26 && continuationAmendment.revisedProjection?.replacementRootedInvocations === 24 && continuationAmendment.revisedProjection?.replacementTotalCommandAttempts === 26 && continuationAmendment.revisedProjection?.afterSubsequentControllerRootedInvocations === 25 && continuationAmendment.revisedProjection?.afterSubsequentControllerTotalCommandAttempts === 27 && continuationAmendment.revisedProjection?.notVerifiedTotals === true && continuationAmendment.revisedProjection?.engineUsageInferred === false && continuationAmendment.revisedProjection?.capsRaised === false && continuationAmendment.revisedProjection?.countersReset === false && continuationAmendment.caseLedger?.cases === 78 && continuationAmendment.caseLedger?.variants === 79 && continuationAmendment.caseLedger?.additionalCaseIds?.length === 0 && continuationAmendment.caseLedger?.additionalVariants === 0;
  const validReview4Amendment = review4Amendment?.recordedBeforeRepairExecution === true && review4Amendment.sourceClassification?.includes('round 4') === true && review4Amendment.sourceClassification?.includes('step 22') === true && review4Amendment.reports?.length === 3 && review4Amendment.reports?.[0]?.assessments?.F009 === 'resolved' && review4Amendment.reports?.[0]?.assessments?.F010 === 'resolved' && review4Amendment.reports?.[0]?.assessments?.F011 === 'open' && review4Amendment.reports?.[1]?.assessments?.F011 === 'resolved' && review4Amendment.reports?.[1]?.assessments?.F012 === 'open' && review4Amendment.reports?.[2]?.assessments?.F011 === 'resolved' && review4Amendment.reviewAccounting?.workerTurn === 11 && review4Amendment.reviewAccounting?.reviewsConsumed === 12 && review4Amendment.reviewAccounting?.reviewLimit === 12 && review4Amendment.reviewAccounting?.auditsConsumed === 0 && review4Amendment.reviewAccounting?.blockerCount === 0 && review4Amendment.reviewAccounting?.engineUsageEvidence === 'unknown-not-inferred' && review4Amendment.reviewAccounting?.countersReset === false && review4Amendment.reviewAccounting?.capsRaised === false && review4Amendment.priorObservedRun === 'a4e0ad69-eb89-4013-b23d-d33a5e36906e' && review4Amendment.priorObservedAccounting?.rootedInvocations === 25 && review4Amendment.priorObservedAccounting?.commandAttempts === 27 && JSON.stringify(review4Amendment.additionalCaseIds) === JSON.stringify(['C79']) && review4Amendment.additionalVariants === 1 && review4Amendment.projectedCases === 79 && review4Amendment.projectedVariants === 80 && review4Amendment.conditionalProjection?.rootedAfterNextCompletion === 26 && review4Amendment.conditionalProjection?.commandAttemptsAfterNextCompletion === 28 && review4Amendment.conditionalProjection?.rootedAfterSeparateControllerGate === 27 && review4Amendment.conditionalProjection?.commandAttemptsAfterSeparateControllerGate === 29 && review4Amendment.conditionalProjection?.notVerifiedTotals === true && review4Amendment.conditionalProjection?.engineUsageInferred === false && review4Amendment.conditionalProjection?.capsRaised === false && review4Amendment.conditionalProjection?.countersReset === false;
  if (!validContinuationAmendment || !validReview3Amendment || !validReview4Amendment || !allowance?.recordedBeforeReplacementExecution || allowance.originalProposal?.batches !== 8 || allowance.originalProposal.classification?.includes('proposal only') !== true || allowance.historicalCompletionAttempts?.length !== 5 || allowance.historicalCompletionAttempts.some((attempt, index) => attempt.attempt !== `attempt-${String(index + 1).padStart(2, '0')}`) || allowance.historicalConsumedCounters?.taskStep !== 4 || allowance.historicalConsumedCounters?.workerTurns !== 2 || allowance.historicalConsumedCounters?.verificationFailures !== 0 || !allowance.historicalConsumedCounters?.classification?.includes('not evidence') || !validHistoricalCounters || !validCurrentCounters || !validRecovery5Projection || !validRecovery6Consumption || !validRecovery6Projection || allowance.revision06BeforeReplacement?.invocations !== 17 || allowance.revision06BeforeReplacement?.passed !== 8 || allowance.revision06BeforeReplacement?.failed !== 9 || allowance.revision06BeforeReplacement?.recordedCases !== 936 || allowance.revision06BeforeReplacement?.unexecutedCases !== 225 || allowance.caseAmendment?.additionalCaseId !== 'C75' || allowance.caseAmendment?.additionalVariants !== 1 || allowance.caseAmendment?.projectedCases !== 75 || allowance.caseAmendment?.projectedVariants !== 76 || previousProjection?.projectedInvocationsAfterReplacement !== 20 || previousProjection?.projectedInvocationsAfterSubsequentController !== 21 || !previousProjection.classification?.includes('Historical') || !amendment?.recordedBeforeCompletionGateExecution || event?.identity !== 'controller-event:1ad22fd0-dd89-45d5-be90-cc685cbe62a9:T001:step-0009:worker-turn-5:completion-module-parse-failure' || event.classification !== 'controller-qualified-unrooted-command-event' || event.status !== 'failed' || event.command !== 'bun frontend/scripts/private-qualification/check.mjs --phase completion' || event.stage !== 'module-parse-before-check-entry' || event.diagnostic !== 'frontend/scripts/private-qualification/tests.mjs:511:7: Expected ";" but found "$"' || event.executedCases !== 0 || event.unexecutedCaseSlots !== 75 || event.receiptRootAllocated !== false || event.runId !== null || event.beginReceipt !== false || event.evidencePath !== historicalCounters?.sourcePath || event.evidenceClassification?.includes('Worker-reported') !== true || correctedProjection?.rootedInvocationsBeforeRecovery !== 17 || correctedProjection?.unrootedFailedCommandEvents !== 1 || correctedProjection?.unreconciledControllerGates !== 2 || correctedProjection?.replacementRootedInvocations !== 20 || correctedProjection?.replacementTotalCommandAttempts !== 21 || correctedProjection?.afterSubsequentControllerRootedInvocations !== 21 || correctedProjection?.afterSubsequentControllerTotalCommandAttempts !== 22 || correctedProjection?.engineUsageInferred !== false || correctedProjection?.capsRaised !== false || correctedProjection?.countersReset !== false)
    throw new Error('Causal allowance and pre-scope failure amendment is incomplete');
  if (!latestAmendment?.recordedAfterCompletionCommandFailure || additionalEvent?.identity !== 'controller-event:1ad22fd0-dd89-45d5-be90-cc685cbe62a9:T001:recovery-3:worker-turn-6:completion-baseline-parse-failure' || additionalEvent.classification !== 'controller-qualified-unrooted-command-event' || additionalEvent.status !== 'failed' || additionalEvent.command !== 'bun frontend/scripts/private-qualification/check.mjs --phase completion' || additionalEvent.stage !== 'baseline-parse-before-check-entry' || additionalEvent.diagnostic !== "frontend/scripts/private-qualification/generate.mjs:7:31: SyntaxError: JSON Parse error: Expected '}'" || additionalEvent.executedCases !== 0 || additionalEvent.unexecutedCaseSlots !== 75 || additionalEvent.receiptRootAllocated !== false || additionalEvent.runId !== null || additionalEvent.beginReceipt !== false || additionalEvent.evidenceClassification?.includes('Observed exact') !== true || latestProjection?.rootedInvocationsBeforeRecovery !== 17 || latestProjection?.unrootedFailedCommandEvents !== 2 || latestProjection?.unreconciledControllerGates !== 2 || latestProjection?.replacementRootedInvocations !== 20 || latestProjection?.replacementTotalCommandAttempts !== 22 || latestProjection?.afterSubsequentControllerRootedInvocations !== 21 || latestProjection?.afterSubsequentControllerTotalCommandAttempts !== 23 || latestProjection?.engineUsageInferred !== false || latestProjection?.capsRaised !== false || latestProjection?.countersReset !== false || priorHistory.commandAttemptAccounting?.unrootedCommandEvents?.length !== 2 || priorHistory.commandAttemptAccounting.unrootedFailedAttempts !== 2 || priorHistory.commandAttemptAccounting.unrootedUnexecutedCaseSlots !== 150)
    throw new Error('Latest pre-scope failure amendment is incomplete');
  const expectedAssociations = baselineContract.controllerGateRecovery.expectedControllerAssociations;
  if (!controllerRecovery || controllerRecovery.associations?.length !== expectedAssociations || priorHistory.controllerAssociations?.length !== expectedAssociations || priorHistory.commandAttemptAccounting.reconciledControllerGates !== expectedAssociations || priorHistory.commandAttemptAccounting.unreconciledControllerGates !== 0 || controllerRecovery.rootedInvocationCount !== priorHistory.totals.invocations)
    throw new Error('Validated controller associations do not match the live invocation union');
  if (baselineContract.newVariants.length !== 26 || baselineContract.newVariants.some((variant, index) => variant.id !== `C${String(index + 54).padStart(2, '0')}`))
    throw new Error('Current actual-caller case ledger is incomplete');
  const bound = readAndBindInputs();
  const result = deriveRet2Runner(bound.revision05);
  const patch = makeUnifiedPatch(bound.revision05, result.derived, result.edits);
  if (applyUnifiedPatch(bound.revision05, patch) !== result.derived)
    throw new Error('C1 runner patch does not reconstruct its candidate');
  const incrementalLines = countPatchLines(patch);
  const baselineLines = countPatchLines(bound.baselinePatchBytes.toString('utf8'));
  if (baselineLines.added !== 269 || baselineLines.deleted !== 135)
    throw new Error('Revision-05 measured vendor baseline changed');
  const fullDiff = differenceCounts(bound.originalRunner, result.derived);
  const supportSource = bound.support.toString('utf8');
  const hosts = section(result.derived, 'var privateCompletion = require', '\n// packages/playwright/src/runner/loaderHost.ts') +
    section(result.derived, 'var InProcessLoaderHost = class', '\n// packages/playwright/src/runner/loadUtils.ts') +
    'var lastWorkerIndex = 0;\n' +
    section(result.derived, 'var WorkerHost = class', '\n// packages/playwright/src/runner/dispatcher.ts');
  const lastRunReporter = section(result.derived, 'var LastRunReporter = class', '\n// packages/playwright/src/runner/testRunner.ts');
  const consumers = section(result.derived, 'var Dispatcher = class', '\nvar JobDispatcher = class') +
    section(result.derived, 'async function loadFileSuites(', '\nasync function createRootSuite(') +
    section(result.derived, 'async function finishTaskRun(', '\nfunction createGlobalSetupTasks(') +
    section(result.derived, 'async function runAllTestsWithConfig(', '\n// packages/playwright/src/runner/testServer.ts') + lastRunReporter;
  const jobDispatchers = section(result.derived, 'var JobDispatcher = class', '\nfunction chunkFromParams');
  const multiplexer = section(result.derived, 'var Multiplexer = class', '\n// packages/playwright/src/reporters/internalReporter.ts');
  for (const region of [hosts.split('var InProcessLoaderHost')[0], consumers.split('async function loadFileSuites')[0], consumers.slice(consumers.indexOf('async function loadFileSuites'), consumers.indexOf('async function finishTaskRun')), jobDispatchers, multiplexer, lastRunReporter])
    if (!result.derived.includes(region.trimEnd())) throw new Error('Derived caller extraction identity mismatch');
  const { prepareHistoricalTests } = await import('./tests.mjs');
  const tests = prepareHistoricalTests(bound.historicalTests.toString('utf8'), new URL('./receipts.mjs', import.meta.url).href);
  const outputDirectory = 'revision-06';
  scope.mkdir(outputDirectory);
  const sourceManifest = {
    selected: '@playwright/test/cli.js',
    version: baselineContract.selectedVersion,
    identities: bound.actualSources,
    revision05: {
      runnerPatchSHA256: sha256(bound.baselinePatchBytes),
      runnerSHA256: baselineContract.revision05.runnerDerived.sha256,
      supportSHA256: baselineContract.revision05.support.sha256,
      testsSHA256: baselineContract.revision05.tests.sha256,
      attempt05ReceiptSHA256: baselineContract.revision05.attempt05Receipt.sha256,
      c43ObservedRetainCalls: case43Count(bound.historicalCases)
    },
    c1: {
      labels: result.edits.map(edit => edit.label),
      incrementalVendorAdded: incrementalLines.added,
      incrementalVendorDeleted: incrementalLines.deleted,
      fullVendorAdded: fullDiff.added,
      fullVendorDeleted: fullDiff.deleted,
      exactIncrementPatchSHA256: sha256(patch)
    },
    derived: { sha256: sha256(result.derived), bytes: Buffer.byteLength(result.derived) },
    support: { sha256: sha256(supportSource), bytes: Buffer.byteLength(supportSource) },
    caseLedger: {
      historicalIds: Array.from({ length: 53 }, (_, index) => `C${String(index + 1).padStart(2, '0')}`),
      executedIds: Array.from({ length: 79 }, (_, index) => `C${String(index + 1).padStart(2, '0')}`),
      c43Variant: 'C43-ret2-retain-zero',
      newCaseCount: 26,
      newCases: baselineContract.newVariants
    },
    repositoryInputs: bound.inputs
  };
  const outputs = new Map([
    [`${outputDirectory}/runner.derived.cjs`, result.derived],
    [`${outputDirectory}/runner-c1.patch`, patch],
    [`${outputDirectory}/privateCompletion.cjs`, bound.support],
    [`${outputDirectory}/hosts.derived.js`, hosts],
    [`${outputDirectory}/consumers.derived.js`, consumers],
    [`${outputDirectory}/jobDispatchers.derived.js`, jobDispatchers],
    [`${outputDirectory}/multiplexer.derived.js`, multiplexer],
    [`${outputDirectory}/source-manifest.json`, JSON.stringify(sourceManifest, null, 2) + '\n'],
    ['tests.mjs', tests]
  ]);
  const sourceBytes = [...outputs.values()].reduce((total, content) => total + Buffer.byteLength(content), 0);
  const forecastBytes = sourceBytes + 2 * 1024 * 1024;
  if (forecastBytes > maxSourceBytes) throw new Error('Bounded source-generation forecast exceeds the output envelope');
  scope.measure(forecastBytes);
  for (const [path, content] of outputs) scope.write(path, content);
  const generated = [...outputs].map(([path, content]) => ({ path, sha256: sha256(content), bytes: Buffer.byteLength(content), lines: lineCount(content) }));
  const sourceCounts = bound.inputs.map(({ path, bytes, lines, sha256: digest }) => ({ path, bytes, lines, sha256: digest }));
  const commandAttemptProjection = {
    priorRootedInvocations: priorHistory.totals.invocations,
    priorCommandAttemptAccounting: priorHistory.commandAttemptAccounting,
    currentCommandAttempts: 1,
    projectedRootedInvocationsAfterCurrent: priorHistory.totals.invocations + 1,
    projectedTotalCommandAttemptsAfterCurrent: priorHistory.commandAttemptAccounting.reconciledCommandAttempts + 1,
    projectedRootedInvocationsAfterSubsequentController: priorHistory.totals.invocations + 2,
    projectedTotalCommandAttemptsAfterSubsequentController: priorHistory.commandAttemptAccounting.reconciledCommandAttempts + 2,
    classification: 'Explicit projection; observed totals are written after this command is reconciled.'
  };
  const report = {
    sourceIdentities: bound.actualSources,
    immutableRevision05: sourceManifest.revision05,
    vendorLines: { revision05Added: baselineLines.added, revision05Deleted: baselineLines.deleted, c1Added: incrementalLines.added, c1Deleted: incrementalLines.deleted, originalToCandidateAdded: fullDiff.added, originalToCandidateDeleted: fullDiff.deleted },
    sourceCounts,
    extractionDuplicates: {
      hostsLines: lineCount(hosts),
      consumersLines: lineCount(consumers),
      jobDispatcherLines: lineCount(jobDispatchers),
      multiplexerLines: lineCount(multiplexer),
      note: 'Extracted methods are VM test inputs duplicated from the generated runner, not additional vendor runtime edits.'
    },
    cases: { historical: 53, new: 26, executed: 79, variants: 80, IDs: sourceManifest.caseLedger.executedIds, c43Revision05RetainCalls: 1, c43CurrentExpectedRetainCalls: 0 },
    revision06PriorInvocations: priorHistory,
    commandAttemptAccounting: commandAttemptProjection,
    controllerGateRecovery: controllerRecovery,
    causalAllowanceAmendment: baselineContract.causalAllowanceAmendment,
    generated,
    patchReconstruction: 'PASS: actual installed runner plus the immutable revision-05 patch reconstructs revision-05 byte-for-byte; revision-05 plus runner-c1.patch reconstructs the fresh candidate byte-for-byte.'
  };
  scope.write('generation.json', JSON.stringify(report, null, 2) + '\n');
  return { ...report, root: scope.root, runId: scope.id, generated, sourceManifest, priorHistory, commandAttemptAccounting: commandAttemptProjection };
}

function case43Count(cases) {
  return cases.find(test => test.id === 'C43').calls.retain;
}
