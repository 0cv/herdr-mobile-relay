import { readFile } from 'node:fs/promises';

export async function requireOwnedDevice(platform: string, identifier: string): Promise<void> {
  const marker = process.env.MOBILE_DEVICE_OWNERSHIP_FILE || '';
  if (!marker || !identifier) throw new Error(`${platform.toUpperCase()}_TARGET: disposable ownership marker is required`);
  const value = await readFile(marker, 'utf8').catch(() => '');
  if (value.trim() !== `${platform}:${identifier}`) {
    throw new Error(`${platform.toUpperCase()}_TARGET: device is not owned by this run`);
  }
}
