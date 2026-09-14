import { strict as assert } from 'node:assert';
import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { cp, mkdtemp, mkdir, readFile, writeFile, rm, realpath, symlink } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer as createNetServer, type AddressInfo } from 'node:net';
import { managedWdaCapabilities } from '../support/ios-xctest';
import { IOSPlatform } from '../platforms/ios';

const credentialBinaryPlistBase64 = 'YnBsaXN0MDDUAQIDBAUGBwhYUEFTU1dPUkRbUFJJVkFURV9VUkxcQVBJX1BBU1NXT1JEXxASQ0ZCdW5kbGVJZGVudGlmaWVyXxAWYmFyZS1wYXNzd29yZC1zZW50aW5lbF8QN2h0dHBzOi8vcHJpdmF0ZS5leGFtcGxlL2luc3RhbGxlZD9yZWY9cHJpdmF0ZS1yZWZlcmVuY2VecGxhaW4tcGFzc3dvcmRfECtjb20uZmFjZWJvb2suV2ViRHJpdmVyQWdlbnRSdW5uZXIueGN0cnVubmVyCBEaJjNIYZuqAAAAAAAAAQEAAAAAAAAACQAAAAAAAAAAAAAAAAAAANg=';

const shim = `#!/bin/sh
cmd=$0
cmd=\${cmd##*/}
root=$STARTUP_TEST_ROOT
mode=$STARTUP_TEST_MODE
trace=$XCTEST_COMMAND_TRACE
if [ -n "$trace" ]; then
  printf 'start %s %s' "$(date +%s%N)" "$cmd" >> "$trace"
  for argument do printf ' [%s]' "$argument" >> "$trace"; done
  printf '\\n' >> "$trace"
  trap 'printf "end %s %s\\n" "$(date +%s%N)" "$cmd" >> "$trace"' 0
fi
has_arg() {
  needle=$1
  shift
  for argument do
    [ "$argument" = "$needle" ] && return 0
  done
  return 1
}
if [ "$mode" = "delayed" ]; then
  if [ "$cmd" = "lsof" ] && has_arg -d "$@"; then
    /bin/sleep 0.6
  elif [ "$cmd" = "ps" ]; then
    delayed_ps_count=0
    [ -e "$root/delayed-ps-count" ] && delayed_ps_count=$(cat "$root/delayed-ps-count")
    printf '%s' "$((delayed_ps_count + 1))" > "$root/delayed-ps-count"
    if [ "$delayed_ps_count" -gt 0 ]; then
      /bin/sleep 0.6
    fi
  fi
fi
case "$cmd" in
plutil)
  if [ "$1" = "-extract" ]; then
    path=$6
    product_info=false
    case "$path" in
      "$STARTUP_TEST_PRODUCT/Info.plist"|/private$STARTUP_TEST_PRODUCT/Info.plist) product_info=true ;;
    esac
    if [ "$mode" = "product" ] && [ "$product_info" = true ]; then
      printf '%s\\n' wrong
    else
      bundle_mismatch=false
      case "$path" in
        "$STARTUP_TEST_RUNNER_RECEIPT"/*|/private$STARTUP_TEST_RUNNER_RECEIPT/*) bundle_mismatch=true ;;
      esac
      if [ "$mode" = "noisy-cleanup" ] && [ "$bundle_mismatch" = true ]; then
        noisy_bundle_count=0
        [ -e "$root/noisy-bundle-count" ] && noisy_bundle_count=$(cat "$root/noisy-bundle-count")
        printf '%s' "$((noisy_bundle_count + 1))" > "$root/noisy-bundle-count"
        if [ "$noisy_bundle_count" -ge 2 ] && [ ! -e "$root/noisy-first-failure" ]; then
          touch "$root/noisy-first-failure"
          printf '%s\\n' 'bundle command diagnostic API_PASSWORD=plain-password' >&2
          exit 77
        fi
      fi
      if [ "$mode" = "listener-bundle-mismatch" ] && [ "$bundle_mismatch" = true ]; then
        printf '%s\\n' wrong.bundle
      else
        printf '%s\\n' com.facebook.WebDriverAgentRunner.xctrunner
      fi
    fi
  elif [ "$1" = "-convert" ] && [ "$2" = "json" ]; then
    if [ "$mode" = "credential" ] || [ "$mode" = "credential-binary" ] || [ "$mode" = "credential-binary-failure" ]; then
      dd if=/dev/zero bs=4080 count=1 2>/dev/null | tr '\\0' x >&2
      printf ' API_PASSWORD=plain-password PRIVATE_URL=https://private.example/internal/path?ref=private-reference\\n' >&2
    fi
    case "$5" in
      "$STARTUP_TEST_RECEIPT/Info.plist"|/private$STARTUP_TEST_RECEIPT/Info.plist)
        if [ "$mode" = "credential-binary-failure" ]; then
          exit 74
        elif [ "$mode" = "credential-binary" ]; then
          printf '%s\n' '{"CFBundleIdentifier":"com.facebook.WebDriverAgentRunner.xctrunner","PASSWORD":"bare-password-sentinel","API_PASSWORD":"plain-password","PRIVATE_URL":"https://private.example/installed?ref=private-reference"}'
        elif [ "$mode" = "credential" ]; then
          printf '%s\\n' '{"CFBundleIdentifier":"com.facebook.WebDriverAgentRunner.xctrunner","PASSWORD":"bare-password-sentinel","API_PASSWORD":"plain-password","PRIVATE_URL":"https://private.example/installed?ref=private-reference"}'
        else
          cat "$5"
        fi
        ;;
      *) cat "$5" ;;
    esac
  fi
  ;;
lsof)
  if [ "$mode" = "occupied" ]; then
    printf '%s\\n' 999999
    exit 0
  fi
  if [ "$mode" = "listener-status-one-stderr" ] && ! has_arg -d "$@" && has_arg "-iTCP:$IOS_WDA_PORT" "$@"; then
    printf '%s\\n' 'status one diagnostic' >&2
    exit 1
  fi
  if [ "$mode" = "listener-status-one-stderr" ] && ! has_arg -d "$@" && has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@"; then
    for attempt in $(seq 1 50); do [ -e "$root/xcode.pid" ] && break; /bin/sleep 0.01; done
    if [ -e "$root/xcode.pid" ]; then
      printf '%s\\n' "$(cat "$root/xcode.pid")"
      exit 0
    fi
  fi
  if [ "$mode" = "noisy-cleanup" ] && [ -e "$root/cleanup-marker" ] && ! has_arg -d "$@"; then
    dd if=/dev/zero bs=4096 count=1 2>/dev/null | tr '\\0' n >&2
    printf '%s\\n' "$(cat "$root/cleanup-pid")"
    exit 0
  fi
  if [ "$mode" = "status-noisy-first-failure" ] && [ -e "$root/status-queries" ] && ! has_arg -d "$@"; then
    dd if=/dev/zero bs=4096 count=1 1>&2 2>/dev/null
    printf '%s\\n' "$(cat "$root/runner.pid")" 11111 22222 33333
    exit 0
  fi
  if [ ! -e "$root/runner.pid" ] && ! { [ "$mode" = "noisy-cleanup" ] && [ -e "$root/cleanup-marker" ] && has_arg -d "$@"; }; then
    exit 1
  fi
  if [ "$mode" = "listener-invalid-pid" ] && ! has_arg -d "$@"; then
    printf '%s\\n' 2147483648
    exit 0
  fi
  if [ "$mode" = "listener-first-failure" ] && ! has_arg -d "$@"; then
    exit 2
  fi
  if [ "$mode" = "listener-second-failure" ] && ! has_arg -d "$@" && has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@"; then
    printf '%s\\n' 'listener command diagnostic' >&2
    exit 2
  fi
  if { [ "$mode" = "listener-endpoints-disappear" ] || [ "$mode" = "status-evidence-disappear" ]; } && ! has_arg -d "$@" && [ -e "$root/status-queries" ]; then
    printf '%s\\n' 'empty listener diagnostic' >&2
    exit 1
  fi
  if has_arg -d "$@"; then
    requested=
    while [ "$#" -gt 0 ]; do
      if [ "$1" = "-p" ]; then
        requested=$2
        break
      fi
      shift
    done
    [ -n "$requested" ] || requested=$(cat "$root/runner.pid" 2>/dev/null || cat "$root/xcode.pid")
    if [ "$mode" = "status-noisy-first-failure" ] && [ -e "$root/status-queries" ]; then
      dd if=/dev/zero bs=4096 count=1 1>&2 2>/dev/null
    fi
    if [ "$mode" = "listener-hash-after-freeze" ]; then
      count=0
      [ -e "$root/listener-evidence-count" ] && count=$(cat "$root/listener-evidence-count")
      printf '%s' "$((count + 1))" > "$root/listener-evidence-count"
      if [ "$count" -ge 1 ]; then
        printf '%s' 'different listener after freeze' > "$STARTUP_TEST_RUNNER_RECEIPT/WebDriverAgentRunner-Runner"
      fi
    fi
    executable=$STARTUP_TEST_RUNNER_RECEIPT/WebDriverAgentRunner-Runner
    { [ "$mode" = "credential" ] || [ "$mode" = "credential-binary" ] || [ "$mode" = "credential-binary-failure" ]; } && executable=$STARTUP_TEST_RUNNER_RECEIPT/unexpected-listener
    printf 'p%s\\nftxt\\nn%s\\n' "$requested" "$executable"
    exit 0
  fi
  if [ "$mode" = "listener-mjpeg-duplicate" ] && has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@"; then
    printf '11111\\n22222\\n'
  elif [ "$mode" = "swap" ] || [ "$mode" = "pid-reuse" ]; then
    count=0
    [ -e "$root/swap-lsof-count" ] && count=$(cat "$root/swap-lsof-count")
    printf '%s' "$((count + 1))" > "$root/swap-lsof-count"
    if [ "$mode" = "pid-reuse" ] || [ "$count" -lt 4 ]; then
      printf '%s\\n' 11111
    else
      printf '%s\\n' 22222
    fi
  else
    printf '%s\\n' "$(cat "$root/runner.pid")"
  fi
  ;;
ps)
  pid=$2
  format=$4
  if [ "$mode" = "status-noisy-first-failure" ] && [ -e "$root/status-queries" ]; then
    dd if=/dev/zero bs=4096 count=1 1>&2 2>/dev/null
  fi
  xcode_pid=
  [ -e "$root/xcode.pid" ] && xcode_pid=$(cat "$root/xcode.pid")
  if { [ "$mode" = "swap" ] || [ "$mode" = "pid-reuse" ]; } && [ "$pid" != "$xcode_pid" ]; then
    generation=0
    [ -e "$root/swap-lsof-count" ] && generation=$(cat "$root/swap-lsof-count")
    reused=false
    [ "$mode" = "pid-reuse" ] && [ "$generation" -ge 5 ] && reused=true
    if [ "$format" = "lstart=" ]; then
      if [ "$reused" = true ] || [ "$pid" = 22222 ]; then
        printf '%s\\n' birth-b
      else
        printf '%s\\n' birth-a
      fi
    else
      printf 'WebDriverAgentRunner-Runner %s\\n' "$pid"
    fi
  elif [ "$format" = "comm=" ]; then
    printf '%s\\n' "$STARTUP_TEST_RECEIPT/WebDriverAgentRunner-Runner"
  elif [ "$mode" = "credential" ] || [ "$mode" = "credential-binary" ] || [ "$mode" = "credential-binary-failure" ]; then
    printf 'xcodebuild test-without-building -xctestrun %s -destination id=%s https://example.test/?token=credential-token\\n' "$STARTUP_TEST_XCTESTRUN" "$IOS_SIMULATOR_UDID"
  else
    printf 'xcodebuild test-without-building -xctestrun %s -destination id=%s\\n' "$STARTUP_TEST_XCTESTRUN" "$IOS_SIMULATOR_UDID"
  fi
  ;;
xcrun)
  if [ "$2" = "list" ]; then
    printf '%s\\n' "$IOS_SIMULATOR_UDID"
  elif [ "$2" = "get_app_container" ]; then
    count=0
    [ -e "$root/receipt-queries" ] && count=$(cat "$root/receipt-queries")
    printf '%s' "$((count + 1))" > "$root/receipt-queries"
    if [ "$mode" = "receipt-failure" ] && [ "$count" -ge 1 ]; then
      exit 77
    elif [ "$mode" = "receipt-missing-then-valid" ] && [ "$count" -eq 1 ]; then
      printf '%s\\n' "$STARTUP_TEST_RUNNER_RECEIPT/missing-receipt"
    else
      printf '%s\\n' "$STARTUP_TEST_RECEIPT"
    fi
  elif [ "$2" = "spawn" ]; then
    printf '%s\\n' 'retained simulator diagnostic'
  elif [ "$2" = "terminate" ] && [ "$mode" = "noisy-cleanup" ]; then
    cat "$root/runner.pid" > "$root/cleanup-pid"
    touch "$root/cleanup-marker"
  fi
  ;;
xcodebuild)
  exec "$STARTUP_TEST_EXECUTABLE" "$STARTUP_TEST_WDA_SERVER" xcodebuild "$@"
  ;;
esac
exit 0
`;
const wdaServer = `import { createServer } from 'node:http';
import { spawn } from 'node:child_process';
import { appendFileSync, existsSync, readFileSync, unlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
const root = process.env.STARTUP_TEST_ROOT;
const mode = process.env.STARTUP_TEST_MODE;
const args = process.argv.slice(3);
const runnerPid = join(root, 'runner.pid');
const xcodePid = join(root, 'xcode.pid');
const remove = path => { try { unlinkSync(path); } catch {} };
appendFileSync(join(root, 'launches'), JSON.stringify(args) + '\\n');
if (mode === 'early') process.exit(43);
if (mode === 'exit-before-close') {
  spawn(process.execPath, ['-e', 'setTimeout(() => {}, 500)'], { stdio: ['ignore', 'inherit', 'inherit'] });
  process.exit(44);
}
if (mode === 'receipt-product-changed') {
  writeFileSync(join(process.env.STARTUP_TEST_PRODUCT, 'WebDriverAgentRunner-Runner'), 'changed');
  writeFileSync(join(root, 'product-changed-at'), String(Date.now()));
}
writeFileSync(xcodePid, String(process.pid));
if (mode === 'credential' || mode === 'credential-binary' || mode === 'credential-binary-failure') {
  process.stdout.write('PASSWORD=bare-password-sentinel API_PASSWORD=plain-password PRIVATE_URL=https://private.example/internal/path?ref=private-reference\\n');
  process.stderr.write('{"PASSWORD":"bare-password-sentinel","api_token":"plain-password","private_url":"https://private.example/internal/path"}\\n');
}
if (mode === 'listener-status-one-stderr') writeFileSync(runnerPid, String(process.pid));
const server = createServer((_request, response) => {
  const statusPath = join(root, 'status-queries');
  const count = existsSync(statusPath) ? Number(readFileSync(statusPath, 'utf8')) : 0;
  writeFileSync(statusPath, String(count + 1));
  const body = mode === 'status-forged-markers'
    ? { payload: 'forged-status-payload', classification: 'forged-status-classification', note: 'status-opaque-secret', PASSWORD: 'status-password-sentinel', PRIVATE_URL: 'https://status.private.example/internal' }
    : { value: { ready: mode !== 'invalid', state: 'success', build: { version: '16.12.1', productBundleIdentifier: 'com.facebook.WebDriverAgentRunner' }, os: { version: '18.6' } } };
  const oversized = mode === 'oversized' || (mode === 'ready-then-oversized' && count >= 1);
  const responseBody = mode === 'status-noisy-first-failure' ? '\\0'.repeat(64000) : JSON.stringify(body) + (oversized ? ' '.repeat(70000) : '');
  response.setHeader('content-type', 'application/json');
  response.end(responseBody);
});
const outputBytes = {stdout: 0, stderr: 0};
const writeChunk = async (stream, chunk) => {
  outputBytes[stream === process.stdout ? 'stdout' : 'stderr'] += chunk.length;
  if (!stream.write(chunk)) await new Promise(resolve => stream.once('drain', resolve));
};
const writeFilled = async (stream, total, fill = 'x') => {
  while (total > 0) {
    const length = Math.min(total, 1024 * 1024);
    const chunk = Buffer.alloc(length, fill);
    if (fill === 'x' && length > 0) chunk[length - 1] = 10;
    await writeChunk(stream, chunk);
    total -= length;
  }
};
const writeRepeated = async (stream, total, line) => {
  const bytes = Buffer.from(line);
  while (total > 0) {
    const length = Math.min(total, 1024 * 1024);
    const chunk = Buffer.alloc(length);
    for (let index = 0; index < length; index += 1) chunk[index] = bytes[index % bytes.length];
    await writeChunk(stream, chunk);
    total -= length;
  }
};
const outputModes = ['stream-framing', 'stream-framing-reversed', 'stream-utf8', 'stream-eof', 'stream-finalization-failure', 'output-below', 'output-equal', 'output-above', 'output-combined', 'output-shrinking', 'output-expanding'];
const emitFixtureOutput = async () => {
  const limit = 104857600;
  if (mode === 'stream-framing' || mode === 'stream-finalization-failure') {
    await writeChunk(process.stdout, Buffer.from('PASSWORD='));
    await writeChunk(process.stderr, Buffer.from('harmless stderr\\n'));
    await writeChunk(process.stdout, Buffer.from('stream-secret\\n'));
    await writeChunk(process.stdout, Buffer.from('PRIVATE_URL=https://private.example/'));
    await writeChunk(process.stderr, Buffer.from('safe stderr\\n'));
    await writeChunk(process.stdout, Buffer.from('internal/path?ref=fragment\\n'));
    await writeChunk(process.stdout, Buffer.from('<plist version="1.0"><dict><key>API_PASSWORD</key>\\n'));
    await writeChunk(process.stderr, Buffer.from('intervening diagnostic\\n'));
    await writeChunk(process.stdout, Buffer.from('<string>multiline-xml-secret</string></dict></plist>\\n'));
    await writeChunk(process.stderr, Buffer.from('PASSWORD="escaped ' + '\\\\' + '"escaped-quote-secret"\\n'));
  } else if (mode === 'stream-framing-reversed') {
    await writeChunk(process.stderr, Buffer.from('API_PASSWORD='));
    await writeChunk(process.stdout, Buffer.from('harmless stdout\\n'));
    await writeChunk(process.stderr, Buffer.from('reverse-secret\\n'));
    await writeChunk(process.stderr, Buffer.from('PRIVATE_URL=https://private.example/'));
    await writeChunk(process.stdout, Buffer.from('safe stdout\\n'));
    await writeChunk(process.stderr, Buffer.from('reverse/path?ref=fragment\\n'));
  } else if (mode === 'stream-utf8') {
    await writeChunk(process.stdout, Buffer.from('PASSWORD=utf8-secret-'));
    const unicode = Buffer.from('秘密\\n');
    await writeChunk(process.stdout, unicode.subarray(0, 2));
    await writeChunk(process.stdout, unicode.subarray(2));
    await writeChunk(process.stderr, Buffer.from('safe-utf8-π\\n'));
  } else if (mode === 'stream-eof') {
    await writeChunk(process.stdout, Buffer.from('PASSWORD=eof-secret'));
    await writeChunk(process.stderr, Buffer.from('safe eof\\n'));
  } else if (mode === 'output-below') {
    await writeFilled(process.stdout, limit - 1);
  } else if (mode === 'output-equal') {
    await writeFilled(process.stdout, limit);
  } else if (mode === 'output-above') {
    await writeFilled(process.stdout, limit);
    writeFileSync(join(root, 'output-ready'), JSON.stringify({mode, limit, outputBytes}));
    while (!existsSync(join(root, 'output-continue'))) await new Promise(resolve => setTimeout(resolve, 5));
    await writeChunk(process.stdout, Buffer.from('z'));
    return;
  } else if (mode === 'output-combined') {
    await writeFilled(process.stdout, Math.floor(limit / 2));
    await writeFilled(process.stderr, limit - Math.floor(limit / 2));
    writeFileSync(join(root, 'output-ready'), JSON.stringify({mode, limit, outputBytes}));
    while (!existsSync(join(root, 'output-continue'))) await new Promise(resolve => setTimeout(resolve, 5));
    await writeChunk(process.stderr, Buffer.from('z'));
    return;
  } else if (mode === 'output-shrinking') {
    await writeRepeated(process.stdout, limit - 1, 'PASSWORD=shrinking-secret' + '~'.repeat(65510) + '\\n');
  } else if (mode === 'output-expanding') {
    await writeRepeated(process.stdout, limit - 1, 'http://x '.repeat(7000) + '~'.repeat(2535) + '\\n');
  }
  writeFileSync(join(root, 'output-ready'), JSON.stringify({mode, limit, outputBytes}));
};
let keepAlive;
let stopping = false;
const stop = () => {
  if (stopping) return;
  stopping = true;
  if (keepAlive) clearInterval(keepAlive);
  server.closeAllConnections?.();
  if (!server.listening) process.exit(0);
  server.close(() => process.exit(0));
};
process.on('SIGTERM', stop);
process.on('SIGINT', stop);
process.on('exit', () => { remove(runnerPid); remove(xcodePid); });
if (outputModes.includes(mode)) {
  keepAlive = setInterval(() => {}, 1000);
  emitFixtureOutput().then(() => {
    if (mode === 'stream-eof') clearInterval(keepAlive);
  }).catch(error => { process.stderr.write(String(error)); process.exit(1); });
} else {
  server.listen(Number(process.env.IOS_WDA_PORT), '127.0.0.1', () => writeFileSync(runnerPid, String(process.pid)));
}
`;
const pause = () => new Promise(resolve => setTimeout(resolve, 25));

