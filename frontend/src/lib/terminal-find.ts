export interface TerminalFindMatch {
  start: number;
  end: number;
}

export interface TerminalFindResult {
  matches: TerminalFindMatch[];
  truncated: boolean;
}

export interface TerminalFindRow {
  text: string;
}

export interface TerminalFindFragment {
  row: number;
  start: number;
  end: number;
}

export const TERMINAL_FIND_MATCH_LIMIT = 1_000;

export function terminalSearchText(rows: readonly TerminalFindRow[]): string {
  return rows.map((row) => row.text).join('\n');
}

export function findTerminalText(
  text: string,
  query: string,
  limit = TERMINAL_FIND_MATCH_LIMIT,
): TerminalFindResult {
  if (!query || limit < 1) return { matches: [], truncated: false };
  const escaped = query.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const expression = new RegExp(escaped, 'giu');
  const matches: TerminalFindMatch[] = [];
  let match: RegExpExecArray | null;
  while ((match = expression.exec(text)) !== null) {
    if (matches.length >= limit) return { matches, truncated: true };
    matches.push({ start: match.index, end: match.index + match[0].length });
  }
  return { matches, truncated: false };
}

export function terminalRowOffsets(rows: readonly TerminalFindRow[]): number[] {
  const offsets = new Array<number>(rows.length);
  let offset = 0;
  for (let index = 0; index < rows.length; index += 1) {
    offsets[index] = offset;
    offset += rows[index].text.length + (index < rows.length - 1 ? 1 : 0);
  }
  return offsets;
}

export function terminalRowForOffset(
  rows: readonly TerminalFindRow[],
  offsets: readonly number[],
  offset: number,
): number {
  if (!rows.length) return -1;
  let low = 0;
  let high = rows.length - 1;
  while (low <= high) {
    const middle = Math.floor((low + high) / 2);
    const start = offsets[middle] || 0;
    const end = start + rows[middle].text.length;
    if (offset < start) high = middle - 1;
    else if (offset > end && middle < rows.length - 1) low = middle + 1;
    else return middle;
  }
  return Math.max(0, Math.min(rows.length - 1, low));
}

export function terminalMatchFragments(
  rows: readonly TerminalFindRow[],
  offsets: readonly number[],
  match: TerminalFindMatch,
): TerminalFindFragment[] {
  if (!rows.length || match.end <= match.start) return [];
  const first = terminalRowForOffset(rows, offsets, match.start);
  const last = terminalRowForOffset(rows, offsets, Math.max(match.start, match.end - 1));
  const fragments: TerminalFindFragment[] = [];
  for (let row = first; row <= last; row += 1) {
    const rowStart = offsets[row];
    const start = Math.max(0, match.start - rowStart);
    const end = Math.min(rows[row].text.length, match.end - rowStart);
    if (end > start) fragments.push({ row, start, end });
  }
  return fragments;
}
