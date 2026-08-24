import { afterEach, describe, expect, it, vi } from 'vitest';
import { dailyActivitySummary, formatWorkingDuration } from '$lib/daily-activity';
import { safeMarkdownHtml } from '$lib/markdown';
import { detectTerminalMenu, terminalTextInputActive } from '$lib/terminal-menu';
import { linkifyTerminalText, renderTerminalContent } from '$lib/terminal';
import type { Activity, Agent, RelayWorkspace } from '$lib/types';
import { informativePath, relayWorkspaceTrees, workspaceGroups, workspaceProvenance, workspaceStateTone } from '$lib/workspaces';

function agent(overrides: Partial<Agent>): Agent {
  return {
    relay_id: 'relay-a',
    relay_label: 'Laptop',
    raw_pane_id: 'pane-1',
    pane_id: 'relay-a::pane-1',
    status: 'idle',
    ...overrides,
  };
}

function workspace(overrides: Partial<RelayWorkspace>): RelayWorkspace {
  return {
    relay_id: 'relay-a',
    relay_label: 'Laptop',
    workspace_id: 'work-1',
    number: 1,
    label: 'Mobile Relay',
    focused: false,
    pane_count: 1,
    tab_count: 1,
    active_tab_id: 'tab-1',
    agent_status: 'idle',
    cwd: '/home/user/mobile',
    worktree: null,
    ...overrides,
  };
}

function activity(overrides: Partial<Activity>): Activity {
  return {
    timestamp: 0,
    relay_id: 'relay-a',
    relay_label: 'Laptop',
    activity_key: 'activity',
    pane_id: 'pane-1',
    ...overrides,
  };
}

