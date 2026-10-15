function replaceUnique(source, oldText, newText, label) {
  const first = source.indexOf(oldText);
  if (first < 0 || source.indexOf(oldText, first + 1) >= 0)
    throw new Error(`Nonunique revision-05 test input: ${label}`);
  return source.slice(0, first) + newText + source.slice(first + oldText.length);
}

export function prepareHistoricalTests(source, receiptsURL) {
  let result = source;
  result = replaceUnique(result,
    "import { readFileSync, writeFileSync, appendFileSync } from 'node:fs';",
    `import { readFileSync, writeFileSync, appendFileSync, readdirSync, lstatSync, statfsSync } from 'node:fs';\nimport { assessRecoveredInvocationLedger, controllerShellAssociationIdentity, deduplicateInvocationRecords, deriveRecoveredInvocationRecord, extractCompletionGateShellResults, formatCompletionPointer, projectCompletionAccounting, reconcileControllerGateAssociations, reconcileInvocationAccounting, reuseControllerRecoveryManifest, summarizeCommandAttempts, validateBoundedEvidenceRead, validateBoundedEvidenceSnapshot, validateControllerRecoveryAssociations, validatePriorAttemptTestEvidence } from ${JSON.stringify(receiptsURL)};`,
    'bounded receipt filesystem inputs and accounting regressions');
  result = replaceUnique(result,
    "const attempt = process.argv[2];",
    "const attempt = 'attempt-06';",
    'attempt');
  result = replaceUnique(result,
    "const revision = process.argv[3];",
    "const revision = 'revision-06';",
    'revision');
  result = replaceUnique(result,
    "const root = dirname(new URL(import.meta.url).pathname);",
    `const root = dirname(new URL(import.meta.url).pathname);\n${outputBudget}`,
    'bounded source output monitor');
  result = replaceUnique(result,
    "writeFileSync(receiptPath, '', { flag: 'wx', mode: 0o600 });",
    "assertOutputBudget(0);\nwriteFileSync(receiptPath, '', { flag: 'wx', mode: 0o600 });",
    'exclusive case receipt admission');
  result = replaceUnique(result,
    "const consumers = readFileSync(join(sourceRoot, 'consumers.derived.js'), 'utf8');\nfor (const piece of [hosts.split('var InProcessLoaderHost')[0], consumers.split('async function loadFileSuites')[0], consumers.slice(consumers.indexOf('async function loadFileSuites'), consumers.indexOf('async function finishTaskRun'))])",
    "const consumers = readFileSync(join(sourceRoot, 'consumers.derived.js'), 'utf8');\nconst jobDispatchers = readFileSync(join(sourceRoot, 'jobDispatchers.derived.js'), 'utf8');\nconst multiplexer = readFileSync(join(sourceRoot, 'multiplexer.derived.js'), 'utf8');\nfor (const piece of [hosts.split('var InProcessLoaderHost')[0], consumers.split('async function loadFileSuites')[0], consumers.slice(consumers.indexOf('async function loadFileSuites'), consumers.indexOf('async function finishTaskRun')), jobDispatchers, multiplexer])",
    'actual JobDispatcher and Multiplexer extraction');
  result = replaceUnique(result,
    "import_common7: { ipc: { serializeConfig: c => c } },",
    "import_common7: { ipc: { serializeConfig: c => options.serializeConfig ? options.serializeConfig(c) : c } },",
    'synchronous pre-host failure adapter');
  result = replaceUnique(result,
    "const published = [];\n  const errors = [];",
    "const published = [];\n  const lastRunWrites = [];\n  const errors = [];",
    'isolated LastRunReporter filesystem sink');
  result = replaceUnique(result,
    "const context = {\n    require, assert:",
    "const context = {\n    AggregateError, import_fs12: { default: { promises: { mkdir: async () => {}, writeFile: async (path, content) => lastRunWrites.push({ path, content }) } } }, import_path16: { default: { join: (...parts) => parts.join('/'), dirname: path => path.slice(0, path.lastIndexOf('/')), resolve: (...parts) => parts.join('/') } }, import_util3: { serializeError: error => error }, eventsHelper: { addEventListener(target, event, listener) { target.on(event, listener); return { target, event, listener }; }, removeEventListeners(listeners) { for (const { target, event, listener } of listeners) target.removeListener(event, listener); } }, require, assert:",
    'actual JobDispatcher event disposal and aggregate errors');
  result = replaceUnique(result,
    "vm.runInContext(hosts + consumers + '\\nglobalThis.bound = { ProcessHost, WorkerHost, OutOfProcessLoaderHost, Dispatcher, loadFileSuites, finishTaskRun, runAllTestsWithConfig };', context, { filename: 'actual-derived-regions.js' });",
    "vm.runInContext(hosts + consumers + jobDispatchers + multiplexer + '\\nglobalThis.bound = { ProcessHost, WorkerHost, OutOfProcessLoaderHost, Dispatcher, JobDispatcher, Multiplexer, LastRunReporter, loadFileSuites, finishTaskRun, runAllTestsWithConfig };', context, { filename: 'actual-derived-regions.js' });",
    'actual Dispatcher, JobDispatcher, Multiplexer, and LastRunReporter execution');
  result = replaceUnique(result,
    "const f = { ...context.bound, completion, clock, children, published, errors, calls, setDeadline: d => deadline = d };",
    "const f = { ...context.bound, completion, clock, children, published, lastRunWrites, errors, calls, setDeadline: d => deadline = d };",
    'isolated reporter evidence');
  result = replaceUnique(result,
    "test('C43', 'worker write-admission failure starts no child and remains failed', async () => {\n  const f = fixture({ admit: () => { throw new Error('admission failed'); } }); const worker = new f.WorkerHost({ workerHash: 'x', projectId: 'p' }, { parallelIndex: 0, outputDir: '/private', extraEnv: {} });\n  await assert.rejects(worker.start(), /admission failed/); incomplete(await worker.stop()); assert.equal(f.calls.fork, 0); return f;\n});",
    "test('C43', 'worker write-admission failure starts no child and remains failed', async () => {\n  const f = fixture({ admit: () => { throw new Error('admission failed'); } }); const worker = new f.WorkerHost({ workerHash: 'x', projectId: 'p' }, { parallelIndex: 0, outputDir: '/private', extraEnv: {} });\n  await assert.rejects(worker.start(), /admission failed/); incomplete(await worker.stop()); assert.equal(f.calls.fork, 0); assert.equal(f.calls.retain, 0); return f;\n});",
    'C43 refused path retention fence');
  result = replaceUnique(result,
    "test('C32', 'loader cache response hang bounds cache consumer then finally stops', async () => {\n  const f = fixture({ deadline: 100 }); const loader = new f.OutOfProcessLoaderHost({}); const start = loader.start([]); const child = f.children.at(-1);\n  child.emit('message', { method: 'ready' }); assert.equal(await start, true); const stop = loader.stop(); stop.catch(() => {});\n  await f.clock.tick(100); await assert.rejects(stop, /loader-cache-consumer/);\n  assert.ok(f.completion.snapshot().consumed.some(c => c.owner === 'OutOfProcessLoaderHost.stop')); return f;\n});",
    "test('C32', 'loader cache response hang bounds cache consumer then finally stops', async () => {\n  const f = fixture({ deadline: 100 }); const loader = new f.OutOfProcessLoaderHost({}); const start = loader.start([]); const child = f.children.at(-1);\n  child.emit('message', { method: 'ready' }); assert.equal(await start, true); const stop = loader.stop(); stop.catch(() => {});\n  await f.clock.tick(100); await assert.rejects(stop, /loader-cache-consumer/);\n  assert.ok(f.completion.snapshot().consumed.some(c => c.owner === 'OutOfProcessLoaderHost.stop'));\n  assert.equal(child.sends.filter(s => s.message.method === '__stop__').length, 0); return f;\n});",
    'C32 expired-parent observation');
  result = replaceUnique(result,
    "const results = [];",
    `${dispatcherCases}\nfunction createCaseWatchdog(run, id, timeout, onTimeout) {\n  let timer;\n  let work;\n  try { work = run(); } catch (error) { work = Promise.reject(error); }\n  const timedOut = new Promise((_, reject) => {\n    timer = setTimeout(() => { onTimeout(); reject(new Error(\`Harness case watchdog: \${id}\`)); }, timeout);\n  });\n  return { promise: Promise.race([work, timedOut]), clear: () => clearTimeout(timer) };\n}\nasync function runUntilAttemptFatal(entries, execute) {\n  const results = [];\n  for (const entry of entries) {\n    const { result, attemptFatal } = await execute(entry);\n    results.push(result);\n    if (attemptFatal) break;\n  }\n  return results;\n}\nconst suiteStarted = performance.now();`,
    'RET2 actual-caller cases and fatal attempt runner');
  result = replaceUnique(result,
    "for (const { id, title, run } of cases) {",
    "const results = await runUntilAttemptFatal(cases, async ({ id, title, run }) => {",
    'fatal attempt runner execution');
  result = replaceUnique(result,
    "  clearTimeout(watchdog);",
    "  watchdog?.clear();",
    'clear shared case watchdog');
  result = replaceUnique(result,
    "  appendFileSync(receiptPath, JSON.stringify(result) + '\\n');",
    "  const caseReceipt = JSON.stringify(result) + '\\n';\n  assertOutputBudget(Buffer.byteLength(caseReceipt));\n  appendFileSync(receiptPath, caseReceipt);",
    'bounded per-case receipt');
  result = replaceUnique(result,
    "  results.push(result);\n  const caseReceipt = JSON.stringify(result) + '\\n';\n  assertOutputBudget(Buffer.byteLength(caseReceipt));\n  appendFileSync(receiptPath, caseReceipt);",
    "  const caseReceipt = JSON.stringify(result) + '\\n';\n  assertOutputBudget(Buffer.byteLength(caseReceipt));\n  appendFileSync(receiptPath, caseReceipt);",
    'fatal runner owns case result collection');
  result = replaceUnique(result,
    "  let watchdog;\n  try {",
    "  let watchdog;\n  let attemptFatal = false;\n  try {",
    'fatal source attempt state');
  result = replaceUnique(result,
    "  try {\n    const f = await Promise.race([run(), new Promise((_, reject) => {\n      watchdog = setTimeout(() => reject(new Error(`Harness case watchdog: ${id}`)), 1000);\n    })]);",
    "  try {\n    if (performance.now() - suiteStarted >= 30000) { attemptFatal = true; throw new Error('30s suite watchdog exhausted before case'); }\n    const remaining = Math.max(0, 30000 - (performance.now() - suiteStarted));\n    watchdog = createCaseWatchdog(run, id, Math.min(1000, remaining), () => { attemptFatal = true; });\n    const f = await watchdog.promise;", 
    '30 second suite and one second case watchdogs');
  result = replaceUnique(result,
    "  console.log(`${result.passed ? 'PASS' : 'FAIL'} ${id}${result.error ? '\\n' + result.error : ''}`);\n}\nconst summary = { attempt,",
    "  console.log(`${result.passed ? 'PASS' : 'FAIL'} ${id}${result.error ? '\\n' + result.error : ''}`);\n  return { result, attemptFatal };\n});\nconst summary = { attempt,",
    'fatal watchdog attempt stops later cases');
  result = replaceUnique(result,
    "writeFileSync(join(root, `${attempt}.summary.json`), JSON.stringify(summary, null, 2) + '\\n', { flag: 'wx', mode: 0o600 });",
    "const summaryText = JSON.stringify(summary, null, 2) + '\\n';\nassertOutputBudget(Buffer.byteLength(summaryText));\nwriteFileSync(join(root, `${attempt}.summary.json`), summaryText, { flag: 'wx', mode: 0o600 });",
    'bounded summary receipt');
  return result;
}

