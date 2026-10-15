export const ret2Edits = [
  {
    label: 'ProcessHost stop signal',
    oldText: String.raw`    this._extraEnv = env;`,
    newText: String.raw`    this._extraEnv = env;
    this._stopRequestedPromise = new Promise(resolve => this._resolveStopRequested = resolve);`
  },
  {
    label: 'ProcessHost stop state',
    oldText: String.raw`  async stop() {
    if (this._stopPromise) return this._stopPromise;
    this._stopPromise = this._stop();
    return this._stopPromise;
  }`,
    newText: String.raw`  async stop() {
    if (this._stopPromise) return this._stopPromise;
    this._stopRequested = true;
    this._resolveStopRequested();
    this._stopPromise = this._stop();
    return this._stopPromise;
  }`
  },
  {
    label: 'ProcessHost late onExit cleanup',
    oldText: String.raw`  _onExit() {
    if (this._onExitStarted) return;
    this._onExitStarted = true;
    try {
      this._completion.run("onExit", () => this.onExit()).then(() => {
        this._didExitAndRanOnExit = true;
      }).catch(() => {}).finally(() => this._announceExit());
    } catch (error) {
      this._completion.fail("onExit-admission", error);
      this._announceExit();
    }
  }`,
    newText: String.raw`  _onExit() {
    if (this._onExitStarted) return;
    this._onExitStarted = true;
    const observe = promise => {
      Promise.resolve(promise).then(() => {
        this._didExitAndRanOnExit = true;
      }, error => this._completion.fail(this._completion.result ? "late-onExit" : "onExit", error)).finally(() => this._announceExit()).catch(() => {});
    };
    try {
      observe(this._completion.result ? this.onExit() : this._completion.run("onExit", () => this.onExit()));
    } catch (error) {
      this._completion.fail(this._completion.result ? "late-onExit-admission" : "onExit-admission", error);
      this._announceExit();
    }
  }`
  },
  {
    label: 'ProcessHost owned graceful stop send',
    oldText: String.raw`      try { this.send({ method: "__stop__" }).catch(() => {}); }
      catch (error) { this._completion.fail("stop-send-admission", error); }`,
    newText: String.raw`      try { this.#sendOwnedStop().catch(error => this._completion.fail("stop-send", error)); }
      catch (error) { this._completion.fail("stop-send-admission", error); }`
  },
  {
    label: 'ProcessHost owned stop sender',
    oldText: String.raw`  didSendStop() {
    return this._didSendStop;
  }`,
    newText: String.raw`  #sendOwnedStop() {
    return this._completion.run("owned-stop-send", () => new Promise((resolve, reject) => {
      const adapters = privateCompletion.adapters();
      if (adapters.now() >= adapters.deadline()) {
        reject(new Error("IPC parent deadline expired"));
        return;
      }
      if (!this.process?.connected) {
        reject(new Error("IPC is not connected"));
        return;
      }
      this.process.send({ method: "__stop__" }, error => error ? reject(error) : resolve());
    }));
  }
  didSendStop() {
    return this._didSendStop;
  }`
  },
  {
    label: 'WorkerHost admission and admitted-only retention',
    oldText: String.raw`  async start() {
    await this._completion.run("worker-admission", () => privateCompletion.adapters().admitWorker(this._params));
    return await this.startRunner(this._params, {
      onStdOut: (chunk) => this._observeEvent("stdOut", import_common6.ipc.stdioChunkToParams(chunk)),
      onStdErr: (chunk) => this._observeEvent("stdErr", import_common6.ipc.stdioChunkToParams(chunk))
    });
  }
  async onExit() {
    await this._completion.run("retained-artifacts", () => privateCompletion.adapters().retainWorker(this._params));
  }`,
    newText: String.raw`  async start() {
    if (this._admissionPromise) {
      const error = new Error("Worker start already attempted");
      this._completion.fail("worker-start-repeated", error);
      this._startFailed = true;
      throw error;
    }
    if (this._stopRequested) {
      const error = new Error("Worker start cancelled after stop");
      this._completion.fail("worker-start-cancelled", error);
      this._startFailed = true;
      return { unexpectedly: true, error };
    }
    this._admissionPromise = this._completion.run("worker-admission", () => privateCompletion.adapters().admitWorker(this._params)).then(() => {
      this._workerAdmitted = true;
    });
    const adapters = privateCompletion.adapters();
    const parentDeadline = adapters.deadline();
    let deadlineTimer;
    const deadlineReached = !Number.isFinite(parentDeadline) ? new Promise(() => {}) : parentDeadline <= adapters.now() ? Promise.resolve({ deadlineExpired: true }) : new Promise(resolve => {
      deadlineTimer = adapters.setTimer(() => resolve({ deadlineExpired: true }), parentDeadline - adapters.now());
    });
    let admission;
    try {
      admission = await Promise.race([
        this._admissionPromise.then(() => ({ admitted: true })),
        this._stopRequestedPromise.then(() => ({ stopped: true })),
        deadlineReached
      ]);
    } catch (error) {
      this._startFailed = true;
      throw error;
    } finally {
      if (deadlineTimer !== undefined) adapters.clearTimer(deadlineTimer);
    }
    const deadlineExpired = admission.deadlineExpired || adapters.now() >= parentDeadline;
    if (!admission.admitted || this._stopRequested || deadlineExpired) {
      const error = new Error(deadlineExpired ? "Worker admission parent deadline expired" : "Worker start cancelled after stop");
      this._completion.fail(deadlineExpired ? "worker-admission-deadline" : "worker-start-cancelled", error);
      this._startFailed = true;
      return { unexpectedly: true, error };
    }
    try {
      const result = await this.startRunner(this._params, {
        onStdOut: (chunk) => this._observeEvent("stdOut", import_common6.ipc.stdioChunkToParams(chunk)),
        onStdErr: (chunk) => this._observeEvent("stdErr", import_common6.ipc.stdioChunkToParams(chunk))
      });
      if (result) this._startFailed = true;
      return result;
    } catch (error) {
      this._startFailed = true;
      throw error;
    }
  }
  async onExit() {
    if (this._admissionPromise) {
      try { await this._admissionPromise; }
      catch { return; }
    }
    if (!this._workerAdmitted || this._retentionAttempted) return;
    this._retentionAttempted = true;
    if (this._completion.result) {
      await privateCompletion.adapters().retainWorker(this._params);
      return;
    }
    await this._completion.run("retained-artifacts", () => privateCompletion.adapters().retainWorker(this._params));
  }`
  },
  {
    label: 'WorkerHost start failure prevents reuse',
    oldText: String.raw`    this._didFail = false;
    this.workerIndex = workerIndex;`,
    newText: String.raw`    this._didFail = false;
    this._startFailed = false;
    this.workerIndex = workerIndex;`
  },
  {
    label: 'WorkerHost didSendStop includes failed start',
    oldText: String.raw`  didFail() {
    return this._didFail;
  }`,
    newText: String.raw`  didSendStop() {
    return this._startFailed || super.didSendStop();
  }
  didFail() {
    return this._didFail;
  }`
  },
  {
    label: 'OutOfProcessLoaderHost idempotent finalization',
    oldText: String.raw`  async stop() {
    if (!this._processHost._readyInitialized)
      return privateCompletion.consume(await this._processHost.stop(), "OutOfProcessLoaderHost.stop");
    try {
      await this._processHost._completion.wait("loader-cache-consumer", async () => {
        const result = await this._processHost.sendMessage({ method: "getCompilationCacheFromLoader" });
        await import_common2.cc.addToCompilationCache(result);
      }, this._processHost._completion.deadline());
    } finally {
      privateCompletion.consume(await this._processHost.stop(), "OutOfProcessLoaderHost.stop");
    }
  }`,
    newText: String.raw`  stop() {
    if (this._stopPromise) return this._stopPromise;
    this._stopPromise = this._stop();
    return this._stopPromise;
  }
  async _stop() {
    if (!this._processHost._readyInitialized)
      return privateCompletion.consume(await this._processHost.stop(), "OutOfProcessLoaderHost.stop");
    try {
      await this._processHost._completion.wait("loader-cache-consumer", async () => {
        const result = await this._processHost.sendMessage({ method: "getCompilationCacheFromLoader" });
        await import_common2.cc.addToCompilationCache(result);
      }, this._processHost._completion.deadline());
    } finally {
      privateCompletion.consume(await this._processHost.stop(), "OutOfProcessLoaderHost.stop");
    }
  }`
  },
  {
    label: 'OutOfProcessLoaderHost preserves returned start error',
    oldText: String.raw`  async start(errors) {
    const startError = await this._processHost.startRunner(import_common2.ipc.serializeConfig(this._config, false));
    if (startError) {
      errors.push({`,
    newText: String.raw`  async start(errors) {
    const startError = await this._processHost.startRunner(import_common2.ipc.serializeConfig(this._config, false));
    if (startError) {
      this._startError = startError;
      errors.push({`
  },
  {
    label: 'loadFileSuites dual error preservation',
    oldText: String.raw`  try {
    if (await loaderHost.start(errors)) {
      for (const file of allTestFiles) {
        const fileSuite = await loaderHost.loadTestFile(file, errors);
        fileSuiteByFile.set(file, fileSuite);
        errors.push(...createDuplicateTitlesErrors(config2, fileSuite));
      }
    }
  } finally {
    await loaderHost.stop();
  }`,
    newText: String.raw`  let primaryError;
  let hasPrimaryError = false;
  let returnedStartError;
  let hasReturnedStartError = false;
  try {
    const started = await loaderHost.start(errors);
    if (!started && loaderHost._startError) {
      returnedStartError = loaderHost._startError.error ?? loaderHost._startError;
      hasReturnedStartError = true;
    }
    if (started) {
      for (const file of allTestFiles) {
        const fileSuite = await loaderHost.loadTestFile(file, errors);
        fileSuiteByFile.set(file, fileSuite);
        errors.push(...createDuplicateTitlesErrors(config2, fileSuite));
      }
    }
  } catch (error) {
    primaryError = error;
    hasPrimaryError = true;
  }
  let cleanupError;
  let hasCleanupError = false;
  try {
    await loaderHost.stop();
  } catch (error) {
    cleanupError = error;
    hasCleanupError = true;
  }
  if (hasPrimaryError && hasCleanupError)
    throw new AggregateError([primaryError, cleanupError], "Test loading and loader finalization both failed", { cause: primaryError });
  if (hasPrimaryError)
    throw primaryError;
  if (hasReturnedStartError && hasCleanupError)
    throw new AggregateError([returnedStartError, cleanupError], "Test loader startup and finalization both failed", { cause: returnedStartError });
  if (hasCleanupError)
    throw cleanupError;`
  },
  {
    label: 'Dispatcher scheduling consumes rejected caller',
    oldText: String.raw`    void this._runJobInWorker(workerIndex, jobDispatcher).then(() => {
      this._workerSlots[workerIndex].jobDispatcher = void 0;
      this._checkFinished();
      this._scheduleJob();
    });`,
    newText: String.raw`    void this._runJobInWorker(workerIndex, jobDispatcher).catch(async error => {
      if (!jobDispatcher.jobResult.isDone())
        jobDispatcher.onExit({ unexpectedly: true, error });
      if (!jobDispatcher._hashCountReleased) {
        jobDispatcher._hashCountReleased = true;
        this._updateCounterForWorkerHash(job.workerHash, -1);
      }
      const worker = this._workerSlots[workerIndex].worker;
      try { await worker?.stop(true); }
      catch { this._testRun.hasWorkerErrors = true; }
    }).then(() => {
      const slot = this._workerSlots[workerIndex];
      if (slot?.jobDispatcher === jobDispatcher)
        slot.jobDispatcher = void 0;
      this._checkFinished();
      this._scheduleJob();
    }).catch(() => {
      this._testRun.hasWorkerErrors = true;
      const slot = this._workerSlots[workerIndex];
      if (slot?.jobDispatcher === jobDispatcher)
        slot.jobDispatcher = void 0;
      this._checkFinished();
      this._scheduleJob();
    });`
  },
  {
    label: 'Dispatcher actual start and fork failure settlement',
    oldText: String.raw`  async _runJobInWorker(index, jobDispatcher) {
    const job = jobDispatcher.job;
    if (jobDispatcher.skipWholeJob())
      return;
    let worker = this._workerSlots[index].worker;
    if (worker && (worker.hash() !== job.workerHash || worker.didSendStop())) {
      await worker.stop();
      worker = void 0;
      if (this._isStopped)
        return;
    }
    let startError;
    if (!worker) {
      worker = this._createWorker(job, index, import_common7.ipc.serializeConfig(this._testRun.config, true));
      this._workerSlots[index].worker = worker;
      worker.on("exit", () => this._workerSlots[index].worker = void 0);
      startError = await worker.start();
      if (this._isStopped)
        return;
    }
    if (startError)
      jobDispatcher.onExit(startError);
    else
      jobDispatcher.runInWorker(worker);
    const result = await jobDispatcher.jobResult;
    this._updateCounterForWorkerHash(job.workerHash, -1);
    if (result.didFail)
      void worker.stop(
        true
        /* didFail */
      );
    else if (this._isWorkerRedundant(worker))
      void worker.stop();
    if (!this._isStopped) {
      if (result.remainingJob) {
        this._queue.unshift(result.remainingJob);
        this._updateCounterForWorkerHash(result.remainingJob.workerHash, 1);
      }
      if (result.isolatedRetriesJob) {
        this._isolatedJobs.add(result.isolatedRetriesJob);
        this._queue.push(result.isolatedRetriesJob);
        this._updateCounterForWorkerHash(result.isolatedRetriesJob.workerHash, 1);
      }
    }
  }`,
    newText: String.raw`  async _runJobInWorker(index, jobDispatcher) {
    const job = jobDispatcher.job;
    if (jobDispatcher.skipWholeJob())
      return;
    let worker;
    try {
      const slot = this._workerSlots[index];
      worker = slot.worker;
      if (worker && (worker.hash() !== job.workerHash || worker.didSendStop())) {
        await worker.stop();
        if (slot.worker === worker)
          slot.worker = void 0;
        worker = void 0;
      }
      if (this._isStopped) {
        jobDispatcher.onExit({ unexpectedly: true, error: new Error("Dispatcher stopped before worker start") });
      } else {
        let startError;
        let startWorker = false;
        if (!worker) {
          worker = this._createWorker(job, index, import_common7.ipc.serializeConfig(this._testRun.config, true));
          slot.worker = worker;
          worker.on("exit", () => {
            const currentSlot = this._workerSlots[index];
            if (currentSlot?.worker === worker)
              currentSlot.worker = void 0;
          });
          startWorker = true;
        }
        jobDispatcher._completionWorker = worker;
        if (startWorker)
          startError = await worker.start();
        if (startError)
          jobDispatcher.onExit(startError);
        else if (this._isStopped)
          jobDispatcher.onExit({ unexpectedly: true, error: new Error("Dispatcher stopped before job execution") });
        else
          jobDispatcher.runInWorker(worker);
      }
    } catch (error) {
      if (!jobDispatcher.jobResult.isDone())
        jobDispatcher.onExit({ unexpectedly: true, error });
    }
    const result = await jobDispatcher.jobResult;
    if (!jobDispatcher._hashCountReleased) {
      jobDispatcher._hashCountReleased = true;
      this._updateCounterForWorkerHash(job.workerHash, -1);
    }
    if (worker && result.didFail) {
      try { await worker.stop(true); }
      catch { this._testRun.hasWorkerErrors = true; }
    } else if (worker && this._isWorkerRedundant(worker)) {
      try { await worker.stop(); }
      catch { this._testRun.hasWorkerErrors = true; }
    }
    if (!this._isStopped) {
      if (result.remainingJob) {
        this._queue.unshift(result.remainingJob);
        this._updateCounterForWorkerHash(result.remainingJob.workerHash, 1);
      }
      if (result.isolatedRetriesJob) {
        this._isolatedJobs.add(result.isolatedRetriesJob);
        this._queue.push(result.isolatedRetriesJob);
        this._updateCounterForWorkerHash(result.isolatedRetriesJob.workerHash, 1);
      }
    }
  }`
  },
  {
    label: 'Dispatcher stop rejects observed at max failures',
    oldText: String.raw`    if (!this._ignoreMaxFailures && this._testRun.hasReachedMaxFailures())
      void this.stop();`,
    newText: String.raw`    if (!this._ignoreMaxFailures && this._testRun.hasReachedMaxFailures())
      void this.stop().catch(() => { this._testRun.hasWorkerErrors = true; });`
  },
  {
    label: 'JobDispatcher once-only completion',
    oldText: String.raw`    this.jobResult = new ManualPromise3();
    this._listeners = [];`,
    newText: String.raw`    this.jobResult = new ManualPromise3();
    this._didFinish = false;
    this._listeners = [];`
  },
  {
    label: 'JobDispatcher preserves start error',
    oldText: '  onExit(data) {\n    const unexpectedExitError = data.unexpectedly ? {\n      message: `Error: worker process exited unexpectedly (code=${data.code}, signal=${data.signal})`\n    } : void 0;\n    this._onDone({ skipTestsDueToSetupFailure: [], fatalErrors: [], unexpectedExitError });\n  }\n  _finished(result) {\n    eventsHelper.removeEventListeners(this._listeners);\n    this.jobResult.resolve(result);\n  }',
    newText: '  onExit(data) {\n    if (this.jobResult.isDone())\n      return;\n    const unexpectedExitError = data.unexpectedly ? data.error ? {\n      message: "Error: worker process failed to start: " + (data.error.message || String(data.error)),\n      stack: data.error.stack\n    } : {\n      message: `Error: worker process exited unexpectedly (code=${data.code}, signal=${data.signal})`\n    } : void 0;\n    try {\n      this._onDone({ skipTestsDueToSetupFailure: [], fatalErrors: [], unexpectedExitError });\n    } catch {\n      this._testRun.hasWorkerErrors = true;\n      this._finished({ didFail: true });\n    }\n  }\n  _finished(result) {\n    if (this._didFinish)\n      return;\n    this._didFinish = true;\n    try { eventsHelper.removeEventListeners(this._listeners); } catch { this._testRun.hasWorkerErrors = true; }\n    this.jobResult.resolve(result);\n  }'
  },
  {
    label: 'Dispatcher settles active jobs from incomplete stop outcomes',
    oldText: String.raw`  async stop() {
    if (this._completionStop) return this._completionStop;
    this._isStopped = true;
    this._completionStop = Promise.all([...this._completionWorkers].map(async worker => {
      const outcome = privateCompletion.consume(await worker.stop(), "Dispatcher.stop");
      if (outcome.state !== "COMPLETE_RETAINED") this._testRun.hasWorkerErrors = true;
      return outcome;
    }));
    const outcomes = await this._completionStop;
    this._checkFinished();
    return outcomes;
  }`,
    newText: String.raw`  async stop() {
    if (this._completionStop) return this._completionStop;
    this._isStopped = true;
    this._completionStop = Promise.all([...this._completionWorkers].map(async worker => {
      const outcome = privateCompletion.consume(await worker.stop(), "Dispatcher.stop");
      if (outcome.state !== "COMPLETE_RETAINED") {
        this._testRun.hasWorkerErrors = true;
        const jobDispatcher = this._workerSlots.find(candidate => candidate.jobDispatcher?._completionWorker === worker)?.jobDispatcher;
        if (jobDispatcher && !jobDispatcher.jobResult.isDone()) {
          const details = [...(outcome.pending || []), ...(outcome.errors || []).map(error => error.error)].filter(Boolean).join("; ");
          jobDispatcher.onExit({ unexpectedly: true, error: new Error("Worker completion was incomplete" + (details ? ": " + details : "")) });
        }
      }
      return outcome;
    }));
    const outcomes = await this._completionStop;
    this._checkFinished();
    return outcomes;
  }`
  },
  {
    label: 'Multiplexer preserves joined failure between reporters',
    oldText: String.raw`  async onEnd(result) {
    for (const reporter of this._reporters) {
      const outResult = await this._wrapAsync(() => reporter.onEnd?.(result));
      if (outResult?.status)
        result.status = outResult.status;
    }
    return result;
  }`,
    newText: String.raw`  async onEnd(result) {
    result.status = privateCompletion.terminalStatus(result.status);
    for (const reporter of this._reporters) {
      const outResult = await this._wrapAsync(() => reporter.onEnd?.(result));
      if (outResult?.status)
        result.status = outResult.status;
      result.status = privateCompletion.terminalStatus(result.status);
    }
    return result;
  }`
  },
  {
    label: 'finishTaskRun publishes joined terminal status',
    oldText: String.raw`async function finishTaskRun(testRun, status) {
  if (status === "passed")
    status = testRun.result();
  const modifiedResult = await testRun.reporter.onEnd({ status });
  if (modifiedResult && modifiedResult.status)
    status = modifiedResult.status;
  await testRun.reporter.onExit();
  if (status === "passed" && testRun.reporter.hasReporterErrors())
    status = "failed";
  return privateCompletion.terminalStatus(status);
}`,
    newText: String.raw`async function finishTaskRun(testRun, status) {
  if (status === "passed")
    status = testRun.result();
  status = privateCompletion.terminalStatus(status);
  const modifiedResult = await testRun.reporter.onEnd({ status });
  if (modifiedResult && modifiedResult.status)
    status = modifiedResult.status;
  status = privateCompletion.terminalStatus(status);
  await testRun.reporter.onExit();
  if (status === "passed" && testRun.reporter.hasReporterErrors())
    status = "failed";
  return privateCompletion.terminalStatus(status);
}`
  }
];

