import { escapeHtml, HTTP_URL_PATTERN } from './rich-output';
import type { Agent, RelayConnectionView } from './types';

export interface FileLinkContext {
  cwd: string;
  home?: string;
}

export interface FileTarget {
  path: string;
  line?: number;
}

interface Span {
  start: number;
  end: number;
}

export interface FileLink extends FileTarget, Span {}

interface UriSpan extends Span {
  http: boolean;
}

export function fileLinkContext(
  agent: Pick<Agent, 'cwd'>,
  connection?: Pick<RelayConnectionView, 'capabilities' | 'home'>,
): FileLinkContext | null {
  if (!agent.cwd || !connection?.capabilities.includes('workspace_inspection')) return null;
  return { cwd: agent.cwd, home: connection.home };
}

function normalizePath(path: string): string | null {
  const parts: string[] = [];
  for (const part of path.split('/')) {
    if (!part || part === '.') continue;
    if (part === '..') {
      if (!parts.length) return null;
      parts.pop();
    } else {
      parts.push(part);
    }
  }
  return parts.join('/');
}

function resolvePath(path: string, { cwd, home }: FileLinkContext): string | null {
  if (path.startsWith('~/')) {
    if (!home?.startsWith('/')) return null;
    path = home + path.slice(1);
  }
  const normalized = normalizePath(path);
  if (!normalized || !path.startsWith('/')) return normalized;
  if (!cwd.startsWith('/')) return null;
  const root = normalizePath(cwd);
  if (root === null) return null;
  if (!root) return normalized;
  return normalized.startsWith(`${root}/`) ? normalized.slice(root.length + 1) : null;
}

const LINE_REFERENCE = /:(\d{1,6})(?:[:-]\d{1,6})?|#L(\d{1,6})(?:C\d{1,6})?/y;
const WEB_URI_WRAPPERS = /^(?:blob|filesystem|view-source):$/i;

