import type { ConversationEntry, ConversationTool } from '$lib/types';

/** Preview budget for a single tool payload, in rendered lines. */
export const maxPayloadLines = 24;
/** Preview budget for a single tool payload, in characters. */
export const maxPayloadChars = 2000;

/**
 * conversationEntries reduces a recorded transcript to the compact view: every
 * user turn, notices, the latest assistant prose of each exchange, and any
 * assistant turn carrying an AskUserQuestion (keeping only its AskUserQuestion
 * tools). Compaction entries are skipped.
 *
 * Intermediate agent updates and tool activity belong to Full history. When the
 * retained answer also carries tools, the compact view projects them out
 * without mutating the recorded entry used by Full history.
 */
export function conversationEntries(recorded: ConversationEntry[]): ConversationEntry[] {
  const conversation: ConversationEntry[] = [];

  // An AskUserQuestion is the agent talking to the user, not tool noise.
  const isQuestion = (tool: ConversationTool) => tool.name === 'AskUserQuestion';
  const answerOnly = (entry: ConversationEntry): ConversationEntry => {
    if (!entry.tools?.length) return entry;
    const answer = { ...entry };
    const questions = entry.tools.filter(isQuestion);
    if (questions.length) answer.tools = questions;
    else delete answer.tools;
    return answer;
  };

  // Per exchange: the latest prose turn plus every question turn, in transcript order.
  let exchange: ConversationEntry[] = [];
  let latestProse: ConversationEntry | null = null;
  const flush = () => {
    for (const entry of exchange) {
      if (entry === latestProse || entry.tools?.some(isQuestion)) conversation.push(answerOnly(entry));
    }
    exchange = [];
    latestProse = null;
  };

  for (const entry of recorded) {
    if (entry.kind === 'compaction') continue;
    if (entry.kind === 'notice') {
      flush();
      conversation.push(entry);
      continue;
    }
    if (entry.role === 'user') {
      flush();
      conversation.push(entry);
      continue;
    }
    if (entry.text.trim()) latestProse = entry;
    exchange.push(entry);
  }
  flush();
  return conversation;
}

/**
 * formatToolPayload turns a tool's recorded payload into something readable.
 *
 * Agents record tool input as a serialised JSON object, so the raw value arrives
 * as one line with escaped newlines - a `Write` call embeds an entire file that
 * way. Decoding it restores real line breaks and puts each argument on its own
 * row. Anything that is not a JSON object is returned untouched, which covers
 * tool output: it is already plain text.
 */
export function formatToolPayload(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed.startsWith('{')) return raw;
  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed);
  } catch {
    return raw;
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return raw;
  const fields = Object.entries(parsed as Record<string, unknown>);
  if (!fields.length) return raw;
  return fields
    .map(([key, value]) => {
      let rendered: string;
      if (typeof value === 'string') rendered = value;
      else if (value === null || value === undefined) rendered = 'null';
      else if (typeof value === 'number' || typeof value === 'boolean') rendered = String(value);
      else rendered = JSON.stringify(value, null, 2);
      return rendered.includes('\n') ? `${key}:\n${rendered}` : `${key}: ${rendered}`;
    })
    .join('\n');
}

/**
 * clampPayload trims a payload to a preview. Recorded tool payloads reach tens
 * of kilobytes, which is unreadable on a phone and expensive to lay out, so the
 * card shows a preview until the reader asks for the rest.
 *
 * A first line longer than the character budget is cut mid-line rather than
 * dropped, so the preview is never empty.
 */
export function clampPayload(
  text: string,
  maxLines: number = maxPayloadLines,
  maxChars: number = maxPayloadChars,
): { preview: string; clamped: boolean } {
  const lines = text.split('\n');
  if (text.length <= maxChars && lines.length <= maxLines) return { preview: text, clamped: false };
  const head: string[] = [];
  let used = 0;
  for (const line of lines) {
    if (head.length >= maxLines) break;
    if (head.length && used + line.length + 1 > maxChars) break;
    head.push(line.length > maxChars ? line.slice(0, maxChars) : line);
    used += Math.min(line.length, maxChars) + 1;
  }
  return { preview: head.join('\n'), clamped: true };
}

export interface AskedQuestion {
  question: string;
  header: string;
  multiSelect: boolean;
  options: Array<{ label: string; description: string }>;
  answer?: string;
}

/** Structured view of a Claude AskUserQuestion call, or null for any other payload. */
export function parseAskUserQuestion(input: string, output = ''): AskedQuestion[] | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(input);
  } catch {
    return null;
  }
  const raw = (parsed as { questions?: unknown } | null)?.questions;
  if (!Array.isArray(raw) || !raw.length) return null;
  // Only a complete `Your questions have been answered:` or `The user answered:` envelope of
  // `"Q"="A"` pairs counts; anything ambiguous leaves the raw output visible instead of guessing.
  const answers = new Map<string, string>();
  const trimmedOutput = output.trim();
  // Claude Code 2.1.x writes `"Q"="A"` pairs without escaping, under one of two envelopes.
  const prefix = ['Your questions have been answered:', 'The user answered:'].find((value) => trimmedOutput.startsWith(value));
  if (prefix) {
    const pair = /\s*"([^"]*)"="([^"]*)"(,|\.|$)/y;
    pair.lastIndex = prefix.length;
    const found = new Map<string, string>();
    let match: RegExpExecArray | null;
    let complete = false;
    while ((match = pair.exec(trimmedOutput))) {
      if (found.has(match[1])) break; // duplicate question text: attribution is ambiguous
      found.set(match[1], match[2]);
      if (match[3] !== ',') {
        complete = true;
        break;
      }
    }
    if (complete) for (const [key, value] of found) answers.set(key, value);
  }
  const text = (value: unknown) => (typeof value === 'string' ? value : '');
  const questions = raw.map((item) => {
    const entry = (item && typeof item === 'object' ? item : {}) as Record<string, unknown>;
    const options = Array.isArray(entry.options) ? entry.options : [];
    const question = text(entry.question);
    return {
      question,
      header: text(entry.header),
      multiSelect: entry.multiSelect === true,
      options: options.map((option) => {
        const value = (option && typeof option === 'object' ? option : {}) as Record<string, unknown>;
        return { label: text(value.label), description: text(value.description) };
      }),
      answer: raw.filter((other) => text((other as Record<string, unknown> | null)?.question) === question).length === 1 ? answers.get(question) : undefined,
    };
  });
  return questions.every((item) => item.question) ? questions : null;
}

/** Whether an option label matches the recorded answer (comma-joined for multi-select). */
export function isChosenOption(item: AskedQuestion, label: string): boolean {
  if (item.answer === undefined) return false;
  if (item.answer === label) return true;
  return item.multiSelect && item.answer.split(', ').includes(label);
}

/** Id of the AskUserQuestion entry still waiting for a reply after the last user turn, if any. */
export function pendingQuestionEntryId(entries: ConversationEntry[]): string | null {
  for (let index = entries.length - 1; index >= 0; index -= 1) {
    const entry = entries[index];
    if (entry.role === 'user') return null;
    if (entry.tools?.some((tool) => tool.name === 'AskUserQuestion' && !tool.output && !tool.error)) return entry.id;
  }
  return null;
}
