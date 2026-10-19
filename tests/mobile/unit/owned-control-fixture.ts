import assert from 'node:assert/strict';
import { closeSync, fstatSync, openSync, readSync } from 'node:fs';
import { controlChannel } from '../support/owned-process-runtime.ts';

const channel = controlChannel(3);
const mode = process.argv[2];
let errors = 0;
channel.on('error', () => { errors++; });

if (mode === 'roundtrip') {
  let text = '';
  let callbacks = 0;
  setTimeout(() => {
    channel.setEncoding('utf8');
    channel.on('data', chunk => { text += String(chunk); });
    channel.on('end', () => {
      assert.equal(text, 'early-😀-late\n');
      assert.equal(channel.readableEnded, true);
      channel.write('response-'.repeat(65_536), error => {
        assert.equal(error ?? null, null);
        assert.equal(++callbacks, 1);
        assert.equal(errors, 0);
        channel.destroy();
        process.stdout.write('roundtrip\n');
      });
    });
  }, 50);
} else if (mode === 'reuse' || mode === 'early-destroy') {
  const destroy = () => {
    channel.destroy();
    setTimeout(() => {
      const replacement = openSync('/dev/zero', 'r');
      assert.equal(replacement, 3);
      channel.destroy(new Error('already destroyed'));
      setTimeout(() => {
        const bytes = Buffer.alloc(4, 1);
        assert.equal(readSync(replacement, bytes), 4);
        assert.deepEqual(bytes, Buffer.alloc(4));
        assert.equal(errors, 0);
        closeSync(replacement);
        process.stdout.write(`${mode}\n`);
      }, 50);
    }, 50);
  };
  if (mode === 'early-destroy') destroy();
  else setTimeout(destroy, 100);
} else if (mode === 'write-error') {
  let callbacks = 0;
  channel.on('data', () => undefined);
  channel.on('end', () => {
    channel.write('after-close', error => {
      assert.ok(error);
      assert.equal(++callbacks, 1);
      channel.destroy(error);
      channel.destroy(error);
      setTimeout(() => {
        assert.equal(errors, 1);
        process.stdout.write('write-error\n');
      }, 50);
    });
  });
} else if (mode === 'callback-throw') {
  let callbacks = 0;
  const failure = new Error('callback failure');
  if (process.versions.bun) {
    assert.throws(() => channel.write('callback', () => { callbacks++; throw failure; }), error => error === failure);
    assert.equal(callbacks, 1);
    channel.destroy();
    process.stdout.write('callback-throw\n');
  } else {
    process.once('uncaughtException', error => {
      assert.equal(error, failure);
      assert.equal(callbacks, 1);
      channel.destroy();
      process.stdout.write('callback-throw\n');
    });
    channel.write('callback', () => { callbacks++; throw failure; });
  }
} else if (mode === 'destroy-in-buffered-data') {
  const events: string[] = [];
  setTimeout(() => {
    channel.setEncoding('utf8');
    channel.on('data', chunk => {
      events.push(`data:${String(chunk)}`);
      channel.destroy();
    });
    channel.on('end', () => events.push('end'));
    setTimeout(() => {
      assert.deepEqual(events, ['data:early-']);
      assert.equal(channel.readableEnded, false);
      assert.equal(errors, 0);
      process.stdout.write('destroy-in-buffered-data\n');
    }, 100);
  }, 500);
} else if (mode === 'undrained-destroy') {
  assert.equal(channel.write('undrained-'.repeat(1_600_000)), false);
  channel.write('final-frame\n');
  channel.destroy();
  assert.throws(() => channel.write('after-destroy'), /control channel is closed/u);
  setTimeout(() => {
    assert.throws(() => fstatSync(3), { code: 'EBADF' });
    assert.equal(errors, 0);
    process.stdout.write('undrained-destroy\n');
  }, 100);
} else if (mode === 'undrained-abort') {
  const keepAlive = setInterval(() => undefined, 1_000);
  assert.equal(channel.write('undrained-'.repeat(1_600_000)), false);
  channel.destroy(new Error('abort teardown'));
  setTimeout(() => {
    assert.throws(() => fstatSync(3), { code: 'EBADF' });
    assert.equal(errors, 1);
    clearInterval(keepAlive);
    process.stdout.write('undrained-abort\n');
  }, 100);
} else throw new Error('unknown control fixture mode');
