interface AdbConnection {
  curDeviceId: string;
  executable: {defaultArgs: string[]};
  remoteAdbHost?: string;
  adbPort?: number;
  adbHost?: string;
  listenAllNetwork?: boolean;
  remoteAdbPort?: number;
}
export type AdbInspectionRead =
  | ['forward', '--list']
  | ['shell', 'pidof', 'com.android.chrome']
  | ['shell', 'dumpsys', 'activity', 'activities']
  | ['shell', 'cat', '/proc/net/unix' | '/proc/self/status' | '/proc/sys/kernel/random/boot_id' | `/proc/${number}/stat` | `/proc/${number}/status`];

export function createAdbInspection(adb: AdbConnection, check: () => void, fail: (error: Error) => Error): {
  cancel(error: Error): void;
  read(args: AdbInspectionRead, deadline: number): Promise<string>;
};
