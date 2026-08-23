import type { ConversationEntry } from '$lib/types';

/** Preview budget for a single tool payload, in rendered lines. */
export const maxPayloadLines = 24;
/** Preview budget for a single tool payload, in characters. */
export const maxPayloadChars = 2000;

/**
 * conversationEntries reduces a recorded transcript to the compact view: every
 * user turn, plus the agent work between them.
 *
 * Prose answers collapse - only the most recent text-bearing assistant entry of
 * an exchange survives, because an agent that revises its answer supersedes the
 * earlier draft. Tool calls do not collapse: each one is a distinct event, and
 * agents that emit one text-less assistant entry per tool call (Claude Code does
 * this for every Bash/Write/MCP invocation) would otherwise have the majority of
 * their transcript discarded before rendering.
 *
 * A pending answer is flushed when a tool entry arrives so that the rendered
 * order stays chronological.
 */
export function conversationEntries(recorded: ConversationEntry[]): ConversationEntry[] {
  const conversation: ConversationEntry[] = [];
  let latestAssistant: ConversationEntry | null = null;
  const flush = () => {
    if (latestAssistant) conversation.push(latestAssistant);
    latestAssistant = null;
  };
  for (const entry of recorded) {
    if (entry.role === 'user') {
      flush();
      conversation.push(entry);
      continue;
    }
    // An entry carrying both prose and tools is one message; the renderer draws
    // both parts, so it must not also be pushed as a separate tool event.
    if (entry.text.trim()) {
      latestAssistant = entry;
      continue;
    }
    if (entry.tools?.length) {
      flush();
      conversation.push(entry);
    }
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
