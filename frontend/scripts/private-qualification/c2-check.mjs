import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { assertC2BudgetEnvelope, generateC2 } from './c2-generate.mjs';
import { applyC2Edits } from './c2-edits.mjs';
import { expectedC2CaseVariants, runC2Tests, runC2DynamicTests } from './c2-tests.mjs';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..');
const expectedBaselineSHA256 = 'eb3aa68403869d3c9565be4dc3985910d7c3764d189556a2b5da82174b6b6db3';
const expectedStageRoot = '/private/tmp/herdr-fullscreen-c2-20261006-0004';
const expectedDynamicStageRoot = '/private/tmp/herdr-fullscreen-c2-dynamic-20261006';
const expectedFollowupStageRoot = '/private/tmp/herdr-fullscreen-c2-followup-20261006';
const expectedFollowupPacketRoot = '/private/tmp/fullscreen-c2-followup-20261006';
const expectedCoordinateStageRoot = '/private/tmp/herdr-fullscreen-c2-coordinate-followup-20261006';
const expectedCoordinatePacketRoot = '/private/tmp/fullscreen-c2-coordinate-followup-20261006';
const expectedGStageRoot = '/private/tmp/herdr-fullscreen-c2-dynamic-mapping-G-20261006';
const expectedGPacketRoot = '/private/tmp/fullscreen-c2-dynamic-mapping-G-20261006';
const gInvocationLimitBytes = 64 * 1024 * 1024;
const gStageLimitBytes = 80 * 1024 * 1024;
const gCampaignLimitBytes = 640 * 1024 * 1024;
const gBudgetLimits = { invocationBytes: gInvocationLimitBytes, invocationEntries: 8192, depth: 32, stageBytes: gStageLimitBytes, campaignBytes: gCampaignLimitBytes };
const gInvocationEnvelope = { allocatedBytes: gInvocationLimitBytes, entries: 8192, maxDepth: 32 };
const gStageEnvelope = { allocatedBytes: gStageLimitBytes, entries: 8192, maxDepth: 32 };
const gCampaignEnvelope = { allocatedBytes: gCampaignLimitBytes };
const expectedDynamicPacket = '/private/tmp/fullscreen-c2-dynamic-repair-20261006';
const invocationLimitBytes = 64 * 1024 * 1024;
const coordinateInvocationLimitBytes = 32 * 1024 * 1024;
const followupStageLimitBytes = 128 * 1024 * 1024;
const coordinateStageLimitBytes = 72 * 1024 * 1024;
const followupCampaignLimitBytes = 512 * 1024 * 1024;
const coordinateAttemptForecastBytes = 24 * 1024 * 1024;
const expectedDynamicManifestSHA256 = '47b2f46b70b760dda40aebb0a66b3897f66975c0d0a641035c8be1800ab04506';
const expectedDynamicInventorySHA256 = '39b5647cfc555097e1738b411e9b13b946ae0488dca09dfd45782af8da2b895f';
const expectedBaselinePath = '/private/tmp/fullscreen-c2-stage-plan-20261006-0004/baseline.json';
const supplementalAllowanceFilename = 'supplemental-allowance-20261006.json';
const supplementalAllowanceSHA256 = 'a192da79b1531773e44299f7f55b219547d868b489ad535b454e2b4da437bb40';
const supplementalAmendmentFilename = 'supplemental-allowance-amendment-20261006.json';
const supplementalAmendmentSHA256 = 'a1462423f85f719359378d5ba126c398833465b9f279b57f7a5cf551788f13a2';
const supplementalAmendment2Filename = 'supplemental-allowance-amendment-02-20261006.json';
const supplementalAmendment2SHA256 = '9c9ffb335894ef6bba4a4d5494ff66f3aabb5e34cc19f45da51f7a46c9e10913';
const attempt10PreflightFilename = 'attempt10-static-preflight.json';
const attempt10PreflightScript = '/private/tmp/fullscreen-c2-stage-plan-20261006-0004/attempt10-static-checks.mjs';
const staticPreflightFilename = 'attempt8-static-preflight.json';
const staticPreflightSHA256 = '43880b3139d051db336588d01cb65c285da348468557dafbc7cabd6400e16e93';
const staticPreflightScriptSHA256 = 'ceb4c355cee6ccac0d6db460e42972ec5f7fbba6e77d9d6d9d53b6978a4505a9';
const staticPreflightRevision02Filename = 'attempt8-static-preflight-revision-02.json';
const staticPreflightRevision02SHA256 = 'bfae09b38806742234f21d8923b8841a04e85bed6a9d26aca0589e45d03e59a5';
const staticPreflightRevision02ScriptSHA256 = 'cb536455c7e8f4bf44eb54dadf74f9df9c1748d86c614c8b346c98d328d12549';
const staticPreflightRevisionFilename = 'attempt8-static-preflight-revision-03.json';
const staticPreflightScript = '/private/tmp/fullscreen-c2-stage-plan-20261006-0004/attempt8-static-checks.mjs';
const expectedC2Variants = 83;
const expectedC2CaseIds = Array.from({ length: 20 }, (_, index) => `C2-${String(index + 1).padStart(2, '0')}`);
if (expectedC2CaseVariants.length !== 20 || expectedC2CaseVariants.reduce((sum, item) => sum + item.variants.length, 0) !== expectedC2Variants || expectedC2CaseVariants.some((item, index) => item.id !== expectedC2CaseIds[index]))
  throw new Error('C2 case and variant plan differs from the dated supplemental acceptance');
const historicalFailedInvocationIds = [
  'b9aa9ec3-98ea-45b1-905b-9b0c1324bf2e',
  '3a37d688-615c-4a39-9c69-d5780adfba45',
  '3480a90a-90f5-436b-8cec-96e8e2673c75',
  'b795b695-10ae-45f8-8a4b-9ffd55fdfc7a',
  '3b67464a-9659-48f9-98dd-1fcc602f61f9',
  '3bdc011a-42a2-4723-a420-27759e1adedb'
];
const supplementalAllowanceRecord = {
  schema: 1,
  recordId: 'herdr-fullscreen-c2-supplemental-20261006',
  recordedAt: '2026-10-06T03:18:24Z',
  authorization: 'Human recovery guidance in this workflow turn',
  stageRoot: expectedStageRoot,
  baselineSHA256: expectedBaselineSHA256,
  originalInvocationCap: 6,
  historicalFailedWorkerInvocationIds: historicalFailedInvocationIds,
  supplementalInvocations: [
    { attemptNumber: 7, phase: 'worker', requirement: 'Final corrected source; pass all 20 cases and 83 required variants with final-byte binding.' },
    { attemptNumber: 8, phase: 'controller', requirement: 'Run only if attempt 7 passes the full native-disabled worker gate.' }
  ],
  totalInvocationCap: 8,
  limits: { stageBytes: 402653184, invocationBytes: 67108864, maxEntries: 8192, maxDepth: 32 },
  acceptance: { cases: 20, variants: 83, nativeEffects: 0, projectTestInvocations: 0 },
  constraints: { preserveHistoricalReceipts: true, preserveC1Evidence: true, allowedRepositoryPaths: [
    'frontend/scripts/run-browser-tests.sh',
    'frontend/scripts/private-qualification/c2-generate.mjs',
    'frontend/scripts/private-qualification/c2-edits.mjs',
    'frontend/scripts/private-qualification/c2-bootstrap.cjs',
    'frontend/scripts/private-qualification/c2-paths.cjs',
    'frontend/scripts/private-qualification/c2-route.cjs',
    'frontend/scripts/private-qualification/c2-tests.mjs',
    'frontend/scripts/private-qualification/c2-check.mjs'
  ], noFurtherAllowance: true }
};
const supplementalAmendmentRecord = {
  schema: 1,
  recordId: 'herdr-fullscreen-c2-supplemental-amendment-20261006-0002',
  recordedAt: '2026-10-06T04:31:00Z',
  authorization: 'Human workflow guidance to proceed with the separately recorded step-5 proposal',
  stageRoot: expectedStageRoot,
  baselineSHA256: expectedBaselineSHA256,
  amendmentTo: { recordId: supplementalAllowanceRecord.recordId, path: supplementalAllowanceFilename, sha256: supplementalAllowanceSHA256, originalInvocationCap: 6, priorTotalInvocationCap: 8 },
  preservedFailedWorkerInvocationIds: [...historicalFailedInvocationIds, '7909f781-7e6d-47a6-a444-46ce2c4cf7e6'],
  supplementalInvocations: [
    { attemptNumber: 8, phase: 'worker', requirement: 'Pass all 20 cases and 83 variants with final-byte binding and native effects disabled.' },
    { attemptNumber: 9, phase: 'controller', requirement: 'Run only if attempt 8 passes all 20 cases and 83 variants with final-byte binding.' }
  ],
  totalInvocationCap: 9,
  limits: { stageBytes: 402653184, invocationBytes: 67108864, maxEntries: 8192, maxDepth: 32 },
  acceptance: { cases: 20, variants: 83, nativeEffects: 0, projectTestInvocations: 0 },
  constraints: {
    preserveHistoricalReceipts: true,
    preserveOriginalAllowanceRecord: true,
    preserveC1Evidence: true,
    allowedRepositoryPaths: [
      'frontend/scripts/run-browser-tests.sh',
      'frontend/scripts/private-qualification/c2-generate.mjs',
      'frontend/scripts/private-qualification/c2-edits.mjs',
      'frontend/scripts/private-qualification/c2-bootstrap.cjs',
      'frontend/scripts/private-qualification/c2-paths.cjs',
      'frontend/scripts/private-qualification/c2-route.cjs',
      'frontend/scripts/private-qualification/c2-tests.mjs',
      'frontend/scripts/private-qualification/c2-check.mjs'
    ],
    noFurtherAllowance: true
  }
};
const supplementalAmendment2Record = {
  schema: 1,
  recordId: 'herdr-fullscreen-c2-supplemental-amendment-20261006-0003',
  recordedAt: '2026-10-06T06:42:00Z',
  authorization: 'Human workflow guidance to add a fresh worker gate and conditional controller gate after attempt 8 failed',
  stageRoot: expectedStageRoot,
  baselineSHA256: expectedBaselineSHA256,
  amendmentTo: { recordId: supplementalAmendmentRecord.recordId, path: supplementalAmendmentFilename, sha256: supplementalAmendmentSHA256, priorTotalInvocationCap: 9 },
  preservedFailedWorkerInvocationIds: [...historicalFailedInvocationIds, '7909f781-7e6d-47a6-a444-46ce2c4cf7e6', '7ce4a3ec-b453-4a8d-b4a6-312b510369bd'],
  preservedUnusedConditionalInvocationNumbers: [9],
  supplementalInvocations: [
    { attemptNumber: 10, phase: 'worker', requirement: 'Pass all 20 cases and 83 variants with final-byte binding and native effects disabled.' },
    { attemptNumber: 11, phase: 'controller', requirement: 'Run only if attempt 10 passes all 20 cases and 83 variants with final-byte binding.' }
  ],
  totalInvocationCap: 11,
  limits: { stageBytes: 402653184, invocationBytes: 67108864, maxEntries: 8192, maxDepth: 32 },
  acceptance: { cases: 20, variants: 83, nativeEffects: 0, projectTestInvocations: 0 },
  constraints: {
    preserveHistoricalReceipts: true,
    preservePriorAmendments: true,
    preserveUnusedConditionalAttempt9: true,
    preserveC1Evidence: true,
    allowedRepositoryPaths: [
      'frontend/scripts/run-browser-tests.sh',
      'frontend/scripts/private-qualification/c2-generate.mjs',
      'frontend/scripts/private-qualification/c2-edits.mjs',
      'frontend/scripts/private-qualification/c2-bootstrap.cjs',
      'frontend/scripts/private-qualification/c2-paths.cjs',
      'frontend/scripts/private-qualification/c2-route.cjs',
      'frontend/scripts/private-qualification/c2-tests.mjs',
      'frontend/scripts/private-qualification/c2-check.mjs'
    ],
    noFurtherAllowance: true
  }
};
const allowedRepositoryPaths = new Set([
  'frontend/scripts/run-browser-tests.sh',
  'frontend/scripts/private-qualification/c2-generate.mjs',
  'frontend/scripts/private-qualification/c2-edits.mjs',
  'frontend/scripts/private-qualification/c2-bootstrap.cjs',
  'frontend/scripts/private-qualification/c2-paths.cjs',
  'frontend/scripts/private-qualification/c2-route.cjs',
  'frontend/scripts/private-qualification/c2-tests.mjs',
  'frontend/scripts/private-qualification/c2-check.mjs'
]);
const requireFromHere = createRequire(import.meta.url);
const bunRuntime = globalThis.Bun;

function sha256(value) {
  return crypto.createHash('sha256').update(value).digest('hex');
}

function fileIdentity(target, expected = {}) {
  const stat = fs.lstatSync(target);
  if (!stat.isFile() || stat.isSymbolicLink()) throw new Error(`Bound source is not a regular file: ${target}`);
  const bytes = fs.readFileSync(target);
  const actual = { sha256: sha256(bytes), bytes: bytes.length, dev: String(stat.dev), ino: String(stat.ino), uid: Number(stat.uid) };
  for (const key of ['sha256', 'bytes', 'dev', 'ino', 'uid'])
    if (expected[key] !== undefined && String(actual[key]) !== String(expected[key]))
      throw new Error(`Bound source identity changed: ${target} (${key})`);
  return actual;
}

function readJournal(stage) {
  if (!stage.journal) return [];
  const before = fs.lstatSync(stage.journal.path);
  if (!before.isFile() || before.isSymbolicLink() || Number(before.uid) !== stage.uid || Number(before.nlink) !== 1 || fs.realpathSync(stage.journal.path) !== stage.journal.path)
    throw new Error('C2 stage journal identity refused');
  const bytes = fs.readFileSync(stage.journal.path);
  const after = fs.lstatSync(stage.journal.path);
  if (before.dev !== after.dev || before.ino !== after.ino || before.size !== after.size || bytes.length !== after.size)
    throw new Error('C2 stage journal changed while reading');
  stage.journal.identity = { dev: String(after.dev), ino: String(after.ino), uid: Number(after.uid), mode: Number(after.mode), nlink: Number(after.nlink), size: Number(after.size), file: after.isFile(), directory: after.isDirectory(), symlink: after.isSymbolicLink() };
  return bytes.toString('utf8').split('\n').filter(Boolean).map(line => JSON.parse(line));
}

function verifyBoundIdentity(item, root = repositoryRoot) {
  const absolute = path.isAbsolute(item.path) ? item.path : path.resolve(root, item.path);
  return fileIdentity(absolute, item);
}

function readStageRecord(stage, filename) {
  const target = path.join(stage.path, filename);
  const before = fs.lstatSync(target);
  if (!before.isFile() || before.isSymbolicLink() || Number(before.uid) !== stage.uid || Number(before.nlink) !== 1 || (Number(before.mode) & 0o077) !== 0 || fs.realpathSync(target) !== target)
    throw new Error(`C2 stage record identity refused: ${filename}`);
  const bytes = fs.readFileSync(target);
  const after = fs.lstatSync(target);
  if (before.dev !== after.dev || before.ino !== after.ino || before.size !== after.size || bytes.length !== after.size)
    throw new Error(`C2 stage record changed while reading: ${filename}`);
  return { path: target, bytes, sha256: sha256(bytes) };
}

function recordSecondAmendment(stage, journal, pathTools) {
  const amendments = journal.filter(row => row.type === 'SUPPLEMENTAL_ALLOWANCE_AMENDMENT');
  if (amendments.length < 1 || amendments.length > 2 || amendments[0].recordId !== supplementalAmendmentRecord.recordId || amendments[0].recordSHA256 !== supplementalAmendmentSHA256 || journal.some(row => row.type === 'BEGIN' && row.attemptNumber === 9))
    throw new Error('Second C2 amendment requires the intact first amendment and unused conditional attempt 9');
  const target = path.join(stage.path, supplementalAmendment2Filename);
  const bytes = Buffer.from(JSON.stringify(supplementalAmendment2Record, null, 2) + '\n');
  if (amendments.length === 2) {
    const identity = fs.lstatSync(target);
    const saved = fs.readFileSync(target);
    const event = amendments[1];
    if (!identity.isFile() || identity.isSymbolicLink() || identity.nlink !== 1 || !saved.equals(bytes) || event.recordId !== supplementalAmendment2Record.recordId || event.recordPath !== target || event.recordSHA256 !== sha256(bytes) || event.supersedesSHA256 !== supplementalAmendmentSHA256)
      throw new Error('Recorded second C2 amendment differs from its pinned append-only authorization');
    return journal;
  }
  if (fs.existsSync(target) || !stage.journal) throw new Error('Second amendment sidecar already exists or the append-only journal is unavailable');
  const created = pathTools.writeStageFile(stage, supplementalAmendment2Filename, bytes);
  const event = { type: 'SUPPLEMENTAL_ALLOWANCE_AMENDMENT', recordId: supplementalAmendment2Record.recordId, recordPath: created.path, recordSHA256: sha256(bytes), recordedAt: supplementalAmendment2Record.recordedAt, supersedesSHA256: supplementalAmendmentSHA256 };
  stage.journal.identity = pathTools.appendStageFile(stage, stage.journal.path, stage.journal.identity, JSON.stringify(event) + '\n');
  return [...journal, event];
}

function verifySupplementalAllowance(stage, journal) {
  const original = readStageRecord(stage, supplementalAllowanceFilename);
  const amendment = readStageRecord(stage, supplementalAmendmentFilename);
  const amendment2 = readStageRecord(stage, supplementalAmendment2Filename);
  const originalBytes = Buffer.from(JSON.stringify(supplementalAllowanceRecord, null, 2) + '\n');
  const amendmentBytes = Buffer.from(JSON.stringify(supplementalAmendmentRecord, null, 2) + '\n');
  const amendment2Bytes = Buffer.from(JSON.stringify(supplementalAmendment2Record, null, 2) + '\n');
  if (original.sha256 !== supplementalAllowanceSHA256 || !original.bytes.equals(originalBytes) || amendment.sha256 !== supplementalAmendmentSHA256 || !amendment.bytes.equals(amendmentBytes) || amendment2.sha256 !== supplementalAmendment2SHA256 || !amendment2.bytes.equals(amendment2Bytes) || JSON.stringify(supplementalAmendmentRecord.constraints.allowedRepositoryPaths) !== JSON.stringify([...allowedRepositoryPaths]) || JSON.stringify(supplementalAmendment2Record.constraints.allowedRepositoryPaths) !== JSON.stringify([...allowedRepositoryPaths]))
    throw new Error('Dated C2 allowance records differ from their pinned authorizations or source boundary');
  const originalEntries = journal.filter(row => row.type === 'SUPPLEMENTAL_ALLOWANCE');
  if (originalEntries.length !== 1 || originalEntries[0].recordId !== supplementalAllowanceRecord.recordId || originalEntries[0].recordPath !== original.path || originalEntries[0].recordSHA256 !== supplementalAllowanceSHA256 || originalEntries[0].recordedAt !== supplementalAllowanceRecord.recordedAt)
    throw new Error('C2 journal does not contain exactly one unchanged original allowance record');
  const amendmentEntries = journal.filter(row => row.type === 'SUPPLEMENTAL_ALLOWANCE_AMENDMENT');
  if (amendmentEntries.length !== 2 || amendmentEntries[0].recordId !== supplementalAmendmentRecord.recordId || amendmentEntries[0].recordPath !== amendment.path || amendmentEntries[0].recordSHA256 !== supplementalAmendmentSHA256 || amendmentEntries[0].recordedAt !== supplementalAmendmentRecord.recordedAt || amendmentEntries[0].supersedesSHA256 !== supplementalAllowanceSHA256 || amendmentEntries[1].recordId !== supplementalAmendment2Record.recordId || amendmentEntries[1].recordPath !== amendment2.path || amendmentEntries[1].recordSHA256 !== supplementalAmendment2SHA256 || amendmentEntries[1].recordedAt !== supplementalAmendment2Record.recordedAt || amendmentEntries[1].supersedesSHA256 !== supplementalAmendmentSHA256)
    throw new Error('C2 journal does not contain the ordered, separately bound supplemental amendments');
  return {
    path: original.path,
    sha256: supplementalAllowanceSHA256,
    recordedAt: supplementalAllowanceRecord.recordedAt,
    originalCap: 6,
    priorTotalCap: 8,
    amendment: { path: amendment.path, sha256: supplementalAmendmentSHA256, recordedAt: supplementalAmendmentRecord.recordedAt, attempts: [8, 9] },
    amendment2: { path: amendment2.path, sha256: supplementalAmendment2SHA256, recordedAt: supplementalAmendment2Record.recordedAt, attempts: [10, 11], preservesUnusedAttempt: 9 },
    totalCap: 11
  };
}

