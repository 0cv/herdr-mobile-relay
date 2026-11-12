import { describe, expect, it } from 'vitest';
import { base64Decode, base64Encode, base64UrlDecode, base64UrlEncode } from '$lib/base64url';

describe('base64url encoding', () => {
  it('keeps standard base64 padding and alphabet', () => {
    const bytes = Uint8Array.from([251, 255]);
    expect(base64Encode(bytes)).toBe('+/8=');
    expect(base64Encode(bytes.buffer)).toBe('+/8=');
    expect(base64Decode('+/8=')).toEqual(bytes);
    expect(base64Decode('')).toEqual(new Uint8Array());
    expect(() => base64Decode('%')).toThrow();
  });

  it('encodes and decodes canonical unpadded values', () => {
    const vectors: Array<[number[], string]> = [
      [[], ''],
      [[102], 'Zg'],
      [[102, 111], 'Zm8'],
      [[102, 111, 111], 'Zm9v'],
      [[251, 255], '-_8'],
    ];

    for (const [bytes, encoded] of vectors) {
      const value = Uint8Array.from(bytes);
      expect(base64UrlEncode(value)).toBe(encoded);
      expect(Array.from(base64UrlDecode(encoded))).toEqual(bytes);
      expect(base64UrlEncode(value.buffer)).toBe(encoded);
    }
  });

  it('handles values larger than the JavaScript argument limit', () => {
    const value = new Uint8Array(new ArrayBuffer(100_000));
    for (let index = 0; index < value.length; index += 1) value[index] = index % 251;
    expect(base64UrlDecode(base64UrlEncode(value))).toEqual(value);
    expect(base64Decode(base64Encode(value))).toEqual(value);
  });

  it.each(['a', 'A=', 'A+', 'a/b', '%'])('rejects malformed value %j', (value) => {
    expect(() => base64UrlDecode(value)).toThrow('Invalid base64url value.');
  });
});
