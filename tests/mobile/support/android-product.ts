import { namespaceForCapability, sameKernelCapability, validKernelCapability } from '../android-appium/kernel-namespace.cjs';
import type { AndroidPlannedTermination, AndroidEnvironmentCheck } from '../android-environment';
import type { RetainedInspectionResult } from '../android-appium/retained-inspection.cjs';
import type { RetainedNativeIdentity } from './android-retained-decoder';
import type { MeasurementIdentity, MobileResultContract, ProductStatus } from './mobile-result';
import { productStatus, validateMeasurementIdentity } from './mobile-result';

export const PRODUCT_RECORD_LIMIT = 1000;
export const PRODUCT_BYTE_LIMIT = 8 * 1024 * 1024;
type Receipt = {
  ordinal: number;
  kind: 'checkpoint' | 'inspection' | 'operation-begin' | 'operation-end' | 'cold-handoff' | 'transition' | 'document-change';
  owner: number;
  name: string;
  target?: number;
  document?: number;
  operation?: number;
  sequence?: number;
  detail?: 'identity' | 'completion' | 'preference' | 'keyboard' | 'control';
  completedOperations?: number;
  termination?: AndroidPlannedTermination;
  beforeInspection?: number;
  afterInspection?: number;
  native?: RetainedNativeIdentity;
  bounds?: Array<{ startedAt: number; finishedAt: number; native: Array<RetainedNativeIdentity & { startedAt: number; finishedAt: number }> }>;
  association?: { kind: string; before: { pid: number; requestId: number; connection: number; startedAt: number; completedAt: number }; after: { pid: number; requestId: number; connection: number; startedAt: number; completedAt: number } };
  startedAt?: number;
  finishedAt?: number;
  deadline?: number;
  pageCount?: number;
};
export interface ProductObservations {
  schema: 1;
  identity: MeasurementIdentity;
  coverage: 'bounded-operations';
  initialOwner: number;
  initialNative: RetainedNativeIdentity;
  records: Receipt[];
  failure?: { category: 'OWNERSHIP_LOSS' | 'INSPECTION_FAILURE' | 'RECEIPT_LIMIT'; ordinal: number };
}
const equal = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);
const sameNative = (first: RetainedNativeIdentity, second: RetainedNativeIdentity) => first.pid === second.pid && first.startTime === second.startTime && first.bootId === second.bootId && first.namespace === second.namespace && sameKernelCapability(first.kernelCapability, second.kernelCapability);
const nativeIdentity = (value: RetainedNativeIdentity): RetainedNativeIdentity => structuredClone({
  pid: value.pid, startTime: value.startTime, bootId: value.bootId, namespace: value.namespace, kernelCapability: value.kernelCapability,
});

export class AndroidProductRecorder {
  private readonly owners = new WeakMap<object, number>();
  private readonly targets = new Map<string, number>();
  private readonly documents = new Map<string, number>();
  private readonly connections = new Map<string, number>();
  private nextOwner = 0;
  private value?: ProductObservations;
  private completedOperations = 0;
  private lastInspection?: Receipt;
  private openOperation?: number;
  private pendingTransition?: string;

  owner(value: object): number {
    let ordinal = this.owners.get(value);
    if (ordinal !== undefined) return ordinal;
    ordinal = ++this.nextOwner;
    this.owners.set(value, ordinal);
    return ordinal;
  }

  activate(identity: MeasurementIdentity, owner: object, native: RetainedNativeIdentity): void {
    if (this.value) throw new Error('ANDROID_PRODUCT: measurement already bound');
    validateMeasurementIdentity(identity);
    this.value = { schema: 1, identity: structuredClone(identity), coverage: 'bounded-operations', initialOwner: this.owner(owner), initialNative: nativeIdentity(native), records: [] };
    this.checkpoint('measurement-start', owner);
  }

  private ordinal(map: Map<string, number>, text: string): number {
    const previous = map.get(text);
    if (previous !== undefined) return previous;
    const next = map.size + 1;
    map.set(text, next);
    return next;
  }

