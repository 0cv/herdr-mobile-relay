import { escapeHtml, safeExternalUrl, trimUrlPunctuation } from './rich-output';
export { safeExternalUrl } from './rich-output';

const URL_PATTERN = /https?:\/\/[^\s<>"']+/giu;
const INLINE_PATTERN = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\[[^\]\n]+\]\(https?:\/\/[^\s)]+\)|https?:\/\/[^\s<>"']+)/giu;
// Markdown does not treat a number longer than nine digits as a list number.
const LIST_ITEM = /^(\s*)(?:([-*+])|(\d{1,9})[.)])(\s+)(.+)$/u;
const CODE_FENCE = /^\s*```\s*([A-Za-z0-9_+.-]*)\s*$/u;

interface ListItem {
  type: 'ul' | 'ol';
  indent: number;
  content: number;
  start: number;
  text: string;
}

// Tabs advance to the next multiple of four columns, as in Markdown.
function columnAfter(whitespace: string, column = 0): number {
  for (const character of whitespace) column = character === '\t' ? column + 4 - (column % 4) : column + 1;
  return column;
}

function listItem(line: string): ListItem | null {
  const match = line.match(LIST_ITEM);
  if (!match) return null;
  const [, lead, bullet, number, gap, text] = match;
  const indent = columnAfter(lead);
  const markerEnd = indent + (bullet ? 1 : number.length + 1);
  const spaces = columnAfter(gap, markerEnd) - markerEnd;
  // Text five or more columns after the marker is indented code, so the item's content starts one column after it.
  return { type: bullet ? 'ul' : 'ol', indent, content: markerEnd + (spaces > 4 ? 1 : spaces), start: bullet ? 1 : Number(number), text };
}

function escapedWithHighlight(value: string, highlight: string): string {
  const needle = highlight.trim();
  if (!needle) return escapeHtml(value);
  const lower = value.toLocaleLowerCase();
  const lowerNeedle = needle.toLocaleLowerCase();
  let cursor = 0;
  let output = '';
  while (cursor < value.length) {
    const index = lower.indexOf(lowerNeedle, cursor);
    if (index < 0) {
      output += escapeHtml(value.slice(cursor));
      break;
    }
    output += escapeHtml(value.slice(cursor, index));
    output += `<mark>${escapeHtml(value.slice(index, index + needle.length))}</mark>`;
    cursor = index + needle.length;
  }
  return output;
}

function renderInline(value: string, highlight: string): string {
  let output = '';
  let cursor = 0;
  for (const match of value.matchAll(INLINE_PATTERN)) {
    const index = match.index || 0;
    output += escapedWithHighlight(value.slice(cursor, index), highlight);
    const token = match[0];
    if (token.startsWith('`') && token.endsWith('`')) {
      output += `<code>${escapedWithHighlight(token.slice(1, -1), highlight)}</code>`;
    } else if (token.startsWith('**') && token.endsWith('**')) {
      output += `<strong>${renderInline(token.slice(2, -2), highlight)}</strong>`;
    } else if (token.startsWith('[')) {
      const link = token.match(/^\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)$/iu);
      const href = link ? safeExternalUrl(link[2]) : '';
      output += href
        ? `<a href="${escapeHtml(href)}" target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer">${escapedWithHighlight(link![1], highlight)}</a>`
        : escapedWithHighlight(token, highlight);
    } else {
      const [candidate, suffix] = trimUrlPunctuation(token);
      const href = safeExternalUrl(candidate);
      output += href
        ? `<a href="${escapeHtml(href)}" target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer">${escapedWithHighlight(candidate, highlight)}</a>${escapedWithHighlight(suffix, highlight)}`
        : escapedWithHighlight(token, highlight);
    }
    cursor = index + token.length;
  }
  output += escapedWithHighlight(value.slice(cursor), highlight);
  return output;
}

type TableAlignment = '' | 'left' | 'center' | 'right';

/**
 * Splits one table row on unescaped pipes. GFM resolves `\|` to a literal pipe
 * before any inline parsing, so the escape is dropped here and the cell text
 * still travels through renderInline for escaping.
 */
function splitTableRow(row: string): string[] {
  const text = row.trim();
  const cells: string[] = [];
  let cell = '';
  let separated = false;
  for (let cursor = 0; cursor < text.length; cursor += 1) {
    if (text[cursor] === '\\' && text[cursor + 1] === '|') {
      cell += '|';
      cursor += 1;
      separated = false;
      continue;
    }
    if (text[cursor] === '|') {
      cells.push(cell.trim());
      cell = '';
      separated = true;
      continue;
    }
    cell += text[cursor];
    separated = false;
  }
  if (!separated || cell.trim()) cells.push(cell.trim());
  if (text.startsWith('|')) cells.shift();
  return cells;
}

