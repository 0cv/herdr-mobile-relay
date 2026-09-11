import type {AndroidDriver} from 'appium-android-driver';
import type {Chromedriver} from 'appium-chromedriver';
import type {DocumentObservation, InspectionResult, OwnerSnapshot} from './target-inspection.cjs';

export interface RetainedInspectionInput {
  deadline: number;
}
export interface NativeObservation {
  pid: string;
  startTime: string;
  bootId: string;
  namespace: 'reader-and-browser-active-in-procfs-mount-pid-namespace';
  startedAt: number;
  finishedAt: number;
  activity: string;
  provider: 'browser' | 'android-standalone';
}
export interface RetainedSnapshot extends OwnerSnapshot {
  document: DocumentObservation;
  native: NativeObservation;
  nativeBefore: NativeObservation;
  forward: {serial: string; port: number; socket: 'chrome_devtools_remote'; inode: string; browserVersion: string};
  startedAt: number;
  finishedAt: number;
}
export interface RetainedInspectionResult extends InspectionResult {
  phase: 'initial-browser-selected' | 'installed-selected';
  original: {pid: string; startTime: string; bootId: string; namespace: 'reader-and-browser-active-in-procfs-mount-pid-namespace'; startedAt: number; finishedAt: number; serial: string; sessionId: string; chromeSessionId: string};
  before: RetainedSnapshot;
  after: RetainedSnapshot;
}

export function installRetainedInspection(
  driver: AndroidDriver,
  owner: Chromedriver,
  requireOwner: () => Chromedriver,
  quarantine: () => void,
): Promise<void>;
