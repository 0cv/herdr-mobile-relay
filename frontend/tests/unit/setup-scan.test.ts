import { describe, expect, it } from 'vitest';
import { quickSetupConfig } from '$lib/config';
import { parseScannedSetupLink } from '$lib/setup-scan';
import { directSetupLink, gatewayInvitationLink, otherLink } from '../support/setup-qr';

const expiry = 4_102_444_800_000;
const direct = new URL(directSetupLink);
const gateway = new URL(gatewayInvitationLink);

describe('scanned setup links', () => {
  it.each([
    [directSetupLink, 'https:', { hash: direct.hash, protocol: 'https:', host: direct.host }],
    [gatewayInvitationLink, 'https:', { hash: gateway.hash, protocol: 'https:', host: gateway.host }],
    [` https://another.example:8443/install/home?view=agents${direct.hash} `, 'https:', {
      hash: direct.hash, protocol: 'https:', host: 'another.example:8443',
    }],
    [`http://localhost:8080/${direct.hash}`, 'http:', { hash: direct.hash, protocol: 'http:', host: 'localhost:8080' }],
  ])('accepts %s', (text, protocol, expected) => {
    expect(parseScannedSetupLink(text, protocol, expiry - 1)).toEqual(expected);
  });

  it('uses the scanned host when the relay parameter is absent', () => {
    const text = directSetupLink.split('&relay=')[0];
    const link = parseScannedSetupLink(text, 'https:', expiry - 1);
    if (typeof link === 'string') throw new Error(link);
    expect(quickSetupConfig(link)?.url).toBe(`wss://${direct.host}`);
  });

  it.each([
    'plain text',
    otherLink,
    `javascript:alert(1)${direct.hash}`,
    `https://user:password@${direct.host}/${direct.hash}`,
    `http://${direct.host}/${direct.hash}`,
    `https://${direct.host}/#setup=short`,
    `${directSetupLink}%2Fpath`,
    `${directSetupLink.split('&relay=')[0]}&relay=`,
    `${gatewayInvitationLink}&relay=`,
    directSetupLink.replace('&relay=', '&gateway='),
    `${directSetupLink}&invite=`,
    gatewayInvitationLink.replace('invite_version=1', 'invite_version=0'),
    gatewayInvitationLink.split('&rendezvous=')[0],
  ])('refuses %s as not-setup', (text) => {
    expect(parseScannedSetupLink(text, 'https:', expiry - 1)).toBe('not-setup');
  });

  it('refuses an invitation that expires at the current time', () => {
    expect(parseScannedSetupLink(gatewayInvitationLink, 'https:', expiry)).toBe('expired');
  });
});