describe('workspace navigation', () => {
  it('groups panes by relay and workspace, then preserves tab hierarchy', () => {
    const groups = workspaceGroups([
      agent({ pane_id: 'relay-a::pane-2', raw_pane_id: 'pane-2', workspace_id: 'work-1', tab_id: 'tab-2', tab_number: 2, tab_label: 'Tests', status: 'working', project: 'mobile' }),
      agent({ workspace_id: 'work-1', tab_id: 'tab-1', tab_number: 1, tab_label: 'Code', project: 'mobile' }),
      agent({ relay_id: 'relay-b', relay_label: 'Desktop', pane_id: 'relay-b::pane-1', workspace_id: 'work-1', project: 'server' }),
    ]);

    expect(groups).toHaveLength(2);
    expect(groups.find((group) => group.relayId === 'relay-a')).toMatchObject({
      label: 'mobile',
      workingCount: 1,
      tabs: [{ label: 'Code' }, { label: 'Tests' }],
    });
  });

  it('keeps authoritative labels and empty shell-only workspaces', () => {
    const groups = workspaceGroups([
      agent({ workspace_id: 'work-1', tab_id: 'tab-1', project: 'fallback' }),
    ], [
      workspace({}),
      workspace({
        workspace_id: 'work-2',
        number: 2,
        label: 'Shell Only',
        cwd: '/home/user/shell',
        pane_count: 1,
        tab_count: 1,
      }),
    ]);

    expect(groups).toHaveLength(2);
    expect(groups.find((group) => group.workspaceId === 'work-1')).toMatchObject({
      label: 'Mobile Relay',
      cwd: '/home/user/mobile',
    });
    expect(groups.find((group) => group.workspaceId === 'work-2')).toMatchObject({
      label: 'Shell Only',
      agents: [],
      tabCount: 1,
      paneCount: 1,
    });
  });

  it('nests linked worktrees under their repository workspace', () => {
    const repo = {
      repo_key: 'repo-key',
      repo_name: 'mobile',
      repo_root: '/home/user/mobile',
    };
    const trees = relayWorkspaceTrees([
      workspace({
        worktree: {
          ...repo,
          checkout_path: '/home/user/mobile',
          is_linked_worktree: false,
        },
      }),
      workspace({
        workspace_id: 'work-2',
        number: 2,
        label: 'fix/one',
        cwd: '/home/user/worktrees/fix-one',
        worktree: {
          ...repo,
          checkout_path: '/home/user/worktrees/fix-one',
          is_linked_worktree: true,
        },
      }),
      workspace({ workspace_id: 'work-3', number: 3, label: 'Other' }),
    ]);

    expect(trees).toHaveLength(2);
    expect(trees[0]).toMatchObject({
      workspace: { workspace_id: 'work-1' },
      children: [{ workspace_id: 'work-2', label: 'fix/one' }],
      workspaceIds: ['work-1', 'work-2'],
    });
    expect(trees[1].workspace.workspace_id).toBe('work-3');
  });

  it('shows workspace provenance and worktree paths only when informative', () => {
    const repo = { repo_key: 'repo-key', repo_name: 'ibkr', repo_root: '/home/user/ibkr' };
    const parent = { label: 'ibkr', worktree: { ...repo, checkout_path: '/home/user/ibkr', is_linked_worktree: false } };
    const renamed = { ...parent, label: 'Trading' };
    const linked = { label: 'fix/one', worktree: { ...repo, checkout_path: '/home/user/worktrees/fix-one', is_linked_worktree: true } };

    // "Repository · ibkr" inside a card titled "ibkr" repeats the title.
    expect(workspaceProvenance(parent)).toBe('');
    expect(workspaceProvenance(renamed)).toBe('Repository · ibkr');
    // Nested under the parent card the tree already says it is a worktree;
    // orphaned at top level the repository name is the only provenance left.
    expect(workspaceProvenance(linked, true)).toBe('');
    expect(workspaceProvenance(linked)).toBe('Worktree of ibkr');
    expect(workspaceProvenance({ label: 'Shell Only', worktree: null })).toBe('');

    // A checkout named after its label carries no extra information.
    expect(informativePath('/home/user/worktrees/fix-one', 'fix-one')).toBe('');
    expect(informativePath('/home/user/worktrees/fix-one', 'fix/one')).toBe('/home/user/worktrees/fix-one');
    expect(informativePath('', 'fix-one')).toBe('');
  });

  it('uses relay activity order when workspace timestamps tie', () => {
    const groups = workspaceGroups([
      agent({ raw_pane_id: 'pane-old', pane_id: 'relay-a::pane-old', project: 'old', activity_seq: 4 }),
      agent({ raw_pane_id: 'pane-new', pane_id: 'relay-a::pane-new', project: 'new', activity_seq: 9 }),
    ]);

    expect(groups.map((group) => group.label)).toEqual(['new', 'old']);
  });

  it('counts done sessions apart from idle and ranks the workspace state dot', () => {
    const [group] = workspaceGroups([
      agent({ workspace_id: 'work-1', status: 'done', project: 'mobile' }),
      agent({ pane_id: 'relay-a::pane-2', raw_pane_id: 'pane-2', workspace_id: 'work-1', status: 'working' }),
      agent({ pane_id: 'relay-a::pane-3', raw_pane_id: 'pane-3', workspace_id: 'work-1', status: 'idle' }),
    ]);
    expect(group).toMatchObject({ doneCount: 1, workingCount: 1, readyCount: 1 });
    // done > working > idle
    expect(workspaceStateTone(group)).toBe('success');
    expect(workspaceStateTone({ ...group, doneCount: 0 })).toBe('warning');
    expect(workspaceStateTone({ ...group, doneCount: 0, workingCount: 0 })).toBe('muted');
  });
});

