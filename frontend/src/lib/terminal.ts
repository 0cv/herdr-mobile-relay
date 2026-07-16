const ANSI_COLORS: Record<number, string> = {
  30: '#555', 31: '#ff5f5f', 32: '#5fd75f', 33: '#ffd75f',
  34: '#5fafff', 35: '#d75fff', 36: '#1abc9c', 37: '#e5e5e5',
  90: '#777', 91: '#ff8080', 92: '#80ff80', 93: '#ffff80',
  94: '#80bfff', 95: '#ff80ff', 96: '#80ffff', 97: '#fff',
};

export const TERMINAL_SEPARATOR_TOKEN = '\uE000HERDR_SEPARATOR\uE000';
export const TERMINAL_REPEATED_RUN_LIMIT = 24;
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
  for (const line of String(content ?? '').split('\n')) {
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
  return /\bctx\s*:?\s*\d+%/i.test(clean) && (model || statusBar);
}

export function terminalDisplayContent(content: unknown, showStatusLine: boolean, trimFrameEdges = false): string {
  const normalized = String(content ?? '').split('\n').map((line) => trimTerminalChrome(line, trimFrameEdges)).join('\n');
  const reflowed = reflowTerminalLines(normalized);
  if (showStatusLine) return reflowed;
  return reflowed.split('\n').filter((line) => !isCodexStatusLine(line) && !isClaudeStatusLine(line)).join('\n');
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
  const flushBlankLines = () => {
    for (let index = 0; index < Math.min(pendingBlankLines, 2); index += 1) output.push('');
    pendingBlankLines = 0;
  };
  for (const rawLine of String(content ?? '').split('\n')) {
    const line = trimTerminalChrome(rawLine, trimFrameEdges);
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

function isNearBlackAnsiColor(color: string): boolean {
  const channels = ansiColorChannels(color);
  return Boolean(channels && Math.max(...channels) <= 40);
}

function normalizedAnsiBackground(color: string, normalize: boolean): string {
  return normalize && isNearWhiteAnsiColor(color) ? CODEX_DARK_ROW_BACKGROUND : color;
}

function normalizedAnsiForeground(color: string, normalize: boolean): string {
  return normalize && isNearBlackAnsiColor(color) ? 'var(--terminal-text)' : color;
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

export function terminalHtml(
  text: string,
  normalizeCodexLightRows = false,
  restoreClaudeHeadings = false,
): string {
  let colored = restoreAgentActivityColors(text);
  if (restoreClaudeHeadings) colored = restoreClaudeHeadingColors(colored);
  const lines = colored.split('\n');
  const backgrounds = ansiLineBackgrounds(lines);
  return lines.map((line, index) => {
    if (line === TERMINAL_SEPARATOR_TOKEN) return '<span class="term-separator" aria-hidden="true"></span>';
    const sourceBackground = backgrounds[index];
    const normalizeRow = normalizeCodexLightRows && isNearWhiteAnsiColor(sourceBackground);
    const normalizeClaudeDarkText = restoreClaudeHeadings && !sourceBackground;
    const background = normalizedAnsiBackground(sourceBackground, normalizeRow);
    const className = background ? ' ansi-line-background' : '';
    const style = background ? ` style="${ansiLineBackgroundStyle(line, background)}"` : '';
    // ansiToHtml escapes every text segment before it emits controlled span markup.
    return `<span class="ansi-line${className}"${style}>${ansiToHtml(trimAnsiLineEnd(line), normalizeRow, normalizeClaudeDarkText)}</span>`;
  }).join('');
}

export function renderTerminalContent(
  content: string,
  format: string,
  agentType: string,
  showStatusLine: boolean,
): { display: string; html: string } {
  const claudeChrome = /\bclaude\b/i.test(agentType);
  const display = compactSeparatorLines(
    terminalDisplayContent(content, showStatusLine, claudeChrome),
    claudeChrome,
  );
  if (format !== 'ansi') {
    return { display, html: escapeHtml(display.replaceAll(TERMINAL_SEPARATOR_TOKEN, '────────')) };
  }
  return {
    display,
    html: terminalHtml(display, /\bcodex\b/i.test(agentType), /\bclaude\b/i.test(agentType)),
  };
}
