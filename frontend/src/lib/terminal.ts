const ANSI_COLORS: Record<number, string> = {
  30: '#555', 31: '#ff5f5f', 32: '#5fd75f', 33: '#ffd75f',
  34: '#5fafff', 35: '#d75fff', 36: '#1abc9c', 37: '#e5e5e5',
  90: '#777', 91: '#ff8080', 92: '#80ff80', 93: '#ffff80',
  94: '#80bfff', 95: '#ff80ff', 96: '#80ffff', 97: '#fff',
};

export const TERMINAL_SEPARATOR_TOKEN = '\uE000HERDR_SEPARATOR\uE000';
export const TERMINAL_REPEATED_RUN_LIMIT = 24;
const TERMINAL_REPEATED_RUN_TRIGGER = 32;
const LIGHT_ROW_FALLBACK_BACKGROUND = 'rgb(61,64,64)';
const ANSI_HEADING_ACCENT = '#3daee9';
const TERMINAL_GRAPHEME_SEGMENTER = new Intl.Segmenter(undefined, { granularity: 'grapheme' });
const TERMINAL_EMOJI_PRESENTATION = /\p{Emoji_Presentation}/u;
const TERMINAL_HORIZONTAL_CELLS: Record<string, true> = { '─': true, '━': true, '═': true };

type TerminalBoxStroke = '' | 'light' | 'heavy' | 'double';
type TerminalBoxArc = 'down-right' | 'down-left' | 'up-right' | 'up-left';
type TerminalBoxCell = readonly [
  up: TerminalBoxStroke,
  right: TerminalBoxStroke,
  down: TerminalBoxStroke,
  left: TerminalBoxStroke,
  arc?: TerminalBoxArc,
];

const TERMINAL_BOX_CELLS: Record<string, TerminalBoxCell> = {
  '│': ['light', '', 'light', ''],
  '┃': ['heavy', '', 'heavy', ''],
  '┌': ['', 'light', 'light', ''],
  '┐': ['', '', 'light', 'light'],
  '└': ['light', 'light', '', ''],
  '┘': ['light', '', '', 'light'],
  '├': ['light', 'light', 'light', ''],
  '┤': ['light', '', 'light', 'light'],
  '┬': ['', 'light', 'light', 'light'],
  '┴': ['light', 'light', '', 'light'],
  '┼': ['light', 'light', 'light', 'light'],
  '┏': ['', 'heavy', 'heavy', ''],
  '┓': ['', '', 'heavy', 'heavy'],
  '┗': ['heavy', 'heavy', '', ''],
  '┛': ['heavy', '', '', 'heavy'],
  '┣': ['heavy', 'heavy', 'heavy', ''],
  '┫': ['heavy', '', 'heavy', 'heavy'],
  '┳': ['', 'heavy', 'heavy', 'heavy'],
  '┻': ['heavy', 'heavy', '', 'heavy'],
  '╋': ['heavy', 'heavy', 'heavy', 'heavy'],
  '╒': ['', 'double', 'light', ''],
  '╓': ['', 'light', 'double', ''],
  '╔': ['', 'double', 'double', ''],
  '╕': ['', '', 'light', 'double'],
  '╖': ['', '', 'double', 'light'],
  '╗': ['', '', 'double', 'double'],
  '╘': ['light', 'double', '', ''],
  '╙': ['double', 'light', '', ''],
  '╚': ['double', 'double', '', ''],
  '╛': ['light', '', '', 'double'],
  '╜': ['double', '', '', 'light'],
  '╝': ['double', '', '', 'double'],
  '╞': ['light', 'double', 'light', ''],
  '╟': ['double', 'light', 'double', ''],
  '╠': ['double', 'double', 'double', ''],
  '╡': ['light', '', 'light', 'double'],
  '╢': ['double', '', 'double', 'light'],
  '╣': ['double', '', 'double', 'double'],
  '╤': ['', 'double', 'light', 'double'],
  '╥': ['', 'light', 'double', 'light'],
  '╦': ['', 'double', 'double', 'double'],
  '╧': ['light', 'double', '', 'double'],
  '╨': ['double', 'light', '', 'light'],
  '╩': ['double', 'double', '', 'double'],
  '╪': ['light', 'double', 'light', 'double'],
  '╫': ['double', 'light', 'double', 'light'],
  '╬': ['double', 'double', 'double', 'double'],
  '╭': ['', 'light', 'light', '', 'down-right'],
  '╮': ['', '', 'light', 'light', 'down-left'],
  '╯': ['light', '', '', 'light', 'up-left'],
  '╰': ['light', 'light', '', '', 'up-right'],
  '╴': ['', '', '', 'light'],
  '╵': ['light', '', '', ''],
  '╶': ['', 'light', '', ''],
  '╷': ['', '', 'light', ''],
  '╸': ['', '', '', 'heavy'],
  '╹': ['heavy', '', '', ''],
  '╺': ['', 'heavy', '', ''],
  '╻': ['', '', 'heavy', ''],
  '╼': ['', 'heavy', '', 'light'],
  '╽': ['light', '', 'heavy', ''],
  '╾': ['', 'light', '', 'heavy'],
  '╿': ['heavy', '', 'light', ''],
  '║': ['double', '', 'double', ''],
};

