import assert from 'node:assert/strict';
import { StringDecoder } from 'node:string_decoder';
import { XCTestOutputDiagnostics } from '../support/xctest-output';

const clock = () => ({ observedAt: '2026-10-23T06:08:59.000Z', monotonicReceiveMs: 42 });
export const xctestOutputTests: Array<[string, () => Promise<void>]> = [];
const test = (name: string, body: () => void) => xctestOutputTests.push([name, async () => body()]);

test('XCTest output synthetic activities retain enums and host receive times only', () => {
  const output = new XCTestOutputDiagnostics(clock);
  const activities = [
    'Tap "private-label" Cell', 'Find the "private-label" Cell', 'Wait for private-app to idle',
    'Wait for private-app animations to finish', 'Check for interrupting elements affecting private-app',
    'Synthesize event', 'Requesting snapshot of accessibility hierarchy for app with pid 98765',
  ];
  output.observe(activities.map(activity => `    t =  12345.67s    ${activity}\r\n`).join(''), 'stdout');
  assert.deepEqual(output.data.events.map(event => event.stage), [
    'tap', 'find-element', 'wait-for-idle', 'wait-for-animation', 'check-interruptions', 'synthesize-event', 'request-snapshot',
  ]);
  assert.ok(output.data.events.every(event => event.observedAt === clock().observedAt && event.monotonicReceiveMs === 42 && event.stream === 'stdout'));
  const serialized = JSON.stringify(output.data);
  for (const value of ['private-label', 'private-app', '98765', '12345.67']) assert.equal(serialized.includes(value), false);
  assert.equal(output.data.completeRecords, 7);
  assert.equal(output.data.recognizedRecords, 7);
  assert.equal(output.data.unknownRecords, 0);
});

test('XCTest output separates fragmented streams and UTF-8 labels without exposing text', () => {
  const output = new XCTestOutputDiagnostics(clock);
  const decoder = new StringDecoder('utf8');
  const bytes = Buffer.from('    t =  1.23s Tap "秘密-secret" Cell\n');
  for (const byte of bytes) output.observe(decoder.write(Buffer.from([byte])), 'stdout');
  output.observe(decoder.end(), 'stdout');
  output.observe('    t =  2.00s Syn', 'stderr');
  output.observe('PASSWORD=opaque-secret\n', 'stdout');
  output.observe('thesize event\n', 'stderr');
  assert.deepEqual(output.data.events.map(event => [event.stream, event.stage]), [['stdout', 'tap'], ['stderr', 'synthesize-event']]);
  assert.equal(output.data.unknownRecords, 1);
  for (const value of ['秘密', 'secret', 'PASSWORD']) assert.equal(JSON.stringify(output.data).includes(value), false);
});

test('XCTest output rejects unsupported malformed and partial records', () => {
  const output = new XCTestOutputDiagnostics(clock);
  output.observe('Tap secret\nnoise t =  1.00s Tap secret\n    t =  1.00s Unsupported secret\n    t =  1.00s Tap \uFFFD\n', 'stdout');
  output.observe('    t =  1.00s Tap incomplete-secret', 'stderr');
  output.finish('stderr');
  output.finish('stderr');
  output.observe('\n    t =  1.00s Tap after-finish\n', 'stderr');
  assert.equal(output.data.unknownRecords, 4);
  assert.equal(output.data.completeRecords, 4);
  assert.equal(output.data.incompleteRecords, 1);
  assert.equal(output.data.recognizedRecords, 0);
  assert.deepEqual(output.data.events, []);
});

test('XCTest output bounds records before concatenation and discards oversized suffixes', () => {
  const output = new XCTestOutputDiagnostics(clock);
  const prefix = '    t =  1.00s Tap ';
  output.observe(prefix + 'x'.repeat(4_096 - Buffer.byteLength(prefix)) + '\n', 'stdout');
  output.observe(prefix, 'stdout');
  for (let index = 0; index < 32; index++) output.observe('x'.repeat(65_536), 'stdout');
  output.observe('    t =  2.00s Synthesize event\n    t =  3.00s Synthesize event\n', 'stdout');
  output.observe('秘密'.repeat(700), 'stderr');
  output.finish('stderr');
  assert.equal(output.data.oversizedRecords, 2);
  assert.equal(output.data.incompleteRecords, 1);
  assert.equal(output.data.completeRecords, 3);
  assert.deepEqual(output.data.events.map(event => event.stage), ['tap', 'synthesize-event']);
  assert.equal(output.data.unknownRecords, 0);
});

test('XCTest output keeps a bounded activity tail and explicit dropped counts under a flood', () => {
  const output = new XCTestOutputDiagnostics(clock);
  output.observe('    t =  1.00s Synthesize event\n'.repeat(1_000), 'stdout');
  output.observe('opaque-secret\n'.repeat(1_000), 'stderr');
  output.finish('stdout');
  output.finish('stderr');
  assert.equal(output.data.recognizedRecords, 1_000);
  assert.equal(output.data.unknownRecords, 1_000);
  assert.equal(output.data.completeRecords, 2_000);
  assert.equal(output.data.events.length, 128);
  assert.equal(output.data.droppedEvents, 872);
  assert.equal(output.data.events[0].ordinal, 873);
  assert.equal(output.data.events.at(-1)?.ordinal, 1_000);
  assert.ok(Buffer.byteLength(JSON.stringify(output.data)) < 32_768);
  assert.equal(JSON.stringify(output.data).includes('opaque-secret'), false);
});
