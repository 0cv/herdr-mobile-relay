import { expect, test } from '@playwright/test';

for (const kind of ['tabs', 'workspaces'] as const) {
  for (const journey of ['geometry', 'gestures'] as const) {
  test(`${kind}: native mouse long-press ${journey} with mocked transport`, async ({ page }) => {
    await page.addInitScript(() => {
      localStorage.setItem('herdr_relays', JSON.stringify([{ id: 'test', label: 'Test', url: 'ws://127.0.0.1:8871', token: '' }]));
      class MockSocket {
        static OPEN = 1;
        static CONNECTING = 0;
        static CLOSED = 3;
        readyState = 0;
        onopen: (() => void) | null = null;
        onmessage: ((event: MessageEvent) => void) | null = null;
        constructor() {
          queueMicrotask(() => {
            this.readyState = 1;
            this.onopen?.();
            this.server({ type: 'push_config', protocol: 3, host: 'test', capabilities: ['tab_reorder', 'workspace_management', 'workspace_reorder_block'] });
            this.server({ type: 'workspaces', workspaces: [1, 2, 3].map((n) => ({ workspace_id: `w${n}`, number: n, label: `Workspace ${n}`, pane_count: 1, tab_count: 1, cwd: `/work/${n}` })) });
            this.server({ type: 'agents', agents: [1, 2, 3].map((n) => ({ pane_id: `p${n}`, server_session_id: 'session', terminal_id: `terminal${n}`, generation: 1, workspace_id: 'w1', project: 'project', agent: 'codex', status: 'working', tab_id: `t${n}`, tab_number: n, tab_order: n, tab_label: `Tab ${n}` })) });
          });
        }
        server(message: unknown) { this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(message) })); }
        send(payload: string) {
          const command = JSON.parse(payload);
          if (command.type === 'tab_reorder' || command.type === 'workspace_reorder') {
            document.body.dataset.reorder = JSON.stringify(command);
          }
        }
        close() { this.readyState = 3; }
      }
      Object.defineProperty(window, 'WebSocket', { value: MockSocket });
    });
    await page.goto('/');
    if (kind === 'workspaces') await page.getByRole('button', { name: 'Manage workspaces' }).click();
    const slots = page.locator(kind === 'tabs' ? '.workspace-tab' : '.workspace-management-slot');
    await expect(slots).toHaveCount(3);
    // Real layout/measurement, with intentionally unequal row heights and gaps.
    await slots.evaluateAll((nodes) => nodes.forEach((node, i) => {
      const element = node as HTMLElement;
      element.style.height = `${[160, 200, 180][i]}px`;
      element.style.marginBottom = `${[10, 20, 10][i]}px`;
    }));
    const handle = kind === 'tabs' ? slots.first().locator('.agent-open') : slots.first().locator('.workspace-drag-handle');
    await handle.scrollIntoViewIfNeeded();
    const start = await handle.boundingBox();
    const first = await slots.first().boundingBox();
    const second = await slots.nth(1).boundingBox();
    const last = await slots.nth(2).boundingBox();
    expect(start && first && second && last).toBeTruthy();
    const span = first!.height + Math.max(0, second!.y - first!.y - first!.height);
    if (journey === 'gestures') {
      // Native mouse slop cancellation. Release outside the card to avoid opening it.
      await page.mouse.move(start!.x + start!.width / 2, start!.y + start!.height / 2);
      await page.mouse.down();
      await page.mouse.move(2, 2);
      await page.waitForTimeout(650);
      await expect(slots.first()).toHaveCSS('transform', 'none');
      await page.mouse.up();
      await expect(page.locator('body')).not.toHaveAttribute('data-reorder');

      // Native mouse hold, but synthetic touch/context/cancel dispatch: NOT native touch.
      await handle.evaluate(node => node.addEventListener('pointerdown', event => {
        (node as HTMLElement).dataset.pointerId = String((event as PointerEvent).pointerId);
      }));
      // Isolate outside release/cancel from slop using synthetic document events.
      // Native mouse supplies the down; these are not native touch evidence.
      for (const type of ['pointerup', 'pointercancel']) {
        await page.mouse.move(start!.x + start!.width / 2, start!.y + start!.height / 2);
        await page.mouse.down();
        const pointerId = Number(await handle.getAttribute('data-pointer-id'));
        await page.evaluate(({ type, pointerId }) => {
          document.body.dispatchEvent(new PointerEvent(type, { bubbles: true, pointerId }));
        }, { type, pointerId });
        await page.waitForTimeout(650);
        await expect(slots.first()).toHaveCSS('transform', 'none');
        await page.mouse.move(2, 2);
        await page.mouse.up();
        await expect(page.locator('body')).not.toHaveAttribute('data-reorder');
      }
      const prevented = (type: string) => handle.evaluate((node, type) => {
        const event = new Event(type, { bubbles: true, cancelable: true });
        node.dispatchEvent(event);
        return event.defaultPrevented;
      }, type);
      await page.mouse.move(start!.x + start!.width / 2, start!.y + start!.height / 2);
      await page.mouse.down();
      expect(await prevented('touchmove')).toBe(false);
      expect(await prevented('contextmenu')).toBe(true);
      await page.waitForTimeout(650);
      expect(await prevented('touchmove')).toBe(true);
      expect(await prevented('contextmenu')).toBe(true);
      await handle.evaluate(node => node.dispatchEvent(new PointerEvent('pointercancel', {
        bubbles: true, pointerId: Number((node as HTMLElement).dataset.pointerId),
      })));
      await expect(slots.first()).toHaveCSS('transform', 'none');
      expect(await prevented('touchmove')).toBe(false);
      expect(await prevented('contextmenu')).toBe(false);
      await page.mouse.up();
      await expect(page.locator('body')).not.toHaveAttribute('data-reorder');

      return;
    }

    // Original native mouse geometry/commit journey follows unchanged.
    await page.mouse.move(start!.x + start!.width / 2, start!.y + start!.height / 2);
    await page.mouse.down();
    await page.waitForTimeout(650);
    await expect(slots.first()).toHaveCSS('transform', 'matrix(1, 0, 0, 1, 0, 0)');
    await page.mouse.move(start!.x + start!.width / 2, last!.y + last!.height / 2 + 5);
    for (const index of [1, 2]) {
      await expect.poll(() => slots.nth(index).evaluate((node) => new DOMMatrix(getComputedStyle(node).transform).m42)).toBeCloseTo(-span, 2);
    }
    await page.mouse.up();
    await expect(page.locator('body')).toHaveAttribute('data-reorder', /reorder/);
    const command = JSON.parse((await page.locator('body').getAttribute('data-reorder'))!);
    if (kind === 'tabs') {
      expect(command).toMatchObject({ type: 'tab_reorder', insert_index: 3 });
    } else {
      expect(command).toMatchObject({ type: 'workspace_reorder', workspace_ids: ['w1'], before_workspace_id: '' });
      expect(command).not.toHaveProperty('insert_index');
    }
  });
  }
}