function verifySavedStaticPreflight(stage, journal, allowance) {
  const original = readStageRecord(stage, staticPreflightFilename);
  const originalEvents = journal.filter(row => row.type === 'STATIC_PREFLIGHT');
  if (original.sha256 !== staticPreflightSHA256 || originalEvents.length !== 1 || originalEvents[0].recordPath !== original.path || originalEvents[0].recordSHA256 !== original.sha256 || originalEvents[0].amendmentSHA256 !== allowance.amendment.sha256 || originalEvents[0].scriptSHA256 !== staticPreflightScriptSHA256)
    throw new Error('Original C2 static-preflight report or journal binding changed');
  const originalRecord = JSON.parse(original.bytes.toString('utf8'));
  if (originalRecord.status !== 'passed' || originalRecord.exitCode !== 0 || originalRecord.result?.historicalPreservation?.amendmentRecords !== 0)
    throw new Error('Original static-preflight evidence no longer preserves its recorded amendment-count discrepancy');
  const revisions = journal.filter(row => row.type === 'STATIC_PREFLIGHT_REVISION');
  if (revisions.length !== 2 || revisions[0].supersedesSHA256 !== original.sha256 || revisions[0].recordSHA256 !== staticPreflightRevision02SHA256 || revisions[1].supersedesSHA256 !== staticPreflightRevision02SHA256)
    throw new Error('C2 static-preflight revisions are missing, reordered, or not append-only');
  const firstRevision = readStageRecord(stage, staticPreflightRevision02Filename);
  if (firstRevision.sha256 !== staticPreflightRevision02SHA256 || revisions[0].recordPath !== firstRevision.path || revisions[0].amendmentSHA256 !== allowance.amendment.sha256 || revisions[0].scriptSHA256 !== staticPreflightRevision02ScriptSHA256)
    throw new Error('First corrected static-preflight revision or journal binding changed');
  const firstRecord = JSON.parse(firstRevision.bytes.toString('utf8'));
  if (firstRecord.status !== 'passed' || firstRecord.exitCode !== 0 || firstRecord.revisionOfSHA256 !== original.sha256 || firstRecord.result?.historicalPreservation?.amendmentRecords !== 1 || firstRecord.result?.historicalPreservation?.amendmentEventType !== 'SUPPLEMENTAL_ALLOWANCE_AMENDMENT')
    throw new Error('First corrected preflight did not verify the amendment event type and count');
  const attempt8Revision = readStageRecord(stage, staticPreflightRevisionFilename);
  if (revisions[1].recordPath !== attempt8Revision.path || revisions[1].recordSHA256 !== attempt8Revision.sha256 || revisions[1].amendmentSHA256 !== allowance.amendment.sha256)
    throw new Error('Attempt-8 static-preflight revision is not bound by its append-only journal record');
  const attempt8Record = JSON.parse(attempt8Revision.bytes.toString('utf8'));
  const attempt8ScriptIdentity = fileIdentity(staticPreflightScript);
  if (attempt8Record.status !== 'passed' || attempt8Record.revisionOfSHA256 !== firstRevision.sha256 || attempt8Record.scriptPath !== staticPreflightScript || attempt8Record.scriptSHA256 !== attempt8ScriptIdentity.sha256 || attempt8Record.result?.historicalPreservation?.amendmentRecords !== 1)
    throw new Error('Attempt-8 static-preflight revision no longer preserves its historical source-only result');
  const saved = readStageRecord(stage, attempt10PreflightFilename);
  const records = journal.filter(row => row.type === 'STATIC_PREFLIGHT_ATTEMPT10');
  if (records.length !== 1 || records[0].recordPath !== saved.path || records[0].recordSHA256 !== saved.sha256 || records[0].amendmentSHA256 !== allowance.amendment2.sha256 || records[0].supersedesSHA256 !== attempt8Revision.sha256)
    throw new Error('Attempt-10 static preflight is not bound as the next append-only preparation record');
  const record = JSON.parse(saved.bytes.toString('utf8'));
  const result = record.result;
  const currentHashes = [...allowedRepositoryPaths].map(file => ({ path: file, sha256: sha256(fs.readFileSync(path.join(repositoryRoot, file))) }));
  const recordedHashes = new Map(result?.finalSourceHashes?.map(file => [file.path, file.sha256]));
  const scriptIdentity = fileIdentity(attempt10PreflightScript);
  const requiredChecks = ['syntax', 'lint', 'wrapperSyntax', 'generatedSourceBindings', 'generatedClosure', 'toolImportClosure', 'privateResolution', 'historicalPreservation', 'caseOutcomeRetention'];
  if (record.schema !== 1 || record.status !== 'passed' || record.supersedesSHA256 !== attempt8Revision.sha256 || record.command !== `bun ${attempt10PreflightScript}` || record.exitCode !== 0 || record.stderr !== '' || typeof record.stdout !== 'string' || record.amendmentSHA256 !== allowance.amendment2.sha256 || record.scriptPath !== attempt10PreflightScript || record.scriptSHA256 !== scriptIdentity.sha256 || record.recordedAt !== records[0].recordedAt || records[0].command !== record.command || records[0].scriptSHA256 !== record.scriptSHA256 || records[0].exitCode !== record.exitCode || records[0].stdoutSHA256 !== sha256(Buffer.from(record.stdout)) || result?.status !== 'passed' || result.projectTestsExecuted !== 0 || result.nativeEffects !== 0 || result.bindings?.sourceBindingCount !== 9 || result.bindings?.additionalContractCount !== 23 || result.generatedClosure?.inputs !== 82 || result.generatedClosure?.staticEdges !== 1133 || result.generatedClosure?.dynamicSites !== 37 || result.generatedClosure?.unresolvedEdges !== 0 || result.casePlan?.cases !== 20 || result.casePlan?.variants !== 83 || result.historicalPreservation?.allowanceRecords !== 1 || result.historicalPreservation?.amendmentRecords !== 2 || result.historicalPreservation?.amendmentEventType !== 'SUPPLEMENTAL_ALLOWANCE_AMENDMENT' || result.historicalPreservation?.unusedConditionalAttemptNumbers?.includes(9) !== true || !Array.isArray(result.checks) || result.checks.some(check => check.exitCode !== 0 || check.status !== 'passed' || typeof check.command !== 'string') || requiredChecks.some(name => !result.checks.some(check => check.name === name)) || recordedHashes.size !== allowedRepositoryPaths.size || currentHashes.some(file => recordedHashes.get(file.path) !== file.sha256))
    throw new Error('Attempt-10 static preflight is incomplete, failed, or bound to different final source bytes');
  return { originalPath: original.path, originalSHA256: original.sha256, attempt8RevisionPath: attempt8Revision.path, attempt8RevisionSHA256: attempt8Revision.sha256, currentPath: saved.path, currentSHA256: saved.sha256, recordedAt: record.recordedAt, scriptPath: record.scriptPath, scriptSHA256: record.scriptSHA256, checks: result.checks.length, sourceFiles: currentHashes.length, stdoutSHA256: records[0].stdoutSHA256, amendmentRecords: result.historicalPreservation.amendmentRecords, unusedConditionalAttemptNumbers: result.historicalPreservation.unusedConditionalAttemptNumbers };
}

function validateSupplementalHistory(stage, begins, terminals, phase, allowance) {
  if (begins.length !== 8 && begins.length !== 9)
    throw new Error('Second amendment permits only attempt 10 worker and conditional attempt 11 controller');
  const terminalById = new Map(terminals.map(row => [row.invocationId, row]));
  for (let index = 0; index < historicalFailedInvocationIds.length; index += 1) {
    const id = historicalFailedInvocationIds[index];
    const begin = begins[index];
    const terminal = terminalById.get(id);
    const root = path.join(stage.path, `invocation-${id}`);
    if (begin?.invocationId !== id || begin.phase !== 'worker' || begin.attemptNumber !== index + 1 || begin.baselineSHA256 !== expectedBaselineSHA256 || begin.invocationRoot !== root || terminal?.status !== 'failed' || terminal.phase !== 'worker' || terminal.invocationRoot !== root || terminal.baselineSHA256 !== expectedBaselineSHA256 || terminal.nativeEffects !== 0)
      throw new Error(`Historical failed C2 attempt was reordered, relabelled, or changed: ${id}`);
    const receipt = JSON.parse(fs.readFileSync(path.join(root, 'completion-result.json'), 'utf8'));
    if (receipt.phase !== 'worker' || receipt.status !== 'failed' || receipt.baselineSHA256 !== expectedBaselineSHA256 || (receipt.cases ?? 0) !== 0 || (receipt.passed ?? 0) !== 0 || (receipt.nativeEffects ?? 0) !== 0 || receipt.outputsRetained !== true || (receipt.currentC2Counters?.projectTestInvocations ?? 0) !== 0 || receipt.currentC2Counters?.sourceGateInvocations !== index + 1)
      throw new Error(`Historical failed C2 receipt no longer preserves its original counters: ${id}`);
  }
  const failedId = '7909f781-7e6d-47a6-a444-46ce2c4cf7e6';
  const failedBegin = begins[6];
  const failedTerminal = terminalById.get(failedId);
  const failedRoot = path.join(stage.path, `invocation-${failedId}`);
  const failedReceipt = JSON.parse(fs.readFileSync(path.join(failedRoot, 'completion-result.json'), 'utf8'));
  if (failedBegin?.invocationId !== failedId || failedBegin.phase !== 'worker' || failedBegin.attemptNumber !== 7 || failedBegin.invocationRoot !== failedRoot || failedBegin.baselineSHA256 !== expectedBaselineSHA256 || failedBegin.supplementalAllowanceSHA256 !== allowance.sha256 || failedTerminal?.phase !== 'worker' || failedTerminal.status !== 'failed' || failedTerminal.nativeEffects !== 0 || failedReceipt.phase !== 'worker' || failedReceipt.status !== 'failed' || failedReceipt.baselineSHA256 !== expectedBaselineSHA256 || failedReceipt.currentC2Counters?.sourceGateInvocations !== 7 || failedReceipt.currentC2Counters?.projectTestInvocations !== 0 || (failedReceipt.cases ?? 0) !== 0 || (failedReceipt.variants ?? 0) !== 0 || failedReceipt.nativeEffects !== 0 || failedReceipt.outputsRetained !== true)
    throw new Error('Supplemental attempt 7 failure or counters were changed before the new amendment');
  const attempt8Id = '7ce4a3ec-b453-4a8d-b4a6-312b510369bd';
  const attempt8Begin = begins[7];
  const attempt8Terminal = terminalById.get(attempt8Id);
  const attempt8Root = path.join(stage.path, `invocation-${attempt8Id}`);
  const attempt8Receipt = JSON.parse(fs.readFileSync(path.join(attempt8Root, 'completion-result.json'), 'utf8'));
  const attempt8Cases = attempt8Receipt.verification?.sourceCases;
  if (attempt8Begin?.invocationId !== attempt8Id || attempt8Begin.phase !== 'worker' || attempt8Begin.attemptNumber !== 8 || attempt8Begin.invocationRoot !== attempt8Root || attempt8Begin.baselineSHA256 !== expectedBaselineSHA256 || attempt8Begin.supplementalAmendmentSHA256 !== allowance.amendment.sha256 || attempt8Terminal?.phase !== 'worker' || attempt8Terminal.status !== 'failed' || attempt8Receipt.phase !== 'worker' || attempt8Receipt.status !== 'failed' || attempt8Receipt.currentC2Counters?.sourceGateInvocations !== 8 || attempt8Receipt.currentC2Counters?.projectTestInvocations !== 0 || attempt8Receipt.nativeEffects !== 0 || attempt8Receipt.outputsRetained !== true || attempt8Cases?.status !== 'failed' || attempt8Cases.cases !== 20 || attempt8Cases.passed !== 19 || attempt8Cases.failed !== 1 || attempt8Cases.variants !== expectedC2Variants)
    throw new Error('Failed attempt 8 or its individual C2-13 failure summary changed before the new amendment');
  if (begins.some(begin => begin.attemptNumber === 9)) throw new Error('Previously unused conditional attempt 9 was consumed or relabelled');
  if (begins.length === 8) {
    if (phase !== 'worker') throw new Error('Amended attempt 10 must be the worker gate');
    return;
  }
  const workerBegin = begins[8];
  const workerTerminal = terminalById.get(workerBegin?.invocationId);
  const workerRoot = path.join(stage.path, `invocation-${workerBegin?.invocationId}`);
  if (phase !== 'controller' || workerBegin?.phase !== 'worker' || workerBegin.attemptNumber !== 10 || workerBegin.baselineSHA256 !== expectedBaselineSHA256 || workerBegin.invocationRoot !== workerRoot || workerBegin.supplementalAllowanceSHA256 !== allowance.sha256 || workerBegin.supplementalAmendmentSHA256 !== allowance.amendment.sha256 || workerBegin.supplementalAmendment2SHA256 !== allowance.amendment2.sha256 || workerTerminal?.phase !== 'worker' || workerTerminal.status !== 'passed' || workerTerminal.attemptNumber !== 10 || workerTerminal.supplementalAmendment2SHA256 !== allowance.amendment2.sha256)
    throw new Error('Attempt 11 controller gate requires the passing, final-byte-bound amended worker attempt 10');
}

function verifyWorkerReceipt(stage, workerTerminal, allowance) {
  const workerRoot = path.join(stage.path, `invocation-${workerTerminal.invocationId}`);
  const receiptPath = path.join(workerRoot, 'completion-result.json');
  const workerReceipt = JSON.parse(fs.readFileSync(receiptPath, 'utf8'));
  if (workerReceipt.phase !== 'worker' || workerReceipt.status !== 'passed' || workerReceipt.invocationId !== workerTerminal.invocationId || workerReceipt.invocationRoot !== workerRoot || workerReceipt.stageRoot !== stage.path || workerReceipt.cases !== 20 || workerReceipt.passed !== 20 || workerReceipt.failed !== 0 || workerReceipt.variants !== expectedC2Variants || workerReceipt.nativeEffects !== 0 || workerReceipt.baselineSHA256 !== expectedBaselineSHA256 || workerReceipt.attemptNumber !== 10 || workerReceipt.currentC2Counters?.sourceGateInvocations !== 9 || workerReceipt.currentC2Counters?.projectTestInvocations !== 0 || workerReceipt.supplementalAllowance?.amendment?.sha256 !== allowance.amendment.sha256 || workerReceipt.supplementalAllowance?.amendment2?.sha256 !== allowance.amendment2.sha256 || workerTerminal.attemptNumber !== 10 || workerTerminal.supplementalAmendment2SHA256 !== allowance.amendment2.sha256 || workerReceipt.historicalC1?.cases !== 79 || workerReceipt.historicalC1?.passed !== 79 || workerReceipt.historicalC1?.variants !== 80 || workerReceipt.historicalCountersPreserved !== true || workerReceipt.invocationBytes > 64 * 1024 * 1024 || workerReceipt.invocationAllocatedBytes > 64 * 1024 * 1024)
    throw new Error('Supplemental worker receipt does not prove the exact 20-case/83-variant native-disabled pass with unchanged C1 evidence');
  const currentFiles = [...allowedRepositoryPaths].map(file => ({ path: file, hash: sha256(fs.readFileSync(path.join(repositoryRoot, file))) }));
  const workerFiles = workerReceipt.accounting?.ordinaryRepositoryFiles;
  const workerHashes = new Map(workerFiles?.map(file => [file.path, file.hash]));
  if (!Array.isArray(workerFiles) || workerHashes.size !== allowedRepositoryPaths.size || currentFiles.some(file => workerHashes.get(file.path) !== file.hash))
    throw new Error('Supplemental worker receipt is not bound to the current final repository source bytes');
  if (!workerReceipt.sourceManifestPath?.startsWith(`${workerRoot}${path.sep}`))
    throw new Error('Supplemental worker source manifest escaped its private invocation');
  const sourceManifestIdentity = fs.lstatSync(workerReceipt.sourceManifestPath);
  if (!sourceManifestIdentity.isFile() || sourceManifestIdentity.isSymbolicLink() || Number(sourceManifestIdentity.uid) !== Number(stage.uid) || Number(sourceManifestIdentity.nlink) !== 1 || fs.realpathSync(workerReceipt.sourceManifestPath) !== workerReceipt.sourceManifestPath || sha256(fs.readFileSync(workerReceipt.sourceManifestPath)) !== workerReceipt.sourceManifestSHA256)
    throw new Error('Supplemental worker source manifest is missing or changed after its receipt');
  const testsPath = path.join(workerRoot, 'tests.ndjson');
  const testsIdentity = fs.lstatSync(testsPath);
  if (!testsIdentity.isFile() || testsIdentity.isSymbolicLink() || Number(testsIdentity.uid) !== Number(stage.uid) || Number(testsIdentity.nlink) !== 1 || fs.realpathSync(testsPath) !== testsPath)
    throw new Error('Supplemental worker case evidence path is not a private regular file');
  const testsBytes = fs.readFileSync(testsPath);
  if (sha256(testsBytes) !== workerReceipt.testsSHA256)
    throw new Error('Supplemental worker case/variant evidence changed after its receipt');
  const tests = testsBytes.toString('utf8').trim().split('\n').map(line => JSON.parse(line));
  const caseVariants = tests.reduce((sum, item, index) => {
    const expected = expectedC2CaseVariants[index];
    if (item.id !== expectedC2CaseIds[index] || item.id !== expected?.id || JSON.stringify(item.variants) !== JSON.stringify(expected?.variants) || item.status !== 'passed' || item.nativeEffects !== 0)
      throw new Error(`Supplemental worker case evidence differs from the bound 20-case contract at ${index}`);
    return sum + item.variants.length;
  }, 0);
  if (tests.length !== 20 || caseVariants !== expectedC2Variants || workerReceipt.accounting?.sourceExtractions?.c2Cases !== 20 || workerReceipt.accounting?.sourceExtractions?.variants !== expectedC2Variants)
    throw new Error('Supplemental worker case IDs or required variant coverage differs from the bound acceptance');
  return { receiptPath, sourceManifestSHA256: workerReceipt.sourceManifestSHA256, testsSHA256: workerReceipt.testsSHA256, cases: tests.length, variants: caseVariants, currentSourceHashes: currentFiles };
}

