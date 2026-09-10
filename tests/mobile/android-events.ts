import type { AndroidEnvironmentSnapshot, AndroidPlannedTermination } from './android-environment';

interface LogRecord {
  line: string;
  time: number;
  pid: string;
  tag: string;
  message: string;
}

export function isAndroidTerminationPackage(name: string): boolean {
  return /^(?:com\.android\.chrome|org\.chromium\.webapk(?:\.[A-Za-z0-9_.-]+)?|com\.google\.android\.webapk(?:\.[A-Za-z0-9_.-]+)?)$/u.test(name);
}

export function isAndroidPackageProcess(name: string, packageName: string): boolean {
  return name === packageName || name.startsWith(`${packageName}:`);
}

const dependencies = ['com.android.chrome', 'com.google.android.gms', 'com.google.android.trichromelibrary'];
const dependency = (name: string) => dependencies.some((packageName) => isAndroidPackageProcess(name, packageName));
const relevant = (name: string) => dependency(name) || isAndroidTerminationPackage(name.split(':')[0]);

type PlannedInterval = Pick<AndroidPlannedTermination, 'packageName' | 'processes'> & { start: number; end: number };

function parseRecord(line: string): LogRecord | undefined {
  const match = line.match(/^(?:(\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3})|(\d{10}\.\d{3,6}))\s+(\d+)\s+\d+\s+[VDIWEF]\s+([^:]+?)\s*:\s?(.*)$/u);
  if (!match) return undefined;
  const time = match[2] ? Number(match[2]) * 1000 : Date.parse(`2000-${match[1].replace(' ', 'T')}Z`);
  if (!Number.isFinite(time)) return undefined;
  return { line, time, pid: match[3], tag: match[4].trim(), message: match[5] };
}

export function androidLogEvents(log: string, processes: Record<string, string> = {}, planned: PlannedInterval[] = []): string[] {
  const records = log.split(/\r?\n/u).map(parseRecord).filter((record): record is LogRecord => Boolean(record));
  const known = new Map(Object.entries(processes).map(([pid, name]) => [pid, { name, until: Infinity }]));
  const events: string[] = [];
  const intervals = planned.map((operation) => ({ ...operation, begun: false, eligible: new Map(Object.entries(operation.processes)) }));
  const forcedStops = new Map<string, { name: string; since: number; until: number }>();
  const exempt = (pid: string, name: string, time: number) => {
    const stopped = forcedStops.get(pid);
    return stopped?.name === name && time >= stopped.since && time <= stopped.until;
  };
  for (const record of records) {
    const { tag, message, time, pid } = record;
    for (const operation of intervals) {
      if (operation.begun || time < operation.start) continue;
      operation.begun = true;
      for (const [targetPid, name] of operation.eligible) known.set(targetPid, { name, until: Infinity });
    }
    if (tag === 'ActivityManager') {
      const start = message.match(/^Start proc (\d+):([^/\s]+)\/u0[a-z0-9]+ for /u);
      if (start) {
        known.set(start[1], { name: start[2], until: Infinity });
        forcedStops.delete(start[1]);
        for (const operation of intervals) if (operation.begun) operation.eligible.delete(start[1]);
      }
      const killing = message.match(/^Killing (\d+):([^/\s]+)\/u0[a-z0-9]+ .*: stop (\S+) due to from pid \d+(?: \([^)]+\))?$/u);
      if (killing) {
        const operation = intervals.find((entry) => time >= entry.start && time <= entry.end
          && entry.packageName === killing[3] && entry.eligible.get(killing[1]) === killing[2]);
        if (operation) forcedStops.set(killing[1], { name: killing[2], since: time, until: operation.end });
      }
    }
    const source = known.get(pid);
    const sourceRelevant = source && source.until >= time && relevant(source.name);
    const moduleChange = /^(?:DynamiteLoaderV2Impl|ChimeraCfgMgr)$/u.test(tag) && (
      /^Module config changed, forcing restart due to module \S+/u.test(message)
      || (() => {
        const change = message.match(/^Updating module config: (.+?) -> (.+)$/u);
        return Boolean(change && change[1] !== change[2]);
      })()
    );
    if (moduleChange && sourceRelevant) events.push(record.line);
    if (tag === 'ActivityManager') {
      const replacement = message.match(/^Force stopping (\S+) appid=\d+ user=(?:0|-1): installPackageLI$/u);
      if (replacement && dependency(replacement[1])) events.push(record.line);
    }
    if (/^(?:PackageManager|PackageInstaller)$/u.test(tag)) {
      const changed = message.match(/^(?:Package (\S+) (?:replaced|codePath changed|updated)(?:\s|$)|Replacing package (\S+)(?:\s|$)|Successfully installed package (\S+)(?:\s|$))/u);
      if (changed && dependency(changed[1] || changed[2] || changed[3])) events.push(record.line);
    }
    let targetPid = '';
    let targetName = '';
    if (tag === 'ActivityManager') {
      const death = message.match(/^(?:Process (\S+) \(pid (\d+)\) has died:|Killing (\d+):([^/\s]+)\/u0[a-z0-9]+(?:\s|:))/u);
      if (death) {
        targetPid = death[2] || death[3];
        targetName = death[1] || death[4];
      }
    }
    if (tag === 'Process') targetPid = message.match(/^Sending signal\. PID: (\d+) SIG: 9$/u)?.[1] || '';
    if (tag === 'Zygote') targetPid = message.match(/^Process (\d+) exited due to signal \d+ /u)?.[1] || '';
    const target = known.get(targetPid);
    targetName ||= target && target.until >= time ? target.name : '';
    if (!targetPid || !targetName) continue;
    if (relevant(targetName) && !exempt(targetPid, targetName, time)) events.push(record.line);
    known.set(targetPid, { name: targetName, until: time + 1000 });
  }
  return [...new Set(events)];
}

