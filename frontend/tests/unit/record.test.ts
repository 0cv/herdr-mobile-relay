import { describe, expect, it } from 'vitest';
import { isRecord } from '$lib/record';

describe('record values', () => {
  it('accepts non-array objects', () => {
    expect(isRecord({})).toBe(true);
    expect(isRecord({ key: 'value' })).toBe(true);
    expect(isRecord(Object.create(null))).toBe(true);
  });

  it.each([null, undefined, [], [1], '', 'value', 0, true, () => ({})])('rejects non-record value %j', (value) => {
    expect(isRecord(value)).toBe(false);
  });
});
