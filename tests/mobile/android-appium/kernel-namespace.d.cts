import type {createAdbInspection} from './adb-inspection.cjs';
export interface KernelCapability {
  mode: 'enabled' | 'disabled';
  source: '/proc/config.gz';
  sha256: string;
  compressedBytes: number;
  configBytes: number;
  compressedLimit: 262144;
  configLimit: 2097152;
  bootId: string;
  acquiredStartedAt: number;
  acquiredFinishedAt: number;
}
export type NativeNamespace = 'reader-and-browser-active-in-procfs-mount-pid-namespace' | 'kernel-pid-namespaces-disabled';
export function acquireKernelCapability(reader: ReturnType<typeof createAdbInspection>, deadline: number, check: () => void): Promise<KernelCapability>;
export function namespaceForCapability(capability: KernelCapability): NativeNamespace;
export function validateNamespaceStatus(text: string, expectedPid: string | undefined, capability: KernelCapability): void;
export function validKernelCapability(capability: KernelCapability, bootId: string, end: number): boolean;
export function sameKernelCapability(a: KernelCapability, b: KernelCapability): boolean;
