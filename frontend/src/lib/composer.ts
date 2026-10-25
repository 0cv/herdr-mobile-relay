import type { Agent } from './types';

export interface ComposerSubmitAdapter {
  filter: boolean;
  validate?: (text: string) => string;
  send: (target: Agent, text: string) => Promise<unknown>;
  sentMessage: string;
  failureMessage: string;
}

export function dispatchFlag(error: unknown, flag: string): boolean {
  if (!error || typeof error !== 'object' || !('data' in error)) return false;
  const data = error.data;
  return Boolean(data && typeof data === 'object' && flag in data && (data as Record<string, unknown>)[flag] === true);
}
