const ANSI_COLORS: Record<number, string> = {
  30: '#555', 31: '#ff5f5f', 32: '#5fd75f', 33: '#ffd75f',
  34: '#5fafff', 35: '#d75fff', 36: '#1abc9c', 37: '#e5e5e5',
  90: '#777', 91: '#ff8080', 92: '#80ff80', 93: '#ffff80',
  94: '#80bfff', 95: '#ff80ff', 96: '#80ffff', 97: '#fff',
};

export const TERMINAL_SEPARATOR_TOKEN = '\uE000HERDR_SEPARATOR\uE000';
const AGENT_CURRENT_UI_TOKEN = '\uE000HERDR_AGENT_CURRENT_UI\uE000';
const CODEX_PICKER_ITEM_PREFIX = '\uE000HERDR_CODEX_PICKER_ITEM:';
const CODEX_PICKER_HELP_PREFIX = '\uE000HERDR_CODEX_PICKER_HELP:';
export const TERMINAL_REPEATED_RUN_LIMIT = 24;
export const CLAUDE_DESKTOP_FOOTER_LINES = 6;
export const CLAUDE_DESKTOP_PROMPT_LINES = 2;
const TERMINAL_REPEATED_RUN_TRIGGER = 32;
const CODEX_DARK_ROW_BACKGROUND = 'rgb(61,64,64)';
const ANSI_HEADING_ACCENT = '#3daee9';

export function escapeHtml(text: unknown): string {
  return String(text ?? '').replace(/[&<>"']/g, (character) => ({
    '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
  })[character] || character);
}

export function stripAnsi(text: unknown): string {
  return String(text ?? '').replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, '');
}