const waitForOutputHandshake = async (state: string, root: string, timeout: number, requireRunning = true): Promise<Record<string, unknown>> => {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (existsSync(join(root, 'output-ready')) && existsSync(join(state, 'owner.json'))) {
      const handshake = JSON.parse(await readFile(join(root, 'output-ready'), 'utf8')) as Record<string, unknown>;
      const owner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8')) as { receivedLogBytes?: number; endedAt?: string };
      const output = handshake.outputBytes as { stdout?: number; stderr?: number } | undefined;
      const expected = (output?.stdout || 0) + (output?.stderr || 0);
      if ((owner.receivedLogBytes || 0) >= expected && (!requireRunning || !owner.endedAt)) return handshake;
    }
    await pause();
  }
  throw new Error('test output handshake deadline');
};

const exportBlock = (source: string): string => {
  const lines = source.split('\n');
  const marker = lines.findIndex(line => line.includes('name: Sanitize bounded diagnostics'));
  const run = lines.findIndex((line, index) => index > marker && line.trim() === 'run: |');
  const runIndent = lines[run].match(/^\s*/u)?.[0].length || 0;
  const shell = lines.findIndex((line, index) => index > run && line.trim() === 'shell: bash');
  const nextStep = lines.findIndex((line, index) => index > run
    && line.trimStart().startsWith('- name:')
    && (line.match(/^\s*/u)?.[0].length || 0) === runIndent - 2);
  const end = shell >= 0 ? shell : nextStep;
  const body = lines.slice(run + 1, end);
  const indent = body.find(line => line.trim())?.match(/^\s*/u)?.[0].length || 0;
  return body.map(line => line.slice(indent)).join('\n');
};