const dispatcherCases = String.raw`
function dispatcherFixture(f, options = {}) {
  const events = { begin: [], end: [], error: [] };
  const test = {
    id: 'test-id', expectedStatus: 'passed', retries: 0, results: [], annotations: [], parent: null,
    _appendTestResult() {
      const result = { stdout: [], stderr: [], steps: [], attachments: [] };
      this.results.push(result);
      return result;
    },
    outcome() { return this.results.at(-1)?.status === this.expectedStatus ? 'expected' : 'unexpected'; }
  };
  const project = { id: 'p', project: { outputDir: '/private' } };
  const testRun = {
    config: { projects: [project], config: { workers: 1, maxFailures: 5 }, retryStrategy: 'immediate' },
    topLevelProjects: [project], options: {}, reporter: {
      onTestBegin: (...args) => events.begin.push(args),
      onTestEnd: (...args) => events.end.push(args),
      onError: error => events.error.push(error),
      onStdOut: options.onStdOut
    },
    hasReachedMaxFailures: () => false,
    failedTestCount: 0,
    hasWorkerErrors: false
  };
  const dispatcher = new f.Dispatcher(testRun);
  const group = { workerHash: 'hash', requireFile: 'test.js', repeatEachIndex: 0, projectId: 'p', tests: [test] };
  return { dispatcher, group, test, testRun, events };
}
async function withoutUnhandledRejections(run) {
  const rejected = [];
  const listener = error => rejected.push(error);
  process.on('unhandledRejection', listener);
  try {
    const result = await run();
    await new Promise(resolve => setTimeout(resolve, 0));
    assert.equal(rejected.length, 0, rejected.map(String).join('\n'));
    return result;
  } finally {
    process.off('unhandledRejection', listener);
  }
}
function assertFailedDispatch(state, message) {
  assert.equal(state.test.results.length, 1);
  assert.equal(state.events.begin.length, 1);
  assert.equal(state.events.end.length, 1);
  assert.equal(state.test.results[0].status, 'failed');
  assert.match(state.test.results[0].error.message, message);
  assert.equal(state.dispatcher._queuedOrRunningHashCount.get('hash'), 0);
  assert.equal(state.dispatcher._workerSlots[0].jobDispatcher, undefined);
}
test('C54', 'actual Dispatcher settles synchronous admission refusal without retention', async () => withoutUnhandledRejections(async () => {
  const cause = new Error('synchronous admission refusal');
  const f = fixture({ admit: () => { throw cause; } });
  const state = dispatcherFixture(f);
  const run = state.dispatcher.run([state.group], new Map());
  await f.clock.tick(1000);
  await run;
  const outcomes = await state.dispatcher.stop();
  assertFailedDispatch(state, /synchronous admission refusal/);
  assert.equal(f.calls.admission, 1);
  assert.equal(f.calls.fork, 0);
  assert.equal(f.calls.retain, 0);
  incomplete(outcomes[0]);
  assert.ok(f.completion.snapshot().consumed.some(item => item.owner === 'Dispatcher.stop'));
  return f;
}));
test('C55', 'actual Dispatcher settles asynchronous admission refusal without retention', async () => withoutUnhandledRejections(async () => {
  const cause = new Error('asynchronous admission refusal');
  const f = fixture({ admit: () => Promise.reject(cause) });
  const state = dispatcherFixture(f);
  const run = state.dispatcher.run([state.group], new Map());
  await f.clock.tick(1000);
  await run;
  const outcomes = await state.dispatcher.stop();
  assertFailedDispatch(state, /asynchronous admission refusal/);
  assert.equal(f.calls.admission, 1);
  assert.equal(f.calls.fork, 0);
  assert.equal(f.calls.retain, 0);
  incomplete(outcomes[0]);
  assert.ok(f.completion.snapshot().consumed.some(item => item.owner === 'Dispatcher.stop'));
  return f;
}));
test('C56', 'actual Dispatcher settles synchronous fork failure and retains admitted paths', async () => withoutUnhandledRejections(async () => {
  const f = fixture({ forkThrow: true });
  const state = dispatcherFixture(f);
  const run = state.dispatcher.run([state.group], new Map());
  await f.clock.tick(1000);
  await run;
  const outcomes = await state.dispatcher.stop();
  assertFailedDispatch(state, /fork failed/);
  assert.equal(f.calls.admission, 1);
  assert.equal(f.calls.fork, 1);
  assert.equal(f.calls.retain, 1);
  incomplete(outcomes[0]);
  assert.ok(f.completion.snapshot().consumed.some(item => item.owner === 'WorkerHost.stop'));
  assert.ok(f.completion.snapshot().consumed.some(item => item.owner === 'Dispatcher.stop'));
  return f;
}));
test('C57', 'actual Dispatcher consumes returned asynchronous startup error', async () => withoutUnhandledRejections(async () => {
  const f = fixture({ child: child => child.pid = undefined });
  const state = dispatcherFixture(f);
  const run = state.dispatcher.run([state.group], new Map());
  await flush();
  const child = f.children.at(-1);
  child.emit('error', new Error('asynchronous spawn failure'));
  f.close(child);
  await f.clock.tick(1000);
  await run;
  const outcomes = await state.dispatcher.stop();
  assertFailedDispatch(state, /asynchronous spawn failure/);
  assert.equal(f.calls.retain, 1);
  incomplete(outcomes[0]);
  return f;
}));
test('C58', 'actual Dispatcher stop during admission prevents a late fork and retains admitted partials', async () => withoutUnhandledRejections(async () => {
  const admission = deferred();
  const f = fixture({ admit: () => admission.promise });
  const state = dispatcherFixture(f);
  const run = state.dispatcher.run([state.group], new Map());
  await flush();
  assert.equal(f.calls.fork, 0);
  const stop = state.dispatcher.stop();
  admission.resolve();
  await f.clock.tick(1000);
  await run;
  const outcomes = await stop;
  assertFailedDispatch(state, /cancelled after stop/);
  assert.equal(f.calls.fork, 0);
  assert.equal(f.calls.retain, 1);
  incomplete(outcomes[0]);
  return f;
}));
test('C59', 'actual Dispatcher handles a synchronous failure before any host exists', async () => withoutUnhandledRejections(async () => {
  const cause = new Error('serialization failed before worker creation');
  const f = fixture({ serializeConfig: () => { throw cause; } });
  const state = dispatcherFixture(f);
  await state.dispatcher.run([state.group], new Map());
  await state.dispatcher.stop();
  assertFailedDispatch(state, /serialization failed before worker creation/);
  assert.equal(f.completion.snapshot().hosts.length, 0);
  assert.equal(f.calls.admission, 0);
  assert.equal(f.calls.fork, 0);
  assert.equal(f.calls.retain, 0);
  return f;
}));
test('C60', 'actual Dispatcher run keeps a late replaced-worker exit from clearing the new slot owner', async () => {
  const f = fixture();
  const state = dispatcherFixture(f);
  const makeTest = id => ({ id, expectedStatus: 'passed', retries: 0, results: [], annotations: [], parent: null,
    _appendTestResult() { const result = { stdout: [], stderr: [], steps: [], attachments: [] }; this.results.push(result); return result; },
    outcome() { return this.results.at(-1)?.status === this.expectedStatus ? 'expected' : 'unexpected'; }
  });
  const makeGroup = (id, workerHash) => ({ ...state.group, workerHash, tests: [makeTest(id)] });
  const groups = [makeGroup('first-test', 'first-hash'), makeGroup('second-test', 'second-hash')];
  const workers = [];
  state.dispatcher._isWorkerRedundant = () => false;
  state.dispatcher._createWorker = (job, index) => {
    const worker = new EventEmitter();
    worker.workerIndex = index; worker.parallelIndex = index;
    worker.hash = () => job.workerHash;
    worker.didSendStop = () => false;
    worker.start = async () => undefined;
    worker.stop = async () => undefined;
    worker.runTestGroup = payload => queueMicrotask(() => {
      for (const entry of payload.entries) {
        worker.emit('testBegin', { testId: entry.testId, startWallTime: Date.now() });
        worker.emit('testEnd', { testId: entry.testId, duration: 0, errors: [], status: 'passed', annotations: [], expectedStatus: 'passed', timeout: 0 });
      }
      worker.emit('done', { fatalErrors: [], skipTestsDueToSetupFailure: [] });
    });
    workers.push(worker);
    return worker;
  };
  await state.dispatcher.run(groups, new Map());
  assert.equal(workers.length, 2);
  const current = state.dispatcher._workerSlots[0].worker;
  assert.strictEqual(current, workers[1]);
  workers[0].emit('exit', { unexpectedly: true });
  assert.strictEqual(state.dispatcher._workerSlots[0].worker, current);
  assert.equal(state.events.begin.length, 2);
  assert.equal(state.events.end.length, 2);
  return f;
});
async function startLoader(f) {
  const loader = new f.OutOfProcessLoaderHost({});
  const start = loader.start([]);
  const child = f.children.at(-1);
  child.emit('message', { method: 'ready' });
  assert.equal(await start, true);
  return { loader, child, host: loader._processHost };
}
test('C61', 'loader sends one owned stop after its join cutoff but before parent deadline', async () => {
  const f = fixture({ deadline: 2000 });
  const { loader, child, host } = await startLoader(f);
  const stop = loader.stop();
  stop.catch(() => {});
  await f.clock.tick(1000);
  assert.equal(child.sends.filter(s => s.message.method === '__stop__').length, 1);
  const beforeProbe = child.sends.length;
  await assert.rejects(host.send({ method: 'general-after-cutoff' }), /deadline/);
  assert.equal(child.sends.length, beforeProbe);
  await f.clock.tick(2000);
  await assert.rejects(stop, /loader-cache-consumer/);
  const outcome = host._completion.result;
  incomplete(outcome);
  assert.equal(outcome.deadline, 1000);
  return f;
});
test('C62', 'expired loader parent deadline sends no graceful stop', async () => {
  const f = fixture({ deadline: 100 });
  const { loader, child, host } = await startLoader(f);
  const stop = loader.stop();
  stop.catch(() => {});
  await f.clock.tick(100);
  await assert.rejects(stop, /loader-cache-consumer/);
  assert.equal(child.sends.filter(s => s.message.method === '__stop__').length, 0);
  incomplete(host._completion.result);
  assert.equal(host._completion.result.deadline, 100);
  return f;
});
test('C63', 'disconnected loader refuses its owned stop send and stays incomplete', async () => {
  const f = fixture({ deadline: 2000 });
  const { loader, child, host } = await startLoader(f);
  const stop = loader.stop();
  stop.catch(() => {});
  child.connected = false;
  await f.clock.tick(1000);
  assert.equal(child.sends.filter(s => s.message.method === '__stop__').length, 0);
  await f.clock.tick(2000);
  await assert.rejects(stop, /loader-cache-consumer/);
  const outcome = host._completion.result;
  incomplete(outcome);
  assert.ok(outcome.errors.some(error => error.error.includes('not connected')));
  return f;
});
test('C64', 'owned loader stop callback error is observed after the join cutoff', async () => {
  const f = fixture({ deadline: 2000 });
  const { loader, child, host } = await startLoader(f);
  const stop = loader.stop();
  stop.catch(() => {});
  child.mode = 'error';
  await f.clock.tick(1000);
  assert.equal(child.sends.filter(s => s.message.method === '__stop__').length, 1);
  await f.clock.tick(2000);
  await assert.rejects(stop, /loader-cache-consumer/);
  const outcome = host._completion.result;
  incomplete(outcome);
  assert.ok(outcome.errors.some(error => error.error.includes('send failed')));
  return f;
});
test('C65', 'repeated loader stop sends once and late callbacks cannot promote the frozen result', async () => {
  const f = fixture({ deadline: 2000 });
  const { loader, child, host } = await startLoader(f);
  child.mode = 'pending';
  const stop = loader.stop();
  stop.catch(() => {});
  const repeatedStop = loader.stop();
  assert.strictEqual(repeatedStop, stop);
  assert.equal(child.sends.filter(s => s.message.method === '__dispatch__' && s.message.params.method === 'getCompilationCacheFromLoader').length, 1);
  await f.clock.tick(1000);
  const graceful = child.sends.filter(s => s.message.method === '__stop__');
  assert.equal(graceful.length, 1);
  await f.clock.tick(2000);
  await assert.rejects(repeatedStop, /loader-cache-consumer/);
  const outcome = host._completion.result;
  incomplete(outcome);
  assert.strictEqual(await host.stop(), outcome);
  assert.equal(child.sends.filter(s => s.message.method === '__stop__').length, 1);
  for (const sent of child.sends) sent.callback?.();
  child.emit('exit', 0, null);
  f.close(child);
  await flush();
  assert.strictEqual(host._completion.result, outcome);
  incomplete(host._completion.result);
  assert.ok(host._completion.late.length > 0);
  return f;
});
test('C66', 'actual loadFileSuites preserves both exact load and finalization error identities', async () => withoutUnhandledRejections(async () => {
  const primary = new Error('primary load object');
  const cleanup = new Error('finalization object');
  const f = fixture({ cache: () => { throw cleanup; } });
  const originalLoad = f.OutOfProcessLoaderHost.prototype.loadTestFile;
  f.OutOfProcessLoaderHost.prototype.loadTestFile = async () => { throw primary; };
  const errors = [];
  const run = f.loadFileSuites({ config: {}, projectFiles: new Map([['p', ['file']]]), projectSuites: new Map() }, 'out-of-process', errors);
  run.catch(() => {});
  const child = f.children.at(-1);
  child.onSend = message => {
    if (message.method === '__dispatch__')
      child.emit('message', { method: '__dispatch__', params: { id: message.params.id, result: {} } });
    if (message.method === '__stop__') {
      child.emit('exit', 0, null);
      f.close(child);
    }
  };
  child.emit('message', { method: 'ready' });
  await assert.rejects(run, error => error instanceof AggregateError && error.errors[0] === primary && error.errors[1] === cleanup && error.cause === primary);
  assert.ok(f.completion.snapshot().consumed.some(item => item.owner === 'OutOfProcessLoaderHost.stop'));
  incomplete(f.completion.snapshot().hosts[0]);
  assert.equal(child.sends.filter(s => s.message.method === '__stop__').length, 1);
  f.OutOfProcessLoaderHost.prototype.loadTestFile = originalLoad;
  return f;
}));
test('C67', 'late successful admission retains owned partials without reviving the frozen host result', async () => withoutUnhandledRejections(async () => {
  const admission = deferred();
  const f = fixture({ deadline: 5000, admit: () => admission.promise });
  const state = dispatcherFixture(f);
  const run = state.dispatcher.run([state.group], new Map());
  run.catch(() => {});
  await flush();
  const worker = state.dispatcher._workerSlots[0].worker;
  const stop = state.dispatcher.stop();
  await f.clock.tick(1000);
  const outcomes = await stop;
  incomplete(outcomes[0]);
  assert.equal(f.calls.retain, 0);
  admission.resolve();
  await run;
  await flush();
  assertFailedDispatch(state, /cancelled after stop/);
  assert.equal(f.calls.fork, 0);
  assert.equal(f.calls.retain, 1);
  assert.strictEqual(worker._completion.result, outcomes[0]);
  incomplete(worker._completion.result);
  return f;
}));
test('C68', 'actual Dispatcher stop bounds never-settling worker admission and settles its slot', async () => withoutUnhandledRejections(async () => {
  const admission = deferred();
  const f = fixture({ admit: () => admission.promise });
  const state = dispatcherFixture(f);
  const run = state.dispatcher.run([state.group], new Map());
  await flush();
  assert.equal(f.calls.admission, 1);
  const stop = state.dispatcher.stop();
  await f.clock.tick(1000);
  const outcomes = await stop;
  await run;
  assertFailedDispatch(state, /cancelled after stop/);
  assert.equal(f.calls.fork, 0);
  assert.equal(f.calls.retain, 0);
  incomplete(outcomes[0], 'worker-admission');
  assert.equal(state.dispatcher._queuedOrRunningHashCount.get('hash'), 0);
  assert.ok(f.completion.snapshot().consumed.some(item => item.owner === 'Dispatcher.stop'));
  return f;
}));
test('C69', 'actual WorkerHost exit after frozen stop retains admitted path without promoting outcome', async () => {
  const f = fixture({ deadline: 1000 });
  const worker = new f.WorkerHost({ workerHash: 'x', projectId: 'p' }, { parallelIndex: 0, outputDir: '/private', extraEnv: {} });
  const start = worker.start();
  await flush();
  const child = f.children.at(-1);
  child.emit('message', { method: 'ready' });
  assert.equal(await start, undefined);
  const stop = worker.stop();
  stop.catch(() => {});
  await f.clock.tick(1000);
  const outcome = await stop;
  incomplete(outcome);
  assert.equal(f.calls.retain, 0);
  child.emit('exit', 0, null);
  await flush();
  assert.equal(f.calls.retain, 1);
  assert.strictEqual(worker._completion.result, outcome);
  incomplete(worker._completion.result);
  f.close(child);
  await flush();
  assert.strictEqual(worker._completion.result, outcome);
  return f;
});
test('C70', 'case watchdog timeout is attempt-fatal and skips later cases', async () => {
  let timedOut = false;
  const watchdog = createCaseWatchdog(() => new Promise(() => {}), 'timeout-probe', 1, () => { timedOut = true; });
  await assert.rejects(watchdog.promise, /Harness case watchdog: timeout-probe/);
  watchdog.clear();
  assert.equal(timedOut, true);
  const visited = [];
  const results = await runUntilAttemptFatal([
    { id: 'before-timeout' },
    { id: 'timed-out' },
    { id: 'after-timeout' }
  ], async entry => {
    visited.push(entry.id);
    return { result: { id: entry.id }, attemptFatal: entry.id === 'timed-out' && timedOut };
  });
  assert.deepEqual(visited, ['before-timeout', 'timed-out']);
  assert.deepEqual(results.map(result => result.id), visited);
  return fixture();
});
test('C71', 'actual loadFileSuites preserves returned loader start error when cleanup fails', async () => withoutUnhandledRejections(async () => {
  const primary = new Error('returned loader startup object');
  const cleanup = new Error('loader cleanup object');
  const f = fixture({ child: child => { child.pid = undefined; } });
  const originalStop = f.ProcessHost.prototype.stop;
  f.ProcessHost.prototype.stop = async function(...args) {
    const outcome = await originalStop.apply(this, args);
    if (this._processName === 'loader') throw cleanup;
    return outcome;
  };
  const errors = [];
  const run = f.loadFileSuites({ config: {}, projectFiles: new Map([['p', ['file']]]), projectSuites: new Map() }, 'out-of-process', errors);
  run.catch(() => {});
  await flush();
  const child = f.children.at(-1);
  child.emit('error', primary);
  f.close(child);
  await assert.rejects(run, error => error instanceof AggregateError && error.errors[0] === primary && error.errors[1] === cleanup && error.cause === primary);
  assert.ok(errors.some(error => error.message.includes('Test loader process failed to start')));
  assert.ok(f.completion.snapshot().consumed.some(item => item.owner === 'ProcessHost.stop'));
  incomplete(f.completion.snapshot().hosts[0]);
  return f;
}));
test('C72', 'completion recovery uses the outer run identity instead of nested history', async () => {
  const command = 'bun frontend/scripts/private-qualification/check.mjs --phase completion';
  const callsite = 'shell/callsite%3A842%3A865/';
  const outer = { runId: '276bbd18-5112-4df9-85d9-0a3879e82fdc', root: '/tmp/herdr-retention-v3-276bbd18-5112-4df9-85d9-0a3879e82fdc', status: 'passed', generated: { priorHistory: { invocations: [{ runId: '38f3f4e1-d7a3-4990-bd66-81398e0a075c' }] } } };
  const later = { runId: '52dc677b-194e-47be-8c7f-677946cb112f', root: '/tmp/herdr-retention-v3-52dc677b-194e-47be-8c7f-677946cb112f', status: 'passed' };
  const firstPath = callsite + 'occurrence%3A1';
  const secondPath = callsite + 'occurrence%3A2';
  const document = { journal: { completed: {
    [firstPath]: { path: firstPath, value: { exitCode: 0, stdout: JSON.stringify(outer), stderr: '' } },
    [secondPath]: { path: secondPath, value: { exitCode: 0, stdout: JSON.stringify(later), stderr: '' } },
    'shell/callsite%3A999%3A1000/occurrence%3A1': { path: 'shell/callsite%3A999%3A1000/occurrence%3A1', value: { exitCode: 0, stdout: JSON.stringify({ runId: '38f3f4e1-d7a3-4990-bd66-81398e0a075c', root: '/tmp/herdr-retention-v3-38f3f4e1-d7a3-4990-bd66-81398e0a075c' }), stderr: '' } }
  } } };
  const recoveredResults = extractCompletionGateShellResults(document, command, callsite, 2);
  assert.equal(recoveredResults.length, 2);
  const [recovered] = recoveredResults;
  assert.equal(recovered.summary.runId, outer.runId);
  assert.equal(recovered.summary.root, outer.root);
  assert.notEqual(recovered.summary.runId, outer.generated.priorHistory.invocations[0].runId);
  const [directJournalResult] = extractCompletionGateShellResults({ completed: document.journal.completed }, command, callsite, 2);
  assert.equal(directJournalResult.summary.runId, outer.runId);
  return fixture();
});
test('C73', 'controller associations count validated known roots once and reload without duplicate appends', async () => {
  const callsite = 'shell/callsite%3A842%3A865/';
  const first = { runId: '00000000-0000-4000-8000-000000000073', root: '/tmp/herdr-retention-v3-00000000-0000-4000-8000-000000000073', status: 'passed', stage: 'completion', expectedCases: 1, expectedVariants: 1, caseOutcomes: { cases: 1, passed: 1, failed: 0, caseIds: ['C01'], failedIds: [], unexecutedCases: 0 }, artifacts: [{ path: 'summary.json', sha256: 'a'.repeat(64), bytes: 1 }], retainedInInvocationJournal: false };
  const second = { runId: '00000000-0000-4000-8000-000000000074', root: '/tmp/herdr-retention-v3-00000000-0000-4000-8000-000000000074', status: 'passed', stage: 'completion', expectedCases: 1, expectedVariants: 1, caseOutcomes: { cases: 1, passed: 1, failed: 0, caseIds: ['C01'], failedIds: [], unexecutedCases: 0 }, artifacts: [{ path: 'summary.json', sha256: 'b'.repeat(64), bytes: 1 }] };
  const candidateFor = (record, index, digest) => {
    const event = { occurrence: 'occurrence%3A' + index, runId: record.runId, root: record.root, status: record.status, exitCode: 0, stdoutSHA256: digest.repeat(64), stdoutBytes: 256 + index };
    const eventIdentity = controllerShellAssociationIdentity(event);
    return { ...event, path: callsite + event.occurrence, invocation: { ...record, provenance: { kind: 'controller-shell-recovery', associationId: eventIdentity, invocationLedger: { status: 'validated' } } } };
  };
  const candidates = [candidateFor(first, 1, 'c'), candidateFor(second, 2, 'd')];
  const reconciled = reconcileControllerGateAssociations(candidates, [first], 2);
  assert.deepEqual(reconciled.associations.map(item => item.classification), ['known-fixed-history', 'new-root']);
  assert.equal(reconciled.newRecords.length, 1);
  const manifestAssociations = reconciled.associations.map(({ invocation, ...association }) => ({ ...association, classification: association.classification === 'new-root' ? 'new-root-appended' : association.classification }));
  const appendedSecond = { ...reconciled.newRecords[0], retainedInInvocationJournal: true };
  assert.equal(validateControllerRecoveryAssociations(manifestAssociations, [first, appendedSecond], 2).length, 2);
  assert.equal(reuseControllerRecoveryManifest(null, [first, appendedSecond], 2), null);
  const storedManifest = {
    associations: manifestAssociations,
    recoveredRunIds: manifestAssociations.map(item => item.runId),
    newlyAppendedRunIds: manifestAssociations.filter(item => item.classification === 'new-root-appended').map(item => item.runId),
    manifestIdentity: { path: 'controller-recovery.json', sha256: 'f'.repeat(64), bytes: 512 }
  };
  const reusedManifest = reuseControllerRecoveryManifest(storedManifest, [first, appendedSecond], 2);
  assert.equal(reusedManifest.rootedInvocationCount, 2);
  assert.deepEqual(reusedManifest.appendRecords, []);
  assert.deepEqual(reusedManifest.newlyAppendedRunIds, [second.runId]);
  assert.equal(reusedManifest.manifestIdentity.sha256, 'f'.repeat(64));
  assert.throws(() => reuseControllerRecoveryManifest(storedManifest, [{ ...first, artifacts: [{ path: 'summary.json', sha256: 'e'.repeat(64), bytes: 1 }] }, appendedSecond], 2), /does not match a unique retained invocation/);
  const reloaded = reconcileControllerGateAssociations(candidates, [first, appendedSecond], 2);
  assert.equal(reloaded.newRecords.length, 0);
  assert.deepEqual(reloaded.associations.map(item => item.classification), ['known-fixed-history', 'known-invocation-journal']);
  const commandAccounting = summarizeCommandAttempts([first, appendedSecond], [
    { identity: 'controller-event:unrooted-1', status: 'failed', unexecutedCaseSlots: 75 },
    { identity: 'controller-event:unrooted-2', status: 'failed', unexecutedCaseSlots: 75 }
  ], 2, manifestAssociations);
  assert.equal(commandAccounting.rootedInvocations, 2);
  assert.equal(commandAccounting.reconciledControllerGates, 2);
  assert.equal(commandAccounting.reconciledCommandAttempts, 4);
  assert.equal(commandAccounting.unrootedUnexecutedCaseSlots, 150);
  assert.throws(() => reconcileControllerGateAssociations(candidates, [{ ...first, artifacts: [{ path: 'summary.json', sha256: 'f'.repeat(64), bytes: 1 }] }], 2), /conflicts with retained accounting/);
  assert.throws(() => reconcileControllerGateAssociations([candidates[0], candidateFor(first, 2, 'e')], [], 2), /repeated or conflicting outer invocation identity/);
  return fixture();
});
test('C74', 'completion stdout projects production accounting into a bounded evidence pointer', async () => {
  const evidenceLimit = 16 * 1024 * 1024;
  const evidenceStat = size => ({ isFile: true, isSymbolicLink: false, uid: 502, mode: 33152, size, dev: '1', ino: '2', mtimeNs: '3', ctimeNs: '4' });
  const evidenceAtLimit = evidenceStat(evidenceLimit);
  assert.equal(validateBoundedEvidenceSnapshot(evidenceAtLimit, 502, evidenceLimit), evidenceAtLimit);
  assert.deepEqual(validateBoundedEvidenceRead(evidenceAtLimit, evidenceStat(evidenceLimit), evidenceLimit, 502, evidenceLimit), { size: evidenceLimit });
  assert.throws(() => validateBoundedEvidenceSnapshot(evidenceStat(evidenceLimit + 1), 502, evidenceLimit), /configured byte limit/);
  assert.throws(() => validateBoundedEvidenceSnapshot({ ...evidenceAtLimit, uid: 501 }, 502, evidenceLimit), /identity mismatch/);
  assert.throws(() => validateBoundedEvidenceRead(evidenceAtLimit, { ...evidenceAtLimit, ctimeNs: '5' }, evidenceLimit, 502, evidenceLimit), /changed while being read/);
  const events = [
    { identity: 'controller-event:1ad22fd0-dd89-45d5-be90-cc685cbe62a9:T001:step-0009:worker-turn-5:completion-module-parse-failure', classification: 'controller-qualified-unrooted-command-event', command: 'bun frontend/scripts/private-qualification/check.mjs --phase completion', status: 'failed', stage: 'module-parse-before-check-entry', diagnostic: 'tests.mjs:511:7: Expected ";" but found "$"', executedCases: 0, unexecutedCaseSlots: 75, receiptRootAllocated: false, runId: null, beginReceipt: false, evidenceClassification: 'Worker-reported retained step-9 failure', evidencePath: '/Users/christophe.vidal/.pi/workflows/projects/herdr-mobile-relay-8c5d7f53c701/sessions/01a0fd10-fe99-772d-b993-10054f3b02b5/runs/1ad22fd0-dd89-45d5-be90-cc685cbe62a9/orchestration/tasks/T001/steps/0009.json' },
    { identity: 'controller-event:1ad22fd0-dd89-45d5-be90-cc685cbe62a9:T001:recovery-3:worker-turn-6:completion-baseline-parse-failure', classification: 'controller-qualified-unrooted-command-event', command: 'bun frontend/scripts/private-qualification/check.mjs --phase completion', status: 'failed', stage: 'baseline-parse-before-check-entry', diagnostic: 'generate.mjs:7:31: SyntaxError: JSON Parse error: Expected }', executedCases: 0, unexecutedCaseSlots: 75, receiptRootAllocated: false, runId: null, beginReceipt: false, evidenceClassification: 'Observed exact completion command output in worker turn 6', evidenceSource: 'Current T001 recovery-3 worker command result' }
  ];
  const commandAttemptAccounting = {
    rootedInvocations: 25, unrootedCommandEvents: events, unrootedFailedAttempts: 2, unrootedUnexecutedCaseSlots: 150,
    reconciledControllerGates: 2, unreconciledControllerGates: 0, reconciledCommandAttempts: 27, expectedControllerGateCount: 2
  };
  const accounting = projectCompletionAccounting({
    commandAttemptAccounting,
    cumulativeRootedInvocations: 25,
    ledgerAppend: { root: '/tmp/herdr-retention-v3-accounting-1ad22fd0-dd89-45d5-be90-cc685cbe62a9', path: 'invocations.ndjson', sequence: 10, sha256: 'f'.repeat(64), bytes: 4096 },
    recoveryManifest: { path: 'controller-recovery.json', sha256: 'e'.repeat(64), bytes: 8192 },
    recoveredRunIds: ['00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002'],
    allowanceProjection: { replacementRootedInvocations: 26, replacementTotalCommandAttempts: 28, afterSubsequentControllerRootedInvocations: 27, afterSubsequentControllerTotalCommandAttempts: 29, notVerifiedTotals: true, engineUsageInferred: false, capsRaised: false, countersReset: false }
  });
  const pointer = formatCompletionPointer({
    status: 'passed', runId: '276bbd18-5112-4df9-85d9-0a3879e82fdc', root: '/tmp/herdr-retention-v3-276bbd18-5112-4df9-85d9-0a3879e82fdc', stage: 'completion',
    sourceTests: { cases: 79, passed: 79, failed: 0, variants: 80 },
    summary: { path: 'summary.json', sha256: 'a'.repeat(64), bytes: 100000 },
    terminalReceipt: { path: 'completion-result.json', sha256: 'e'.repeat(64), bytes: 5000 },
    caseReceipt: { path: 'attempt-06.ndjson', sha256: 'b'.repeat(64), bytes: 50000 },
    caseSummary: { path: 'attempt-06.summary.json', sha256: 'c'.repeat(64), bytes: 100 },
    invocationLedger: { path: 'invocation-ledger.json', sha256: 'd'.repeat(64), bytes: 5000 },
    accounting
  });
  assert.ok(Buffer.byteLength(pointer) < 3000);
  const parsed = JSON.parse(pointer);
  assert.equal(parsed.status, 'passed');
  assert.equal(parsed.stage, 'completion');
  assert.equal(parsed.runId, '276bbd18-5112-4df9-85d9-0a3879e82fdc');
  assert.equal(parsed.root, '/tmp/herdr-retention-v3-276bbd18-5112-4df9-85d9-0a3879e82fdc');
  assert.equal(parsed.sourceTests.cases, 79);
  assert.equal(parsed.sourceTests.variants, 80);
  assert.equal(parsed.summary.path, 'summary.json');
  assert.equal(parsed.terminalReceipt.path, 'completion-result.json');
  assert.equal(parsed.invocationLedger.path, 'invocation-ledger.json');
  assert.match(parsed.summary.sha256, /^[a-f0-9]{64}$/);
  assert.equal(parsed.accounting.rootedInvocations, 25);
  assert.equal(parsed.accounting.cumulativeRootedInvocations, 25);
  assert.equal(parsed.accounting.reconciledCommandAttempts, 27);
  assert.equal(parsed.accounting.unrootedFailedAttempts, 2);
  assert.equal(parsed.accounting.unrootedUnexecutedCaseSlots, 150);
  assert.equal(parsed.accounting.reconciledControllerGates, 2);
  assert.equal(parsed.accounting.unreconciledControllerGates, 0);
  assert.deepEqual(parsed.accounting.unrootedEventIds, events.map(event => event.identity));
  assert.equal(parsed.accounting.ledgerAppend.path, 'invocations.ndjson');
  assert.equal(parsed.accounting.ledgerAppend.sequence, 10);
  assert.equal(parsed.accounting.ledgerAppend.sha256, 'f'.repeat(64));
  assert.deepEqual(parsed.accounting.recoveredRunIds, ['00000000-0000-4000-8000-000000000001', '00000000-0000-4000-8000-000000000002']);
  assert.equal(parsed.accounting.recoveryManifest.path, 'controller-recovery.json');
  assert.equal(parsed.accounting.recoveryManifest.sha256, 'e'.repeat(64));
  assert.equal(parsed.accounting.allowanceProjection.replacementRootedInvocations, 26);
  assert.equal(parsed.accounting.allowanceProjection.replacementTotalCommandAttempts, 28);
  assert.equal(parsed.accounting.allowanceProjection.afterSubsequentControllerRootedInvocations, 27);
  assert.equal(parsed.accounting.allowanceProjection.afterSubsequentControllerTotalCommandAttempts, 29);
  assert.equal(parsed.accounting.allowanceProjection.notVerifiedTotals, true);
  assert.equal(Object.hasOwn(parsed.accounting, 'unrootedCommandEvents'), false);
  assert.equal(JSON.stringify(parsed).includes('diagnostic'), false);
  assert.equal(JSON.stringify(parsed).includes('Both exact controller gates'), false);
  assert.equal(events[0].diagnostic.length > 0, true);
  const failedPointer = JSON.parse(formatCompletionPointer({
    status: 'failed', runId: '00000000-0000-4000-8000-000000000074', root: '/tmp/herdr-retention-v3-00000000-0000-4000-8000-000000000074', stage: 'generation-module-initialization',
    sourceTests: null,
    summary: { path: 'summary.json', sha256: 'c'.repeat(64), bytes: 1200 },
    terminalReceipt: { path: 'failure.json', sha256: 'd'.repeat(64), bytes: 800 },
    caseReceipt: null, caseSummary: null, invocationLedger: null,
    accounting: projectCompletionAccounting({ commandAttemptAccounting: null })
  }));
  assert.equal(failedPointer.stage, 'generation-module-initialization');
  assert.equal(failedPointer.terminalReceipt.path, 'failure.json');
  assert.equal(failedPointer.accounting.available, false);
  assert.equal(failedPointer.accounting.unavailableReason, 'accounting-unavailable');
  const appendIdentity = { path: 'invocations.ndjson', sequence: 21, sha256: 'f'.repeat(64), bytes: 512 };
  const reconciliationError = new Error('Exact post-append history could not be reconciled');
  const accountingAttempt = reconcileInvocationAccounting(() => appendIdentity, () => { throw reconciliationError; });
  assert.equal(accountingAttempt.status, 'incomplete');
  assert.equal(accountingAttempt.appendResult, appendIdentity);
  assert.equal(accountingAttempt.appendError, null);
  assert.equal(accountingAttempt.reconciliationError, reconciliationError);
  assert.equal(accountingAttempt.reconciled, null);
  const failedSummary = {
    status: 'failed', stage: 'post-run-accounting-reconciliation', accountingStatus: accountingAttempt.status,
    appendResult: accountingAttempt.appendResult, appendError: null, reconciliationError: String(accountingAttempt.reconciliationError)
  };
  const incompleteAccounting = projectCompletionAccounting({
    commandAttemptAccounting: null, cumulativeRootedInvocations: null, ledgerAppend: appendIdentity,
    recoveryManifest: null, recoveredRunIds: ['00000000-0000-4000-8000-000000000021'], accountingStatus: accountingAttempt.status
  });
  const accountingFailurePointer = JSON.parse(formatCompletionPointer({
    status: 'failed', runId: '00000000-0000-4000-8000-000000000021', root: '/tmp/herdr-retention-v3-00000000-0000-4000-8000-000000000021', stage: failedSummary.stage,
    sourceTests: { cases: 75, passed: 75, failed: 0, variants: 76 },
    summary: { path: 'summary.json', sha256: '9'.repeat(64), bytes: Buffer.byteLength(JSON.stringify(failedSummary)) },
    terminalReceipt: { path: 'failure.json', sha256: '8'.repeat(64), bytes: 512 },
    caseReceipt: { path: 'attempt-06.ndjson', sha256: '7'.repeat(64), bytes: 1024 },
    caseSummary: { path: 'attempt-06.summary.json', sha256: '6'.repeat(64), bytes: 128 },
    invocationLedger: null, accounting: incompleteAccounting
  }));
  assert.equal(accountingFailurePointer.accounting.available, false);
  assert.equal(accountingFailurePointer.accounting.status, 'incomplete');
  assert.equal(accountingFailurePointer.accounting.unavailableReason, 'accounting-incomplete');
  assert.equal(accountingFailurePointer.accounting.ledgerAppend.sequence, 21);
  assert.equal(accountingFailurePointer.accounting.recoveredRunIds[0], '00000000-0000-4000-8000-000000000021');
  assert.ok(Buffer.byteLength(JSON.stringify(accountingFailurePointer)) < 3000);
  assert.equal(failedSummary.reconciliationError, 'Error: Exact post-append history could not be reconciled');
  return fixture();
});
test('C75', 'pre-ledger completion evidence is explicitly marked rather than requiring a fabricated ledger', async () => {
  const expected = {
    command: 'bun frontend/scripts/private-qualification/check.mjs --phase completion',
    runId: '00000000-0000-4000-8000-000000000075',
    root: '/tmp/herdr-retention-v3-00000000-0000-4000-8000-000000000075',
    status: 'passed', stage: 'completion', expectedCases: 1, expectedVariants: 1,
    caseOutcomes: { cases: 1, passed: 1, failed: 0, caseIds: ['C01'], failedIds: [], unexecutedCases: 0 },
    artifacts: [{ path: 'BEGIN.json', sha256: 'a'.repeat(64), bytes: 1 }]
  };
  const absent = assessRecoveredInvocationLedger(null, expected);
  assert.equal(absent.status, 'historically-absent');
  assert.match(absent.omission, /No invocation-ledger.json existed/);
  const recovered = deriveRecoveredInvocationRecord(expected, 'e'.repeat(64), { kind: 'controller-shell-recovery', invocationLedger: absent });
  assert.equal(recovered.runId, expected.runId);
  assert.deepEqual(recovered.artifacts, expected.artifacts);
  assert.equal(recovered.provenance.invocationLedger.status, 'historically-absent');
  const ledger = {
    schema: 1, command: expected.command, historicalRunIds: [],
    priorTotals: { invocations: 0, passed: 0, failed: 0, recordedCases: 0, passedCases: 0, failedCases: 0, unexecutedCases: 0, uniqueCaseIds: [] },
    current: { ...expected, artifacts: expected.artifacts },
    cumulative: { invocations: 1, passed: 1, failed: 0, recordedCases: 1, passedCases: 1, failedCases: 0, unexecutedCases: 0, uniqueCaseIds: ['C01'] }
  };
  assert.equal(assessRecoveredInvocationLedger(ledger, expected).status, 'validated');
  const preVariantLedger = structuredClone(ledger);
  delete preVariantLedger.current.expectedVariants;
  const assessedPreVariantLedger = assessRecoveredInvocationLedger(preVariantLedger, expected);
  assert.equal(assessedPreVariantLedger.status, 'validated-with-omissions');
  assert.match(assessedPreVariantLedger.omission, /omitted expectedVariants/);
  assert.throws(() => assessRecoveredInvocationLedger({ ...ledger, current: { ...ledger.current, status: 'failed' } }, expected), /conflicts with validated run evidence/);
  const zeroCaseFailure = {
    runId: '00000000-0000-4000-8000-000000000076', root: '/tmp/herdr-retention-v3-00000000-0000-4000-8000-000000000076',
    status: 'failed', stage: 'controller-gate-recovery', expectedCases: 75,
    caseOutcomes: { cases: 0, passed: 0, failed: 0, caseIds: [], failedIds: [], unexecutedCases: 75 }
  };
  const zeroCaseFailureEvidence = {
    record: zeroCaseFailure,
    failure: { runId: zeroCaseFailure.runId, status: 'failed', stage: zeroCaseFailure.stage },
    completion: null, caseReceipt: false, caseSummary: false, testConsole: false, testFailures: false, expectedCases: 75
  };
  assert.deepEqual(validatePriorAttemptTestEvidence(zeroCaseFailureEvidence), { execution: 'not-started', stageEvidence: 'matched' });
  assert.throws(() => validatePriorAttemptTestEvidence({ ...zeroCaseFailureEvidence, failure: null }), /Pre-test failure receipts disagree/);
  assert.throws(() => validatePriorAttemptTestEvidence({ ...zeroCaseFailureEvidence, caseReceipt: true }), /Pre-test failure receipts disagree/);
  assert.throws(() => validatePriorAttemptTestEvidence({ ...zeroCaseFailureEvidence, record: { ...zeroCaseFailure, stage: 'case-ledger-validation' }, failure: { ...zeroCaseFailureEvidence.failure, stage: 'case-ledger-validation' } }), /Executed prior invocation lacks its case receipt/);
  const events = [
    { identity: 'controller-event:pre-scope-1', status: 'failed', unexecutedCaseSlots: 75 },
    { identity: 'controller-event:pre-scope-2', status: 'failed', unexecutedCaseSlots: 75 }
  ];
  const invocation = (index, status, retainedInInvocationJournal = true) => {
    const runId = '00000000-0000-4000-8000-' + String(index).padStart(12, '0');
    return {
      runId, root: '/tmp/herdr-retention-v3-' + runId, status, stage: status === 'passed' ? 'completion' : 'source-generation',
      expectedCases: 75, expectedVariants: 76,
      caseOutcomes: { cases: 0, passed: 0, failed: 0, caseIds: [], failedIds: [], unexecutedCases: 75 },
      artifacts: [{ path: 'BEGIN.json', sha256: 'a'.repeat(64), bytes: 1 }], retainedInInvocationJournal
    };
  };
  const priorRoots = Array.from({ length: 19 }, (_, index) => invocation(index + 100, index < 8 ? 'passed' : 'failed', index !== 0));
  const existingControllerRoot = priorRoots[0];
  const newControllerRoot = invocation(119, 'passed', false);
  const shellCandidate = (record, occurrence, digest) => {
    const event = {
      occurrence: 'occurrence%3A' + occurrence, runId: record.runId, root: record.root, status: record.status,
      exitCode: record.status === 'passed' ? 0 : 1, stdoutSHA256: digest.repeat(64), stdoutBytes: 256 + occurrence
    };
    const eventIdentity = controllerShellAssociationIdentity(event);
    return {
      ...event, path: 'shell/callsite%3A842%3A865/' + event.occurrence,
      invocation: { ...record, provenance: { kind: 'controller-shell-recovery', associationId: eventIdentity, invocationLedger: { status: 'validated' } } }
    };
  };
  const shellCandidates = [shellCandidate(existingControllerRoot, 1, 'c'), shellCandidate(newControllerRoot, 2, 'd')];
  const associationResult = reconcileControllerGateAssociations(shellCandidates, priorRoots, 2);
  assert.equal(associationResult.newRecords.length, 1);
  const associations = associationResult.associations.map(({ invocation: recoveredInvocation, ...association }) => ({
    ...association, classification: association.classification === 'new-root' ? 'new-root-appended' : association.classification
  }));
  const appendedControllerRoot = { ...associationResult.newRecords[0], retainedInInvocationJournal: true };
  const rootsAfterRecovery = [...priorRoots, appendedControllerRoot];
  validateControllerRecoveryAssociations(associations, rootsAfterRecovery, 2);
  const afterRecovery = summarizeCommandAttempts(rootsAfterRecovery, events, 2, associations);
  assert.equal(afterRecovery.rootedInvocations, 20);
  assert.equal(afterRecovery.reconciledControllerGates, 2);
  assert.equal(afterRecovery.unreconciledControllerGates, 0);
  assert.equal(afterRecovery.reconciledCommandAttempts, 22);
  assert.equal(afterRecovery.unrootedUnexecutedCaseSlots, 150);
  const afterReplacement = summarizeCommandAttempts([...rootsAfterRecovery, invocation(120, 'passed')], events, 2, associations);
  assert.equal(afterReplacement.rootedInvocations, 21);
  assert.equal(afterReplacement.reconciledCommandAttempts, 23);
  const afterController = summarizeCommandAttempts([...rootsAfterRecovery, invocation(120, 'passed'), invocation(121, 'passed')], events, 2, associations);
  assert.equal(afterController.rootedInvocations, 22);
  assert.equal(afterController.reconciledCommandAttempts, 24);
  const afterReviewRepair = summarizeCommandAttempts([...rootsAfterRecovery, invocation(120, 'passed'), invocation(121, 'passed'), invocation(122, 'passed')], events, 2, associations);
  assert.equal(afterReviewRepair.rootedInvocations, 23);
  assert.equal(afterReviewRepair.reconciledCommandAttempts, 25);
  const afterReviewController = summarizeCommandAttempts([...rootsAfterRecovery, invocation(120, 'passed'), invocation(121, 'passed'), invocation(122, 'passed'), invocation(123, 'passed')], events, 2, associations);
  assert.equal(afterReviewController.rootedInvocations, 24);
  assert.equal(afterReviewController.reconciledCommandAttempts, 26);
  return fixture();
});
test('C76', 'actual Dispatcher settles the exact active job when bounded exit dispatch admission is exhausted', async () => withoutUnhandledRejections(async () => {
  const f = fixture({ deadline: 1000 });
  const state = dispatcherFixture(f);
  const run = state.dispatcher.run([state.group], new Map());
  let runOutcome;
  const observedRun = run.then(value => { runOutcome = { status: 'fulfilled', value }; }, error => { runOutcome = { status: 'rejected', error }; });
  const heldExit = deferred();
  let worker;
  let child;
  let stop;
  let frozenOutcome;
  try {
    await flush();
    worker = state.dispatcher._workerSlots[0].worker;
    child = f.children.at(-1);
    const jobDispatcher = state.dispatcher._workerSlots[0].jobDispatcher;
    assert.ok(worker);
    assert.ok(jobDispatcher);
    assert.strictEqual(jobDispatcher._completionWorker, worker);
    worker.setMaxListeners(0);
    for (let index = 0; index < 16; ++index) worker.on('exit', () => heldExit.promise);
    child.emit('message', { method: 'ready' });
    await flush();
    assert.ok(child.sends.some(sent => sent.message.method === '__dispatch__' && sent.message.params.method === 'runTestGroup'));
    child.emit('message', { method: 'testBegin', params: { testId: 'test-id', startWallTime: 0 } });
    await flush();
    stop = state.dispatcher.stop();
    await f.clock.tick(1000);
    const outcomes = await stop;
    incomplete(outcomes[0]);
    frozenOutcome = worker._completion.result;
    assert.ok(frozenOutcome);
    assert.strictEqual(jobDispatcher._completionWorker, worker);
    assert.equal(state.dispatcher._workerSlots[0].worker, undefined);
    assert.ok(jobDispatcher.jobResult.isDone());
    await flush();
    assert.equal(state.dispatcher._finished.isDone(), true);
    assert.equal(state.dispatcher._workerSlots[0].jobDispatcher, undefined);
    assert.equal(jobDispatcher._hashCountReleased, true);
    assertFailedDispatch(state, /Worker completion was incomplete/);
    assert.equal(state.events.end.length, 1);
    assert.equal(state.test.results[0].status, 'failed');
    assert.equal(state.dispatcher._queuedOrRunningHashCount.get('hash'), 0);
    assert.ok(f.completion.snapshot().consumed.some(item => item.owner === 'Dispatcher.stop'));
    assert.equal(f.calls.retain, 0);
    await flush();
    assert.equal(runOutcome?.status, 'fulfilled');
    await observedRun;
    child.emit('exit', 0, null);
    f.close(child);
    await flush();
    assert.strictEqual(worker._completion.result, frozenOutcome);
    assert.equal(state.events.end.length, 1);
    assert.equal(f.calls.retain, 1);
    return f;
  } finally {
    if (worker && !worker._completion.result) {
      stop ??= state.dispatcher.stop();
      await f.clock.tick(1000);
      await stop.catch(() => {});
    }
    heldExit.resolve();
    await flush();
    if (frozenOutcome) assert.strictEqual(worker._completion.result, frozenOutcome);
  }
}));
test('C77', 'owned graceful-stop sender is private and repeated direct attempts cannot bypass stop once-only state', async () => {
  const f = fixture({ deadline: 2000 });
  const { loader, child, host } = await startLoader(f);
  child.mode = 'pending';
  const stop = loader.stop();
  stop.catch(() => {});
  await f.clock.tick(1000);
  assert.equal(child.sends.filter(sent => sent.message.method === '__stop__').length, 1);
  const exposedSender = Reflect.get(host, '_sendOwnedStop');
  assert.equal(exposedSender, undefined);
  const directAttempt = exposedSender?.call(host);
  directAttempt?.catch(() => {});
  assert.equal(child.sends.filter(sent => sent.message.method === '__stop__').length, 1);
  assert.strictEqual(loader.stop(), stop);
  const repeatedHostStop = host.stop();
  repeatedHostStop.catch(() => {});
  assert.equal(child.sends.filter(sent => sent.message.method === '__stop__').length, 1);
  await f.clock.tick(2000);
  await assert.rejects(stop, /loader-cache-consumer/);
  const frozenOutcome = host._completion.result;
  incomplete(frozenOutcome);
  assert.equal(child.sends.filter(sent => sent.message.method === '__stop__').length, 1);
  for (const sent of child.sends) sent.callback?.();
  child.emit('exit', 0, null);
  f.close(child);
  await flush();
  assert.strictEqual(host._completion.result, frozenOutcome);
  assert.equal(child.sends.filter(sent => sent.message.method === '__stop__').length, 1);
  return f;
});
test('C78', 'finishTaskRun keeps joined failure through actual Multiplexer reporter ordering and LastRun', async () => {
  const f = fixture({ deadline: 1000 });
  const host = f.host();
  host._completion.begin('reporter-host-pending');
  const stop = host.stop();
  stop.catch(() => {});
  await f.clock.tick(1000);
  incomplete(await stop, 'reporter-host-pending');
  const project = { project: { outputDir: '/qualification-owned' } };
  const lastRun = new f.LastRunReporter([project], false, undefined);
  lastRun.onBegin({ allTests: () => [] });
  const observed = [];
  let exits = 0;
  const ordinaryReporter = {
    async onEnd(result) { observed.push(result.status); return { status: 'passed' }; },
    async onExit() { ++exits; }
  };
  const testRun = {
    result: () => 'passed',
    reporter: new f.Multiplexer([ordinaryReporter, lastRun])
  };
  assert.equal(await f.finishTaskRun(testRun, 'passed'), 'failed');
  assert.deepEqual(observed, ['failed']);
  assert.equal(exits, 1);
  assert.equal(f.lastRunWrites.length, 1);
  assert.equal(JSON.parse(f.lastRunWrites[0].content).status, 'failed');
  assert.equal(f.calls.native, 0);
  return f;
});
test('C79', 'actual Dispatcher refuses a successful admission that settles at the parent deadline before fork', async () => withoutUnhandledRejections(async () => {
  let f;
  let admittedAtDeadline = false;
  f = fixture({ deadline: 1000, admit: () => new Promise(resolve => f.clock.set(() => { admittedAtDeadline = true; resolve(); }, 1000)) });
  const state = dispatcherFixture(f);
  const run = state.dispatcher.run([state.group], new Map());
  let runOutcome;
  const observedRun = run.then(value => { runOutcome = { status: 'fulfilled', value }; }, error => { runOutcome = { status: 'rejected', error }; });
  await flush();
  const worker = state.dispatcher._workerSlots[0].worker;
  const jobDispatcher = state.dispatcher._workerSlots[0].jobDispatcher;
  assert.ok(worker);
  assert.ok(jobDispatcher);
  assert.strictEqual(jobDispatcher._completionWorker, worker);
  await f.clock.tick(1000);
  await flush();
  assert.equal(admittedAtDeadline, true);
  assert.equal(f.calls.admission, 1);
  assert.equal(f.calls.fork, 0);
  assert.equal(f.children.length, 0);
  assert.equal(runOutcome?.status, 'fulfilled');
  await observedRun;
  const outcomes = await state.dispatcher.stop();
  incomplete(outcomes[0]);
  assertFailedDispatch(state, /Worker admission parent deadline expired/);
  assert.equal(jobDispatcher._hashCountReleased, true);
  assert.equal(f.calls.retain, 1);
  assert.ok(worker._completion.result.errors.some(entry => entry.key === 'worker-admission-deadline'));
  assert.ok(f.completion.snapshot().consumed.some(item => item.owner === 'Dispatcher.stop'));
  assert.equal(f.calls.native, 0);
  return f;
}));
`;