export function trimAnsiLineEnd(line: unknown): string {
  const value = String(line ?? '');
  const match = value.match(/((?:\x1b\[[0-9;?]*[ -/]*[@-~])*)\r?$/);
  const end = match ? match.index : value.length;
  const suffix = match?.[1] || '';
  return value.slice(0, end).replace(/[ \t]+$/, '') + suffix;
}

export function reflowTerminalLines(content: unknown): string {
  const output: string[] = [];
  const structural = /^(?:[-*+] |\d+[.)] |[•›⚠✔✖└├┌│─━═]|```)/;
  let preserveAgentUiLayout = false;
  for (const line of String(content ?? '').split('\n')) {
    // Agent picker rows use two-space indentation intentionally; they are not
    // wrapped transcript continuations.
    if (line === AGENT_CURRENT_UI_TOKEN) preserveAgentUiLayout = true;
    const clean = stripAnsi(line);
    const trimmed = clean.trim();
    const indent = (clean.match(/^ */) || [''])[0].length;
    const previous = output.length ? stripAnsi(output[output.length - 1]) : '';
    const previousTrimmed = previous.trim();
    const previousIndent = (previous.match(/^ */) || [''])[0].length;
    const continuation = Boolean(
      trimmed
      && previousTrimmed
      && indent === 2
      && previousIndent <= 2
      && !preserveAgentUiLayout
      && !structural.test(trimmed)
      && !isSeparatorOnlyLine(line),
    );
    if (!continuation) {
      output.push(line);
      continue;
    }
    const next = line.replace(/^((?:\x1b\[[0-9;?]*[ -/]*[@-~])*) {2}/, '$1');
    output[output.length - 1] = `${trimAnsiLineEnd(output[output.length - 1])} ${next}`;
  }
  return output.join('\n');
}

export function isCodexStatusLine(line: string): boolean {
  const clean = stripAnsi(line).replace(/\s+/g, ' ').trim();
  return /\bContext\s+\d+%\s+used\b/i.test(clean) && /\bgpt[-\w.]*/i.test(clean);
}

export function isClaudeStatusLine(line: string): boolean {
  const clean = stripAnsi(line).replace(/\s+/g, ' ').trim();
  const model = /\b(claude|sonnet|opus|haiku|fable|mythos)\b/i.test(clean);
  const statusBar = /[·|•]/.test(clean) || /(^|\s)[.~]?\//.test(clean);
  return /\bctx\s*:?\s*(?:\d+%|-+)/i.test(clean) && (model || statusBar);
}

function isClaudeStatusFragment(line: string): boolean {
  const clean = stripAnsi(line).replace(/\s+/g, ' ').trim();
  if (!clean) return false;
  if (isClaudeStatusLine(clean)) return true;
  if (/\b(claude|sonnet|opus|haiku|fable|mythos)\b/i.test(clean) && /[·|•]/.test(clean)) return true;
  if (/(?:^|\s)[.~]?\/\S+/.test(clean) && (/\bon\b/i.test(clean) || /[·|•]/.test(clean))) return true;
  if (/\b(?:5h|7d)\s*:?[ \t]*(?:\d+%|-+)/i.test(clean) && /[·|•]/.test(clean)) return true;
  if (/\b(?:manual|plan)\s+mode\b/i.test(clean)) return true;
  return /\bPR\s*#?\d+\b/i.test(clean) || /\b\d+\s+agents?\b/i.test(clean);
}

export function removeClaudeStatusBlocks(content: string): string {
  const lines = String(content ?? '').split('\n');
  const hidden = new Set<number>();
  for (let index = 0; index < lines.length; index += 1) {
    if (!isClaudeStatusLine(lines[index])) continue;
    hidden.add(index);
    for (let offset = 1; offset <= 3 && index - offset >= 0; offset += 1) {
      if (!isClaudeStatusFragment(lines[index - offset])) break;
      hidden.add(index - offset);
    }
    for (let offset = 1; offset <= 3 && index + offset < lines.length; offset += 1) {
      if (!isClaudeStatusFragment(lines[index + offset])) break;
      hidden.add(index + offset);
    }
  }
  return lines.filter((_line, index) => !hidden.has(index)).join('\n');
}

export function claudeMobileTerminalContent(
  content: string,
  showStatusLine: boolean,
  footerLines = CLAUDE_DESKTOP_FOOTER_LINES,
  promptLines = CLAUDE_DESKTOP_PROMPT_LINES,
): { content: string; separated: boolean } {
  const lines = String(content ?? '').split('\n');
  const footerCount = Number.isInteger(footerLines) && footerLines > 0 ? footerLines : CLAUDE_DESKTOP_FOOTER_LINES;
  const promptCount = Number.isInteger(promptLines) && promptLines >= 0
    ? Math.min(promptLines, footerCount)
    : CLAUDE_DESKTOP_PROMPT_LINES;
  if (lines.length > footerCount * 2) {
    const body = lines.slice(0, -footerCount);
    const status = lines.slice(-footerCount + promptCount);
    return {
      content: [...body, ...(showStatusLine ? status : [])].join('\n'),
      separated: true,
    };
  }

  const tailStart = Math.max(0, lines.length - footerCount);
  for (let index = lines.length - 2; index >= tailStart; index -= 1) {
    if (!/^\s*[❯›]\s+\S/u.test(stripAnsi(lines[index]))) continue;
    if (!lines.slice(index + 1).some(isClaudeStatusFragment)) continue;
    return {
      content: [...lines.slice(0, index), ...(showStatusLine ? lines.slice(index + 1) : [])].join('\n'),
      separated: true,
    };
  }
  return { content, separated: false };
}

function isPiFooterStatsLine(line: string): boolean {
  const clean = stripAnsi(line).replace(/\s+/g, ' ').trim();
  return /(?:\d+(?:\.\d+)?%|\?)\/(?:\d+(?:\.\d+)?[kKmMgGtT]?|\?)(?:\s*\(auto\))?/.test(clean);
}

function isPiFooterPathLine(line: string): boolean {
  const clean = stripAnsi(line).trim();
  return /^(?:~(?:[\\/]|$)|\/|[A-Za-z]:[\\/])/.test(clean);
}

function piFooterBounds(lines: string[]): { path: number; stats: number } | null {
  const tailStart = Math.max(0, lines.length - 8);
  for (let stats = lines.length - 1; stats >= tailStart; stats -= 1) {
    if (!isPiFooterStatsLine(lines[stats])) continue;
    let path = stats - 1;
    while (path >= tailStart && !stripAnsi(lines[path]).trim()) path -= 1;
    if (path >= tailStart && isPiFooterPathLine(lines[path])) return { path, stats };
  }
  return null;
}

function piAutocompletePrimaryColumn(line: string): string {
  const clean = stripAnsi(line).replaceAll('\r', '').trimEnd();
  const columns = clean.match(/^((?:→ | {2}).*?\S)( {2,})(?=\S)/u);
  if (!columns) return trimAnsiLineEnd(line);
  return trimAnsiLineEnd(line.slice(0, rawIndexAtVisibleOffset(line, columns[1].length)));
}

function splitPiFooterStatsLine(line: string): string {
  const clean = stripAnsi(line).replaceAll('\r', '');
  const gaps = [...clean.matchAll(/(\S)( {2,})(?=\S)/g)];
  const gap = gaps.at(-1);
  if (gap?.index === undefined) return line;
  const start = gap.index + gap[1].length;
  const end = start + gap[2].length;
  const rawStart = rawIndexAtVisibleOffset(line, start);
  const rawEnd = rawIndexAtVisibleOffset(line, end);
  const stylePrefix = line.match(/^((?:\x1b\[[0-9;?]*[ -/]*[@-~])*)/)?.[1] || '';
  return `${trimAnsiLineEnd(line.slice(0, rawStart))}\n${stylePrefix}${line.slice(rawEnd)}`;
}

interface PiCurrentUi {
  start: number;
  end: number;
  defaultEditor: boolean;
}

function piCurrentUi(lines: string[], footerPath: number): PiCurrentUi | null {
  const borderStart = Math.max(0, footerPath - 80);
  const borders: number[] = [];
  for (let index = borderStart; index < footerPath; index += 1) {
    if (isSeparatorOnlyLine(lines[index])) borders.push(index);
  }
  if (borders.length < 2) return null;

  const start = borders[borders.length - 2];
  const end = borders[borders.length - 1];
  const afterBorder = lines.slice(end + 1, footerPath).some((line) => stripAnsi(line).trim());
  const editorRows = lines.slice(start + 1, end);
  const hasCursor = editorRows.some((line) => /\x1b\[7m/.test(line));
  const selectorHint = editorRows.some((line) => (
    /(?:Esc|escape)\s+(?:to\s+)?(?:cancel|close|go back)|(?:Enter|Space).*\b(?:change|select|confirm)\b/i
      .test(stripAnsi(line))
  ));
  return { start, end, defaultEditor: afterBorder || (hasCursor && !selectorHint) };
}

export function piTerminalDraft(content: string): string | null {
  const lines = String(content ?? '').split('\n');
  const footer = piFooterBounds(lines);
  if (!footer) return null;
  const currentUi = piCurrentUi(lines, footer.path);
  if (!currentUi?.defaultEditor) return null;
  return lines.slice(currentUi.start + 1, currentUi.end)
    .map((line) => stripAnsi(line).replace(/\r/g, '').trimEnd())
    .join('\n')
    .replace(/\n+$/g, '');
}

// The browser composer replaces Pi's native desktop editor. Keep custom Pi
// selectors interactive and reduce autocomplete to a mobile-sized column.
export function piMobileTerminalContent(content: string, showStatusLine: boolean): string {
  const lines = String(content ?? '').split('\n');
  const footer = piFooterBounds(lines);
  if (!footer) return content;
  const currentUi = piCurrentUi(lines, footer.path);

  if (currentUi?.defaultEditor) {
    const autocomplete = lines.slice(currentUi.end + 1, footer.path).map(piAutocompletePrimaryColumn);
    const status = showStatusLine ? lines.slice(footer.path) : [];
    if (showStatusLine) status[footer.stats - footer.path] = splitPiFooterStatsLine(lines[footer.stats]);
    const mobileUi = [...autocomplete, ...status];
    return [
      ...lines.slice(0, currentUi.start),
      ...(mobileUi.some((line) => stripAnsi(line).trim()) ? [AGENT_CURRENT_UI_TOKEN, ...mobileUi] : []),
    ].join('\n');
  }

  if (showStatusLine) lines[footer.stats] = splitPiFooterStatsLine(lines[footer.stats]);
  else lines.splice(footer.path);
  if (currentUi) lines.splice(currentUi.start, 0, AGENT_CURRENT_UI_TOKEN);
  return lines.join('\n');
}

interface CodexDesktopInput {
  start: number;
  end: number;
  prompt: number;
}

function isCodexPickerTail(lines: string[]): boolean {
  return lines.some((line) => (
    /\benter\s+insert\b.*\besc\s+close\b/i.test(stripAnsi(line))
    || /^\s*[>›]?\s*\S.*\s(?:Skill|Plugin)\s*$/u.test(stripAnsi(line))
  ));
}

function codexDesktopInput(lines: string[]): CodexDesktopInput | null {
  const tailStart = Math.max(0, lines.length - 60);
  for (let end = lines.length - 1; end >= tailStart; end -= 1) {
    if (!hasAnsiBackgroundStyle(lines[end])) continue;
    let start = end;
    while (start > tailStart && hasAnsiBackgroundStyle(lines[start - 1])) start -= 1;
    const promptOffset = lines.slice(start, end + 1)
      .findIndex((line) => /^\s*[❯›](?:\s|$)/u.test(stripAnsi(line)));
    if (promptOffset < 0) {
      end = start;
      continue;
    }
    const after = lines.slice(end + 1);
    if (after.some(isCodexStatusLine) || isCodexPickerTail(after)) {
      return { start, end, prompt: start + promptOffset };
    }
    end = start;
  }
  return null;
}

export function codexPickerActive(content: string): boolean {
  const lines = String(content ?? '').split('\n');
  const input = codexDesktopInput(lines);
  return Boolean(input && isCodexPickerTail(lines.slice(input.end + 1)));
}

export function codexTerminalDraft(content: string): string | null {
  const lines = String(content ?? '').split('\n');
  const input = codexDesktopInput(lines);
  if (!input) return null;
  const promptLine = lines[input.prompt];
  const cleanPrompt = stripAnsi(promptLine).replace(/\r/g, '');
  const marker = cleanPrompt.match(/^\s*[❯›]\s?/u);
  if (!marker) return null;
  const markerWidth = marker[0].replace(/\s+$/u, '').length;
  const rawAfterMarker = promptLine.slice(rawIndexAtVisibleOffset(promptLine, markerWidth));
  if (hasAnsiDimStyle(rawAfterMarker)) return '';

  const rows = lines.slice(input.prompt, input.end + 1)
    .map((line) => stripAnsi(line).replace(/\r/g, '').trimEnd());
  rows[0] = rows[0].replace(/^\s*[❯›]\s?/u, '');
  while (rows.length && !rows[rows.length - 1]) rows.pop();
  return rows.join('\n');
}

interface CodexPickerItem {
  selected: boolean;
  name: string;
  description: string;
  type: string;
}

interface CodexPickerHelp {
  instructions: string;
  modes: Array<{ label: string; active: boolean }>;
}

function encodeTerminalPayload(prefix: string, payload: CodexPickerItem | CodexPickerHelp): string {
  return `${prefix}${encodeURIComponent(JSON.stringify(payload))}`;
}

function decodeTerminalPayload<T>(line: string, prefix: string): T | null {
  if (!line.startsWith(prefix)) return null;
  try {
    return JSON.parse(decodeURIComponent(line.slice(prefix.length))) as T;
  } catch {
    return null;
  }
}

function compactCodexPickerLine(line: string): string {
  const clean = stripAnsi(line).replaceAll('\r', '').trimEnd();
  if (/\benter\s+insert\b.*\besc\s+close\b/i.test(clean)) {
    const gaps = [...clean.matchAll(/ {3,}/g)];
    const gap = gaps.sort((left, right) => right[0].length - left[0].length)[0];
    const splitAt = gap?.index ?? clean.length;
    const instructions = clean.slice(0, splitAt).trim();
    const modes = clean.slice(splitAt + (gap?.[0].length ?? 0)).trim()
      .split(/ {2,}/)
      .filter(Boolean)
      .map((mode) => ({
        label: mode.replace(/^\[|\]$/g, ''),
        active: mode.startsWith('[') && mode.endsWith(']'),
      }));
    return encodeTerminalPayload(CODEX_PICKER_HELP_PREFIX, { instructions, modes });
  }

  const item = clean.match(/^(> | {2})(.*?\S) {2,}(.*?\S) {2,}(\S+)\s*$/u);
  if (item) {
    return encodeTerminalPayload(CODEX_PICKER_ITEM_PREFIX, {
      selected: item[1] === '> ',
      name: item[2],
      description: item[3],
      type: item[4],
    });
  }
  return trimAnsiLineEnd(line);
}

function codexMobileTerminalContent(content: string): string {
  const lines = String(content ?? '').split('\n');
  const input = codexDesktopInput(lines);
  if (!input) return content;
  const tail = lines.slice(input.end + 1);
  const statusOffset = tail.findIndex(isCodexStatusLine);
  const pickerEnd = statusOffset >= 0 ? statusOffset : tail.length;
  const mobileTail = [
    ...tail.slice(0, pickerEnd).map(compactCodexPickerLine),
    ...tail.slice(pickerEnd),
  ];
  return [
    ...lines.slice(0, input.start),
    ...(mobileTail.some((line) => stripAnsi(line).trim()) ? [AGENT_CURRENT_UI_TOKEN, ...mobileTail] : []),
  ].join('\n');
}

export function removeCodexDesktopInput(content: string): string {
  const lines = String(content ?? '').split('\n');
  const input = codexDesktopInput(lines);
  if (!input) return content;
  lines.splice(input.start, input.end - input.start + 1);
  return lines.join('\n');
}

function hasAnsiDimStyle(line: string): boolean {
  for (const match of line.matchAll(/\x1b\[([0-9;]*)m/g)) {
    const codes = match[1] ? match[1].split(';').map(Number) : [0];
    for (let position = 0; position < codes.length; position += 1) {
      const code = codes[position];
      if (code === 2) return true;
      if (code !== 38 && code !== 48) continue;
      if (codes[position + 1] === 2) position += 4;
      else if (codes[position + 1] === 5) position += 2;
    }
  }
  return false;
}

function hasAnsiBackgroundStyle(line: string): boolean {
  for (const match of line.matchAll(/\x1b\[([0-9;]*)m/g)) {
    const codes = match[1] ? match[1].split(';').map(Number) : [0];
    for (let position = 0; position < codes.length; position += 1) {
      const code = codes[position];
      if (code === 48 || (code >= 40 && code <= 47) || (code >= 100 && code <= 107)) return true;
      if (code !== 38) continue;
      if (codes[position + 1] === 2) position += 4;
      else if (codes[position + 1] === 5) position += 2;
    }
  }
  return false;
}

export function terminalDisplayContent(content: unknown, showStatusLine: boolean, trimFrameEdges = false): string {
  const normalized = String(content ?? '').split('\n').map((line) => trimTerminalChrome(line, trimFrameEdges)).join('\n');
  const reflowed = reflowTerminalLines(normalized);
  if (showStatusLine) return reflowed;
  const withoutClaudeStatus = trimFrameEdges ? removeClaudeStatusBlocks(reflowed) : reflowed;
  return withoutClaudeStatus.split('\n')
    .filter((line) => !isCodexStatusLine(line) && !isClaudeStatusLine(line))
    .join('\n');
}

export function isSeparatorOnlyLine(line: string): boolean {
  const characters = [...stripAnsi(line).replace(/\s+/g, '')];
  const isRepeatedSymbol = (run: string[]) => Boolean(
    run.length >= 8
    && !/[\p{L}\p{N}]/u.test(run[0])
    && run.every((character) => character === run[0]),
  );
  if (isRepeatedSymbol(characters)) return true;
  return characters.length >= 10
    && !/[\p{L}\p{N}]/u.test(characters[0])
    && !/[\p{L}\p{N}]/u.test(characters.at(-1) || '')
    && isRepeatedSymbol(characters.slice(1, -1));
}

function rawIndexAtVisibleOffset(line: string, target: number): number {
  let rawIndex = 0;
  let visibleIndex = 0;
  while (rawIndex < line.length && visibleIndex < target) {
    const ansi = line.slice(rawIndex).match(/^\x1b\[[0-9;?]*[ -/]*[@-~]/);
    if (ansi) {
      rawIndex += ansi[0].length;
      continue;
    }
    const width = (line.codePointAt(rawIndex) || 0) > 0xFFFF ? 2 : 1;
    rawIndex += width;
    visibleIndex += width;
  }
  return rawIndex;
}

export function trimTrailingDecoration(line: string): string {
  const clean = stripAnsi(line);
  const decoration = clean.match(/[ \t]+([^\p{L}\p{N}\s])\1{7,}(?:[^\p{L}\p{N}\s])?[ \t]*\r?$/u);
  if (decoration?.index === undefined) return line;
  return trimAnsiLineEnd(line.slice(0, rawIndexAtVisibleOffset(line, decoration.index)));
}

function trimLeadingDecoration(line: string): string {
  const clean = stripAnsi(line);
  const decoration = clean.match(/^[ \t]*(?:[^\p{L}\p{N}\s])?([^\p{L}\p{N}\s])\1{7,}(?:[^\p{L}\p{N}\s])?[ \t]+/u);
  if (!decoration) return line;
  const remainder = line.slice(rawIndexAtVisibleOffset(line, decoration[0].length));
  if (!isClaudeStatusLine(remainder) && !isAgentTurnDurationLine(remainder)) return line;
  return remainder;
}

export function compactRepeatedCharacterRuns(line: string): string {
  const repeated = new RegExp(`([^\\p{L}\\p{N}\\s])\\1{${TERMINAL_REPEATED_RUN_TRIGGER},}`, 'gu');
  return line.replace(repeated, (_run, character: string) => character.repeat(TERMINAL_REPEATED_RUN_LIMIT));
}

export function trimTerminalChrome(line: string, trimFrameEdges = true): string {
  let trimmed = trimLeadingDecoration(trimTrailingDecoration(line));
  if (!trimFrameEdges) return trimAnsiLineEnd(trimmed);
  const vertical = '[│┃┆┇┊┋╎╏║]';
  const leading = stripAnsi(trimmed).match(new RegExp(`^[ \\t]*${vertical}[ \\t]{0,2}`, 'u'));
  if (leading) trimmed = trimmed.slice(rawIndexAtVisibleOffset(trimmed, leading[0].length));
  const clean = stripAnsi(trimmed);
  const trailing = clean.match(new RegExp(`[ \\t]*${vertical}[ \\t]*\\r?$`, 'u'));
  if (trailing?.index !== undefined) {
    trimmed = trimmed.slice(0, rawIndexAtVisibleOffset(trimmed, trailing.index));
  }
  return trimAnsiLineEnd(trimmed);
}

export function compactSeparatorLines(content: unknown, trimFrameEdges = false): string {
  const output: string[] = [];
  let pendingBlankLines = 0;
  let previousContentWasSeparator = false;
  let preserveAgentUiLayout = false;
  const flushBlankLines = () => {
    for (let index = 0; index < Math.min(pendingBlankLines, 2); index += 1) output.push('');
    pendingBlankLines = 0;
  };
  for (const rawLine of String(content ?? '').split('\n')) {
    const line = trimTerminalChrome(rawLine, trimFrameEdges);
    if (line === AGENT_CURRENT_UI_TOKEN) {
      flushBlankLines();
      output.push(line);
      previousContentWasSeparator = false;
      preserveAgentUiLayout = true;
      continue;
    }
    if (preserveAgentUiLayout) {
      if (!stripAnsi(line).trim()) output.push(line);
      else if (isSeparatorOnlyLine(line)) output.push(TERMINAL_SEPARATOR_TOKEN);
      else output.push(compactRepeatedCharacterRuns(trimTrailingDecoration(line)));
      continue;
    }
    if (!stripAnsi(line).trim()) {
      pendingBlankLines += 1;
      continue;
    }
    if (isSeparatorOnlyLine(line)) {
      if (previousContentWasSeparator) {
        pendingBlankLines = 0;
        continue;
      }
      flushBlankLines();
      output.push(TERMINAL_SEPARATOR_TOKEN);
      previousContentWasSeparator = true;
      continue;
    }
    flushBlankLines();
    output.push(compactRepeatedCharacterRuns(trimTrailingDecoration(line)));
    previousContentWasSeparator = false;
  }
  flushBlankLines();
  return output.join('\n');
}

export function isAgentTurnDurationLine(line: string): boolean {
  const clean = stripAnsi(line).replaceAll(TERMINAL_SEPARATOR_TOKEN, '').trim();
  return /^[^\p{L}\p{N}]*\p{L}+(?:ed|ing)\s+for\s+(?:\d+h\s*)?(?:\d+m\s*)?\d+s\b/iu.test(clean);
}

export function lastCompletedResponse(content: unknown): string {
  const lines = stripAnsi(content)
    .replaceAll(AGENT_CURRENT_UI_TOKEN, '')
    .replace(/\r/g, '')
    .split('\n')
    .map((line) => line.replaceAll(TERMINAL_SEPARATOR_TOKEN, '').replace(/[ \t]+$/g, ''));
  let end = -1;
  for (let index = lines.length - 1; index >= 0; index -= 1) {
    if (isAgentTurnDurationLine(lines[index])) {
      end = index;
      break;
    }
  }
  if (end < 0) return '';
  let start = -1;
  for (let index = end - 1; index >= 0; index -= 1) {
    if (/^\s*[•●]\s+\S/.test(lines[index])) {
      start = index;
      break;
    }
    if (isAgentTurnDurationLine(lines[index])) break;
  }
  if (start < 0) return '';
  const response = lines.slice(start, end);
  response[0] = response[0].replace(/^\s*[•●]\s+/, '');
  for (let index = 1; index < response.length; index += 1) {
    response[index] = response[index].replace(/^ {2}/, '');
  }
  while (response.length && !response[response.length - 1].trim()) response.pop();
  return response.join('\n').trim();
}

function ansiColorChannels(color: string): number[] | null {
  const value = color.trim();
  const hex = value.match(/^#([0-9a-f]{3}|[0-9a-f]{6})$/i);
  if (hex) {
    const digits = hex[1].length === 3 ? [...hex[1]].map((character) => character + character).join('') : hex[1];
    return [0, 2, 4].map((offset) => Number.parseInt(digits.slice(offset, offset + 2), 16));
  }
  const rgb = value.match(/^rgb\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*\)$/i);
  return rgb ? rgb.slice(1).map(Number) : null;
}

export function isNearWhiteAnsiColor(color: string): boolean {
  const channels = ansiColorChannels(color);
  return Boolean(channels && Math.min(...channels) >= 220 && Math.max(...channels) - Math.min(...channels) <= 40);
}

function ansiRelativeLuminance(color: string): number | null {
  const channels = ansiColorChannels(color);
  if (!channels) return null;
  const linear = channels.map((channel) => {
    const value = channel / 255;
    return value <= 0.04045 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2];
}

function normalizedAnsiBackground(color: string, normalize: boolean): string {
  return normalize && isNearWhiteAnsiColor(color) ? CODEX_DARK_ROW_BACKGROUND : color;
}

function normalizedAnsiForeground(color: string, normalize: boolean): string {
  if (!normalize) return color;
  const channels = ansiColorChannels(color);
  const luminance = ansiRelativeLuminance(color);
  if (!channels || luminance === null) return color;
  const spread = Math.max(...channels) - Math.min(...channels);
  if (Math.max(...channels) <= 96 && spread <= 30) return 'var(--terminal-text)';
  if (luminance < 0.14) {
    return `color-mix(in srgb, ${color} 35%, var(--terminal-text))`;
  }
  return color;
}

export function ansi256Color(index: number): string {
  const value = Number(index);
  if (!Number.isInteger(value) || value < 0 || value > 255) return '';
  if (value < 8) return ANSI_COLORS[30 + value];
  if (value < 16) return ANSI_COLORS[90 + value - 8];
  if (value < 232) {
    const offset = value - 16;
    const levels = [0, 95, 135, 175, 215, 255];
    return `rgb(${levels[Math.floor(offset / 36)]},${levels[Math.floor((offset % 36) / 6)]},${levels[offset % 6]})`;
  }
  const gray = 8 + (value - 232) * 10;
  return `rgb(${gray},${gray},${gray})`;
}

function ansiStyleName(name: string): string {
  return ({
    fontWeight: 'font-weight',
    fontStyle: 'font-style',
    textDecoration: 'text-decoration',
    backgroundColor: 'background-color',
  } as Record<string, string>)[name] || name;
}

export function ansiToHtml(
  text: string,
  normalizeNearWhiteBackground = false,
  normalizeNearBlackForeground = false,
): string {
  let html = '';
  let open = false;
  let styles: Record<string, string> = {};
  const parts = text.split(/\x1b\[([0-9;]*)m/g);
  for (let index = 0; index < parts.length; index += 1) {
    if (index % 2 === 0) {
      html += escapeHtml(parts[index]);
      continue;
    }
    if (open) {
      html += '</span>';
      open = false;
    }
    const codes = parts[index] ? parts[index].split(';').map(Number) : [0];
    if (codes.includes(0)) styles = {};
    for (let position = 0; position < codes.length; position += 1) {
      const code = codes[position];
      if (code === 1) styles.fontWeight = '700';
      else if (code === 2) styles.opacity = '0.7';
      else if (code === 3) styles.fontStyle = 'italic';
      else if (code === 4) styles.textDecoration = 'underline';
      else if (code === 22) {
        delete styles.fontWeight;
        delete styles.opacity;
      } else if (code === 23) delete styles.fontStyle;
      else if (code === 24) delete styles.textDecoration;
      else if (code === 39) delete styles.color;
      else if (code === 49) delete styles.backgroundColor;
      else if (code === 38 || code === 48) {
        let color = '';
        let consumed = 0;
        if (codes[position + 1] === 2 && codes.length > position + 4) {
          color = `rgb(${codes[position + 2]},${codes[position + 3]},${codes[position + 4]})`;
          consumed = 4;
        } else if (codes[position + 1] === 5 && codes.length > position + 2) {
          color = ansi256Color(codes[position + 2]);
          consumed = 2;
        }
        if (color) {
          if (code === 38) styles.color = normalizedAnsiForeground(color, normalizeNearBlackForeground);
          else styles.backgroundColor = normalizedAnsiBackground(color, normalizeNearWhiteBackground);
          position += consumed;
        }
      } else if (ANSI_COLORS[code]) {
        styles.color = normalizedAnsiForeground(ANSI_COLORS[code], normalizeNearBlackForeground);
      } else if (ANSI_COLORS[code - 10]) {
        styles.backgroundColor = normalizedAnsiBackground(ANSI_COLORS[code - 10], normalizeNearWhiteBackground);
      }
    }
    const effective = styles.fontStyle === 'italic' && styles.fontWeight === '700' && !styles.color
      ? { ...styles, color: ANSI_HEADING_ACCENT }
      : styles;
    const style = Object.entries(effective).map(([name, value]) => `${ansiStyleName(name)}:${value}`).join(';');
    if (style) {
      html += `<span style="${style}">`;
      open = true;
    }
  }
  if (open) html += '</span>';
  return html;
}

function restoreAgentActivityColors(text: string): string {
  return text.replace(/\x1b\[1mExplored\x1b\[0m/g, '\x1b[1;38;5;4mExplored\x1b[0m');
}

function restoreClaudeHeadingColors(text: string): string {
  let restored = text.replace(
    /(^|\n)([ \t]*(?:\x1b\[0m)*)\x1b\[1m([^\x1b\n]{1,160}:)\x1b\[0m/g,
    '$1$2\x1b[1;38;2;56;162;223m$3\x1b[0m',
  );
  restored = restored.replace(
    /(^|\n)([ \t]*(?:\x1b\[0m)*)\x1b\[1m([^\x1b\n]{1,160})\x1b\[0m:(?=[ \t]|\n|$)/g,
    '$1$2\x1b[1;38;2;56;162;223m$3:\x1b[0m',
  );
  restored = restored.replace(
    /(^|\n)([ \t]*-[ \t]+(?:\x1b\[0m)*)\x1b\[1m([^\x1b\n]{1,160})\x1b\[0m/g,
    '$1$2\x1b[1;38;2;56;162;223m$3\x1b[0m',
  );
  return restored.replace(
    /(^|\n)([ \t]*(?:\x1b\[0m)*)\x1b\[1m([^\x1b\n]{1,80})\x1b\[0m([ \t]*)(?=\n|$)/g,
    (match, lineStart, indentation, label, trailing) => {
      const trimmed = String(label).trimEnd();
      if (!trimmed || /[.!?;:]$/.test(trimmed)) return match;
      return `${lineStart}${indentation}\x1b[1;38;2;56;162;223m${trimmed}\x1b[0m${trailing}`;
    },
  );
}

export function ansiLineBackground(line: string): string {
  let background = '';
  const parts = line.split(/\x1b\[([0-9;]*)m/g);
  for (let index = 0; index < parts.length; index += 1) {
    if (index % 2 === 0) {
      if (parts[index].replaceAll('\r', '').trim().length) return background;
      continue;
    }
    const codes = parts[index] ? parts[index].split(';').map(Number) : [0];
    if (codes.includes(0)) background = '';
    for (let position = 0; position < codes.length; position += 1) {
      const code = codes[position];
      if (code === 38) {
        if (codes[position + 1] === 2 && codes.length > position + 4) position += 4;
        else if (codes[position + 1] === 5 && codes.length > position + 2) position += 2;
      } else if (code === 49) background = '';
      else if (code === 48) {
        if (codes[position + 1] === 2 && codes.length > position + 4) {
          background = `rgb(${codes[position + 2]},${codes[position + 3]},${codes[position + 4]})`;
          position += 4;
        } else if (codes[position + 1] === 5 && codes.length > position + 2) {
          background = ansi256Color(codes[position + 2]);
          position += 2;
        }
      } else if (ANSI_COLORS[code - 10]) background = ANSI_COLORS[code - 10];
    }
  }
  return background;
}

export function ansiLineBackgroundIndent(line: string): number {
  let visiblePrefix = '';
  const parts = line.split(/\x1b\[([0-9;]*)m/g);
  for (let index = 0; index < parts.length; index += 1) {
    if (index % 2 === 0) {
      visiblePrefix += parts[index].replaceAll('\r', '');
      continue;
    }
    const codes = parts[index] ? parts[index].split(';').map(Number) : [0];
    for (let position = 0; position < codes.length; position += 1) {
      const code = codes[position];
      if (code === 38) {
        if (codes[position + 1] === 2 && codes.length > position + 4) position += 4;
        else if (codes[position + 1] === 5 && codes.length > position + 2) position += 2;
        continue;
      }
      if (code === 48 || ANSI_COLORS[code - 10]) {
        return visiblePrefix.trim() ? 0 : visiblePrefix.replaceAll('\t', '    ').length;
      }
    }
  }
  return 0;
}

export function ansiLineBackgroundStyle(line: string, background: string): string {
  const indent = ansiLineBackgroundIndent(line);
  if (!indent) return `background-color:${background}`;
  const edge = `${indent}ch`;
  return `background-image:linear-gradient(to right,transparent 0 ${edge},${background} ${edge});padding-left:${edge};text-indent:-${edge}`;
}

export function ansiLineBackgrounds(lines: string[]): string[] {
  const backgrounds = lines.map(ansiLineBackground);
  for (let start = 1; start < lines.length - 1; start += 1) {
    if (backgrounds[start] || stripAnsi(lines[start]).trim()) continue;
    let end = start;
    while (end + 1 < lines.length && !backgrounds[end + 1] && !stripAnsi(lines[end + 1]).trim()) end += 1;
    const previous = backgrounds[start - 1];
    const next = backgrounds[end + 1];
    if (previous && previous === next) backgrounds.fill(previous, start, end + 1);
    start = end;
  }
  return backgrounds;
}

function codexPickerDisplayLine(line: string): string {
  const item = decodeTerminalPayload<CodexPickerItem>(line, CODEX_PICKER_ITEM_PREFIX);
  if (item) return `${item.selected ? '> ' : '  '}${item.name} — ${item.description} (${item.type})`;
  const help = decodeTerminalPayload<CodexPickerHelp>(line, CODEX_PICKER_HELP_PREFIX);
  if (!help) return line;
  const modes = help.modes.map((mode) => mode.active ? `[${mode.label}]` : mode.label).join(' · ');
  return [help.instructions, modes ? `  ${modes}` : ''].filter(Boolean).join('\n');
}

function codexPickerHtml(line: string): string | null {
  const item = decodeTerminalPayload<CodexPickerItem>(line, CODEX_PICKER_ITEM_PREFIX);
  if (item) {
    const selected = item.selected ? ' selected' : '';
    return `<span class="codex-picker-item${selected}"><span class="codex-picker-name">${escapeHtml(item.name)}</span><span class="codex-picker-type">${escapeHtml(item.type)}</span><span class="codex-picker-description">${escapeHtml(item.description)}</span></span>`;
  }
  const help = decodeTerminalPayload<CodexPickerHelp>(line, CODEX_PICKER_HELP_PREFIX);
  if (!help) return null;
  const modes = help.modes.map((mode) => (
    `<span class="codex-picker-mode${mode.active ? ' active' : ''}">${escapeHtml(mode.label)}</span>`
  )).join('');
  return `<span class="codex-picker-help"><span>${escapeHtml(help.instructions)}</span>${modes ? `<span class="codex-picker-modes" aria-label="Search modes">${modes}</span>` : ''}</span>`;
}

export function terminalHtml(
  text: string,
  normalizeLightPalette = false,
  restoreClaudeHeadings = false,
): string {
  let colored = restoreAgentActivityColors(text);
  if (restoreClaudeHeadings) colored = restoreClaudeHeadingColors(colored);
  const lines = colored.split('\n');
  const backgrounds = ansiLineBackgrounds(lines);
  return lines.map((line, index) => {
    if (line === TERMINAL_SEPARATOR_TOKEN) return '<span class="term-separator" aria-hidden="true"></span>';
    if (line === AGENT_CURRENT_UI_TOKEN) return '<span class="agent-current-ui-start" aria-hidden="true"></span>';
    const picker = codexPickerHtml(line);
    if (picker) return picker;
    const sourceBackground = backgrounds[index];
    const normalizeRow = normalizeLightPalette && isNearWhiteAnsiColor(sourceBackground);
    const normalizeDarkText = normalizeLightPalette && (!sourceBackground || normalizeRow);
    const background = normalizedAnsiBackground(sourceBackground, normalizeRow);
    const className = background ? ' ansi-line-background' : '';
    const style = background ? ` style="${ansiLineBackgroundStyle(line, background)}"` : '';
    // ansiToHtml escapes every text segment before it emits controlled span markup.
    return `<span class="ansi-line${className}"${style}>${ansiToHtml(trimAnsiLineEnd(line), normalizeRow, normalizeDarkText)}</span>`;
  }).join('');
}

export function renderTerminalContent(
  content: string,
  format: string,
  agentType: string,
  showStatusLine: boolean,
): { display: string; html: string } {
  const claudeChrome = /\bclaude\b/i.test(agentType);
  const codexChrome = /\bcodex\b/i.test(agentType);
  const piChrome = /\bpi\b/i.test(agentType);
  let mobileContent = codexChrome ? codexMobileTerminalContent(content) : content;
  if (piChrome) mobileContent = piMobileTerminalContent(mobileContent, showStatusLine);
  const markedDisplay = compactSeparatorLines(
    terminalDisplayContent(mobileContent, showStatusLine, claudeChrome),
    claudeChrome,
  );
  const display = markedDisplay.split('\n')
    .filter((line) => line !== AGENT_CURRENT_UI_TOKEN)
    .map(codexPickerDisplayLine)
    .join('\n');
  if (format !== 'ansi') {
    return { display, html: escapeHtml(display.replaceAll(TERMINAL_SEPARATOR_TOKEN, '────────')) };
  }
  return {
    display,
    html: terminalHtml(markedDisplay, true, claudeChrome),
  };
}
