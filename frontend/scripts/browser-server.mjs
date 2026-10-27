import { createHash } from 'node:crypto';
import { createReadStream } from 'node:fs';
import { readFile, stat } from 'node:fs/promises';
import { extname, resolve, sep } from 'node:path';
import { createServer } from 'node:http';

const root = resolve(process.argv[2] || 'dist');
const port = Number(process.env.PORT || 4173);
const types = {
  '.css': 'text/css; charset=utf-8',
  '.woff2': 'font/woff2',
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.webmanifest': 'application/manifest+json; charset=utf-8',
};

const notFoundBody = await readFile(resolve(root, '404.html'));
const policies = new Map([...((await readFile(resolve(root, '_headers'), 'utf8'))
  .matchAll(/^(\/[^\n]*)\n {2}Cache-Control: ([^\n]*)$/gm))]
  .map((match) => [match[1], match[2]]));

function cachePolicy(pathname) {
  if (policies.has(pathname)) return policies.get(pathname);
  if (pathname.startsWith('/assets/')) return policies.get('/assets/*');
  if (pathname.startsWith('/builds/')) return policies.get('/builds/*');
  return 'no-cache';
}

function notFound(response) {
  response.writeHead(404, {
    'Cache-Control': 'no-store',
    'Content-Type': 'text/html; charset=utf-8',
    'X-Content-Type-Options': 'nosniff',
  }).end(notFoundBody);
}

function encodingQuality(value, encoding) {
  if (!value) return 0;
  let explicit;
  let wildcard = 0;
  for (const item of value.split(',')) {
    const [rawName, ...parameters] = item.split(';');
    const name = rawName.trim().toLowerCase();
    if (name !== encoding && name !== '*') continue;
    let quality = 1;
    for (const parameter of parameters) {
      const [rawKey, rawValue] = parameter.split('=', 2);
      if (rawKey?.trim().toLowerCase() !== 'q') continue;
      quality = Number(rawValue?.trim());
      if (!Number.isFinite(quality) || quality < 0 || quality > 1) quality = 0;
      break;
    }
    if (name === encoding) explicit = Math.max(explicit ?? 0, quality);
    else wildcard = Math.max(wildcard, quality);
  }
  return explicit ?? wildcard;
}

createServer(async (request, response) => {
  const pathname = decodeURIComponent(new URL(request.url || '/', 'http://localhost').pathname);
  const relative = pathname === '/' ? 'index.html' : pathname.replace(/^\/+/, '');
  const file = resolve(root, relative);
  if (file !== root && !file.startsWith(`${root}${sep}`)) {
    notFound(response);
    return;
  }
  const details = await stat(file).catch(() => null);
  if (!details?.isFile()) {
    notFound(response);
    return;
  }
  const compressedFile = `${file}.br`;
  const compressedDetails = await stat(compressedFile).catch(() => null);
  const useBrotli = compressedDetails?.isFile()
    && encodingQuality(request.headers['accept-encoding'], 'br') > 0;
  const headers = {
    'Cache-Control': cachePolicy(pathname),
    'Content-Security-Policy': "default-src 'self'; connect-src 'self' https: ws: wss:; img-src 'self' blob: data:; style-src 'self'; style-src-attr 'unsafe-inline'; script-src 'self'; worker-src 'self'; manifest-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'",
    'Content-Type': types[extname(file)] || 'application/octet-stream',
    Vary: 'Accept-Encoding',
    'X-Content-Type-Options': 'nosniff',
  };
  if (useBrotli) headers['Content-Encoding'] = 'br';
  const selectedFile = useBrotli ? compressedFile : file;
  const etag = `"${createHash('sha256').update(await readFile(selectedFile)).digest('hex')}"`;
  headers.ETag = etag;
  if (request.headers['if-none-match']?.split(',').some((value) => value.trim().replace(/^W\//, '') === etag)) {
    response.writeHead(304, headers).end();
    return;
  }
  response.writeHead(200, headers);
  if (request.method === 'HEAD') response.end();
  else createReadStream(selectedFile).pipe(response);
}).listen(port, '127.0.0.1', () => {
  console.log(`Serving ${root} on http://127.0.0.1:${port}`);
});
