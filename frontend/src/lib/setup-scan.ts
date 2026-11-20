import { quickSetupConfig, quickSetupInvitation } from './config';

export function parseScannedSetupLink(
  text: string,
  appProtocol: string,
  now: number,
): Pick<Location, 'hash' | 'protocol' | 'host'> | 'not-setup' | 'expired' {
  let url: URL;
  try {
    url = new URL(text.trim());
  } catch {
    return 'not-setup';
  }
  const { hash, protocol, host } = url;
  if (protocol !== 'https:' && !(protocol === 'http:' && appProtocol === 'http:')) return 'not-setup';
  if (url.username || url.password) return 'not-setup';

  const link = { hash, protocol, host };
  if (!quickSetupConfig(link)) return 'not-setup';
  if (new URLSearchParams(hash.slice(1)).has('invite')) {
    const invitation = quickSetupInvitation(link);
    if (!invitation) return 'not-setup';
    if (invitation.expiresAt <= now) return 'expired';
  }
  return link;
}
