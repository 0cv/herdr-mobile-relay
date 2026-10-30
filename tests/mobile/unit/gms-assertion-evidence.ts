export const gmsPersistenceTestTitle = 'Android production CLI timed-out GMS persistence retires its file worker before finalization returns';
export const gmsPersistenceTestID = 'outer:cb5da75c7dbaeebd:1';
export const gmsPersistenceAssertionIDs = Object.freeze(['GMS-P01', 'GMS-P02', 'GMS-P03', 'GMS-P04', 'GMS-P05', 'GMS-P06',
  'GMS-P07', 'GMS-P08', 'GMS-P09', 'GMS-P10', 'GMS-P11', 'GMS-P12', 'GMS-P13', 'GMS-P14', 'GMS-P15',
  'GMS-P16', 'GMS-P17', 'GMS-P18', 'GMS-P19', 'GMS-P20', 'GMS-P21'] as const);
export type GmsPersistenceAssertionID = typeof gmsPersistenceAssertionIDs[number];
type AssertionRecord = { id: GmsPersistenceAssertionID; state: 'unexecuted' | 'pending' | 'passed' | 'failed';
  evidence: Record<string, boolean | number | string> };

const marker = 'GMS_ASSERTIONS: ';
const invalidEvidence = `${marker}{"state":"invalid"}`;
const maximumAssertionBytes = 8_192;
const countFields = ['actual', 'expected', 'byteLength', 'expectedByteLength'];
const observationFields = ['observed', 'bytesEqual', 'hashesEqual'];
const classifications = ['unknown', 'retired', 'pending-temporary-write', 'removed-after-failure', 'pending', 'not-needed', 'unconfirmed', 'confirmed'];
const evidenceFields = ['assertionPassed', ...countFields, ...observationFields, 'classification'];

function invalid(): never {
  throw new Error('GMS assertion evidence is invalid');
}

function data(value: unknown, key: string): unknown {
  if (!value || typeof value !== 'object' || Array.isArray(value)) invalid();
  const descriptor = Object.getOwnPropertyDescriptor(value, key);
  if (descriptor && !('value' in descriptor)) invalid();
  return descriptor?.value;
}

function validCount(value: unknown): boolean {
  return value === 'unknown' || (typeof value === 'number' && Number.isSafeInteger(value) && value >= 0);
}

function validObservation(value: unknown): boolean {
  return value === 'unknown' || typeof value === 'boolean';
}

function serializeEvidence(value: unknown, serialize: (value: unknown) => string): string {
  const expected = JSON.stringify(value);
  const text = serialize === JSON.stringify ? expected : serialize(value);
  if (text !== expected || Buffer.byteLength(text) > maximumAssertionBytes) invalid();
  return `${marker}${text}`;
}

export function createGmsSuccessCertificate(input: unknown, serialize: (value: unknown) => string = JSON.stringify): string {
  if (!Array.isArray(input) || input.length !== gmsPersistenceAssertionIDs.length) invalid();
  const canonical = gmsPersistenceAssertionIDs.map((id, index) => {
    const descriptor = Object.getOwnPropertyDescriptor(input, String(index));
    if (!descriptor || !('value' in descriptor)) invalid();
    const record: unknown = descriptor.value;
    const recordID = data(record, 'id');
    const state = data(record, 'state');
    if (recordID !== id || state !== 'passed') invalid();
    const evidence = data(record, 'evidence');
    if (!evidence || typeof evidence !== 'object' || Array.isArray(evidence) || Object.getOwnPropertySymbols(evidence).length) invalid();
    const assertionPassed = data(evidence, 'assertionPassed');
    if (assertionPassed !== true) invalid();
    for (const key of Object.getOwnPropertyNames(evidence)) {
      if (!evidenceFields.includes(key)) invalid();
      const value = key === 'assertionPassed' ? assertionPassed : data(evidence, key);
      if (countFields.includes(key) && !validCount(value)) invalid();
      if (observationFields.includes(key) && !validObservation(value)) invalid();
      if (key === 'classification' && (typeof value !== 'string' || !classifications.includes(value))) invalid();
    }
    return { id: recordID, state, evidence: { assertionPassed } };
  });
  return serializeEvidence(canonical, serialize);
}

