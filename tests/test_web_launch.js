const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync('web/index.html', 'utf8');
const start = html.indexOf('function launchNamePart');
const end = html.indexOf('function updateLaunchName', start);

assert.ok(start >= 0 && end > start, 'Launch-name helpers not found');

const sandbox = {};
vm.runInNewContext(`
${html.slice(start, end)}
this.suggestedLaunchName = suggestedLaunchName;
`, sandbox);

const setupStart = html.indexOf('function quickSetupConfig');
const setupEnd = html.indexOf('function importQuickSetup', setupStart);
assert.ok(setupStart >= 0 && setupEnd > setupStart, 'Quick-setup helper not found');
const setupSandbox = {URLSearchParams};
vm.runInNewContext(`
${html.slice(setupStart, setupEnd)}
this.quickSetupConfig = quickSetupConfig;
`, setupSandbox);

const quickSetup = setupSandbox.quickSetupConfig({
  hash: '#setup=0123456789abcdef0123456789abcdef&label=Fedora%20Workstation',
  protocol: 'https:',
  host: 'relay.example.com',
});
assert.equal(quickSetup.label, 'Fedora Workstation');
assert.equal(quickSetup.url, 'wss://relay.example.com');
assert.equal(quickSetup.token, '0123456789abcdef0123456789abcdef');
assert.equal(setupSandbox.quickSetupConfig({hash: '', protocol: 'https:', host: 'relay.example.com'}), null);
assert.equal(setupSandbox.quickSetupConfig({hash: '#setup=too-short', protocol: 'https:', host: 'relay.example.com'}), null);
assert.match(html, /let relayConfigs = loadRelayConfigs\(\).*\nimportQuickSetup\(\);/);
assert.match(html, /history\.replaceState\(history\.state, '', location\.pathname \+ location\.search\)/);

assert.equal(
  sandbox.suggestedLaunchName('/home/me/Development/herdr-mobile-relay', 'codex'),
  'herdr-mobile-relay-codex'
);
assert.equal(sandbox.suggestedLaunchName('/Users/me/Projects/Málaga App', 'claude'), 'malaga-app-claude');
assert.equal(sandbox.suggestedLaunchName('/', 'opencode'), 'project-opencode');

const longName = sandbox.suggestedLaunchName(`/home/me/${'project'.repeat(12)}`, 'codex');
assert.ok(longName.length <= 48);
assert.match(longName, /-codex$/);

const cwdField = html.indexOf('<div class="field-label" id="launchCwdLabel">Working Directory</div>');
const nameField = html.indexOf('<label for="launchName">Name</label>');
assert.ok(cwdField >= 0 && cwdField < nameField, 'Working Directory must appear before Name');
assert.match(html, /<input id="launchCwd" type="hidden" \/>/);
assert.match(html, /id="launchDirectoryUp"[^>]*openParentLaunchDirectory/);
assert.match(html, /id="launchDirectoryList"/);
assert.match(html, /id="launchDirectoryToggle"[^>]*aria-expanded="false"/);
assert.match(html, /\.directory-list \{ display: none;/);
assert.match(html, /\.directory-browser\.open \.directory-list \{ display: block;/);
assert.match(html, /onfocusout="handleLaunchDirectoryFocusOut\(event\)"/);
assert.match(html, /The folder shown above is selected\. Tap it to browse; use ↑ or Parent folder to go back\./);
assert.match(html, /Parent folder<\/button>/);
assert.match(html, /This folder has no subdirectories\. It remains selected\./);
assert.match(html, /Update and restart this computer’s relay to browse directories\./);
assert.match(html, /Sent to the agent as its first prompt after it starts\./);

console.log('Agent launch form tests passed');
