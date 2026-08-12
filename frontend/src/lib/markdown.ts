const URL_PATTERN = /https?:\/\/[^\s<>"']+/giu;
const INLINE_PATTERN = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\[[^\]\n]+\]\(https?:\/\/[^\s)]+\)|https?:\/\/[^\s<>"']+)/giu;

function escapeHtml(value: string): string {
  return value.replace(/[&<>"']/g, (character) => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
  })[character] || character);
}

function trimUrlPunctuation(value: string): [string, string] {
  let url = value;
  let suffix = '';
  while (url && /[.,;:!?\])]/u.test(url.at(-1) || '')) {
    const character = url.at(-1) || '';
    if (character === ')' && (url.match(/\(/g)?.length || 0) >= (url.match(/\)/g)?.length || 0)) break;
    if (character === ']' && (url.match(/\[/g)?.length || 0) >= (url.match(/\]/g)?.length || 0)) break;
    suffix = character + suffix;
    url = url.slice(0, -1);
  }
  return [url, suffix];
}

export function safeExternalUrl(value: string): string {
  try {
    const parsed = new URL(value);
    if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') return '';
    return parsed.href;
  } catch {
    return '';
  }
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


export function safeMarkdownHtml(value: string, highlight = ''): string {
  const lines = value.replace(/\r\n?/g, '\n').split('\n');
  const output: string[] = [];
  let inCode = false;
  let codeLanguage = '';
  let codeLines: string[] = [];
  let list: 'ul' | 'ol' | '' = '';
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
  };
  const flushList = () => {
    if (!list) return;
    output.push(`</${list}>`);
    list = '';
  };

  for (const line of lines) {
    const fence = line.match(/^\s*```\s*([A-Za-z0-9_+.-]*)\s*$/u);
    if (fence) {
      if (inCode) flushCode();
      else {
        flushParagraph();
        flushList();
        inCode = true;
        codeLanguage = fence[1] || '';
      }
      continue;
    }
    if (inCode) {
      codeLines.push(line);
      continue;
    }
    if (!line.trim()) {
      flushParagraph();
      flushList();
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
    const unordered = line.match(/^\s*[-*+]\s+(.+)$/u);
    if (unordered) {
      flushParagraph();
      if (list !== 'ul') {
        flushList();
        list = 'ul';
        output.push('<ul>');
      }
      output.push(`<li>${renderInline(unordered[1], highlight)}</li>`);
      continue;
    }
    const ordered = line.match(/^\s*\d+[.)]\s+(.+)$/u);
    if (ordered) {
      flushParagraph();
      if (list !== 'ol') {
        flushList();
        list = 'ol';
        output.push('<ol>');
      }
      output.push(`<li>${renderInline(ordered[1], highlight)}</li>`);
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
    flushList();
    paragraph.push(line);
  }
  flushParagraph();
  flushList();
  flushCode();
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
