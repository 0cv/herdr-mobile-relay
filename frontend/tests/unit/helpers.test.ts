import { describe, expect, it } from 'vitest';
import { activityForNotification, activityMatchesSearch } from '$lib/activity';
import {
  agentStatusTone,
  agentUpdatedAt,
  mergeAgentDetails,
  mergeAgentList,
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
  claudeMobileTerminalContent,
  compactRepeatedCharacterRuns,
  isSeparatorOnlyLine,
  lastCompletedResponse,
  removeCodexDesktopInput,
  removeClaudeStatusBlocks,
  renderTerminalContent,
  stripAnsi,
  TERMINAL_REPEATED_RUN_LIMIT,
  TERMINAL_SEPARATOR_TOKEN,
  terminalHtml,
  trimTerminalChrome,
  trimTrailingDecoration,
} from '$lib/terminal';
import type { Agent, QuestionInteraction, RelayConnectionView } from '$lib/types';

function agent(overrides: Partial<Agent>): Agent {
  return {
    relay_id: 'relay', relay_label: 'Fedora', raw_pane_id: 'w1:p1', pane_id: 'relay::w1:p1', ...overrides,
  };
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
    expect(renderTerminalContent('<script>alert(1)</script>', 'plain', 'codex', true).html).toBe('&lt;script&gt;alert(1)&lt;/script&gt;');
  });

  it('normalizes light-origin ANSI colors onto the dark mobile terminal', () => {
    const blackText = renderTerminalContent('\x1b[38;2;24;24;24mMac text\x1b[0m', 'ansi', 'codex', true);
    expect(blackText.html).toContain('color:var(--terminal-text)');

    const darkBlue = renderTerminalContent('\x1b[38;2;20;40;80mBlue text\x1b[0m', 'ansi', 'claude', true);
    expect(darkBlue.html).toContain('color:color-mix(in srgb, rgb(20,40,80) 35%, var(--terminal-text))');

    const lightRow = renderTerminalContent(
      '\x1b[48;2;250;250;250;38;2;20;20;20mLight terminal row\x1b[0m',
      'ansi',
      'codex',
      true,
    );
    expect(lightRow.html).toContain('background-color:rgb(61,64,64)');
    expect(lightRow.html).toContain('color:var(--terminal-text)');

    const brightAccent = renderTerminalContent('\x1b[38;2;95;175;255mAccent\x1b[0m', 'ansi', 'codex', true);
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
    ].join('\n'), 'ansi', 'codex', true);
    expect(rendered.display).toBe([
      'Before', '', '', TERMINAL_SEPARATOR_TOKEN, '', '', 'After',
    ].join('\n'));
    expect(rendered.html.match(/class="term-separator"/g)).toHaveLength(1);
    expect(rendered.html.match(/class="ansi-line"/g)).toHaveLength(6);
  });

  it('caps arbitrary repeated symbols embedded in terminal output', () => {
    const progress = `${'.'.repeat(120)} [29%]`;
    expect(compactRepeatedCharacterRuns(progress))
      .toBe(`${'.'.repeat(TERMINAL_REPEATED_RUN_LIMIT)} [29%]`);
    expect(compactRepeatedCharacterRuns('a'.repeat(120))).toBe('a'.repeat(120));

    const rendered = renderTerminalContent(progress, 'ansi', 'codex', true);
    expect(stripAnsi(rendered.display))
      .toBe(`${'.'.repeat(TERMINAL_REPEATED_RUN_LIMIT)} [29%]`);
  });

  it('removes desktop-width decoration after terminal status text', () => {
    const decorated = `\x1b[2m─ Worked for 1m 46s ${'─'.repeat(120)}\x1b[0m`;
    expect(stripAnsi(trimTrailingDecoration(decorated))).toBe('─ Worked for 1m 46s');

    const rendered = renderTerminalContent(decorated, 'ansi', 'codex', true);
    expect(stripAnsi(rendered.display)).toBe('─ Worked for 1m 46s');
    expect(rendered.html).toContain('Worked for 1m 46s');
    expect(rendered.html).not.toContain('────────');
  });

  it('removes Claude box chrome and leading status decoration', () => {
    const rule = '─'.repeat(120);
    expect(isSeparatorOnlyLine(`╰${rule}╯`)).toBe(true);
    expect(stripAnsi(trimTerminalChrome(`\x1b[36m│\x1b[0m  Result text${' '.repeat(20)}\x1b[36m│\x1b[0m`)))
      .toBe('Result text');
    expect(stripAnsi(trimTerminalChrome(`\x1b[2m${rule} Opus 4.8 | ctx: 20%\x1b[0m`)))
      .toBe('Opus 4.8 | ctx: 20%');
    expect(stripAnsi(trimTerminalChrome(`\x1b[2m${'§'.repeat(120)} Opus 4.8 | ctx: 20%\x1b[0m`)))
      .toBe('Opus 4.8 | ctx: 20%');

    const rendered = renderTerminalContent([
      `\x1b[36m│\x1b[0m  Result text${' '.repeat(20)}\x1b[36m│\x1b[0m`,
      '\x1b[36m│\x1b[0m',
      `\x1b[36m╰${rule}╯\x1b[0m`,
      `\x1b[36m${rule}\x1b[0m`,
      `\x1b[36m╭${rule}╮\x1b[0m`,
      `\x1b[2m${rule} Opus 4.8 | ctx: 20%\x1b[0m`,
    ].join('\n'), 'ansi', 'claude', true);
    expect(stripAnsi(rendered.display)).toBe([
      'Result text', '', TERMINAL_SEPARATOR_TOKEN, 'Opus 4.8 | ctx: 20%',
    ].join('\n'));
    expect(rendered.html.match(/class="term-separator"/g)).toHaveLength(1);
  });

  it('hides current multi-row Claude status blocks as one unit', () => {
    const frame = [
      'Answer remains visible.',
      '› Try "fix typecheck errors"',
      `${'─'.repeat(100)} Fable 5 |`,
      '~/Development/sfdc/argenx-patient on infinitus-pa-',
      'appeal-main | ctx -- | 5h 49% 7d 12% manual mode',
      'on · PR #804 · ← 1 agent',
    ].join('\n');
    const hidden = removeClaudeStatusBlocks(frame);
    expect(hidden).toContain('Answer remains visible.');
    expect(hidden).toContain('Try "fix typecheck errors"');
    expect(hidden).not.toContain('Fable 5');
    expect(hidden).not.toContain('ctx --');
    expect(hidden).not.toContain('PR #804');

    const rendered = renderTerminalContent(frame, 'ansi', 'claude', false);
    expect(stripAnsi(rendered.display)).toContain('Answer remains visible.');
    expect(stripAnsi(rendered.display)).not.toContain('Fable 5');
    expect(stripAnsi(rendered.display)).not.toContain('argenx-patient');
    expect(stripAnsi(rendered.display)).not.toContain('ctx --');

    const shown = renderTerminalContent(frame, 'ansi', 'claude', true);
    expect(stripAnsi(shown.display)).toContain('Fable 5');
    expect(stripAnsi(shown.display)).toContain('ctx --');

    const unavailableContext = [
      '› Try "edit Info.plist to..."',
      `${'─'.repeat(80)} Opus 4.8 | ctx: - | main ~16 /rc ⏸ manual mode on · ← for agents`,
    ].join('\n');
    const hiddenUnavailable = renderTerminalContent(unavailableContext, 'ansi', 'claude', false);
    expect(stripAnsi(hiddenUnavailable.display)).toContain('edit Info.plist');
    expect(stripAnsi(hiddenUnavailable.display)).not.toContain('Opus 4.8');
    expect(stripAnsi(hiddenUnavailable.display)).not.toContain('ctx: -');
    expect(stripAnsi(hiddenUnavailable.display)).not.toContain('manual mode');
  });

  it('separates Claude conversation output from desktop prompt and status chrome', () => {
    const footer = [
      '❯ Try "edit Info.plist to..."',
      'separator',
      'Opus 4.8',
      'ctx: -',
      'main ~16',
      '/rc ⏸ manual mode on · ← for agents',
    ];
    const content = [...Array.from({ length: 8 }, (_, index) => `output ${index + 1}`), ...footer].join('\n');

    const hidden = claudeMobileTerminalContent(content, false);
    expect(hidden.separated).toBe(true);
    expect(hidden.content).toContain('output 8');
    expect(hidden.content).not.toContain('Try "edit Info.plist');
    expect(hidden.content).not.toContain('Opus 4.8');

    const shown = claudeMobileTerminalContent(content, true);
    expect(shown.separated).toBe(true);
    expect(shown.content).not.toContain('Try "edit Info.plist');
    expect(shown.content).not.toContain('separator');
    expect(shown.content).toContain('Opus 4.8');
    expect(shown.content).toContain('manual mode');
  });

  it('removes the final styled Codex desktop input block regardless of its placeholder', () => {
    const backgroundRow = '\x1b[48;2;61;64;64m                    \x1b[0m';
    const promptRow = (text: string) => `\x1b[1;48;2;61;64;64m›\x1b[0m\x1b[2;48;2;61;64;64m ${text}\x1b[0m`;
    const status = 'gpt-5.6-sol xhigh · ~/project · main · Context 30% used';
    const content = [
      'Completed output', '', backgroundRow, promptRow('Review the current diff'), backgroundRow, status,
    ].join('\n');
    expect(stripAnsi(removeCodexDesktopInput(content))).toBe(`Completed output\n\n${status}`);
    expect(stripAnsi(renderTerminalContent(content, 'ansi', 'codex', true).display))
      .not.toContain('Review the current diff');

    const transcript = `${promptRow('Historical prompt')}\nActual response`;
    expect(removeCodexDesktopInput(transcript)).toBe(transcript);
    expect(removeCodexDesktopInput('› Normal transcript text')).toBe('› Normal transcript text');
  });

  it('extracts the latest completed Codex and Claude responses', () => {
    expect(lastCompletedResponse([
      '• Earlier answer.', '─ Worked for 2s ─', '', '› New question', '', '• Latest answer.',
      '  - First detail', '  - Second detail', '', '─ Worked for 8m 05s ─', '', '› Next question',
    ].join('\n'))).toBe('Latest answer.\n- First detail\n- Second detail');
    expect(lastCompletedResponse('● The implementation is ready.\n  It works.\n\n✻ Crunched for 1m 49s\n❯ '))
      .toBe('The implementation is ready.\nIt works.');
    expect(lastCompletedResponse('● Still working\n')).toBe('');
  });
});

describe('agent state and sorting', () => {
  it('maps active agent states to semantic indicator tones', () => {
    expect(agentStatusTone(agent({ status: 'working' }))).toBe('warning');
    expect(agentStatusTone(agent({ status: 'blocked' }))).toBe('danger');
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
    const sorted = sortedAgents([
      agent({ pane_id: 'relay::old', raw_pane_id: 'old', updated_at: 1 }),
      agent({ pane_id: 'relay::new', raw_pane_id: 'new', updated_at: 3 }),
    ]);
    expect(sorted.map((item) => item.raw_pane_id)).toEqual(['new', 'old']);
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
    const question = agent({ status: 'blocked', interaction, question_layout: true });
    const approval = agent({ status: 'blocked', interaction: null, question_layout: false });
    expect(mergeAgentDetails(question, approval)).toMatchObject({
      status: 'blocked', interaction: null, question_layout: false,
    });

    const sparse = agent({ status: 'blocked' });
    expect(mergeAgentDetails(question, sparse)).toMatchObject({
      interaction, question_layout: true,
    });
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
