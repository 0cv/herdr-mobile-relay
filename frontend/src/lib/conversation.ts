import type { ConversationEntry } from '$lib/types';

/** Preview budget for a single tool payload, in rendered lines. */
export const maxPayloadLines = 24;
/** Preview budget for a single tool payload, in characters. */
export const maxPayloadChars = 2000;

/**
 * conversationEntries reduces a recorded transcript to the compact view: every
 * user turn and only the latest assistant prose from each exchange.
 *
 * Intermediate agent updates and tool activity belong to Full history. When the
 * retained answer also carries tools, the compact view projects them out
 * without mutating the recorded entry used by Full history.
 */
export function conversationEntries(recorded: ConversationEntry[]): ConversationEntry[] {
  const conversation: ConversationEntry[] = [];
  let latestAssistant: ConversationEntry | null = null;

  const answerOnly = (entry: ConversationEntry): ConversationEntry => {
    if (!entry.tools?.length) return entry;
    const answer = { ...entry };
    delete answer.tools;
    return answer;
  };

  for (const entry of recorded) {
    if (entry.role === 'user') {
      if (latestAssistant) conversation.push(answerOnly(latestAssistant));
      latestAssistant = null;
      conversation.push(entry);
      continue;
    }
    if (entry.text.trim()) latestAssistant = entry;
  }
  if (latestAssistant) conversation.push(answerOnly(latestAssistant));
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
