import { describe, expect, it } from 'vitest';
import {
  importQuickSetup,
  loadRelayConfigs,
  normalizeRelayConfig,
  quickSetupConfig,
  quickSetupInvitation,
  shouldDeferPairingConnection,
  shouldRetainSetupFragment,
  saveRelayConfigs,
} from '$lib/config';

const TOKEN = '0123456789abcdef0123456789abcdef';

/** A setup link as the relay prints it: everything secret stays in the fragment. */
function setupLink(fragment: string): Pick<Location, 'hash' | 'protocol' | 'host'> {
  return { hash: `#setup=${TOKEN}&${fragment}`, protocol: 'https:', host: 'app.example.com' };
}

describe('Home Screen setup handoff', () => {
  it('retains a valid setup fragment only in an iOS browser tab', () => {
    const locationValue = setupLink('label=Fedora&relay=wss%3A%2F%2Frelay.example.com');
    expect(shouldRetainSetupFragment(locationValue, false)).toBe(true);
    expect(shouldRetainSetupFragment(locationValue, true)).toBe(false);
    expect(shouldRetainSetupFragment(locationValue, undefined)).toBe(false);
    expect(shouldRetainSetupFragment({
      ...locationValue,
      hash: '#setup=short',
    }, false)).toBe(false);
  });

  it('defers one-use pairing secrets until the iOS Home Screen app opens', () => {
    const invitation = {
      ...setupLink('label=Fedora&relay=wss%3A%2F%2Frelay.example.com'),
      hash: `#setup=${'A'.repeat(43)}&invite=${'B'.repeat(24)}&invite_version=1&invite_expires=2000000000000`,
    };
    expect(shouldDeferPairingConnection(invitation, false, 'Mozilla/5.0 (iPhone)', 0)).toBe(true);
    expect(shouldDeferPairingConnection(invitation, false, 'Mozilla/5.0 (Macintosh)', 5)).toBe(true);
    expect(shouldDeferPairingConnection(invitation, true, 'Mozilla/5.0 (iPhone)', 0)).toBe(false);
    expect(shouldDeferPairingConnection(invitation, false, 'Mozilla/5.0 (Android)', 5)).toBe(false);

    // The bootstrap relay key printed by setup is one-use as well: a tab that
    // redeems it leaves the installed copy with a spent link.
    const bootstrap = setupLink('label=Fedora&relay=wss%3A%2F%2Frelay.example.com');
    expect(shouldDeferPairingConnection(bootstrap, false, 'Mozilla/5.0 (iPhone)', 0)).toBe(true);
    expect(shouldDeferPairingConnection(bootstrap, false, 'Mozilla/5.0 (iPad)', 0)).toBe(true);
    expect(shouldDeferPairingConnection(bootstrap, true, 'Mozilla/5.0 (iPhone)', 0)).toBe(false);
    expect(shouldDeferPairingConnection(bootstrap, false, 'Mozilla/5.0 (Android)', 5)).toBe(false);
    expect(shouldDeferPairingConnection({ ...bootstrap, hash: '#label=Fedora' }, false, 'Mozilla/5.0 (iPhone)', 0)).toBe(false);
  });
});
describe('device invitation setup', () => {
  it('imports a one-use invitation without retaining its secret as a relay token', () => {
    const secret = 'A'.repeat(43);
    const locationValue = {
      hash: `#setup=${secret}&invite=${'B'.repeat(24)}&invite_version=2&invite_expires=2000000000000&label=Phone&relay=wss%3A%2F%2Frelay.example.com`,
      protocol: 'https:',
      host: 'app.example.com',
    };
    expect(quickSetupInvitation(locationValue)).toEqual({
      id: 'B'.repeat(24),
      version: 2,
      secret,
      expiresAt: 2_000_000_000_000,
    });
    expect(quickSetupConfig(locationValue)).toEqual({
      label: 'Phone',
      url: 'wss://relay.example.com',
      token: '',
    });
  });

  it('preserves existing relay transport fields when importing an invitation', () => {
    const existing = normalizeRelayConfig({
      label: 'Phone',
      url: 'wss://old-relay.example.com',
      token: TOKEN,
      transport: 'hybrid',
      gatewayUrl: 'wss://gateway.example.com',
      gatewayUrls: ['wss://gateway.example.com'],
    });
    const locationValue = {
      hash: `#setup=${'A'.repeat(43)}&invite=${'B'.repeat(24)}&invite_version=2&invite_expires=2000000000000&label=Phone&relay=wss%3A%2F%2Fold-relay.example.com`,
      protocol: 'https:',
      host: 'app.example.com',
    };

    expect(importQuickSetup([existing], locationValue)?.[0]).toMatchObject({
      id: existing.id,
      token: TOKEN,
      transport: 'hybrid',
      gatewayUrl: 'wss://gateway.example.com',
      gatewayUrls: ['wss://gateway.example.com'],
    });
  });

  it('rejects malformed invitation selectors', () => {
    expect(quickSetupInvitation({
      hash: `#setup=${'A'.repeat(43)}&invite=short&invite_version=1&invite_expires=2000000000000`,
    })).toBeNull();
  });

  it('imports a gateway invitation with the rendezvous it needs to connect', () => {
    const relayId = 'Ccy3nT9AULlAceTEnhTvoQ';
    const rendezvous = 'xvT5VptkJHebIfy8b9PSGTJMkdRb-J_P2SXrtNRoLyA';
    const link = (extra: string) => ({
      hash: `#setup=${'A'.repeat(43)}&invite=${'B'.repeat(24)}&invite_version=1&invite_expires=2000000000000`
        + `&label=Fedora&gateways=wss%3A%2F%2Fa.example,wss%3A%2F%2Fb.example${extra}`,
      protocol: 'https:',
      host: 'app.example.com',
    });
    expect(quickSetupConfig(link(`&relay_id=${relayId}&rendezvous=${rendezvous}`))).toEqual({
      label: 'Fedora',
      url: '',
      token: '',
      transport: 'hybrid',
      gatewayUrl: 'wss://a.example',
      gatewayUrls: ['wss://a.example', 'wss://b.example'],
      gatewayRelayId: relayId,
      rendezvousKey: rendezvous,
    });
    // Without the rendezvous the invited device could never reach the relay.
    expect(quickSetupConfig(link(''))).toBeNull();
    expect(quickSetupConfig(link(`&relay_id=${relayId}`))).toBeNull();
    expect(quickSetupConfig(link(`&relay_id=short&rendezvous=${rendezvous}`))).toBeNull();

    // The stored entry keeps both, and a second computer on the same gateway
    // with the same label stays a separate entry because its relay id differs.
    const imported = importQuickSetup([], link(`&relay_id=${relayId}&rendezvous=${rendezvous}`))!;
    expect(imported).toHaveLength(1);
    expect(imported[0]).toMatchObject({ paired: true, gatewayRelayId: relayId, rendezvousKey: rendezvous });
    saveRelayConfigs(imported);
    expect(loadRelayConfigs()[0]).toMatchObject({ gatewayRelayId: relayId, rendezvousKey: rendezvous });
    const otherId = 'Ccy3nT9AULlAceTEnhTvoR';
    const both = importQuickSetup(imported, link(`&relay_id=${otherId}&rendezvous=${rendezvous}`))!;
    expect(both).toHaveLength(2);
    expect(both.map((relay) => relay.gatewayRelayId)).toEqual([relayId, otherId]);
    // Re-importing the first computer's invitation updates its entry in place.
    const again = importQuickSetup(both, link(`&relay_id=${relayId}&rendezvous=${rendezvous}`))!;
    expect(again).toHaveLength(2);
    expect(again[0].id).toBe(imported[0].id);
  });
});