  private append(receipt: Omit<Receipt, 'ordinal'>): number {
    if (!this.value) return 0;
    if (this.value.failure) throw new Error('ANDROID_PRODUCT: observation already failed');
    const ordinal = this.value.records.length + 1;
    const next = structuredClone({ ...receipt, ordinal });
    if (ordinal > PRODUCT_RECORD_LIMIT || new TextEncoder().encode(JSON.stringify({ ...this.value, records: [...this.value.records, next] })).length > PRODUCT_BYTE_LIMIT) {
      this.fail('RECEIPT_LIMIT');
      throw new Error('ANDROID_PRODUCT: receipt bound exceeded');
    }
    this.value.records.push(next);
    return ordinal;
  }

  checkpoint(name: string, owner: object): void {
    if (!this.value) return;
    this.append({ kind: 'checkpoint', name, owner: this.owner(owner), beforeInspection: this.lastInspection?.ordinal, completedOperations: this.completedOperations });
    this.pendingTransition = undefined;
  }

  transition(name: 'initial-install' | 'pairing-navigation' | 'update-activation' | 'warm-launch', owner: object): void {
    if (!this.value) return;
    this.append({ kind: 'transition', name, owner: this.owner(owner), beforeInspection: this.lastInspection?.ordinal });
    this.pendingTransition = name;
  }

  fail(category: NonNullable<ProductObservations['failure']>['category']): void {
    if (this.value && !this.value.failure) this.value.failure = { category, ordinal: this.value.records.length + 1 };
  }

  inspection(owner: object, result: RetainedInspectionResult, startedAt: number, finishedAt: number, deadline: number): number {
    if (!this.value) return 0;
    const association = (entry: RetainedInspectionResult['processAssociation']['before']) => ({
      pid: entry.pid, requestId: entry.requestId, connection: this.ordinal(this.connections, entry.connectionId), startedAt: entry.startedAt, completedAt: entry.completedAt,
    });
    const ordinal = this.append({
      kind: 'inspection', name: result.phase, owner: this.owner(owner), native: nativeIdentity(result.original),
      target: this.ordinal(this.targets, result.selectedHandle),
      document: this.ordinal(this.documents, JSON.stringify([result.selectedHandle, result.after.document])),
      startedAt, finishedAt, deadline, pageCount: result.targets.filter(target => target.type === 'page').length,
      bounds: [result.before, result.after].map(snapshot => ({ startedAt: snapshot.startedAt, finishedAt: snapshot.finishedAt,
        native: [snapshot.nativeBefore, snapshot.native].map(native => ({ ...nativeIdentity(native), startedAt: native.startedAt, finishedAt: native.finishedAt })),
      })),
      association: { kind: result.processAssociation.kind, before: association(result.processAssociation.before), after: association(result.processAssociation.after) },
    });
    const current = this.value.records[ordinal - 1];
    if (this.lastInspection && (current.target !== this.lastInspection.target || current.document !== this.lastInspection.document)) {
      const operation = this.value.records[(this.openOperation || 0) - 1];
      const reason = operation?.name === 'mutation' ? 'mutation' : this.pendingTransition;
      if (!reason || operation?.name === 'read') { this.fail('INSPECTION_FAILURE'); throw new Error('ANDROID_PRODUCT: unrecorded document transition'); }
      this.append({ kind: 'document-change', name: reason, owner: this.owner(owner), beforeInspection: this.lastInspection.ordinal, afterInspection: ordinal });
    }
    this.lastInspection = current;
    return ordinal;
  }

  begin(name: 'read' | 'mutation', owner: object, beforeInspection: number, detail: Receipt['detail'] = 'control'): number {
    if (!this.value) return 0;
    if (this.openOperation) { this.fail('INSPECTION_FAILURE'); throw new Error('ANDROID_PRODUCT: overlapping operation'); }
    this.openOperation = this.append({ kind: 'operation-begin', name, owner: this.owner(owner), beforeInspection, detail, sequence: this.completedOperations + 1 });
    return this.openOperation;
  }

