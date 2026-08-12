import { describe, expect, it } from 'vitest';
import { dailyActivitySummary, formatWorkingDuration } from '$lib/daily-activity';
import { safeMarkdownHtml } from '$lib/markdown';
import { detectTerminalMenu } from '$lib/terminal-menu';
import { linkifyTerminalText, renderTerminalContent } from '$lib/terminal';
import type { Activity, Agent } from '$lib/types';
import { workspaceGroups } from '$lib/workspaces';

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

  it('uses relay activity order when workspace timestamps tie', () => {
    const groups = workspaceGroups([
      agent({ raw_pane_id: 'pane-old', pane_id: 'relay-a::pane-old', project: 'old', activity_seq: 4 }),
      agent({ raw_pane_id: 'pane-new', pane_id: 'relay-a::pane-new', project: 'new', activity_seq: 9 }),
    ]);

    expect(groups.map((group) => group.label)).toEqual(['new', 'old']);
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