const networkAttempts = new WeakMap<import('@playwright/test').BrowserContext, { http: number; sockets: number }>();

// Install before every journey's navigation. MockSocket never uses a native socket.
test.beforeEach(async ({ context }) => {
  const attempts = { http: 0, sockets: 0 };
  networkAttempts.set(context, attempts);
  await context.route('**/*', async route => {
    if (new URL(route.request().url()).origin !== 'http://127.0.0.1:4173') {
      attempts.http++;
      await route.abort('blockedbyclient');
      return;
    }
    await route.continue();
  });
  await context.routeWebSocket(/.*/, async socket => {
    attempts.sockets++;
    await socket.close({ code: 1008, reason: 'Unexpected native test socket' });
  });
});
test.afterEach(async ({ context }, info) => {
  const attempts = networkAttempts.get(context);
  await info.attach('network-attempt-counts', { body: JSON.stringify(attempts), contentType: 'application/json' });
  expect(attempts).toEqual({ http: 0, sockets: 0 }); // Counts only: never log unexpected URLs.
});

async function ownerClickFixture(page: import('@playwright/test').Page) {
  await page.addInitScript(() => {
    localStorage.setItem('herdr_relays', JSON.stringify([{ id: 'test', label: 'Test', url: 'ws://127.0.0.1:8871', token: '' }]));
    class OwnerMockSocket {
      static OPEN = 1;
      static CONNECTING = 0;
      static CLOSED = 3;
      readyState = 0;
      onopen: (() => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      constructor(url: string) {
        if (new URL(url).href !== 'ws://127.0.0.1:8871/') throw new Error('Unexpected mock transport');
        queueMicrotask(() => {
          this.readyState = 1;
          this.onopen?.();
          this.server({ type: 'push_config', protocol: 3, host: 'test', capabilities: ['tab_reorder'] });
          this.server({ type: 'agents', agents: [1, 2, 3].map(n => ({
            pane_id: `p${n}`, server_session_id: 'session', terminal_id: `terminal${n}`, generation: 1,
            workspace_id: 'w1', project: 'project', agent: 'codex', status: 'working',
            tab_id: `t${n}`, tab_number: n, tab_order: n, tab_label: `Tab ${n}`,
          })) });
        });
      }
      server(message: unknown) { this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(message) })); }
      send(payload: string) {
        if (JSON.parse(payload).type === 'tab_reorder') document.body.dataset.unexpectedOwnerReorder = 'true';
      }
      close() { this.readyState = 3; }
    }
    Object.defineProperty(window, 'WebSocket', { value: OwnerMockSocket });
  });
  await page.goto('/');
  await expect(page.locator('.workspace-tab')).toHaveCount(3);
  const handle = page.locator('.agent-open').first();
  await handle.scrollIntoViewIfNeeded();
  await handle.evaluate(node => {
    const element = node as HTMLElement;
    type Entry = { phase: string; pointerId?: number; pointerType?: string; trusted?: boolean; prevented?: boolean; detail?: number };
    let entries: Entry[] = [];
    const record = (phase: string, event: MouseEvent | PointerEvent) => {
      const pointer = event as PointerEvent;
      entries.push({ phase, pointerId: pointer.pointerId, pointerType: pointer.pointerType,
        trusted: event.isTrusted, prevented: event.defaultPrevented, detail: event.detail });
      document.documentElement.dataset.ownerProof = JSON.stringify(entries);
    };
    node.addEventListener('pointerdown', raw => {
      const event = raw as PointerEvent;
      if (!event.isTrusted) return;
      entries = [];
      record('native-down', event);
      element.dataset.ownerId = String(event.pointerId);
      element.dataset.ownerType = event.pointerType;
    });
    // Registered AFTER the action: its node pointerup listener finishes/resets first.
    node.addEventListener('pointerup', raw => {
      const event = raw as PointerEvent;
      if (!event.isTrusted) return;
      record('later-native-up-listener', event);
      if (!element.dataset.overlapId) return;
      const touch = new Event('touchmove', { bubbles: true, cancelable: true });
      node.dispatchEvent(touch);
      entries.push({ phase: 'helper-reset', prevented: touch.defaultPrevented });
      const other = new PointerEvent('click', { bubbles: true, cancelable: true, detail: 1,
        pointerId: Number(element.dataset.overlapId), pointerType: element.dataset.overlapType });
      delete element.dataset.overlapId;
      node.dispatchEvent(other);
      record('delayed-B-return', other);
    });
    // Later capture sees prevention even when production stops click propagation.
    node.addEventListener('click', raw => {
      const event = raw as PointerEvent;
      record(event.isTrusted ? 'native-owner-click' : 'delayed-B-click', event);
    }, true);
  });
  return handle;
}