  end(name: 'read' | 'mutation', owner: object, operation: number, afterInspection: number): void {
    if (!this.value) return;
    this.append({ kind: 'operation-end', name, owner: this.owner(owner), operation, afterInspection, sequence: ++this.completedOperations });
    this.openOperation = undefined;
  }

  handoff(owner: object, operations: () => AndroidPlannedTermination[]): void {
    if (!this.value) return;
    const planned = operations();
    this.append({ kind: 'cold-handoff', name: 'planned-cold', owner: this.owner(owner), operation: planned.length, termination: planned.at(-1) });
    this.lastInspection = undefined;
    this.pendingTransition = 'cold-launch';
  }

  snapshot(): ProductObservations | undefined { return this.value ? structuredClone(this.value) : undefined; }
}

export function validateProductObservations(value: ProductObservations, identity: MeasurementIdentity, planned: AndroidPlannedTermination[] = []): void {
  const require = (condition: unknown) => { if (!condition) throw new Error('ANDROID_PRODUCT: invalid observation coverage'); };
  require(value?.schema === 1 && value.coverage === 'bounded-operations' && equal(value.identity, identity));
  validateMeasurementIdentity(identity);
  require(!value.failure && Number.isSafeInteger(value.initialOwner) && value.initialOwner > 0);
  require(Array.isArray(value.records) && value.records.length > 0 && value.records.length <= PRODUCT_RECORD_LIMIT
    && new TextEncoder().encode(JSON.stringify(value)).length <= PRODUCT_BYTE_LIMIT);
  let owner = value.initialOwner;
  const validNative = (native: RetainedNativeIdentity, end: number) => Boolean(native && /^[1-9]\d*$/u.test(native.pid)
    && Number(native.pid) <= 2147483647 && /^[1-9]\d*$/u.test(native.startTime)
    && validKernelCapability(native.kernelCapability, native.bootId, end) && native.namespace === namespaceForCapability(native.kernelCapability));
  require(validNative(value.initialNative, Number.MAX_SAFE_INTEGER));
  let native: RetainedNativeIdentity | undefined = value.initialNative;
  let handoffs = 0;
  let lastInspection: Receipt | undefined;
  let previousInspectionEnd = 0;
  const operations = new Map<number, Receipt>();
  const checkpoints = new Set<string>();
  let inspections = 0;
  let completedOperations = 0;
  let checkpointInspection = 0;
  let transition: string | undefined;
  const phaseCoverage = new Set<string>();
  const requiredCoverage: Record<string, string[]> = {
    paired: ['initial-install', 'pairing-navigation', 'identity', 'preference', 'mutation'],
    candidate: ['update-activation', 'identity', 'completion', 'mutation'],
    'lifecycle-complete': ['warm-launch', 'cold-launch', 'identity', 'completion', 'preference', 'mutation'],
    'scenario-complete': identity.suite === 'smoke' ? ['warm-launch', 'identity', 'preference'] : [],
  };
  for (const [index, record] of value.records.entries()) {
    require(record.ordinal === index + 1);
    if (record.kind === 'cold-handoff') {
      const termination = planned[handoffs];
      require(record.owner === owner + 1 && record.operation === ++handoffs && identity.suite === 'release' && operations.size === 0
        && checkpoints.has('candidate') && !checkpoints.has('lifecycle-complete'));
      require(termination?.succeeded === true && termination.measurementId === identity.measurementId && equal(record.termination, termination)
        && termination.pid === native?.pid && termination.processes[termination.pid] === termination.packageName);
      owner = record.owner;
      native = undefined;
      lastInspection = undefined;
      transition = 'cold-launch';
      phaseCoverage.add(transition);
      continue;
    }
    require(record.owner === owner);
    if (record.kind === 'transition') {
      require(operations.size === 0 && record.beforeInspection === lastInspection?.ordinal);
      const phase = checkpoints.size;
      require((record.name === 'initial-install' && phase === 1 && !lastInspection)
        || (record.name === 'pairing-navigation' && phase === 1 && lastInspection?.name === 'installed-selected')
        || (record.name === 'update-activation' && phase === 2 && lastInspection?.name === 'installed-selected')
        || (record.name === 'warm-launch' && phase === 3 && lastInspection?.name === 'installed-selected'));
      transition = record.name; phaseCoverage.add(record.name);
      continue;
    }
    if (record.kind === 'document-change') {
      const before = value.records[(record.beforeInspection || 0) - 1];
      require(before?.kind === 'inspection' && record.afterInspection === lastInspection?.ordinal && lastInspection!.ordinal === record.ordinal - 1
        && before.owner === owner && before.ordinal < lastInspection!.ordinal && (before.target !== lastInspection!.target || before.document !== lastInspection!.document));
      require(record.name === (operations.values().next().value?.name === 'mutation' ? 'mutation' : transition));
      continue;
    }
    if (record.kind === 'checkpoint') {
      const sequence = identity.suite === 'release' ? ['measurement-start', 'paired', 'candidate', 'lifecycle-complete', 'scenario-complete'] : ['measurement-start', 'paired', 'candidate', 'scenario-complete'];
      require(record.name === sequence[checkpoints.size] && (record.name === 'measurement-start' ? index === 0 : lastInspection?.name === 'installed-selected'));
      require(operations.size === 0 && record.completedOperations === completedOperations && record.beforeInspection === lastInspection?.ordinal);
      const required = requiredCoverage[record.name] || [];
      for (const item of required) require(phaseCoverage.has(item));
      if (required.length) require(lastInspection!.ordinal > checkpointInspection);
      if (record.name === 'scenario-complete') require(index === value.records.length - 1);
      checkpointInspection = lastInspection?.ordinal || 0;
      phaseCoverage.clear(); transition = undefined;
      checkpoints.add(record.name);
      continue;
    }
    if (record.kind === 'operation-begin') {
      require(operations.size === 0 && ['read', 'mutation'].includes(record.name) && record.sequence === completedOperations + 1);
      require(record.name === 'read' ? ['identity', 'completion', 'preference', 'keyboard'].includes(record.detail || '') : record.detail === 'control');
      const first = value.records[(record.beforeInspection || 0) - 1];
      require(first?.kind === 'inspection' && first.ordinal === lastInspection?.ordinal && first.ordinal < record.ordinal && first.owner === owner);
      operations.set(record.ordinal, record);
      continue;
    }
    if (record.kind === 'operation-end') {
      const begin = operations.get(record.operation || 0);
      const before = value.records[(begin?.beforeInspection || 0) - 1];
      const after = value.records[(record.afterInspection || 0) - 1];
      require(begin && begin.name === record.name && after?.kind === 'inspection' && after.ordinal > begin.ordinal && after.ordinal < record.ordinal && after.ordinal === lastInspection?.ordinal && after.owner === owner);
      if (record.name === 'read') require(before.target === after.target && before.document === after.document);
      require(record.sequence === ++completedOperations);
      phaseCoverage.add(record.name);
      phaseCoverage.add(begin!.detail!);
      operations.delete(record.operation!);
      continue;
    }
    require(record.kind === 'inspection');
    inspections++;
    require(record.native && validNative(record.native, record.startedAt!));
    if (inspections === 1) require(validNative(value.initialNative, record.startedAt!));
    require(['initial-browser-selected', 'installed-selected'].includes(record.name));
    if (lastInspection && lastInspection.name !== 'initial-browser-selected') require(record.name === 'installed-selected' && record.target === lastInspection.target);
    if (lastInspection && (record.target !== lastInspection.target || record.document !== lastInspection.document)) {
      const active = operations.values().next().value;
      require(active?.name !== 'read' && (active?.name === 'mutation' || transition));
      const change = value.records[index + 1];
      require(change?.kind === 'document-change' && change.beforeInspection === lastInspection.ordinal && change.afterInspection === record.ordinal);
    }
    lastInspection = record;
    if (native) require(sameNative(record.native!, native));
    native = record.native!;
    require(Number.isSafeInteger(record.target) && record.target! > 0 && Number.isSafeInteger(record.document) && record.document! > 0);
    require(record.bounds?.length === 2 && record.startedAt! > 0 && record.startedAt! >= previousInspectionEnd && record.startedAt! <= record.finishedAt! && record.finishedAt! <= record.deadline!);
    previousInspectionEnd = record.finishedAt!;
    require(Number.isSafeInteger(record.pageCount) && record.pageCount! >= 1 && record.pageCount! <= 32);
    const [before, after] = record.bounds!;
    require(record.startedAt! <= before.startedAt && before.startedAt <= before.finishedAt && before.finishedAt <= after.startedAt && after.startedAt <= after.finishedAt && after.finishedAt <= record.finishedAt!);
    for (const bound of record.bounds!) {
      require(bound.native.length === 2 && bound.native[0].finishedAt <= bound.native[1].startedAt);
      for (const observed of bound.native) require(validNative(observed, observed.finishedAt) && sameNative(observed, native) && bound.startedAt <= observed.startedAt && observed.startedAt <= observed.finishedAt && observed.finishedAt <= bound.finishedAt);
    }
    const association = record.association!;
    require(association?.kind === 'bounded-sequential-service-to-process');
    require(association.before.pid === Number(native.pid) && association.after.pid === Number(native.pid)
      && association.before.requestId === 1 && association.after.requestId === 4 + 7 * record.pageCount!
      && association.before.connection === association.after.connection && Number.isSafeInteger(association.before.connection) && association.before.connection > 0
      && before.finishedAt <= association.before.startedAt && association.before.startedAt <= association.before.completedAt
      && association.before.completedAt <= association.after.startedAt && association.after.startedAt <= association.after.completedAt
      && association.after.completedAt <= after.startedAt);
  }
  require(inspections > 0 && operations.size === 0 && handoffs === planned.length && handoffs === (identity.suite === 'release' ? 1 : 0));
  for (const name of ['measurement-start', 'paired', 'candidate', 'scenario-complete']) require(checkpoints.has(name));
  if (identity.suite === 'release') require(checkpoints.has('lifecycle-complete'));
}