const runExportBlock = (script: string, cwd: string, env: NodeJS.ProcessEnv): Promise<{ code: number | null; stderr: string }> => new Promise((resolve, reject) => {
  const child = spawn('bash', ['-euo', 'pipefail', '-c', script], {cwd, env, stdio: ['ignore', 'ignore', 'pipe']});
  let stderr = '';
  child.stderr.on('data', chunk => { stderr += chunk.toString(); });
  child.on('error', reject);
  child.on('close', code => resolve({code, stderr}));
});

const assertSanitizedExports = async (root: string, state: string, forbidden: string[], requiredArtifacts: string[] = []): Promise<void> => {
  const repo = join(import.meta.dirname, '../../..');
  const output = join(root, 'export-output');
  const runnerTemp = join(root, 'export-runner-temp');
  await mkdir(runnerTemp, { recursive: true });
  for (const path of ['.github/workflows/mobile-ci.yml', '.github/actions/mobile-device-run/action.yml']) {
    await rm(output, { recursive: true, force: true });
    const source = await readFile(join(repo, path), 'utf8');
    const result = await runExportBlock(exportBlock(source), repo, {
      ...process.env,
      MOBILE_PLATFORM: 'ios',
      IOS_XCTEST_STATE_DIR: state,
      MOBILE_OUTPUT: output,
      RUNNER_TEMP: runnerTemp,
    });
    assert.equal(result.code, 0, result.stderr);
    for (const artifact of requiredArtifacts) {
      const artifactPath = join(output, `ios-xctest/${artifact}`);
      assert.equal(existsSync(artifactPath), true, `${path} missing ${artifact}`);
      assert.ok((await readFile(artifactPath, 'utf8')).length > 0, `${path} empty ${artifact}`);
    }
    for (const artifact of [...new Set(requiredArtifacts)]) {
      const exported = await readFile(join(output, `ios-xctest/${artifact}`), 'utf8');
      for (const value of forbidden) assert.equal(exported.includes(value), false, `${path} ${artifact} leaked ${value}`);
    }
  }
};

const assertReadyStatus = (candidate: unknown): void => {
  assert.ok(candidate && typeof candidate === 'object');
  const status = candidate as { statusCode?: number; bytes?: number; body?: Record<string, unknown> };
  assert.equal(status.statusCode, 200);
  assert.ok(Number.isInteger(status.bytes));
  assert.deepEqual(status.body, {
    classification: 'wda-status',
    ready: true,
    state: 'success',
    buildVersion: '16.12.1',
    productBundleIdentifier: 'com.facebook.WebDriverAgentRunner',
    osVersion: '18.6',
    payload: 'suppressed',
    diagnostic: 'suppressed',
    diagnosticBytes: status.bytes,
    diagnosticTruncated: false,
    bodyBytes: status.bytes,
  });
};

