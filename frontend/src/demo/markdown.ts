import type { FileLinkContext } from '../lib/file-links';
import { renderOutputText, safeMarkdownHtml as renderMarkdown } from '../lib/markdown';

export * from '../lib/markdown';

export function safeMarkdownHtml(text: string, highlight = '', fileLinks?: FileLinkContext | null, role?: string): string {
  if (role === 'user') return `<p class="demo-user-text">${renderOutputText(text, highlight, fileLinks)}</p>`;
  return renderMarkdown(text, highlight, fileLinks);
}