interface TerminalBoxRendering {
  className: string;
  style: string;
}

function terminalBoxArmLayers(
  direction: 'up' | 'right' | 'down' | 'left' | 'vertical' | 'horizontal',
  stroke: TerminalBoxStroke,
): string[] {
  if (!stroke) return [];
  const vertical = direction === 'up' || direction === 'down' || direction === 'vertical';
  const wholeAxis = direction === 'vertical' || direction === 'horizontal';
  const edge = wholeAxis ? '50%' : direction === 'up' ? 'top' : direction === 'down' ? 'bottom' : direction;
  const length = wholeAxis ? '100%' : '50%';
  const extent = vertical ? `.75px ${length}` : `${length} 1px`;
  const layer = (position: string, size = extent) =>
    `linear-gradient(currentColor,currentColor) ${position}/${size} no-repeat`;
  if (stroke === 'double') {
    if (vertical) {
      return [
        layer(`calc(50% - .15em) ${edge}`),
        layer(`calc(50% + .15em) ${edge}`),
      ];
    }
    return [
      layer(`${edge} calc(50% - .15em)`),
      layer(`${edge} calc(50% + .15em)`),
    ];
  }
  const position = vertical ? `50% ${edge}` : `${edge} 50%`;
  const size = stroke === 'heavy'
    ? (vertical ? `2px ${length}` : `${length} 2px`)
    : extent;
  return [layer(position, size)];
}

const TERMINAL_BOX_RENDERINGS: Record<string, TerminalBoxRendering> = {};
for (const [character, [up, right, down, left, arc]] of Object.entries(TERMINAL_BOX_CELLS)) {
  if (arc) {
    TERMINAL_BOX_RENDERINGS[character] = {
      className: `terminal-cell-box terminal-cell-arc terminal-cell-arc-${arc}`,
      style: '',
    };
    continue;
  }
  const verticalLayers = up && up === down
    ? terminalBoxArmLayers('vertical', up)
    : [...terminalBoxArmLayers('up', up), ...terminalBoxArmLayers('down', down)];
  const horizontalLayers = right && right === left
    ? terminalBoxArmLayers('horizontal', right)
    : [...terminalBoxArmLayers('right', right), ...terminalBoxArmLayers('left', left)];
  const layers = [...verticalLayers, ...horizontalLayers];
  TERMINAL_BOX_RENDERINGS[character] = {
    className: 'terminal-cell-box',
    style: `background:${layers.join(',')}`,
  };
}

function isWideTerminalCodePoint(codePoint: number): boolean {
  return codePoint >= 0x1100 && (
    codePoint <= 0x115f
    || codePoint === 0x2329
    || codePoint === 0x232a
    || (codePoint >= 0x2e80 && codePoint <= 0xa4cf && codePoint !== 0x303f)
    || (codePoint >= 0xac00 && codePoint <= 0xd7a3)
    || (codePoint >= 0xf900 && codePoint <= 0xfaff)
    || (codePoint >= 0xfe10 && codePoint <= 0xfe19)
    || (codePoint >= 0xfe30 && codePoint <= 0xfe6f)
    || (codePoint >= 0xff00 && codePoint <= 0xff60)
    || (codePoint >= 0xffe0 && codePoint <= 0xffe6)
    || (codePoint >= 0x20000 && codePoint <= 0x3fffd)
  );
}

function terminalGraphemeWidth(grapheme: string): number {
  if (!grapheme || /^\p{Mark}+$/u.test(grapheme)) return 0;
  if (grapheme.includes('\uFE0F') || TERMINAL_EMOJI_PRESENTATION.test(grapheme)) return 2;
  const codePoint = grapheme.codePointAt(0) || 0;
  return isWideTerminalCodePoint(codePoint) ? 2 : 1;
}

function terminalCellsHtml(text: string, startingColumn: number): { html: string; column: number } {
  let column = startingColumn;
  let html = '';
  let plain = '';
  let plainCells = 0;
  let horizontal = '';
  let horizontalCells = 0;
  const flushPlain = () => {
    if (!plain) return;
    html += `<span class="terminal-cell-run" style="width:${plainCells}ch">${escapeHtml(plain)}</span>`;
    plain = '';
    plainCells = 0;
  };
  const flushHorizontal = () => {
    if (!horizontal) return;
    const kind = horizontal[0] === '━' ? 'heavy' : horizontal[0] === '═' ? 'double' : 'single';
    html += `<span class="terminal-cell-horizontal terminal-cell-horizontal-${kind}" style="width:${horizontalCells}ch">${escapeHtml(horizontal)}</span>`;
    horizontal = '';
    horizontalCells = 0;
  };
  for (const { segment } of TERMINAL_GRAPHEME_SEGMENTER.segment(text)) {
    if (segment === '\t') {
      flushHorizontal();
      const spaces = 8 - (column % 8);
      plain += ' '.repeat(spaces);
      plainCells += spaces;
      column += spaces;
      continue;
    }
    const width = terminalGraphemeWidth(segment);
    const box = TERMINAL_BOX_RENDERINGS[segment];
    if (box) {
      flushHorizontal();
      flushPlain();
      const style = box.style ? ` style="${box.style}"` : '';
      html += `<span class="terminal-cell ${box.className}"${style}>${escapeHtml(segment)}</span>`;
      column += width;
      continue;
    }
    if (TERMINAL_HORIZONTAL_CELLS[segment]) {
      flushPlain();
      if (horizontal && horizontal[0] !== segment) flushHorizontal();
      horizontal += segment;
      horizontalCells += width;
      column += width;
      continue;
    }
    flushHorizontal();
    if (/^[\x20-\x7e]$/u.test(segment)) {
      plain += segment;
      plainCells += width;
    } else {
      flushPlain();
      const className = width === 2 ? ' terminal-cell-wide' : '';
      html += `<span class="terminal-cell${className}">${escapeHtml(segment)}</span>`;
    }
    column += width;
  }
  flushHorizontal();
  flushPlain();
  return { html, column };
}


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


