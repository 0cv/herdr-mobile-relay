import { describe, expect, it } from 'vitest';
import { qrBitmap } from '$lib/qr';

function packed(size: number, dark: [number, number][]): string {
  const bytes = new Uint8Array(Math.ceil((size * size) / 8));
  for (const [row, column] of dark) {
    const index = row * size + column;
    bytes[index >> 3] |= 1 << (7 - (index & 7));
  }
  return btoa(String.fromCharCode(...bytes));
}

describe('relay qr bitmaps', () => {
  it('draws one square per dark module', () => {
    const bitmap = qrBitmap(21, packed(21, [[0, 0], [0, 20], [20, 0], [5, 7]]));
    expect(bitmap?.size).toBe(21);
    expect(bitmap?.path.match(/M/g)).toHaveLength(4);
    // Row-major order: the module at row 5, column 7 lands at those units.
    expect(bitmap?.path).toContain('M7 5h1v1h-1z');
    expect(bitmap?.path).toContain('M20 0h1v1h-1z');
    expect(bitmap?.path).toContain('M0 20h1v1h-1z');
  });

  it('refuses payloads that are not a square bitmap', () => {
    // A wrong byte count, a size no QR version uses, and junk must all draw
    // nothing rather than something that looks scannable.
    expect(qrBitmap(21, packed(25, [[0, 0]]))).toBeNull();
    expect(qrBitmap(20, packed(20, []))).toBeNull();
    expect(qrBitmap(21, 'not base64 $$$')).toBeNull();
    expect(qrBitmap('21', packed(21, []))).toBeNull();
    expect(qrBitmap(21, undefined)).toBeNull();
  });
});
