import { describe, expect, it } from 'vitest';
import { fileLinkContext, findFileLinks } from '../../src/lib/file-links';
import { safeMarkdownHtml } from '../../src/lib/markdown';
import { escapeHtml } from '../../src/lib/rich-output';
import { linkifyTerminalText, renderTerminalContent } from '../../src/lib/terminal';

const context = { cwd: '/home/me/app', home: '/home/me' };

function linkedPaths(text: string, root = context) {
  return findFileLinks(text, root).map(({ start, end, path, line }) => [text.slice(start, end), path, line]);
}

describe('file path recognition', () => {
  it.each([
    ['internal/push/manager.go', 'internal/push/manager.go', 'internal/push/manager.go', undefined],
    ...[':42', ':42:7', ':42-50', '#L42', '#L42C7'].map((suffix) => [`src/app.ts${suffix}`, `src/app.ts${suffix}`, 'src/app.ts', 42] as const),
    ...[':0', ':42abc', ':42-50abc', ':1234567', ': fixed', '.:42'].map((suffix) => [`src/app.ts${suffix}`, 'src/app.ts', 'src/app.ts', undefined] as const),
    ['See docs/mobile-app.md.', 'docs/mobile-app.md', 'docs/mobile-app.md', undefined],
    ['(frontend/src/lib/terminal.ts)', 'frontend/src/lib/terminal.ts', 'frontend/src/lib/terminal.ts', undefined],
    ['--config=conf/app.yml', 'conf/app.yml', 'conf/app.yml', undefined],
    ['.github/workflows/check.yml', '.github/workflows/check.yml', '.github/workflows/check.yml', undefined],
    ['config/.env', 'config/.env', 'config/.env', undefined],
    ['./src/lib/../app.ts', './src/lib/../app.ts', 'src/app.ts', undefined],
    ['/home/me/app/src/app.ts:3', '/home/me/app/src/app.ts:3', 'src/app.ts', 3],
    ['~/app/docs/guide.md', '~/app/docs/guide.md', 'docs/guide.md', undefined],
  ] as const)('recognises %s', (text, printed, path, line) => {
    expect(linkedPaths(text)).toEqual([[printed, path, line]]);
  });

  it.each([
    'and/or', 'yes/no', 'origin/main', 'internal/push', 'internal/push/',
    'README.md', 'package.json', 'v1.2.3', 'example.com', '1.2/3.4', '2024/01/02', 'HTTP/1.1',
    'src/*.ts', '$HOME/app/main.ts', '@/lib/utils.ts', 'C:\\Users\\me\\a.ts',
    'file:///home/me/app/src/app.ts', 'file:///tmp?path=src/app.ts', 'file:///tmp?path=(src/app.ts),docs/guide.md',
    'git@github.com:owner/repo.git',
    '/etc/ssh/ssh_config.d/local.conf', '/home/me/other/src/app.ts', '~/other/notes.md', '../other/src/app.ts',
    '/home/me/app2/src/app.ts', '/../../home/me/app/src/app.ts', 'src//app.ts', 'src/~app.ts', 'src/app.abcdefghijk',
  ])('leaves %s as text', (text) => {
    expect(findFileLinks(text, context)).toEqual([]);
  });

  it('keeps punctuation out of the link bounds', () => {
    expect(findFileLinks('See src/app.ts:42.', context)).toEqual([{ start: 4, end: 17, path: 'src/app.ts', line: 42 }]);
    expect(linkedPaths('a/b.ts,c/d.ts')).toEqual([['a/b.ts', 'a/b.ts', undefined], ['c/d.ts', 'c/d.ts', undefined]]);
  });

  it.each([
    '(file:///tmp/guide.md),src/main.ts',
    '(file:///tmp?path=(src/app.ts),docs/guide.md),src/main.ts',
    '[sftp://[::1]/?path=src/app.ts],src/main.ts',
  ])('skips the URI but not the path after its closing punctuation in %s', (text) => {
    expect(linkedPaths(text)).toEqual([['src/main.ts', 'src/main.ts', undefined]]);
  });

  it('normalises the root and keeps linked worktrees separate', () => {
    expect(linkedPaths('/home/me/app/src/app.ts src/app.ts /home/me/app-worktrees/fix-one/src/app.ts', {
      ...context, cwd: '/home/me/app-worktrees/./fix-one/',
    })).toEqual([
      ['src/app.ts', 'src/app.ts', undefined],
      ['/home/me/app-worktrees/fix-one/src/app.ts', 'src/app.ts', undefined],
    ]);
    expect(linkedPaths('/src/app.ts', { ...context, cwd: '/' })).toEqual([['/src/app.ts', 'src/app.ts', undefined]]);
  });

  it('skips home paths without a home and runs over 4096 characters', () => {
    expect(linkedPaths('~/app/docs/guide.md', { ...context, home: '' })).toEqual([]);
    expect(findFileLinks('src/app.ts'.padEnd(5000), context)).toEqual([]);
  });

  it('requires the inspection capability and a working directory', () => {
    const connection = { capabilities: ['workspace_inspection'], home: context.home };
    expect(fileLinkContext({ cwd: context.cwd }, connection)).toEqual(context);
    expect(fileLinkContext({ cwd: context.cwd }, { ...connection, capabilities: [] })).toBeNull();
    expect(fileLinkContext({ cwd: '' }, connection)).toBeNull();
  });
});

