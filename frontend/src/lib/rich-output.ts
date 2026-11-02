export function escapeHtml(value: string): string {
  return value.replace(/[&<>"']/g, (character) => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
  })[character] || character);
}

export function trimUrlPunctuation(value: string): [string, string] {
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