export function terminalDisplayContent(content: unknown): string {
  const normalized = String(content ?? '')
    .split('\n')
    .map((line) => trimTerminalChrome(line, false))
    .join('\n');
  return reflowTerminalLines(normalized);
}

function preservedTerminalDisplayContent(content: unknown): string {
  return String(content ?? '').split('\n')
    .map((line) => line.endsWith('\r') ? line.slice(0, -1) : line)
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


export function compactRepeatedCharacterRuns(line: string): string {
  const repeated = new RegExp(`([^\\p{L}\\p{N}\\s])\\1{${TERMINAL_REPEATED_RUN_TRIGGER},}`, 'gu');
  return line.replace(repeated, (_run, character: string) => character.repeat(TERMINAL_REPEATED_RUN_LIMIT));
}

export function trimTerminalChrome(line: string, trimFrameEdges = true): string {
  let trimmed = trimTrailingDecoration(line);
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
  return normalize && isNearWhiteAnsiColor(color) ? LIGHT_ROW_FALLBACK_BACKGROUND : color;
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
  preserveTerminalCells = false,
): string {
  let html = '';
  let open = false;
  let styles: Record<string, string> = {};
  let column = 0;
  const parts = text.split(/\x1b\[([0-9;]*)m/g);
  for (let index = 0; index < parts.length; index += 1) {
    if (index % 2 === 0) {
      if (preserveTerminalCells) {
        const rendered = terminalCellsHtml(parts[index], column);
        html += rendered.html;
        column = rendered.column;
      } else {
        html += escapeHtml(parts[index]);
      }
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


function hasTerminalBoxCell(line: string): boolean {
  for (const character of line) {
    if (TERMINAL_BOX_RENDERINGS[character]) return true;
  }
  return false;
}

export function terminalHtml(
  text: string,
  normalizeLightPalette = false,
  preserveLineEnds = false,
): string {
  const lines = text.split('\n');
  const backgrounds = ansiLineBackgrounds(lines);
  return lines.map((line, index) => {
    if (line === TERMINAL_SEPARATOR_TOKEN) return '<span class="term-separator" aria-hidden="true"></span>';
    const renderedLine = preserveLineEnds
      ? (line.endsWith('\r') ? line.slice(0, -1) : line)
      : trimAnsiLineEnd(line);
    const sourceBackground = backgrounds[index];
    const normalizeRow = normalizeLightPalette && isNearWhiteAnsiColor(sourceBackground);
    const normalizeDarkText = normalizeLightPalette && (!sourceBackground || normalizeRow);
    const background = normalizedAnsiBackground(sourceBackground, normalizeRow);
    const classes = [
      'ansi-line',
      background ? 'ansi-line-background' : '',
      preserveLineEnds && hasTerminalBoxCell(renderedLine) ? 'terminal-grid-line' : '',
    ].filter(Boolean).join(' ');
    const style = background ? ` style="${ansiLineBackgroundStyle(renderedLine, background)}"` : '';
    // ansiToHtml escapes every text segment before it emits controlled span markup.
    return `<span class="${classes}"${style}>${ansiToHtml(renderedLine, normalizeRow, normalizeDarkText, preserveLineEnds)}</span>`;
  }).join('');
}

export function renderTerminalContent(
  content: string,
  format: string,
  preserveLayout = false,
  preserveLineEnds = preserveLayout,
): { display: string; html: string } {
  const markedDisplay = preserveLayout
    ? preservedTerminalDisplayContent(content)
    : compactSeparatorLines(terminalDisplayContent(content));
  const display = preserveLayout && !preserveLineEnds
    ? markedDisplay.split('\n').map(trimAnsiLineEnd).join('\n')
    : markedDisplay;
  if (format !== 'ansi') {
    return { display, html: escapeHtml(display.replaceAll(TERMINAL_SEPARATOR_TOKEN, '────────')) };
  }
  return {
    display,
    html: terminalHtml(display, true, preserveLayout),
  };
}
