import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { activityForNotification, activityMatchesSearch } from '$lib/activity';
import {
  agentActivitySeq,
  agentNeedsInspection,
  agentStatusGroup,
  agentStatusTone,
  agentUpdatedAt,
  approvalOptions,
  mergeAgentDetails,
  mergeAgentList,
  normalizeAgent,
  sortedAgents,
  tabName,
} from '$lib/agents';
import { APP_PROTOCOL_VERSION } from '$lib/config';
import { quickSetupConfig } from '$lib/config';
import { suggestedLaunchName, validAgentName } from '$lib/launch';
import { parseNotificationTarget, relayProtocolError, relayVersionMeta } from '$lib/protocol';
import { relayPushScope } from '$lib/push';
import { stateFromLocation } from '$lib/router';
import {
  createQuestionDraft,
  questionSubmitAllowed,
  shouldRestoreQuestionDraft,
  updateQuestionOption,
  updateQuestionOther,
} from '$lib/questions';
import {
  ansi256Color,
  ansiToHtml,
  compactRepeatedCharacterRuns,
  isSeparatorOnlyLine,
  latestCompletedResponse,
  renderTerminalContent,
  stripAnsi,
  TERMINAL_REPEATED_RUN_LIMIT,
  TERMINAL_SEPARATOR_TOKEN,
  terminalHtml,
  trimTrailingDecoration,
} from '$lib/terminal';
import { VirtualTerminalIndex } from '$lib/virtual-terminal';
import type { Agent, QuestionInteraction, RelayConnectionView } from '$lib/types';

function agent(overrides: Partial<Agent>): Agent {
  return {
    relay_id: 'relay', relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: 'relay::w1:p1', ...overrides,
  };
}

const TEST_DIRECTORY = dirname(fileURLToPath(import.meta.url));

function attentionFixture(name: string): string {
  return readFileSync(resolve(TEST_DIRECTORY, '..', '..', '..', 'internal', 'question', 'testdata', 'attention', name), 'utf8');
}

describe('protocol and setup parsing', () => {
  it('keeps protocol v2 mutation compatibility explicit', () => {
    expect(APP_PROTOCOL_VERSION).toBe(2);
    expect(relayProtocolError({ protocol: 2 } as RelayConnectionView)).toBe('');
    expect(relayProtocolError({ protocol: 0 } as RelayConnectionView)).toMatch(/Waiting/);
    expect(relayProtocolError({ protocol: 1 } as RelayConnectionView)).toMatch(/v1/);
    expect(relayVersionMeta({ status: 'connected', protocol: 3, version: 'future' } as RelayConnectionView)?.label).toMatch(/App outdated/);
  });

  it('sanitizes setup links and notification routes', () => {
    expect(quickSetupConfig({
      hash: '#setup=0123456789abcdef0123456789abcdef&label=Fedora%20Workstation',
      protocol: 'https:',
      host: 'relay.example.com',
    } as Location)).toEqual({
      label: 'Fedora Workstation', url: 'wss://relay.example.com', token: '0123456789abcdef0123456789abcdef',
    });
    expect(quickSetupConfig({
      hash: '#setup=0123456789abcdef0123456789abcdef&label=Mac&relay=wss%3A%2F%2Frelay-mac.example.com',
      protocol: 'https:',
      host: 'app.example.com',
    } as Location)).toEqual({
      label: 'Mac', url: 'wss://relay-mac.example.com', token: '0123456789abcdef0123456789abcdef',
    });
    expect(quickSetupConfig({ hash: '#setup=short', protocol: 'https:', host: 'relay.example.com' } as Location)).toBeNull();
    expect(quickSetupConfig({ hash: '#setup=0123456789abcdef', protocol: 'javascript:', host: 'bad' } as Location)).toBeNull();
    expect(quickSetupConfig({
      hash: '#setup=0123456789abcdef&relay=javascript%3Aalert(1)',
      protocol: 'https:',
      host: 'app.example.com',
    } as Location)).toBeNull();
    expect(quickSetupConfig({
      hash: '#setup=0123456789abcdef&relay=wss%3A%2F%2Fuser%40relay.example.com',
      protocol: 'https:',
      host: 'app.example.com',
    } as Location)).toBeNull();

    const encoded = encodeURIComponent(JSON.stringify({ pane_id: 'w1:p1', host: 'Fedora', action: 'approve', index: 0, total: 3 }));
    expect(parseNotificationTarget(encoded)).toMatchObject({ pane_id: 'w1:p1', action: 'approve', index: 0, total: 3 });
    expect(parseNotificationTarget(encodeURIComponent(JSON.stringify({ pane_id: 'w1:p1', action: 'approve', index: 9, total: 3 })))).toBeNull();
    expect(parseNotificationTarget('%not-json')).toBeNull();
    expect(stateFromLocation({ hash: '#pane=%invalid' } as Location)).toEqual({ view: 'agents' });
    expect(relayPushScope('UPPER-id-')).toBe('./push/upper-id/');
    expect(relayPushScope('---')).toBe('./push/relay/');
  });
});