const outputBudget = String.raw`const maximumSourceBytes = 64 * 1024 * 1024;
function assertOutputBudget(extraBytes = 0) {
  let logicalBytes = 0;
  let allocatedBytes = 0;
  let entries = 0;
  const inspect = (directory, depth) => {
    if (depth > 8) throw new Error('Source output depth limit exceeded');
    for (const name of readdirSync(directory)) {
      if (++entries > 256) throw new Error('Source output entry limit exceeded');
      const path = join(directory, name);
      const stat = lstatSync(path);
      if (stat.isSymbolicLink()) throw new Error('Unexpected source output symlink');
      if (stat.isDirectory()) inspect(path, depth + 1);
      else if (stat.isFile()) {
        logicalBytes += stat.size;
        allocatedBytes += Number(stat.blocks || Math.ceil(stat.size / 4096)) * 512;
      } else throw new Error('Unexpected source output file type');
      if (logicalBytes + extraBytes > maximumSourceBytes || allocatedBytes + extraBytes > maximumSourceBytes)
        throw new Error('64 MiB source output envelope exceeded');
    }
  };
  inspect(root, 0);
  const space = statfsSync(root);
  const available = Number(space.bavail) * Number(space.bsize);
  if (!Number.isFinite(available) || available < extraBytes)
    throw new Error('Source output headroom could not be established');
}`;