export function publicMobileSummary(result: MobileResultContract, check?: AndroidEnvironmentCheck): Record<string, unknown> {
  validateMeasurementIdentity(result.identity);
  if (!['PASS', 'FAIL', 'INDETERMINATE'].includes(result.product?.status) || !['PASS', 'FAIL'].includes(result.finalization?.status)) throw new Error('MOBILE_SUMMARY: invalid statuses');
  const environment = result.environment_qualification;
  const status = environment.platform === 'ios' ? environment.applicability : environment.status;
  if (!['PASS', 'FAIL', 'UNKNOWN', 'NOT_APPLICABLE'].includes(status)) throw new Error('MOBILE_SUMMARY: invalid applicability');
  const allowed = new Set(['GMS_COMPONENT_DRIFT', 'IDENTITY_DEVIATION', 'ADVERSE_DEATH', 'RETIREMENT_PROOF_UNKNOWN', 'MODULE_CHANGE', 'PACKAGE_REPLACEMENT', 'EVIDENCE_INVALID']);
  const findings = check?.findings || [];
  const projected = findings.slice(0, 64).map(finding => ({ category: allowed.has(finding.category) ? finding.category : 'EVIDENCE_INVALID',
    ...(Number.isSafeInteger(finding.eventIndex) && finding.eventIndex! >= 0 ? { eventOrdinal: finding.eventIndex! + 1 } : {}),
    ...(['com.google.android.gms', 'com.android.chrome', 'com.google.android.trichromelibrary'].includes(finding.packageName || '') ? { packageName: finding.packageName } : {}),
  }));
  const packageNames = ['com.google.android.gms', 'com.android.chrome', 'com.google.android.trichromelibrary'];
  const components = (check?.componentDeltas || []).slice(0, 6).filter(delta => packageNames.includes(delta.packageName) && ['enabledComponents', 'disabledComponents'].includes(delta.field)).map(delta => {
    const project = (field: typeof delta.before) => {
      if (!Number.isSafeInteger(field.total) || field.total < 0 || !Array.isArray(field.names)) throw new Error('MOBILE_SUMMARY: component counts invalid');
      const names = field.names.filter(name => typeof name === 'string' && /^(?:com\.google\.android\.gms|com\.android\.chrome|com\.google\.android\.trichromelibrary)\.[A-Za-z0-9_.$]+$/u.test(name) && new TextEncoder().encode(name).length <= 256).slice(0, 128);
      if (names.length > field.total) throw new Error('MOBILE_SUMMARY: component counts inconsistent');
      return { total: field.total, names, withheldCount: field.total - names.length };
    };
    return { packageName: delta.packageName, field: delta.field, before: project(delta.before), after: project(delta.after) };
  });
  const events = (check?.nativeEvents || []).slice(0, 64).map((event, index) => ({ ordinal: index + 1,
    pid: /^[1-9]\d*$/u.test(event.pid || '') ? event.pid : undefined,
    signal: Number(event.line.match(/\b(?:SIG: |signal )(\d+)/u)?.[1]) || undefined,
    exit: event.line.match(/exited cleanly \((\d+)\)/u)?.[1],
    relation: 'NOT_ESTABLISHED_BY_EVENT_ALONE',
  }));
  const summary = {
    runId: result.identity.runId, attempt: result.identity.attempt, suite: result.identity.suite, platform: result.identity.platform,
    baseline: result.identity.baseline, scenario: result.identity.scenario, sourceCommit: result.identity.sourceCommit,
    sourceRunHeadSha: result.identity.sourceRunHeadSha, candidateWebHash: result.identity.candidateWebHash, candidateBuild: result.identity.candidateBuild,
    product: result.product.status, finalization: result.finalization.status, environment: status,
    collection: environment.platform === 'android' ? environment.collection : 'NOT_APPLICABLE',
    findings: projected, omittedFindings: Math.max(0, findings.length - projected.length), components, events,
    componentDeltaCount: check?.componentDeltas.length || 0, omittedComponentDeltas: Math.max(0, (check?.componentDeltas.length || 0) - components.length),
    omittedEvents: Math.max(0, (check?.nativeEvents.length || 0) - events.length),
    fatalEvents: Number.isSafeInteger(check?.eventCounts.fatalEvents) && check!.eventCounts.fatalEvents >= 0 ? check!.eventCounts.fatalEvents : null,
  };
  for (const delta of [...components].reverse()) for (const field of [delta.after, delta.before]) {
    while (field.names.length && new TextEncoder().encode(JSON.stringify(summary)).length > 60_000) { field.names.pop(); field.withheldCount++; }
  }
  if (new TextEncoder().encode(JSON.stringify(summary)).length > 65_536) throw new Error('MOBILE_SUMMARY: projection exceeds bound');
  return summary;
}

