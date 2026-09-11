export interface AssociationSnapshot {
  endpoint: {host: '127.0.0.1'; port: number; browserPath: string};
  browserVersion: string;
}
export interface OwnerSnapshot extends AssociationSnapshot {
  handles: string[];
  selectedHandle: string;
}
export interface DocumentObservation {
  href: string;
  origin: string;
  standalone: boolean;
  provider: 'android-standalone' | 'browser';
  timeOrigin: number;
  backendNodeId: number;
}
export interface ProcessObservation {
  pid: number;
  requestId: number;
  connectionId: string;
  startedAt: number;
  completedAt: number;
}
export interface ProcessAssociation {
  kind: 'bounded-sequential-service-to-process';
  before: ProcessObservation;
  after: ProcessObservation;
}
export interface AssociationResult {
  kind: 'bounded-browser-process-association';
  processAssociation: ProcessAssociation;
}
export interface InspectionResult {
  processAssociation: ProcessAssociation;
  kind: 'bounded-nonactivating-observation';
  targets: Array<{targetId: string; type: string; url: string; title: string}>;
  observations: Array<{targetId: string; document: DocumentObservation}>;
  selectedHandle: string;
}
export interface AssociationContract<Owner extends object, Snapshot extends AssociationSnapshot> {
  /** Original independent native capture, never learned from CDP; integer in 1..2147483647. */
  expectedBrowserPid: number;
  deadline: number;
  assertOwner(owner: Owner): void;
  failure(error: Error): void;
  snapshot(owner: Owner): Promise<Snapshot>;
  /** Fresh original PID/exact starttime and native endpoint/forward bounds in the trusted same PID namespace. */
  validateSnapshot(owner: Owner, before: Snapshot, after: Snapshot): Promise<void>;
}
export interface InspectionContract<Owner extends object, Snapshot extends OwnerSnapshot> extends AssociationContract<Owner, Snapshot> {
  validateSelectedDocument(owner: Owner, result: InspectionResult, before: Snapshot, after: Snapshot): Promise<void>;
}
export function associateBrowserProcess<Owner extends object, Snapshot extends AssociationSnapshot>(owner: Owner, contract: AssociationContract<Owner, Snapshot>): Promise<AssociationResult>;
export function inspectTargets<Owner extends object, Snapshot extends OwnerSnapshot>(owner: Owner, contract: InspectionContract<Owner, Snapshot>): Promise<InspectionResult>;