function uriEnd(text: string, start: number, opener: string): number {
  const closer = opener === '(' ? ')' : opener === '[' ? ']' : opener === '{' ? '}' : opener === "'" ? opener : '';
  let depth = 0;
  let inQuery = false;
  let quotedValue = false;
  let end = start;
  for (; end < text.length; end += 1) {
    const character = text[end];
    if (/[\s<>"`]/.test(character)) break;
    if (character === '?') inQuery = true;
    if (character === closer) {
      const next = text[end + 1];
      if (closer === "'") {
        if (quotedValue) quotedValue = false;
        else if (!next || /[\s<>"`)\]},.;!?]/.test(next)) break;
        else if (inQuery && /[=,]/.test(text[end - 1])) quotedValue = true;
      } else if (depth) depth -= 1;
      else if (!inQuery || next !== '&') break;
    } else if (character === opener) depth += 1;
  }
  return end;
}

export function findUriSpans(text: string, inlineMarkupEnd?: (text: string, start: number) => number | undefined): UriSpan[] {
  if (!text.includes(':') || !text.includes('/')) return [];
  const runs = /[a-z0-9_.+-]+:?/gi;
  const uris: UriSpan[] = [];
  for (let match = runs.exec(text); match; match = runs.exec(text)) {
    const start = match.index;
    const before = text[start - 1] || '';
    if (/[a-z0-9_.+@~/-]/i.test(before) || !/^[a-z][a-z0-9+.-]*:$/i.test(match[0])) continue;
    const afterScheme = runs.lastIndex;
    const end = uriEnd(text, afterScheme, before);
    const body = text.slice(afterScheme, end);
    if (!body.includes('/')) {
      runs.lastIndex = end;
      continue;
    }
    if (/^https?:\/\//i.test(body) && !WEB_URI_WRAPPERS.test(match[0])) continue;
    const markupEnd = inlineMarkupEnd?.(text, afterScheme);
    if (markupEnd !== undefined && !/[@?]/.test(text.slice(markupEnd, end).replace(/[?!.,;]+$/, ''))) continue;
    uris.push({ start, end, http: /^https?:$/i.test(match[0]) && body.startsWith('//') });
    runs.lastIndex = end;
  }
  return uris;
}

export function outsideUnsupportedUris(uris: UriSpan[]) {
  let uri = 0;
  return function* (text: string, pattern: RegExp, offset = 0) {
    const scan = new RegExp(pattern);
    for (let match = scan.exec(text); match; match = scan.exec(text)) {
      const start = offset + match.index;
      while (uris[uri]?.end <= start) uri += 1;
      const span = uris[uri];
      if (span && span.start <= start && !span.http) scan.lastIndex = span.end - offset;
      else yield match;
    }
  };
}

export function findFileLinks(text: string, context?: FileLinkContext | null, uriSpans?: UriSpan[]): FileLink[] {
  if (!context || text.length > 4096 || !text.includes('/')) return [];
  const uris = uriSpans ?? findUriSpans(text);
  const web = outsideUnsupportedUris(uris)(text, HTTP_URL_PATTERN);
  let address = web.next().value;
  const runs = /[a-z0-9_.+@~/-]+/gi;
  const links: FileLink[] = [];
  let uri = 0;
  for (let match = runs.exec(text); match; match = runs.exec(text)) {
    const start = match.index;
    while (uris[uri]?.end <= start) uri += 1;
    if (uris[uri]?.start < runs.lastIndex) {
      runs.lastIndex = Math.max(runs.lastIndex, uris[uri].end);
      continue;
    }
    while (address && address.index + address[0].length <= start) address = web.next().value;
    if (address && address.index < runs.lastIndex) continue;
    const before = text[start - 1];
    if (before && !/[\s([{<"'`=,]/.test(before)) continue;
    const run = match[0];
    let length = run.length;
    while (run[length - 1] === '.') length -= 1;
    const candidate = run.slice(0, length);
    if (!candidate.includes('/') || candidate.includes('//') || !/^[a-z0-9_./~]/i.test(candidate)
      || candidate.indexOf('~', candidate.startsWith('~/') ? 1 : 0) !== -1) continue;
    const extension = candidate.match(/\.[a-z0-9]{1,10}$/i)?.[0];
    if (!extension || !/[a-z]/i.test(extension)) continue;
    const path = resolvePath(candidate, context);
    if (!path) continue;

    let end = start + length;
    let line = 0;
    if (length === run.length) {
      LINE_REFERENCE.lastIndex = end;
      const reference = LINE_REFERENCE.exec(text);
      if (reference && !/[a-z0-9]/i.test(text[LINE_REFERENCE.lastIndex] || '')) {
        line = Number(reference[1] ?? reference[2]);
        if (line) end = LINE_REFERENCE.lastIndex;
      }
    }
    links.push(line ? { start, end, path, line } : { start, end, path });
  }
  return links;
}

export function fileLinksInFragments(links: FileLink[]) {
  let next = 0;
  return (start: number, end: number): FileLink[] => {
    const fragment: FileLink[] = [];
    for (; links[next]?.start < end; next += 1) {
      const link = links[next];
      if (link.start >= start && link.end <= end) {
        fragment.push({ ...link, start: link.start - start, end: link.end - start });
      }
    }
    return fragment;
  };
}

function fileLinkHtml({ path, line }: FileTarget, labelHtml: string): string {
  return `<a class="file-link" role="button" tabindex="0" aria-haspopup="dialog" data-file-path="${escapeHtml(path)}"${line ? ` data-file-line="${line}"` : ''}>${labelHtml}</a>`;
}

export function renderFileLinks(
  text: string,
  links: FileLink[],
  render = (start: number, end: number) => escapeHtml(text.slice(start, end)),
): string {
  let html = '';
  let cursor = 0;
  for (const link of links) {
    const { start, end } = link;
    if (cursor < start) html += render(cursor, start);
    html += fileLinkHtml(link, render(start, end));
    cursor = end;
  }
  return cursor < text.length ? html + render(cursor, text.length) : html;
}
