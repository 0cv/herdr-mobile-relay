import assert from 'node:assert/strict';
import { realpath } from 'node:fs/promises';
import { resolve } from 'node:path';

const [profile, home, node, ...extra] = process.argv.slice(2);
assert.ok((profile === 'original' || profile === 'patched') && home && node && extra.length === 0,
  'Usage: ios-appium/test.ts original|patched <canonical-home> <canonical-node>');
assert.equal(resolve(home), await realpath(home), 'Canonical fixture home required');
assert.equal(resolve(node), await realpath(node), 'Canonical Node required');
process.env.IOS_PRODUCER_HOME = home;
process.env.IOS_PRODUCER_ORIGINAL_HOME = profile === 'original' ? home : '';
process.env.IOS_PRODUCER_NODE = node;
const { iosProducerOriginalTests, iosProducerTests, iosProducerGateTests, iosInstalledClientTests, iosProducerPrivacyTests } = await import('../unit/ios-producer');
const tests = profile === 'original' ? iosProducerOriginalTests : [...iosProducerTests, ...iosProducerGateTests, ...iosInstalledClientTests, ...iosProducerPrivacyTests];
assert.equal(tests.length, profile === 'original' ? 1 : 14, 'Unexpected fixture regression inventory');
let passed = 0;
for (const [name, body] of tests) {
  await body();
  console.log(`ok ${++passed} - ${name}`);
}
console.log(`${passed} fixture regressions passed (${profile})`);
