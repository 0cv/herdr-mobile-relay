const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync('web/index.html', 'utf8');
assert.match(html, /\.term-content \{[^}]*padding: 10px 16px/);
assert.match(html, /\.ansi-line \{[^}]*overflow: hidden/);
const colorsStart = html.indexOf('const ANSI_COLORS =');
const colorsEnd = html.indexOf('\n};', colorsStart) + 3;
const rendererStart = html.indexOf('function trimAnsiLineEnd');
const rendererEnd = html.indexOf('function hostLabel', rendererStart);

assert.ok(colorsStart >= 0 && colorsEnd > colorsStart, 'ANSI color table not found');
assert.ok(rendererStart >= 0 && rendererEnd > rendererStart, 'ANSI renderer not found');

const sandbox = {};
vm.runInNewContext(`
${html.slice(colorsStart, colorsEnd)}
const TERMINAL_SEPARATOR_TOKEN = '\\uE000HERDR_SEPARATOR\\uE000';
${html.slice(rendererStart, rendererEnd)}
this.ansi256Color = ansi256Color;
this.ansiToHtml = ansiToHtml;
this.isNearWhiteAnsiColor = isNearWhiteAnsiColor;
this.ansiLineBackground = ansiLineBackground;
this.ansiLineBackgroundIndent = ansiLineBackgroundIndent;
this.ansiLineBackgroundStyle = ansiLineBackgroundStyle;
this.ansiLineBackgrounds = ansiLineBackgrounds;
this.terminalHtml = terminalHtml;
`, sandbox);

assert.equal(sandbox.ansi256Color(6), '#1abc9c');
assert.equal(sandbox.ansi256Color(196), 'rgb(255,0,0)');
assert.equal(sandbox.ansi256Color(244), 'rgb(128,128,128)');

