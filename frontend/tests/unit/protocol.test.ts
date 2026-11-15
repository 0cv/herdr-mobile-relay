import { describe, expect, it } from 'vitest';
import { parseCommandErrorDetail, parseWireSafeInteger } from '$lib/protocol';

describe('parseCommandErrorDetail', () => {
  it.each([
    { name: 'empty', value: '', bytes: 0 },
    { name: 'whitespace', value: ' \t\n', bytes: 3 },
    { name: 'padded', value: ' detail ', bytes: 8 },
    { name: 'embedded NUL', value: 'a\0b', bytes: 3 },
    { name: 'ASCII boundary', value: 'a'.repeat(512), bytes: 512 },
    { name: 'two-byte boundary', value: 'é'.repeat(256), bytes: 512 },
    { name: 'three-byte boundary', value: '界'.repeat(170) + 'ab', bytes: 512 },
    { name: 'emoji boundary', value: '😀'.repeat(128), bytes: 512 },
    { name: 'lone high surrogate boundary', value: 'a'.repeat(509) + '\ud800', bytes: 512 },
    { name: 'lone low surrogate boundary', value: 'a'.repeat(509) + '\udc00', bytes: 512 },
    { name: 'composed', value: 'é', bytes: 2 },
    { name: 'decomposed', value: 'e\u0301', bytes: 3 },
  ])('returns the original $name string without normalization', ({ value, bytes }) => {
    expect(new TextEncoder().encode(value).byteLength).toBe(bytes);
    expect(parseCommandErrorDetail(value)).toBe(value);
  });

  it.each([
    { name: 'ASCII', value: 'a'.repeat(513), bytes: 513 },
    { name: 'two-byte', value: 'é'.repeat(256) + 'a', bytes: 513 },
    { name: 'three-byte', value: '界'.repeat(171), bytes: 513 },
    { name: 'emoji', value: '😀'.repeat(129), bytes: 516 },
    { name: 'lone high surrogate', value: 'a'.repeat(510) + '\ud800', bytes: 513 },
    { name: 'lone low surrogate', value: 'a'.repeat(510) + '\udc00', bytes: 513 },
  ])('rejects oversized $name entirely, never truncating', ({ value, bytes }) => {
    expect(new TextEncoder().encode(value).byteLength).toBe(bytes);
    expect(parseCommandErrorDetail(value)).toBe('');
  });

  it.each([
    { name: 'undefined', value: undefined },
    { name: 'null', value: null },
    { name: 'number', value: 512 },
    { name: 'boolean', value: true },
    { name: 'array', value: ['detail'] },
    { name: 'record', value: { detail: 'wire' } },
    { name: 'boxed string', value: Object('detail') },
    { name: 'bigint', value: 512n },
    { name: 'symbol', value: Symbol('detail') },
    { name: 'function', value: () => 'detail' },
  ])('does not accept or coerce $name', ({ value }) => {
    expect(parseCommandErrorDetail(value)).toBe('');
  });
});

describe('parseWireSafeInteger', () => {
  it.each([-5, 0, 7, Number.MIN_SAFE_INTEGER, Number.MAX_SAFE_INTEGER])('accepts the integer %d', value => {
    expect(parseWireSafeInteger(value)).toBe(value);
  });

  it.each([
    Number.MIN_SAFE_INTEGER - 1, Number.MAX_SAFE_INTEGER + 1,
    1.5, -0.5, NaN, Infinity, -Infinity, undefined,
  ])('rejects %s', value => {
    expect(parseWireSafeInteger(value)).toBeUndefined();
  });
});
