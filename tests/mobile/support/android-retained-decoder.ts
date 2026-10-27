import type { RetainedInspectionResult, NativeObservation } from '../android-appium/retained-inspection.cjs';
import type { CoreDocumentObservation, SelectedDocumentObservation } from '../android-appium/target-inspection.cjs';
import { isAndroidPersistentWebAppActivity } from './oracle';
import { namespaceForCapability, sameKernelCapability, validKernelCapability, type KernelCapability, type NativeNamespace } from '../android-appium/kernel-namespace.cjs';

export const nativeNamespace = 'reader-and-browser-active-in-procfs-mount-pid-namespace' as const;
export interface RetainedNativeIdentity { pid: string; startTime: string; bootId: string; namespace: NativeNamespace; kernelCapability: KernelCapability }

export function decodeRetainedInspection(value: unknown, owner: RetainedNativeIdentity, bounds: {
  startedAt: number; deadline: number; finishedAt: number; serial: string; scope: string; selectedHandle: string;
  assertSession: (session: string) => void;
}): { result: RetainedInspectionResult; candidate: string } {
  const require = (condition: unknown): void => { if (!condition) throw new Error('Invalid bounded retained inspection'); };
  const object = (item: unknown): Record<string, any> => {
    require(item !== null && typeof item === 'object' && !Array.isArray(item));
    return item as Record<string, any>;
  };
  const exact = (item: Record<string, any>, keys: string[]): void => {
    require(JSON.stringify(Object.keys(item).sort()) === JSON.stringify([...keys].sort()));
  };
  const text = (item: unknown): boolean => typeof item === 'string' && item.length > 0;
  const integer = (item: unknown): boolean => Number.isSafeInteger(item) && Number(item) > 0;
  const interval = (item: Record<string, any>, start: number, end: number): void => {
    require(integer(item.startedAt) && integer(item.finishedAt) && start <= item.startedAt && item.startedAt <= item.finishedAt && item.finishedAt <= end);
  };
  const same = (a: unknown, b: unknown): boolean => JSON.stringify(a) === JSON.stringify(b);
  const selectedCore = (document: Record<string, any>): CoreDocumentObservation => ({ href: document.href, origin: document.origin, timeOrigin: document.timeOrigin, backendNodeId: document.backendNodeId });
  const coreDocument = (raw: unknown): CoreDocumentObservation => {
    const d = object(raw);
    exact(d, ['href', 'origin', 'timeOrigin', 'backendNodeId']);
    require(text(d.href) && text(d.origin) && integer(d.backendNodeId)
      && typeof d.timeOrigin === 'number' && Number.isFinite(d.timeOrigin) && d.timeOrigin > 0 && d.timeOrigin <= bounds.finishedAt);
    require(new URL(d.href).origin === d.origin);
    return { href: d.href, origin: d.origin, timeOrigin: d.timeOrigin, backendNodeId: d.backendNodeId };
  };
  const selectedDocument = (raw: unknown): SelectedDocumentObservation => {
    const d = object(raw);
    exact(d, ['href', 'origin', 'standalone', 'provider', 'timeOrigin', 'backendNodeId']);
    const core = coreDocument({ href: d.href, origin: d.origin, timeOrigin: d.timeOrigin, backendNodeId: d.backendNodeId });
    require(typeof d.standalone === 'boolean' && d.provider === (d.standalone ? 'android-standalone' : 'browser'));
    return { ...core, standalone: d.standalone, provider: d.provider };
  };
  const r = object(value);
  exact(r, ['kind', 'processAssociation', 'targets', 'observations', 'selectedHandle', 'phase', 'original', 'before', 'after']);
  require(r.kind === 'bounded-nonactivating-core-observation');
  const original = object(r.original);
  require(original.pid === owner.pid && original.startTime === owner.startTime && original.bootId === owner.bootId && original.namespace === owner.namespace
    && original.serial === bounds.serial && text(original.chromeSessionId) && text(original.sessionId));
  interval(original, 1, bounds.startedAt);
  require(validKernelCapability(owner.kernelCapability, owner.bootId, bounds.startedAt)
    && validKernelCapability(original.kernelCapability, owner.bootId, original.finishedAt)
    && original.startedAt <= original.kernelCapability.acquiredStartedAt
    && sameKernelCapability(original.kernelCapability, owner.kernelCapability)
    && original.namespace === namespaceForCapability(original.kernelCapability));
  bounds.assertSession(original.sessionId);
  require(bounds.finishedAt <= bounds.deadline);
  const native = (raw: unknown, start: number, end: number): NativeObservation => {
    const n = object(raw);
    interval(n, start, end);
    require(validKernelCapability(n.kernelCapability, owner.bootId, n.finishedAt) && same(n.kernelCapability, original.kernelCapability));
    require(n.pid === owner.pid && n.startTime === owner.startTime && n.bootId === owner.bootId && n.namespace === owner.namespace
      && n.provider === 'android-standalone' && isAndroidPersistentWebAppActivity(n.activity));
    return n as NativeObservation;
  };
  const snapshots = [object(r.before), object(r.after)];
  for (const s of snapshots) {
    exact(s, ['endpoint', 'browserVersion', 'handles', 'selectedHandle', 'document', 'native', 'nativeBefore', 'forward', 'startedAt', 'finishedAt']);
    interval(s, bounds.startedAt, bounds.finishedAt);
    const first = native(s.nativeBefore, s.startedAt, s.finishedAt);
    const last = native(s.native, s.startedAt, s.finishedAt);
    const selected = selectedDocument(s.document);
    require(first.finishedAt <= last.startedAt && first.activity === last.activity);
    require(Array.isArray(s.handles) && s.handles.length > 0 && s.handles.length <= 32
      && s.handles.every((h: unknown) => typeof h === 'string' && /^[a-fA-F0-9]{32}$/u.test(h)) && new Set(s.handles).size === s.handles.length
      && s.handles.includes(s.selectedHandle) && s.selectedHandle === r.selectedHandle);
    const endpoint = object(s.endpoint);
    const forward = object(s.forward);
    require(endpoint.host === '127.0.0.1' && integer(endpoint.port) && endpoint.port <= 65535 && typeof endpoint.browserPath === 'string'
      && /^\/devtools\/browser(?:\/[A-Za-z0-9-]+)?$/u.test(endpoint.browserPath)
      && s.browserVersion === 'Chrome/131.0.6778.200' && forward.browserVersion === s.browserVersion
      && forward.serial === bounds.serial && forward.port === endpoint.port && forward.socket === 'chrome_devtools_remote'
      && typeof forward.inode === 'string' && /^[1-9]\d*$/u.test(forward.inode));
    require(selected.origin === new URL(bounds.scope).origin);
  }
  const [before, after] = snapshots;
  const selected = selectedDocument(before.document);
  require(before.finishedAt <= after.startedAt && same(before.endpoint, after.endpoint) && same(before.forward, after.forward)
    && same([...before.handles].sort(), [...after.handles].sort()) && same(selected, selectedDocument(after.document))
    && before.native.activity === after.native.activity && before.native.provider === after.native.provider);
  const association = object(r.processAssociation);
  exact(association, ['kind', 'before', 'after']);
  require(association.kind === 'bounded-sequential-service-to-process');
  const first = object(association.before);
  const last = object(association.after);
  for (const p of [first, last]) require(p.pid === Number(owner.pid) && integer(p.requestId) && typeof p.connectionId === 'string' && /^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/u.test(p.connectionId)
    && integer(p.startedAt) && integer(p.completedAt) && p.startedAt <= p.completedAt);
  require(first.requestId === 1 && last.requestId > first.requestId && first.connectionId === last.connectionId
    && before.finishedAt <= first.startedAt && first.completedAt <= last.startedAt && last.completedAt <= after.startedAt);
  require(Array.isArray(r.targets) && r.targets.length > 0 && r.targets.length <= 32 && Array.isArray(r.observations));
  const ids = new Set<string>();
  for (const raw of r.targets) {
    const t = object(raw);
    exact(t, ['targetId', 'type', 'url', 'title']);
    require((t.type === 'page' || t.type === 'service_worker') && text(t.targetId) && t.targetId.length <= 8192
      && (t.type !== 'page' || /^[a-fA-F0-9]{32}$/u.test(t.targetId))
      && typeof t.url === 'string' && typeof t.title === 'string' && !ids.has(t.targetId));
    ids.add(t.targetId);
  }
  const pages = new Set<string>(r.targets.filter((t: Record<string, any>) => t.type === 'page').map((t: Record<string, any>) => t.targetId));
  require(same([...pages].sort(), [...before.handles].sort()) && r.observations.length === pages.size && last.requestId === 4 + 7 * pages.size);
  const observed = new Set<string>();
  const candidates: string[] = [];
  let scope: URL;
  try { scope = new URL(bounds.scope); } catch { require(false); }
  const inScope = (href: string): boolean => {
    try {
      const url = new URL(href);
      return url.origin === scope.origin && url.pathname.startsWith(scope.pathname) && !url.username && !url.password;
    } catch {
      return false;
    }
  };
  for (const raw of r.observations) {
    const o = object(raw);
    exact(o, ['targetId', 'document']);
    require(pages.has(o.targetId) && !observed.has(o.targetId));
    observed.add(o.targetId);
    const d = coreDocument(o.document);
    const target = r.targets.find((t: Record<string, any>) => t.targetId === o.targetId);
    require(target && target.url === d.href);
    if (o.targetId === r.selectedHandle) require(same(d, selectedCore(before.document)));
    if (o.targetId !== r.selectedHandle && inScope(d.href)) candidates.push(o.targetId);
  }
  require(observed.size === pages.size && inScope(selected.href));
  let candidate: string;
  if (selected.standalone) {
    require(r.phase === 'installed-selected' && before.native.provider === 'android-standalone' && after.native.provider === 'android-standalone');
    candidate = r.selectedHandle;
  } else {
    require(r.phase === 'initial-browser-selected' && !bounds.selectedHandle && candidates.length === 1);
    candidate = candidates[0];
  }
  if (bounds.selectedHandle) require(candidate === bounds.selectedHandle && r.selectedHandle === bounds.selectedHandle && r.phase === 'installed-selected');
  return { result: r as RetainedInspectionResult, candidate };
}
