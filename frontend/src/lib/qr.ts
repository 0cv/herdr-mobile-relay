/**
 * The relay encodes QR codes with the same library it uses for the terminal
 * setup code and sends the modules packed into bits, so the app only unpacks
 * and draws them.
 */

export interface QrBitmap {
  size: number;
  /** One SVG path covering every dark module, at one unit per module. */
  path: string;
}

/**
 * Unpacks a row-major bitmap of dark modules. Returns null when the payload is
 * not a square bitmap, so a malformed answer draws nothing instead of a
 * scannable-looking square.
 */
export function qrBitmap(size: unknown, modules: unknown): QrBitmap | null {
  if (typeof size !== 'number' || !Number.isInteger(size) || size < 21 || size > 177) return null;
  if (typeof modules !== 'string') return null;
  let bytes: Uint8Array;
  try {
    bytes = Uint8Array.from(atob(modules), (character) => character.charCodeAt(0));
  } catch {
    return null;
  }
  if (bytes.length !== Math.ceil((size * size) / 8)) return null;
  let path = '';
  for (let row = 0; row < size; row++) {
    for (let column = 0; column < size; column++) {
      const index = row * size + column;
      if (bytes[index >> 3] & (1 << (7 - (index & 7)))) path += `M${column} ${row}h1v1h-1z`;
    }
  }
  return { size, path };
}