const guideUrl = 'https://example.com/guide.html';
const uriParameters = ['path=src/app.ts', `next=${guideUrl}`];
const unsupportedUris = [
  'mailto:dev@example.com?body=src/main.ts',
  'custom:**notes?body=src/main.ts',
  'file:///tmp/notes.md?url=https://example.com/guide.html',
  'blob:https://example.com/550e8400-e29b-41d4-a716-446655440000',
  'filesystem:https://example.com/temporary/docs/guide.md',
  `view-source:${guideUrl}`,
  "mailto:dev@example.com?body='src/main.ts'",
  "file:///tmp/notes.md?tag='important'&next=https://example.com/guide.html",
  ...uriParameters.flatMap((parameter) => [
    `file:///tmp/notes.md?text=)&${parameter}`,
    `file:///tmp/notes.md?tag='one','two'&${parameter}`,
  ]),
];
const externalAttributes = 'target="_blank" rel="noopener noreferrer" referrerpolicy="no-referrer"';
const docsLink = `<a class="terminal-link" href="https://example.com/a" ${externalAttributes}>https://example.com/a</a>`;
const fileAttributes = 'class="file-link" role="button" tabindex="0" aria-haspopup="dialog"';

const fileAnchor = (printed: string, path: string, line?: number) =>
  `<a ${fileAttributes} data-file-path="${path}"${line ? ` data-file-line="${line}"` : ''}>${printed}</a>`;

describe('adjacent output links', () => {
  it.each([
    ['Conversation', (text: string, fileLinks: typeof context | null) => safeMarkdownHtml(text, '', fileLinks), '<p>', '</p>', ''],
    ['Terminal', linkifyTerminalText, '', '', ' class="terminal-link"'],
  ] as const)('keeps links after a closed file URI in %s', (_view, render, open, close, className) => {
    for (const fileLinks of [null, context]) {
      const url = `<a${className} href="${guideUrl}" ${externalAttributes}>${guideUrl}</a>`;
      expect(render(`'file:///tmp/notes.md','${guideUrl}'`, fileLinks)).toBe(
        `${open}&#39;file:///tmp/notes.md&#39;,&#39;${url}&#39;${close}`,
      );
      const path = fileLinks ? fileAnchor('docs/guide.md', 'docs/guide.md') : 'docs/guide.md';
      expect(render('((file:///tmp/notes.md),docs/guide.md)', fileLinks)).toBe(
        `${open}((file:///tmp/notes.md),${path})${close}`,
      );
    }
  });
});

