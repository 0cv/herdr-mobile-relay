import { render, screen, waitFor } from '@testing-library/svelte';
import { get } from 'svelte/store';
import { afterEach, expect, it, vi } from 'vitest';
import TerminalView from '$components/TerminalView.svelte';
import { readTerminalResizeSession, terminalHeightLease, terminalResizeSession } from '$lib/preferences';
import { relayStore } from '$lib/store';
import type { Agent } from '$lib/types';

const agent: Agent = {
  relay_id: 'resize-default', relay_label: 'Synthetic', raw_pane_id: 'pane',
  pane_id: 'resize-default::pane', server_session_id: 'primary', terminal_id: 'terminal',
  generation: 1, agent_session_id: 'session', agent: 'codex', project: 'Synthetic', status: 'working',
};

afterEach(() => {
  vi.restoreAllMocks();
  relayStore.connections.set(new Map());
  terminalResizeSession.set(false);
  terminalHeightLease.set(false);
});

it.each([
  { name: 'unsupported controller', readOnly: false, capabilities: [] },
  { name: 'legacy-only controller', readOnly: false, capabilities: ['pane_size_lease'] },
  { name: 'capable reader', readOnly: true, capabilities: ['pane_size_opt_in', 'pane_size_opt_in_rows'] },
])('keeps the $name terminal usable and quiet with the resize default on', async ({ readOnly, capabilities }) => {
  terminalResizeSession.set(readTerminalResizeSession());
  expect(get(terminalResizeSession)).toBe(true);
  expect(get(terminalHeightLease)).toBe(false);
  relayStore.connections.set(new Map([[agent.relay_id, { status: 'connected', capabilities }]] as never));
  const lease = vi.spyOn(relayStore, 'leasePaneSize');
  const release = vi.spyOn(relayStore, 'releasePaneSize');
  const toast = vi.spyOn(relayStore, 'showToast');
  for (const method of ['readPane', 'watchPane', 'unwatchPane'] as const) vi.spyOn(relayStore, method).mockImplementation(() => {});
  const view = render(TerminalView, {
    agent, allAgents: [agent], responding: new Set<string>(), readOnly,
    frame: { paneId: agent.pane_id, format: 'plain', content: 'Synthetic terminal output' },
  });
  await waitFor(() => expect(screen.getByRole('log')).toHaveTextContent('Synthetic terminal output'));
  expect(lease).not.toHaveBeenCalled();
  expect(release).not.toHaveBeenCalled();
  expect(toast).not.toHaveBeenCalled();
  expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  view.unmount();
});