describe('safe rich output', () => {
  it('turns explicit terminal URLs into isolated external links', () => {
    const html = linkifyTerminalText('Docs: https://example.com/a?q=1). Local: http://[::1]:8375/path');
    expect(html).toContain('href="https://example.com/a?q=1"');
    expect(html).toContain('</a>).');
    expect(html).toContain('href="http://[::1]:8375/path"');
    expect(html).toContain('rel="noopener noreferrer"');
    expect(html).toContain('referrerpolicy="no-referrer"');
    expect(renderTerminalContent('\x1b[32mhttps://example.com/build\x1b[0m', 'ansi').html)
      .toContain('class="terminal-link"');
  });

  it('renders a bounded Markdown subset without trusting message HTML or schemes', () => {
    const html = safeMarkdownHtml('# Result\n\n**Done**: [report](https://example.com/report)\n\n<script>alert(1)</script>\n\n[x](javascript:alert(1))');
    expect(html).toContain('<h3>Result</h3>');
    expect(html).toContain('<strong>Done</strong>');
    expect(html).toContain('href="https://example.com/report"');
    expect(html).toContain('&lt;script&gt;alert(1)&lt;/script&gt;');
    expect(html).not.toContain('<script>');
    expect(html).not.toContain('href="javascript:');
  });

  it('renders a GFM table with per-column alignment inside a scroll container', () => {
    const html = safeMarkdownHtml([
      'Comparison:',
      '',
      '| Model | Speed | Cost |',
      '| --- | :---: | ---: |',
      '| `fast` | high | $1 |',
      '| slow | low | $2 |',
      '',
      'After.',
    ].join('\n'));

    expect(html).toContain('<div class="conversation-table"><table><thead><tr>');
    expect(html).toContain('<th>Model</th>');
    expect(html).toContain('<th style="text-align:center">Speed</th>');
    expect(html).toContain('<th style="text-align:right">Cost</th>');
    expect(html).toContain('<tbody><tr><td><code>fast</code></td>');
    expect(html).toContain('<td style="text-align:right">$2</td></tr></tbody></table></div>');
    expect(html).toContain('<p>After.</p>');
  });

  it('accepts bare and left-aligned delimiter rows without outer pipes', () => {
    const html = safeMarkdownHtml('Name | Value\n:-- | -\nalpha | 1');

    expect(html).toContain('<th style="text-align:left">Name</th><th>Value</th>');
    expect(html).toContain('<tbody><tr><td style="text-align:left">alpha</td><td>1</td></tr></tbody>');
  });

  it('keeps escaped pipes literal and still escapes table cell content', () => {
    const html = safeMarkdownHtml('| a \\| b | c |\n|---|---|\n| <img src=x> | y \\| z |');

    expect(html).toContain('<th>a | b</th>');
    expect(html).toContain('<td>&lt;img src=x&gt;</td>');
    expect(html).toContain('<td>y | z</td>');
    expect(html).not.toContain('<img');
  });

  it('pads ragged rows and drops cells past the header width', () => {
    const html = safeMarkdownHtml('| a | b |\n|---|---|\n| 1 |\n| 1 | 2 | 3 |');

    expect(html).toContain('<tr><td>1</td><td></td></tr>');
    expect(html).toContain('<tr><td>1</td><td>2</td></tr>');
    expect(html).not.toContain('<td>3</td>');
  });

  it('leaves delimiter-looking lines alone when no header row precedes them', () => {
    expect(safeMarkdownHtml('|---|')).toBe('<p>|---|</p>');
    expect(safeMarkdownHtml('| a | b |\n| --- |\n| 1 | 2 |')).not.toContain('<table>');
    expect(safeMarkdownHtml('---')).toBe('<hr>');
  });
});