function verifyBaseline(baseline) {
  const checked = { changedFiles: 0, deletedPaths: 0, inputs: 0, closureFiles: 0, qualificationSources: 0, c1Artifacts: 0, acceptedRecords: 0, requirements: 0 };
  for (const item of baseline.changedFiles) {
    const target = path.resolve(repositoryRoot, item.path);
    if (item.state.includes('D')) {
      if (fs.existsSync(target)) throw new Error(`Baseline deleted path reappeared: ${item.path}`);
      checked.deletedPaths += 1;
    } else {
      verifyBoundIdentity(item);
      checked.changedFiles += 1;
    }
  }
  for (const item of baseline.inputs) {
    if (item.path === 'frontend/scripts/run-browser-tests.sh') continue;
    verifyBoundIdentity(item);
    checked.inputs += 1;
  }
  for (const item of baseline.closureCandidate) {
    verifyBoundIdentity(item);
    checked.closureFiles += 1;
  }
  for (const item of baseline.qualificationSources) {
    verifyBoundIdentity(item);
    checked.qualificationSources += 1;
  }
  for (const item of baseline.reusableC1) {
    verifyBoundIdentity(item, '/');
    checked.c1Artifacts += 1;
  }
  for (const item of baseline.acceptedRecordBinding) {
    verifyBoundIdentity(item, '/');
    checked.acceptedRecords += 1;
  }
  for (const item of baseline.requirements) {
    verifyBoundIdentity(item, '/');
    checked.requirements += 1;
  }
  return checked;
}

function sourceBoundWrapper(baseline) {
  const target = path.join(repositoryRoot, 'frontend/scripts/run-browser-tests.sh');
  const current = fs.readFileSync(target, 'utf8');
  const start = current.indexOf('if [ "${HERDR_FULLSCREEN_C2_PRIVATE:-}" = 1 ]; then');
  const end = current.indexOf('\n\nSCRIPT_DIR=', start);
  if (start < 0 || end < start) throw new Error('C2 wrapper branch boundaries are not uniquely present');
  const stock = current.slice(0, start) + current.slice(end + 2);
  const identity = baseline.inputs.find(item => item.path === 'frontend/scripts/run-browser-tests.sh');
  if (!identity || sha256(Buffer.from(stock)) !== identity.sha256)
    throw new Error('Original browser wrapper changed outside the single private branch');
  return { current, stock, identity };
}

function statfsRecord(target) {
  const stat = fs.statfsSync(target);
  const availableBytes = Number(stat.bavail) * Number(stat.bsize);
  const totalBytes = Number(stat.blocks) * Number(stat.bsize);
  return { path: target, availableBytes, totalBytes, sampledAt: new Date().toISOString() };
}

function enforceSpace(sample, baseline, invocationBudgetBytes) {
  const GiB = 1024 ** 3;
  const saved = baseline.scopedSpaceObservation?.availableBytes;
  const policy = { preflightGiB: 32, stopGiB: 28, hardStopGiB: 24 };
  if (!Number.isFinite(sample.availableBytes) || sample.availableBytes < policy.preflightGiB * GiB)
    throw new Error(`Scoped /private/tmp space is below the saved ${policy.preflightGiB} GiB pre-generation floor`);
  if (sample.availableBytes - invocationBudgetBytes < policy.stopGiB * GiB)
    throw new Error(`Projected C2 output would cross the saved ${policy.stopGiB} GiB stop line`);
  if (sample.availableBytes - invocationBudgetBytes < policy.hardStopGiB * GiB)
    throw new Error(`Projected C2 output would cross the saved ${policy.hardStopGiB} GiB hard stop`);
  return { ...sample, baselineAvailableBytes: saved, deltaFromBaseline: sample.availableBytes - saved, policy, projectedRemainingBytes: sample.availableBytes - invocationBudgetBytes };
}

function estimateScope(baseline, projectInputs, effectiveInvocationLimits = null) {
  const excluded = baseline.closureCandidate.filter(item => item.path.includes('/node_modules/playwright/node_modules/fsevents/'));
  const logicalBytes = baseline.closureTotals.logicalBytes - excluded.reduce((sum, item) => sum + item.bytes, 0) + projectInputs.reduce((sum, item) => sum + item.bytes, 0) + 16 * 1024 * 1024;
  const allocatedBytes = baseline.closureTotals.allocatedBytes - excluded.reduce((sum, item) => sum + item.allocatedBytes, 0) + projectInputs.reduce((sum, item) => sum + item.allocatedBytes, 0) + 20 * 1024 * 1024;
  const entries = baseline.closureCandidate.length - excluded.length + projectInputs.length + 1024;
  const maxDepth = Math.max(...baseline.closureCandidate.map(item => item.path.split('/').length)) + 8;
  const limit = 64 * 1024 * 1024;
  if (effectiveInvocationLimits) assertC2BudgetEnvelope({ allocatedBytes, entries, maxDepth }, effectiveInvocationLimits, 'projected invocation');
  else if (logicalBytes > limit || allocatedBytes > limit || entries > 8192 || maxDepth > 32)
    throw new Error('C2 closure forecast exceeds the per-invocation 64 MiB/8192-entry/depth-32 envelope');
  return { logicalBytes, allocatedBytes, entries, maxDepth, limitBytes: limit, budgetBasis: effectiveInvocationLimits ? 'allocatedBytes' : 'allocatedBytes-and-logicalBytes', excludedOptional: excluded.map(item => item.path) };
}

function walkScope(root) {
  const entries = [];
  let logicalBytes = 0;
  let allocatedBytes = 0;
  let maxDepth = 0;
  const pending = [{ target: root, depth: 0 }];
  while (pending.length) {
    const { target, depth } = pending.pop();
    const stat = fs.lstatSync(target);
    if (stat.isSymbolicLink()) {
      entries.push({ path: path.relative(root, target) || '.', kind: 'symlink', bytes: stat.size, allocatedBytes: Number(stat.blocks || 0) * 512, dev: String(stat.dev), ino: String(stat.ino), uid: Number(stat.uid), mode: Number(stat.mode), nlink: Number(stat.nlink) });
      logicalBytes += stat.size;
      allocatedBytes += Number(stat.blocks || 0) * 512;
      maxDepth = Math.max(maxDepth, depth);
      continue;
    }
    if (!stat.isDirectory() && !stat.isFile()) throw new Error(`C2 output contains a non-regular object: ${target}`);
    const kind = stat.isDirectory() ? 'directory' : 'file';
    const allocated = Number(stat.blocks || 0) * 512;
    entries.push({ path: path.relative(root, target) || '.', kind, bytes: stat.isFile() ? stat.size : 0, allocatedBytes: allocated, dev: String(stat.dev), ino: String(stat.ino), uid: Number(stat.uid), mode: Number(stat.mode), nlink: Number(stat.nlink) });
    allocatedBytes += allocated;
    if (stat.isFile()) logicalBytes += stat.size;
    maxDepth = Math.max(maxDepth, depth);
    if (stat.isDirectory()) {
      for (const name of fs.readdirSync(target)) pending.push({ target: path.join(target, name), depth: depth + 1 });
    }
  }
  return { entries, entryCount: entries.length, logicalBytes, allocatedBytes, maxDepth };
}

function validateRegisteredOutputs(scope, childScopes, knownNegativePaths = []) {
  const registered = new Map();
  for (const item of scope.snapshot().entries) registered.set(path.resolve(scope.root, item.path), item);
  for (const child of childScopes) {
    for (const item of child.pathPolicy.snapshot().entries) {
      const absolute = item.path === '.' ? child.root : path.resolve(child.root, item.path);
      registered.set(absolute, item);
    }
  }
  const knownNegative = new Set(knownNegativePaths.map(target => path.resolve(target)));
  const walk = walkScope(scope.root);
  const unregistered = walk.entries.filter(item => !registered.has(path.resolve(scope.root, item.path)) && !knownNegative.has(path.resolve(scope.root, item.path)));
  const changed = walk.entries.filter(item => {
    const expected = registered.get(path.resolve(scope.root, item.path));
    return expected && !knownNegative.has(path.resolve(scope.root, item.path)) && (expected.kind !== item.kind || expected.dev !== item.dev || expected.ino !== item.ino || expected.uid !== item.uid || expected.mode !== item.mode || expected.kind === 'file' && expected.nlink !== item.nlink);
  });
  if (walk.entryCount > 8192 || walk.maxDepth > 32 || walk.logicalBytes > 64 * 1024 * 1024 || walk.allocatedBytes > 64 * 1024 * 1024)
    throw new Error('Measured C2 invocation output exceeds its 64 MiB/8192-entry/depth-32 envelope');
  if (unregistered.length || changed.length)
    throw new Error(`C2 invocation contains unregistered or replaced filesystem outputs: ${[...unregistered, ...changed].slice(0, 8).map(item => item.path).join(', ')}`);
  for (const target of knownNegative) if (!fs.existsSync(target)) throw new Error(`Refusal fixture disappeared: ${target}`);
  return { ...walk, registeredObjects: registered.size, knownNegativeObjects: knownNegative.size, unregisteredObjects: unregistered.length, changedObjects: changed.length };
}

async function runCommand(command, args, cwd) {
  const child = bunRuntime.spawn([command, ...args], { cwd, stdout: 'pipe', stderr: 'pipe' });
  const [stdout, stderr, exitCode] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited]);
  return { command: [command, ...args].join(' '), cwd, exitCode, stdout: stdout.slice(-16000), stderr: stderr.slice(-16000) };
}

function syntaxAndTranspile(generated, runtimeRoot) {
  const parsed = [];
  for (const file of generated.manifest.generatedFiles) {
    if (!/\.[cm]?js$/.test(file.privatePath)) continue;
    const source = fs.readFileSync(file.privatePath, 'utf8');
    new bunRuntime.Transpiler({ loader: 'js', target: 'bun' }).transformSync(source);
    parsed.push({ path: file.privatePath, sha256: sha256(Buffer.from(source)), mode: 'transpile-parse-only' });
  }
  const tsInputs = [
    ['frontend/playwright.config.ts', path.join(runtimeRoot, 'project/frontend/playwright.config.ts')],
    ['frontend/tests/browser/mobile-journeys.spec.ts', path.join(runtimeRoot, 'project/frontend/tests/browser/mobile-journeys.spec.ts')],
    ['frontend/tests/browser/release-resources.spec.ts', path.join(runtimeRoot, 'project/frontend/tests/browser/release-resources.spec.ts')],
    ['frontend/scripts/pages-policy.ts', path.join(runtimeRoot, 'project/frontend/scripts/pages-policy.ts')]
  ];
  const transpiled = [];
  for (const [sourcePath, privatePath] of tsInputs) {
    const source = fs.readFileSync(privatePath, 'utf8');
    const output = new bunRuntime.Transpiler({ loader: 'ts', target: 'bun' }).transformSync(source);
    if (!output || typeof output !== 'string') throw new Error(`TypeScript parser did not return output: ${sourcePath}`);
    transpiled.push({ source: sourcePath, path: privatePath, inputSHA256: sha256(Buffer.from(source)), outputSHA256: sha256(Buffer.from(output)), inputBytes: Buffer.byteLength(source), outputBytes: Buffer.byteLength(output), executed: false });
  }
  for (const pathName of ['frontend/scripts/private-qualification/c2-generate.mjs', 'frontend/scripts/private-qualification/c2-edits.mjs', 'frontend/scripts/private-qualification/c2-tests.mjs', 'frontend/scripts/private-qualification/c2-check.mjs']) {
    const source = fs.readFileSync(path.join(repositoryRoot, pathName), 'utf8');
    new bunRuntime.Transpiler({ loader: 'js', target: 'bun' }).transformSync(source);
  }
  for (const pathName of ['frontend/scripts/private-qualification/c2-bootstrap.cjs', 'frontend/scripts/private-qualification/c2-paths.cjs', 'frontend/scripts/private-qualification/c2-route.cjs']) {
    const source = fs.readFileSync(path.join(repositoryRoot, pathName), 'utf8');
    new bunRuntime.Transpiler({ loader: 'js', target: 'bun' }).transformSync(source);
  }
  return { parsedJavaScriptFiles: parsed.length, parsedJavaScript: parsed, transpiledTypeScript: transpiled, executedProjectFiles: 0 };
}

function vendorAccounting(manifest, tests, wrapperSource, fixtureOriginal) {
  const c1Added = 481;
  const c1Deleted = 168;
  const c2Added = manifest.vendorCounts.added;
  const c2Deleted = manifest.vendorCounts.deleted;
  const originalFixtureLines = fixtureOriginal.length ? fixtureOriginal.split('\n').length - (fixtureOriginal.endsWith('\n') ? 1 : 0) : 0;
  const fixtureCounts = manifest.fixtureEdits.reduce((sum, edit) => ({
    added: sum.added + Math.max(0, edit.newText.split('\n').length - (edit.newText.endsWith('\n') ? 1 : 0) - (edit.oldText.split('\n').length - (edit.oldText.endsWith('\n') ? 1 : 0))),
    deleted: sum.deleted + Math.max(0, edit.oldText.split('\n').length - (edit.oldText.endsWith('\n') ? 1 : 0) - (edit.newText.split('\n').length - (edit.newText.endsWith('\n') ? 1 : 0)))
  }), { added: 0, deleted: 0 });
  if (originalFixtureLines !== manifest.fixture.sourceLines) throw new Error('Release fixture source line accounting differs');
  const fixtureLines = { original: originalFixtureLines, generated: manifest.fixture.generatedLines, ...fixtureCounts };
  const files = [...allowedRepositoryPaths].map(file => {
    const content = fs.readFileSync(path.join(repositoryRoot, file));
    const text = content.toString('utf8');
    return { path: file, bytes: content.length, lines: text.length ? text.split('\n').length - (text.endsWith('\n') ? 1 : 0) : 0, hash: sha256(content) };
  });
  const wrapperLineDelta = wrapperSource.current.split('\n').length - wrapperSource.stock.split('\n').length;
  return {
    c1HistoricalRunnerAgainstInstalled: { added: c1Added, deleted: c1Deleted, reusedWithoutRerun: true },
    c2IncrementalVendor: { added: c2Added, deleted: c2Deleted, patchedFiles: [...new Set(manifest.vendorEdits.map(edit => edit.file))] },
    composedVendor: { added: c1Added + c2Added, deleted: c1Deleted + c2Deleted },
    ordinaryRepositoryFiles: files,
    wrapperBranch: { addedLines: wrapperLineDelta, originalContractBytesPreserved: true },
    generatedReleaseFixture: fixtureLines,
    sourceExtractions: { uniqueBindings: Object.keys(tests.sourceBindings).length, repeatedCaseReferences: tests.cases.reduce((sum, item) => sum + item.bindings.length, 0), c2Cases: tests.expectedCases, variants: tests.variantCount },
    closure: { copiedFiles: manifest.closure.copiedEntries, originalLogicalBytes: manifest.closure.candidateLogicalBytes, outputLogicalBytes: manifest.output.logicalBytes, outputFiles: manifest.output.files, staticEdges: manifest.closure.staticEdges.length, dynamicSites: manifest.closure.dynamicSites.length }
  };
}

function maybeRestoreEnv(before) {
  for (const [key, value] of Object.entries(before)) {
    if (value === undefined) delete process.env[key];
    else process.env[key] = value;
  }
}

function repairVariantPlan(inventory) {
  const ids = inventory.sites.map(site => site.siteId);
  const allowed = new Set(['DS01', 'DS02', 'DS05', 'DS23', 'DS24', 'DS25', 'DS26', 'DS31', 'DS32', 'DS34', 'DS35', 'DS36', 'DS37']);
  return [
    ...expectedC2CaseVariants,
    { id: 'C2-21', variants: ids.map(id => `${id}-guarded`) },
    { id: 'C2-22', variants: ids.filter(id => allowed.has(id)).map(id => `${id}-allowed`) },
    { id: 'C2-23', variants: ids.map(id => `${id}-refused`) },
    { id: 'C2-24', variants: ['require-config', 'import-config-esm', 'transform-bundle-load', 'loader-emitted-require', 'loader-emitted-import', 'package-json-read', 'refused-stock-leak', 'refused-unapproved-computed'] }
  ];
}

function readDynamicRepairInputs(baselinePath, inventoryPath) {
  const manifestPath = path.join(expectedDynamicPacket, 'evidence/repair-manifest.json');
  const manifestBytes = fs.readFileSync(manifestPath);
  if (sha256(manifestBytes) !== expectedDynamicManifestSHA256) throw new Error('Pinned C2 dynamic repair manifest changed');
  const repairManifest = JSON.parse(manifestBytes.toString('utf8'));
  for (const item of repairManifest.preRepairSources)
    if (sha256(fs.readFileSync(path.join(expectedDynamicPacket, item.copy))) !== item.sha256)
      throw new Error(`Pinned pre-repair source snapshot changed: ${item.repositoryPath}`);
  for (const group of Object.values(repairManifest.evidence))
    for (const item of Array.isArray(group) ? group : [group])
      if (sha256(fs.readFileSync(path.join(expectedDynamicPacket, item.path))) !== item.sha256)
        throw new Error(`Pinned C2 dynamic evidence changed: ${item.path}`);
  const baselineBytes = fs.readFileSync(baselinePath);
  if (sha256(baselineBytes) !== expectedBaselineSHA256) throw new Error('Saved C2 baseline SHA-256 changed');
  if (sha256(fs.readFileSync(repairManifest.pinned.baseline.path)) !== repairManifest.pinned.baseline.sha256)
    throw new Error('Pinned baseline identity changed');
  const inventoryBytes = fs.readFileSync(inventoryPath);
  if (sha256(inventoryBytes) !== expectedDynamicInventorySHA256 || sha256(fs.readFileSync(path.join(expectedDynamicPacket, 'evidence/dynamic-site-inventory.json'))) !== expectedDynamicInventorySHA256)
    throw new Error('Pinned dynamic-site inventory changed');
  for (const item of [...repairManifest.pinned.oldStage.files, ...repairManifest.pinned.oldStage.attempt11Receipts, ...repairManifest.pinned.c3b900c5, ...repairManifest.pinned.ed2e47a3, ...repairManifest.pinned.closeoutPacket, repairManifest.pinned.c2PlanV2])
    if (sha256(fs.readFileSync(item.path)) !== item.sha256) throw new Error(`Preserved C2 history changed: ${item.path}`);
  const oldStage = walkScope(repairManifest.pinned.oldStage.root);
  if (oldStage.allocatedBytes !== repairManifest.pinned.oldStage.allocatedBytes || oldStage.entryCount !== repairManifest.pinned.oldStage.entries)
    throw new Error('Read-only historical C2 stage size or entry count changed');
  const baseline = JSON.parse(baselineBytes.toString('utf8'));
  const inventory = JSON.parse(inventoryBytes.toString('utf8'));
  if (inventory.total !== 37 || inventory.sites.length !== 37 || repairManifest.pinned.baseline.sha256 !== expectedBaselineSHA256)
    throw new Error('C2 dynamic repair inputs differ from the pinned 37-site plan');
  return { repairManifest, baseline, inventory, inventorySHA256: expectedDynamicInventorySHA256 };
}