describe('markdown file links', () => {
  const file = fileAnchor('src/app.ts:42', 'src/app.ts', 42);

  it.each([
    ['See src/app.ts:42.', `<p>See ${file}.</p>`],
    ['`src/app.ts:42`', `<p><code>${file}</code></p>`],
    ['**src/app.ts:42**', `<p><strong>${file}</strong></p>`],
    ['- src/app.ts:42', `<ul><li>${file}</li></ul>`],
    ['| File |\n| --- |\n| src/app.ts:42 |', `<td>${file}</td>`],
  ])('links paths in %s', (text, html) => {
    expect(safeMarkdownHtml(text, '', context)).toContain(html);
  });

  it('leaves fenced code, bare file names and inline code URLs alone', () => {
    const text = '```\nsrc/app.ts:42\n```\n\n`README.md` `https://example.com/?path=src/app.ts`';
    expect(safeMarkdownHtml(text, '', context)).toBe('<pre><code>src/app.ts:42</code></pre><p><code>README.md</code> <code>https://example.com/?path=src/app.ts</code></p>');
  });

  it('escapes and highlights within a link', () => {
    expect(safeMarkdownHtml('<src/app.ts:42 &>', 'app', context)).toBe(`<p>&lt;${fileAnchor('src/<mark>app</mark>.ts:42', 'src/app.ts', 42)} &amp;&gt;</p>`);
  });

  it('highlights a phrase across file link boundaries', () => {
    expect(safeMarkdownHtml('Updated src/main.ts', 'Updated src/main.ts', context)).toBe(
      `<p><mark>Updated </mark>${fileAnchor('<mark>src/main.ts</mark>', 'src/main.ts')}</p>`,
    );
    expect(safeMarkdownHtml('See a/b.ts and c/d.ts.', 'b.ts and c/d', context)).toBe(
      `<p>See ${fileAnchor('a/<mark>b.ts</mark>', 'a/b.ts')}<mark> and </mark>${fileAnchor('<mark>c/d</mark>.ts', 'c/d.ts')}.</p>`,
    );
  });

  it('does not add links inside explicit external links', () => {
    expect(safeMarkdownHtml('[docs](https://example.com/a/b.md) [a/b.md](https://example.com/)', '', context)).toBe(
      `<p><a href="https://example.com/a/b.md" ${externalAttributes}>docs</a> <a href="https://example.com/" ${externalAttributes}>a/b.md</a></p>`,
    );
  });

  it.each(unsupportedUris)('leaves %s plain without hiding adjacent links', (uri) => {
    for (const fileLinks of [null, context]) {
      const path = fileLinks ? fileAnchor('docs/guide.md', 'docs/guide.md') : 'docs/guide.md';
      for (const [open, close] of [['(', ')'], ["'", "'"]]) {
        expect(safeMarkdownHtml(`${open}${uri}${close},docs/guide.md https://example.com/a`, '', fileLinks)).toBe(
          `<p>${escapeHtml(open + uri + close)},${path} <a href="https://example.com/a" ${externalAttributes}>https://example.com/a</a></p>`,
        );
      }
    }
  });

  it('keeps explicit Markdown after prose colons without workspace inspection', () => {
    for (const fileLinks of [null, context]) {
      for (const suffix of ['', '?', '?!']) {
        expect(safeMarkdownHtml(`See:[docs](${guideUrl})${suffix}`, '', fileLinks)).toBe(
          `<p>See:<a href="${guideUrl}" ${externalAttributes}>docs</a>${suffix}</p>`,
        );
      }
    }
    expect(safeMarkdownHtml('Status:**ready** and docs/guide.md')).toBe('<p>Status:<strong>ready</strong> and docs/guide.md</p>');
    expect(safeMarkdownHtml('Status:ok**ready** and docs/guide.md')).toBe('<p>Status:ok<strong>ready</strong> and docs/guide.md</p>');
  });

  it('links a web address after a prose label without workspace inspection', () => {
    expect(safeMarkdownHtml(`URL:${guideUrl}`)).toBe(`<p>URL:<a href="${guideUrl}" ${externalAttributes}>${guideUrl}</a></p>`);
  });

  it.each(['src/app.ts', guideUrl])('leaves a mailto URI with a literal asterisk recipient and body %s plain', (body) => {
    const uri = `mailto:**dev**@example.com?body=${body}`;
    expect(safeMarkdownHtml(uri)).toBe(`<p>${uri}</p>`);
    expect(safeMarkdownHtml(uri, '', context)).toBe(`<p>${uri}</p>`);
  });

  it('does not shorten a file candidate at a bare HTTP match', () => {
    const text = 'src/app.tshttps://example.com/guide.html and docs/guide.md';
    expect(safeMarkdownHtml(text, '', context)).toBe(
      `<p>src/app.ts<a href="https://example.com/guide.html" ${externalAttributes}>https://example.com/guide.html</a> and ${fileAnchor('docs/guide.md', 'docs/guide.md')}</p>`,
    );
  });

  it('keeps the old HTML without a context', () => {
    expect(safeMarkdownHtml('See **`src/app.ts:42`** and https://example.com/a', 'app')).toBe(
      `<p>See <strong><code>src/<mark>app</mark>.ts:42</code></strong> and <a href="https://example.com/a" ${externalAttributes}>https://example.com/a</a></p>`,
    );
  });
});