describe('ordered gateway lists', () => {
  it('reads the whole ordered list out of a setup fragment', () => {
    expect(quickSetupConfig(setupLink(
      'label=Fedora&gateways=wss%3A%2F%2Fa.example,wss%3A%2F%2Fb.example',
    ))).toEqual({
      label: 'Fedora',
      url: '',
      token: TOKEN,
      transport: 'hybrid',
      gatewayUrl: 'wss://a.example',
      gatewayUrls: ['wss://a.example', 'wss://b.example'],
    });
  });

  it('normalizes the list without ever reordering it', () => {
    // Padding, a trailing slash, a repeat and one unusable entry all survive
    // contact with the parser; the order the relay chose is what remains.
    const setup = quickSetupConfig(setupLink(
      'gateways=wss%3A%2F%2Fb.example%2F,%20wss%3A%2F%2Fa.example%20,wss%3A%2F%2Fb.example,http%3A%2F%2Fc.example',
    ));
    expect(setup?.gatewayUrls).toEqual(['wss://b.example', 'wss://a.example']);
    expect(setup?.gatewayUrl).toBe('wss://b.example');
  });

  it('rejects the unreleased scalar gateway field', () => {
    expect(quickSetupConfig(setupLink(
      'label=Fedora&gateway=wss%3A%2F%2Fgw.example.com',
    ))).toBeNull();
  });

  it.each([
    ['an empty list', 'gateways='],
    ['nothing but junk', 'gateways=javascript%3Aalert(1),http%3A%2F%2Fa.example'],
    ['paths that would leak the key', 'gateways=wss%3A%2F%2Fa.example%2Fconnect%3Fkey%3Dleak'],
  ])('rejects a setup link carrying %s', (_label, fragment) => {
    expect(quickSetupConfig(setupLink(fragment))).toBeNull();
  });

  it('normalizes a preferred gateway into the complete candidate list', () => {
    const stored = normalizeRelayConfig({ label: 'Fedora', url: '', token: TOKEN, gatewayUrl: 'wss://gw.example.com/' });
    expect(stored).toEqual({
      id: 'fedora-wss-gw-example-com',
      label: 'Fedora',
      url: '',
      token: TOKEN,
      transport: 'hybrid',
      gatewayUrl: 'wss://gw.example.com',
      gatewayUrls: ['wss://gw.example.com'],
    });

    saveRelayConfigs([stored]);
    expect(loadRelayConfigs()).toEqual([stored]);
  });

  it('keeps the primary equal to the first list entry', () => {
    const relay = normalizeRelayConfig({
      label: 'Fedora',
      token: TOKEN,
      // A gateway the relay advertised on a live session leads the stored list:
      // it is the fresher address, and the rest stay behind it as fallbacks.
      gatewayUrl: 'wss://c.example',
      gatewayUrls: ['wss://a.example', 'not-a-url', 'wss://b.example'],
    });
    expect(relay.gatewayUrls).toEqual(['wss://c.example', 'wss://a.example', 'wss://b.example']);
    expect(relay.gatewayUrl).toBe(relay.gatewayUrls?.[0]);

    // A LAN pairing over plain http keeps its ws: address across reloads.
    expect(normalizeRelayConfig({ label: 'Pi', token: TOKEN, gatewayUrls: ['ws://pi.local:8080'] }).gatewayUrls)
      .toEqual(['ws://pi.local:8080']);

    // An unusable entry is dropped rather than taking the relay down with it.
    expect(normalizeRelayConfig({
      label: 'Fedora',
      token: TOKEN,
      gatewayUrl: 'wss://gw.example.com',
      gatewayUrls: ['wss://gw.example.com/connect', 'wss://gw.example.com'],
    }).gatewayUrls).toEqual(['wss://gw.example.com']);
  });

  it('leaves a hybrid entry addressless when no gateway survives', () => {
    // The entry has no usable address, so the transport reports it fatally.
    expect(normalizeRelayConfig({
      label: 'Fedora',
      token: TOKEN,
      transport: 'hybrid',
      gatewayUrl: 'gw.example.com',
    })).toEqual({ id: 'fedora', label: 'Fedora', url: '', token: TOKEN, transport: 'hybrid', gatewayUrl: '' });
  });

  it('updates a paired computer whose gateway list changed', () => {
    const paired = importQuickSetup([], setupLink(
      'label=Fedora&gateways=wss%3A%2F%2Fa.example,wss%3A%2F%2Fb.example',
    ));
    expect(paired).toHaveLength(1);

    // The relay promoted its second gateway. The computer is the same one, so
    // the stored entry follows the new order instead of pairing itself twice.
    const repaired = importQuickSetup(paired!, setupLink(
      'label=Fedora&gateways=wss%3A%2F%2Fb.example,wss%3A%2F%2Fa.example',
    ));
    expect(repaired).toHaveLength(1);
    expect(repaired?.[0].id).toBe(paired?.[0].id);
    expect(repaired?.[0].gatewayUrls).toEqual(['wss://b.example', 'wss://a.example']);
    expect(repaired?.[0].gatewayUrl).toBe('wss://b.example');
  });

  it('follows a restarted quick tunnel to its new hostname by relay key', () => {
    const paired = importQuickSetup([], setupLink('label=cv&relay=wss%3A%2F%2Fold-tunnel.trycloudflare.com'));
    expect(paired).toHaveLength(1);

    // The relay restarted: same persistent key, brand-new tunnel hostname.
    // The stored entry follows the relay so the device credential enrolled
    // under its id keeps authenticating - the one-use bootstrap invitation is
    // consumed and could never enroll this phone a second time.
    const moved = importQuickSetup(paired!, setupLink('label=cv&relay=wss%3A%2F%2Fnew-tunnel.trycloudflare.com'));
    expect(moved).toHaveLength(1);
    expect(moved?.[0].id).toBe(paired?.[0].id);
    expect(moved?.[0].url).toBe('wss://new-tunnel.trycloudflare.com');

    // A different computer's link carries a different key and pairs separately.
    const other = importQuickSetup(moved!, {
      hash: `#setup=${'f'.repeat(32)}&label=mac&relay=wss%3A%2F%2Fmac-tunnel.trycloudflare.com`,
      protocol: 'https:',
      host: 'app.example.com',
    });
    expect(other).toHaveLength(2);
  });

  // The producer of this string is shell, not TypeScript: it is the verbatim
  // output of build_transport_setup_fragment (relay/common.sh) for
  // HERDR_GATEWAY_URL="wss://primary.example, wss://backup.example/". The two
  // sides encode and split the list independently, so this pins the seam.
  it('parses the fragment the relay actually emits', () => {
    const setup = quickSetupConfig(setupLink(
      'label=cv&setup=2435028f051dfa73447b2e2b185c3ca4'
      + '&gateways=wss%3A%2F%2Fprimary.example,wss%3A%2F%2Fbackup.example',
    ));
    expect(setup?.transport).toBe('hybrid');
    expect(setup?.gatewayUrl).toBe('wss://primary.example');
    expect(setup?.gatewayUrls).toEqual(['wss://primary.example', 'wss://backup.example']);
  });
});
