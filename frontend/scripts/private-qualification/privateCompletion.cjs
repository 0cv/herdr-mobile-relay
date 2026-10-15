'use strict';
let runtime;
class Completion {
  constructor(name, adapters) {
    this.name = name;
    this.adapters = adapters;
    this.pending = new Map();
    this.errors = [];
    this.late = [];
    this.next = 0;
    this.started = 0;
    this.waiters = new Set();
  }
  fail(key, error) {
    if (this.result) {
      if (this.late.length < 16) this.late.push({ key, error: String(error) });
      return;
    }
    if (!this.errors.some(e => e.key === key) && this.errors.length < 16)
      this.errors.push({ key, error: String(error) });
    this.notify();
  }
  begin(label) {
    if (this.result || this.pending.size >= 16 || this.started >= 8192) {
      const error = new Error(`Completion admission refused: ${label}`);
      this.fail(`admission:${label}`, error);
      throw error;
    }
    const key = `${++this.next}:${label}`;
    ++this.started;
    this.pending.set(key, label);
    let settled = false;
    return error => {
      if (settled) return;
      settled = true;
      if (this.result && this.late.length < 16) this.late.push({ key, settled: true });
      if (error) this.fail(key, error);
      this.pending.delete(key);
      this.notify();
    };
  }
  run(label, action) {
    const settle = this.begin(label);
    try {
      const value = action();
      if (!value || typeof value.then !== 'function') {
        settle();
        return Promise.resolve(value);
      }
      return Promise.resolve(value).then(value => { settle(); return value; }, error => {
        settle(error);
        throw error;
      });
    } catch (error) {
      settle(error);
      return Promise.reject(error);
    }
  }
  notify() {
    for (const check of this.waiters) check();
  }
  deadline() {
    if (this.joinDeadline === undefined)
      this.joinDeadline = Math.min(this.adapters.now() + 1000, this.adapters.deadline());
    return this.joinDeadline;
  }
  join(beforePublish = () => {}) {
    if (this.joinPromise) return this.joinPromise;
    const deadline = this.deadline();
    this.joinPromise = new Promise(resolve => {
      let timer;
      let finishing = false;
      const check = () => {
        if (this.result || finishing) return;
        const expired = this.adapters.now() >= deadline;
        if (this.pending.size && !expired) return;
        finishing = true;
        this.waiters.delete(check);
        if (timer !== undefined) this.adapters.clearTimer(timer);
        if (expired && this.pending.size) this.fail('deadline', new Error('Completion deadline expired'));
        try { beforePublish(); } catch (error) { this.fail('beforePublish', error); }
        if (this.pending.size && !expired) {
          finishing = false;
          this.waiters.add(check);
          timer = this.adapters.setTimer(check, Math.max(0, deadline - this.adapters.now()));
          return;
        }
        const outcome = {
          host: this.name,
          state: this.pending.size || this.errors.length ? 'INCOMPLETE_RETAINED' : 'COMPLETE_RETAINED',
          deadline, pending: [...this.pending.values()], errors: this.errors.map(e => ({ ...e })),
          started: this.started
        };
        let publication;
        let asynchronousPublication = false;
        try {
          publication = this.begin('terminal-publication');
          const published = this.adapters.publish(outcome);
          if (published && typeof published.then === 'function') {
            asynchronousPublication = true;
            Promise.resolve(published).then(() => publication(), error => publication(error));
            throw new Error('Terminal publication must be synchronous');
          }
          publication();
        } catch (error) {
          if (!asynchronousPublication) publication?.(error);
          this.fail('publication', error);
          outcome.state = 'INCOMPLETE_RETAINED';
          outcome.pending = [...this.pending.values()];
          outcome.errors = this.errors.map(e => ({ ...e }));
        }
        this.result = Object.freeze({ ...outcome, pending: Object.freeze(outcome.pending), errors: Object.freeze(outcome.errors.map(Object.freeze)) });
        resolve(this.result);
      };
      this.waiters.add(check);
      timer = this.adapters.setTimer(check, Math.max(0, deadline - this.adapters.now()));
      check();
    });
    return this.joinPromise;
  }
  async wait(label, action, deadline) {
    if (this.adapters.now() >= deadline) {
      const error = new Error(`${label} deadline expired`);
      this.fail(`deadline:${label}`, error);
      throw error;
    }
    const promise = this.run(label, action);
    let timer;
    const timed = new Promise((_, reject) => {
      timer = this.adapters.setTimer(() => {
        const error = new Error(`${label} deadline expired`);
        this.fail(`deadline:${label}`, error);
        reject(error);
      }, Math.max(0, deadline - this.adapters.now()));
    });
    try { return await Promise.race([promise, timed]); }
    finally { this.adapters.clearTimer(timer); }
  }
}
function configure(adapters) {
  if (runtime) throw new Error('Private completion already configured');
  for (const key of ['now', 'deadline', 'setTimer', 'clearTimer', 'publish', 'admitWorker', 'retainWorker', 'requestOwnedForce'])
    if (typeof adapters[key] !== 'function') throw new Error(`Missing private adapter: ${key}`);
  runtime = { adapters, hosts: [], consumed: [], sealed: false };
}
function required() {
  if (!runtime) throw new Error('Private completion adapters not installed');
  return runtime;
}
function host(name) {
  const r = required();
  if (r.sealed || r.hosts.length >= 1024) throw new Error('Terminal host admission refused');
  const ledger = new Completion(name, r.adapters);
  r.hosts.push(ledger);
  return ledger;
}
function consume(outcome, owner) {
  const r = required();
  if (!outcome || !r.hosts.some(h => h.result === outcome)) throw new Error(`Unbound completion: ${owner}`);
  if (!r.consumed.some(e => e.outcome === outcome && e.owner === owner))
    r.consumed.push({ outcome, owner });
  return outcome;
}
function terminalStatus(status) {
  return required().hosts.some(h => !h.result || h.result.state !== 'COMPLETE_RETAINED') && status === 'passed' ? 'failed' : status;
}
function sealTerminal(status) {
  const r = required();
  r.sealed = true;
  return terminalStatus(status);
}
module.exports = { configure, host, consume, terminalStatus, sealTerminal,
  adapters: () => required().adapters,
  snapshot: () => ({ hosts: required().hosts.map(h => h.result || { host: h.name, state: 'INCOMPLETE_RETAINED', pending: [...h.pending.values()] }), consumed: required().consumed.map(e => ({ host: e.outcome.host, owner: e.owner })) }) };