function tableAlignments(row: string): TableAlignment[] | null {
  if (!row.includes('|')) return null;
  const cells = splitTableRow(row);
  if (!cells.length) return null;
  const alignments: TableAlignment[] = [];
  for (const cell of cells) {
    if (!/^:?-+:?$/u.test(cell)) return null;
    const left = cell.startsWith(':');
    const right = cell.endsWith(':');
    alignments.push(left && right ? 'center' : right ? 'right' : left ? 'left' : '');
  }
  return alignments;
}

function renderTableRow(cells: string[], alignments: TableAlignment[], tag: 'th' | 'td', highlight: string): string {
  let html = '<tr>';
  for (let column = 0; column < alignments.length; column += 1) {
    const align = alignments[column] ? ` style="text-align:${alignments[column]}"` : '';
    html += `<${tag}${align}>${renderInline(cells[column] || '', highlight)}</${tag}>`;
  }
  return `${html}</tr>`;
}


export function fencedCodeText(value: string): string | null {
  const blocks: string[] = [];
  let current: string[] | null = null;
  for (const line of value.replace(/\r\n?/g, '\n').split('\n')) {
    if (CODE_FENCE.test(line)) {
      if (current === null) {
        current = [];
      } else {
        const block = current.join('\n');
        if (block.trim()) blocks.push(block);
        current = null;
      }
      continue;
    }
    if (current !== null) current.push(line);
  }
  return blocks.length ? blocks.join('\n\n') : null;
}

/**
 * Reduces markdown to prose worth hearing. A speech engine reads formatting
 * characters out loud - backticks became "backtick" on a real phone - and a
 * fenced code block spoken character by character is noise, so structure is
 * dropped and only human-directed text survives.
 */