export const xctestOwnerTests: Array<[string, () => Promise<void>]> = [];
for (const mode of ['ready', 'ready-then-oversized', 'early', 'exit-before-close', 'invalid', 'occupied', 'ownership', 'ambiguous', 'product', 'receipt-failure', 'receipt-missing-then-valid', 'receipt-product-changed', 'swap', 'pid-reuse', 'oversized', 'delayed', 'credential', 'credential-binary', 'credential-binary-failure', 'stream-framing', 'stream-framing-reversed', 'stream-utf8', 'stream-eof', 'stream-finalization-failure', 'output-below', 'output-equal', 'output-above', 'output-combined', 'output-shrinking', 'output-expanding', 'noisy-cleanup', 'status-noisy-first-failure', 'status-forged-markers', 'status-evidence-disappear', 'listener-mjpeg-duplicate', 'listener-bundle-mismatch', 'listener-hash-mismatch', 'listener-invalid-pid', 'listener-first-failure', 'listener-status-one-stderr', 'listener-second-failure', 'listener-endpoints-disappear', 'listener-hash-after-freeze']) {
  xctestOwnerTests.push([`Native startup actual XCTest supervisor ${mode}`, async () => {
    const root = await mkdtemp(join(tmpdir(), 'herdr-xctest-test-'));
    const udid = '82342155-D8BD-4C4D-BD5E-1EDCDF9CFB40';
    const product = join(root, 'products/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app');
    const receipt = join(root, `Devices/${udid}/data/Containers/Bundle/Application/10C0E9C0-50FD-4C3E-AC55-AC1158A00568/WebDriverAgentRunner-Runner.app`);
    const runnerReceipt = join(root, `Devices/${udid}/data/Containers/Bundle/Application/1C1FB5F7-2D0E-49AC-BE09-62816F229E5C/WebDriverAgentRunner-Runner.app`);
    const state = join(root, 'state');
    const bin = join(root, 'bin');
    const xctestrun = join(root, 'products/WebDriverAgentRunner_test.xctestrun');
    await Promise.all([mkdir(bin), mkdir(state), mkdir(join(product, 'PlugIns/WebDriverAgentRunner.xctest'), { recursive: true }), mkdir(receipt, { recursive: true }), mkdir(runnerReceipt, { recursive: true }), mkdir(join(root, 'wda'))]);
    for (const dir of [product, receipt, runnerReceipt]) {
      const info: Record<string, string> = { CFBundleIdentifier: 'com.facebook.WebDriverAgentRunner.xctrunner' };
      if ((mode === 'credential' || mode === 'credential-binary' || mode === 'credential-binary-failure') && dir === receipt) {
        await writeFile(join(dir, 'Info.plist'), `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
  <dict>
    <key>CFBundleIdentifier</key>
    <string>${info.CFBundleIdentifier}</string>
    <key>PASSWORD</key>
    <string>bare-password-sentinel</string>
    <key>API_PASSWORD</key>
    <string>
      plain-password
    </string>
    <key>PRIVATE_URL</key>
    <string>https://private.example/installed?ref=private-reference</string>
    <key>UNTRUSTED_VALUE</key>
    <string>multiline-xml-secret</string>
  </dict>
</plist>
`);
      } else {
        await writeFile(join(dir, 'Info.plist'), JSON.stringify(info));
      }
      await writeFile(join(dir, 'WebDriverAgentRunner-Runner'), mode === 'listener-hash-mismatch' && dir === runnerReceipt ? 'different listener' : 'exact built runner');
    }
    if (mode === 'credential-binary' || mode === 'credential-binary-failure') {
      await writeFile(join(receipt, 'Info.plist'), Buffer.from(credentialBinaryPlistBase64, 'base64'));
      if (process.platform === 'darwin') {
        const converted = spawnSync('/usr/bin/plutil', ['-convert', 'json', '-o', '-', join(receipt, 'Info.plist')], { encoding: 'utf8' });
        assert.equal(converted.status, 0, converted.stderr || 'Apple plutil binary fixture verification failed');
        assert.equal(JSON.parse(converted.stdout).CFBundleIdentifier, 'com.facebook.WebDriverAgentRunner.xctrunner');
      }
    }
    await writeFile(join(root, 'wda/package.json'), JSON.stringify({ version: '16.12.1' }));
    const xctestrunData: { WebDriverAgentRunner: Record<string, unknown> } = { WebDriverAgentRunner: {
      TestHostBundleIdentifier: 'com.facebook.WebDriverAgentRunner.xctrunner',
      TestBundlePath: '__TESTHOST__/PlugIns/WebDriverAgentRunner.xctest',
      TestHostPath: '__TESTROOT__/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app',
    } };
    if (mode === 'credential' || mode === 'credential-binary' || mode === 'credential-binary-failure') xctestrunData.WebDriverAgentRunner.EnvironmentVariables = {
      FILLER: 'x'.repeat(4096),
      PASSWORD: 'bare-password-sentinel',
      API_PASSWORD: 'plain-password',
      PRIVATE_URL: 'https://private.example/internal/path?ref=private-reference',
      ENV_VALUE: 'ordinary-environment-secret',
    };
    await writeFile(xctestrun, JSON.stringify(xctestrunData));
    if (mode === 'status-evidence-disappear') await mkdir(join(state, 'wda-status.json'));
    if (mode === 'stream-finalization-failure') await mkdir(join(root, 'blocked-log-target'));
    if (mode === 'ambiguous') await writeFile(join(root, 'products/WebDriverAgentRunner_second.xctestrun'), await readFile(xctestrun));
    if (mode === 'product') await writeFile(join(product, 'Info.plist'), JSON.stringify({ CFBundleIdentifier: 'wrong' }));
    if (mode === 'credential' || mode === 'credential-binary' || mode === 'credential-binary-failure') await writeFile(join(runnerReceipt, 'unexpected-listener'), 'unexpected listener');
    await writeFile(join(root, 'owned'), `ios:${mode === 'ownership' ? 'wrong' : udid}`);
    const dispatcher = join(bin, 'xctest-command-dispatcher');
    await writeFile(dispatcher, shim, { mode: 0o700 });
    for (const cmd of ['plutil', 'lsof', 'ps', 'xcrun', 'xcodebuild']) await symlink(dispatcher, join(bin, cmd));
    const wdaServerFile = join(bin, 'xctest-wda-server.ts');
    await writeFile(wdaServerFile, wdaServer, { mode: 0o700 });
    const reserve = createNetServer();
    await new Promise<void>((resolve, reject) => {
      reserve.once('error', reject);
      reserve.listen(0, '127.0.0.1', resolve);
    });
    const port = (reserve.address() as AddressInfo).port;
    await new Promise<void>((resolve, reject) => reserve.close(error => error ? reject(error) : resolve()));
    const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, STARTUP_TEST_ROOT: root, STARTUP_TEST_MODE: mode, STARTUP_TEST_EXECUTABLE: process.execPath, STARTUP_TEST_WDA_SERVER: wdaServerFile,
      STARTUP_TEST_RECEIPT: receipt, STARTUP_TEST_RUNNER_RECEIPT: runnerReceipt, STARTUP_TEST_PRODUCT: product, STARTUP_TEST_XCTESTRUN: xctestrun, IOS_XCTEST_STATE_DIR: state,
      IOS_SIMULATOR_UDID: udid, IOS_PLATFORM_VERSION: '18.6', IOS_WDA_PORT: String(port), IOS_WDA_MJPEG_PORT: String(port === 65535 ? port - 1 : port + 1),
      IOS_WDA_PREBUILT_PATH: product, IOS_WDA_BOOTSTRAP_PATH: join(root, 'products'), IOS_WDA_AGENT_PATH: join(root, 'wda/WebDriverAgent.xcodeproj'),
      MOBILE_DEVICE_OWNERSHIP_FILE: join(root, 'owned'),
      ...(mode === 'stream-finalization-failure' ? { STARTUP_TEST_LOG_FINALIZATION_TARGET: join(root, 'blocked-log-target') } : {}) };
    const initialProductHash = createHash('sha256').update(await readFile(join(product, 'WebDriverAgentRunner-Runner'))).digest('hex');
    const deadline = Date.now() + (mode === 'invalid' || mode === 'oversized' ? 2000 : mode === 'delayed' ? 1500 : 10_000);
    await writeFile(join(state, 'deadline'), String(deadline));
    const child = spawn(process.execPath, [join(import.meta.dirname, '../support/ios-xctest.ts'), 'supervise'], { env, stdio: ['ignore', 'ignore', 'pipe'] });
    const supervisorStarted = Date.now();
    let errors = '';
    child.stderr.on('data', chunk => { errors += chunk.toString(); });
    let done = false;
    let assertionsPassed = false;
    const exited = new Promise<void>(resolve => child.on('close', () => { done = true; resolve(); }));
    try {
      const credentialMode = ['credential', 'credential-binary', 'credential-binary-failure'].includes(mode);
      const streamMode = ['stream-framing', 'stream-framing-reversed', 'stream-utf8', 'stream-eof', 'stream-finalization-failure'].includes(mode);
      const outputBoundaryMode = ['output-below', 'output-equal', 'output-above', 'output-combined', 'output-shrinking', 'output-expanding'].includes(mode);
      if (credentialMode) {
        while (!done && Date.now() < deadline && !existsSync(join(state, 'wda-preflight-private.log'))) await pause();
        assert.equal(existsSync(join(state, 'owner.json')), true);
        assert.equal(existsSync(join(state, 'selected.xctestrun')), true);
        assert.equal(existsSync(join(state, 'installed-Info.plist')), true);
        assert.equal(existsSync(join(state, 'wda-preflight-private.log')), true);
        await assertSanitizedExports(root, state, ['plain-password', 'bare-password-sentinel', 'private-reference', 'https://private.example', 'https://example.test', 'multiline-xml-secret', 'escaped-quote-secret', 'ordinary-environment-secret'], ['owner.json', 'selected.xctestrun', 'installed-Info.plist', 'wda-preflight.log']);
      }
      if (mode === 'status-forged-markers') {
        const forbidden = ['status-opaque-secret', 'status-password-sentinel', 'https://status.private.example'];
        while (!done && Date.now() < deadline && !existsSync(join(state, 'wda-status.json'))) await pause();
        const unfinishedOwner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8')) as { status?: { statusCode?: number; bytes?: number; body?: Record<string, unknown> } };
        const unfinishedPrivateOwner = JSON.parse(await readFile(join(state, 'owner-private.json'), 'utf8')) as { status?: { statusCode?: number; bytes?: number; body?: Record<string, unknown> } };
        const unfinishedStatus = JSON.parse(await readFile(join(state, 'wda-status.json'), 'utf8')) as { statusCode?: number; bytes?: number; body?: Record<string, unknown> };
        const assertAllowlistedStatus = (evidence: { statusCode?: number; bytes?: number; body?: Record<string, unknown> }): void => {
          assert.equal(evidence.statusCode, 200);
          assert.ok(Number.isInteger(evidence.bytes));
          assert.deepEqual(evidence.body, {
            classification: 'unrecognized',
            ready: 'not-evaluated',
            state: 'unrecognized',
            buildVersion: 'unrecognized',
            productBundleIdentifier: 'unrecognized',
            osVersion: 'unrecognized',
            payload: 'suppressed',
            diagnostic: 'suppressed',
            diagnosticBytes: evidence.bytes,
            diagnosticTruncated: false,
            bodyBytes: evidence.bytes,
          });
        };
        for (const evidence of [unfinishedOwner.status, unfinishedPrivateOwner.status, unfinishedStatus]) {
          assertAllowlistedStatus(evidence || {});
          assert.equal(JSON.stringify(evidence).includes('status-opaque-secret'), false);
          assert.equal(JSON.stringify(evidence).includes('status-password-sentinel'), false);
          assert.equal(JSON.stringify(evidence).includes('status.private.example'), false);
        }
        await assertSanitizedExports(root, state, forbidden, ['owner.json', 'wda-status.json']);
        await writeFile(join(state, 'stop'), 'forged status fixture finalization');
      }
      if (streamMode) {
        const handshake = await waitForOutputHandshake(state, root, 15_000, mode !== 'stream-eof');
        const output = handshake.outputBytes as { stdout: number; stderr: number };
        const ownerBeforeStop = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8')) as { endedAt?: string; receivedLogBytes?: number; receivedStreamBytes?: { stdout: number; stderr: number }; diagnostics?: { lifecycle: Array<{ event: string; detail?: { stream?: string; offset?: number; bytes?: number } }> } };
        if (mode !== 'stream-eof') assert.equal(ownerBeforeStop.endedAt, undefined);
        assert.equal(ownerBeforeStop.receivedLogBytes, output.stdout + output.stderr);
        assert.deepEqual(ownerBeforeStop.receivedStreamBytes, output);
        for (const stream of ['stdout', 'stderr']) {
          const events = (ownerBeforeStop.diagnostics?.lifecycle || []).filter(event => event.event === 'child-output' && event.detail?.stream === stream);
          assert.ok(events.length > 0);
          assert.ok(events.every(event => Number.isInteger(event.detail?.offset) && Number.isInteger(event.detail?.bytes)));
        }
        const forbidden = ['stream-secret', 'reverse-secret', 'utf8-secret', '秘密', 'eof-secret', 'fragment', 'private.example', 'multiline-xml-secret', 'escaped-quote-secret'];
        const privateLog = await readFile(join(state, 'wda-preflight-private.log'), 'utf8');
        for (const value of forbidden) assert.equal(privateLog.includes(value), false, `private log leaked ${value}`);
        assert.match(privateLog, /startup output (?:suppressed|summary)/u);
        await assertSanitizedExports(root, state, forbidden, ['owner.json', 'selected.xctestrun', 'installed-Info.plist', 'wda-preflight.log']);
        await writeFile(join(state, 'stop'), 'stream fixture finalization');
      }
      if (outputBoundaryMode) {
        const handshake = await waitForOutputHandshake(state, root, 15_000, false) as { outputBytes: { stdout: number; stderr: number } };
        const output = handshake.outputBytes;
        const ownerOutput = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8')) as { endedAt?: string; receivedLogBytes?: number; receivedStreamBytes?: { stdout: number; stderr: number }; diagnostics?: { lifecycle: Array<{ event: string; detail?: { stream?: string; offset?: number; bytes?: number } }> } };
        assert.ok((ownerOutput.receivedLogBytes || 0) >= output.stdout + output.stderr);
        assert.ok(ownerOutput.receivedStreamBytes);
        for (const stream of ['stdout', 'stderr']) {
          const events = (ownerOutput.diagnostics?.lifecycle || []).filter(event => event.event === 'child-output' && event.detail?.stream === stream);
          if (output[stream as 'stdout' | 'stderr'] > 0) {
            assert.ok((ownerOutput.receivedStreamBytes?.[stream as 'stdout' | 'stderr'] || 0) >= output[stream as 'stdout' | 'stderr']);
            if (events.length) assert.ok(events.every(event => Number.isInteger(event.detail?.offset) && Number.isInteger(event.detail?.bytes)));
          }
        }
        if (mode === 'output-above' || mode === 'output-combined') await writeFile(join(root, 'output-continue'), 'cross raw boundary');
        else if (!ownerOutput.endedAt) await writeFile(join(state, 'stop'), 'output boundary fixture finalization');
      }
      if (mode === 'ready' || mode === 'ready-then-oversized') {
        while (Date.now() < deadline && !done) {
          if (existsSync(join(state, 'owner.json')) && JSON.parse(await readFile(join(state, 'owner.json'), 'utf8')).ready) break;
          await pause();
        }
        const owner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
        assert.ok(owner.ready, errors || owner.error);
        if (mode === 'ready') {
          const privateOwner = JSON.parse(await readFile(join(state, 'owner-private.json'), 'utf8'));
          const standaloneStatus = JSON.parse(await readFile(join(state, 'wda-status.json'), 'utf8'));
          for (const status of [owner.status, privateOwner.status, standaloneStatus]) assertReadyStatus(status);
        }
        const previous = { ...process.env };
        Object.assign(process.env, env);
        try {
          if (mode === 'ready-then-oversized') {
            await assert.rejects(managedWdaCapabilities(udid), /managed WDA not ready/u);
          } else {
            assert.deepEqual(await managedWdaCapabilities(udid), { 'appium:webDriverAgentUrl': `http://127.0.0.1:${port}` });
            const platform = new IOSPlatform({ deviceId: udid, origin: 'https://example.test', setupUrl: 'https://example.test/setup', outputDir: root, appiumUrl: 'http://127.0.0.1:1', certificate: '/unused' });
            let sessions = 0;
            platform.driver.create = async options => {
              sessions++;
              assert.equal(options.capabilities['appium:webDriverAgentUrl'], `http://127.0.0.1:${port}`);
              assert.equal(options.capabilities['appium:platformVersion'], '18.6');
              assert.equal(options.capabilities['appium:usePreinstalledWDA'], undefined);
              assert.equal(options.capabilities['appium:prebuiltWDAPath'], undefined);
              throw new Error('session request captured');
            };
            await assert.rejects(platform.startFreshDevice(), /session request captured/u);
            assert.equal(sessions, 1);
          }
        }
        finally { for (const key of Object.keys(process.env)) if (!(key in previous)) delete process.env[key]; Object.assign(process.env, previous); }
        await writeFile(join(state, 'stop'), 'test finalization');
      }
      await Promise.race([exited, new Promise((_, reject) => setTimeout(() => reject(new Error('test supervisor deadline')), 15_000))]);
      const owner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
      assert.ok(owner.endedAt);
      assert.equal(owner.ready, false);
      assert.ok(Buffer.byteLength(await readFile(join(state, 'owner.json'), 'utf8')) <= 1048576);
      assert.ok(Buffer.byteLength(await readFile(join(state, 'owner-private.json'), 'utf8')) <= 1048576);
      if (mode === 'ready') {
        const privateOwner = JSON.parse(await readFile(join(state, 'owner-private.json'), 'utf8'));
        const standaloneStatus = JSON.parse(await readFile(join(state, 'wda-status.json'), 'utf8'));
        for (const status of [owner.status, privateOwner.status, standaloneStatus]) assertReadyStatus(status);
      }
      assert.equal(owner.diagnostics?.schema, 1);
      assert.equal(owner.diagnostics?.clock?.kind, 'process-relative-monotonic');
      assert.ok(owner.diagnostics?.commands.length <= 128);
      assert.ok(owner.diagnostics?.inspections.length <= 8);
      assert.ok(owner.diagnostics?.lifecycle.length <= 128);
      for (const command of owner.diagnostics?.commands || []) {
        assert.ok(command.startedAt);
        assert.ok(command.endedAt);
        assert.ok(Date.parse(command.endedAt) >= Date.parse(command.startedAt));
        assert.ok(command.monotonicEndMs >= command.monotonicStartMs);
        assert.ok(command.durationMs >= 0);
        assert.equal(command.source, 'tests/mobile/support/ios-xctest.ts');
        assert.ok(command.timeoutMs > 0);
        assert.ok(command.stdout.content.length <= 4096);
        assert.ok(command.stderr.content.length <= 4096);
        assert.ok(!command.stdout.content.includes('credential-token'));
        assert.ok(!command.stderr.content.includes('credential-token'));
        assert.ok(!command.stdout.content.includes('plain-password'));
        assert.ok(!command.stderr.content.includes('plain-password'));
        assert.ok(!command.stdout.content.includes('bare-password-sentinel'));
        assert.ok(!command.stderr.content.includes('bare-password-sentinel'));
        assert.ok(!command.stdout.content.includes('private-reference'));
        assert.ok(!command.stderr.content.includes('private-reference'));
      }
      for (const inspection of owner.diagnostics?.inspections || []) {
        assert.ok(Date.parse(inspection.endedAt) >= Date.parse(inspection.startedAt));
        assert.ok(inspection.monotonicEndMs >= inspection.monotonicStartMs);
        assert.ok(inspection.durationMs >= 0);
      }
      const lifecycle = owner.diagnostics?.lifecycle || [];
      const cleanupEntry = lifecycle.find((event: { event: string }) => event.event === 'cleanup-enter');
      const endedEntry = lifecycle.find((event: { event: string }) => event.event === 'owner-ended');
      assert.ok(cleanupEntry && endedEntry && cleanupEntry.id < endedEntry.id);
      if (!['ready', 'ready-then-oversized', 'stream-framing', 'stream-framing-reversed', 'stream-utf8', 'stream-eof', 'output-below', 'output-equal', 'output-shrinking', 'output-expanding'].includes(mode)) assert.ok(owner.firstFailure);
      if (owner.firstFailure) assert.ok(Array.isArray(owner.firstFailure.causalCommands));
      if (mode === 'exit-before-close') {
        const childExit = lifecycle.find((event: { event: string }) => event.event === 'child-exit');
        const childClose = lifecycle.find((event: { event: string }) => event.event === 'child-close');
        assert.ok(childExit && childClose && childExit.id < childClose.id && childExit.id < cleanupEntry.id);
        assert.ok(childClose.monotonicMs - childExit.monotonicMs >= 300);
        assert.equal((childExit.detail as { exitCode: number }).exitCode, 44);
        assert.equal(lifecycle.some((event: { event: string }) => event.event === 'child-signal-requested'), false);
      }
      if (mode === 'listener-status-one-stderr') {
        assert.equal(owner.firstFailure.stage, 'wda-pid-count');
        assert.equal(owner.firstFailure.frozenOwner, false);
        assert.equal(owner.firstFailure.predicate.status, 'evaluated');
        assert.equal(owner.firstFailure.predicate.listenerValidation.endpoints.wda.count, 0);
        assert.equal(owner.firstFailure.predicate.listenerValidation.endpoints.mjpeg.count, 1);
        const initialListenerCommands = owner.diagnostics.commands.filter((command: { phase: string; operation: string }) => command.phase === 'initial' && command.operation === 'inspect-listener');
        assert.deepEqual(initialListenerCommands.slice(0, 2).map((command: { endpoint: string; status: number | null }) => ({endpoint: command.endpoint, status: command.status})), [
          {endpoint: 'wda', status: 1}, {endpoint: 'mjpeg', status: 0},
        ]);
        assert.equal(initialListenerCommands[0].stderr.classification, 'present');
        assert.equal(initialListenerCommands[0].stderr.suppressed, true);
        const initialInspection = owner.diagnostics.inspections.find((inspection: { phase: string }) => inspection.phase === 'initial');
        assert.equal(initialInspection.frozenOwner, false);
        assert.equal(initialInspection.endpoints.wda.count, 0);
        assert.equal(initialInspection.endpoints.mjpeg.count, 1);
        assert.ok(owner.firstFailure.commandIds.some((id: number) => initialListenerCommands.some((command: { id: number }) => command.id === id)));
      }
      if (credentialMode) {
        const diagnosticCommands = owner.diagnostics.commands;
        assert.ok(diagnosticCommands.some((command: { operation: string; stdout: { suppressed?: boolean } }) => ['select-xctestrun', 'read-xctestrun'].includes(command.operation) && command.stdout.suppressed === true));
        if (mode === 'credential-binary' || mode === 'credential-binary-failure') {
          const installedInfoCommand = diagnosticCommands.find((command: { operation: string; stdout: { suppressed?: boolean } }) => command.operation === 'read-installed-info');
          assert.ok(installedInfoCommand);
          if (mode === 'credential-binary') assert.equal(installedInfoCommand.stdout.suppressed, true);
        }
        assert.equal(JSON.stringify(owner).includes('plain-password'), false);
        assert.equal(JSON.stringify(owner).includes('bare-password-sentinel'), false);
        assert.equal(JSON.stringify(owner).includes('private-reference'), false);
        assert.equal(JSON.stringify(owner).includes('ordinary-environment-secret'), false);
        await assertSanitizedExports(root, state, ['plain-password', 'bare-password-sentinel', 'private-reference', 'https://private.example', 'https://example.test', 'multiline-xml-secret', 'escaped-quote-secret', 'ordinary-environment-secret'], ['owner.json', 'selected.xctestrun', 'installed-Info.plist', 'ios-wda-system.log', 'wda-preflight.log']);
      }
      if (mode === 'listener-endpoints-disappear') {
        assert.equal(owner.firstFailure.stage, 'managed-wda-pid-count');
        assert.equal(owner.firstFailure.frozenOwner, true);
        const afterStatusListener = owner.diagnostics.commands.find((command: { phase: string; endpoint: string; status: number | null }) => command.phase === 'after-status' && command.endpoint === 'wda' && command.status === 1);
        assert.equal(afterStatusListener.stderr.classification, 'present');
        assert.equal(afterStatusListener.stderr.suppressed, true);
      }
      if (mode === 'status-evidence-disappear') {
        const privateOwner = JSON.parse(await readFile(join(state, 'owner-private.json'), 'utf8'));
        assertReadyStatus(owner.status);
        assertReadyStatus(privateOwner.status);
        assert.equal(owner.firstFailure.stage, 'status-evidence-write');
        assert.match(owner.firstFailure.message, /WDA status evidence write failed/u);
        assert.equal(owner.listenerValidation.failureStage, 'managed-wda-pid-count');
        const statusFailure = lifecycle.find((event: { event: string; detail?: { stage?: string } }) => event.event === 'failure-observed' && event.detail?.stage === 'status-evidence-write');
        assert.ok(statusFailure);
      }
      if (mode === 'status-noisy-first-failure') {
        assert.equal(owner.firstFailure.stage, 'managed-wda-pid-count');
        assert.equal(owner.status.body.diagnosticTruncated, true);
        assert.ok(owner.status.body.diagnostic.length <= 16384);
        assert.ok(owner.firstFailure.causalCommands.length > 0);
        assert.ok(owner.firstFailure.causalCommands.some((command: {stderr: {bytes: number; suppressed?: boolean}}) => command.stderr.bytes > 0 && command.stderr.suppressed === true));
        assert.ok(owner.firstFailure.causalCommands.every((command: {id: number}) => owner.diagnostics.commands.some((current: {id: number}) => current.id === command.id)));
        await assertSanitizedExports(root, state, []);
      }
      if (mode === 'noisy-cleanup') {
        assert.equal(owner.firstFailure.stage, 'runner-bundle-id-command');
        assert.ok(owner.firstFailure.causalCommands.some((command: { operation: string; status: number | null }) => command.operation === 'inspect-runner-bundle-id' && command.status === 77));
        assert.ok(owner.diagnostics.droppedCommands > 0);
        assert.ok(owner.firstFailure.causalCommands.length > 0);
        const causalIds = new Set(owner.firstFailure.causalCommands.map((command: { id: number }) => command.id));
        assert.ok([...causalIds].some(id => !owner.diagnostics.commands.some((command: { id: number }) => command.id === id)));
        assert.ok(Buffer.byteLength(await readFile(join(state, 'owner.json'), 'utf8')) <= 1048576);
        assert.ok(Buffer.byteLength(await readFile(join(state, 'owner-private.json'), 'utf8')) <= 1048576);
        await assertSanitizedExports(root, state, ['plain-password']);
      }
      if (mode === 'listener-second-failure') {
        assert.equal(owner.firstFailure.stage, 'mjpeg-listener-command');
        assert.equal(owner.firstFailure.predicate.status, 'evaluated');
        assert.equal(owner.firstFailure.predicate.listenerValidation.endpoints.wda.count, 1);
        assert.equal(owner.firstFailure.predicate.listenerValidation.endpoints.mjpeg.status, 'error');
      }
      if (['occupied', 'ownership', 'ambiguous', 'product'].includes(mode)) assert.equal(existsSync(join(root, 'launches')), false);
      else {
        const commands = (await readFile(join(root, 'launches'), 'utf8')).trim().split('\n').map(line => JSON.parse(line));
        assert.deepEqual(commands, [['test-without-building', '-xctestrun', owner.xctestrun, '-destination', `id=${udid}`]]);
      }
      if (mode === 'early') assert.equal(owner.exitCode, 43);
      if (mode === 'invalid' || mode === 'oversized' || mode === 'delayed') {
        assert.ok(owner.firstFailure);
        assert.ok(owner.error === 'XCTEST: startup deadline' || owner.error === `XCTEST: ${owner.firstFailure.category}`);
      }
      if (mode === 'oversized') assert.equal(owner.status?.truncated, true);
      if (mode === 'receipt-failure' || mode === 'receipt-missing-then-valid' || mode === 'receipt-product-changed') {
        assert.ok(owner.listenerEvidence?.[0]?.birth);
        assert.ok(owner.receiptError);
        if (mode === 'receipt-failure') {
          assert.equal(owner.receiptError, 'receipt-command-failed');
          assert.equal(owner.listenerValidation?.failureStage, 'validated');
          assert.ok(owner.firstFailure.causalCommands.some((command: { operation: string; status: number | null }) => command.operation === 'read-install-receipt' && command.status === 77));
          assert.equal(owner.listenerValidation?.executableHash.status, 'evaluated');
          assert.equal(owner.listenerValidation?.executableHash.expected, owner.cachedProductExecutableHash);
          assert.match(owner.cachedProductExecutableHash, /^[0-9a-f]{64}$/u);
          assert.match(owner.cachedProductExecutableHashAt, /^\d{4}-\d{2}-\d{2}T/u);
        }
        else if (mode === 'receipt-missing-then-valid') assert.equal(owner.receiptError, 'receipt-unavailable');
        else {
          assert.match(owner.error, /registration receipt validation failed/u);
          assert.equal(owner.receiptError, 'receipt-mismatch');
          const currentProductHash = createHash('sha256').update(await readFile(join(product, 'WebDriverAgentRunner-Runner'))).digest('hex');
          assert.notEqual(currentProductHash, initialProductHash);
          for (const installed of [receipt, runnerReceipt]) {
            assert.equal(createHash('sha256').update(await readFile(join(installed, 'WebDriverAgentRunner-Runner'))).digest('hex'), initialProductHash);
          }
          assert.equal(owner.cachedProductExecutableHash, initialProductHash);
          assert.equal(owner.listenerValidation?.cachedProductExecutableHash, initialProductHash);
          assert.equal(owner.listenerValidation?.cachedProductExecutableHashAt, owner.cachedProductExecutableHashAt);
          assert.ok(Date.parse(owner.cachedProductExecutableHashAt) >= Date.parse(owner.startedAt));
          assert.ok(Date.parse(owner.cachedProductExecutableHashAt) <= Number(await readFile(join(root, 'product-changed-at'), 'utf8')));
          assert.equal(owner.listenerValidation?.failureStage, 'validated');
          assert.deepEqual(owner.listenerValidation?.executableHash, {status: 'evaluated', expected: initialProductHash, actual: initialProductHash, matches: true});
          assert.equal(owner.installReceipt, await realpath(receipt));
          assert.equal(owner.receipt, await realpath(receipt));
          assert.equal(await readFile(join(root, 'receipt-queries'), 'utf8'), '2');
          assert.equal(owner.runnerPid, undefined);
          assert.equal(owner.runnerBirth, undefined);
          assert.equal(owner.runnerExecutable, undefined);
          assert.equal(owner.status, undefined);
        }
        assert.equal(existsSync(join(root, 'status-queries')), false);
      }
      if (mode === 'swap' || mode === 'pid-reuse') {
        assert.match(owner.error, /managed WDA listener changed/u);
        assert.equal(owner.runnerPid, '11111');
      }
      if (mode === 'delayed') assert.ok(Date.now() - supervisorStarted < 15_000);
      if (mode === 'credential' || mode === 'credential-binary' || mode === 'credential-binary-failure') {
        const publicOwner = await readFile(join(state, 'owner.json'), 'utf8');
        const privateOwner = await readFile(join(state, 'owner-private.json'), 'utf8');
        assert.equal(publicOwner.includes('credential-token'), false);
        assert.equal(publicOwner.includes('bare-password-sentinel'), false);
        assert.equal(publicOwner.includes('[REDACTED'), false);
        assert.equal(publicOwner.includes('ordinary-environment-secret'), false);
        assert.ok(privateOwner.includes('credential-token'));
        const privateLog = await readFile(join(state, 'wda-preflight-private.log'), 'utf8');
        for (const value of ['plain-password', 'bare-password-sentinel', 'private-reference', 'https://private.example']) assert.equal(privateLog.includes(value), false);
      }
      if (['listener-mjpeg-duplicate', 'listener-bundle-mismatch', 'listener-hash-mismatch', 'credential', 'credential-binary', 'credential-binary-failure'].includes(mode)) {
        const validation = owner.listenerValidation;
        assert.ok(validation);
        assert.equal(validation.endpoints.wda.status, 'evaluated');
        assert.equal(validation.endpoints.wda.count, 1);
        assert.ok(validation.endpoints.wda.pids.length <= 16);
        assert.equal(validation.endpoints.mjpeg.status, 'evaluated');
        assert.ok(validation.commandStatus.every((entry: { port: number; status: number | null }) => Number.isInteger(entry.port)));
        assert.equal(validation.runnerEvidencePresent, 'present');
        assert.equal(validation.runnerAssociation, 'not-evaluated');
        assert.equal(validation.mjpegAssociation, mode === 'listener-mjpeg-duplicate' ? 'mismatch' : 'not-evaluated');
        if (mode === 'listener-mjpeg-duplicate') {
          assert.equal(validation.endpoints.mjpeg.count, 2);
          assert.deepEqual(validation.endpoints.mjpeg.pids, ['11111', '22222']);
          assert.equal(validation.failureStage, 'mjpeg-pid-count');
          assert.equal(validation.errorCategory, 'listener-endpoint-cardinality');
        }
        if (mode === 'listener-bundle-mismatch') {
          assert.equal(validation.bundleId.status, 'evaluated');
          assert.equal(validation.bundleId.matches, false);
          assert.equal(validation.executableHash.status, 'not-evaluated');
          assert.equal(validation.failureStage, 'runner-bundle-id');
        }
        if (mode === 'listener-hash-mismatch') {
          assert.equal(validation.bundleId.matches, true);
          assert.equal(validation.executableHash.status, 'evaluated');
          assert.equal(validation.executableHash.matches, false);
          assert.match(validation.executableHash.expected, /^[0-9a-f]{64}$/u);
          assert.match(validation.executableHash.actual, /^[0-9a-f]{64}$/u);
          assert.equal(validation.failureStage, 'runner-executable-hash');
        }
        if (mode === 'credential' || mode === 'credential-binary' || mode === 'credential-binary-failure') {
          assert.equal(validation.pathShape.executableName, false);
          assert.equal(validation.bundleId.status, 'not-evaluated');
          assert.equal(validation.executableHash.status, 'not-evaluated');
          assert.equal(validation.failureStage, 'runner-executable-name');
        }
      }
      if (['listener-invalid-pid', 'listener-first-failure', 'listener-second-failure', 'listener-endpoints-disappear', 'status-evidence-disappear', 'listener-hash-after-freeze'].includes(mode)) {
        const validation = owner.listenerValidation;
        assert.ok(validation);
        assert.ok(validation.commandStatus.every((entry: { port: number; status: number | null }) => Number.isInteger(entry.port)));
        if (mode === 'listener-invalid-pid') {
          assert.deepEqual(validation.endpoints.wda, {status: 'error', errorCategory: 'invalid-process-id'});
          assert.deepEqual(validation.endpoints.mjpeg, {status: 'not-evaluated'});
          assert.equal(validation.commandStatus[0].status, 0);
          assert.equal(validation.failureStage, 'wda-listener-command');
          assert.equal(validation.errorCategory, 'listener-invalid-process-id');
        }
        if (mode === 'listener-first-failure') {
          assert.deepEqual(validation.endpoints.wda, {status: 'error', errorCategory: 'command-error'});
          assert.deepEqual(validation.endpoints.mjpeg, {status: 'not-evaluated'});
          assert.equal(validation.runnerEvidencePresent, 'not-evaluated');
          assert.equal(validation.commandStatus[0].status, 2);
          assert.equal(validation.failureStage, 'wda-listener-command');
        }
        if (mode === 'listener-second-failure') {
          assert.deepEqual(validation.endpoints.wda, {status: 'evaluated', count: 1, pids: [String(owner.pid)]});
          assert.deepEqual(validation.endpoints.mjpeg, {status: 'error', errorCategory: 'command-error'});
          assert.equal(validation.runnerEvidencePresent, 'not-evaluated');
          assert.equal(validation.commandStatus.at(-1).status, 2);
          assert.equal(validation.failureStage, 'mjpeg-listener-command');
        }
        if (mode === 'listener-endpoints-disappear') {
          assert.deepEqual(validation.endpoints.wda, {status: 'evaluated', count: 0, pids: []});
          assert.deepEqual(validation.endpoints.mjpeg, {status: 'evaluated', count: 0, pids: []});
          assert.equal(validation.runnerEvidencePresent, 'not-evaluated');
          assert.equal(validation.runnerAssociation, 'mismatch');
          assert.equal(validation.mjpegAssociation, 'not-evaluated');
          assert.equal(validation.failureStage, 'managed-wda-pid-count');
        }
        if (mode === 'listener-hash-after-freeze') {
          assert.equal(validation.endpoints.wda.status, 'evaluated');
          assert.equal(validation.runnerEvidencePresent, 'present');
          assert.equal(validation.runnerAssociation, 'match');
          assert.equal(validation.mjpegAssociation, 'match');
          assert.equal(validation.executableHash.status, 'evaluated');
          assert.equal(validation.executableHash.matches, false);
          assert.equal(validation.executableHash.expected, owner.cachedProductExecutableHash);
          assert.notEqual(validation.executableHash.actual, validation.executableHash.expected);
          assert.equal(validation.failureStage, 'runner-executable-hash');
        }
      }
      if (credentialMode) {
        assert.equal(owner.receipt, await realpath(receipt));
        const installedInfo = await readFile(join(state, 'installed-Info.plist'), 'utf8');
        for (const value of ['plain-password', 'bare-password-sentinel', 'private-reference', 'https://private.example', 'multiline-xml-secret']) assert.equal(installedInfo.includes(value), false);
        const installedEvidence = JSON.parse(installedInfo) as {schema: number; format: string; status: string; bundleIdentifier: string; values: string};
        assert.equal(installedEvidence.values, 'suppressed');
        if (mode === 'credential-binary-failure') assert.deepEqual(installedEvidence, {schema: 1, format: 'binary', status: 'conversion-failed', bundleIdentifier: 'not-evaluated', values: 'suppressed'});
        else if (mode === 'credential-binary') assert.deepEqual(installedEvidence, {schema: 1, format: 'binary', status: 'parsed', bundleIdentifier: 'known', values: 'suppressed'});
        else assert.deepEqual(installedEvidence, {schema: 1, format: 'xml', status: 'suppressed', bundleIdentifier: 'not-evaluated', values: 'suppressed'});
      }
      if (streamMode) {
        const forbidden = ['stream-secret', 'reverse-secret', 'utf8-secret', '秘密', 'eof-secret', 'fragment', 'private.example', 'multiline-xml-secret', 'escaped-quote-secret'];
        const privateLog = await readFile(join(state, 'wda-preflight-private.log'), 'utf8');
        for (const value of forbidden) assert.equal(privateLog.includes(value), false, `final private log leaked ${value}`);
        assert.match(privateLog, /startup output (?:suppressed|summary)/u);
        const requiredAfterCleanup = ['owner.json', 'selected.xctestrun', 'installed-Info.plist', 'ios-wda-system.log', 'wda-preflight.log'];
        await assertSanitizedExports(root, state, forbidden, requiredAfterCleanup);
        if (mode === 'stream-finalization-failure') {
          assert.equal(owner.firstFailure.stage, 'supervisor-error');
          assert.ok(owner.diagnostics.lifecycle.some((event: { event: string; detail?: { stage?: string } }) => event.event === 'failure-observed' && event.detail?.stage === 'startup-log-finalization'));
        } else assert.equal(existsSync(join(state, 'wda-preflight.log')), true);
      }
      if (mode === 'status-forged-markers') {
        const forbidden = ['status-opaque-secret', 'status-password-sentinel', 'https://status.private.example'];
        const finalPrivateOwner = JSON.parse(await readFile(join(state, 'owner-private.json'), 'utf8')) as { status?: { statusCode?: number; bytes?: number; body?: Record<string, unknown> } };
        const finalStatus = JSON.parse(await readFile(join(state, 'wda-status.json'), 'utf8')) as { statusCode?: number; bytes?: number; body?: Record<string, unknown> };
        assert.deepEqual(owner.status?.body, {
          classification: 'unrecognized',
          ready: 'not-evaluated',
          state: 'unrecognized',
          buildVersion: 'unrecognized',
          productBundleIdentifier: 'unrecognized',
          osVersion: 'unrecognized',
          payload: 'suppressed',
          diagnostic: 'suppressed',
          diagnosticBytes: owner.status?.bytes,
          diagnosticTruncated: false,
          bodyBytes: owner.status?.bytes,
        });
        assert.deepEqual(finalStatus.body, {
          classification: 'unrecognized',
          ready: 'not-evaluated',
          state: 'unrecognized',
          buildVersion: 'unrecognized',
          productBundleIdentifier: 'unrecognized',
          osVersion: 'unrecognized',
          payload: 'suppressed',
          diagnostic: 'suppressed',
          diagnosticBytes: finalStatus.bytes,
          diagnosticTruncated: false,
          bodyBytes: finalStatus.bytes,
        });
        assert.equal(finalPrivateOwner.status?.statusCode, owner.status?.statusCode);
        assert.equal(finalPrivateOwner.status?.bytes, owner.status?.bytes);
        assert.deepEqual(finalPrivateOwner.status?.body, owner.status?.body);
        assert.equal(JSON.stringify(finalPrivateOwner.status).includes('status-opaque-secret'), false);
        assert.equal(JSON.stringify(finalPrivateOwner.status).includes('status-password-sentinel'), false);
        assert.equal(JSON.stringify(finalPrivateOwner.status).includes('status.private.example'), false);
        assert.equal(JSON.stringify(owner.status).includes('status-opaque-secret'), false);
        assert.equal(JSON.stringify(owner.status).includes('status-password-sentinel'), false);
        assert.equal(JSON.stringify(owner.status).includes('status.private.example'), false);
        await assertSanitizedExports(root, state, forbidden, ['owner.json', 'wda-status.json', 'ios-wda-system.log']);
      }
      if (outputBoundaryMode) {
        const output = (JSON.parse(await readFile(join(root, 'output-ready'), 'utf8')) as { outputBytes: { stdout: number; stderr: number } }).outputBytes;
        assert.equal(owner.pendingLogBytes, 0);
        const total = output.stdout + output.stderr;
        assert.ok((await readFile(join(state, 'wda-preflight-private.log'), 'utf8')).length > 0);
        if (mode === 'output-above' || mode === 'output-combined') {
          assert.ok((owner.receivedLogBytes || 0) > 104857600);
          assert.equal(owner.firstFailure.stage, 'startup-log-limit');
          assert.ok(owner.startupOutputLimit);
          assert.ok(owner.startupOutputLimit.receivedBytes > 104857600);
          assert.equal(owner.logTruncated, true);
        } else {
          assert.equal(owner.startupOutputLimit, undefined);
          assert.equal(owner.logTruncated, undefined);
          assert.notEqual(owner.firstFailure?.stage, 'startup-log-limit');
          assert.ok(total <= 104857600);
          if (mode === 'output-below') assert.equal(total, 104857599);
          if (mode === 'output-equal') assert.equal(total, 104857600);
          if (mode === 'output-shrinking') assert.ok(owner.safeLogBytes < owner.receivedLogBytes);
          if (mode === 'output-expanding') {
            assert.equal(owner.logPersistenceTruncated, undefined);
            assert.ok(owner.safeLogBytes < 1024);
          }
        }
      }
      if (mode === 'ready') assert.equal(owner.error, undefined);
      if (mode !== 'ownership') assert.match(await readFile(join(state, 'ios-wda-system.log'), 'utf8'), /output":"suppressed"/u);
      assert.equal(existsSync(join(root, 'runner.pid')), false);
      assertionsPassed = true;
    } finally {
      if (!done) { await writeFile(join(state, 'stop'), 'failed test cleanup'); child.kill('SIGTERM'); await exited; }
      const diagnosticRoot = process.env.XCTEST_DIAGNOSTIC_ROOT;
      if (!assertionsPassed && diagnosticRoot) {
        await rm(diagnosticRoot, { recursive: true, force: true });
        await cp(root, diagnosticRoot, { recursive: true, force: true });
      }
      await rm(root, { recursive: true, force: true });
    }
  }]);
}

