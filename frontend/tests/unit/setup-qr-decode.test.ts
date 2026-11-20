import { describe, expect, it } from 'vitest';
import { decodeQR } from 'qr/decode.js';
import { directSetupLink, gatewayInvitationLink, otherLink, qrPixels } from '../support/setup-qr';

describe('relay QR codes', () => {
  it.each([
    ['setup-qr-invitation', gatewayInvitationLink],
    ['setup-qr-direct', directSetupLink],
    ['other-qr', otherLink],
  ])('decodes %s in both polarities', (fixture, text) => {
    expect(decodeQR(qrPixels(fixture))).toBe(text);
    expect(decodeQR(qrPixels(fixture, true))).toBe(text);
  });

  it('throws for a blank image', () => {
    expect(() => decodeQR({ width: 64, height: 64, data: new Uint8ClampedArray(64 * 64 * 4).fill(255) })).toThrow();
  });
});