export function speakableText(value: string): string {
  const lines: string[] = [];
  let inCode = false;
  for (const line of value.replace(/\r\n?/g, '\n').split('\n')) {
    if (/^\s*```/u.test(line)) {
      if (!inCode) lines.push('Code block omitted.');
      inCode = !inCode;
      continue;
    }
    if (inCode) continue;
    if (/^\s*(?:---+|___+|\*\*\*+)\s*$/u.test(line)) continue;
    // A table's alignment row is pure formatting; content rows read as
    // comma-separated cells.
    if (/^\s*\|?[\s:|-]+\|?\s*$/u.test(line) && line.includes('-') && line.includes('|')) continue;
    lines.push(line
      .replace(/^\s*#{1,6}\s+/u, '')
      .replace(/^\s*>\s?/u, '')
      .replace(/^\s*(?:[-*+]|\d{1,3}[.)])\s+/u, '')
      .replace(/\[([^\]]+)\]\(([^)]*)\)/gu, '$1')
      .replace(/https?:\/\/\S+/gu, '')
      .replace(/`([^`]*)`/gu, '$1')
      .replace(/(\*\*|__|~~)(.+?)\1/gu, '$2')
      .replace(/(^|\s)[*_](\S(?:[^*_]*\S)?)[*_](?=\s|$|[.,;:!?])/gu, '$1$2')
      .replace(/\s*\|\s*/gu, ', ')
      .replace(/^[,\s]+|[,\s]+$/gu, ''));
  }
  return lines.filter(Boolean).join('\n').replace(/[ \t]+/gu, ' ').trim();
}

export function safeMarkdownHtml(value: string, highlight = ''): string {
  const lines = value.replace(/\r\n?/g, '\n').split('\n');
  const output: string[] = [];
  let inCode = false;
  let codeLanguage = '';
  let codeLines: string[] = [];
  // Leading whitespace of a code fence inside a list item, removed from its code lines.
  let codeIndent = '';
  // Open lists, outermost first. Each level keeps its <li> open so a deeper list or code block nests inside it.
  const lists: { type: 'ul' | 'ol'; indent: number; content: number }[] = [];
  // Text lines of the innermost open item that are not yet written to the output.
  let itemLines: string[] = [];
  let itemParagraph = false;
  let paragraph: string[] = [];

  const flushParagraph = () => {
    if (!paragraph.length) return;
    output.push(`<p>${paragraph.map((line) => renderInline(line, highlight)).join('<br>')}</p>`);
    paragraph = [];
  };
  const flushCode = () => {
    if (!inCode) return;
    const language = /^[A-Za-z0-9_+.-]{1,32}$/u.test(codeLanguage) ? ` class="language-${escapeHtml(codeLanguage)}"` : '';
    output.push(`<pre><code${language}>${escapedWithHighlight(codeLines.join('\n'), highlight)}</code></pre>`);
    inCode = false;
    codeLanguage = '';
    codeLines = [];
    codeIndent = '';
  };
  const flushItemLines = () => {
    if (!itemLines.length) return;
    const html = itemLines.map((line) => renderInline(line, highlight)).join('<br>');
    output.push(itemParagraph ? `<p>${html}</p>` : html);
    itemLines = [];
    itemParagraph = false;
  };
  const closeLevel = () => {
    flushItemLines();
    const level = lists.pop();
    if (level) output.push(`</li></${level.type}>`);
  };
  const flushList = () => {
    while (lists.length) closeLevel();
    itemParagraph = false;
  };
  const leadOf = (text: string) => text.match(/^\s*/u)?.[0] || '';
  // The number of open items whose content a line indented this far belongs to.
  const depthAt = (indent: number) => {
    let depth = 0;
    while (depth < lists.length && indent >= lists[depth].content) depth += 1;
    return depth;
  };
  const openItem = (item: ListItem) => {
    flushParagraph();
    const depth = depthAt(item.indent);
    while (lists.length > depth + 1) closeLevel();
    if (lists.length > depth && lists[depth].type !== item.type) closeLevel();
    flushItemLines();
    if (lists.length > depth) output.push('</li><li>');
    else output.push(item.type === 'ol' && item.start !== 1 ? `<ol start="${item.start}"><li>` : `<${item.type}><li>`);
    lists[depth] = { type: item.type, indent: item.indent, content: item.content };
    itemLines = [item.text];
    itemParagraph = false;
  };

  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index];
    const fence = line.match(CODE_FENCE);
    if (fence) {
      if (inCode) flushCode();
      else {
        flushParagraph();
        const lead = leadOf(line);
        const depth = depthAt(columnAfter(lead));
        if (depth) {
          while (lists.length > depth) closeLevel();
          flushItemLines();
          codeIndent = lead;
        } else {
          flushList();
        }
        inCode = true;
        codeLanguage = fence[1] || '';
      }
      continue;
    }
    if (inCode) {
      codeLines.push(line.startsWith(codeIndent) ? line.slice(codeIndent.length) : line.trimStart());
      continue;
    }
    if (!line.trim()) {
      let next = index + 1;
      while (next < lines.length && !lines[next].trim()) next += 1;
      index = next - 1;
      flushParagraph();
      const following = lines[next];
      if (lists.length && following !== undefined
        && (LIST_ITEM.test(following) || columnAfter(leadOf(following)) >= lists[0].content)) {
        flushItemLines();
        itemParagraph = true;
      } else {
        flushList();
      }
      continue;
    }
    const heading = line.match(/^\s{0,3}(#{1,4})\s+(.+)$/u);
    if (heading) {
      flushParagraph();
      flushList();
      const level = Math.min(6, heading[1].length + 2);
      output.push(`<h${level}>${renderInline(heading[2], highlight)}</h${level}>`);
      continue;
    }
    const item = listItem(line);
    if (item) {
      openItem(item);
      continue;
    }
    const quote = line.match(/^\s*>\s?(.*)$/u);
    if (quote) {
      flushParagraph();
      flushList();
      output.push(`<blockquote>${renderInline(quote[1], highlight)}</blockquote>`);
      continue;
    }
    if (/^\s*(?:---+|___+|\*\*\*+)\s*$/u.test(line)) {
      flushParagraph();
      flushList();
      output.push('<hr>');
      continue;
    }
    const alignments = line.includes('|') ? tableAlignments(lines[index + 1] || '') : null;
    const header = alignments ? splitTableRow(line) : [];
    if (alignments && header.length === alignments.length) {
      flushParagraph();
      flushList();
      const body: string[] = [];
      let row = index + 2;
      while (row < lines.length && lines[row].trim() && lines[row].includes('|') && !lines[row].trimStart().startsWith('```')) {
        body.push(renderTableRow(splitTableRow(lines[row]), alignments, 'td', highlight));
        row += 1;
      }
      index = row - 1;
      const tbody = body.length ? `<tbody>${body.join('')}</tbody>` : '';
      output.push(`<div class="conversation-table"><table><thead>${renderTableRow(header, alignments, 'th', highlight)}</thead>${tbody}</table></div>`);
      continue;
    }
    // An indented plain line continues the deepest item whose content it reaches, or the outermost item.
    const indent = columnAfter(leadOf(line));
    if (lists.length && indent > lists[0].indent) {
      while (lists.length > Math.max(depthAt(indent), 1)) closeLevel();
      itemLines.push(line.trim());
      continue;
    }
    flushList();
    paragraph.push(line);
  }
  flushParagraph();
  flushCode();
  flushList();
  return output.join('');
}

export function linkifyPlainText(value: string): string {
  let output = '';
  let cursor = 0;
  for (const match of value.matchAll(URL_PATTERN)) {
    const index = match.index || 0;
    output += escapeHtml(value.slice(cursor, index));
    const [candidate, suffix] = trimUrlPunctuation(match[0]);
    const href = safeExternalUrl(candidate);
    output += href
      ? `<a href="${escapeHtml(href)}" target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer">${escapeHtml(candidate)}</a>${escapeHtml(suffix)}`
      : escapeHtml(match[0]);
    cursor = index + match[0].length;
  }
  output += escapeHtml(value.slice(cursor));
  return output;
}
