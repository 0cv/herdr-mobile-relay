import type { ConversationEntry } from '$lib/types';

/** Preview budget for a single tool payload, in rendered lines. */
export const maxPayloadLines = 24;
/** Preview budget for a single tool payload, in characters. */
export const maxPayloadChars = 2000;

/**
 * conversationEntries reduces a recorded transcript to the compact view: every
 * user turn, every tool event, and only the latest assistant prose from each
 * exchange.
 *
 * Tool activity stays at its recorded position because it is useful even when
 * a later answer supersedes the prose around it. When a tool-bearing entry also
 * carries superseded prose, the compact view projects that entry to tools only;
 * Full history still receives the untouched recorded entry.
 */
export function conversationEntries(recorded: ConversationEntry[]): ConversationEntry[] {
  const conversation: ConversationEntry[] = [];
  let assistantExchange: ConversationEntry[] = [];

  const flushAssistantExchange = () => {
    let latestTextIndex = -1;
    for (let index = 0; index < assistantExchange.length; index += 1) {
      if (assistantExchange[index].text.trim()) latestTextIndex = index;
    }

    for (let index = 0; index < assistantExchange.length; index += 1) {
      const entry = assistantExchange[index];
      if (index === latestTextIndex) {
        conversation.push(entry);
        continue;
      }
      if (!entry.tools?.length) continue;
      if (!entry.text.trim()) {
        conversation.push(entry);
        continue;
      }
      const toolsOnly = { ...entry, text: '' };
      delete toolsOnly.truncated;
      conversation.push(toolsOnly);
    }
    assistantExchange = [];
  };

  for (const entry of recorded) {
    if (entry.role === 'user') {
      flushAssistantExchange();
      conversation.push(entry);
      continue;
    }
    assistantExchange.push(entry);
  }
  flushAssistantExchange();
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
