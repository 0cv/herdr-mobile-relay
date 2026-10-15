import { appendFileSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

export default function evidence(pi) {
  const root = process.env.HERDR_P1_FIXTURE_ROOT;
  if (!root || process.env.PI_OFFLINE !== '1' || process.env.PI_CODING_AGENT_DIR !== join(root, 'pi')) throw new Error('isolated Pi required');
  const mode = process.argv[process.argv.indexOf('--tui-mode') + 1];
  if (!['fullscreen', 'regular'].includes(mode)) throw new Error('explicit fixture renderer required');
  const file = join(root, 'logs', `pi-${mode}-events.jsonl`);
  const record = value => appendFileSync(file, JSON.stringify({ pid: process.pid, ns: process.hrtime.bigint().toString(), ...value }) + '\n', { mode: 0o600 });
  let received = 0;
  const input = data => {
    received += Buffer.byteLength(data);
    if (received > 64 * 1024) throw new Error('fixture input bound exceeded');
    record({ event: 'input', base64: Buffer.from(data).toString('base64') });
  };
  let originalSetWidget;
  let overlayTimer;
  pi.registerCommand('p1-overlay', {
    description: 'Owned qualification overlay',
    handler: async (_args, ctx) => {
      await ctx.ui.custom((_tui, _theme, _keys, done) => {
        record({ event: 'overlay_started' });
        overlayTimer = setTimeout(() => {
          record({ event: 'overlay_ended' });
          writeFileSync(join(root, 'overlay-done'), 'done', { mode: 0o600 });
          done();
        }, 3000);
        return { render: () => ['P1 private overlay'], invalidate() {} };
      }, { overlay: true, overlayOptions: { nonCapturing: true, width: 24 } });
    },
  });
  pi.registerCommand('p1-remove-widget', {
    description: 'Owned qualification widget baseline',
    handler: async (_args, ctx) => {
      ctx.ui.setWidget('herdr-mobile-relay:scroll-state', undefined);
      record({ event: 'widget_removed' });
    },
  });
  pi.on('before_provider_request', () => {
    record({ event: 'forbidden_provider_request' });
    throw new Error('no model requests allowed in P1 qualification');
  });
  pi.on('session_start', (_event, ctx) => {
    process.stdin.on('data', input);
    record({ event: 'session_start', session: ctx.sessionManager.getSessionId(), file: ctx.sessionManager.getSessionFile(), rows: process.stdout.rows, columns: process.stdout.columns });
    originalSetWidget = ctx.ui.setWidget;
    ctx.ui.setWidget = function(key, factory, options) {
      if (key !== 'herdr-mobile-relay:scroll-state' || typeof factory !== 'function') return originalSetWidget.call(this, key, factory, options);
      return originalSetWidget.call(this, key, (tui, theme) => {
        const component = factory(tui, theme);
        const render = component.render;
        component.render = function(...args) {
          const rows = render.apply(this, args);
          record({ event: 'widget', renderer: tui.mode, rows: rows.length, inputHandler: typeof component.handleInput, mouseHandler: typeof component.handleMouse });
          return rows;
        };
        return component;
      }, options);
    };
    process.stdout.on('resize', () => record({ event: 'resize', rows: process.stdout.rows, columns: process.stdout.columns }));
    const seed = readFileSync(ctx.sessionManager.getSessionFile(), 'utf8');
    if (!seed.includes('P1 TEST USER [096]')) throw new Error('offline transcript missing');
    writeFileSync(join(root, `pi-${mode}.json`), JSON.stringify({ pid: process.pid, session: ctx.sessionManager.getSessionId(), file: ctx.sessionManager.getSessionFile() }), { mode: 0o600 });
  });
  pi.on('session_shutdown', (event, ctx) => {
    clearTimeout(overlayTimer);
    record({ event: 'shutdown', reason: event.reason });
    process.stdin.removeListener('data', input);
    if (originalSetWidget) ctx.ui.setWidget = originalSetWidget;
  });
}