describe('terminal file links', () => {
  it('keeps HTTP links external and only links paths outside them', () => {
    const text = 'See https://example.com/a and docs/guide.md';
    expect(linkifyTerminalText(text, context)).toBe(`See ${docsLink} and ${fileAnchor('docs/guide.md', 'docs/guide.md')}`);
    expect(linkifyTerminalText(text)).toBe(`See ${docsLink} and docs/guide.md`);
    expect(linkifyTerminalText('https://example.com/docs/guide.html', context)).not.toContain('data-file-path');
  });

  it('links a web address after a prose label without workspace inspection', () => {
    expect(linkifyTerminalText(`URL:${guideUrl}`)).toBe(`URL:<a class="terminal-link" href="${guideUrl}" ${externalAttributes}>${guideUrl}</a>`);
  });

  it('escapes surrounding text and links a plain row without linking the whole frame', () => {
    const rendered = renderTerminalContent('<src/app.ts:42 &>', 'plain', { fileLinks: context });
    expect(rendered.rows[0].html).toBe(`<span class="ansi-line">&lt;${fileAnchor('src/app.ts:42', 'src/app.ts', 42)} &amp;&gt;</span>`);
    expect(rendered.html).toBe('&lt;src/app.ts:42 &amp;&gt;');
  });

  it.each(['\x1b[32m M src/app.ts:42\x1b[0m', '│ M src/app.ts:42 │'])('keeps row text and columns in %s', (text) => {
    const options = { preserveLayout: true };
    const [plain] = renderTerminalContent(text, 'ansi', options).rows;
    const [linked] = renderTerminalContent(text, 'ansi', { ...options, fileLinks: context }).rows;
    expect(linked).toEqual({ ...plain, html: plain.html.replace('src/app.ts:42', fileAnchor('src/app.ts:42', 'src/app.ts', 42)) });
  });

  it('does not link a path suffix after a non-ASCII cell in a fixed grid', () => {
    const text = '│ src/éfoo/bar.ts │';
    const options = { preserveLayout: true };
    const [plain] = renderTerminalContent(text, 'ansi', options).rows;
    const [linked] = renderTerminalContent(text, 'ansi', { ...options, fileLinks: context }).rows;
    expect(plain.fixedGrid).toBe(true);
    expect(linked).toEqual(plain);
  });

  it.each([
    'src/\x1b[32mfoo/bar.ts\x1b[0m',
    'src/app.ts\x1b[32m.backup\x1b[0m',
    'src/app.ts:4\x1b[32m2\x1b[0m',
    '│ https://example.com/café.md?path=src/app.ts │',
    '│ (https://example.com/café?text=)&path=src/app.ts) │',
  ])('does not link partial paths or paths inside a fragmented URI in %s', (text) => {
    const [row] = renderTerminalContent(text, 'ansi', { preserveLayout: true, fileLinks: context }).rows;
    expect(row.html).not.toContain('data-file-path');
  });

  it('keeps the file link offsets after tabs and non-ASCII cells', () => {
    const [row] = renderTerminalContent('│ é\t\x1b[32m\tsrc/app.ts:42\x1b[0m │', 'ansi', { preserveLayout: true, fileLinks: context }).rows;
    expect(row.fixedGrid).toBe(true);
    expect(row.html).toContain(fileAnchor('src/app.ts:42', 'src/app.ts', 42));
  });

  it.each(unsupportedUris)('leaves %s plain without hiding adjacent links', (uri) => {
    for (const fileLinks of [null, context]) {
      const path = fileLinks ? fileAnchor('docs/guide.md', 'docs/guide.md') : 'docs/guide.md';
      for (const [open, close] of [['(', ')'], ["'", "'"]]) {
        expect(linkifyTerminalText(`${open}${uri}${close},docs/guide.md https://example.com/a`, fileLinks)).toBe(
          `${escapeHtml(open + uri + close)},${path} ${docsLink}`,
        );
      }
    }
  });

  it('leaves embedded HTTPS text plain when ANSI splits the unsupported URI', () => {
    const [row] = renderTerminalContent('file:///tmp/notes.md?url=\x1b[32mhttps://example.com/guide.html\x1b[0m', 'ansi').rows;
    expect(row.html).not.toContain('<a ');
  });
});