function readFollowupBinding(attempt, currentSources, inputs, bindingPath) {
  const packetStat = fs.lstatSync(expectedFollowupPacketRoot);
  const uid = process.getuid?.();
  if (!Number.isInteger(uid) || !packetStat.isDirectory() || packetStat.isSymbolicLink() || Number(packetStat.uid) !== uid || (Number(packetStat.mode) & 0o077) !== 0 || fs.realpathSync(expectedFollowupPacketRoot) !== expectedFollowupPacketRoot)
    throw new Error('C2 follow-up packet is not a private canonical directory');
  const expectedBindingPath = path.join(expectedFollowupPacketRoot, 'bindings', `${attempt}-v2.json`);
  if (bindingPath !== expectedBindingPath) throw new Error(`C2 follow-up ${attempt} binding path differs from the separately versioned record`);
  const bindingStat = fs.lstatSync(bindingPath);
  if (!bindingStat.isFile() || bindingStat.isSymbolicLink() || Number(bindingStat.uid) !== uid || Number(bindingStat.nlink) !== 1 || (Number(bindingStat.mode) & 0o077) !== 0 || fs.realpathSync(bindingPath) !== bindingPath)
    throw new Error(`C2 follow-up ${attempt} source binding identity is refused`);
  const bytes = fs.readFileSync(bindingPath);
  const after = fs.lstatSync(bindingPath);
  if (bindingStat.dev !== after.dev || bindingStat.ino !== after.ino || bindingStat.size !== after.size || bytes.length !== after.size)
    throw new Error(`C2 follow-up ${attempt} source binding changed while reading`);
  const record = JSON.parse(bytes.toString('utf8'));
  if (record.schema !== 1 || record.stageRoot !== expectedFollowupStageRoot || record.attempt !== attempt || record.baselineSHA256 !== expectedBaselineSHA256 || record.inventorySHA256 !== expectedDynamicInventorySHA256 || record.authorization !== 'separately-accounted owner-authorized E1/E2 qualification')
    throw new Error(`C2 follow-up ${attempt} source binding differs from its authorized plan`);
  const paths = [...allowedRepositoryPaths];
  if (JSON.stringify(Object.keys(record.sourceHashes || {}).sort()) !== JSON.stringify([...paths].sort()) || paths.some(file => record.sourceHashes[file] !== currentSources[file]))
    throw new Error(`C2 follow-up ${attempt} source hashes were not bound to the current eight source bytes`);
  const limits = record.limits;
  if (limits?.invocationBytes !== invocationLimitBytes || limits?.invocationEntries !== 8192 || limits?.depth !== 32 || limits?.stageBytes !== followupStageLimitBytes || limits?.campaignBytes !== followupCampaignLimitBytes)
    throw new Error(`C2 follow-up ${attempt} limits differ from the owner authorization`);
  const bindingSHA256 = sha256(bytes);
  const readPacketJson = name => {
    const target = path.join(expectedFollowupPacketRoot, name);
    const stat = fs.lstatSync(target);
    if (!stat.isFile() || stat.isSymbolicLink() || Number(stat.uid) !== uid || Number(stat.nlink) !== 1 || (Number(stat.mode) & 0o077) !== 0 || fs.realpathSync(target) !== target)
      throw new Error(`C2 follow-up packet file identity is refused: ${name}`);
    const data = fs.readFileSync(target);
    const afterRead = fs.lstatSync(target);
    if (stat.dev !== afterRead.dev || stat.ino !== afterRead.ino || stat.size !== afterRead.size || data.length !== afterRead.size)
      throw new Error(`C2 follow-up packet file changed while reading: ${name}`);
    return { value: JSON.parse(data.toString('utf8')), sha256: sha256(data) };
  };
  const expectedPreflightPath = attempt === 'E1' ? 'preflight-v2.json' : 'preflight-E2-v2.json';
  if (record.authorizationPath !== 'authorization-v2.json' || record.preservedHistoryPath !== 'preserved-history-identities-v2.json' || record.preflightPath !== expectedPreflightPath)
    throw new Error(`C2 follow-up ${attempt} binding references unsupported evidence files`);
  const authorization = readPacketJson(record.authorizationPath);
  const preservation = readPacketJson(record.preservedHistoryPath);
  const preflight = readPacketJson(record.preflightPath).value;
  if (authorization.sha256 !== record.authorizationSHA256 || preservation.sha256 !== record.preservedHistorySHA256 || authorization.value.authorization !== record.authorization || authorization.value.authorizedRoot !== expectedFollowupStageRoot || JSON.stringify(authorization.value.attempts) !== JSON.stringify(['E1', 'E2']) || authorization.value.preservedHistorySnapshot?.sha256 !== preservation.sha256 || preservation.value.createdFor !== 'herdr-fullscreen-c2-followup-20261006-v2' || preflight.status !== 'passed' || preflight.bindingSHA256 !== bindingSHA256 || preflight.preservedHistorySHA256 !== preservation.sha256 || preflight.authorizationSHA256 !== authorization.sha256 || preflight.baselineSHA256 !== expectedBaselineSHA256 || preflight.inventorySHA256 !== expectedDynamicInventorySHA256 || paths.some(file => preflight.sourceHashes?.[file] !== currentSources[file]))
    throw new Error(`C2 follow-up ${attempt} is not tied to its preflight and preserved-history authorization`);
  return { path: bindingPath, sha256: bindingSHA256, sourceHashes: record.sourceHashes, record };
}

function verifyCoordinateRootSnapshot(rootRecord) {
  if (!rootRecord || typeof rootRecord.path !== 'string' || !Array.isArray(rootRecord.entries))
    throw new Error('C2 coordinate preservation root snapshot is malformed');
  const rootStat = fs.lstatSync(rootRecord.path);
  if (!rootStat.isDirectory() || rootStat.isSymbolicLink() || fs.realpathSync(rootRecord.path) !== rootRecord.path)
    throw new Error(`C2 coordinate preservation root is not canonical: ${rootRecord.path}`);
  const expected = new Map(rootRecord.entries.map(entry => [entry.path, entry]));
  if (expected.size !== rootRecord.entries.length) throw new Error(`C2 coordinate preservation snapshot has duplicate paths: ${rootRecord.name}`);
  const seen = new Set();
  const pending = [{ target: rootRecord.path, depth: 0 }];
  while (pending.length) {
    const { target, depth } = pending.pop();
    const relativePath = path.relative(rootRecord.path, target) || '.';
    const stat = fs.lstatSync(target);
    const kind = stat.isSymbolicLink() ? 'symlink' : stat.isDirectory() ? 'directory' : stat.isFile() ? 'file' : 'special';
    const row = expected.get(relativePath);
    if (!row || row.kind !== kind || row.dev !== String(stat.dev) || row.ino !== String(stat.ino) || row.mode !== Number(stat.mode) || row.uid !== Number(stat.uid) || row.gid !== Number(stat.gid) || row.nlink !== Number(stat.nlink) || row.size !== Number(stat.size) || row.blocks !== Number(stat.blocks || 0) || row.depth !== depth || row.mtimeMs !== stat.mtimeMs || row.ctimeMs !== stat.ctimeMs)
      throw new Error(`C2 coordinate preservation identity changed: ${rootRecord.name}/${relativePath}`);
    if (kind === 'special') throw new Error(`C2 coordinate preservation contains a special object: ${relativePath}`);
    if (kind === 'symlink') {
      if (row.linkTarget !== fs.readlinkSync(target)) throw new Error(`C2 coordinate preservation link changed: ${rootRecord.name}/${relativePath}`);
    } else if (kind === 'file') {
      const bytes = fs.readFileSync(target);
      const after = fs.lstatSync(target);
      if (bytes.length !== stat.size || after.dev !== stat.dev || after.ino !== stat.ino || after.size !== stat.size || sha256(bytes) !== row.sha256)
        throw new Error(`C2 coordinate preservation file changed: ${rootRecord.name}/${relativePath}`);
    }
    seen.add(relativePath);
    if (kind === 'directory')
      for (const name of fs.readdirSync(target).sort()) pending.push({ target: path.join(target, name), depth: depth + 1 });
  }
  if (seen.size !== expected.size) throw new Error(`C2 coordinate preservation tree gained or lost entries: ${rootRecord.name}`);
  return { name: rootRecord.name, path: rootRecord.path, entries: seen.size };
}

function readCoordinateBinding(attempt, currentSources, inputs, bindingPath) {
  const packetStat = fs.lstatSync(expectedCoordinatePacketRoot);
  const uid = process.getuid?.();
  if (!Number.isInteger(uid) || !packetStat.isDirectory() || packetStat.isSymbolicLink() || Number(packetStat.uid) !== uid || (Number(packetStat.mode) & 0o077) !== 0 || fs.realpathSync(expectedCoordinatePacketRoot) !== expectedCoordinatePacketRoot)
    throw new Error('C2 coordinate-follow-up packet is not a private canonical directory');
  const expectedBindingPath = path.join(expectedCoordinatePacketRoot, 'bindings', `${attempt}.json`);
  if (bindingPath !== expectedBindingPath) throw new Error(`C2 coordinate-follow-up ${attempt} binding path differs from its authorization`);
  const readPrivateFile = target => {
    const stat = fs.lstatSync(target);
    if (!stat.isFile() || stat.isSymbolicLink() || Number(stat.uid) !== uid || Number(stat.nlink) !== 1 || (Number(stat.mode) & 0o077) !== 0 || fs.realpathSync(target) !== target)
      throw new Error(`C2 coordinate-follow-up packet file identity is refused: ${path.basename(target)}`);
    const bytes = fs.readFileSync(target);
    const after = fs.lstatSync(target);
    if (stat.dev !== after.dev || stat.ino !== after.ino || stat.size !== after.size || bytes.length !== after.size)
      throw new Error(`C2 coordinate-follow-up packet file changed while reading: ${path.basename(target)}`);
    return { value: JSON.parse(bytes.toString('utf8')), sha256: sha256(bytes) };
  };
  const binding = readPrivateFile(bindingPath);
  const record = binding.value;
  const paths = [...allowedRepositoryPaths];
  const expectedLimits = { invocationBytes: coordinateInvocationLimitBytes, invocationEntries: 8192, depth: 32, stageBytes: coordinateStageLimitBytes, campaignBytes: followupCampaignLimitBytes, preservedCampaignBytes: 384 * 1024 * 1024 };
  if (record.schema !== 1 || record.stageRoot !== expectedCoordinateStageRoot || record.attempt !== attempt || record.baselineSHA256 !== expectedBaselineSHA256 || record.inventorySHA256 !== expectedDynamicInventorySHA256 || record.authorization !== 'parent-authorized bounded C2 coordinate correction and F1/F2 qualification')
    throw new Error(`C2 coordinate-follow-up ${attempt} binding differs from its separately authorized plan`);
  if (JSON.stringify(Object.keys(record.sourceHashes || {}).sort()) !== JSON.stringify([...paths].sort()) || paths.some(file => record.sourceHashes[file] !== currentSources[file]))
    throw new Error(`C2 coordinate-follow-up ${attempt} source hashes are not bound to the current eight source bytes`);
  if (JSON.stringify(record.limits) !== JSON.stringify(expectedLimits))
    throw new Error(`C2 coordinate-follow-up ${attempt} limits differ from the owner authorization`);
  const authorizationPath = path.join(expectedCoordinatePacketRoot, 'authorization-F.json');
  const preservationPath = path.join(expectedCoordinatePacketRoot, 'preserved-history-identities-F.json');
  const preflightPath = path.join(expectedCoordinatePacketRoot, `preflight-${attempt}.json`);
  if (record.authorizationPath !== 'authorization-F.json' || record.preservedHistoryPath !== 'preserved-history-identities-F.json' || record.preflightPath !== `preflight-${attempt}.json`)
    throw new Error(`C2 coordinate-follow-up ${attempt} binding references unsupported evidence files`);
  const authorization = readPrivateFile(authorizationPath);
  const preservation = readPrivateFile(preservationPath);
  const preflight = readPrivateFile(preflightPath);
  const expectedCoordinateRule = {
    inventorySHA256: expectedDynamicInventorySHA256,
    sourcePath: 'frontend/node_modules/playwright/lib/common/index.js',
    originalFileSHA256: '8f2588ccdb7ce2d245fdbe3222ba67817293799dff1ba2e58303f0d558b6526f',
    expressionSHA256: '6574596c5796fdd9b09b2f2e56c76ed1e30a30909e2ef953120fc88e0c86e99a',
    originalRawLine: 1109,
    inventoryTransformedLine: 1107,
    inventoryUnchanged: true,
    identityFields: ['exact-canonical-source-path', 'expression-SHA256', 'exact-occurrence-multiplicity'],
    requireWholeOriginalFileHash: true,
    coordinatesRecordedSeparately: true,
    failClosedOnSourcePathExpressionOrMultiplicityDrift: true
  };
  const priorPreservationPath = path.join(expectedFollowupPacketRoot, 'preserved-history-identities-v2.json');
  const priorPostflightPath = path.join(expectedFollowupPacketRoot, 'postflight-E2-v2.json');
  if (authorization.sha256 !== record.authorizationSHA256 || preservation.sha256 !== record.preservedHistorySHA256 || authorization.value.authorization !== record.authorization || authorization.value.authorizedRoot !== expectedCoordinateStageRoot || JSON.stringify(authorization.value.attempts) !== JSON.stringify(['F1', 'F2']) || JSON.stringify(authorization.value.limits) !== JSON.stringify(expectedLimits) || JSON.stringify(authorization.value.coordinateRule) !== JSON.stringify(expectedCoordinateRule) || authorization.value.baselineSHA256 !== expectedBaselineSHA256 || authorization.value.inventorySHA256 !== expectedDynamicInventorySHA256)
    throw new Error(`C2 coordinate-follow-up ${attempt} authorization record differs from the parent guidance`);
  if (preservation.value.createdFor !== 'herdr-fullscreen-c2-coordinate-followup-20261006' || preservation.value.priorPreservedHistorySHA256 !== sha256(fs.readFileSync(priorPreservationPath)) || preservation.value.priorE2PostflightSHA256 !== sha256(fs.readFileSync(priorPostflightPath)) || preservation.value.originalVerifierSHA256 !== '83b4f3c198f91da3e7012a1f68455742890a5c39c5cd092b052c8d7f40e04cb9' || preservation.value.d3IncidentStatus !== 'failed' || preservation.value.d3AllocatedBytes !== 69877760 || preservation.value.coordinateRoots?.length !== 2)
    throw new Error('C2 coordinate-follow-up preservation record does not bind the preserved E2 history, D3 incident, and original verifier');
  const verifiedCoordinateRoots = preservation.value.coordinateRoots.map(verifyCoordinateRootSnapshot);
  if (!verifiedCoordinateRoots.some(root => root.name === 'E1-E2-followup-stage' && root.path === expectedFollowupStageRoot) || !verifiedCoordinateRoots.some(root => root.name === 'E1-E2-followup-packet' && root.path === expectedFollowupPacketRoot))
    throw new Error('C2 coordinate-follow-up preservation record omits the prior E stage or packet');
  if (preflight.value.status !== 'passed' || preflight.value.attempt !== attempt || preflight.value.stageRoot !== expectedCoordinateStageRoot || preflight.value.bindingSHA256 !== binding.sha256 || preflight.value.authorizationSHA256 !== authorization.sha256 || preflight.value.preservedHistorySHA256 !== preservation.sha256 || preflight.value.baselineSHA256 !== expectedBaselineSHA256 || preflight.value.inventorySHA256 !== expectedDynamicInventorySHA256 || JSON.stringify(preflight.value.sourceHashes) !== JSON.stringify(currentSources) || JSON.stringify(preflight.value.coordinateRule) !== JSON.stringify(expectedCoordinateRule) || preflight.value.currentCampaignAllocatedBytes > followupCampaignLimitBytes || preflight.value.forecastInvocationAllocatedBytes > coordinateInvocationLimitBytes || preflight.value.forecastStageAllocatedBytes > coordinateStageLimitBytes || preflight.value.forecastCampaignAllocatedBytes > followupCampaignLimitBytes)
    throw new Error(`C2 coordinate-follow-up ${attempt} is not tied to a current-byte preflight within the authorized envelope`);
  if (attempt === 'F1' && record.priorAttempt !== null)
    throw new Error('C2 F1 binding must not claim a prior coordinate-follow-up invocation');
  if (attempt === 'F2') {
    const prior = record.priorAttempt;
    if (!prior || prior.attempt !== 'F1' || prior.status !== 'failed' || prior.terminalStatus !== 'failed' || !/^[0-9a-f-]{36}$/.test(prior.invocationId || '') || !/^[a-f0-9]{64}$/.test(prior.completionSHA256 || ''))
      throw new Error('C2 F2 binding does not identify a failed F1 attempt');
    const priorCompletionPath = path.join(expectedCoordinateStageRoot, `invocation-${prior.invocationId}/completion-result.json`);
    const priorCompletion = readPrivateFile(priorCompletionPath);
    if (priorCompletion.sha256 !== prior.completionSHA256 || priorCompletion.value.attempt !== 'F1' || priorCompletion.value.status !== 'failed' || priorCompletion.value.invocationId !== prior.invocationId || paths.every(file => priorCompletion.value.sourceBinding?.sourceHashes?.[file] === currentSources[file]))
      throw new Error('C2 F2 binding does not preserve a failed F1 receipt with changed source bytes');
  }
  return { path: bindingPath, sha256: binding.sha256, sourceHashes: record.sourceHashes, record };
}