xctestOwnerTests.push(['Native startup status publication ordering is deterministic against the legacy source', async () => {
  const sourcePath = join(import.meta.dirname, '../support/ios-xctest.ts');
  const source = await readFile(sourcePath, 'utf8');
  const currentStatusBlock = [
    '          const bounded = boundedStatus(status);',
    '          owner.status = bounded;',
    "          if (!save()) throw new Error('XCTEST: owner evidence write failed');",
    '          try {',
    '            writeStatusEvidence(root, bounded);',
    '          } catch {',
    "            const statusError = new Error('XCTEST: WDA status evidence write failed');",
    "            rememberFailure({error: statusError, phase: 'status', stage: 'status-evidence-write', category: 'status-evidence-write', frozenOwner: Boolean(frozenRunner)});",
    '            throw statusError;',
    '          }',
  ].join('\n');
  assert.equal(source.includes(currentStatusBlock), true);
  const legacyStatusBlock = [
    '          const bounded = boundedStatus(status);',
    '          owner.status = bounded;',
    '          try {',
    "            writeFileSync(join(root, 'wda-status.json'), JSON.stringify({checkedAt: new Date().toISOString(), ...bounded}) + '\\n', { mode: 0o600 });",
    "            writeFileSync(join(root, 'legacy-status-published'), '');",
    "            while (!stop && Date.now() < deadline && !existsSync(join(root, 'legacy-continue'))) await sleep(1);",
    "            if (stop) throw new Error('XCTEST: publication ordering barrier cancelled');",
    "            if (Date.now() >= deadline) throw new Error('XCTEST: publication ordering barrier deadline');",
    '          } catch {',
    "            const statusError = new Error('XCTEST: WDA status evidence write failed');",
    "            rememberFailure({error: statusError, phase: 'status', stage: 'status-evidence-write', category: 'status-evidence-write', frozenOwner: Boolean(frozenRunner)});",
    '            throw statusError;',
    '          }',
    '          save();',
  ].join('\n');
  const legacySource = source.replace(currentStatusBlock, legacyStatusBlock);
  const candidateStatusBlock = currentStatusBlock.replace(
    "          if (!save()) throw new Error('XCTEST: owner evidence write failed');",
    "          if (!save()) throw new Error('XCTEST: owner evidence write failed');\n          writeFileSync(join(root, 'candidate-owner-saved'), '');\n          while (!stop && Date.now() < deadline && !existsSync(join(root, 'candidate-continue'))) await sleep(1);\n          if (stop) throw new Error('XCTEST: publication ordering barrier cancelled');\n          if (Date.now() >= deadline) throw new Error('XCTEST: publication ordering barrier deadline');",
  );
  const candidateSource = source.replace(currentStatusBlock, candidateStatusBlock);
  assert.notEqual(legacySource, source);
  assert.notEqual(candidateSource, source);

  const assertForgedStatus = (value: unknown): void => {
    assert.ok(value && typeof value === 'object');
    const status = value as { statusCode?: number; bytes?: number; body?: Record<string, unknown> };
    assert.equal(status.statusCode, 200);
    assert.ok(Number.isInteger(status.bytes));
    assert.deepEqual(status.body, {
      classification: 'unrecognized',
      ready: 'not-evaluated',
      state: 'unrecognized',
      buildVersion: 'unrecognized',
      productBundleIdentifier: 'unrecognized',
      osVersion: 'unrecognized',
      payload: 'suppressed',
      diagnostic: 'suppressed',
      diagnosticBytes: status.bytes,
      diagnosticTruncated: false,
      bodyBytes: status.bytes,
    });
    const serialized = JSON.stringify(value);
    for (const forbidden of ['status-opaque-secret', 'status-password-sentinel', 'https://status.private.example']) assert.equal(serialized.includes(forbidden), false);
  };

  const runVariant = async (label: 'legacy' | 'candidate', variantSource: string, legacyOrdering: boolean, forceAssertionFailure = false): Promise<void> => {
    const root = await mkdtemp(join(tmpdir(), `herdr-xctest-publication-${label}-`));
    const udid = '82342155-D8BD-4C4D-BD5E-1EDCDF9CFB40';
    const product = join(root, 'products/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app');
    const receipt = join(root, `Devices/${udid}/data/Containers/Bundle/Application/10C0E9C0-50FD-4C3E-AC55-AC1158A00568/WebDriverAgentRunner-Runner.app`);
    const runnerReceipt = join(root, `Devices/${udid}/data/Containers/Bundle/Application/1C1FB5F7-2D0E-49AC-BE09-62816F229E5C/WebDriverAgentRunner-Runner.app`);
    const state = join(root, 'state');
    const bin = join(root, 'bin');
    const support = join(root, 'support');
    const xctestrun = join(root, 'products/WebDriverAgentRunner_test.xctestrun');
    let child: ReturnType<typeof spawn> | undefined;
    let done = false;
    let exited: Promise<void> | undefined;
    let errors = '';
    const waitForExit = async (timeoutMs: number): Promise<boolean> => {
      if (!exited || done) return true;
      let timer: ReturnType<typeof setTimeout> | undefined;
      const timed = new Promise<boolean>(resolve => { timer = setTimeout(() => resolve(false), timeoutMs); });
      const settled = await Promise.race([exited.then(() => true), timed]);
      if (timer) clearTimeout(timer);
      return settled;
    };
    try {
      await Promise.all([
        mkdir(bin),
        mkdir(state),
        mkdir(support),
        mkdir(join(product, 'PlugIns/WebDriverAgentRunner.xctest'), { recursive: true }),
        mkdir(receipt, { recursive: true }),
        mkdir(runnerReceipt, { recursive: true }),
        mkdir(join(root, 'wda')),
      ]);
      for (const dir of [product, receipt, runnerReceipt]) {
        await writeFile(join(dir, 'Info.plist'), JSON.stringify({ CFBundleIdentifier: 'com.facebook.WebDriverAgentRunner.xctrunner' }));
        await writeFile(join(dir, 'WebDriverAgentRunner-Runner'), 'exact built runner');
      }
      await writeFile(join(root, 'wda/package.json'), JSON.stringify({ version: '16.12.1' }));
      await writeFile(xctestrun, JSON.stringify({ WebDriverAgentRunner: {
        TestHostBundleIdentifier: 'com.facebook.WebDriverAgentRunner.xctrunner',
        TestBundlePath: '__TESTHOST__/PlugIns/WebDriverAgentRunner.xctest',
        TestHostPath: '__TESTROOT__/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app',
      } }));
      await writeFile(join(root, 'owned'), `ios:${udid}`);
      await writeFile(join(support, 'diagnostics.ts'), await readFile(join(import.meta.dirname, '../support/diagnostics.ts')));
      const variantPath = join(support, `${label}.ts`);
      await writeFile(variantPath, variantSource);
      const dispatcher = join(bin, 'xctest-command-dispatcher');
      await writeFile(dispatcher, shim, { mode: 0o700 });
      for (const cmd of ['plutil', 'lsof', 'ps', 'xcrun', 'xcodebuild']) await symlink(dispatcher, join(bin, cmd));
      const wdaServerFile = join(bin, 'xctest-wda-server.ts');
      await writeFile(wdaServerFile, wdaServer);
      const reserve = createNetServer();
      await new Promise<void>((resolve, reject) => {
        reserve.once('error', reject);
        reserve.listen(0, '127.0.0.1', resolve);
      });
      const port = (reserve.address() as AddressInfo).port;
      await new Promise<void>((resolve, reject) => reserve.close(error => error ? reject(error) : resolve()));
      const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, STARTUP_TEST_ROOT: root, STARTUP_TEST_MODE: 'status-forged-markers', STARTUP_TEST_EXECUTABLE: process.execPath, STARTUP_TEST_WDA_SERVER: wdaServerFile,
        STARTUP_TEST_RECEIPT: receipt, STARTUP_TEST_RUNNER_RECEIPT: runnerReceipt, STARTUP_TEST_PRODUCT: product, STARTUP_TEST_XCTESTRUN: xctestrun, IOS_XCTEST_STATE_DIR: state,
        IOS_SIMULATOR_UDID: udid, IOS_PLATFORM_VERSION: '18.6', IOS_WDA_PORT: String(port), IOS_WDA_MJPEG_PORT: String(port === 65535 ? port - 1 : port + 1),
        IOS_WDA_PREBUILT_PATH: product, IOS_WDA_BOOTSTRAP_PATH: join(root, 'products'), IOS_WDA_AGENT_PATH: join(root, 'wda/WebDriverAgent.xcodeproj'),
        MOBILE_DEVICE_OWNERSHIP_FILE: join(root, 'owned') };
      const deadline = Date.now() + 10_000;
      await writeFile(join(state, 'deadline'), String(deadline));
      child = spawn(process.execPath, [variantPath, 'supervise'], { env, stdio: ['ignore', 'ignore', 'pipe'] });
      if (!child.stderr) throw new Error('publication ordering fixture did not provide stderr');
      child.stderr.on('data', chunk => { errors += chunk.toString(); });
      exited = new Promise<void>(resolve => child?.on('close', () => { done = true; resolve(); }));
      const waitFor = async (path: string): Promise<void> => {
        while (!done && Date.now() < deadline && !existsSync(path)) await pause();
        assert.equal(existsSync(path), true, `${label} barrier was not reached: ${errors}`);
      };
      const statusPath = join(state, 'wda-status.json');
      const ownerPath = join(state, 'owner.json');
      const privateOwnerPath = join(state, 'owner-private.json');
      if (legacyOrdering) {
        await waitFor(join(state, 'legacy-status-published'));
        const owner = JSON.parse(await readFile(ownerPath, 'utf8')) as { status?: unknown };
        const privateOwner = JSON.parse(await readFile(privateOwnerPath, 'utf8')) as { status?: unknown };
        assert.equal(owner.status, undefined);
        assert.equal(privateOwner.status, undefined);
        assertForgedStatus(JSON.parse(await readFile(statusPath, 'utf8')));
        if (forceAssertionFailure) assert.equal(existsSync(statusPath), false);
        await writeFile(join(state, 'legacy-continue'), 'continue');
      } else {
        await waitFor(join(state, 'candidate-owner-saved'));
        const owner = JSON.parse(await readFile(ownerPath, 'utf8')) as { status?: unknown };
        const privateOwner = JSON.parse(await readFile(privateOwnerPath, 'utf8')) as { status?: unknown };
        assertForgedStatus(owner.status);
        assertForgedStatus(privateOwner.status);
        assert.equal(existsSync(statusPath), false);
        if (forceAssertionFailure) assert.equal(existsSync(statusPath), true);
        await writeFile(join(state, 'candidate-continue'), 'continue');
        await waitFor(statusPath);
        assertForgedStatus(JSON.parse(await readFile(statusPath, 'utf8')));
      }
      await writeFile(join(state, 'stop'), 'publication ordering fixture finalization');
      await Promise.race([exited, new Promise<never>((_, reject) => setTimeout(() => reject(new Error(`${label} supervisor deadline: ${errors}`)), 15_000))]);
      const finalOwner = JSON.parse(await readFile(ownerPath, 'utf8')) as { status?: unknown };
      const finalPrivateOwner = JSON.parse(await readFile(privateOwnerPath, 'utf8')) as { status?: unknown };
      assertForgedStatus(finalOwner.status);
      assertForgedStatus(finalPrivateOwner.status);
      assertForgedStatus(JSON.parse(await readFile(statusPath, 'utf8')));
    } finally {
      if (child && !done) {
        await writeFile(join(state, `${label}-continue`), 'failed publication ordering fixture cleanup');
        await writeFile(join(state, 'stop'), 'failed publication ordering fixture cleanup');
        child.kill('SIGTERM');
        if (!await waitForExit(2_000)) {
          child.kill('SIGKILL');
          assert.equal(await waitForExit(2_000), true, `${label} supervisor did not settle after forced cleanup`);
        }
      }
      if (child) {
        assert.equal(existsSync(join(root, 'runner.pid')), false, `${label} WDA runner remained after cleanup`);
        assert.equal(existsSync(join(root, 'xcode.pid')), false, `${label} WDA subprocess remained after cleanup`);
      }
      await rm(root, { recursive: true, force: true });
    }
  };

  await runVariant('legacy', legacySource, true);
  await runVariant('candidate', candidateSource, false);
  await assert.rejects(() => runVariant('legacy', legacySource, true, true), /true !== false/u);
  await assert.rejects(() => runVariant('candidate', candidateSource, false, true), /false !== true/u);
}]);