function ownedEventRelation(check: AndroidEnvironmentCheck, observations: ProductObservations, eventOrdinal: number): { owner: number; inspectionOrdinal: number; startLine: number; endLine: number; clock: string } | undefined {
  const proof = check.processBoundaryProof;
  const event = check.nativeEvents[eventOrdinal - 1];
  if (!proof || check.collection.status !== 'PASS' || check.boundaryDiscordances.length || event?.kind !== 'process-death' || !event.pid) return undefined;
  const eventPoint = proof.events.find(record => record.eventOrdinal === eventOrdinal);
  if (!eventPoint) return undefined;
  const segments = [observations.initialOwner, ...observations.records.filter(record => record.kind === 'cold-handoff').map(record => record.owner)].map(owner => ({
    owner, inspection: observations.records.find(record => record.kind === 'inspection' && record.owner === owner),
  }));
  const matching = segments.filter(segment => segment.inspection?.native?.pid === event.pid);
  if (matching.length !== 1) return undefined;
  const segment = matching[0], index = segments.indexOf(segment);
  const start = index ? proof.operations[index - 1]?.end : proof.start;
  const end = proof.operations[index]?.begin || proof.end;
  const between = (point: { line: number; time: number }) => start && point.line > start.line && point.line < end.line && point.time > start.time && point.time < end.time;
  if (!start || !between(eventPoint)) return undefined;
  const births = proof.births.filter(birth => birth.pid === event.pid && birth.line > proof.start.line && birth.time > proof.start.time && birth.line < proof.end.line && birth.time < proof.end.time);
  if (!index) {
    if (proof.before[event.pid] !== 'com.android.chrome' || births.length) return undefined;
  } else {
    const termination = check.plannedOperations[index - 1];
    if (proof.before[event.pid] || termination?.processes[event.pid] || proof.after[event.pid] || births.length !== 1) return undefined;
    const birth = births[0];
    if (birth.name !== 'com.android.chrome' || !between(birth) || birth.line >= eventPoint.line || birth.time >= eventPoint.time) return undefined;
  }
  if (event.processName && event.processName !== 'com.android.chrome') return undefined;
  return { owner: segment.owner, inspectionOrdinal: segment.inspection!.ordinal, startLine: start.line, endLine: end.line, clock: proof.clock };
}

