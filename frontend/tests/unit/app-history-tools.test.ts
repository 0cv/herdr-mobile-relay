import { fireEvent, render, screen } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, expect, it, vi } from 'vitest';
import App from '../../src/App.svelte';
import { currentView } from '$lib/router';
import { targetRefForAgent } from '$lib/resource-id';
import { securityState } from '$lib/security';
import { relayStore } from '$lib/store';
import type { Agent } from '$lib/types';

vi.mock('$lib/router', async (original) => ({
  ...await original<typeof import('$lib/router')>(),
  initializeRouter: () => () => {},
  followInitialAgentSession: () => () => {},
}));
vi.mock('$lib/security', async (original) => ({
  ...await original<typeof import('$lib/security')>(), initializeDeviceSecurity: () => () => {},
}));
vi.mock('$lib/speech', async (original) => ({
  ...await original<typeof import('$lib/speech')>(), initializeSpeech: () => () => {},
}));
vi.mock('$lib/push', async (original) => ({
  ...await original<typeof import('$lib/push')>(), initializePush: () => {},
}));
vi.mock('$lib/updates', async (original) => ({
  ...await original<typeof import('$lib/updates')>(), initializeAppUpdates: () => () => {},
}));

it.each(['Inspect workspace', 'Manage agent'] as const)('opens %s from history and invalidates it on route change or lock', async (label) => {
  const agent: Agent = {
    relay_id: 'tools', relay_label: 'Tools', raw_pane_id: 'pane', pane_id: 'tools::pane',
    server_session_id: 'primary', terminal_id: 'terminal', generation: 1, agent: 'codex', cwd: '/workspace',
  };
  const target = targetRefForAgent(agent)!;
  currentView.set({ view: 'history', paneId: agent.pane_id, target });
  relayStore.agents.set([agent]);
  relayStore.connections.set(new Map([[agent.relay_id, {
    status: 'connected', inventory: { state: 'ready' }, capabilities: ['workspace_inspection'],
    update: { state: 'idle' }, appDeploy: { state: 'idle' },
  } as never]]));
  const showModal = vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (this: HTMLDialogElement) { this.open = true; });
  const view = render(App);
  try {
    await fireEvent.click(screen.getByRole('button', { name: label }));
    await tick();
    expect(screen.getByRole('dialog', { name: label })).toBeInTheDocument();
    currentView.set({ view: 'agents' });
    await tick();
    expect(screen.queryByRole('dialog', { name: label })).not.toBeInTheDocument();
    currentView.set({ view: 'history', paneId: agent.pane_id, target });
    await tick();
    await fireEvent.click(screen.getByRole('button', { name: label }));
    expect(screen.getByRole('dialog', { name: label })).toBeInTheDocument();
    securityState.update((state) => ({ ...state, locked: true }));
    await tick();
    expect(screen.queryByRole('dialog', { name: label })).not.toBeInTheDocument();
    expect(showModal).toHaveBeenCalled();
  } finally {
    view.unmount();
  }
});

afterEach(() => {
  relayStore.destroy();
  relayStore.agents.set([]);
  relayStore.connections.set(new Map());
  currentView.set({ view: 'agents' });
  securityState.update((state) => ({ ...state, locked: false }));
  vi.restoreAllMocks();
});
