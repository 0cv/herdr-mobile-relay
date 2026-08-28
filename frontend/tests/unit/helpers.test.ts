import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { activityForNotification, activityMatchesSearch } from '$lib/activity';
import {
  agentActivitySeq,
  agentLastActiveAt,
  agentNeedsInspection,
  agentStatusGroup,
  agentStatusTone,
  agentUpdatedAt,
  approvalOptions,
  mergeAgentDetails,
  mergeAgentList,
  normalizeAgent,
  sortedAgents,
  sessionName,
  tabName,
} from '$lib/agents';
import { APP_PROTOCOL_VERSION } from '$lib/config';
import { PANE_LEASE_HIDDEN_GRACE_MS, paneLeaseRenewalAllowed, quickSetupConfig } from '$lib/config';
import { suggestedLaunchName, validAgentName } from '$lib/launch';
import { parseNotificationTarget, relayProtocolError, relayVersionMeta, shortRevision } from '$lib/protocol';
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
  renderedRowShift,
  setTerminalScheme,
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

  it('shortens git revisions for a phone row without losing the full hash', () => {
    const revision = 'e3816cb50d9fcc4220558c278be8d31d6d7a12d0';
    const meta = relayVersionMeta({
      status: 'connected', protocol: 2, version: revision, releaseVersion: '0.17.0', revision,
    } as RelayConnectionView);
    expect(meta?.label).toBe('v0.17.0 · e3816cb');
    // The full hash stays reachable: the row is for scanning, not for copying.
    expect(meta?.title).toContain(revision);

    expect(shortRevision(`${revision}-dirty`)).toBe('e3816cb-dirty');
    // Anything that is not a hash is left intact rather than truncated blindly.
    expect(shortRevision('0.17.0')).toBe('0.17.0');
    expect(shortRevision('unknown')).toBe('unknown');
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
    expect(quickSetupConfig({
      hash: '#setup=0123456789abcdef0123456789abcdef&label=Fedora&gateways=wss%3A%2F%2Fgw.example.com',
      protocol: 'https:',
      host: 'app.example.com',
    } as Location)).toEqual({
      label: 'Fedora',
      url: '',
      token: '0123456789abcdef0123456789abcdef',
      transport: 'hybrid',
      gatewayUrl: 'wss://gw.example.com',
      gatewayUrls: ['wss://gw.example.com'],
    });
    expect(quickSetupConfig({
      hash: '#setup=0123456789abcdef0123456789abcdef&gateways=wss%3A%2F%2Fgw.example.com%2Fconnect%3Fkey%3Dleak',
      protocol: 'https:',
      host: 'app.example.com',
    } as Location)).toBeNull();

    const encoded = encodeURIComponent(JSON.stringify({ pane_id: 'w1:p1', host: 'Fedora', action: 'approve', index: 0, total: 3 }));
    expect(parseNotificationTarget(encoded)).toMatchObject({ pane_id: 'w1:p1', action: 'approve', index: 0, total: 3 });
    expect(parseNotificationTarget(encodeURIComponent(JSON.stringify({ pane_id: 'w1:p1', action: 'approve', index: 9, total: 3 })))).toBeNull();
    expect(parseNotificationTarget('%not-json')).toBeNull();
    expect(stateFromLocation({ hash: '#pane=%invalid' } as Location)).toEqual({ view: 'agents' });
    expect(stateFromLocation({ hash: '#history=relay%3A%3Apane-1' } as Location)).toEqual({
      view: 'history',
      paneId: 'relay::pane-1',
    });
    expect(relayPushScope('UPPER-id-')).toBe('./push/upper-id/');
    expect(relayPushScope('---')).toBe('./push/relay/');
  });
});