describe('rendering without Intl.Segmenter', () => {
  afterEach(() => {
    vi.resetModules();
  });

  it('measures code points when the WebView ships no grapheme segmenter', async () => {
    const intl = Intl as unknown as { Segmenter?: typeof Intl.Segmenter };
    const segmenter = intl.Segmenter;
    const grid = '┌───┬──┐\n│ ab │漢 │';
    const expected = renderTerminalContent(grid, 'ansi', true).rows;
    delete intl.Segmenter;
    try {
      vi.resetModules();
      // Static import cannot work: the module must be evaluated again with
      // Intl.Segmenter absent to exercise the feature-detected fallback.
      const terminal = await import('$lib/terminal');
      const rows = terminal.renderTerminalContent(grid, 'ansi', true).rows;

      expect(rows.map((row) => [row.text, row.columns, row.fixedGrid]))
        .toEqual(expected.map((row) => [row.text, row.columns, row.fixedGrid]));
      expect(rows.map((row) => row.html)).toEqual(expected.map((row) => row.html));
      expect(terminal.renderTerminalContent('ab漢字 👍', 'text').rows[0].columns).toBe(9);
    } finally {
      intl.Segmenter = segmenter;
    }
  });
});

describe('terminal key-hint fallback', () => {
  it('derives only explicitly named keys and actions from the terminal footer', () => {
    const menu = detectTerminalMenu([
      'Choose a model',
      'Current: balanced',
      '↑/↓ to navigate · Enter to select · Esc to cancel',
    ].join('\n'));

    expect(menu?.title).toBe('Current: balanced');
    expect(menu?.actions).toEqual([
      { label: 'Up', keys: ['Up'], cancel: false },
      { label: 'Down', keys: ['Down'], cancel: false },
      { label: 'Select', keys: ['Enter'], cancel: false },
      { label: 'Cancel', keys: ['Escape'], cancel: true },
    ]);
    expect(detectTerminalMenu('Press Enter when the build is done.')).toBeNull();
  });

  it('places previous and next around vertical plan navigation', () => {
    const menu = detectTerminalMenu([
      'Ask',
      'Other (type your own)',
      'Enter select · n note · ↑/↓ move · Tab/←/→ · Esc cancel',
    ].join('\n'));

    expect(menu?.actions.map((action) => action.label)).toEqual([
      'Previous', 'Up', 'Down', 'Next', 'Select', 'Cancel',
    ]);
  });

  it('enables terminal text only while an editor is asking for submission', () => {
    expect(terminalTextInputActive([
      'Custom answer: Which weekend?',
      '>',
      'enter or ctrl+q submit  esc cancel  ctrl+g external editor',
    ].join('\n'))).toBe(true);
    expect(terminalTextInputActive([
      'Other (type your own)',
      'Enter select · n note · ↑/↓ move · Tab/←/→ · Esc cancel',
    ].join('\n'))).toBe(false);
  });
});

describe('daily activity summary', () => {
  it('measures observed working intervals and counts retained outcomes', () => {
    const now = Date.UTC(2026, 7, 12, 12);
    const current = agent({ project: 'mobile', status: 'idle' });
    const summary = dailyActivitySummary([
      activity({ activity_key: '1', kind: 'working', timestamp: now - 120 * 60_000 }),
      activity({ activity_key: '2', kind: 'blocked', timestamp: now - 90 * 60_000 }),
      activity({ activity_key: '3', kind: 'working', timestamp: now - 60 * 60_000 }),
      activity({ activity_key: '4', kind: 'finished', timestamp: now - 10 * 60_000 }),
      activity({ activity_key: '5', kind: 'prompt', timestamp: now - 5 * 60_000 }),
    ], [current], now);

    expect(summary).toMatchObject({
      workingMs: 80 * 60_000,
      attention: 1,
      completions: 1,
      actions: 1,
      relays: 1,
    });
    expect(summary.agents[0]).toMatchObject({ label: 'mobile', workingMs: 80 * 60_000 });
    expect(formatWorkingDuration(summary.workingMs)).toBe('1h 20m');
  });

  it('bounds an already-running agent to the 24-hour summary window', () => {
    const now = Date.UTC(2026, 7, 12, 12);
    const summary = dailyActivitySummary([
      activity({ kind: 'working', timestamp: now - 26 * 60 * 60_000 }),
    ], [agent({ status: 'working' })], now);

    expect(summary.workingMs).toBe(24 * 60 * 60_000);
    expect(summary.relays).toBe(1);
  });
});
