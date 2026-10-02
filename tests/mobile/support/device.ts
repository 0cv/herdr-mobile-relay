import { readFile } from 'node:fs/promises';

export async function requireOwnedDevice(platform: string, identifier: string, signal?: AbortSignal): Promise<void> {
  const marker = process.env.MOBILE_DEVICE_OWNERSHIP_FILE || '';
  if (!marker || !identifier) throw new Error(`${platform.toUpperCase()}_TARGET: disposable ownership marker is required`);
  if (signal?.aborted) throw signal.reason || new Error('device ownership verification was aborted');
  const value = await readFile(marker, { encoding: 'utf8', signal }).catch((error: unknown) => {
    if (signal?.aborted) throw signal.reason || error;
    return '';
  });
  if (signal?.aborted) throw signal.reason || new Error('device ownership verification was aborted');
  if (value.trim() !== `${platform}:${identifier}`) {
    throw new Error(`${platform.toUpperCase()}_TARGET: device is not owned by this run`);
  }
}
