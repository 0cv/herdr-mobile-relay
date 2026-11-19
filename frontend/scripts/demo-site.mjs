import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import { build } from 'vite';

const frontend = fileURLToPath(new URL('..', import.meta.url));
const [source, destination] = process.argv.slice(2);
if (!source || !destination) throw new Error('Usage: demo-site.mjs <build directory> <new demo directory>');

const root = resolve(source);
const output = resolve(destination);
const release = JSON.parse(await readFile(join(root, 'release.json'), 'utf8'));
const { path: stylesheet, integrity } = release.files.stylesheet;
const script = 'assets/demo.js';
const bundle = await build({
  configFile: false,
  root: frontend,
  publicDir: false,
  plugins: [svelte()],
  resolve: {
    alias: [
      { find: /^\$lib\/(preferences|store)$/, replacement: join(frontend, 'src/demo/home-context.ts') },
      { find: '$lib', replacement: join(frontend, 'src/lib') },
      { find: '$components', replacement: join(frontend, 'src/components') },
    ],
  },
  build: {
    write: false,
    target: 'esnext',
    rolldownOptions: {
      input: join(frontend, 'src/demo/main.ts'),
      output: { codeSplitting: false, entryFileNames: script },
    },
  },
});
const entry = bundle.output.find((file) => file.type === 'chunk' && file.isEntry);
if (!entry) throw new Error('Demo entry was not built.');

const loader = `import('/${script}').then(() => document.getElementById('demo-startup').remove(), () => {});`;
const policy = "default-src 'self'; connect-src 'none'; worker-src 'none'; style-src-attr 'unsafe-inline'; img-src 'self' data:";
const html = `<!doctype html>
<html lang="en" data-demo data-theme="nord" data-interface-size="compact">
<head>
  <meta charset="utf-8">
  <meta http-equiv="Content-Security-Policy" content="${policy}">
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
  <meta name="theme-color" content="#2e3440">
  <title>Herdr — Demo</title>
  <link rel="stylesheet" href="/${stylesheet}" integrity="${integrity}" crossorigin="anonymous">
</head>
<body>
  <div id="app"></div>
  <main id="demo-startup" class="page">
    <h1>Demo — sample data</h1>
    <p role="alert">Demo could not load. Reload this page to try again.</p>
  </main>
  <script type="module" src="/assets/demo-loader.js"></script>
</body>
</html>
`;

await mkdir(output);
const files = new Map([
  ['index.html', html],
  [script, entry.code],
  ['assets/demo-loader.js', loader],
  ['_headers', '/assets/*\n  Cache-Control: no-cache\n'],
]);
for (const path of [stylesheet, '404.html', 'fonts/nerd-symbols-mono-v3.4.0.woff2']) {
  files.set(path, await readFile(join(root, path)));
}
for (const [path, content] of files) {
  const target = join(output, path);
  await mkdir(dirname(target), { recursive: true });
  await writeFile(target, content);
}
