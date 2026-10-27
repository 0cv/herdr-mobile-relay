const cacheRules = [
  ['/herdr-bootstrap.js', 'no-cache, no-store'],
  ['/manifest-loader.js', 'no-cache, no-store'],
  ['/manifest.webmanifest', 'no-cache, no-store'],
  ['/setup.webmanifest', 'no-cache, no-store'],
  ['/sw.js', 'no-cache, no-store'],
  ['/version.json', 'no-cache, no-store'],
  ['/release.json', 'no-cache, no-store'],
  ['/', 'no-cache, no-store'],
  ['/index.html', 'no-cache, no-store'],
  ['/404.html', 'no-store'],
  ['/404', 'no-store'],
  ['/builds/*', 'no-cache'],
  ['/assets/*', 'no-cache'],
] as const;

export function pagesHeaders(): string {
  return `${cacheRules.map(([route, policy]) => `${route}\n  Cache-Control: ${policy}`).join('\n\n')}\n`;
}

function nonemptyLines(source: string): string[] {
  return source.split(/\r?\n/).map((line) => line.trimEnd()).filter((line) => line.trim().length > 0);
}

export function validatePagesHeaders(source: string): void {
  if (nonemptyLines(source).join('\n') !== nonemptyLines(pagesHeaders()).join('\n')) {
    throw new Error('_headers must use the generated revalidation and non-storing error policy without overlapping rules');
  }
}

export function validatePagesRedirects(source: string, entry: string): void {
  const expected = `/ ${entry} 302\n/index.html ${entry} 302`;
  if (nonemptyLines(source).join('\n') !== expected) {
    throw new Error('_redirects must route only stable bootstrap paths to the current build entry');
  }
}