export function deriveRet2Runner(revision05) {
  const found = [];
  const ranges = [];
  for (const edit of ret2Edits) {
    let index = revision05.indexOf(edit.oldText);
    if (index < 0 || revision05.indexOf(edit.oldText, index + 1) >= 0)
      throw new Error(`Nonunique revision-05 source for ${edit.label}`);
    for (const range of ranges) {
      if (index < range.end && index + edit.oldText.length > range.start)
        throw new Error(`Overlapping revision-05 source for ${edit.label}`);
    }
    ranges.push({ start: index, end: index + edit.oldText.length, label: edit.label });
    found.push({ ...edit, index });
  }
  found.sort((a, b) => a.index - b.index);
  let cursor = 0;
  const pieces = [];
  for (const edit of found) {
    pieces.push(revision05.slice(cursor, edit.index), edit.newText);
    cursor = edit.index + edit.oldText.length;
  }
  pieces.push(revision05.slice(cursor));
  const derived = pieces.join('');
  return { derived, edits: found.map(({ label, index, oldText, newText }) => ({ label, index, oldText, newText })) };
}

export function makeUnifiedPatch(before, after, edits) {
  const oldLines = before.split('\n');
  const newLines = after.split('\n');
  const changes = edits.map(edit => {
    const oldStart = before.slice(0, edit.index).split('\n').length - 1;
    const oldCount = edit.oldText.split('\n').length;
    const priorDelta = edits.filter(other => other.index < edit.index).reduce((sum, other) => sum + other.newText.length - other.oldText.length, 0);
    const newStart = after.slice(0, edit.index + priorDelta).split('\n').length - 1;
    const newCount = edit.newText.split('\n').length;
    return { oldStart, oldEnd: oldStart + oldCount, newStart, newEnd: newStart + newCount };
  }).sort((a, b) => a.oldStart - b.oldStart);
  const groups = [];
  for (const change of changes) {
    const start = Math.max(0, change.oldStart - 3);
    const end = Math.min(oldLines.length, change.oldEnd + 3);
    const previous = groups.at(-1);
    if (previous && start <= previous.oldEnd) {
      previous.oldEnd = Math.max(previous.oldEnd, end);
      previous.newEnd = Math.max(previous.newEnd, Math.min(newLines.length, change.newEnd + 3));
    } else {
      groups.push({ oldStart: start, oldEnd: end, newStart: Math.max(0, change.newStart - 3), newEnd: Math.min(newLines.length, change.newEnd + 3) });
    }
  }
  const hunks = groups.map(group => {
    const oldCount = group.oldEnd - group.oldStart;
    const newCount = group.newEnd - group.newStart;
    const oldRegion = oldLines.slice(group.oldStart, group.oldEnd);
    const newRegion = newLines.slice(group.newStart, group.newEnd);
    const out = [];
    let oldIndex = 0;
    let newIndex = 0;
    const editLines = edits.map(edit => ({
      oldStart: before.slice(0, edit.index).split('\n').length - 1,
      oldLines: edit.oldText.split('\n'),
      newLines: edit.newText.split('\n')
    })).filter(edit => edit.oldStart >= group.oldStart && edit.oldStart < group.oldEnd).sort((a, b) => a.oldStart - b.oldStart);
    for (const edit of editLines) {
      const relativeStart = edit.oldStart - group.oldStart;
      while (oldIndex < relativeStart) {
        out.push(` ${oldRegion[oldIndex]}`);
        ++oldIndex;
        ++newIndex;
      }
      for (const line of edit.oldLines) out.push(`-${line}`);
      for (const line of edit.newLines) out.push(`+${line}`);
      oldIndex += edit.oldLines.length;
      newIndex += edit.newLines.length;
    }
    while (oldIndex < oldRegion.length && newIndex < newRegion.length) {
      if (oldRegion[oldIndex] !== newRegion[newIndex])
        throw new Error('Unable to reconstruct exact revision-05 hunk');
      out.push(` ${oldRegion[oldIndex]}`);
      ++oldIndex;
      ++newIndex;
    }
    while (oldIndex < oldRegion.length) out.push(`-${oldRegion[oldIndex++]}`);
    while (newIndex < newRegion.length) out.push(`+${newRegion[newIndex++]}`);
    return `@@ -${group.oldStart + 1},${oldCount} +${group.newStart + 1},${newCount} @@\n${out.join('\n')}`;
  });
  return `--- a/playwright/lib/runner/index.js\n+++ b/playwright/lib/runner/index.js\n${hunks.join('\n')}\n`;
}

export function countPatchLines(patch) {
  const lines = patch.split('\n');
  return {
    added: lines.filter(line => line.startsWith('+') && !line.startsWith('+++')).length,
    deleted: lines.filter(line => line.startsWith('-') && !line.startsWith('---')).length
  };
}
