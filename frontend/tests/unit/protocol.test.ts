import { describe, expect, it, vi } from 'vitest';
import { parseCommandErrorDetail } from '$lib/protocol';

describe('parseCommandErrorDetail (post-extraction helper qualification)', () => {
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

  it('never invokes object coercion hooks', () => {
    const coerce = vi.fn(() => { throw new Error('must not coerce'); });
    const value = { toString: coerce, valueOf: coerce, [Symbol.toPrimitive]: coerce };
    expect(parseCommandErrorDetail(value)).toBe('');
    expect(coerce).not.toHaveBeenCalled();
  });
});

import { parseWireSafeInteger } from '$lib/protocol';

describe('parseWireSafeInteger supplemental controls after consumer freeze', () => {
  it('keeps native absent/null/boolean/string/array/record coercion', () => {
    for (const [value, expected] of [
      [undefined, undefined], [null, 0], [false, 0], [true, 1],
      ['', 0], [' \t ', 0], [' 42 ', 42], ['0x10', 16], ['1e2', 100],
      [[], 0], [[null], 0], [[2], 2], [['3'], 3], [[1, 2], undefined], [{}, undefined],
    ] as Array<[unknown, number | undefined]>) {
      expect(parseWireSafeInteger(value)).toBe(expected);
    }
  });

  it('returns representative real integers and rejects fractions and ordinary invalid values', () => {
    for (const value of [-5, 0, 7]) expect(parseWireSafeInteger(value)).toBe(value);
    for (const value of [1.5, -0.5, NaN, Infinity, -Infinity, 'NaN', 'Infinity', 'bad']) {
      expect(parseWireSafeInteger(value)).toBeUndefined();
    }
  });

  it('preserves negative zero rather than applying a truthy fallback', () => {
    for (const value of [-0, '-0']) {
      expect(parseWireSafeInteger(value)).toBe(-0);
      expect(Object.is(parseWireSafeInteger(value), -0)).toBe(true);
    }
    for (const value of [0, '0']) expect(Object.is(parseWireSafeInteger(value), -0)).toBe(false);
  });

  it('includes both safe integer endpoints and excludes the immediately adjacent numbers', () => {
    for (const value of [Number.MIN_SAFE_INTEGER, Number.MAX_SAFE_INTEGER]) {
      expect(parseWireSafeInteger(value)).toBe(value);
      expect(parseWireSafeInteger(String(value))).toBe(value);
    }
    for (const value of [Number.MIN_SAFE_INTEGER - 1, Number.MAX_SAFE_INTEGER + 1]) {
      expect(parseWireSafeInteger(value)).toBeUndefined();
      expect(parseWireSafeInteger(String(value))).toBeUndefined();
    }
  });

  it('performs exactly two successful native conversions and returns the second conversion', () => {
    const valueOf = vi.fn().mockReturnValueOnce(7).mockReturnValueOnce(9);
    const toString = vi.fn(() => '11');
    expect(parseWireSafeInteger({ valueOf, toString })).toBe(9);
    expect(valueOf).toHaveBeenCalledTimes(2);
    expect(toString).not.toHaveBeenCalled();
  });

  it('performs only the invalid first conversion with no second conversion', () => {
    const valueOf = vi.fn(() => 1.5);
    const toString = vi.fn(() => '2');
    expect(parseWireSafeInteger({ valueOf, toString })).toBeUndefined();
    expect(valueOf).toHaveBeenCalledTimes(1);
    expect(toString).not.toHaveBeenCalled();
  });

  it('propagates exact first and second conversion exceptions without wrapping', () => {
    for (const failureAt of [1, 2]) {
      const sentinel = new TypeError('native conversion ' + failureAt);
      let conversions = 0;
      const coerce = vi.fn(() => {
        if (++conversions === failureAt) throw sentinel;
        return 7;
      });
      let caught: unknown;
      try { parseWireSafeInteger({ [Symbol.toPrimitive]: coerce }); } catch (error) { caught = error; }
      expect(caught).toBe(sentinel);
      expect(coerce).toHaveBeenCalledTimes(failureAt);
      expect(coerce.mock.calls).toEqual(Array.from({ length: failureAt }, () => ['number']));
    }
    expect(() => parseWireSafeInteger({ toString: null })).toThrow(TypeError);
  });
});
