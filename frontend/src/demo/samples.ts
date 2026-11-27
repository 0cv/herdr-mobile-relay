import { minuteMs, sampleDeviceIds, type DemoComputer } from './scene';

export function sampleDevices(computer: DemoComputer, epoch: number) {
  const { deviceId, credentialId } = sampleDeviceIds(computer.id);
  const paired_at = new Date(epoch - 90 * minuteMs).toISOString();
  const last_seen_at = new Date(epoch - 2 * minuteMs).toISOString();
  const current = { device_id: deviceId, credential_id: credentialId, role: computer.role, paired_at, last_seen_at, current: true };
  return computer.role === 'controller'
    ? [{ ...current, name: 'Slate Tablet' }, {
      device_id: 'demo-atlas-field-phone', credential_id: 'demo-atlas-field-credential', name: 'Field Phone', role: 'reader',
      paired_at, last_seen_at: new Date(epoch - 32 * minuteMs).toISOString(), current: false,
    }]
    : [{ ...current, name: 'Pocket Reader' }];
}

export function sampleSpeechVoices() {
  return {
    type: 'speech_voices' as const, cache_dir: '', engine_installed: false, languages: [],
    voices: [{ language: 'en', name: 'English — Meadow' }, { language: 'fr', name: 'French — Willow' }]
      .map((voice) => ({ ...voice, installed: false, bytes: 0, engine: '' })),
  };
}

export const sampleQr = {
  size: 21,
  modules: '/lv8FZBukrt05duprsEpB/qv4AAAqkiSDIzL/f/osiNvHABB6/k+cEwjuuiF0pyupSsEOy/rLYA=',
};

export const notesPath = 'docs/release-notes.md';
export const resultsPath = 'src/search/Results.svelte';
export const testsPath = 'tests/search.test.ts';

type Hunk = { context: string; before: string[]; after: string[] };

function sampleChange(path: string, parts: Array<string[] | Hunk>) {
  let oldStart = 1;
  const text: string[] = [];
  const diff = [`diff --git a/${path} b/${path}`, `--- a/${path}`, `+++ b/${path}`];
  for (const part of parts) {
    if (Array.isArray(part)) {
      oldStart += part.length;
      text.push(...part);
      continue;
    }
    const { context, before, after } = part;
    diff.push(`@@ -${oldStart},${before.length + 1} +${text.length + 1},${after.length + 1} @@`,
      ` ${context}`, ...before.map((line) => `-${line}`), ...after.map((line) => `+${line}`));
    oldStart += before.length + 1;
    text.push(context, ...after);
  }
  return { path, diff: diff.join('\n'), text: `${text.join('\n')}\n` };
}

const changes = [
  sampleChange(notesPath, [
    { context: '# Search rollout', before: ['Roll out to everyone.'], after: ['Start with the search preview group.', 'Try it at `/search?preview=1`.'] },
    ['', '## Before widening access', 'Review the preview feedback.', 'Compare results with classic search.', 'Keep both links in the guide.', ''],
    { context: '### Checklist', before: ['- Smoke-test search after launch.'], after: ['- Check keyboard focus and no matches.', '- Verify the classic search fallback.'] },
    ['', '## Support', 'Group feedback by query and browser.', 'Use the daily review to compare notes.', 'Leave the preview link in the guide.', ''],
    { context: '### Fallback', before: ['Pause the release if needed.'], after: ['Turn off the preview flag to return to classic search.'] },
  ]),
  sampleChange(resultsPath, [
    ['<script>', '  export let items = [];', '  export let query = "";', '  let searchBox;'],
    { context: '  function reset() {', before: ['    query = "";'], after: ['    query = "";', '    searchBox?.focus();'] },
    ['  }', '</script>', '', '<label for="search">Search</label>', '<input id="search" bind:this={searchBox} bind:value={query} />', ''],
    { context: '{#if items.length === 0}', before: ['  <p>No results.</p>'], after: ['  <p role="status">No matches. Try a shorter query.</p>', '  <button onclick={reset}>Clear search</button>'] },
    ['{:else}', '  <ul>{#each items as item}<li>{item}</li>{/each}</ul>', '{/if}'],
  ]),
  sampleChange(testsPath, [
    ['import { expect, test } from "vitest";', '', 'const searchUrl = "/search?preview=1";', ''],
    { context: 'test("opens the preview", () => {', before: ['  expect(searchUrl).toBe("/search");'], after: ['  const preview = "/search?preview=1";', '  expect(searchUrl).toBe(preview);'] },
    ['});'],
  ]),
];

const workspaceFiles = new Map([
  ['README.md', '# Search preview\n\nA sample project for trying the new search page. See docs/release-notes.md for the rollout plan.\n'],
  ...changes.map(({ path, text }) => [path, text] as const),
]);

export function sampleWorkspaceFile(path: unknown) {
  if (typeof path !== 'string') return;
  const text = workspaceFiles.get(path);
  if (!text) return;
  return { path, kind: 'text', media_type: path.endsWith('.md') ? 'text/markdown' : 'text/plain', size: text.length, text };
}

export function sampleWorkspaceTree(root: string) {
  return {
    root,
    entries: [
      ...['docs', 'src', 'src/search', 'tests'].map((path) => ({ path, name: path.split('/').at(-1), kind: 'directory' })),
      ...[...workspaceFiles].map(([path, text]) => ({ path, name: path.split('/').at(-1), kind: 'file', size: text.length })),
    ],
  };
}

export function sampleGitStatus(branch: string) {
  return { available: true, branch, files: changes.map(({ path }) => ({ path, status: ' M' })) };
}

export function sampleGitDiff(path: unknown) {
  const change = changes.find((entry) => entry.path === path);
  if (change) return { path: change.path, diff: change.diff };
}
