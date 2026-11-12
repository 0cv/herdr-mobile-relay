import { describe, expect, it } from 'vitest';
import {
  isAuthenticationIdentifier,
  isAuthenticationSecret,
  isAuthenticationVersion,
  isDeviceLocale,
  isDeviceRole,
  isInvitationId,
} from '$lib/device-auth';

describe('device authentication formats', () => {
  it('checks identifiers without normalizing them', () => {
    for (const value of ['credential-id', ' credential-id ', 'a'.repeat(256)]) {
      expect(isAuthenticationIdentifier(value)).toBe(true);
    }
    for (const value of [null, 1, '', 'a'.repeat(257), 'id\u0000', 'id\u007f']) {
      expect(isAuthenticationIdentifier(value)).toBe(false);
    }
  });

  it('requires positive safe integer versions', () => {
    for (const value of [1, Number.MAX_SAFE_INTEGER]) expect(isAuthenticationVersion(value)).toBe(true);
    for (const value of [null, '1', 0, -1, 1.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
      expect(isAuthenticationVersion(value)).toBe(false);
    }
  });

  it('requires unpadded 32-byte authentication secrets', () => {
    for (const value of ['a'.repeat(43), '-_'.repeat(21) + 'A']) expect(isAuthenticationSecret(value)).toBe(true);
    for (const value of [null, 1, '', 'a'.repeat(42), 'a'.repeat(44), 'a'.repeat(42) + '=', '+'.repeat(43)]) {
      expect(isAuthenticationSecret(value)).toBe(false);
    }
  });

  it('keeps invitation ids narrower than credential ids', () => {
    for (const value of ['a'.repeat(16), '-_'.repeat(64)]) expect(isInvitationId(value)).toBe(true);
    for (const value of [null, 1, 'a'.repeat(15), 'a'.repeat(129), ' '.repeat(16), '.'.repeat(16)]) {
      expect(isInvitationId(value)).toBe(false);
    }
  });

  it('accepts only device roles and bounded language tags', () => {
    expect(isDeviceRole('reader')).toBe(true);
    expect(isDeviceRole('controller')).toBe(true);
    for (const value of [null, 'admin', 'Reader']) expect(isDeviceRole(value)).toBe(false);
    for (const value of ['en', 'eng', 'zh-Hant-TW', 'en-' + 'a'.repeat(8) + '-bbbbbbbb-cccccccc-ddddd']) {
      expect(isDeviceLocale(value)).toBe(true);
    }
    for (const value of [null, 1, '', 'e', 'en_US', 'en-abcdefghi', 'en-' + 'a'.repeat(8) + '-bbbbbbbb-cccccccc-dddddd']) {
      expect(isDeviceLocale(value)).toBe(false);
    }
  });
});
