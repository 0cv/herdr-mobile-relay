import { createHash } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import { assetIdentity } from '../../scripts/asset-identity.mjs';

const empty = {
  sha256: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
  integrity: 'sha256-47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=',
};
const abc = {
  sha256: 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad',
  integrity: 'sha256-ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0=',
};

describe('asset identity', () => {
  it.each([['', empty], ['abc', abc]] as const)('matches the fixed %j SHA-256 and padded SRI vector', (source, expected) => {
    expect(assetIdentity(source)).toEqual(expected);
  });

  it('hashes strings as their explicit UTF-8 bytes without normalization', () => {
    const bytes = new Uint8Array([0xc3, 0xa9, 0xe9, 0x9b, 0xaa, 0xf0, 0x9f, 0x8c, 0xbf]);
    expect(Array.from(new TextEncoder().encode('é雪🌿'))).toEqual(Array.from(bytes));
    const digest = createHash('sha256').update(bytes).digest();
    const expected = { sha256: digest.toString('hex'), integrity: `sha256-${digest.toString('base64')}` };
    expect(assetIdentity('é雪🌿')).toEqual(expected);
    expect(assetIdentity(bytes)).toEqual(expected);
    expect(assetIdentity('é')).not.toEqual(assetIdentity('e\u0301'));
  });

  it.each(['Uint8Array', 'Buffer'] as const)('uses only the nonzero-offset, limited-length %s view', (kind) => {
    const backing = kind === 'Buffer' ? Buffer.from([255, 97, 98, 99, 128]) : new Uint8Array([255, 97, 98, 99, 128]);
    const before = Array.from(backing);
    const view = backing.subarray(1, 4);
    expect(view.byteOffset).toBe(backing.byteOffset + 1);
    expect(view.byteLength).toBe(3);
    expect(assetIdentity(view)).toEqual(abc);
    expect(Array.from(backing)).toEqual(before);
  });

  it.each(['Uint8Array', 'Buffer'] as const)('hashes an empty %s view without including its backing storage', (kind) => {
    const backing = kind === 'Buffer' ? Buffer.from([255, 128, 7]) : new Uint8Array([255, 128, 7]);
    const before = Array.from(backing);
    const view = backing.subarray(2, 2);
    expect(view.byteOffset).toBe(backing.byteOffset + 2);
    expect(assetIdentity(view)).toEqual(empty);
    expect(Array.from(backing)).toEqual(before);
  });

  it('returns an ordinary fresh object with sha256 then integrity keys on every call', () => {
    const first = assetIdentity('abc');
    const second = assetIdentity('abc');
    expect(Object.getPrototypeOf(first)).toBe(Object.prototype);
    expect(Object.keys(first)).toEqual(['sha256', 'integrity']);
    expect(first).not.toBe(second);
    first.sha256 = 'changed';
    first.integrity = 'changed';
    expect(second).toEqual(abc);
    expect(assetIdentity('abc')).toEqual(abc);
  });

  it('does not mutate binary input or cache a previous view result', () => {
    const backing = new Uint8Array([0, 97, 98, 99, 255]);
    const view = new Uint8Array(backing.buffer, 1, 3);
    expect(assetIdentity(view)).toEqual(abc);
    expect(Array.from(backing)).toEqual([0, 97, 98, 99, 255]);
    view[0] = 100;
    const expected = createHash('sha256').update(new Uint8Array([100, 98, 99])).digest();
    expect(assetIdentity(view)).toEqual({
      sha256: expected.toString('hex'), integrity: `sha256-${expected.toString('base64')}`,
    });
    expect(Array.from(backing)).toEqual([0, 100, 98, 99, 255]);
  });
});