const runStopCli = (env: NodeJS.ProcessEnv): Promise<{ code: number | null; stderr: string }> => new Promise(resolve => {
  const child = spawn(process.execPath, [join(import.meta.dirname, '../support/ios-xctest.ts'), 'stop'], { env, stdio: ['ignore', 'ignore', 'pipe'] });
  let stderr = '';
  child.stderr.on('data', chunk => { stderr += chunk.toString(); });
  child.on('close', (code) => resolve({ code, stderr }));
});

xctestOwnerTests.push(['Native startup stop CLI propagates recorded cleanup errors', async () => {
  const root = await mkdtemp(join(tmpdir(), 'herdr-xctest-stop-'));
  const state = join(root, 'state');
  const udid = '82342155-D8BD-4C4D-BD5E-1EDCDF9CFB40';
  await mkdir(state);
  await writeFile(join(root, 'owned'), `ios:${udid}`);
  const owner: { udid: string; product: string; url: string; startedAt: string; ready: boolean; endedAt: string; error?: string } = { udid, product: '', url: 'http://127.0.0.1:8100', startedAt: new Date().toISOString(), ready: false,
    endedAt: new Date().toISOString(), error: 'XCTEST: cleanup failed https://example.test/?token=stop-secret' };
  const privateSerialized = `${JSON.stringify(owner)}\n`;
  const publicSerialized = `${JSON.stringify({ ...owner, error: 'XCTEST: cleanup failed https://example.test/?token=[REDACTED]' })}\n`;
  await writeFile(join(state, 'owner-private.json'), privateSerialized, { mode: 0o600 });
  await writeFile(join(state, 'owner.json'), publicSerialized, { mode: 0o600 });
  const env = { ...process.env, IOS_XCTEST_STATE_DIR: state, IOS_SIMULATOR_UDID: udid, MOBILE_DEVICE_OWNERSHIP_FILE: join(root, 'owned') };
  try {
    const failed = await runStopCli(env);
    assert.notEqual(failed.code, 0);
    assert.match(failed.stderr, /cleanup failed/u);
    assert.equal(failed.stderr.includes('stop-secret'), false);
    assert.ok(failed.stderr.includes('[REDACTED]'));
    owner.error = undefined;
    await writeFile(join(state, 'owner-private.json'), `${JSON.stringify(owner)}\n`, { mode: 0o600 });
    await writeFile(join(state, 'owner.json'), `${JSON.stringify(owner)}\n`, { mode: 0o600 });
    const clean = await runStopCli(env);
    assert.equal(clean.code, 0, clean.stderr);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}]);
