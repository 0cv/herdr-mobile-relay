import { unsavedPromptDraft } from './prompt-drafts';
import { terminalInputUnsettled } from './terminal-input-delivery.svelte';

/** What a reload would lose: the fence on an unconfirmed send, or text this phone has not saved. */
export type ReloadRisk = 'delivery' | 'draft';

const checks = new Set<() => ReloadRisk | null>();

/** Adds a check for state that only lives in this page. Returns the function that removes it. */
export function guardReload(check: () => ReloadRisk | null): () => void {
  checks.add(check);
  return () => { checks.delete(check); };
}

export function reloadRisk(): ReloadRisk | null {
  if (terminalInputUnsettled()) return 'delivery';
  let risk: ReloadRisk | null = unsavedPromptDraft() ? 'draft' : null;
  for (const check of checks) {
    const found = check();
    if (found === 'delivery') return found;
    risk ||= found;
  }
  return risk;
}

export function reloadRiskMessage(risk: ReloadRisk): string {
  return risk === 'delivery'
    ? 'A send to a terminal is not confirmed yet, and loading the app update forgets it. Check the terminal before sending again.'
    : 'A draft is not saved on this phone, and loading the app update discards it.';
}
