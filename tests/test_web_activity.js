const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const html = fs.readFileSync('web/index.html', 'utf8');
const start = html.indexOf('function activityMatchesSearch');
const end = html.indexOf('function activityColor', start);

assert.ok(start >= 0 && end > start, 'Activity search helper not found');

const sandbox = {};
vm.runInNewContext(`${html.slice(start, end)}\nthis.activityMatchesSearch = activityMatchesSearch;`, sandbox);

const activity = {
  summary: 'Approval accepted',
  kind: 'approval',
  status: 'confirmed',
  relay_label: 'Fedora',
  project: 'herdr-mobile-relay',
  agent: 'Codex',
  details: {choice: 'Approve once'},
};

assert.equal(sandbox.activityMatchesSearch(activity, ''), true);
assert.equal(sandbox.activityMatchesSearch(activity, 'fedora'), true);
assert.equal(sandbox.activityMatchesSearch(activity, 'herdr-mobile'), true);
assert.equal(sandbox.activityMatchesSearch(activity, 'approve once'), true);
assert.equal(sandbox.activityMatchesSearch(activity, 'missing'), false);

const sortStart = html.indexOf('function agentUpdatedAt');
const sortEnd = html.indexOf('function sortedAgents', sortStart);
assert.ok(sortStart >= 0 && sortEnd > sortStart, 'Agent activity sort helpers not found');
const sortSandbox = {};
vm.runInNewContext(`
${html.slice(sortStart, sortEnd)}
this.agentUpdatedAt = agentUpdatedAt;
this.compareAgentUpdatedAt = compareAgentUpdatedAt;
`, sortSandbox);

assert.equal(sortSandbox.agentUpdatedAt({updated_at: '2000'}), 2000);
assert.equal(sortSandbox.agentUpdatedAt({updated_at: 'invalid'}), 0);
assert.ok(sortSandbox.compareAgentUpdatedAt({updated_at: 3000}, {updated_at: 1000}) < 0);
assert.ok(sortSandbox.compareAgentUpdatedAt({updated_at: 1000}, {updated_at: 3000}) > 0);
assert.match(html, /compareAgentUpdatedAt\(a, b\) \|\|/);
assert.match(html, /a\.host, a\.updated_at, a\.prompt/);

console.log('Activity search and agent sorting tests passed');
