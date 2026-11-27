import { expect, it } from 'vitest';
import { matchSlashCommands } from '$lib/composer';
import type { SlashCommand } from '$lib/types';

const catalogue: SlashCommand[] = ['/help', '/model', '/resume', '/review', '/status']
  .map((command) => ({ command, description: command, source: 'builtin' }));
const recent = ['/missing', '/review', '/resume', '/model'];

it('puts recent catalogue commands first and keeps catalogue order for the rest', () => {
  expect(matchSlashCommands(catalogue, '', recent).map((entry) => entry.command))
    .toEqual(['/review', '/resume', '/model', '/help', '/status']);
  expect(matchSlashCommands(catalogue, '')).toEqual(catalogue);
  expect(catalogue.map((entry) => entry.command)).toEqual(['/help', '/model', '/resume', '/review', '/status']);
});

it('only orders commands that match the query and are in the catalogue', () => {
  expect(matchSlashCommands(catalogue, 're', recent).map((entry) => entry.command))
    .toEqual(['/review', '/resume']);
  expect(matchSlashCommands(catalogue, 'miss', recent)).toEqual([]);
  expect(matchSlashCommands(catalogue, null, recent)).toEqual([]);
});
