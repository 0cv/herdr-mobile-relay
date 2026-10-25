import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import ConversationMessage from '$components/ConversationMessage.svelte';
import { clampPayload, conversationEntries, formatToolPayload, maxPayloadLines, parseAskUserQuestion } from '$lib/conversation';
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
  it('drops text-less assistant tool turns from the compact view', () => {
    const kept = conversationEntries([
      turn('u1', 'user', 'run the probe'),
      turn('t1', 'assistant', '', [bashTool]),
      turn('t2', 'assistant', '', [{ name: 'Write', input: writeInput, output: 'created' }]),
      turn('a1', 'assistant', 'Probe finished.'),
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 'a1']);
  });

  it('collapses superseded prose answers to the newest one', () => {
    const kept = conversationEntries([
      turn('u1', 'user', 'question'),
      turn('a1', 'assistant', 'draft answer'),
      turn('a2', 'assistant', 'final answer'),
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 'a2']);
  });

  it('strips tools from a retained answer without mutating full history', () => {
    const recorded = turn('a1', 'assistant', 'ran it', [bashTool]);
    const kept = conversationEntries([
      turn('u1', 'user', 'question'),
      recorded,
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 'a1']);
    expect(kept[1].text).toBe('ran it');
    expect(kept[1].tools).toBeUndefined();
    expect(recorded.tools).toEqual([bashTool]);
  });

  it('drops a superseded tool-bearing answer entirely', () => {
    const superseded = turn('a1', 'assistant', 'ran it', [bashTool]);
    superseded.truncated = true;
    const kept = conversationEntries([
      turn('u1', 'user', 'question'),
      superseded,
      turn('a2', 'assistant', 'final answer'),
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 'a2']);
    expect(superseded.text).toBe('ran it');
    expect(superseded.tools).toEqual([bashTool]);
    expect(superseded.truncated).toBe(true);
  });

  it('keeps only final prose across interleaved tool activity', () => {
    const kept = conversationEntries([
      turn('u1', 'user', 'question'),
      turn('a1', 'assistant', 'draft answer'),
      turn('t1', 'assistant', '', [bashTool]),
      turn('a2', 'assistant', 'progress answer'),
      turn('t2', 'assistant', '', [{ name: 'Write', input: writeInput, output: 'created' }]),
      turn('a3', 'assistant', 'final answer'),
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 'a3']);
    expect(kept.map((entry) => entry.text)).toEqual(['question', 'final answer']);
  });

  it('keeps preceding prose when only tool activity follows', () => {
    const kept = conversationEntries([
      turn('u1', 'user', 'question'),
      turn('a1', 'assistant', 'let me check'),
      turn('t1', 'assistant', '', [bashTool]),
    ]);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 'a1']);
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
    const { container } = render(ConversationMessage, { viewState: { tableOffsets: [], openTools: new Set<string>() }, text: '', tools: [bashTool] });
    const input = container.querySelector('pre');
    expect(input).toHaveTextContent('command: python3 /tmp/band-sample.py');
    expect(container.textContent).not.toContain('{"command"');
  });

  it('reveals a clamped payload on request', async () => {
    const user = userEvent.setup();
    const output = Array.from({ length: 200 }, (_, index) => `line-${index}`).join('\n');
    const { container } = render(ConversationMessage, {
      viewState: { tableOffsets: [], openTools: new Set<string>() },
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

describe('parseAskUserQuestion', () => {
  const input = JSON.stringify({ questions: [{ question: 'Pick one?', header: 'Pick', multiSelect: false, options: [{ label: 'A', description: 'first' }, { label: 'B', description: '' }] }] });

  it('structures the question and pairs it with the recorded answer', () => {
    const [item] = parseAskUserQuestion(input, 'The user answered: "Pick one?"="B". Read the answers carefully.')!;
    expect(item.options.map((option) => option.label)).toEqual(['A', 'B']);
    expect(item.answer).toBe('B');
  });

  it('ignores other tool payloads', () => {
    expect(parseAskUserQuestion('{"command":"ls"}')).toBeNull();
    expect(parseAskUserQuestion('not json')).toBeNull();
  });

  it('extracts answers from the "Your questions have been answered" envelope', () => {
    const output = 'Your questions have been answered: "Pick one?"="A". You can now continue with these answers in mind.';
    expect(parseAskUserQuestion(input, output)![0].answer).toBe('A');
  });

  it('extracts answers from the "The user answered" envelope with its closing sentence', () => {
    const output = 'The user answered: "Pick one?"="B". Read the answers carefully \u2014 they may request clarification, changes, or that you not proceed \u2014 and follow what they actually say.';
    expect(parseAskUserQuestion(input, output)![0].answer).toBe('B');
  });

  it('attributes two answers to the right questions', () => {
    const two = JSON.stringify({ questions: [{ question: 'Q1', options: [] }, { question: 'Q2', options: [] }] });
    const output = 'Your questions have been answered: "Q1"="A1", "Q2"="A2". You can now continue with these answers in mind.';
    expect(parseAskUserQuestion(two, output)!.map((item) => item.answer)).toEqual(['A1', 'A2']);
  });

  it('keeps a multi-select answer as one value', () => {
    const output = 'Your questions have been answered: "Pick one?"="A, B". You can now continue with these answers in mind.';
    expect(parseAskUserQuestion(input, output)![0].answer).toBe('A, B');
  });

  it('returns backslashes unchanged', () => {
    const output = 'Your questions have been answered: "Pick one?"="C:\\temp\\new". You can now continue with these answers in mind.';
    expect(parseAskUserQuestion(input, output)![0].answer).toBe('C:\\temp\\new');
  });

  it('leaves answer undefined for notes, no selection and unanswered replies', () => {
    for (const output of [
      'Your questions have been answered: "Pick one?"="A" notes: please hurry. You can now continue with these answers in mind.',
      'Your questions have been answered: "Pick one?"=(no option selected). You can now continue with these answers in mind.',
      'The user did not answer the questions.',
      'The user answered: "Pick one?"="A" selected preview:\nsome preview',
      'The user responded: "Pick one?"="A".',
    ]) expect(parseAskUserQuestion(input, output)![0].answer).toBeUndefined();
  });
});

describe('conversationEntries keeps agent questions', () => {
  it('shows an AskUserQuestion exchange even when the turn has no prose, and still drops other tools', () => {
    const entries = [
      { id: 'u1', role: 'user', text: 'go', timestamp: '' },
      { id: 'a1', role: 'assistant', text: '', timestamp: '', tools: [{ id: 't', name: 'AskUserQuestion', input: '{}', output: '' }, { id: 'b', name: 'Bash', input: '{}', output: '' }] },
    ] as ConversationEntry[];
    const kept = conversationEntries(entries);
    expect(kept.map((entry) => entry.id)).toEqual(['u1', 'a1']);
    expect(kept[1].tools?.map((tool) => tool.name)).toEqual(['AskUserQuestion']);
  });
});

describe('AskUserQuestion hardening', () => {
  const ask = (questions: unknown) => JSON.stringify({ questions });
  const q = { question: 'Q', header: '', multiSelect: false, options: [{ label: 'A', description: '' }, { label: 'A', description: '' }] };

  it('keeps a question when the assistant continues with prose', () => {
    const entries = [
      { id: 'u1', role: 'user', text: 'go', timestamp: '' },
      { id: 'a1', role: 'assistant', text: '', timestamp: '', tools: [{ id: 't', name: 'AskUserQuestion', input: '{}', output: '' }] },
      { id: 'a2', role: 'assistant', text: 'Thanks, doing it.', timestamp: '' },
    ] as ConversationEntry[];
    expect(conversationEntries(entries).map((entry) => entry.id)).toEqual(['u1', 'a1', 'a2']);
  });

  it('keeps earlier prose when a question-only turn follows it', () => {
    const entries = [
      { id: 'u1', role: 'user', text: 'go', timestamp: '' },
      { id: 'a1', role: 'assistant', text: 'Here is the answer.', timestamp: '' },
      { id: 'a2', role: 'assistant', text: '', timestamp: '', tools: [{ id: 't', name: 'AskUserQuestion', input: '{}', output: '' }] },
    ] as ConversationEntry[];
    expect(conversationEntries(entries).map((entry) => entry.id)).toEqual(['u1', 'a1', 'a2']);
  });

  it('does not attribute one answer to duplicate input questions', () => {
    const dup = JSON.stringify({ questions: [{ question: 'Q', options: [] }, { question: 'Q', options: [] }] });
    const output = 'Your questions have been answered: "Q"="A". You can now continue with these answers in mind.';
    expect(parseAskUserQuestion(dup, output)!.map((item) => item.answer)).toEqual([undefined, undefined]);
  });

  it('does not attribute answers when question text repeats', () => {
    const dup = JSON.stringify({ questions: [{ question: 'Q', options: [] }, { question: 'Q', options: [] }] });
    const result = parseAskUserQuestion(dup, 'The user answered: "Q"="A", "Q"="B". Read the answers carefully.')!;
    expect(result.map((item) => item.answer)).toEqual([undefined, undefined]);
  });

  it('only trusts a complete answered envelope', () => {
    expect(parseAskUserQuestion(ask([q]), 'The user answered: "Q"="A". Read the answers carefully.')![0].answer).toBe('A');
    expect(parseAskUserQuestion(ask([q]), 'Error: expected "Q"="A"')![0].answer).toBeUndefined();
    expect(parseAskUserQuestion(ask([q]), 'The user answered: "Q"="use "=" here"')![0].answer).toBeUndefined();
  });

  it('renders duplicate questions and labels without throwing, and keeps raw output for errors', () => {
    const tool = { id: 't', name: 'AskUserQuestion', input: ask([q, q]), output: 'Error: denied', error: true } as ConversationTool;
    const { container } = render(ConversationMessage, { viewState: { tableOffsets: [], openTools: new Set<string>() }, text: '', tools: [tool] });
    expect(container.querySelectorAll('.conversation-question')).toHaveLength(2);
    expect(container.textContent).toContain('Error: denied');
    expect(container.textContent).not.toContain('Answered:');
  });
});