export function androidProductImpact(check: AndroidEnvironmentCheck, observations?: ProductObservations): { status: ProductStatus; categories: string[]; relations: Array<{ eventOrdinal: number; relation: 'OWNED_BROWSER' | 'UNRESOLVED'; coverage: 'bounded-operations'; initiator: 'KNOWN' | 'UNKNOWN'; proof?: ReturnType<typeof ownedEventRelation> }> } {
  const categories: string[] = [];
  const statuses: ProductStatus[] = [];
  const relations: ReturnType<typeof androidProductImpact>['relations'] = [];
  let trusted = false;
  if (!observations || observations.failure) { statuses.push('FAIL'); categories.push(observations?.failure?.category || 'OBSERVATIONS_MISSING'); }
  else {
    try { validateProductObservations(observations, check.identity, check.plannedOperations); trusted = true; }
    catch { statuses.push('FAIL'); categories.push('OBSERVATION_INVALID'); }
  }
  for (const finding of check.findings) {
    if (finding.category === 'GMS_COMPONENT_DRIFT') continue;
    if (finding.category === 'RETIREMENT_PROOF_UNKNOWN' || finding.category === 'ADVERSE_DEATH') {
      const event = finding.eventIndex === undefined ? undefined : check.nativeEvents[finding.eventIndex];
      const proof = trusted && finding.eventIndex !== undefined ? ownedEventRelation(check, observations!, finding.eventIndex + 1) : undefined;
      const owned = Boolean(proof);
      if (finding.eventIndex !== undefined) relations.push({ eventOrdinal: finding.eventIndex + 1, relation: owned ? 'OWNED_BROWSER' : 'UNRESOLVED', coverage: 'bounded-operations', initiator: event?.initiatorPid ? 'KNOWN' : 'UNKNOWN', ...(proof ? { proof } : {}) });
      statuses.push(owned ? 'FAIL' : 'INDETERMINATE'); categories.push(owned ? 'OWNED_BROWSER_LOSS' : 'TARGET_RELATION_UNRESOLVED');
    } else { statuses.push('FAIL'); categories.push(finding.category); }
  }
  return { status: productStatus(statuses), categories: [...new Set(categories)], relations };
}