function readGBinding(attempt, currentSources, inputs, bindingPath) {
  const uid = process.getuid?.();
  const packetStat = fs.lstatSync(expectedGPacketRoot);
  if (!Number.isInteger(uid) || !packetStat.isDirectory() || packetStat.isSymbolicLink() || Number(packetStat.uid) !== uid || (Number(packetStat.mode) & 0o077) !== 0 || fs.realpathSync(expectedGPacketRoot) !== expectedGPacketRoot)
    throw new Error('C2 G preparation packet is not a private canonical directory');
  if (attempt !== 'G1' || bindingPath !== path.join(expectedGPacketRoot, 'bindings/G1.json'))
    throw new Error('C2 G1 binding path or attempt differs from the single-attempt authorization');
  const readPrivateFile = target => {
    const stat = fs.lstatSync(target);
    if (!stat.isFile() || stat.isSymbolicLink() || Number(stat.uid) !== uid || Number(stat.nlink) !== 1 || (Number(stat.mode) & 0o077) !== 0 || fs.realpathSync(target) !== target)
      throw new Error(`C2 G packet file identity is refused: ${path.basename(target)}`);
    const bytes = fs.readFileSync(target);
    const after = fs.lstatSync(target);
    if (stat.dev !== after.dev || stat.ino !== after.ino || stat.size !== after.size || bytes.length !== after.size)
      throw new Error(`C2 G packet file changed while reading: ${path.basename(target)}`);
    return { bytes, value: JSON.parse(bytes.toString('utf8')), sha256: sha256(bytes) };
  };
  const binding = readPrivateFile(bindingPath);
  const record = binding.value;
  const paths = [...allowedRepositoryPaths];
  const authorization = readPrivateFile(path.join(expectedGPacketRoot, 'authorization-G.json'));
  const preservation = readPrivateFile(path.join(expectedGPacketRoot, 'preserved-history-identities-G.json'));
  const parentFindings = readPrivateFile(path.join(expectedGPacketRoot, 'parent-findings.json'));
  const regression = readPrivateFile(path.join(expectedGPacketRoot, 'harness-regressions.json'));
  const preflight = readPrivateFile(path.join(expectedGPacketRoot, 'preflight-G1.json'));
  const authorizationText = 'parent-authorized single-attempt G correction after bounded mapping, DS02 provenance, and budget regressions';
  if (record.schema !== 1 || record.stageRoot !== expectedGStageRoot || record.attempt !== attempt || record.baselineSHA256 !== expectedBaselineSHA256 || record.inventorySHA256 !== expectedDynamicInventorySHA256 || record.authorization !== authorizationText || JSON.stringify(record.limits) !== JSON.stringify(gBudgetLimits))
    throw new Error('C2 G1 source binding differs from the single-attempt authorization');
  if (JSON.stringify(Object.keys(record.sourceHashes || {}).sort()) !== JSON.stringify([...paths].sort()) || paths.some(file => record.sourceHashes[file] !== currentSources[file]))
    throw new Error('C2 G1 source hashes are not bound to the exact current eight source files');
  if (record.authorizationPath !== 'authorization-G.json' || record.preservedHistoryPath !== 'preserved-history-identities-G.json' || record.parentFindingsPath !== 'parent-findings.json' || record.regressionPath !== 'harness-regressions.json' || record.preflightPath !== 'preflight-G1.json' || record.authorizationSHA256 !== authorization.sha256 || record.preservedHistorySHA256 !== preservation.sha256 || record.parentFindingsSHA256 !== parentFindings.sha256 || record.regressionSHA256 !== regression.sha256)
    throw new Error('C2 G1 binding does not identify its authorization, preserved roots, parent finding, and regression receipt');
  if (authorization.value.authorization !== authorizationText || authorization.value.authorizedRoot !== expectedGStageRoot || JSON.stringify(authorization.value.attempts) !== JSON.stringify(['G1']) || JSON.stringify(authorization.value.allowedRepositoryPaths?.slice().sort()) !== JSON.stringify(paths.slice().sort()) || JSON.stringify(authorization.value.limits) !== JSON.stringify(gBudgetLimits) || authorization.value.baselineSHA256 !== expectedBaselineSHA256 || authorization.value.inventorySHA256 !== expectedDynamicInventorySHA256 || authorization.value.parentFindingId !== 'PARENT/F-001')
    throw new Error('C2 G authorization record differs from its one-attempt scope');
  if (regression.value.status !== 'passed' || regression.value.execution !== 'harness-only' || regression.value.regressionExecutions < 1 || regression.value.regressionExecutions > 2 || regression.value.inventorySHA256 !== expectedDynamicInventorySHA256 || JSON.stringify(regression.value.sourceHashes) !== JSON.stringify(currentSources) || regression.value.mapping?.status !== 'passed' || regression.value.ds02?.status !== 'passed' || regression.value.budget?.status !== 'passed')
    throw new Error('C2 G1 source binding is not backed by the required bounded harness regressions');
  if (parentFindings.value.findings?.length !== 1 || parentFindings.value.findings[0].id !== 'PARENT/F-001' || parentFindings.value.findings[0].status !== 'retained-open')
    throw new Error('C2 G parent finding PARENT/F-001 was not retained as open');
  const f2BindingPath = path.join(expectedCoordinatePacketRoot, 'bindings/F2.json');
  const f2CompletionPath = path.join(expectedCoordinateStageRoot, 'invocation-28e45bc0-e01a-427c-bfaf-379b94f99f2d/completion-result.json');
  const f2Binding = readPrivateFile(f2BindingPath);
  const f2Completion = readPrivateFile(f2CompletionPath);
  const f2Limits = f2Binding.value.limits;
  if (parentFindings.value.findings[0].evidence?.bindingSHA256 !== f2Binding.sha256 || parentFindings.value.findings[0].evidence?.completionSHA256 !== f2Completion.sha256 || f2Binding.value.attempt !== 'F2' || f2Completion.value.status !== 'failed' || f2Completion.value.passed !== 0 || f2Completion.value.failed !== 24 || f2Completion.value.variants !== 178 || f2Completion.value.nativeEffects !== 0 || f2Completion.value.currentC2Counters?.projectTestInvocations !== 0 || f2Limits.invocationBytes !== 32 * 1024 * 1024 || f2Completion.value.verification?.projected?.limitBytes !== 64 * 1024 * 1024)
    throw new Error('C2 G parent finding does not preserve the exact F2 receipt-limit discrepancy');
  const preservationRecord = preservation.value;
  if (preservationRecord.createdFor !== 'herdr-fullscreen-c2-dynamic-mapping-G-20261006' || preservationRecord.originalVerifierSHA256 !== '83b4f3c198f91da3e7012a1f68455742890a5c39c5cd092b052c8d7f40e04cb9' || preservationRecord.inventorySHA256 !== expectedDynamicInventorySHA256 || preservationRecord.d3IncidentStatus !== 'failed' || preservationRecord.d3AllocatedBytes !== 69877760 || !Array.isArray(preservationRecord.roots) || !Array.isArray(preservationRecord.files))
    throw new Error('C2 G preserved-history record omits the immutable verifier, inventory, or D3 failure identity');
  const verifiedRoots = preservationRecord.roots.map(verifyCoordinateRootSnapshot);
  if (!verifiedRoots.some(root => root.path === expectedCoordinateStageRoot) || !verifiedRoots.some(root => root.path === expectedCoordinatePacketRoot))
    throw new Error('C2 G preserved-history record omits the completed F1/F2 roots');
  for (const file of preservationRecord.files) {
    const stat = fs.lstatSync(file.path);
    const bytes = fs.readFileSync(file.path);
    if (!stat.isFile() || stat.isSymbolicLink() || Number(stat.nlink) !== file.nlink || Number(stat.size) !== file.bytes || Number(stat.blocks || 0) * 512 !== file.allocatedBytes || sha256(bytes) !== file.sha256)
      throw new Error(`C2 G preserved historical file changed: ${file.path}`);
  }
  if (preflight.value.status !== 'passed' || preflight.value.attempt !== attempt || preflight.value.stageRoot !== expectedGStageRoot || preflight.value.bindingSHA256 !== binding.sha256 || preflight.value.authorizationSHA256 !== authorization.sha256 || preflight.value.preservedHistorySHA256 !== preservation.sha256 || preflight.value.parentFindingsSHA256 !== parentFindings.sha256 || preflight.value.regressionSHA256 !== regression.sha256 || preflight.value.baselineSHA256 !== expectedBaselineSHA256 || preflight.value.inventorySHA256 !== expectedDynamicInventorySHA256 || JSON.stringify(preflight.value.sourceHashes) !== JSON.stringify(currentSources) || JSON.stringify(preflight.value.limits) !== JSON.stringify(gBudgetLimits) || preflight.value.forecastInvocationAllocatedBytes > gInvocationLimitBytes || preflight.value.forecastStageAllocatedBytes > gStageLimitBytes || preflight.value.forecastCampaignAllocatedBytes > gCampaignLimitBytes)
    throw new Error('C2 G1 binding is not tied to a passing exact-source preflight within all authorized limits');
  if (fs.existsSync(expectedGStageRoot)) throw new Error('C2 G1 cannot adopt or reuse a pre-existing stage root');
  try { fs.lstatSync(expectedGStageRoot); throw new Error('C2 G1 cannot use a pre-existing stage entry'); }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
  return { path: bindingPath, sha256: binding.sha256, sourceHashes: record.sourceHashes, record, preflight: preflight.value, regression: regression.value };
}

function measureFollowupCampaign(inputs, sourceHashes) {
  const manifest = inputs.repairManifest;
  const priorCloseout = path.dirname(path.dirname(manifest.pinned.closeoutPacket[0].path));
  const c3Run = path.dirname(manifest.pinned.c3b900c5[0].path);
  const ed2Run = path.dirname(path.dirname(path.dirname(manifest.pinned.ed2e47a3[0].path)));
  const priorRoots = [
    { name: 'historical-c2-stage', path: manifest.pinned.oldStage.root },
    { name: 'D1-D4-repair-stage', path: expectedDynamicStageRoot },
    { name: 'repair-and-closeout-packet', path: expectedDynamicPacket },
    { name: 'c3b900c5-history', path: c3Run },
    { name: 'ed2e47a3-history', path: ed2Run },
    { name: 'prior-closeout-packet', path: priorCloseout },
    { name: 'original-stage-plan-packet', path: '/private/tmp/fullscreen-c2-stage-plan-20261006-0004' }
  ];
  const additionalHistoryRoots = [
    { name: 'revised-stage-plan-packet', path: path.dirname(manifest.pinned.c2PlanV2.path) }
  ];
  const followupRoots = [{ name: 'followup-review-packet', path: expectedFollowupPacketRoot }, { name: 'E1-E2-followup-stage', path: expectedFollowupStageRoot }];
  const measureRoot = item => {
    const stat = fs.lstatSync(item.path);
    if (!stat.isDirectory() || stat.isSymbolicLink() || fs.realpathSync(item.path) !== item.path)
      throw new Error(`C2 campaign accounting root is not canonical: ${item.path}`);
    const measured = walkScope(item.path);
    return { name: item.name, path: item.path, allocatedBytes: measured.allocatedBytes, logicalBytes: measured.logicalBytes, entries: measured.entryCount, maxDepth: measured.maxDepth };
  };
  const prior = priorRoots.map(measureRoot);
  const additionalHistory = additionalHistoryRoots.map(measureRoot);
  const followup = followupRoots.map(measureRoot);
  const sourceFiles = [...allowedRepositoryPaths].map(file => {
    const target = path.join(repositoryRoot, file);
    const stat = fs.lstatSync(target);
    if (!stat.isFile() || stat.isSymbolicLink() || sha256(fs.readFileSync(target)) !== sourceHashes[file])
      throw new Error(`C2 campaign source allocation differs from its bound hash: ${file}`);
    return { path: file, sha256: sourceHashes[file], allocatedBytes: Number(stat.blocks || 0) * 512, logicalBytes: Number(stat.size), entries: 1 };
  });
  const preservation = JSON.parse(fs.readFileSync(path.join(expectedFollowupPacketRoot, 'preserved-history-identities-v2.json'), 'utf8'));
  const additionalHistoryFiles = preservation.c184ReviewEvidence.map(item => {
    const stat = fs.lstatSync(item.path);
    if (!stat.isFile() || stat.isSymbolicLink() || Number(stat.nlink) !== 1 || Number(stat.size) !== item.bytes || Number(stat.blocks || 0) * 512 !== item.allocatedBytes || sha256(fs.readFileSync(item.path)) !== item.sha256)
      throw new Error(`C2 prior reviewer evidence changed: ${item.path}`);
    return { name: 'c184-review-evidence', path: item.path, sha256: item.sha256, allocatedBytes: Number(stat.blocks || 0) * 512, logicalBytes: Number(stat.size), entries: 1, maxDepth: 0 };
  });
  const sum = values => values.reduce((total, item) => ({ allocatedBytes: total.allocatedBytes + item.allocatedBytes, logicalBytes: total.logicalBytes + item.logicalBytes, entries: total.entries + item.entries, maxDepth: Math.max(total.maxDepth, item.maxDepth) }), { allocatedBytes: 0, logicalBytes: 0, entries: 0, maxDepth: 0 });
  const priorTotal = sum([...prior, ...sourceFiles]);
  const total = sum([...prior, ...additionalHistory, ...additionalHistoryFiles, ...followup, ...sourceFiles]);
  return { priorRoots: prior, additionalHistoryRoots: additionalHistory, additionalHistoryFiles, followupRoots: followup, sourceFiles, prior: priorTotal, total };
}

function measureCoordinateCampaign(inputs, sourceHashes) {
  const existing = measureFollowupCampaign(inputs, sourceHashes);
  const measureRoot = item => {
    const stat = fs.lstatSync(item.path);
    if (!stat.isDirectory() || stat.isSymbolicLink() || fs.realpathSync(item.path) !== item.path)
      throw new Error(`C2 coordinate campaign accounting root is not canonical: ${item.path}`);
    const measured = walkScope(item.path);
    return { name: item.name, path: item.path, allocatedBytes: measured.allocatedBytes, logicalBytes: measured.logicalBytes, entries: measured.entryCount, maxDepth: measured.maxDepth };
  };
  const coordinateRoots = [
    { name: 'coordinate-review-packet', path: expectedCoordinatePacketRoot },
    { name: 'F1-F2-coordinate-stage', path: expectedCoordinateStageRoot }
  ].map(measureRoot);
  const sum = values => values.reduce((total, item) => ({ allocatedBytes: total.allocatedBytes + item.allocatedBytes, logicalBytes: total.logicalBytes + item.logicalBytes, entries: total.entries + item.entries, maxDepth: Math.max(total.maxDepth, item.maxDepth) }), { allocatedBytes: 0, logicalBytes: 0, entries: 0, maxDepth: 0 });
  return { ...existing, coordinateRoots, total: sum([existing.total, ...coordinateRoots]) };
}