const tools = sandbox.ansiToHtml('\x1b[38;5;6mSearch\x1b[0m and \x1b[38;5;6mRead\x1b[0m');
assert.match(tools, /color:#1abc9c[^>]*>Search/);
assert.match(tools, /color:#1abc9c[^>]*>Read/);

const background = sandbox.ansiToHtml('\x1b[1;48;5;196mAlert\x1b[0m');
assert.match(background, /font-weight:700/);
assert.match(background, /background-color:rgb\(255,0,0\)/);

const explored = sandbox.terminalHtml('\x1b[1mExplored\x1b[0m');
assert.match(explored, /font-weight:700;color:#5fafff[^>]*>Explored/);

const claudeHeading = sandbox.terminalHtml(
  '  \x1b[0m\x1b[1mWhat happened:\x1b[0m normal explanation',
  false,
  true
);
assert.match(claudeHeading, /font-weight:700;color:rgb\(56,162,223\)[^>]*>What happened:/);
const claudeSplitColonHeading = sandbox.terminalHtml(
  '  \x1b[0m\x1b[1mThe fix in start.sh\x1b[0m: normal explanation',
  false,
  true
);
assert.match(claudeSplitColonHeading, /font-weight:700;color:rgb\(56,162,223\)[^>]*>The fix in start\.sh:/);
const claudeEmphasis = sandbox.terminalHtml('\x1b[1mRoot cause found and fixed.\x1b[0m', false, true);
assert.match(claudeEmphasis, /font-weight:700[^>]*>Root cause found and fixed/);
assert.doesNotMatch(claudeEmphasis, /color:rgb\(56,162,223\)/);
const claudeStandaloneHeading = sandbox.terminalHtml('  \x1b[0m\x1b[1mCode style\x1b[0m\n', false, true);
assert.match(claudeStandaloneHeading, /font-weight:700;color:rgb\(56,162,223\)[^>]*>Code style/);
const claudeListLeadIn = sandbox.terminalHtml(
  '  - \x1b[0m\x1b[1mReconnect is a fixed 3-second loop forever.\x1b[0m Normal explanation',
  false,
  true
);
assert.match(
  claudeListLeadIn,
  /font-weight:700;color:rgb\(56,162,223\)[^>]*>Reconnect is a fixed 3-second loop forever\./
);
const claudeBulletEmphasis = sandbox.terminalHtml('• \x1b[1mImportant emphasis.\x1b[0m\n', false, true);
assert.doesNotMatch(claudeBulletEmphasis, /color:rgb\(56,162,223\)/);

const prompt = sandbox.terminalHtml([
  '\x1b[48;2;61;64;64m› First prompt paragraph   \x1b[0m',
  '\r',
  '\x1b[48;2;61;64;64mSecond paragraph that wraps on a phone   \x1b[0m',
].join('\n'));
assert.equal(sandbox.ansiLineBackground('\x1b[48;2;61;64;64m› Prompt\x1b[0m'), 'rgb(61,64,64)');
assert.match(prompt, /class="ansi-line ansi-line-background" style="background-color:rgb\(61,64,64\)"/);
assert.equal((prompt.match(/class="ansi-line ansi-line-background"/g) || []).length, 3);
assert.doesNotMatch(prompt, /paragraph {3}/);

const claudeToolHeader = sandbox.terminalHtml(
  '\x1b[0m\x1b[38;2;78;186;101m● \x1b[0m\x1b[1mUpdate\x1b[0m(relay/start.sh)',
  false,
  true
);
assert.doesNotMatch(claudeToolHeader, /background-color|background-image/);
assert.match(claudeToolHeader, /color:rgb\(78,186,101\)[^>]*>●/);

const claudeDiff = sandbox.terminalHtml(
  '     \x1b[38;2;80;200;80m\x1b[48;2;2;40;0m  88 +\x1b[0m\x1b[48;2;2;40;0m changed code\x1b[0m',
  false,
  true
);
assert.equal(
  sandbox.ansiLineBackground('     \x1b[38;2;80;200;80m\x1b[48;2;2;40;0m  88 +\x1b[0m'),
  'rgb(2,40,0)'
);
assert.match(claudeDiff, /background-image:linear-gradient\(to right,transparent 0 5ch,rgb\(2,40,0\) 5ch\)/);
assert.match(claudeDiff, /padding-left:5ch;text-indent:-5ch/);

const separateBlocks = sandbox.ansiLineBackgrounds([
  '\x1b[48;5;1mFirst block\x1b[0m',
  '',
  'Normal output',
  '',
  '\x1b[48;5;1mSecond block\x1b[0m',
]);
assert.deepEqual(separateBlocks, ['#ff5f5f', '', '', '', '#ff5f5f']);

assert.equal(sandbox.isNearWhiteAnsiColor('#fff'), true);
assert.equal(sandbox.isNearWhiteAnsiColor('#e5e5e5'), true);
assert.equal(sandbox.isNearWhiteAnsiColor('rgb(242,242,242)'), true);
assert.equal(sandbox.isNearWhiteAnsiColor('rgb(220,220,180)'), false);

for (const lightBackground of [
  '\x1b[107m› Standard bright-white prompt\x1b[0m',
  '\x1b[48;5;15m› ANSI-256 white prompt\x1b[0m',
  '\x1b[48;2;242;242;242m› Truecolor near-white prompt\x1b[0m',
]) {
  const normalized = sandbox.terminalHtml(lightBackground, true);
  assert.match(normalized, /class="ansi-line ansi-line-background" style="background-color:rgb\(61,64,64\)"/);
  assert.doesNotMatch(normalized, /background-color:(?:#fff|rgb\(242,242,242\))/);
}

// Colorless bold+italic mirrors herdr's theme accent (Codex markdown
// headings); plain italic and explicitly colored italic retain their styles.
const italicHeading = sandbox.ansiToHtml('\x1b[0m\x1b[1m\x1b[3m### Heading\x1b[0m');
assert.match(italicHeading, /font-style:italic/);
assert.match(italicHeading, /color:#3daee9/);
const plainItalic = sandbox.ansiToHtml('\x1b[3memphasized text\x1b[0m');
assert.match(plainItalic, /font-style:italic/);
assert.doesNotMatch(plainItalic, /#3daee9/);
const italicColored = sandbox.ansiToHtml('\x1b[3;38;2;148;226;213mteal italic\x1b[0m');
assert.match(italicColored, /color:rgb\(148,226,213\)/);
assert.doesNotMatch(italicColored, /#3daee9/);

const nonCodexWhite = sandbox.terminalHtml('\x1b[107mWhite row\x1b[0m');
assert.match(nonCodexWhite, /background-color:#fff/);

const inlineWhite = sandbox.terminalHtml('Normal text \x1b[107mwhite highlight\x1b[0m', true);
assert.match(inlineWhite, /background-color:#fff/);
assert.doesNotMatch(inlineWhite, /ansi-line-background/);

// DOM writes in the terminal view must be skipped when nothing changed:
// unconditional rebuilds at polling frequency flicker the approval buttons
// and cancel Android's keyboard suggestion session while it initialises.
assert.match(html, /function updateTerminalContent[\s\S]*?if \(displayContent === lastTerminalDisplayContent && format === lastTerminalFormat\) return;/);
assert.match(html, /function syncTerminalChrome[\s\S]*?if \(signature !== lastTerminalChrome\)/);

// --- Behavioral: the quick-actions guard must keep identical button DOM
// untouched, yet restore has-quick-actions after terminal navigation strips
// it (leaving keeps the cached buttons but removes the class).
const qaStart = html.indexOf('function renderTerminalActions');
const qaEnd = html.indexOf('function nextBlockedAgent', qaStart);
assert.ok(qaStart >= 0 && qaEnd > qaStart, 'quick actions functions not found');

function fakeElement() {
  const el = {
    _html: '',
    htmlWrites: 0,
    dataset: {},
    classes: new Set(),
    get innerHTML() { return this._html; },
    set innerHTML(value) { this._html = value; this.htmlWrites += 1; },
  };
  el.classList = {
    add: (...names) => names.forEach(name => el.classes.add(name)),
    remove: (...names) => names.forEach(name => el.classes.delete(name)),
    toggle: (name, force) => { if (force) el.classes.add(name); else el.classes.delete(name); },
    contains: (name) => el.classes.has(name),
  };
  return el;
}

const qaEl = fakeElement();
const viewEl = fakeElement();
let heightCalls = 0;
const qaSandbox = {
  document: {getElementById: (id) => (id === 'quickActions' ? qaEl : viewEl)},
  nextBlockedAgent: () => null,
  agentStatusGroup: () => 'blocked',
  respondingPaneIds: new Set(),
  approvalOptions: () => ['yes', 'always', 'no'],
  approvalButtonClass: () => 'btn',
  approvalButtonLabel: (option) => option,
  escapeHtml: (value) => String(value),
  updateBottomChromeHeight: () => { heightCalls += 1; },
};
vm.runInNewContext(`
${html.slice(qaStart, qaEnd)}
this.renderTerminalActions = renderTerminalActions;
this.clearQuickActions = clearQuickActions;
`, qaSandbox);

const blockedAgent = {pane_id: 'r::w1:p1'};
qaSandbox.renderTerminalActions(blockedAgent);
assert.equal(qaEl.htmlWrites, 1);
assert.ok(qaEl.innerHTML.includes('respond(0, 3)'));
assert.ok(viewEl.classes.has('has-quick-actions'));
const renderedButtons = qaEl.innerHTML;

// Identical update: no rewrite, no height recalculation.
const heightAfterFirst = heightCalls;
qaSandbox.renderTerminalActions(blockedAgent);
assert.equal(qaEl.htmlWrites, 1, 'identical update must not rebuild buttons');
assert.equal(heightCalls, heightAfterFirst);

// Leaving the terminal strips the class but keeps the cached buttons;
// re-entering the same blocked agent must restore the class without a rebuild.
viewEl.classList.remove('active', 'has-quick-actions');
qaSandbox.renderTerminalActions(blockedAgent);
assert.ok(viewEl.classes.has('has-quick-actions'), 're-entry must restore has-quick-actions');
assert.equal(qaEl.htmlWrites, 1, 're-entry must not rebuild identical buttons');
assert.equal(qaEl.innerHTML, renderedButtons);

// Clearing resets everything and the next render rebuilds from scratch.
qaSandbox.clearQuickActions();
assert.equal(qaEl.innerHTML, '');
assert.ok(!viewEl.classes.has('has-quick-actions'));
qaSandbox.renderTerminalActions(blockedAgent);
assert.equal(qaEl.htmlWrites, 3);
assert.ok(viewEl.classes.has('has-quick-actions'));

// --- Behavioral: the composer must lock while the active agent waits for
// approval (free text is meaningless against an approval menu) and unlock
// once the agent moves on.
const composerStart = html.indexOf('function updateComposerState');
const composerEnd = html.indexOf('function sendKey', composerStart);
assert.ok(composerStart >= 0 && composerEnd > composerStart, 'updateComposerState not found');

const inputEl = {disabled: false, placeholder: 'Type…', value: ''};
const sendEl = {disabled: true};
const attachEl = {disabled: false};
const fieldEl = {classes: new Set()};
fieldEl.classList = {toggle: (name, force) => { if (force) fieldEl.classes.add(name); else fieldEl.classes.delete(name); }};
let composerAgent = {pane_id: 'r::w1:p1', status: 'blocked'};
const composerSandbox = {
  document: {getElementById: (id) => ({termInput: inputEl, sendButton: sendEl, attachButton: attachEl, composerField: fieldEl}[id])},
  activeAgent: () => composerAgent,
  agentStatusGroup: (agent) => agent.status,
  composerPromptText: (input) => input.value,
};
vm.runInNewContext(`
${html.slice(composerStart, composerEnd)}
this.updateComposerState = updateComposerState;
`, composerSandbox);

composerSandbox.updateComposerState();
assert.equal(inputEl.disabled, true, 'composer must lock while blocked');
assert.equal(attachEl.disabled, true);
assert.equal(sendEl.disabled, true);
assert.match(inputEl.placeholder, /approval/i);

composerAgent = {pane_id: 'r::w1:p1', status: 'working'};
inputEl.value = 'hello';
composerSandbox.updateComposerState();
assert.equal(inputEl.disabled, false, 'composer must unlock when no longer blocked');
assert.equal(attachEl.disabled, false);
assert.equal(sendEl.disabled, false);
assert.equal(inputEl.placeholder, 'Type…');

console.log('ANSI terminal renderer tests passed');