for (const activation of ['click', 'Enter', 'Space'] as const) {
  test(`tabs: native owner with synthetic delayed overlap, then ${activation}`, async ({ page }, info) => {
    const handle = await ownerClickFixture(page);
    const homeURL = page.url();
    for (let cycle = 0; cycle < 3; cycle++) {
      const box = (await handle.boundingBox())!;
      await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
      await page.mouse.down();
      const owner = { pointerId: Number(await handle.getAttribute('data-owner-id')),
        pointerType: await handle.getAttribute('data-owner-type') };
      expect(Number.isInteger(owner.pointerId) && owner.pointerId > 0).toBe(true);
      expect(['mouse', 'touch', 'pen']).toContain(owner.pointerType);
      await page.waitForTimeout(650);
      await expect(page.locator('.workspace-tab').first()).toHaveClass(/tab-dragging/);
      await handle.evaluate((node, owner) => {
        const element = node as HTMLElement;
        const other = { pointerId: owner.pointerId + 1000, pointerType: owner.pointerType === 'pen' ? 'mouse' : 'pen' };
        element.dataset.overlapId = String(other.pointerId);
        element.dataset.overlapType = other.pointerType;
        for (const type of ['pointerdown', 'pointerup']) node.dispatchEvent(new PointerEvent(type, {
          bubbles: true, cancelable: true, isPrimary: true, button: 0, ...other,
        })); // Click withheld until later native pointerup listener, after helper reset.
      }, owner);
      await page.mouse.up();
      const trace = JSON.parse((await page.locator('html').getAttribute('data-owner-proof'))!) as {
        phase: string; pointerId?: number; pointerType?: string; trusted?: boolean; prevented?: boolean; detail?: number;
      }[];
      await info.attach(`owner-cycle-${cycle}`, { body: JSON.stringify(trace, null, 2), contentType: 'application/json' });
      expect(trace.map(e => e.phase)).toEqual([
        'native-down', 'later-native-up-listener', 'helper-reset', 'delayed-B-click', 'delayed-B-return', 'native-owner-click',
      ]);
      expect(trace[0]).toMatchObject({ ...owner, trusted: true });
      expect(trace[1]).toMatchObject({ ...owner, trusted: true });
      expect(trace[2].prevented).toBe(false);
      expect(trace[3]).toMatchObject({ pointerId: owner.pointerId + 1000, trusted: false, prevented: true });
      expect(trace[4].prevented).toBe(true);
      const click = trace[5];
      const attributable = Number.isInteger(click.pointerId) && click.pointerId! > 0
        && ['mouse', 'touch', 'pen'].includes(click.pointerType || '');
      await info.attach(`owner-classification-${cycle}`, { body: attributable
        ? 'attributable native owner click' : 'identified B / legacy native A', contentType: 'text/plain' });
      if (attributable) expect(click).toMatchObject(owner);
      expect(click.trusted).toBe(true);
      expect(click.prevented).toBe(true);
      expect(page.url()).toBe(homeURL);
      await expect(page.locator('.agent-open')).toHaveCount(3);
      await expect(page.locator('body')).not.toHaveAttribute('data-unexpected-owner-reorder');
    }
    // Separate final-activation cases; no promise-dependent committed-drag repetition.
    if (activation === 'click') await handle.click();
    else {
      await handle.focus();
      await page.keyboard.press(activation);
    }
    await expect(page.locator('.agent-open')).toHaveCount(0);
  });
}


