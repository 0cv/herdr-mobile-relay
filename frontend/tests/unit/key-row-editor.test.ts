import { render, screen, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';
import { get } from 'svelte/store';
import { afterEach, expect, it, vi } from 'vitest';
import ManageDialog from '$components/ManageDialog.svelte';
import { setTerminalKeyRow, terminalKeyRow } from '$lib/preferences';
import { relayStore } from '$lib/store';
import { defaultKeyRow } from '$lib/terminal-key-row';
import type { Agent } from '$lib/types';

const agent: Agent = {
  relay_id: 'fedora', relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: 'fedora::w1:p1',
  server_session_id: 'primary', terminal_id: 'terminal', generation: 1,
  project: 'relay', agent: 'claude', status: 'working',
};

const key = (name: string) => screen.getByRole('button', { name });

afterEach(() => {
  vi.restoreAllMocks();
  setTerminalKeyRow(defaultKeyRow);
});

it('lets a reader add, move, remove, reset, cancel and save without calling the relay', async () => {
  setTerminalKeyRow(['Tab', 'Up']);
  const sendToAgent = vi.spyOn(relayStore, 'sendToAgent');
  const sendCommand = vi.spyOn(relayStore, 'sendCommand');
  const user = userEvent.setup();
  render(ManageDialog, { open: true, readOnly: true, agent });
  const dialog = screen.getByRole('dialog', { name: 'Manage Agent' });
  await user.click(key('Edit key row'));
  const picker = await within(dialog).findByRole('combobox', { name: 'Add a key' });
  expect(picker).toHaveFocus();
  expect(within(dialog).getByText('Applies to terminals on this device.')).toBeVisible();
  expect(key('Move earlier: Tab')).toBeDisabled();
  expect(key('Move later: Up')).toBeDisabled();
  expect(within(picker).queryByRole('option', { name: 'Up' })).not.toBeInTheDocument();

  await user.selectOptions(picker, 'Ctrl+letter');
  const letters = screen.getByRole('combobox', { name: 'Letter' });
  expect(within(letters).queryByRole('option', { name: 'I' })).not.toBeInTheDocument();
  expect(within(letters).queryByRole('option', { name: 'M' })).not.toBeInTheDocument();
  await user.selectOptions(letters, 'C');
  await user.click(key('Add'));
  await user.click(key('Move earlier: Ctrl+C'));
  expect(key('Move earlier: Ctrl+C')).toHaveFocus();
  expect(screen.getByRole('status')).toHaveTextContent('Ctrl+C moved to position 2 of 3.');
  await user.click(key('Remove Tab'));
  expect(key('Remove Ctrl+C')).toHaveFocus();
  await user.selectOptions(picker, 'Ctrl+letter');
  expect(within(screen.getByRole('combobox', { name: 'Letter' })).queryByRole('option', { name: 'C' })).not.toBeInTheDocument();
  expect(get(terminalKeyRow)).toEqual(['Tab', 'Up']);

  await user.click(key('Save'));
  expect(key('Edit key row')).toHaveFocus();
  expect(get(terminalKeyRow)).toEqual(['ctrl+c', 'Up']);
  await user.click(key('Edit key row'));
  await screen.findByRole('button', { name: 'Reset to default' });
  await user.click(key('Reset to default'));
  expect(screen.getAllByRole('listitem')).toHaveLength(defaultKeyRow.length);
  await user.click(key('Cancel'));
  expect(key('Edit key row')).toHaveFocus();
  expect(get(terminalKeyRow)).toEqual(['ctrl+c', 'Up']);
  expect(sendToAgent).not.toHaveBeenCalled();
  expect(sendCommand).not.toHaveBeenCalled();
});

it('retains the draft and applied row after a failed save', async () => {
  setTerminalKeyRow(['Tab']);
  const user = userEvent.setup();
  render(ManageDialog, { open: true, agent });
  await user.click(key('Edit key row'));
  await user.click(await screen.findByRole('button', { name: 'Remove Tab' }));
  expect(screen.getByRole('combobox', { name: 'Add a key' })).toHaveFocus();

  vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('blocked'); });
  await user.click(key('Save'));
  expect(screen.getByRole('alert')).toHaveTextContent('Could not save the key row');
  expect(screen.queryByRole('listitem')).not.toBeInTheDocument();
  expect(get(terminalKeyRow)).toEqual(['Tab']);

  vi.mocked(localStorage.setItem).mockRestore();
  await user.click(key('Save'));
  expect(get(terminalKeyRow)).toEqual([]);
});
