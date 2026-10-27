import { existsSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import { pagesHeaders, validatePagesHeaders, validatePagesRedirects } from '../../scripts/pages-policy';

const entry = '/builds/1.2.3-400-0123456789abcdef/index.html';

describe('Pages cache policy', () => {
  it('has no competing public header file copied by Vite', () => {
    expect(existsSync(new URL('../../public/_headers', import.meta.url))).toBe(false);
  });

  it('revalidates assets and entries while keeping bootstrap and error documents non-storing', () => {
    const headers = pagesHeaders();
    expect(headers).toContain('/assets/*\n  Cache-Control: no-cache\n');
    expect(headers).toContain('/builds/*\n  Cache-Control: no-cache\n');
    for (const path of ['/', '/index.html', '/herdr-bootstrap.js', '/manifest-loader.js', '/release.json', '/version.json', '/manifest.webmanifest', '/setup.webmanifest', '/sw.js']) {
      expect(headers).toContain(`${path}\n  Cache-Control: no-cache, no-store\n`);
    }
    for (const path of ['/404', '/404.html']) {
      expect(headers).toContain(`${path}\n  Cache-Control: no-store\n`);
    }
    expect(headers).not.toMatch(/immutable|max-age/);
    expect(() => validatePagesHeaders(headers)).not.toThrow();
    expect(() => validatePagesHeaders(headers.replaceAll('\n', '\r\n'))).not.toThrow();
  });

  it.each([
    ['old asset policy', () => pagesHeaders().replace('/assets/*\n  Cache-Control: no-cache', '/assets/*\n  Cache-Control: public, max-age=31536000, immutable')],
    ['old entry policy', () => pagesHeaders().replace('/builds/*\n  Cache-Control: no-cache', '/builds/*\n  Cache-Control: public, max-age=31536000, immutable')],
    ['overlapping wildcard', () => `${pagesHeaders()}\n/*\n  Cache-Control: public, max-age=31536000, immutable\n`],
    ['overlapping origin rule', () => `${pagesHeaders()}\nhttps://app.example.com/assets/*\n  Cache-Control: immutable\n`],
    ['duplicate policy', () => `${pagesHeaders()}\n/assets/*\n  Cache-Control: no-cache\n`],
    ['header removal', () => `${pagesHeaders()}\n/assets/*\n  ! Cache-Control\n`],
    ['bad indentation', () => pagesHeaders().replaceAll('  Cache-Control:', 'Cache-Control:')],
    ['missing error policy', () => pagesHeaders().replace('/404.html\n  Cache-Control: no-store\n', '')],
  ])('rejects %s rather than accumulating headers', (_name, makeHeaders) => {
    expect(() => validatePagesHeaders(makeHeaders())).toThrow(/without overlapping rules/);
  });
});

describe('Pages stable routing', () => {
  it('selects exactly the finalized entry for root and index', () => {
    expect(() => validatePagesRedirects(`/ ${entry} 302\n/index.html ${entry} 302\n`, entry)).not.toThrow();
  });

  it.each([
    `/ ${entry} 302\n/index.html /builds/stale/index.html 302\n`,
    `/ ${entry} 302\n/index.html ${entry} 302\n/* /index.html 200\n`,
    '/ https://other.example.com/ 302\n',
  ])('rejects stale, SPA, and cross-origin selectors', (redirects) => {
    expect(() => validatePagesRedirects(redirects, entry)).toThrow(/only stable bootstrap paths/);
  });
});