type AdmissionControl = {
  history: Record<string, unknown>[];
  settle(index: number, ok: boolean): void;
  snapshot(ids: number[]): void;
};

for (const kind of ['tabs', 'workspaces'] as const) {
  for (const ok of [true, false]) {
    test(`${kind}: command admission hybrid mouse/keyboard then ${ok ? 'success' : 'rejection'}`, async ({ page }, info) => {
      await page.addInitScript(() => {
        localStorage.setItem('herdr_relays', JSON.stringify([{ id: 'test', label: 'Test', url: 'ws://127.0.0.1:8871', token: '' }]));
        class AdmissionMockSocket {
          static OPEN = 1;
          static CONNECTING = 0;
          static CLOSED = 3;
          readyState = 0;
          onopen: (() => void) | null = null;
          onmessage: ((event: MessageEvent) => void) | null = null;
          history: Record<string, unknown>[] = [];
          constructor(url: string) {
            if (new URL(url).href !== 'ws://127.0.0.1:8871/') throw new Error('Unexpected mock transport');
            (window as unknown as { admission: AdmissionControl }).admission = this;
            queueMicrotask(() => {
              this.readyState = 1;
              this.onopen?.();
              this.server({ type: 'push_config', protocol: 3, host: 'test', capabilities: ['tab_reorder', 'workspace_management', 'workspace_reorder_block'] });
              this.snapshot([1, 2, 3]);
            });
          }
          server(message: unknown) { this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(message) })); }
          snapshot(ids: number[]) {
            this.server({ type: 'workspaces', workspaces: ids.map((n, i) => ({
              workspace_id: `w${n}`, number: i + 1, label: `Workspace ${n}`, pane_count: 1, tab_count: 1, cwd: `/work/${n}`,
            })) });
            this.server({ type: 'agents', agents: ids.map((n, i) => ({
              pane_id: `p${n}`, server_session_id: 'session', terminal_id: `terminal${n}`, generation: 1,
              workspace_id: 'w1', project: 'project', agent: 'codex', status: 'working',
              tab_id: `t${n}`, tab_number: n, tab_order: i + 1, tab_label: `Tab ${n}`,
            })) });
          }
          settle(index: number, ok: boolean) {
            this.server({ type: 'command_result', request_id: this.history[index].request_id, ok, error: ok ? undefined : 'Selected command refused' });
          }
          send(payload: string) {
            const command = JSON.parse(payload);
            if (command.type === 'tab_reorder' || command.type === 'workspace_reorder') this.history.push(command);
            // Deliberately withhold every correlated result until the test settles it.
          }
          close() { this.readyState = 3; }
        }
        Object.defineProperty(window, 'WebSocket', { value: AdmissionMockSocket });
      });
      await page.goto('/');
      if (kind === 'workspaces') await page.getByRole('button', { name: 'Manage workspaces' }).click();
      const selector = kind === 'tabs' ? '.workspace-tab' : '.workspace-management-slot';
      const slots = page.locator(selector);
      await expect(slots).toHaveCount(3);
      await slots.evaluateAll(nodes => nodes.forEach(node => {
        (node as HTMLElement).style.height = '160px';
        (node as HTMLElement).style.marginBottom = '10px';
      }));
      const slot = (id: number) => page.locator(`${selector}[${kind === 'tabs' ? 'data-tab-id="t' : 'data-workspace-key="w'}${id}"]`);
      const handle = (id: number) => slot(id).locator(kind === 'tabs' ? '.agent-open' : '.workspace-drag-handle');
      const order = () => slots.evaluateAll((nodes, kind) => nodes.map(node => Number((kind === 'tabs'
        ? (node as HTMLElement).dataset.tabId : (node as HTMLElement).dataset.workspaceKey)!.slice(1))), kind);
      const history = () => page.evaluate(() => (window as unknown as { admission: AdmissionControl }).admission.history);
      const key = (id: number) => handle(id).evaluate(node => node.dispatchEvent(new KeyboardEvent('keydown', {
        bubbles: true, cancelable: true, altKey: true, key: 'ArrowUp',
      })));
      const snapshot = async (ids: number[]) => {
        await page.evaluate(ids => (window as unknown as { admission: AdmissionControl }).admission.snapshot(ids), ids);
        await expect.poll(order).toEqual(ids);
      };
      const settle = (index: number, ok: boolean) => page.evaluate(({ index, ok }) =>
        (window as unknown as { admission: AdmissionControl }).admission.settle(index, ok), { index, ok });
      const expected = (id: number, index: number, before = '') => kind === 'tabs'
        ? { type: 'tab_reorder', pane_id: `p${id}`, server_session_id: 'session',
          target: { relay_id: 'test', server_session_id: 'session', pane_id: `p${id}`, terminal_id: `terminal${id}`, generation: 1 }, insert_index: index, protocol: 3 }
        : { type: 'workspace_reorder', workspace_ids: [`w${id}`], before_workspace_id: before, protocol: 3 };
      await slot(1).evaluate(node => {
        const events: { type: string; trusted: boolean; pointerType: string }[] = [];
        for (const type of ['pointerdown', 'pointerup']) node.addEventListener(type, event => {
          events.push({ type, trusted: event.isTrusted, pointerType: (event as PointerEvent).pointerType });
          document.body.dataset.admissionMouse = JSON.stringify(events);
        });
      });
      const assertHistory = async (payloads: Record<string, unknown>[]) => {
        const commands = await history();
        expect(commands).toHaveLength(payloads.length);
        expect(new Set(commands.map(command => command.request_id)).size).toBe(commands.length);
        expect(commands.map(command => {
          expect(command.request_id).toEqual(expect.any(String));
          expect(command.client_id).toEqual(expect.any(String));
          return Object.fromEntries(Object.entries(command).filter(([key]) => key !== 'request_id' && key !== 'client_id'));
        })).toEqual(payloads);
      };
      const drag = async (id: number) => {
        await handle(id).scrollIntoViewIfNeeded();
        // Inline cleanup precedes the workspace reset transition's visual completion.
        await page.evaluate(selector => new Promise<void>((resolve, reject) => {
          const deadline = performance.now() + 2000;
          let previous = '';
          let stable = 0;
          const frame = () => {
            const nodes = [...document.querySelectorAll<HTMLElement>(selector)];
            const ready = nodes.length === 3 && nodes.every(node => getComputedStyle(node).transform === 'none'
              && node.getAnimations().every(animation => animation.playState !== 'running'));
            const geometry = JSON.stringify([scrollX, scrollY, nodes.map(node => node.getBoundingClientRect().toJSON())]);
            stable = ready && geometry === previous ? stable + 1 : 0;
            previous = geometry;
            if (stable >= 2) return resolve();
            if (performance.now() >= deadline) return reject(new Error('Reorder fixture layout did not settle'));
            requestAnimationFrame(frame);
          };
          requestAnimationFrame(frame);
        }), selector);
        const start = (await handle(id).boundingBox())!;
        await page.mouse.move(start.x + start.width / 2, start.y + start.height / 2);
        await page.mouse.down();
        await page.waitForTimeout(650);
        await expect(slot(id)).toHaveClass(/dragging/);
        // Measure the live destination after lift, not a pre-hold transformed row.
        const last = (await slots.last().boundingBox())!;
        await page.mouse.move(start.x + start.width / 2, last.y + last.height / 2 + 5);
        await expect(slot(id)).not.toHaveCSS('transform', 'matrix(1, 0, 0, 1, 0, 0)');
      };
      try {
        await drag(1); // Real mouse capture/move; no synthetic pointer ownership claim.
        // Swap the other two rows without relocating the captured native owner node.
        await key(3); // Synthetic keyboard event on ANOTHER actual handle/currentTarget.
        const first = expected(3, 1, 'w2');
        await assertHistory([first]);
        await expect.poll(order).toEqual([1, 3, 2]); // First command has a nonzero effect.
        await page.mouse.up(); // Native owner release while keyboard command unresolved.
        expect(JSON.parse((await page.locator('body').getAttribute('data-admission-mouse'))!)).toEqual([
          { type: 'pointerdown', trusted: true, pointerType: 'mouse' },
          { type: 'pointerup', trusted: true, pointerType: 'mouse' },
        ]);
        await assertHistory([first]);
        await expect.poll(order).toEqual([1, 3, 2]);
        await expect.poll(() => slots.evaluateAll(nodes => nodes.every(node => !(node as HTMLElement).style.transform))).toBe(true);
        await key(2);
        await assertHistory([first]); // Busy owner retained, no queue/retry.

        await settle(0, ok);
        if (ok) await snapshot([1, 3, 2]);
        else await expect.poll(order).toEqual([1, 2, 3]);
        await snapshot([3, 1, 2]);
        await drag(3);
        await page.mouse.up();
        const second = expected(3, 3);
        await assertHistory([first, second]);
        await expect.poll(order).toEqual([1, 2, 3]);
        await settle(1, true);
        await snapshot([1, 2, 3]);
        await key(3);
        await assertHistory([first, second, first]);
        await expect.poll(order).toEqual([1, 3, 2]);
        await settle(2, false);
        await expect.poll(order).toEqual([1, 2, 3]);
        await page.waitForTimeout(650); // Bounded late-effect observation, not command timeout.
        await assertHistory([first, second, first]);
        expect(await slots.evaluateAll(nodes => nodes.every(node => !(node as HTMLElement).style.transform))).toBe(true);
      } finally {
        await info.attach('command-admission-history', { body: JSON.stringify(await history(), null, 2), contentType: 'application/json' });
      }
    });
  }
}
