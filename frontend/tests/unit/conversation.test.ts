import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import ConversationMessage from '$components/ConversationMessage.svelte';
import { clampPayload, conversationEntries, formatToolPayload, maxPayloadLines } from '$lib/conversation';
import type { ConversationEntry, ConversationTool } from '$lib/types';

// Shapes copied from a recorded Claude Code transcript: tool arguments arrive as
// a serialised JSON object, and a Write call embeds a whole file in one string.
const bashInput = JSON.stringify({
  command: 'python3 /tmp/band-sample.py',
  description: 'List 11-15 band',
  timeout: 300000,
});
const writeInput = JSON.stringify({
  content: '#!/usr/bin/env python3\nimport json\nprint("hi")\n',
  file_path: '/tmp/band-sample.py',
});

function turn(id: string, role: 'user' | 'assistant', text = '', tools?: ConversationTool[]): ConversationEntry {
  return { id, role, text, timestamp: '2026-08-23T12:00:00Z', ...(tools ? { tools } : {}) };
}

const bashTool: ConversationTool = { id: 'call-1', name: 'Bash', input: bashInput, output: 'done' };

describe('conversationEntries', () => {
  it('keeps text-less assistant turns that carry tool activity', () => {
    const kept = conversationEntries([
      turn('u1', 'user', 'run the probe'),
      turn('t1', 'assistant', '', [bashTool]),
      turn('t2', 'assistant', '', [{ name: 'Write', input: writeInput, output: 'created' }]),
      turn('a1', 'assistant', 'Probe finished.'),
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 't1', 't2', 'a1']);
  });

  it('collapses superseded prose answers to the newest one', () => {
    const kept = conversationEntries([
      turn('u1', 'user', 'question'),
      turn('a1', 'assistant', 'draft answer'),
      turn('a2', 'assistant', 'final answer'),
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 'a2']);
  });

  it('lists an entry carrying both prose and tools exactly once', () => {
    const kept = conversationEntries([
      turn('u1', 'user', 'question'),
      turn('a1', 'assistant', 'ran it', [bashTool]),
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 'a1']);
  });

  it('keeps chronological order when prose precedes tool activity', () => {
    const kept = conversationEntries([
      turn('u1', 'user', 'question'),
      turn('a1', 'assistant', 'let me check'),
      turn('t1', 'assistant', '', [bashTool]),
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 'a1', 't1']);
  });

  it('drops assistant turns with neither prose nor tools', () => {
    const kept = conversationEntries([
      turn('u1', 'user', 'question'),
      turn('empty', 'assistant', '   '),
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1']);
  });
});

describe('formatToolPayload', () => {
  it('decodes a serialised argument object into one row per argument', () => {
    const formatted = formatToolPayload(bashInput);
    expect(formatted).toBe('command: python3 /tmp/band-sample.py\ndescription: List 11-15 band\ntimeout: 300000');
    expect(formatted).not.toContain('{"command"');
  });

  it('restores real line breaks in an embedded file body', () => {
    const formatted = formatToolPayload(writeInput);
    expect(formatted).toContain('content:\n#!/usr/bin/env python3\nimport json');
    expect(formatted).not.toContain('\\n');
  });

  it('leaves plain-text tool output untouched', () => {
    const output = '=== 11-15 band ===\n  15  apps/catalyst/src/solver.ts:44';
    expect(formatToolPayload(output)).toBe(output);
  });

  it('leaves a malformed object untouched rather than losing it', () => {
    const broken = '{"command": "python3", truncated';
    expect(formatToolPayload(broken)).toBe(broken);
  });

  it('renders nested and non-string values readably', () => {
    const formatted = formatToolPayload(JSON.stringify({
      multiSelect: false,
      retries: 3,
      fallback: null,
      options: [{ label: 'yes' }],
    }));
    expect(formatted).toContain('multiSelect: false');
    expect(formatted).toContain('retries: 3');
    expect(formatted).toContain('fallback: null');
    expect(formatted).toContain('options:\n[\n  {\n    "label": "yes"\n  }\n]');
  });
});

describe('clampPayload', () => {
  it('returns a short payload unchanged', () => {
    const short = 'command: ls\ndescription: list';
    expect(clampPayload(short)).toEqual({ preview: short, clamped: false });
  });

  it('clamps a payload that exceeds the line budget', () => {
    const long = Array.from({ length: 200 }, (_, index) => `line-${index}`).join('\n');
    const { preview, clamped } = clampPayload(long);
    expect(clamped).toBe(true);
    expect(preview.split('\n')).toHaveLength(maxPayloadLines);
    expect(preview).not.toContain('line-199');
  });

  it('slices an over-long single line so the preview is never empty', () => {
    const { preview, clamped } = clampPayload('x'.repeat(30000));
    expect(clamped).toBe(true);
    expect(preview.length).toBe(2000);
  });
});

describe('ConversationMessage tool cards', () => {
  it('shows decoded arguments instead of the raw JSON blob', () => {
    const { container } = render(ConversationMessage, { text: '', tools: [bashTool] });
    const input = container.querySelector('pre');
    expect(input).toHaveTextContent('command: python3 /tmp/band-sample.py');
    expect(container.textContent).not.toContain('{"command"');
  });

  it('reveals a clamped payload on request', async () => {
    const user = userEvent.setup();
    const output = Array.from({ length: 200 }, (_, index) => `line-${index}`).join('\n');
    const { container } = render(ConversationMessage, {
      text: '',
      tools: [{ name: 'Bash', input: bashInput, output }],
    });

    expect(container.textContent).not.toContain('line-199');
    const toggle = screen.getByRole('button', { expanded: false, name: 'Show all 200 lines' });

    await user.click(toggle);

    expect(container.textContent).toContain('line-199');
    expect(screen.getByRole('button', { expanded: true, name: 'Show less' })).toBeInTheDocument();
  });
});
