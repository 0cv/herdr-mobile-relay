import { pathBase } from './paths';

export const launchAbandoned = 'Launch confirmation stopped. Check Agents before starting again; the desktop launch may still finish.';
export const launchConfirmed = 'Start confirmed. Refreshing agent list.';
export const launchError = (error: Error) => `${error.message}. Check Agents before starting again; a tab may already exist.`;
export const launchWorkspaceSuffix = ' The desktop keeps its current focus.';
export const launchDirectoryUnavailable = 'Update and restart this computer’s relay to browse directories.';
export const launchTaskHint = 'Sent to the agent as its first prompt after it starts.';

export function validAgentName(value: unknown): boolean {
  return /^[a-z][a-z0-9_-]{0,31}$/.test(String(value || ''));
}

export function launchNamePart(value: unknown, fallback: string): string {
  const normalized = String(value || '').normalize('NFKD').replace(/[\u0300-\u036f]/g, '').toLowerCase();
  const cleaned = normalized.replace(/[^a-z0-9_-]+/g, '-').replace(/^[-_]+|[-_]+$/g, '');
  if (!cleaned) return fallback;
  return /^[a-z]/.test(cleaned) ? cleaned : `${fallback}-${cleaned}`;
}

export function suggestedLaunchName(cwd: string, profileId: string): string {
  const directory = launchNamePart(pathBase(String(cwd || '')), 'project');
  const agent = launchNamePart(profileId, 'agent');
  const suffix = `-${agent.slice(0, 12)}`;
  return `${directory.slice(0, Math.max(1, 32 - suffix.length))}${suffix}`;
}
