import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  clearPromptDraft,
  loadPromptDraft,
  promptDraftIdentity,
  prunePromptDrafts,
  savePromptDraft,
} from '$lib/prompt-drafts';
import type { Agent } from '$lib/types';

function agent(overrides: Partial<Agent> = {}): Agent {
  return {
    relay_id: 'relay-1',
    relay_label: 'Fedora',
    raw_pane_id: 'pane-1',
    pane_id: 'relay-1::pane-1',
    terminal_id: 'terminal-1',
    agent: 'codex',
    cwd: '/work/project',
    ...overrides,
  };
}

describe('prompt drafts', () => {
  beforeEach(() => localStorage.clear());

  it('restores a draft only for the same relay pane identity', () => {
    const current = agent();
    expect(savePromptDraft(current, 'unfinished reply', 1_000)).toBe('saved');
    expect(loadPromptDraft(current, 2_000)).toBe('unfinished reply');
    expect(loadPromptDraft(agent({ terminal_id: 'terminal-2' }), 2_000)).toBe('');
    expect(loadPromptDraft(agent({ relay_id: 'relay-2' }), 2_000)).toBe('');
  });

  it('expires old drafts and clears successful sends', () => {
    const current = agent();
    savePromptDraft(current, 'stale', 1_000);
    expect(loadPromptDraft(current, 48 * 60 * 60 * 1_000 + 1_001)).toBe('');

    savePromptDraft(current, 'send me', 2_000);
    clearPromptDraft(current);
    expect(loadPromptDraft(current, 2_001)).toBe('');
  });

  it('keeps an oversize draft in memory and deletes the record it cannot replace', () => {
    const current = agent({ terminal_id: 'terminal-oversize' });
    const oversize = 'x'.repeat(64 * 1_024 + 1);
    expect(savePromptDraft(current, 'short enough to persist', 1_000)).toBe('saved');
    expect(savePromptDraft(current, oversize, 2_000)).toBe('too-large');
    expect(localStorage.length).toBe(0);

    // A pane switch remounts the composer, which reloads the draft.
    expect(loadPromptDraft(current, 3_000)).toBe(oversize);

    clearPromptDraft(current);
    expect(loadPromptDraft(current, 3_001)).toBe('');
    expect(localStorage.length).toBe(0);
  });

  it('keeps only the newest bounded set of drafts', () => {
    for (let index = 0; index < 70; index += 1) {
      savePromptDraft(agent({ terminal_id: `terminal-${index}` }), `draft ${index}`, 1_000 + index);
    }
    prunePromptDrafts(2_000);
    expect(localStorage.length).toBe(64);
    expect(loadPromptDraft(agent({ terminal_id: 'terminal-0' }), 2_000)).toBe('');
    expect(loadPromptDraft(agent({ terminal_id: 'terminal-69' }), 2_000)).toBe('draft 69');
  });

  it('keeps the live composer usable when browser storage throws', () => {
    const getItem = vi.spyOn(localStorage, 'getItem').mockImplementation(() => { throw new Error('blocked'); });
    expect(loadPromptDraft(agent())).toBe('');
    getItem.mockRestore();
    const setItem = vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('full'); });
    expect(savePromptDraft(agent(), 'still visible')).toBe('unavailable');
    setItem.mockRestore();
  });

  it('does not include mutable display labels in its identity', () => {
    expect(promptDraftIdentity(agent({ relay_label: 'Laptop', project: 'Old' })))
      .toBe(promptDraftIdentity(agent({ relay_label: 'Renamed laptop', project: 'New' })));
  });
});