export function measuredAndroidEvents(
  log: string,
  before: AndroidEnvironmentSnapshot,
  after: AndroidEnvironmentSnapshot,
  operations: AndroidPlannedTermination[],
): { events: string[]; issues: string[] } {
  const issues: string[] = [];
  const first = before.measurement;
  const last = after.measurement;
  if (!first || !last || first.boundary !== 'start' || last.boundary !== 'end' || first.id !== last.id || !/^[A-Za-z0-9-]{1,80}$/u.test(first.id)) {
    return { events: [], issues: ['measurement snapshot boundaries are missing or inconsistent'] };
  }
  if (!log.endsWith('\n') || /(?:logcat:|Unexpected EOF|dropped \d+|chatty\s*:.*expire)/iu.test(log)) issues.push('measurement log is truncated or reports lost records');
  const records = log.split(/\r?\n/u).map(parseRecord).filter((record): record is LogRecord => Boolean(record));
  const marker = (message: string) => records.filter((record) => record.tag === 'HerdrMeasure' && record.message === `${first.id} ${message}`);
  const starts = marker('START');
  const ends = marker('END');
  if (starts.length !== 1 || ends.length !== 1 || starts[0].time >= ends[0].time || records.indexOf(starts[0]) >= records.indexOf(ends[0])) {
    return { events: [], issues: [...issues, 'measurement start/end markers are missing, ambiguous or out of order'] };
  }
  const rawLines = log.split(/\r?\n/u);
  const rawInterval = rawLines.slice(rawLines.indexOf(starts[0].line), rawLines.indexOf(ends[0].line) + 1);
  if (rawInterval.some((line) => line && !parseRecord(line) && !/^--------- (?:beginning of|switch to) (?:main|system)$/u.test(line))) {
    issues.push('measurement interval contains malformed log records');
  }
  const interval = records.slice(records.indexOf(starts[0]), records.indexOf(ends[0]) + 1);
  if (interval.some((record) => record.time < starts[0].time || record.time > ends[0].time)) issues.push('measurement clock moved outside its boundaries');
  const planned: PlannedInterval[] = [];
  const seen = new Set<string>();
  for (const operation of operations) {
    const begin = marker(`OP_BEGIN ${operation.id} ${operation.packageName} ${operation.pid}`);
    const end = marker(`OP_END ${operation.id} ${operation.packageName} ${operation.pid}`);
    if (seen.has(operation.id) || !/^[A-Za-z0-9-]{1,80}$/u.test(operation.id) || !/^[1-9]\d*$/u.test(operation.pid)
      || operation.measurementId !== first.id || !isAndroidTerminationPackage(operation.packageName) || operation.succeeded !== true
      || !operation.processes || typeof operation.processes !== 'object' || Array.isArray(operation.processes)
      || operation.processes[operation.pid] !== operation.packageName
      || Object.entries(operation.processes).some(([pid, name]) => !/^[1-9]\d*$/u.test(pid) || typeof name !== 'string'
        || !/^\S+$/u.test(name) || !isAndroidPackageProcess(name, operation.packageName))
      || JSON.stringify(operation.command) !== JSON.stringify(['shell', 'am', 'force-stop', '--user', '0', operation.packageName])
      || begin.length !== 1 || end.length !== 1 || begin[0].time < starts[0].time || end[0].time > ends[0].time
      || begin[0].time >= end[0].time || end[0].time - begin[0].time > 30_000) {
      issues.push('planned termination lacks a successful operation, observed process set or exact PID/package interval');
      continue;
    }
    if (planned.some((previous) => begin[0].time <= previous.end && end[0].time >= previous.start)) {
      issues.push('planned termination intervals overlap');
      continue;
    }
    seen.add(operation.id);
    planned.push({ packageName: operation.packageName, processes: operation.processes, start: begin[0].time, end: end[0].time });
  }
  const operationMarkers = interval.filter((record) => record.tag === 'HerdrMeasure' && record.message.startsWith(`${first.id} OP_`));
  if (operationMarkers.length !== operations.length * 2) issues.push('unrecorded or incomplete planned termination markers');
  return { events: androidLogEvents(interval.map((record) => record.line).join('\n'), first.processes, planned), issues };
}