function measureGCampaign(inputs, sourceHashes) {
  const existing = measureCoordinateCampaign(inputs, sourceHashes);
  const roots = [{ name: 'G-preparation-packet', path: expectedGPacketRoot }];
  try {
    fs.lstatSync(expectedGStageRoot);
    roots.push({ name: 'G1-correction-stage', path: expectedGStageRoot });
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
  const measuredRoots = roots.map(item => {
    const stat = fs.lstatSync(item.path);
    if (!stat.isDirectory() || stat.isSymbolicLink() || fs.realpathSync(item.path) !== item.path)
      throw new Error(`C2 G campaign accounting root is not canonical: ${item.path}`);
    const measured = walkScope(item.path);
    return { name: item.name, path: item.path, allocatedBytes: measured.allocatedBytes, logicalBytes: measured.logicalBytes, entryCount: measured.entryCount, maxDepth: measured.maxDepth };
  });
  const total = [existing.total, ...measuredRoots].reduce((sum, item) => ({ allocatedBytes: sum.allocatedBytes + item.allocatedBytes, logicalBytes: sum.logicalBytes + item.logicalBytes, entryCount: sum.entryCount + (item.entryCount ?? item.entries), maxDepth: Math.max(sum.maxDepth, item.maxDepth) }), { allocatedBytes: 0, logicalBytes: 0, entryCount: 0, maxDepth: 0 });
  return { ...existing, gRoots: measuredRoots, total };
}

function repairStage(stageRoot, attemptPrefix = 'D', attemptLimit = 4) {
  const tools = requireFromHere('./c2-paths.cjs');
  let created = false;
  if (!fs.existsSync(stageRoot)) {
    fs.mkdirSync(stageRoot, { mode: 0o700 });
    created = true;
  }
  const stat = fs.lstatSync(stageRoot);
  const uid = process.getuid?.();
  if (!Number.isInteger(uid) || !stat.isDirectory() || stat.isSymbolicLink() || Number(stat.uid) !== uid || (Number(stat.mode) & 0o077) !== 0 || fs.realpathSync(stageRoot) !== stageRoot)
    throw new Error('C2 dynamic repair stage root is not a private, canonical directory');
  const stage = { path: stageRoot, parent: path.dirname(stageRoot), parentAncestry: tools.pinExistingAncestry(path.dirname(stageRoot)), identity: tools.identity(stat), uid };
  tools.verifyAncestry(stage.parentAncestry);
  const journalPath = path.join(stageRoot, 'journal.ndjson');
  if (created) {
    const fd = fs.openSync(journalPath, fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL | (fs.constants.O_NOFOLLOW || 0), 0o600);
    fs.fsyncSync(fd);
    fs.closeSync(fd);
  } else if (!fs.existsSync(journalPath)) {
    throw new Error('Existing unregistered C2 dynamic repair root cannot be adopted');
  }
  stage.journal = { path: journalPath, identity: tools.identity(fs.lstatSync(journalPath)) };
  if (!stage.journal.identity.file || stage.journal.identity.symlink || stage.journal.identity.uid !== uid || stage.journal.identity.nlink !== 1 || (stage.journal.identity.mode & 0o077) !== 0 || fs.realpathSync(journalPath) !== journalPath)
    throw new Error('C2 dynamic repair journal identity refused');
  const rows = readJournal(stage);
  const begins = rows.filter(row => row.type === 'BEGIN');
  const terminals = rows.filter(row => row.type === 'TERMINAL');
  if (rows.length !== begins.length + terminals.length || begins.length > attemptLimit || terminals.length !== begins.length || new Set(begins.map(row => row.invocationId)).size !== begins.length || new Set(terminals.map(row => row.invocationId)).size !== terminals.length)
    throw new Error('C2 dynamic repair journal has an incomplete or unsupported attempt history');
  const terminalById = new Map(terminals.map(row => [row.invocationId, row]));
  for (const [index, begin] of begins.entries()) {
    const terminal = terminalById.get(begin.invocationId);
    if (begin.attempt !== `${attemptPrefix}${index + 1}` || begin.phase !== 'worker' || terminal?.attempt !== begin.attempt || terminal.phase !== 'worker' || !['passed', 'failed'].includes(terminal.status) || rows.indexOf(terminal) <= rows.indexOf(begin))
      throw new Error(`C2 dynamic repair attempt sequence is invalid at ${attemptPrefix}${index + 1}`);
    if (index + 1 < begins.length && rows.indexOf(terminal) >= rows.indexOf(begins[index + 1]))
      throw new Error(`C2 dynamic repair attempts overlap at ${attemptPrefix}${index + 1}`);
  }
  const expected = new Set(['journal.ndjson', ...begins.map(row => `invocation-${row.invocationId}`)]);
  for (const name of fs.readdirSync(stageRoot)) {
    const entry = fs.lstatSync(path.join(stageRoot, name));
    if (name === 'final-evidence' || name === 'final-evidence-ndjson') {
      if (!entry.isDirectory() || entry.isSymbolicLink()) throw new Error(`C2 repair publication directory is invalid: ${name}`);
      continue;
    }
    if (expected.has(name) && ((name === 'journal.ndjson' && entry.isFile()) || (name.startsWith('invocation-') && entry.isDirectory())) && !entry.isSymbolicLink()) continue;
    throw new Error(`C2 dynamic repair stage contains an unregistered path: ${name}`);
  }
  if ([...terminalById.values()].some(row => !begins.some(begin => begin.invocationId === row.invocationId)))
    throw new Error('C2 dynamic repair journal has an unbound terminal record');
  return { stage, rows, begins, terminals, tools };
}

function appendRepairJournal(stage, row) {
  const tools = requireFromHere('./c2-paths.cjs');
  tools.verifyAncestry(stage.parentAncestry);
  tools.checkedStat(stage.path, stage.uid, 'directory', stage.identity);
  const target = stage.journal.path;
  const before = tools.checkedStat(target, stage.uid, 'file', stage.journal.identity);
  const fd = fs.openSync(target, fs.constants.O_WRONLY | fs.constants.O_APPEND | (fs.constants.O_NOFOLLOW || 0));
  try {
    if (!tools.sameObject(before, tools.identity(fs.fstatSync(fd)))) throw new Error('C2 repair journal descriptor changed identity');
    const bytes = Buffer.from(JSON.stringify(row) + '\n');
    let offset = 0;
    while (offset < bytes.length) {
      const written = fs.writeSync(fd, bytes, offset, bytes.length - offset);
      if (!written) throw new Error('C2 repair journal append made no progress');
      offset += written;
    }
    fs.fsyncSync(fd);
    stage.journal.identity = tools.identity(fs.fstatSync(fd));
    tools.checkedStat(target, stage.uid, 'file', stage.journal.identity);
  } finally {
    fs.closeSync(fd);
  }
}

function atomicRepairWrite(target, content, uid) {
  const bytes = Buffer.isBuffer(content) ? content : Buffer.from(content);
  const parent = path.dirname(target);
  const parentStat = fs.lstatSync(parent);
  if (!parentStat.isDirectory() || parentStat.isSymbolicLink() || Number(parentStat.uid) !== uid || fs.realpathSync(parent) !== parent)
    throw new Error(`C2 repair publication parent refused: ${parent}`);
  const temporary = path.join(parent, `.${path.basename(target)}.tmp-${crypto.randomUUID()}`);
  const fd = fs.openSync(temporary, fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL | (fs.constants.O_NOFOLLOW || 0), 0o600);
  try {
    let offset = 0;
    while (offset < bytes.length) {
      const written = fs.writeSync(fd, bytes, offset, bytes.length - offset);
      if (!written) throw new Error(`C2 repair publication made no progress: ${target}`);
      offset += written;
    }
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }
  fs.renameSync(temporary, target);
  const directory = fs.openSync(parent, fs.constants.O_RDONLY);
  try { fs.fsyncSync(directory); } finally { fs.closeSync(directory); }
}

function publishRepairJournal(stage) {
  const directory = path.join(stage.path, 'final-evidence-ndjson');
  if (!fs.existsSync(directory)) fs.mkdirSync(directory, { mode: 0o700 });
  atomicRepairWrite(path.join(directory, 'stage-journal.ndjson'), fs.readFileSync(stage.journal.path), stage.uid);
}

function publishRepairPass(stage, invocationRoot, sourceHashes) {
  const jsonNames = ['attempt-summary.json', 'completion-result.json', 'invocation.json', 'source-manifest.json', 'dynamic-sites.json'];
  const jsonDirectory = path.join(stage.path, 'final-evidence');
  const ndjsonDirectory = path.join(stage.path, 'final-evidence-ndjson');
  if (!fs.existsSync(jsonDirectory)) fs.mkdirSync(jsonDirectory, { mode: 0o700 });
  if (!fs.existsSync(ndjsonDirectory)) fs.mkdirSync(ndjsonDirectory, { mode: 0o700 });
  const files = {};
  for (const name of jsonNames) {
    const bytes = fs.readFileSync(path.join(invocationRoot, name));
    atomicRepairWrite(path.join(jsonDirectory, name), bytes, stage.uid);
    files[name] = sha256(bytes);
  }
  const tests = fs.readFileSync(path.join(invocationRoot, 'tests.ndjson'));
  atomicRepairWrite(path.join(ndjsonDirectory, 'tests.ndjson'), tests, stage.uid);
  files['tests.ndjson'] = sha256(tests);
  const publication = { schema: 1, invocationId: path.basename(invocationRoot).slice('invocation-'.length), attempt: JSON.parse(fs.readFileSync(path.join(invocationRoot, 'attempt-summary.json'), 'utf8')).attempt, files, sourceFiles: sourceHashes };
  atomicRepairWrite(path.join(jsonDirectory, 'publication.json'), JSON.stringify(publication, null, 2) + '\n', stage.uid);
}

async function runDynamicRepair(values) {
  const stageRoot = values['--stage-root'];
  const baselinePath = values['--baseline'];
  const inventoryPath = values['--inventory'];
  const followup = stageRoot === expectedFollowupStageRoot;
  const coordinateFollowup = stageRoot === expectedCoordinateStageRoot;
  const gFollowup = stageRoot === expectedGStageRoot;
  const separatelyBound = followup || coordinateFollowup || gFollowup;
  if ((!separatelyBound && stageRoot !== expectedDynamicStageRoot) || baselinePath !== '/private/tmp/fullscreen-c2-stage-plan-20261006-0004/baseline.json' || inventoryPath !== path.join(expectedDynamicPacket, 'evidence/dynamic-site-inventory.json') || separatelyBound !== (typeof values['--binding'] === 'string'))
    throw new Error('C2 dynamic repair command differs from the pinned rooted invocation');
  const inputs = readDynamicRepairInputs(baselinePath, inventoryPath);
  verifyBaseline(inputs.baseline);
  const currentSources = Object.fromEntries([...allowedRepositoryPaths].map(file => [file, sha256(fs.readFileSync(path.join(repositoryRoot, file)))]));
  const attemptPrefix = gFollowup ? 'G' : coordinateFollowup ? 'F' : followup ? 'E' : 'D';
  const attemptLimit = gFollowup ? 1 : separatelyBound ? 2 : 4;
  const historicalAllocated = inputs.repairManifest.pinned.oldStage.allocatedBytes;
  let projected;
  let attempt;
  let sourceBinding;
  let followupCampaign;
  let before;
  let stage;
  let begins;
  let terminals;
  let tools;
  if (gFollowup) {
    attempt = 'G1';
    sourceBinding = readGBinding(attempt, currentSources, inputs, values['--binding']);
    projected = estimateScope(inputs.baseline, inputs.baseline.inputs.filter(item => item.path.startsWith('frontend/')), gInvocationEnvelope);
    const reserveBytes = projected.allocatedBytes + 2 * 1024 * 1024;
    assertC2BudgetEnvelope({ allocatedBytes: reserveBytes, entries: projected.entries, maxDepth: projected.maxDepth }, gInvocationEnvelope, 'G1 preflight invocation forecast');
    assertC2BudgetEnvelope({ allocatedBytes: reserveBytes, entries: projected.entries + 2, maxDepth: projected.maxDepth + 1 }, gStageEnvelope, 'G1 preflight stage forecast');
    followupCampaign = measureGCampaign(inputs, currentSources);
    if (sourceBinding.preflight.currentCampaignAllocatedBytes !== followupCampaign.total.allocatedBytes || sourceBinding.preflight.forecastInvocationAllocatedBytes !== reserveBytes || sourceBinding.preflight.forecastStageAllocatedBytes !== reserveBytes || sourceBinding.preflight.forecastCampaignAllocatedBytes !== followupCampaign.total.allocatedBytes + reserveBytes)
      throw new Error('C2 G1 preflight forecast differs from the current preserved-root and output measurement');
    assertC2BudgetEnvelope({ allocatedBytes: followupCampaign.total.allocatedBytes + reserveBytes }, gCampaignEnvelope, 'G1 preflight campaign forecast');
    ({ stage, begins, terminals, tools } = repairStage(stageRoot, attemptPrefix, attemptLimit));
    if (begins.length !== 0 || terminals.length !== 0) throw new Error('C2 G1 requires an empty one-attempt stage');
    before = walkScope(stageRoot);
    assertC2BudgetEnvelope(before, gStageEnvelope, 'G1 stage before invocation');
    followupCampaign = measureGCampaign(inputs, currentSources);
    assertC2BudgetEnvelope({ allocatedBytes: followupCampaign.total.allocatedBytes + reserveBytes }, gCampaignEnvelope, 'G1 campaign before invocation');
  } else {
    ({ stage, begins, terminals, tools } = repairStage(stageRoot, attemptPrefix, attemptLimit));
    if (begins.length >= attemptLimit) throw new Error(`C2 dynamic repair ${attemptPrefix}1-${attemptPrefix}${attemptLimit} attempt cap is exhausted`);
    if (terminals.at(-1)?.status === 'passed') throw new Error('A passing C2 dynamic repair stage cannot be rerun');
    if (terminals.length && terminals.at(-1).status === 'failed') {
      const previous = begins.at(-1).sourceHashes || {};
      if (![...allowedRepositoryPaths].some(file => previous[file] !== currentSources[file]))
        throw new Error(`C2 dynamic repair ${begins.at(-1).attempt} failed and no source changed to address it`);
    }
    projected = estimateScope(inputs.baseline, inputs.baseline.inputs.filter(item => item.path.startsWith('frontend/')));
    before = walkScope(stageRoot);
    attempt = `${attemptPrefix}${begins.length + 1}`;
    sourceBinding = followup
      ? readFollowupBinding(attempt, currentSources, inputs, values['--binding'])
      : coordinateFollowup
        ? readCoordinateBinding(attempt, currentSources, inputs, values['--binding'])
        : null;
    if (separatelyBound) {
      followupCampaign = coordinateFollowup ? measureCoordinateCampaign(inputs, currentSources) : measureFollowupCampaign(inputs, currentSources);
      const oldCampaignLimit = 384 * 1024 * 1024;
      if (followupCampaign.prior.allocatedBytes > oldCampaignLimit || followupCampaign.prior.logicalBytes > oldCampaignLimit)
        throw new Error('Preserved qualification and closeout evidence exceeds the unchanged 384 MiB ceiling');
      const projectedReserveBytes = (coordinateFollowup ? coordinateAttemptForecastBytes : projected.allocatedBytes) + 2 * 1024 * 1024;
      const stageLimitBytes = coordinateFollowup ? coordinateStageLimitBytes : followupStageLimitBytes;
      if (coordinateFollowup && coordinateAttemptForecastBytes > coordinateInvocationLimitBytes)
        throw new Error('C2 coordinate-follow-up forecast exceeds the 32 MiB invocation allocation limit');
      if (before.allocatedBytes + projectedReserveBytes > stageLimitBytes || before.maxDepth + 1 > 32 || projected.entries > 8192 || projected.maxDepth + 1 > 32)
        throw new Error(`C2 ${attemptPrefix}-stage cannot fit the bound output forecast within its authorized retained-output envelope`);
      if (followupCampaign.total.allocatedBytes + projectedReserveBytes > followupCampaignLimitBytes || followupCampaign.total.logicalBytes + (coordinateFollowup ? coordinateAttemptForecastBytes : projected.logicalBytes) + 2 * 1024 * 1024 > followupCampaignLimitBytes)
        throw new Error(`C2 ${attemptPrefix}-stage cannot fit the bound output forecast within the 512 MiB combined evidence envelope`);
    } else if (before.allocatedBytes + projected.allocatedBytes + historicalAllocated > 384 * 1024 * 1024 || before.entryCount + projected.entries > 8192 || Math.max(before.maxDepth, projected.maxDepth + 1) > 32) {
      throw new Error('C2 dynamic repair cannot fit within the cumulative retained-output envelope');
    }
  }
  const invocationId = crypto.randomUUID();
  const invocationRoot = path.join(stageRoot, `invocation-${invocationId}`);
  const startedAt = new Date().toISOString();
  appendRepairJournal(stage, { type: 'BEGIN', attempt, phase: 'worker', invocationId, invocationRoot, startedAt, baselineSHA256: expectedBaselineSHA256, inventorySHA256: expectedDynamicInventorySHA256, sourceHashes: currentSources, ...(sourceBinding ? { sourceBinding: { path: sourceBinding.path, sha256: sourceBinding.sha256 } } : {}) });
  const paths = tools.createInvocation(stage, invocationRoot, invocationId);
  const packageInput = inputs.baseline.inputs.find(item => item.path === 'frontend/package.json');
  const configInput = inputs.baseline.inputs.find(item => item.path === 'frontend/playwright.config.ts');
  const descriptor = {
    schema: 1,
    invocationId,
    root: invocationRoot,
    stageRoot,
    routeManifestPath: path.join(invocationRoot, 'control/route-map.json'),
    uid: paths.uid,
    nativeReady: false,
    configPath: path.join(invocationRoot, 'runtime/project/frontend/playwright.config.ts'),
    configSHA256: configInput.sha256,
    packageSHA256: packageInput.sha256,
    webRoot: path.resolve(repositoryRoot, 'frontend/dist'),
    runtimeRoot: path.join(invocationRoot, 'runtime'),
    internalCacheDir: path.join(invocationRoot, 'transform-cache'),
    deadline: Number.MAX_SAFE_INTEGER,
    ...(sourceBinding ? { qualificationSourceBinding: { path: sourceBinding.path, sha256: sourceBinding.sha256, sourceHashes: sourceBinding.sourceHashes } } : {})
  };
  const descriptorPath = path.join(invocationRoot, 'invocation.json');
  paths.acquireFile('completion-result.json', 'controller', '');
  paths.acquireFile('attempt-summary.json', 'controller', '');
  const envKeys = ['HERDR_C2_INVOCATION', 'HERDR_C2_INVOCATION_ROOT', 'HERDR_C2_NATIVE_READY', 'HERDR_WEB_ROOT', 'PWTEST_CACHE_DIR'];
  const envBefore = Object.fromEntries(envKeys.map(key => [key, process.env[key]]));
  let result;
  let failure;
  let route;
  let generatedResult;
  let testRows = [];
  let testsBytes = Buffer.from('');
  let dynamicSitesBytes = Buffer.from('');
  let sourceManifestBytes = Buffer.from('');
  const verification = { projected, historicalAllocatedBytes: historicalAllocated, priorAttempts: begins.length, ...(sourceBinding ? { sourceBinding: { path: sourceBinding.path, sha256: sourceBinding.sha256, sourceHashes: sourceBinding.sourceHashes }, priorCampaign: followupCampaign, ...(coordinateFollowup ? { coordinateAttemptForecastBytes } : {}), ...(gFollowup ? { gPreflight: { path: path.join(expectedGPacketRoot, 'preflight-G1.json'), sha256: sourceBinding.preflight ? sha256(fs.readFileSync(path.join(expectedGPacketRoot, 'preflight-G1.json'))) : null, regressionSHA256: sourceBinding.record.regressionSHA256, limits: gBudgetLimits } } : {}) } : {}) };
  try {
    const wrapper = sourceBoundWrapper(inputs.baseline);
    verification.baselineBefore = verifyBaseline(inputs.baseline);
    process.env.HERDR_C2_INVOCATION = descriptorPath;
    process.env.HERDR_C2_INVOCATION_ROOT = invocationRoot;
    process.env.HERDR_WEB_ROOT = descriptor.webRoot;
    process.env.PWTEST_CACHE_DIR = descriptor.internalCacheDir;
    delete process.env.HERDR_C2_NATIVE_READY;
    const routeModule = requireFromHere('./c2-route.cjs');
    route = routeModule.createPrivateRoute(paths, descriptor);
    generatedResult = generateC2({ baselinePath, descriptor, invocationRoot, runtimeRoot: descriptor.runtimeRoot, route, dynamicInventory: inputs.inventory, dynamicInventorySHA256: inputs.inventorySHA256 });
    paths.acquireFile('invocation.json', 'controller', JSON.stringify(descriptor, null, 2) + '\n');
    const reconstructed = applyC2Edits({ sources: generatedResult.original, releaseFixture: generatedResult.projectSources['frontend/tests/browser/release-resources.spec.ts'], dynamicSites: generatedResult.dynamicEditSites });
    for (const [file, content] of Object.entries(generatedResult.generated))
      if (reconstructed.generated[file] !== content) throw new Error(`C2 exact dynamic source patch reconstruction differs: ${file}`);
    verification.patchReconstruction = { exact: true, editCount: reconstructed.edits.length, files: Object.keys(reconstructed.generated).length, vendorAddedLines: generatedResult.manifest.vendorCounts.added, vendorDeletedLines: generatedResult.manifest.vendorCounts.deleted, supportLines: generatedResult.manifest.supportCounts };
    const bootstrapPath = path.join(descriptor.runtimeRoot, 'project/frontend/private/c2-bootstrap.cjs');
    const supportPath = path.join(descriptor.runtimeRoot, 'project/frontend/private/privateCompletion.cjs');
    const generatedRequire = createRequire(bootstrapPath);
    const bootstrap = generatedRequire(bootstrapPath);
    const support = generatedRequire(supportPath);
    const generatedRouteModule = generatedRequire('./c2-route.cjs');
    const runtime = bootstrap.installSourceRuntime({ nativeEffectsDisabled: true, paths, descriptor, route });
    if (runtime.completion !== support || bootstrap.getPrivateCompletion() !== support)
      throw new Error('Generated C2 bootstrap and completion support do not share the private module instance');
    verification.privateBootstrap = { path: bootstrapPath, sha256: sha256(fs.readFileSync(bootstrapPath)), supportPath, supportSHA256: sha256(fs.readFileSync(supportPath)), dynamicManifestPath: descriptor.dynamicManifestPath, dynamicManifestSHA256: descriptor.dynamicManifestSHA256, configuredOnce: true, nativeReady: false };
    verification.sourceParse = syntaxAndTranspile(generatedResult, descriptor.runtimeRoot);
    const casesRootActor = 'test-case-root';
    paths.registerActor(casesRootActor, paths.root);
    const casesRoot = paths.acquireSharedDirectory('test-cases', casesRootActor).path;
    const wrapperSourceInput = fs.readFileSync(path.join(repositoryRoot, 'frontend/scripts/run-browser-tests.sh'), 'utf8');
    const context = {
      paths, pathTools: tools, casesRoot, childScopes: [], knownNegativePaths: [], descriptor, bootstrap, support, route,
      routeFactory: generatedRouteModule.createPrivateRoute, routeHelpers: generatedRouteModule,
      manifest: generatedResult.manifest, generated: generatedResult.generated, projectSources: generatedResult.projectSources,
      originalReleaseFixture: generatedResult.projectSources['frontend/tests/browser/release-resources.spec.ts'],
      runtimeRoot: descriptor.runtimeRoot, wrapperSource: wrapperSourceInput, wrapperBaseline: wrapper.stock,
      priorInvocationIds: begins.map(row => row.invocationId), nativeEffectsDisabled: true, dynamicSites: generatedResult.dynamicSites
    };
    const sourceCases = await runC2Tests(context);
    testRows = sourceCases.cases;
    const dynamicCases = await runC2DynamicTests(context);
    testRows = [...testRows, ...dynamicCases.cases];
    const variantIds = testRows.flatMap(item => item.variants);
    if (testRows.length !== 24 || testRows.some((item, index) => item.id !== `C2-${String(index + 1).padStart(2, '0')}`) || new Set(variantIds).size !== variantIds.length)
      throw new Error('C2 repair case order, count, or unique variant contract differs');
    const totalVariants = variantIds.length;
    const planVariants = repairVariantPlan(inputs.inventory).flatMap(item => item.variants);
    if (totalVariants !== planVariants.length || planVariants.some(variant => !variantIds.includes(variant)))
      throw new Error('C2 repair dropped or changed a required historical or dynamic variant');
    const dynamicReceipt = { schema: 1, inventorySHA256: inputs.inventorySHA256, sites: generatedResult.dynamicSites };
    dynamicSitesBytes = Buffer.from(JSON.stringify(dynamicReceipt, null, 2) + '\n');
    sourceManifestBytes = fs.readFileSync(generatedResult.manifestPath);
    testsBytes = Buffer.from(testRows.map(row => JSON.stringify(row)).join('\n') + '\n');
    route.writeRetainedFile(path.join(invocationRoot, 'dynamic-sites.json'), dynamicSitesBytes, 'c2-dynamic-sites');
    route.writeRetainedFile(path.join(invocationRoot, 'tests.ndjson'), testsBytes, 'c2-tests-results');
    verification.caseTotals = { cases: 24, passed: testRows.filter(item => item.status === 'passed').length, failed: testRows.filter(item => item.status !== 'passed').length, variants: totalVariants };
    verification.outputEnvelopeBeforeReceipts = validateRegisteredOutputs(paths, context.childScopes, context.knownNegativePaths);
    verification.baselineAfter = verifyBaseline(inputs.baseline);
    const sourceFiles = [...allowedRepositoryPaths].map(file => ({ path: file, hash: sha256(fs.readFileSync(path.join(repositoryRoot, file))) }));
    if (JSON.stringify(Object.fromEntries(sourceFiles.map(item => [item.path, item.hash]))) !== JSON.stringify(currentSources))
      throw new Error('C2 source files changed during the repair invocation');
    const testsSHA256 = sha256(testsBytes);
    const sourceManifestSHA256 = sha256(sourceManifestBytes);
    const dynamicSitesSHA256 = sha256(dynamicSitesBytes);
    const passed = testRows.every(item => item.status === 'passed') && verification.caseTotals.failed === 0;
    result = {
      schema: 1, phase: 'worker', attempt, status: passed ? 'passed' : 'failed', invocationId, invocationRoot, stageRoot,
      baselineSHA256: expectedBaselineSHA256, inventorySHA256: expectedDynamicInventorySHA256,
      ...(sourceBinding ? { sourceBinding: { path: sourceBinding.path, sha256: sourceBinding.sha256, sourceHashes: sourceBinding.sourceHashes } } : {}),
      startedAt, completedAt: new Date().toISOString(), cases: 24, passed: verification.caseTotals.passed, failed: verification.caseTotals.failed,
      variants: totalVariants, nativeEffects: 0, historicalC1: { ...generatedResult.manifest.c1, reusedUnchanged: true },
      currentC2Counters: { projectTestInvocations: 0, dynamicSourceGateInvocations: begins.length + 1 },
      accounting: { ordinaryRepositoryFiles: sourceFiles, projected, outputEnvelope: verification.outputEnvelopeBeforeReceipts, ...(gFollowup ? { effectiveBudget: { basis: 'allocatedBytes', invocation: gInvocationEnvelope, stage: gStageEnvelope, campaign: gCampaignEnvelope, sourceBindingSHA256: sourceBinding.sha256, preflightSHA256: verification.gPreflight.sha256 } } : {}) },
      testsSHA256, sourceManifestSHA256, dynamicSitesSHA256,
      verification, futureAcceptance: ['C3', 'C4', 'C5', 'P1', 'P2', 'P3', 'native-loaded closure', 'real browser/Pi/Herdr/live qualification']
    };
  } catch (error) {
    failure = error;
    const variants = repairVariantPlan(inputs.inventory);
    const recorded = new Set(testRows.map(item => item.id));
    for (const item of variants)
      if (!recorded.has(item.id)) testRows.push({ id: item.id, variants: item.variants, status: 'failed', nativeEffects: 0, error: String(error.stack || error) });
    testRows.sort((left, right) => Number(left.id.slice(3)) - Number(right.id.slice(3)));
    if (!dynamicSitesBytes.length && generatedResult?.dynamicSites) dynamicSitesBytes = Buffer.from(JSON.stringify({ schema: 1, inventorySHA256: inputs.inventorySHA256, sites: generatedResult.dynamicSites }, null, 2) + '\n');
    if (!sourceManifestBytes.length && generatedResult?.manifestPath && fs.existsSync(generatedResult.manifestPath)) sourceManifestBytes = fs.readFileSync(generatedResult.manifestPath);
    if (!testsBytes.length) testsBytes = Buffer.from(testRows.map(row => JSON.stringify(row)).join('\n') + '\n');
    try {
      if (dynamicSitesBytes.length && !paths.objects.has(path.join(invocationRoot, 'dynamic-sites.json')))
        route?.writeRetainedFile(path.join(invocationRoot, 'dynamic-sites.json'), dynamicSitesBytes, 'c2-dynamic-sites');
    } catch (writeError) {
      failure = writeError;
    }
    try {
      if (testsBytes.length && !paths.objects.has(path.join(invocationRoot, 'tests.ndjson')))
        route?.writeRetainedFile(path.join(invocationRoot, 'tests.ndjson'), testsBytes, 'c2-tests-results');
    } catch (writeError) {
      failure = writeError;
    }
    result = {
      schema: 1, phase: 'worker', attempt, status: 'failed', invocationId, invocationRoot, stageRoot,
      baselineSHA256: expectedBaselineSHA256, inventorySHA256: expectedDynamicInventorySHA256,
      ...(sourceBinding ? { sourceBinding: { path: sourceBinding.path, sha256: sourceBinding.sha256, sourceHashes: sourceBinding.sourceHashes } } : {}),
      startedAt, completedAt: new Date().toISOString(), cases: 24,
      passed: testRows.filter(item => item.status === 'passed').length,
      failed: testRows.filter(item => item.status !== 'passed').length,
      variants: testRows.flatMap(item => item.variants).length, nativeEffects: route?.nativeEffectCount?.() ?? 0,
      historicalC1: generatedResult?.manifest?.c1 ? { ...generatedResult.manifest.c1, reusedUnchanged: true } : { reusedUnchanged: true },
      currentC2Counters: { projectTestInvocations: 0, dynamicSourceGateInvocations: begins.length + 1 },
      accounting: { ordinaryRepositoryFiles: [...allowedRepositoryPaths].map(file => ({ path: file, hash: sha256(fs.readFileSync(path.join(repositoryRoot, file))) })), ...(gFollowup ? { effectiveBudget: { basis: 'allocatedBytes', invocation: gInvocationEnvelope, stage: gStageEnvelope, campaign: gCampaignEnvelope, sourceBindingSHA256: sourceBinding.sha256, preflightSHA256: verification.gPreflight.sha256 } } : {}) },
      testsSHA256: sha256(testsBytes), sourceManifestSHA256: sourceManifestBytes.length ? sha256(sourceManifestBytes) : null,
      dynamicSitesSHA256: dynamicSitesBytes.length ? sha256(dynamicSitesBytes) : null,
      error: String(error.stack || error), verification
    };
  } finally {
    maybeRestoreEnv(envBefore);
  }
  if (result.status === 'passed' && (!sourceManifestBytes.length || !dynamicSitesBytes.length || !testsBytes.length)) {
    failure ||= new Error('Passing C2 repair attempt is missing a required byte-bound receipt');
    result.status = 'failed';
  }
  const testsPath = path.join(invocationRoot, 'tests.ndjson');
  const dynamicPath = path.join(invocationRoot, 'dynamic-sites.json');
  if (testsBytes.length && !paths.objects.has(testsPath)) {
    try { route.writeRetainedFile(testsPath, testsBytes, 'c2-tests-results'); } catch (error) { failure ||= error; result.status = 'failed'; }
  }
  if (dynamicSitesBytes.length && !paths.objects.has(dynamicPath)) {
    try { route.writeRetainedFile(dynamicPath, dynamicSitesBytes, 'c2-dynamic-sites'); } catch (error) { failure ||= error; result.status = 'failed'; }
  }
  result.testsSHA256 = testsBytes.length ? sha256(testsBytes) : null;
  result.sourceManifestSHA256 = sourceManifestBytes.length ? sha256(sourceManifestBytes) : null;
  result.dynamicSitesSHA256 = dynamicSitesBytes.length ? sha256(dynamicSitesBytes) : null;
  const completionPath = path.join(invocationRoot, 'completion-result.json');
  const attemptSummaryPath = path.join(invocationRoot, 'attempt-summary.json');
  const summaryFor = () => tools.attemptSummary(result);
  paths.continueFile(completionPath, 'controller', JSON.stringify(result, null, 2) + '\n');
  paths.continueFile(attemptSummaryPath, 'controller', JSON.stringify(summaryFor(), null, 2) + '\n');
  let invocationMeasure = walkScope(invocationRoot);
  let stageMeasure = walkScope(stageRoot);
  let campaignMeasure = gFollowup
    ? measureGCampaign(inputs, currentSources)
    : followup
      ? measureFollowupCampaign(inputs, currentSources)
      : coordinateFollowup
        ? measureCoordinateCampaign(inputs, currentSources)
        : null;
  const measureSummary = measured => ({ logicalBytes: measured.logicalBytes, allocatedBytes: measured.allocatedBytes, entryCount: measured.entryCount, maxDepth: measured.maxDepth });
  const retentionRecord = () => gFollowup
    ? { invocation: measureSummary(invocationMeasure), stage: measureSummary(stageMeasure), campaign: campaignMeasure.total, priorCampaign: campaignMeasure.prior, limits: gBudgetLimits, basis: 'allocatedBytes' }
    : separatelyBound
      ? { invocation: measureSummary(invocationMeasure), stage: measureSummary(stageMeasure), campaign: campaignMeasure.total, priorCampaign: campaignMeasure.prior, limits: { invocationBytes: coordinateFollowup ? coordinateInvocationLimitBytes : invocationLimitBytes, invocationEntries: 8192, depth: 32, stageBytes: coordinateFollowup ? coordinateStageLimitBytes : followupStageLimitBytes, campaignBytes: followupCampaignLimitBytes } }
      : { invocation: invocationMeasure, stage: stageMeasure, historicalAllocatedBytes: historicalAllocated, limits: { invocationBytes: invocationLimitBytes, campaignBytes: 384 * 1024 * 1024, entries: 8192, depth: 32 } };
  const withinBudget = (measured, limits, label) => {
    try { assertC2BudgetEnvelope(measured, limits, label); return true; }
    catch { return false; }
  };
  const retentionExceeded = () => {
    if (gFollowup) return !withinBudget(invocationMeasure, gInvocationEnvelope, 'G1 retained invocation') || !withinBudget(stageMeasure, gStageEnvelope, 'G1 retained stage') || !withinBudget(campaignMeasure.total, gCampaignEnvelope, 'G1 retained campaign');
    const currentInvocationLimit = coordinateFollowup ? coordinateInvocationLimitBytes : invocationLimitBytes;
    if (((followup || coordinateFollowup) && invocationMeasure.logicalBytes > currentInvocationLimit) || invocationMeasure.allocatedBytes > currentInvocationLimit || invocationMeasure.entryCount > 8192 || invocationMeasure.maxDepth > 32) return true;
    if (separatelyBound) {
      const currentStageLimit = coordinateFollowup ? coordinateStageLimitBytes : followupStageLimitBytes;
      return stageMeasure.allocatedBytes > currentStageLimit || stageMeasure.maxDepth > 32 || campaignMeasure.total.allocatedBytes > followupCampaignLimitBytes || campaignMeasure.total.logicalBytes > followupCampaignLimitBytes || campaignMeasure.prior.allocatedBytes > 384 * 1024 * 1024 || campaignMeasure.prior.logicalBytes > 384 * 1024 * 1024;
    }
    return stageMeasure.allocatedBytes + historicalAllocated > 384 * 1024 * 1024 || stageMeasure.entryCount > 8192 || stageMeasure.maxDepth > 32;
  };
  const reconcileRetention = () => {
    const retentionError = new Error('C2 dynamic repair output crossed a retention limit');
    failure ||= retentionError;
    if (!tools.reconcileRetentionFailure(result, true, retentionRecord(), retentionError.message))
      throw new Error('C2 retention failure was not reconciled into the completion result');
    paths.rewriteFile(completionPath, 'controller', JSON.stringify(result, null, 2) + '\n');
    paths.rewriteFile(attemptSummaryPath, 'controller', JSON.stringify(summaryFor(), null, 2) + '\n');
    invocationMeasure = walkScope(invocationRoot);
    stageMeasure = walkScope(stageRoot);
    campaignMeasure = gFollowup
      ? measureGCampaign(inputs, currentSources)
      : followup
        ? measureFollowupCampaign(inputs, currentSources)
        : coordinateFollowup
          ? measureCoordinateCampaign(inputs, currentSources)
          : null;
  };
  if (gFollowup) {
    if (retentionExceeded()) reconcileRetention();
    else {
      result.verification.retention = { status: 'passed', ...retentionRecord(), receiptsReconciled: true };
      paths.rewriteFile(completionPath, 'controller', JSON.stringify(result, null, 2) + '\n');
      paths.rewriteFile(attemptSummaryPath, 'controller', JSON.stringify(summaryFor(), null, 2) + '\n');
      invocationMeasure = walkScope(invocationRoot);
      stageMeasure = walkScope(stageRoot);
      campaignMeasure = measureGCampaign(inputs, currentSources);
      if (retentionExceeded()) reconcileRetention();
    }
  }
  if (!separatelyBound) {
    if (retentionExceeded()) reconcileRetention();
    appendRepairJournal(stage, tools.terminalRecord(result, new Date().toISOString()));
    publishRepairJournal(stage);
    if (result.status === 'passed') publishRepairPass(stage, invocationRoot, currentSources);
  } else {
    if (retentionExceeded()) reconcileRetention();
    publishRepairJournal(stage);
    if (result.status === 'passed') {
      publishRepairPass(stage, invocationRoot, currentSources);
      stageMeasure = walkScope(stageRoot);
      campaignMeasure = gFollowup ? measureGCampaign(inputs, currentSources) : coordinateFollowup ? measureCoordinateCampaign(inputs, currentSources) : measureFollowupCampaign(inputs, currentSources);
      if (retentionExceeded()) {
        reconcileRetention();
        publishRepairPass(stage, invocationRoot, currentSources);
        stageMeasure = walkScope(stageRoot);
        campaignMeasure = gFollowup ? measureGCampaign(inputs, currentSources) : coordinateFollowup ? measureCoordinateCampaign(inputs, currentSources) : measureFollowupCampaign(inputs, currentSources);
      } else if (gFollowup) {
        result.verification.retention = { status: 'passed', ...retentionRecord(), receiptsReconciled: true };
        paths.rewriteFile(completionPath, 'controller', JSON.stringify(result, null, 2) + '\n');
        paths.rewriteFile(attemptSummaryPath, 'controller', JSON.stringify(summaryFor(), null, 2) + '\n');
        publishRepairPass(stage, invocationRoot, currentSources);
        invocationMeasure = walkScope(invocationRoot);
        stageMeasure = walkScope(stageRoot);
        campaignMeasure = measureGCampaign(inputs, currentSources);
        if (retentionExceeded()) {
          reconcileRetention();
          publishRepairPass(stage, invocationRoot, currentSources);
          stageMeasure = walkScope(stageRoot);
          campaignMeasure = measureGCampaign(inputs, currentSources);
        }
      }
    }
    if (gFollowup && result.status === 'passed') {
      try {
        const terminalReserve = 64 * 1024;
        assertC2BudgetEnvelope({ allocatedBytes: stageMeasure.allocatedBytes + terminalReserve, entries: stageMeasure.entryCount + 2, maxDepth: stageMeasure.maxDepth }, gStageEnvelope, 'G1 final stage reserve');
        assertC2BudgetEnvelope({ allocatedBytes: campaignMeasure.total.allocatedBytes + terminalReserve }, gCampaignEnvelope, 'G1 final campaign reserve');
      } catch (error) {
        failure ||= error;
        reconcileRetention();
        publishRepairPass(stage, invocationRoot, currentSources);
        stageMeasure = walkScope(stageRoot);
        campaignMeasure = measureGCampaign(inputs, currentSources);
      }
    }
    appendRepairJournal(stage, tools.terminalRecord(result, new Date().toISOString()));
    publishRepairJournal(stage);
  }
  stageMeasure = walkScope(stageRoot);
  campaignMeasure = gFollowup
    ? measureGCampaign(inputs, currentSources)
    : followup
      ? measureFollowupCampaign(inputs, currentSources)
      : coordinateFollowup
        ? measureCoordinateCampaign(inputs, currentSources)
        : null;
  const gBudgetReceipt = gFollowup ? {
    invocation: assertC2BudgetEnvelope(invocationMeasure, gInvocationEnvelope, 'G1 invocation receipt'),
    stage: assertC2BudgetEnvelope(stageMeasure, gStageEnvelope, 'G1 stage receipt'),
    campaign: assertC2BudgetEnvelope(campaignMeasure.total, gCampaignEnvelope, 'G1 campaign receipt'),
    limits: gBudgetLimits
  } : null;
  process.stdout.write(`${JSON.stringify({ phase: 'worker', attempt, status: result.status, invocationId, invocationRoot, cases: result.cases, passed: result.passed, failed: result.failed, variants: result.variants, nativeEffects: result.nativeEffects, invocationBytes: invocationMeasure.logicalBytes, invocationAllocatedBytes: invocationMeasure.allocatedBytes, stageAllocatedBytes: stageMeasure.allocatedBytes, stageEntries: stageMeasure.entryCount, ...(separatelyBound ? { campaignAllocatedBytes: campaignMeasure.total.allocatedBytes, campaignLogicalBytes: campaignMeasure.total.logicalBytes, sourceBindingSHA256: sourceBinding.sha256 } : {}), ...(gFollowup ? { budgetReceipt: gBudgetReceipt } : {}) })}\n`);
  if (failure || result.status !== 'passed') process.exitCode = 1;
}

async function main() {
  const args = process.argv.slice(2);
  const values = {};
  for (let index = 0; index < args.length; index += 1) {
    if (!['--phase', '--stage-root', '--baseline', '--inventory', '--binding'].includes(args[index]) || values[args[index]])
      throw new Error(`Unsupported or repeated C2 gate argument: ${args[index]}`);
    values[args[index]] = args[index + 1];
    index += 1;
  }
  const phase = values['--phase'];
  const stageRootPath = values['--stage-root'];
  const baselinePath = values['--baseline'];
  if (phase === 'repair') {
    await runDynamicRepair(values);
    return;
  }
  if (values['--binding'] !== undefined) throw new Error('C2 source binding arguments are only valid for the separately versioned repair phase');
  if (!['worker', 'controller'].includes(phase) || stageRootPath !== expectedStageRoot || baselinePath !== expectedBaselinePath)
    throw new Error('C2 gate phase or bound source paths differ from the configured command');
  const start = Date.now();
  const baselineBytes = fs.readFileSync(baselinePath);
  if (sha256(baselineBytes) !== expectedBaselineSHA256) throw new Error('Saved C2 baseline SHA-256 changed');
  const baseline = JSON.parse(baselineBytes.toString('utf8'));
  const preflightSpace = enforceSpace(statfsRecord('/private/tmp'), baseline, 40 * 1024 * 1024);
  const projected = estimateScope(baseline, baseline.inputs.filter(item => item.path.startsWith('frontend/')));
  let stage;
  if (fs.existsSync(stageRootPath)) {
    stage = requireFromHere('./c2-paths.cjs').readStageRoot(stageRootPath);
  } else {
    stage = requireFromHere('./c2-paths.cjs').createStageRoot(stageRootPath);
    requireFromHere('./c2-paths.cjs').writeStageFile(stage, 'stage-root.json', JSON.stringify({ schema: 1, path: stage.path, identity: stage.identity, parentAncestry: stage.parentAncestry }) + '\n');
  }
  const pathTools = requireFromHere('./c2-paths.cjs');
  const journal = recordSecondAmendment(stage, readJournal(stage), pathTools);
  const priorBegins = journal.filter(row => row.type === 'BEGIN');
  const priorTerminals = journal.filter(row => row.type === 'TERMINAL');
  const terminalIds = new Set(priorTerminals.map(row => row.invocationId));
  if (priorBegins.some(row => !terminalIds.has(row.invocationId)))
    throw new Error('C2 stage contains an invocation without a terminal receipt; preserve it and stop');
  const journaledIds = new Set(priorBegins.map(row => row.invocationId));
  const stageEntries = fs.readdirSync(stage.path);
  const invocationEntries = stageEntries.filter(name => name.startsWith('invocation-'));
  const allowedStageEntries = new Set(['stage-root.json', 'journal.ndjson', supplementalAllowanceFilename, supplementalAmendmentFilename, supplementalAmendment2Filename, staticPreflightFilename, staticPreflightRevision02Filename, staticPreflightRevisionFilename, attempt10PreflightFilename]);
  if (stageEntries.some(name => !allowedStageEntries.has(name) && !/^invocation-[0-9a-f-]{36}$/.test(name)) || invocationEntries.length !== priorBegins.length || invocationEntries.some(name => !journaledIds.has(name.slice('invocation-'.length))))
    throw new Error('C2 stage root contains an unjournaled or out-of-scope retained path');
  const allowance = verifySupplementalAllowance(stage, journal);
  const staticPreflight = verifySavedStaticPreflight(stage, journal, allowance);
  validateSupplementalHistory(stage, priorBegins, priorTerminals, phase, allowance);
  if (priorBegins.length >= allowance.totalCap) throw new Error(`Dated C2 supplemental allowance of ${allowance.totalCap} total invocations has been consumed`);
  const stagePreflight = walkScope(stage.path);
  const stageLimitBytes = 384 * 1024 * 1024;
  if (stagePreflight.logicalBytes + 64 * 1024 * 1024 > stageLimitBytes || stagePreflight.allocatedBytes + 64 * 1024 * 1024 > stageLimitBytes || stagePreflight.entryCount + projected.entries > 8192 || Math.max(stagePreflight.maxDepth, projected.maxDepth + 1) > 32)
    throw new Error('C2 supplemental invocation cannot fit inside the unchanged retained-stage envelope');
  let predecessorWorkerBinding;
  if (phase === 'controller') {
    const worker = [...priorTerminals].reverse().find(row => row.phase === 'worker' && row.status === 'passed' && row.baselineSHA256 === expectedBaselineSHA256);
    if (!worker) throw new Error('C2 controller gate requires the passing supplemental worker-phase source gate');
    predecessorWorkerBinding = verifyWorkerReceipt(stage, worker, allowance);
  }
  const invocationId = crypto.randomUUID();
  const invocationRoot = path.join(stage.path, `invocation-${invocationId}`);
  const paths = pathTools.createInvocation(stage, invocationRoot, invocationId);
  const attemptNumber = priorBegins.length === 8 ? 10 : 11;
  const invocation = { id: invocationId, attemptNumber, phase, root: invocationRoot, startedAt: new Date().toISOString(), baselineSHA256: expectedBaselineSHA256 };
  const journalRecord = { type: 'BEGIN', phase, invocationId, invocationRoot, startedAt: invocation.startedAt, baselineSHA256: expectedBaselineSHA256, projected, preflightSpace, stagePreflight: { logicalBytes: stagePreflight.logicalBytes, allocatedBytes: stagePreflight.allocatedBytes, entries: stagePreflight.entryCount, limitBytes: stageLimitBytes }, attemptNumber, supplementalAllowanceSHA256: allowance.sha256, supplementalAmendmentSHA256: allowance.amendment.sha256, supplementalAmendment2SHA256: allowance.amendment2.sha256, usage: 'provider workflow telemetry unavailable; no orchestration dispatch' }
  if (!stage.journal) {
    const sidecar = pathTools.writeStageFile(stage, 'journal.ndjson', JSON.stringify(journalRecord) + '\n');
    stage.journal = { path: sidecar.path, identity: sidecar.identity };
  } else {
    stage.journal.identity = pathTools.appendStageFile(stage, stage.journal.path, stage.journal.identity, JSON.stringify(journalRecord) + '\n');
  }
  const envKeys = ['HERDR_C2_INVOCATION', 'HERDR_C2_INVOCATION_ROOT', 'HERDR_C2_NATIVE_READY', 'HERDR_WEB_ROOT', 'PWTEST_CACHE_DIR'];
  const envBefore = Object.fromEntries(envKeys.map(key => [key, process.env[key]]));
  let result;
  let runtime;
  let route;
  let generated;
  let tests;
  let testsSHA256;
  let testsEvidencePath;
  let testContext;
  let verification = {};
  let failure = null;
  let wrapperSource;
  let descriptor;
  let descriptorPath;
  let runtimeRoot;
  try {
    wrapperSource = sourceBoundWrapper(baseline);
    const packageJson = baseline.inputs.find(item => item.path === 'frontend/package.json');
    const configInput = baseline.inputs.find(item => item.path === 'frontend/playwright.config.ts');
    descriptor = {
      schema: 1,
      invocationId,
      root: invocationRoot,
      stageRoot: stage.path,
      routeManifestPath: path.join(invocationRoot, 'control/route-map.json'),
      uid: paths.uid,
      nativeReady: false,
      configPath: path.join(invocationRoot, 'runtime/project/frontend/playwright.config.ts'),
      configSHA256: configInput.sha256,
      webRoot: path.resolve(repositoryRoot, 'frontend/dist'),
      internalCacheDir: path.join(invocationRoot, 'transform-cache'),
      packageSHA256: packageJson.sha256,
      deadline: Number.MAX_SAFE_INTEGER
    };
    descriptorPath = path.join(invocationRoot, 'invocation.json');
    const descriptorBytes = JSON.stringify(descriptor, null, 2) + '\n';
    paths.acquireFile('invocation.json', 'controller', descriptorBytes);
    process.env.HERDR_C2_INVOCATION = descriptorPath;
    process.env.HERDR_C2_INVOCATION_ROOT = invocationRoot;
    process.env.HERDR_WEB_ROOT = descriptor.webRoot;
    process.env.PWTEST_CACHE_DIR = descriptor.internalCacheDir;
    delete process.env.HERDR_C2_NATIVE_READY;
    paths.acquireFile('completion-result.json', 'controller', '');
    paths.acquireFile('attempt-summary.json', 'controller', '');
    verification.supplementalAllowance = allowance;
    verification.staticPreflight = staticPreflight;
    verification.stagePreflight = { logicalBytes: stagePreflight.logicalBytes, allocatedBytes: stagePreflight.allocatedBytes, entries: stagePreflight.entryCount, limitBytes: stageLimitBytes };
    if (predecessorWorkerBinding) verification.predecessorWorkerBinding = predecessorWorkerBinding;
    verification.baselineBefore = verifyBaseline(baseline);
    const scope = enforceSpace(statfsRecord('/private/tmp'), baseline, 40 * 1024 * 1024);
    verification.preGenerationSpace = scope;
    const wrapperRestoredSHA256 = sha256(Buffer.from(wrapperSource.stock));
    if (wrapperRestoredSHA256 !== wrapperSource.identity.sha256) throw new Error('C2 wrapper stock source does not match its baseline');
    const routeModule = requireFromHere('./c2-route.cjs');
    route = routeModule.createPrivateRoute(paths, descriptor);
    runtimeRoot = path.join(invocationRoot, 'runtime');
    const generatedResult = generateC2({ baselinePath, descriptor, invocationRoot, runtimeRoot, route });
    generated = generatedResult;
    const reconstructed = applyC2Edits({ sources: generatedResult.original, releaseFixture: generatedResult.projectSources['frontend/tests/browser/release-resources.spec.ts'] });
    for (const [file, content] of Object.entries(generatedResult.generated))
      if (reconstructed.generated[file] !== content) throw new Error(`C2 exact source patch reconstruction differs: ${file}`);
    verification.patchReconstruction = { editCount: reconstructed.edits.length, generatedFiles: Object.keys(reconstructed.generated).length, exact: true, patchSHA256: generatedResult.manifest.patchReconstruction.sha256 };
    const privateDir = path.join(runtimeRoot, 'project/frontend/private');
    const bootstrapPath = path.join(privateDir, 'c2-bootstrap.cjs');
    const supportPath = path.join(privateDir, 'privateCompletion.cjs');
    const generatedRequire = createRequire(bootstrapPath);
    const bootstrap = generatedRequire(bootstrapPath);
    const support = generatedRequire(supportPath);
    const generatedRouteModule = createRequire(bootstrapPath)('./c2-route.cjs');
    runtime = bootstrap.installSourceRuntime({ nativeEffectsDisabled: true, paths, descriptor, route });
    if (runtime.completion !== support || bootstrap.getPrivateCompletion() !== support)
      throw new Error('Generated C2 bootstrap and completion support do not share the private module instance');
    verification.privateBootstrap = { path: bootstrapPath, sha256: sha256(fs.readFileSync(bootstrapPath)), supportPath, supportSHA256: sha256(fs.readFileSync(supportPath)), configuredOnce: true, nativeReady: false };
    verification.sourceParse = syntaxAndTranspile(generatedResult, runtimeRoot);
    const wrapperSyntax = await runCommand('bash', ['-n', path.join(repositoryRoot, 'frontend/scripts/run-browser-tests.sh')], repositoryRoot);
    verification.wrapperSyntax = wrapperSyntax;
    if (wrapperSyntax.exitCode !== 0) throw new Error(`bash -n failed: ${wrapperSyntax.stderr}`);
    const lintArgs = [
      'node_modules/eslint/bin/eslint.js',
      'scripts/private-qualification/c2-generate.mjs',
      'scripts/private-qualification/c2-edits.mjs',
      'scripts/private-qualification/c2-bootstrap.cjs',
      'scripts/private-qualification/c2-paths.cjs',
      'scripts/private-qualification/c2-route.cjs',
      'scripts/private-qualification/c2-tests.mjs',
      'scripts/private-qualification/c2-check.mjs'
    ];
    const lint = await runCommand('bun', lintArgs, path.join(repositoryRoot, 'frontend'));
    verification.targetedLint = lint;
    if (lint.exitCode !== 0) throw new Error(`Targeted ESLint failed: ${lint.stderr || lint.stdout}`);
    const casesRootActor = 'test-case-root';
    paths.registerActor(casesRootActor, paths.root);
    const casesRoot = paths.acquireSharedDirectory('test-cases', casesRootActor).path;
    const wrapperSourceInput = fs.readFileSync(path.join(repositoryRoot, 'frontend/scripts/run-browser-tests.sh'), 'utf8');
    const priorInvocationIds = priorBegins.map(row => row.invocationId);
    testContext = {
      paths,
      pathTools,
      casesRoot,
      childScopes: [],
      knownNegativePaths: [],
      descriptor,
      bootstrap,
      support,
      route,
      routeFactory: generatedRouteModule.createPrivateRoute,
      routeHelpers: generatedRouteModule,
      manifest: generatedResult.manifest,
      generated: generatedResult.generated,
      projectSources: generatedResult.projectSources,
      originalReleaseFixture: generatedResult.projectSources['frontend/tests/browser/release-resources.spec.ts'],
      runtimeRoot,
      wrapperSource: wrapperSourceInput,
      wrapperBaseline: wrapperSource.stock,
      priorInvocationIds,
      nativeEffectsDisabled: true
    };
    tests = await runC2Tests(testContext);
    verification.sourceCases = { status: tests.status, cases: tests.expectedCases, passed: tests.passedCases, failed: tests.failedCases, variants: tests.variantCount, nativeEffects: tests.nativeEffects, durationMs: tests.durationMs };
    const testsNDJSON = tests.cases.map(testCase => JSON.stringify(testCase)).join('\n') + '\n';
    testsSHA256 = sha256(Buffer.from(testsNDJSON));
    testsEvidencePath = path.join(invocationRoot, 'tests.ndjson');
    route.writeRetainedFile(testsEvidencePath, testsNDJSON, 'c2-tests-results');
    verification.caseEvidence = { path: testsEvidencePath, sha256: testsSHA256, cases: tests.expectedCases, passed: tests.passedCases, failed: tests.failedCases, variants: tests.variantCount };
    if (tests.status !== 'passed' || tests.expectedCases !== 20 || tests.passedCases !== 20 || tests.failedCases !== 0 || tests.variantCount !== expectedC2Variants || tests.nativeEffects !== 0 || tests.durationMs > 30000)
      throw new Error('C2 exact 20-case source suite did not pass within its bounded native-disabled contract');
    const measuredScope = validateRegisteredOutputs(paths, testContext.childScopes, testContext.knownNegativePaths);
    verification.outputEnvelope = { ...measuredScope, projected, limitBytes: 64 * 1024 * 1024, elapsedMs: Date.now() - start };
    const accounting = vendorAccounting(generatedResult.manifest, tests, wrapperSource, generatedResult.projectSources['frontend/tests/browser/release-resources.spec.ts']);
    const boundaryAfter = verifyBaseline(baseline);
    verification.baselineAfter = boundaryAfter;
    const statfsAfter = statfsRecord('/private/tmp');
    if (statfsAfter.availableBytes < 24 * 1024 ** 3) throw new Error('C2 stage crossed the saved 24 GiB hard stop; retain outputs and stop');
    if (statfsAfter.availableBytes < 28 * 1024 ** 3) throw new Error('C2 stage crossed the saved 28 GiB stop line; retain outputs and stop');
    verification.statfsAfter = statfsAfter;
    if (Date.now() - start > 120000) throw new Error('C2 whole source gate exceeded its 120-second ceiling');
    result = {
      schema: 1,
      phase,
      attemptNumber,
      status: 'passed',
      invocationId,
      invocationRoot,
      stageRoot: stage.path,
      baselineSHA256: expectedBaselineSHA256,
      startedAt: invocation.startedAt,
      completedAt: new Date().toISOString(),
      elapsedMs: Date.now() - start,
      cases: tests.expectedCases,
      passed: tests.passedCases,
      failed: tests.failedCases,
      variants: tests.variantCount,
      nativeEffects: tests.nativeEffects,
      historicalC1: generatedResult.manifest.c1,
      historicalCountersPreserved: true,
      supplementalAllowance: allowance,
      currentC2Counters: { workflowLaunches: 0, workerTurns: 0, independentReviews: 0, audits: 0, projectTestInvocations: 0, sourceGateInvocations: priorBegins.length + 1, providerUsage: 'unavailable; no orchestration dispatch or provider telemetry was created' },
      accounting,
      sourceManifestPath: generatedResult.manifestPath,
      sourceManifestSHA256: sha256(fs.readFileSync(generatedResult.manifestPath)),
      testsSHA256,
      verification,
      futureAcceptance: ['C3', 'C4', 'C5', 'P1', 'P2', 'P3', 'native-loaded closure', 'real browser/Pi/Herdr/live qualification']
    };
    paths.continueFile(path.join(invocationRoot, 'completion-result.json'), 'controller', JSON.stringify(result, null, 2) + '\n');
    paths.continueFile(path.join(invocationRoot, 'attempt-summary.json'), 'controller', JSON.stringify({ phase, attemptNumber, status: result.status, invocationId, cases: tests.expectedCases, passed: tests.passedCases, failed: tests.failedCases, variants: tests.variantCount, nativeEffects: tests.nativeEffects, elapsedMs: result.elapsedMs }) + '\n');
  } catch (error) {
    failure = error;
    const measured = (() => { try { return validateRegisteredOutputs(paths, testContext?.childScopes || [], testContext?.knownNegativePaths || []); } catch (scopeError) { return { error: String(scopeError.message || scopeError) }; } })();
    result = {
      schema: 1,
      phase,
      attemptNumber,
      status: 'failed',
      supplementalAllowance: allowance,
      invocationId,
      invocationRoot,
      stageRoot: stage.path,
      baselineSHA256: expectedBaselineSHA256,
      startedAt: invocation.startedAt,
      completedAt: new Date().toISOString(),
      elapsedMs: Date.now() - start,
      nativeEffects: route?.nativeEffectCount?.() ?? 0,
      historicalC1: generated?.manifest?.c1 ?? null,
      currentC2Counters: { sourceGateInvocations: priorBegins.length + 1, projectTestInvocations: 0, providerUsage: 'unavailable; no orchestration dispatch or provider telemetry was created' },
      error: String(error.stack || error),
      partialMeasuredScope: measured,
      outputsRetained: true,
      verification
    };
    try {
      paths.continueFile(path.join(invocationRoot, 'completion-result.json'), 'controller', JSON.stringify(result, null, 2) + '\n');
    } catch {
      try {
        paths.acquireFile('failure.json', 'controller', JSON.stringify(result, null, 2) + '\n');
      } catch (receiptError) {
        failure ||= receiptError;
      }
    }
  } finally {
    maybeRestoreEnv(envBefore);
    const terminal = { type: 'TERMINAL', phase, status: result?.status || 'failed', invocationId, invocationRoot, completedAt: new Date().toISOString(), baselineSHA256: expectedBaselineSHA256, supplementalAllowanceSHA256: allowance.sha256, supplementalAmendmentSHA256: allowance.amendment.sha256, supplementalAmendment2SHA256: allowance.amendment2.sha256, attemptNumber, elapsedMs: Date.now() - start, nativeEffects: result?.nativeEffects ?? 0, usage: 'provider usage unavailable; source gate execution is not an orchestration launch' };
    try {
      stage.journal.identity = pathTools.appendStageFile(stage, stage.journal.path, stage.journal.identity, JSON.stringify(terminal) + '\n');
    } catch (error) {
      failure ||= error;
    }
  }
  const finalRoot = walkScope(invocationRoot);
  if (finalRoot.logicalBytes > 64 * 1024 * 1024 || finalRoot.allocatedBytes > 64 * 1024 * 1024 || finalRoot.entryCount > 8192 || finalRoot.maxDepth > 32) {
    failure ||= new Error('Final C2 receipt outputs exceed the per-invocation envelope');
    if (result) result.status = 'failed';
  }
  const stageUsage = walkScope(stage.path);
  if (stageUsage.logicalBytes > 384 * 1024 * 1024 || stageUsage.allocatedBytes > 384 * 1024 * 1024 || stageUsage.entryCount > 8192 || stageUsage.maxDepth > 32) {
    failure ||= new Error('C2 retained stage exceeds its 384 MiB/8192-entry/depth-32 envelope');
    if (result) result.status = 'failed';
  }
  const receipt = { ...result, invocationBytes: finalRoot.logicalBytes, invocationAllocatedBytes: finalRoot.allocatedBytes, invocationEntries: finalRoot.entryCount, terminalStatus: result?.status || 'failed' };
  process.stdout.write(`${JSON.stringify({ phase, attemptNumber, status: result?.status || 'failed', invocationId, invocationRoot, summaryPath: path.join(invocationRoot, 'completion-result.json'), summarySHA256: fs.existsSync(path.join(invocationRoot, 'completion-result.json')) ? sha256(fs.readFileSync(path.join(invocationRoot, 'completion-result.json'))) : null, cases: result?.cases ?? 0, passed: result?.passed ?? 0, failed: result?.failed ?? 1, variants: result?.variants ?? 0, nativeEffects: result?.nativeEffects ?? 0, stageAttempts: priorBegins.length + 1, invocationBytes: receipt.invocationBytes, invocationAllocatedBytes: receipt.invocationAllocatedBytes, stageBytes: stageUsage.logicalBytes, stageAllocatedBytes: stageUsage.allocatedBytes, stageEntries: stageUsage.entryCount})}\n`);
  if (failure || result?.status !== 'passed') process.exitCode = 1;
}

await main();