describe('terminal rendering', () => {
  it('renders ANSI locally and escapes relay-controlled HTML', () => {
    expect(ansi256Color(6)).toBe('#1abc9c');
    expect(ansi256Color(196)).toBe('rgb(255,0,0)');
    expect(ansiToHtml('\x1b[38;5;6mSearch\x1b[0m')).toMatch(/color:#1abc9c/);
    const html = terminalHtml('<img src=x onerror=alert(1)> \x1b[1mready\x1b[0m');
    expect(html).toContain('&lt;img src=x onerror=alert(1)&gt;');
    expect(html).not.toContain('<img');
    expect(html).toContain('font-weight:700');
    expect(renderTerminalContent('<script>alert(1)</script>', 'plain').html).toBe('&lt;script&gt;alert(1)&lt;/script&gt;');
  });

  it('extracts the latest completed agent response for copy', () => {
    const content = [
      '• Earlier answer.',
      '─ Worked for 2s ─',
      '',
      '• Latest answer.',
      '  Second line.',
      '\x1b[32m  Third line.\x1b[0m',
      '',
      '─ Worked for 2m 19s ─',
      '› Next question',
    ].join('\n');

    expect(latestCompletedResponse(content)).toBe('Latest answer.\nSecond line.\nThird line.');
    const omp = [
      '│ ⟨Wall: 0.08s | Exit: 0⟩ │',
      '╰────────────────────────────────╯',
      '',
      'Updated the canonical plan to:',
      '',
      '- Final plan consistency assertions pass.',
      '',
      '│ ※ recap: Goal: implement the strategy. │',
      '╭── terminal prompt ──╮',
    ].join('\n');
    expect(latestCompletedResponse(omp)).toBe(
      'Updated the canonical plan to:\n\n- Final plan consistency assertions pass.',
    );

    expect(latestCompletedResponse(attentionFixture('omp-copy-button-review.ansi'))).toBe([
      'Implemented and verified the copy-button review fixes.',
      '',
      'Changes:',
      '- Replaced lexical tool-name filtering with structural detection using indented ⎿/└ continuation markers plus explicit system/tool phrases.',
      '- Preserved legitimate prose beginning with words such as “Read”, “Write”, “Ran”, and “Update”.',
      '- Preserved nested • Markdown list items when the response marker is ⏺.',
      '- Prevented indented Codex question prompts from being merged into completed plans.',
      '- Added regression coverage for Claude fixtures, Codex plans, prose collisions, unknown tools, and Markdown lists.',
      '- Added browser assertions for both copy-toast paths and removed test formatting noise.',
      '- Updated README.md and CHANGELOG.md.',
      '',
      'Verification:',
      '- make frontend-check — lint clean, svelte-check: 454 files / 0 errors / 0 warnings, 121 unit tests passed, production build and size validation',
      '  passed.',
      '- make frontend-browser — 44 Chromium tests passed and 44 WebKit tests passed.',
      '- Focused copy browser tests — 3 passed.',
      '- git diff --check — clean.',
      '',
      'Left unrelated untracked review artifacts untouched: COPY_BUTTON_REVIEW.md, COPY_BUTTON_REVIEW_2.md, and WATCHDOG.yml.',
    ].join('\n'));
    expect(latestCompletedResponse('• Still working')).toBe('');
    expect(latestCompletedResponse('※ recap: without a command result')).toBe('');
  });

  it('extracts captured Qoder, Kimi, and OpenCode responses', () => {
    const qoderResponse = [
      'This is Herdr Mobile Relay — a Herdr plugin that lets you control Herdr agents from your phone.',
      '',
      '- What it does: each Linux/macOS machine runs a relay; a phone connects directly (via Cloudflare tunnel) and merges all agents into one installable',
      'web app. You can start/stop/rename agents, send prompts, terminal keys, screenshots, and answer Codex/Claude Code/Qoder approvals, with push',
      'notifications.',
      '- Stack: Go backend (cmd/, internal/, relay/), Svelte frontend (frontend/), Makefile build, contracts and Playwright/unit tests.',
      '- Setup paths: Quick Start (temporary TryCloudflare tunnel + QR) or stable install with your own Cloudflare domain and a user service.',
      '- Current version: 0.13.5.',
      '',
      'There are uncommitted changes touching TerminalView.svelte, terminal.ts, tests, plus untracked review docs (COPY_BUTTON_REVIEW*.md, WATCHDOG.yml) —',
      'looks like in-progress copy-button work.',
    ].join('\n');
    expect(latestCompletedResponse(attentionFixture('qodercli-repo-summary.ansi'))).toBe(qoderResponse);
    expect(latestCompletedResponse(attentionFixture('qodercli-repo-summary-no-second-thinking.ansi'))).toBe(qoderResponse);

    const kimi = [
      '• Earlier answer.',
      'Thought for 2s · 10 tokens',
      '',
      '• Latest Kimi answer.',
      '  Second line.',
      'Thought for 4s · 12 tokens',
      '>',
    ].join('\n');
    expect(latestCompletedResponse(kimi)).toBe('Latest Kimi answer.\nSecond line.');

    const opencode = [
      '     ┃  Previous request',
      '     + Thought: 2.5s',
      '     ▣ Build · model · 2.5s',
      '     ┃  Latest request',
      '     + Thought: 1.4s',
      '     → Read README.md',
      '     + Thought: 3.2s',
      '     Latest OpenCode answer.',
      '     Second line.',
      '     ▣ Build · model · 4.5s',
    ].join('\n');
    expect(latestCompletedResponse(opencode)).toBe('Latest OpenCode answer.\nSecond line.');
    expect(latestCompletedResponse(attentionFixture('opencode-many-questions-confirm.ansi'))).toBe(
      'This directory contains a few .ansi terminal output files and a text file. Would you like me to look\n'
      + "at any of them, or do you have a different project you'd like to explore?",
    );
  });

  it('extracts Claude responses from the captured terminal fixtures', () => {
    const createdResponse = 'Created /workspace/example-user/workspace/tmp/notes.md with a simple markdown skeleton (Overview, Tasks, Links sections).';
    for (const fixture of [
      'claude-multi-select.ansi',
      'claude-multi-select-with-free-text.ansi',
      'claude-multi-select-submit-end.ansi',
    ]) {
      expect(latestCompletedResponse(attentionFixture(fixture))).toBe(createdResponse);
    }

    for (const fixture of ['claude-plan-one-question.ansi', 'claude-plan-one-question-notes.ansi']) {
      expect(latestCompletedResponse(attentionFixture(fixture))).toBe('Renamed to hello_world.txt.');
    }

    for (const fixture of ['claude-plan-multi-question.ansi', 'claude-plan-submit-answers.ansi']) {
      expect(latestCompletedResponse(attentionFixture(fixture)))
        .toBe('What would you like to clarify before I ask questions about the trip?');
    }

    expect(latestCompletedResponse(attentionFixture('claude-single-approval.ansi'))).toBe(
      'I still need a couple details: what should the file be named, where should it live (I\'ll default to the current directory,\n'
      + '/workspace/example-user/workspace/tmp, unless you say otherwise), and what content should it contain?',
    );
    expect(latestCompletedResponse(attentionFixture('claude-single-question.ansi'))).toBe('');
  });

  it('keeps indented Codex question prompts out of the completed plan', () => {
    const response = latestCompletedResponse(attentionFixture('codex-implement-plan.ansi'));
    expect(response).toMatch(/^Proposed Plan\n\n\n# Create a New File/u);
    expect(response).not.toContain('Please confirm exact file spec now');
  });

  it('rejects in-progress, tool-only, and unbounded response markers', () => {
    expect(latestCompletedResponse([
      '• Partial streaming text',
      'Thinking for 8s (esc to interrupt)',
    ].join('\n'))).toBe('');

    expect(latestCompletedResponse([
      '• Here is my answer.',
      '  Detail line.',
      '',
      '• Read(src/app.ts)',
      '  ⎿ Read 40 lines',
      '─ Worked for 30s ─',
    ].join('\n'))).toBe('Here is my answer.\nDetail line.');

    for (const prose of [
      'Ran the full suite; all 120 tests pass.',
      'Explored the repository and found the stale guide.',
      'Read the updated guide before starting.',
      'Write access is required for that directory.',
      'Edit the config before retrying.',
      'Update the cache before rerunning.',
      'Delete the stale lockfile before rerunning.',
      'Create a backup before changing anything.',
    ]) {
      expect(latestCompletedResponse([
        '⏺ Earlier answer.',
        `⏺ ${prose}`,
        '✻ Crunched for 9s',
      ].join('\n'))).toBe(`Earlier answer.\n${prose}`);
    }

    expect(latestCompletedResponse([
      '⏺ Here is my answer.',
      '  Detail line.',
      '⏺ Bash(npm test)',
      '  ⎿ 120 passed',
      '✻ Crunched for 9s',
    ].join('\n'))).toBe('Here is my answer.\nDetail line.');

    expect(latestCompletedResponse([
      '⏺ Next steps:',
      '',
      '• Read the migration guide',
      '• Update the config',
      '✻ Crunched for 9s',
    ].join('\n'))).toBe('Next steps:\n\n• Read the migration guide\n• Update the config');

    expect(latestCompletedResponse([
      '⏺ Answer inside a box.',
      '│   Second line.',
      '✻ Worked for 30s',
    ].join('\n'))).toBe('Answer inside a box.\nSecond line.');

    expect(latestCompletedResponse([
      'old transcript line one',
      'old transcript line two',
      '  ▣ Build · model · 4.5s',
    ].join('\n'))).toBe('');
  });

  it('accepts OpenCode hour-based completion durations', () => {
    for (const duration of ['1h', '1h 2m', '1h 2m 3s']) {
      expect(latestCompletedResponse([
        '     + Thought: 1.4s',
        '     Latest OpenCode answer.',
        `     ▣ Build · model · ${duration}`,
      ].join('\n'))).toBe('Latest OpenCode answer.');
    }
  });

  it('normalizes light-origin ANSI colors onto the dark mobile terminal', () => {
    const blackText = renderTerminalContent('\x1b[38;2;24;24;24mMac text\x1b[0m', 'ansi');
    expect(blackText.html).toContain('color:var(--terminal-text)');

    const darkBlue = renderTerminalContent('\x1b[38;2;20;40;80mBlue text\x1b[0m', 'ansi');
    expect(darkBlue.html).toContain('color:color-mix(in srgb, rgb(20,40,80) 35%, var(--terminal-text))');

    const lightRow = renderTerminalContent('\x1b[48;2;250;250;250;38;2;20;20;20mLight terminal row\x1b[0m', 'ansi');
    expect(lightRow.html).toContain('background-color:rgb(61,64,64)');
    expect(lightRow.html).toContain('color:var(--terminal-text)');

    const brightAccent = renderTerminalContent('\x1b[38;2;95;175;255mAccent\x1b[0m', 'ansi');
    expect(brightAccent.html).toContain('color:rgb(95,175,255)');
  });

  it('limits blank gaps and merges separator fragments across whitespace', () => {
    expect(isSeparatorOnlyLine('----------------')).toBe(true);
    expect(isSeparatorOnlyLine('————————')).toBe(true);
    expect(isSeparatorOnlyLine('________________')).toBe(true);
    expect(isSeparatorOnlyLine('▔'.repeat(120))).toBe(true);
    expect(isSeparatorOnlyLine('▁'.repeat(120))).toBe(true);
    expect(isSeparatorOnlyLine('§'.repeat(120))).toBe(true);
    expect(isSeparatorOnlyLine(`╰${'§'.repeat(120)}╯`)).toBe(true);
    expect(isSeparatorOnlyLine('---')).toBe(false);
    expect(isSeparatorOnlyLine('- meaningful item')).toBe(false);

    const rendered = renderTerminalContent([
      'Before', '', '', '', '', '',
      '----------------', '', '————————', '', '  ________________', '', `  ${'▔'.repeat(120)}`,
      '', '', '', '', 'After',
    ].join('\n'), 'ansi');
    expect(rendered.display).toBe([
      'Before', '', '', TERMINAL_SEPARATOR_TOKEN, '', '', 'After',
    ].join('\n'));
    expect(rendered.html.match(/class="term-separator"/g)).toHaveLength(1);
    expect(rendered.html.match(/class="ansi-line"/g)).toHaveLength(6);
    expect(rendered.rows).toHaveLength(7);
    expect(rendered.rows.filter((row) => row.separator)).toHaveLength(1);
  });

  it('caps arbitrary repeated symbols embedded in terminal output', () => {
    const progress = `${'.'.repeat(120)} [29%]`;
    expect(compactRepeatedCharacterRuns(progress))
      .toBe(`${'.'.repeat(TERMINAL_REPEATED_RUN_LIMIT)} [29%]`);
    expect(compactRepeatedCharacterRuns('a'.repeat(120))).toBe('a'.repeat(120));

    const rendered = renderTerminalContent(progress, 'ansi');
    expect(stripAnsi(rendered.display))
      .toBe(`${'.'.repeat(TERMINAL_REPEATED_RUN_LIMIT)} [29%]`);
  });

  it('preserves fixed-grid rows without transcript reflow or border trimming', () => {
    const border = `╭── omp v17.1.5 ${'─'.repeat(40)}╮`;
    const frame = [
      border,
      '│ Welcome back!                                      │',
      '  prewalk    Switch model                            │',
      '  dump       Copy session                            │',
    ].join('\n');

    const readable = renderTerminalContent(frame, 'ansi');
    const preserved = renderTerminalContent(frame, 'ansi', true);

    expect(stripAnsi(readable.display).split('\n')).toHaveLength(2);
    expect(stripAnsi(preserved.display).split('\n')).toEqual(frame.split('\n'));
    expect(readable.rows).toHaveLength(2);
    expect(preserved.rows).toHaveLength(4);
    expect(preserved.rows.every((row) => row.fixedGrid)).toBe(true);
    expect(readable.html).not.toContain('terminal-cell');
    expect(preserved.html).toContain('<span class="terminal-cell terminal-cell-box terminal-cell-arc terminal-cell-arc-down-right">╭</span>');
    expect(preserved.html).toContain(`class="terminal-cell-horizontal terminal-cell-horizontal-single" style="width:40ch">${'─'.repeat(40)}</span>`);
    expect(preserved.html).toContain('<span class="terminal-cell terminal-cell-box terminal-cell-arc terminal-cell-arc-down-left">╮</span>');
    const mixedBorders = renderTerminalContent('╘╿├┤┴', 'ansi', true);
    expect(stripAnsi(mixedBorders.display)).toBe('╘╿├┤┴');
    expect(mixedBorders.html.match(/terminal-cell-box/g)).toHaveLength(5);
    expect(mixedBorders.html).not.toContain('<span class="terminal-cell">');
  });

  it('marks fixed-grid rows so resized history does not wrap their cells', () => {
    const table = renderTerminalContent([
      `┌${'─'.repeat(80)}┐`,
      `│ ${'Metric'.padEnd(78)}│`,
      `└${'─'.repeat(80)}┘`,
    ].join('\n'), 'ansi', true);
    expect(table.html.match(/terminal-grid-line/g)).toHaveLength(3);
    const plainTable = renderTerminalContent(table.display, 'plain', true);
    expect(plainTable.rows.every((row) => row.fixedGrid)).toBe(true);
    expect(plainTable.rows.filter((row) => row.html.includes('terminal-grid-line')))
      .toHaveLength(3);
    expect(renderTerminalContent('plain terminal output', 'plain', true).rows[0].fixedGrid)
      .toBe(false);
    expect(renderTerminalContent('plain terminal output', 'ansi', true).html)
      .not.toContain('terminal-grid-line');
  });

  it('removes desktop-width decoration after terminal status text', () => {
    const decorated = `\x1b[2m─ Worked for 1m 46s ${'─'.repeat(120)}\x1b[0m`;
    expect(stripAnsi(trimTrailingDecoration(decorated))).toBe('─ Worked for 1m 46s');

    const rendered = renderTerminalContent(decorated, 'ansi');
    expect(stripAnsi(rendered.display)).toBe('─ Worked for 1m 46s');
    expect(rendered.html).toContain('Worked for 1m 46s');
    expect(rendered.html).not.toContain('────────');
  });

  it('keeps agent-specific terminal content in the shared rendering pipeline', () => {
    const frame = [
      '\x1b[1;48;2;61;64;64m› Codex desktop draft\x1b[0m',
      'Opus 4.8 | ctx: 20% | Claude status',
      '~/Development/project (main) | Pi status',
    ].join('\n');

    const readable = stripAnsi(renderTerminalContent(frame, 'ansi').display);
    expect(readable).toContain('Codex desktop draft');
    expect(readable).toContain('Claude status');
    expect(readable).toContain('Pi status');

    const preserved = stripAnsi(renderTerminalContent(frame, 'ansi', true).display);
    expect(preserved).toBe(stripAnsi(frame));
  });
});

describe('agent state and sorting', () => {
  it('maps active agent states to semantic indicator tones', () => {
    expect(agentStatusTone(agent({ status: 'working' }))).toBe('warning');
    expect(agentStatusTone(agent({
      status: 'blocked', attention_kind: 'approval', attention_capable: true,
    }))).toBe('danger');
    expect(agentStatusTone(agent({
      status: 'blocked', attention_kind: 'unknown', attention_capable: true,
    }))).toBe('warning');
    expect(agentStatusGroup(agent({
      status: 'blocked', attention_kind: 'chat', attention_capable: true,
    }))).toBe('ready');
    expect(agentStatusTone(agent({ status: 'done' }))).toBe('success');
    expect(agentStatusTone(agent({ status: 'idle' }))).toBe('muted');
  });

  it('resolves the tab name from the Herdr tab label over the pane name', () => {
    expect(tabName(agent({ name: 'pane-name', tab_label: 'tab-label' }))).toBe('tab-label');
    expect(tabName(agent({ name: 'pane-name' }))).toBe('pane-name');
    expect(tabName(agent({ tab_label: '  spaced  ' }))).toBe('spaced');
    expect(tabName(agent({}))).toBe('');
  });

  it('sorts activity newest-first with stable host fallback', () => {
    expect(agentUpdatedAt(agent({ updated_at: 'invalid' }))).toBe(0);
    expect(agentActivitySeq(agent({ activity_seq: 'invalid' }))).toBe(0);
    const sorted = sortedAgents([
      agent({ pane_id: 'relay::old', raw_pane_id: 'old', updated_at: 1 }),
      agent({ pane_id: 'relay::new', raw_pane_id: 'new', updated_at: 3 }),
    ]);
    expect(sorted.map((item) => item.raw_pane_id)).toEqual(['new', 'old']);
  });

  it('uses the relay activity sequence when a cold snapshot has no timestamps', () => {
    const sorted = sortedAgents([
      agent({ pane_id: 'relay::old', raw_pane_id: 'old', updated_at: 0, activity_seq: 10 }),
      agent({ pane_id: 'relay::current', raw_pane_id: 'current', updated_at: 0, activity_seq: 20 }),
    ]);
    expect(sorted.map((item) => item.raw_pane_id)).toEqual(['current', 'old']);

    const otherRelay = agent({
      relay_id: 'other',
      relay_label: 'Mac',
      pane_id: 'other::pane',
      raw_pane_id: 'pane',
      updated_at: 0,
      activity_seq: 999,
    });
    expect(sortedAgents([otherRelay, ...sorted])[0].relay_id).toBe('relay');
  });

  it('requires two contradictory snapshots before clearing blocked controls', () => {
    const misses = new Map<string, number>();
    const blocked = agent({ status: 'blocked', command: 'touch marker' });
    const working = agent({ status: 'working' });
    const first = mergeAgentList([blocked], 'relay', [working], misses, new Set())[0];
    expect(first.status).toBe('blocked');
    expect(first.command).toBe('touch marker');
    const second = mergeAgentList([first], 'relay', [working], misses, new Set())[0];
    expect(second.status).toBe('working');

    const immediate = mergeAgentList([blocked], 'relay', [working], new Map(), new Set([blocked.pane_id]))[0];
    expect(immediate.status).toBe('working');
  });

  it('clears an old question when a blocked update explicitly becomes an approval', () => {
    const interaction: QuestionInteraction = {
      id: 'old-question', kind: 'single_select', question: 'Old question',
      options: [{ index: 0, label: 'First' }],
    };
    const question = agent({
      status: 'blocked', attention_kind: 'question', attention_capable: true,
      interaction, question_layout: true,
    });
    const approval = agent({
      status: 'blocked', attention_kind: 'approval', attention_capable: true,
      options: ['Approve', 'Reject'], interaction: null, question_layout: false,
    });
    expect(mergeAgentDetails(question, approval)).toMatchObject({
      status: 'blocked', attention_kind: 'approval',
      options: ['Approve', 'Reject'], interaction: null, question_layout: false,
    });

    const sparse = agent({ status: 'blocked', attention_capable: true });
    expect(mergeAgentDetails(question, sparse)).toMatchObject({
      interaction, question_layout: true,
    });
  });

  it('uses terminal-only fallback for relays without attention classification', () => {
    const normalized = normalizeAgent('relay', 'Fedora', {
      pane_id: 'w1:p1',
      status: 'blocked',
      attention_kind: 'approval',
      options: ['Approve once', 'Deny'],
    }, false);
    expect(normalized).toMatchObject({
      attention_kind: 'unknown',
      attention_capable: false,
      question_layout: false,
    });
    expect(normalized.options).toBeUndefined();
    expect(approvalOptions(normalized)).toEqual([]);
    expect(agentStatusGroup(normalized)).toBe('attention');
    expect(agentNeedsInspection(normalized)).toBe(true);
    expect(agentNeedsInspection(agent({ status: 'blocked' }))).toBe(true);
  });

  it('clears a previous session title when the relay sends an empty value', () => {
    const previous = agent({ session: 'old-session' });
    const resumed = agent({ session: '' });
    expect(mergeAgentDetails(previous, resumed).session).toBe('');
  });
});

describe('activity, question drafts, and launch names', () => {
  it('searches every stored activity field including the excerpt and details', () => {
    const activity = {
      summary: 'Approval accepted', kind: 'approval', status: 'confirmed', relay_label: 'Fedora',
      project: 'herdr-mobile-relay', session: 'activity-cards', agent: 'Codex', host: 'fedora',
      pane_id: 'w1:p2', request_id: 'req-9', extract: 'refactor the relay tests',
      details: { choice: 'Approve once' },
    };
    expect(activityMatchesSearch(activity, 'fedora')).toBe(true);
    expect(activityMatchesSearch(activity, 'activity-cards')).toBe(true);
    expect(activityMatchesSearch(activity, 'approve once')).toBe(true);
    expect(activityMatchesSearch(activity, 'refactor the relay')).toBe(true);
    expect(activityMatchesSearch(activity, 'w1:p2')).toBe(true);
    expect(activityMatchesSearch(activity, 'req-9')).toBe(true);
    expect(activityMatchesSearch(activity, 'missing')).toBe(false);
  });

  it('correlates a notification to its stored activity by event id', () => {
    const activities = [
      { activity_key: 'a', details: { event_id: 'evt-1' } },
      { activity_key: 'b', details: { event_id: 'evt-2' } },
    ];
    expect(activityForNotification(activities, 'evt-2')?.activity_key).toBe('b');
    expect(activityForNotification(activities, 'evt-9')).toBeNull();
    expect(activityForNotification(activities, '')).toBeNull();
  });

  it('keeps staged question answers local and validates single choice', () => {
    const interaction: QuestionInteraction = {
      id: 'q1', kind: 'single_select', question: 'Choose scope',
      options: [{ index: 0, label: 'Repository' }, { index: 1, label: 'Module' }],
      other: { label: 'None', allow_empty: true },
    };
    let draft = createQuestionDraft(interaction);
    expect(questionSubmitAllowed(interaction, draft)).toBe(false);
    draft = updateQuestionOption(interaction, draft, 1, true);
    expect([...draft.selected]).toEqual([1]);
    expect(questionSubmitAllowed(interaction, draft)).toBe(true);
    draft = updateQuestionOther(interaction, draft, true, 'Custom');
    expect([...draft.selected]).toEqual([]);
    expect(draft.otherText).toBe('Custom');
  });

  it('prefers a confirmed single choice over an incomplete cached draft', () => {
    const interaction: QuestionInteraction = {
      id: 'confirmed-q1', kind: 'single_select', question: 'Choose reconnect behavior',
      options: [
        { index: 0, label: 'Backoff' },
        { index: 1, label: 'Signals', selected: true },
      ],
      other: { label: 'Other' },
    };
    const incoming = createQuestionDraft(interaction);
    const incomplete = { selected: new Set<number>(), otherSelected: false, otherText: '' };
    const unsent = { selected: new Set([0]), otherSelected: false, otherText: '' };

    expect(shouldRestoreQuestionDraft(interaction, incomplete, incoming)).toBe(false);
    expect(shouldRestoreQuestionDraft(interaction, unsent, incoming)).toBe(true);

    const multi = { ...interaction, kind: 'multi_select' as const };
    expect(shouldRestoreQuestionDraft(multi, incomplete, createQuestionDraft(multi))).toBe(true);
  });

  it('builds bounded portable launch names', () => {
    expect(suggestedLaunchName('/home/me/Development/herdr-mobile-relay', 'codex')).toBe('herdr-mobile-relay-codex');
    expect(suggestedLaunchName('/Users/me/Projects/Málaga App', 'claude')).toBe('malaga-app-claude');
    expect(suggestedLaunchName('/', 'opencode')).toBe('project-opencode');
    expect(suggestedLaunchName(`/home/me/${'project'.repeat(12)}`, 'codex').length).toBeLessThanOrEqual(32);
    expect(suggestedLaunchName('/home/me/123.App', 'codex')).toBe('project-123-app-codex');
    expect(validAgentName('project-codex')).toBe(true);
    expect(validAgentName('Project.codex')).toBe(false);
  });
});

describe('virtual terminal row index', () => {
  it('maps variable row heights to bounded visible ranges', () => {
    const index = new VirtualTerminalIndex();
    index.reset([10, 20, 30, 40]);

    expect(index.total).toBe(100);
    expect(index.offset(2)).toBe(30);
    expect(index.range(30, 20, 0)).toEqual({
      start: 2,
      end: 3,
      top: 30,
      bottom: 40,
      total: 100,
    });
  });

  it('updates following offsets without rebuilding the index', () => {
    const index = new VirtualTerminalIndex();
    index.reset([10, 20, 30, 40]);

    expect(index.update(1, 45)).toBe(25);
    expect(index.total).toBe(125);
    expect(index.offset(2)).toBe(55);
    expect(index.range(55, 30, 5)).toMatchObject({
      start: 1,
      end: 4,
      top: 10,
      bottom: 0,
    });
    expect(index.update(8, 50)).toBe(0);
  });
});
