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
const variants = new Map(Object.entries({
  'lib/transports/index.ts': 'transport.ts',
  'lib/config.ts': 'config.ts',
  'lib/route-history.ts': 'route-history.ts',
  'lib/attention.svelte.ts': 'inactive.ts',
  'lib/push.ts': 'inactive.ts',
  'lib/push-open.ts': 'inactive.ts',
  'lib/viewed-pane.ts': 'inactive.ts',
  'lib/update-reload.ts': 'inactive.ts',
  'lib/updates.ts': 'updates.ts',
  'lib/wake-lock.ts': 'inactive.ts',
  'lib/launch.ts': 'launch.ts',
  'lib/composer-policy.ts': 'composer-policy.ts',
  'lib/questions.ts': 'questions.ts',
  'lib/terminal-menu.ts': 'inactive.ts',
  'lib/terminal-key-messages.ts': 'terminal-key-messages.ts',
  'lib/rich-output.ts': 'rich-output.ts',
  'lib/markdown.ts': 'markdown.ts',
  'components/SettingsView.svelte': 'Settings.svelte',
  'components/QuestionForm.svelte': 'RolloutQuestion.svelte',
  'components/WorkspaceManager.svelte': 'Workspaces.svelte',
  'components/ManageDialog.svelte': 'ManageAgent.svelte',
  'components/UpdateProgressDialog.svelte': 'NoUpdateProgress.svelte',
  'components/ConversationMessageActions.svelte': 'NoMessageActions.svelte',
  'components/TerminalResponseActions.svelte': 'NoResponseActions.svelte',
}).map(([original, variant]) => [join(frontend, 'src', original), join(frontend, 'src/demo', variant)]));

const demoModules = {
  name: 'demo-modules',
  enforce: 'pre',
  async resolveId(source, importer, options) {
    if (!importer || !source.startsWith('.') && !source.startsWith('/') && !source.startsWith('$')) return;
    const original = await this.resolve(source, importer, { ...options, skipSelf: true });
    const variant = variants.get(original?.id);
    if (variant && variant !== importer) return variant;
  },
};

function buildEntry(input, name, { external, ...config } = {}) {
  return build({
    configFile: false,
    root: frontend,
    publicDir: false,
    ...config,
    build: {
      write: false,
      target: 'esnext',
      cssCodeSplit: false,
      modulePreload: false,
      rolldownOptions: {
        external,
        input: join(frontend, input),
        output: {
          codeSplitting: false,
          entryFileNames: `assets/${name}.js`,
          assetFileNames: ({ names }) => names.some((file) => file.endsWith('.css'))
            ? `assets/${name}.css` : 'assets/[name][extname]',
        },
      },
    },
  });
}

const loader = await buildEntry('src/demo/main.ts', 'demo-loader', {
  external: ['/assets/demo.js'],
});
const app = await buildEntry('src/demo/app.ts', 'demo', {
  plugins: [demoModules, svelte()],
  resolve: {
    alias: {
      $lib: join(frontend, 'src/lib'),
      $components: join(frontend, 'src/components'),
    },
  },
  define: {
    __APP_PROTOCOL_VERSION__: '3',
    __APP_VERSION__: JSON.stringify(release.version),
    __APP_ASSET_VERSION__: JSON.stringify(release.assets),
    __APP_BUILD_ID__: JSON.stringify(release.build),
    __SERVICE_WORKER_URL__: '""',
  },
});
const outputFiles = [...loader.output, ...app.output];
const stylesheets = outputFiles.filter((file) => file.type === 'asset' && file.fileName.endsWith('.css'));
if (!stylesheets.length) throw new Error('Demo stylesheet was not built.');
const policy = "default-src 'self'; connect-src 'none'; worker-src 'none'; style-src-attr 'unsafe-inline'; img-src 'self' data:";
const html = `<!doctype html>
<html lang="en" spellcheck="false" autocorrect="off" data-theme="nord" data-interface-size="compact">
<head>
  <meta charset="utf-8">
  <meta http-equiv="Content-Security-Policy" content="${policy}">
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
  <meta name="theme-color" content="#2e3440">
  <title>Herdr — Demo</title>
  ${stylesheets.map(({ fileName }) => `<link rel="stylesheet" href="/${fileName}">`).join('\n  ')}
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
  ['_headers', '/assets/*\n  Cache-Control: no-cache\n'],
]);
for (const file of outputFiles) files.set(file.fileName, file.type === 'chunk' ? file.code : file.source);
for (const path of ['404.html', 'fonts/nerd-symbols-mono-v3.4.0.woff2']) {
  files.set(path, await readFile(join(root, path)));
}
for (const [path, content] of files) {
  const target = join(output, path);
  await mkdir(dirname(target), { recursive: true });
  await writeFile(target, content);
}
