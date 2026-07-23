import { createReadStream } from 'node:fs';
import { stat } from 'node:fs/promises';
import { extname, resolve, sep } from 'node:path';
import { createServer } from 'node:http';

const root = resolve(process.argv[2] || 'dist');
const port = Number(process.env.PORT || 4173);
const types = {
  '.css': 'text/css; charset=utf-8',
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.png': 'image/png',
  '.svg': 'image/svg+xml',
  '.webmanifest': 'application/manifest+json; charset=utf-8',
};

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
    response.writeHead(404).end('Not found\n');
    return;
  }
  const details = await stat(file).catch(() => null);
  if (!details?.isFile()) {
    response.writeHead(404).end('Not found\n');
    return;
  }
  const compressedFile = `${file}.br`;
  const compressedDetails = await stat(compressedFile).catch(() => null);
  const useBrotli = compressedDetails?.isFile()
    && encodingQuality(request.headers['accept-encoding'], 'br') > 0;
  const headers = {
    'Cache-Control': 'no-cache',
    'Content-Type': types[extname(file)] || 'application/octet-stream',
    Vary: 'Accept-Encoding',
    'X-Content-Type-Options': 'nosniff',
  };
  if (useBrotli) headers['Content-Encoding'] = 'br';
  response.writeHead(200, headers);
  createReadStream(useBrotli ? compressedFile : file).pipe(response);
}).listen(port, '127.0.0.1', () => {
  console.log(`Serving ${root} on http://127.0.0.1:${port}`);
});