describe('pane lease hidden grace', () => {
  it('renews while visible, within the hidden grace, and never past it', () => {
    // Desktop Safari reports occluded windows as hidden; renewals must keep
    // the lease alive across app switches but still lapse for a page that
    // stays hidden, so the desktop gets its pane size back.
    expect(paneLeaseRenewalAllowed(true, 0, 1_000_000)).toBe(true);
    const hiddenAt = 1_000_000;
    expect(paneLeaseRenewalAllowed(false, hiddenAt, hiddenAt + 1)).toBe(true);
    expect(paneLeaseRenewalAllowed(false, hiddenAt, hiddenAt + PANE_LEASE_HIDDEN_GRACE_MS - 1)).toBe(true);
    expect(paneLeaseRenewalAllowed(false, hiddenAt, hiddenAt + PANE_LEASE_HIDDEN_GRACE_MS)).toBe(false);
    // A page that mounts hidden never held the pane and must not take it.
    expect(paneLeaseRenewalAllowed(false, 0, 1_000_000)).toBe(false);
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

  it('extracts the latest OpenCode response for copy', () => {
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

  it('normalizes dark-origin ANSI colors onto a light mobile terminal', () => {
    setTerminalScheme('light');
    try {
      const whiteText = renderTerminalContent('\x1b[38;2;229;229;229mMac text\x1b[0m', 'ansi');
      expect(whiteText.html).toContain('color:var(--terminal-text)');

      const paleYellow = renderTerminalContent('\x1b[38;2;255;255;128mPale text\x1b[0m', 'ansi');
      expect(paleYellow.html).toContain('color:color-mix(in srgb, rgb(255,255,128) 35%, var(--terminal-text))');

      const darkRow = renderTerminalContent('\x1b[48;2;8;8;8;38;2;229;229;229mDark terminal row\x1b[0m', 'ansi');
      expect(darkRow.html).toContain('background-color:rgb(204,208,218)');
      expect(darkRow.html).toContain('color:var(--terminal-text)');

      const lightRow = renderTerminalContent('\x1b[48;2;250;250;250;38;2;20;20;20mLight terminal row\x1b[0m', 'ansi');
      expect(lightRow.html).toContain('background-color:rgb(250,250,250)');
      expect(lightRow.html).toContain('color:rgb(20,20,20)');

      const basicRed = renderTerminalContent('\x1b[31mError\x1b[0m', 'ansi');
      expect(basicRed.html).toContain('color:#d20f39');
    } finally {
      setTerminalScheme('dark');
    }
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
    expect(preserved.html).toContain(`class="terminal-cell-horizontal terminal-cell-horizontal-single" style="width:calc(40 * var(--terminal-cell-width, 1ch))">${'─'.repeat(40)}</span>`);
    expect(preserved.html).toContain('<span class="terminal-cell terminal-cell-box terminal-cell-arc terminal-cell-arc-down-left">╮</span>');
    const mixedBorders = renderTerminalContent('╘╿├┤┴', 'ansi', true);
    expect(stripAnsi(mixedBorders.display)).toBe('╘╿├┤┴');
    expect(mixedBorders.html.match(/terminal-cell-box/g)).toHaveLength(5);
    expect(mixedBorders.html).not.toContain('<span class="terminal-cell">');
  });

  it('keeps current-width grids aligned and wraps stale wider grids in Resize Session', () => {
    const desktopTable = [
      `┌${'─'.repeat(80)}┐`,
      `│ ${'Metric'.padEnd(78)}│`,
      `└${'─'.repeat(80)}┘`,
    ].join('\n');
    const preserved = renderTerminalContent(desktopTable, 'ansi', true);
    expect(preserved.rows.every((row) => row.fixedGrid)).toBe(true);

    const resizedHistory = renderTerminalContent(desktopTable, 'ansi', true, false, 40);
    expect(resizedHistory.rows.every((row) => row.fixedGrid)).toBe(false);
    expect(resizedHistory.html).not.toContain('terminal-grid-line');
    expect(resizedHistory.html).not.toContain('terminal-cell-box');
    expect(resizedHistory.rows.filter((row) => row.separator)).toHaveLength(2);
    expect(Math.max(...resizedHistory.rows.map((row) => row.columns))).toBeLessThanOrEqual(40);

    const currentTable = renderTerminalContent([
      `┌${'─'.repeat(38)}┐`,
      `│ ${'Metric'.padEnd(36)}│`,
      `└${'─'.repeat(38)}┘`,
    ].join('\n'), 'ansi', true, false, 40);
    expect(currentTable.rows.every((row) => row.fixedGrid)).toBe(true);
    expect(currentTable.html.match(/terminal-grid-line/g)).toHaveLength(3);
  });

  it('collapses stale-width junctioned table borders to rules instead of wrapping them', () => {
    const table = [
      `┌${'─'.repeat(30)}┬${'─'.repeat(30)}┐`,
      `│ ${'Period'.padEnd(29)}│ ${'Trades'.padEnd(29)}│`,
      `├${'─'.repeat(30)}┼${'─'.repeat(30)}┤`,
      `│ ${'Combined'.padEnd(27)}│ ${'156'.padEnd(32)}│`,
      `└${'─'.repeat(30)}┴${'─'.repeat(30)}┘`,
    ].join('\n');
    const resized = renderTerminalContent(table, 'ansi', true, false, 40);
    expect(resized.rows.filter((row) => row.separator)).toHaveLength(3);
    expect(resized.display).not.toContain('┬');
    expect(resized.display).not.toContain('┼');
    expect(resized.display).toContain('Period');
    expect(resized.display).toContain('Combined');

    // A current-width junctioned table keeps its exact grid.
    const narrow = renderTerminalContent([
      `┌${'─'.repeat(17)}┬${'─'.repeat(17)}┐`,
      `│ ${'x'.padEnd(16)}│ ${'y'.padEnd(16)}│`,
      `└${'─'.repeat(17)}┴${'─'.repeat(17)}┘`,
    ].join('\n'), 'ansi', true, false, 40);
    expect(narrow.rows.every((row) => row.fixedGrid)).toBe(true);

    // A wide row of empty cells is content (cell walls), never a border rule.
    const emptyCells = renderTerminalContent(
      `│${' '.repeat(30)}│${' '.repeat(30)}│`,
      'ansi',
      true,
      false,
      40,
    );
    expect(emptyCells.rows.filter((row) => row.separator)).toHaveLength(0);
  });

  it('keeps a bare horizontal rule on one row instead of wrapping a stub under it', () => {
    // The composer divider most agents print is drawn from ─ alone, with no
    // corners or junctions. Wider than the phone, it used to fall through to
    // ordinary text and wrap, leaving a short second line under a full one.
    for (const stroke of ['─', '━', '═']) {
      const wide = renderTerminalContent(stroke.repeat(74), 'ansi', true, true, 59);
      expect(wide.rows.filter((row) => row.separator)).toHaveLength(1);
      expect(wide.rows.every((row) => !row.text.includes(stroke))).toBe(true);
    }

    // A rule that fits is drawn as cells, which keeps the stroke continuous.
    const fits = renderTerminalContent('─'.repeat(40), 'ansi', true, true, 59);
    expect(fits.rows[0].fixedGrid).toBe(true);

    // Without a width lease there is nothing to fold it into, so the rule stays
    // a grid row the pane scrolls sideways — still never a wrapped stub.
    const unleased = renderTerminalContent('─'.repeat(74), 'ansi', true, true, 0);
    expect(unleased.rows[0].fixedGrid).toBe(true);

    // Prose that merely contains a stroke is still prose, and still re-wraps.
    const prose = renderTerminalContent(`Connected ─ ${'detail '.repeat(20)}`, 'ansi', true, true, 59);
    expect(prose.rows[0].separator).toBe(false);
    expect(prose.rows[0].fixedGrid).toBe(false);
  });

  it('draws a rule the pty already reflowed as one line, not a line plus a stub', () => {
    // The pane hard-wraps a divider wider than itself, and a physical-row read
    // returns both halves. That is one rule: the phone showed a full-width
    // stroke with a short offcut hanging under it.
    const reflowed = renderTerminalContent(
      [`${'─'.repeat(59)}`, `${'─'.repeat(15)}`].join('\n'),
      'ansi', true, true, 59,
    );
    expect(reflowed.rows.filter((row) => row.separator)).toHaveLength(1);
    expect(reflowed.rows).toHaveLength(1);

    // Several continuation rows collapse into the same single rule.
    const long = renderTerminalContent(
      ['─'.repeat(59), '─'.repeat(59), '─'.repeat(12)].join('\n'),
      'ansi', true, true, 59,
    );
    expect(long.rows).toHaveLength(1);

    // Two rules that each fit were never one line, so they stay two.
    const deliberate = renderTerminalContent(
      ['─'.repeat(20), '─'.repeat(20)].join('\n'),
      'ansi', true, true, 59,
    );
    expect(deliberate.rows).toHaveLength(2);
    expect(deliberate.rows.every((row) => row.fixedGrid)).toBe(true);

    // A full-width rule followed by content keeps both.
    const followed = renderTerminalContent(
      ['─'.repeat(59), 'next line'].join('\n'),
      'ansi', true, true, 59,
    );
    expect(followed.rows).toHaveLength(2);
    expect(followed.rows[1].text).toBe('next line');
  });

  it('renders long prose lines so they re-wrap at the phone width', () => {
    // Herdr reflows the pane window to the pty width on every read: no hard
    // wrap is baked in, so the row keeps its full text and the browser wraps it.
    const prose = '\x1b[1mBoth questions answered\x1b[0m — the pane window reflows to the '
      + 'current width, so history rows re-wrap on the phone.';
    const rendered = renderTerminalContent(prose, 'ansi', true, false, 40);
    expect(rendered.rows).toHaveLength(1);
    expect(rendered.rows[0].fixedGrid).toBe(false);
    expect(rendered.rows[0].text).toBe(stripAnsi(prose));
    expect(rendered.html).toContain('font-weight:700');
  });

  it('detects rows cropped from the front of a growing log', () => {
    const row = (text: string) => ({ html: '', text, columns: text.length, fixedGrid: false, separator: false });
    const rows = (start: number, length: number) =>
      Array.from({ length }, (_, index) => row(`log line ${start + index}`));
    expect(renderedRowShift(rows(0, 100), rows(10, 100))).toBe(10);
    expect(renderedRowShift(rows(0, 100), rows(0, 110))).toBe(0);
    expect(renderedRowShift(rows(0, 100), rows(500, 100))).toBe(0);
    const blankHead = [row(''), row('  '), ...rows(40, 60)];
    expect(renderedRowShift([...rows(0, 20), ...blankHead], blankHead)).toBe(20);
    expect(renderedRowShift(rows(0, 4), [row(''), row('only one line')])).toBe(0);
    expect(renderedRowShift([], rows(0, 10))).toBe(0);
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

  it('sorts observed activity newest-first without treating metadata updates as activity', () => {
    expect(agentUpdatedAt(agent({ updated_at: 'invalid' }))).toBe(0);
    expect(agentLastActiveAt(agent({ updated_at: 5 }))).toBe(0);
    expect(agentActivitySeq(agent({ activity_seq: 'invalid' }))).toBe(0);
    const sorted = sortedAgents([
      agent({ pane_id: 'relay::old', raw_pane_id: 'old', last_active_at: 1, updated_at: 9 }),
      agent({ pane_id: 'relay::new', raw_pane_id: 'new', last_active_at: 3, updated_at: 2 }),
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

  it('keeps a structured question mounted across transient answer states', () => {
    const first: QuestionInteraction = {
      id: 'question-1', kind: 'single_select', question: 'First question',
      options: [{ index: 0, label: 'First answer' }],
    };
    const second: QuestionInteraction = {
      ...first, id: 'question-2', question: 'Second question',
    };
    const question = agent({
      status: 'blocked', attention_kind: 'question', attention_capable: true,
      interaction: first, question_layout: true,
    });
    const responding = new Set([question.pane_id]);
    const misses = new Map<string, number>();

    const working = mergeAgentList(
      [question],
      'relay',
      [agent({ status: 'working', attention_capable: true })],
      misses,
      responding,
    )[0];
    expect(working).toMatchObject({
      status: 'blocked', attention_kind: 'question',
      interaction: first, question_layout: true,
    });

    const chat = mergeAgentList(
      [working],
      'relay',
      [agent({ status: 'blocked', attention_kind: 'chat', attention_capable: true })],
      misses,
      responding,
    )[0];
    expect(chat).toMatchObject({
      status: 'blocked', attention_kind: 'question',
      interaction: first, question_layout: true,
    });

    const nextQuestion = mergeAgentList(
      [chat],
      'relay',
      [agent({
        status: 'blocked', attention_kind: 'question', attention_capable: true,
        interaction: second, question_layout: true,
      })],
      misses,
      responding,
    )[0];
    expect(nextQuestion).toMatchObject({
      status: 'blocked', attention_kind: 'question',
      interaction: second, question_layout: true,
    });
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

  it('uses explicit and legacy session titles without exposing identifiers', () => {
    expect(sessionName({ session_name: ' Current title ', session: '/opaque/session.jsonl' })).toBe('Current title');
    expect(sessionName({ session: 'Legacy session title' })).toBe('Legacy session title');
    expect(sessionName({ session: '/home/user/.omp/session.jsonl' })).toBe('');
    expect(sessionName({ session: '019ff024-28dd-7000-8fc4-6961c8ca619b' })).toBe('');
    expect(sessionName({ session_name: '', session: 'stale legacy title' })).toBe('');
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
