import { createHash } from 'node:crypto';

/**
 * @param {string | Uint8Array} source
 * @returns {{ sha256: string, integrity: string }}
 */
export function assetIdentity(source) {
  const hash = createHash('sha256').update(source).digest();
  return {
    sha256: hash.toString('hex'),
    integrity: `sha256-${hash.toString('base64')}`,
  };
}
