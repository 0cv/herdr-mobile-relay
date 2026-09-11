import type { RetainedInspectionResult, NativeObservation } from '../android-appium/retained-inspection.cjs';
import type { DocumentObservation } from '../android-appium/target-inspection.cjs';
import { isAndroidPersistentWebAppActivity } from './oracle';

export const nativeNamespace = 'reader-and-browser-active-in-procfs-mount-pid-namespace' as const;
export interface RetainedNativeIdentity { pid: string; startTime: string; bootId: string; namespace: typeof nativeNamespace }

export function decodeRetainedInspection(value: unknown, owner: RetainedNativeIdentity, bounds: {
  startedAt: number; deadline: number; finishedAt: number; serial: string; scope: string; selectedHandle: string;
  assertSession: (session: string) => void;
}): { result: RetainedInspectionResult; candidate: string } {
  const require = (condition: unknown): void => { if (!condition) throw new Error('Invalid bounded retained inspection'); };
  const object = (item: unknown): Record<string, any> => {
    require(item !== null && typeof item === 'object' && !Array.isArray(item));
    return item as Record<string, any>;
  };
  const text = (item: unknown): boolean => typeof item === 'string' && item.length > 0;
  const integer = (item: unknown): boolean => Number.isSafeInteger(item) && Number(item) > 0;
  const interval = (item: Record<string, any>, start: number, end: number): void => {
    require(integer(item.startedAt) && integer(item.finishedAt) && start <= item.startedAt && item.startedAt <= item.finishedAt && item.finishedAt <= end);
  };
  const same = (a: unknown, b: unknown): boolean => JSON.stringify(a) === JSON.stringify(b);
  const document = (raw: unknown): DocumentObservation => {
    const d = object(raw);
    require(text(d.href) && text(d.origin) && typeof d.standalone === 'boolean'
      && d.provider === (d.standalone ? 'android-standalone' : 'browser') && integer(d.backendNodeId)
      && typeof d.timeOrigin === 'number' && Number.isFinite(d.timeOrigin) && d.timeOrigin > 0 && d.timeOrigin <= bounds.finishedAt);
    require(new URL(d.href).origin === d.origin);
    return { href: d.href, origin: d.origin, standalone: d.standalone, provider: d.provider, timeOrigin: d.timeOrigin, backendNodeId: d.backendNodeId };
  };
  const r = object(value);
  require(r.kind === 'bounded-nonactivating-observation');
  const original = object(r.original);
  require(original.pid === owner.pid && original.startTime === owner.startTime && original.bootId === owner.bootId && original.namespace === owner.namespace
    && original.serial === bounds.serial && text(original.chromeSessionId) && text(original.sessionId));
  interval(original, 1, bounds.startedAt);
  bounds.assertSession(original.sessionId);
  require(bounds.finishedAt <= bounds.deadline);
  const native = (raw: unknown, start: number, end: number): NativeObservation => {
    const n = object(raw);
    interval(n, start, end);
    require(n.pid === owner.pid && n.startTime === owner.startTime && n.bootId === owner.bootId && n.namespace === owner.namespace
      && n.provider === 'android-standalone' && isAndroidPersistentWebAppActivity(n.activity));
    return n as NativeObservation;
  };
  const snapshots = [object(r.before), object(r.after)];
  for (const s of snapshots) {
    interval(s, bounds.startedAt, bounds.finishedAt);
    const first = native(s.nativeBefore, s.startedAt, s.finishedAt);
    const last = native(s.native, s.startedAt, s.finishedAt);
    require(first.finishedAt <= last.startedAt && first.activity === last.activity);
    require(Array.isArray(s.handles) && s.handles.length > 0 && s.handles.length <= 32
      && s.handles.every((h: unknown) => typeof h === 'string' && /^[a-fA-F0-9]{32}$/u.test(h)) && new Set(s.handles).size === s.handles.length
      && s.handles.includes(s.selectedHandle) && s.selectedHandle === r.selectedHandle);
    document(s.document);
    const endpoint = object(s.endpoint);
    const forward = object(s.forward);
    require(endpoint.host === '127.0.0.1' && integer(endpoint.port) && endpoint.port <= 65535 && typeof endpoint.browserPath === 'string'
      && /^\/devtools\/browser(?:\/[A-Za-z0-9-]+)?$/u.test(endpoint.browserPath)
      && s.browserVersion === 'Chrome/131.0.6778.200' && forward.browserVersion === s.browserVersion
      && forward.serial === bounds.serial && forward.port === endpoint.port && forward.socket === 'chrome_devtools_remote'
      && typeof forward.inode === 'string' && /^[1-9]\d*$/u.test(forward.inode));
  }
  const [before, after] = snapshots;
  require(before.finishedAt <= after.startedAt && same(before.endpoint, after.endpoint) && same(before.forward, after.forward)
    && same([...before.handles].sort(), [...after.handles].sort()) && same(document(before.document), document(after.document))
    && before.native.activity === after.native.activity);
  const association = object(r.processAssociation);
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
    require((t.type === 'page' || t.type === 'service_worker') && text(t.targetId) && t.targetId.length <= 8192
      && (t.type !== 'page' || /^[a-fA-F0-9]{32}$/u.test(t.targetId))
      && typeof t.url === 'string' && typeof t.title === 'string' && !ids.has(t.targetId));
    ids.add(t.targetId);
  }
  const pages = new Set<string>(r.targets.filter((t: Record<string, any>) => t.type === 'page').map((t: Record<string, any>) => t.targetId));
  require(same([...pages].sort(), [...before.handles].sort()) && r.observations.length === pages.size && last.requestId === 4 + 7 * pages.size);
  const observed = new Set<string>();
  const candidates: string[] = [];
  const scope = new URL(bounds.scope);
  for (const raw of r.observations) {
    const o = object(raw);
    require(pages.has(o.targetId) && !observed.has(o.targetId));
    observed.add(o.targetId);
    const d = document(o.document);
    require(r.targets.find((t: Record<string, any>) => t.targetId === o.targetId).url === d.href);
    if (o.targetId === r.selectedHandle) require(same(d, document(before.document)));
    if (!d.standalone) continue;
    const url = new URL(d.href);
    require(url.origin === scope.origin && url.pathname.startsWith(scope.pathname) && !url.username && !url.password);
    candidates.push(o.targetId);
  }
  require(candidates.length === 1);
  const candidate = candidates[0];
  require(r.phase === (before.document.standalone ? 'installed-selected' : 'initial-browser-selected'));
  if (bounds.selectedHandle) require(candidate === bounds.selectedHandle && r.selectedHandle === bounds.selectedHandle && r.phase === 'installed-selected');
  return { result: r as RetainedInspectionResult, candidate };
}