export const gmsPersistenceExpectedSuccessLine = `${marker}${JSON.stringify(gmsPersistenceAssertionIDs.map(id =>
  ({ id, state: 'passed', evidence: { assertionPassed: true } })))}`;

export function createGmsPersistenceAssertions() {
  const records: AssertionRecord[] = gmsPersistenceAssertionIDs.map(id => ({ id, state: 'unexecuted', evidence: {} }));
  const entered: AssertionRecord[] = [];
  const settled: AssertionRecord[] = [];
  const faults: unknown[] = [];
  let active: AssertionRecord | undefined;
  let projection: (() => Record<string, unknown>) | undefined;
  let invalidExecution = false;
  let publicationAttempted = false;
  const settle = (state: 'passed' | 'failed') => {
    if (!active) { invalidExecution = true; return; }
    const current = active;
    active = undefined;
    current.state = state;
    current.evidence.assertionPassed = state === 'passed';
    settled.push(current);
    try {
      const values = projection ? projection() : {};
      const assertionPassed = data(values, 'assertionPassed');
      if (assertionPassed !== undefined && assertionPassed !== (state === 'passed')) invalid();
      for (const key of countFields) {
        const value = data(values, key);
        current.evidence[key] = value === undefined ? 'unknown' : validCount(value) ? value as number | string : 'invalid';
      }
      for (const key of observationFields) {
        const value = data(values, key);
        current.evidence[key] = value === undefined ? 'unknown' : validObservation(value) ? value as boolean | string : 'invalid';
      }
      const classification = data(values, 'classification');
      current.evidence.classification = classification === undefined ? 'unknown'
        : typeof classification === 'string' && classifications.includes(classification) ? classification : 'invalid';
    } catch (error) {
      faults.push(error);
      current.evidence.projection = 'invalid';
    }
    projection = undefined;
  };
  const certificate = (serialize: (value: unknown) => string = JSON.stringify): string => {
    if (invalidExecution || active || faults.length || entered.length !== records.length || settled.length !== records.length
      || entered.some((record, index) => record !== settled[index])) invalid();
    return createGmsSuccessCertificate(entered, serialize);
  };
  return {
    enter(id: GmsPersistenceAssertionID, evidence?: () => Record<string, unknown>): void {
      if (publicationAttempted || active || entered.length >= records.length || id !== gmsPersistenceAssertionIDs[entered.length]) invalidExecution = true;
      const record: AssertionRecord = { id, state: 'pending', evidence: {} };
      const index = gmsPersistenceAssertionIDs.indexOf(id);
      if (index < 0 || records[index]!.state !== 'unexecuted') invalidExecution = true;
      else records[index] = record;
      entered.push(record);
      active = record;
      projection = evidence;
    },
    passed(): void { settle('passed'); },
    failed(): void { settle('failed'); },
    faults(): unknown[] { return [...faults]; },
    render(serialize: (value: unknown) => string = JSON.stringify): string {
      try {
        const diagnostic = records.map(record => ({ ...record, evidence: { ...record.evidence } }));
        if (invalidExecution) diagnostic[0]!.evidence.execution = 'invalid';
        return serializeEvidence(diagnostic, serialize);
      }
      catch (error) { faults.push(error); return invalidEvidence; }
    },
    certificate,
    async publish(publish: (line: string) => Promise<void>, serialize: (value: unknown) => string = JSON.stringify): Promise<void> {
      if (publicationAttempted) invalid();
      publicationAttempted = true;
      await publish(`${certificate(serialize)}\n`);
    },
  };
}
