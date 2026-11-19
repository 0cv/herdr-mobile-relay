import { escapedWithHighlight, safeMarkdownHtml as renderMarkdown } from '../lib/markdown';

export * from '../lib/markdown';

export function safeMarkdownHtml(text: string, highlight = '', role?: string): string {
  if (role === 'user') return `<p class="demo-user-text">${escapedWithHighlight(text, highlight)}</p>`;
  return renderMarkdown(text, highlight);
}
