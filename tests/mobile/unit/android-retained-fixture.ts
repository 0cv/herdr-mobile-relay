import type { RetainedInspectionResult } from '../android-appium/retained-inspection.cjs';
import { nativeNamespace } from '../support/android-retained-decoder';
import { createHash } from 'node:crypto';
import { gzipSync } from 'node:zlib';
import type { KernelCapability } from '../android-appium/kernel-namespace.cjs';

export function kernelFixture(mode: 'enabled' | 'disabled' = 'enabled', time = 1): KernelCapability {
  const config = Buffer.from('CONFIG_IKCONFIG=y\nCONFIG_IKCONFIG_PROC=y\n' + (mode === 'enabled' ? 'CONFIG_PID_NS=y\n' : 'CONFIG_NAMESPACES=y\n# CONFIG_PID_NS is not set\n'));
  return {mode, source: '/proc/config.gz', sha256: createHash('sha256').update(config).digest('hex'),
    compressedBytes: gzipSync(config).length, configBytes: config.length, compressedLimit: 262144, configLimit: 2097152,
    bootId: '11111111-1111-1111-1111-111111111111', acquiredStartedAt: time, acquiredFinishedAt: time};
}

export function retainedFixture(handles: string[], selectedHandle: string, installed: string, startedAt: number): RetainedInspectionResult {
  const now = Date.now();
  const identity = { pid: '123', startTime: '456', bootId: '11111111-1111-1111-1111-111111111111', namespace: nativeNamespace, kernelCapability: kernelFixture('enabled', startedAt) };
  const observations = handles.map((targetId, index) => ({ targetId, document: {
    href: 'https://fixture.test/', origin: 'https://fixture.test', standalone: targetId === installed,
    provider: targetId === installed ? 'android-standalone' as const : 'browser' as const,
    timeOrigin: startedAt - 1, backendNodeId: index + 1,
  } }));
  const native = { ...identity, startedAt: now, finishedAt: now, activity: 'org.chromium.chrome.browser.webapps.WebappActivity', provider: 'android-standalone' as const };
  const snapshot = {
    endpoint: { host: '127.0.0.1' as const, port: 9222, browserPath: '/devtools/browser/original' },
    browserVersion: 'Chrome/131.0.6778.200', handles, selectedHandle,
    document: observations.find(o => o.targetId === selectedHandle)!.document,
    native: structuredClone(native), nativeBefore: structuredClone(native),
    forward: { serial: 'emulator-5554', port: 9222, socket: 'chrome_devtools_remote' as const, inode: '42', browserVersion: 'Chrome/131.0.6778.200' },
    startedAt: now, finishedAt: now,
  };
  return structuredClone({
    kind: 'bounded-nonactivating-observation', phase: selectedHandle === installed ? 'installed-selected' : 'initial-browser-selected',
    original: { ...identity, startedAt, finishedAt: startedAt, serial: 'emulator-5554', sessionId: 'original', chromeSessionId: 'backend-original' },
    before: structuredClone(snapshot), after: structuredClone(snapshot), observations, selectedHandle,
    targets: observations.map(o => ({ targetId: o.targetId, type: 'page', url: o.document.href, title: '' })),
    processAssociation: { kind: 'bounded-sequential-service-to-process',
      before: { pid: 123, requestId: 1, connectionId: '11111111-1111-4111-8111-111111111111', startedAt: now, completedAt: now },
      after: { pid: 123, requestId: 4 + 7 * handles.length, connectionId: '11111111-1111-4111-8111-111111111111', startedAt: now, completedAt: now } },
  });
}
