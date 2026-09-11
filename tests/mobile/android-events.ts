import type { AndroidEnvironmentSnapshot, AndroidPlannedTermination } from './android-environment';

interface LogRecord {
  line: string;
  lineNumber: number;
  time: number;
  pid: string;
  tid: string;
  priority: string;
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

type PlannedInterval = Pick<AndroidPlannedTermination, 'packageName' | 'processes'> & { start: number; end: number; startLine: number; endLine: number };

function parseRecord(line: string, index = 0): LogRecord | undefined {
  const match = line.match(/^(?:(\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3})|[ \t]*(\d{10}\.\d{3,6}))\s+(\d+)\s+(\d+)\s+([VDIWEF])\s+([^:]+?)\s*:\s?(.*)$/u);
  if (!match) return undefined;
  const time = match[2] ? Number(match[2]) * 1000 : Date.parse(`2000-${match[1].replace(' ', 'T')}Z`);
  if (!Number.isFinite(time)) return undefined;
  return { line, lineNumber: index + 1, time, pid: match[3], tid: match[4], priority: match[5], tag: match[6].trim(), message: match[7] };
}

export function androidLogEvents(log: string, processes: Record<string, string> = {}, planned: PlannedInterval[] = [], snapshotBoundary?: Pick<LogRecord, 'time' | 'lineNumber'>): string[] {
  const records = log.split(/\r?\n/u).map(parseRecord).filter((record): record is LogRecord => Boolean(record));
  const known = new Map(Object.entries(processes).map(([pid, name]) => [pid, {
    name, time: snapshotBoundary?.time ?? -Infinity, lineNumber: snapshotBoundary?.lineNumber ?? 0,
  }]));
  const previousIdentities = new Map<string, { name: string; untilTime: number; untilLine: number }[]>();
  const capturedIdentities = new Map<string, { name: string; time: number; lineNumber: number }[]>();
  for (const record of records) {
    if (record.tag !== 'ActivityManager') continue;
    const start = record.message.match(/^(?:Start proc|Killing) (\d+):([^/\s]+)\/u0[a-z0-9]+(?:-\d+)?(?:\s|:)/u);
    const death = record.message.match(/^Process (\S+) \(pid (\d+)\) has died(?::|\s|$)/u);
    const pid = start?.[1] || death?.[2];
    const name = start?.[2] || death?.[1];
    if (!pid || !name || !relevant(name)) continue;
    const identities = capturedIdentities.get(pid) || [];
    identities.push({ name, time: record.time, lineNumber: record.lineNumber });
    capturedIdentities.set(pid, identities);
  }
  const identity = (pid: string, record: LogRecord) => {
    const current = known.get(pid);
    if (current && relevant(current.name)) return current;
    return previousIdentities.get(pid)?.find((entry) => relevant(entry.name)
      && (record.time <= entry.untilTime || record.lineNumber <= entry.untilLine))
      || capturedIdentities.get(pid)?.find((entry) => entry.lineNumber > record.lineNumber && entry.time <= record.time)
      || current;
  };
  const remember = (pid: string, name: string, record: Pick<LogRecord, 'time' | 'lineNumber'>) => {
    const current = known.get(pid);
    if (current?.name === name) {
      known.set(pid, { name, time: Math.max(current.time, record.time), lineNumber: Math.max(current.lineNumber, record.lineNumber) });
      return;
    }
    if (snapshotBoundary && processes[pid]
      && (record.lineNumber <= snapshotBoundary.lineNumber || record.time <= snapshotBoundary.time)
      && (relevant(known.get(pid)?.name || '') || !relevant(name))) return;
    if (current) {
      const previous = previousIdentities.get(pid) || [];
      const ordered = record.time > current.time && record.lineNumber > current.lineNumber;
      previous.push({ name: current.name, untilTime: ordered ? record.time : Infinity, untilLine: ordered ? record.lineNumber : Infinity });
      previousIdentities.set(pid, previous);
    }
    known.set(pid, { name, time: record.time, lineNumber: record.lineNumber });
  };
  const events: string[] = [];
  const cleanExits: { record: LogRecord; pid: string }[] = [];
  const intervals = planned.map((operation) => ({ ...operation, begun: false,
    eligible: new Map(Object.entries(operation.processes).filter(([pid]) => !records.some((record) =>
      record.tag === 'ActivityManager' && record.message.startsWith(`Start proc ${pid}:`)
      && (record.lineNumber > operation.startLine || record.time >= operation.start)
      && (record.lineNumber < operation.endLine || record.time <= operation.end)))),
  }));
  const forcedStops = new Map<string, { name: string; since: number; until: number; startLine: number; endLine: number }>();
  const exempt = (pid: string, name: string, time: number, lineNumber: number) => {
    const stopped = forcedStops.get(pid);
    return stopped?.name === name && time >= stopped.since && time <= stopped.until && lineNumber > stopped.startLine && lineNumber < stopped.endLine;
  };
  for (const record of records) {
    const { tag, message, time, pid } = record;
    for (const operation of intervals) {
      if (operation.begun || time < operation.start || time > operation.end || record.lineNumber <= operation.startLine || record.lineNumber >= operation.endLine) continue;
      operation.begun = true;
      for (const [targetPid, name] of operation.eligible) remember(targetPid, name, { time: operation.start, lineNumber: operation.startLine });
    }
    if (tag === 'ActivityManager') {
      const start = message.match(/^Start proc (\d+):([^/\s]+)\/u0[a-z0-9]+(?:-\d+)? for /u);
      if (start) {
        remember(start[1], start[2], record);
        forcedStops.delete(start[1]);
        for (const operation of intervals) if (operation.begun) operation.eligible.delete(start[1]);
      }
      const killing = message.match(/^Killing (\d+):([^/\s]+)\/u0[a-z0-9]+ .*: stop (\S+) due to from pid \d+(?: \([^)]+\))?$/u);
      if (killing) {
        const operation = intervals.find((entry) => time >= entry.start && time <= entry.end
          && record.lineNumber > entry.startLine && record.lineNumber < entry.endLine
          && entry.packageName === killing[3] && entry.eligible.get(killing[1]) === killing[2]);
        if (operation) forcedStops.set(killing[1], { name: killing[2], since: time, until: operation.end, startLine: operation.startLine, endLine: operation.endLine });
      }
    }
    const source = identity(pid, record);
    const sourceRelevant = source && relevant(source.name);
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
    const cleanPid = tag === 'Zygote' ? message.match(/^Process (\d+) exited cleanly \(0\)$/u)?.[1] : undefined;
    const cleanTarget = cleanPid ? identity(cleanPid, record) : undefined;
    if (cleanPid && cleanTarget && relevant(cleanTarget.name)
      && !exempt(cleanPid, cleanTarget.name, time, record.lineNumber)) cleanExits.push({ record, pid: cleanPid });
    let targetPid = '';
    let targetName = '';
    if (tag === 'ActivityManager') {
      const death = message.match(/^(?:Process (\S+) \(pid (\d+)\) has died(?::|\s|$)|Killing (\d+):([^/\s]+)\/u0[a-z0-9]+(?:-\d+)?(?:\s|:))/u);
      if (death) {
        targetPid = death[2] || death[3];
        targetName = death[1] || death[4];
      }
    }
    if (tag === 'Process') targetPid = message.match(/^Sending signal\. PID: (\d+)(?:\s|$)/u)?.[1] || '';
    if (tag === 'Zygote') targetPid = message.match(/^Process (\d+) exited(?! cleanly \(0\)$)/u)?.[1] || '';
    const target = identity(targetPid, record);
    targetName ||= target ? target.name : '';
    if (!targetPid || !targetName) continue;
    const completeTermination = (tag === 'Process' && /^Sending signal\. PID: \d+ SIG: \d+$/u.test(message))
      || (tag === 'Zygote' && /^Process \d+ exited (?:due to signal \d+ \([^)]+\)|cleanly \(\d+\))$/u.test(message))
      || (tag === 'ActivityManager' && /^(?:Killing \d+:\S+\/\S+ .*: .+|Process \S+ \(pid \d+\) has died: .+)$/u.test(message));
    if (relevant(targetName) && (!completeTermination || !exempt(targetPid, targetName, time, record.lineNumber))) events.push(record.line);
    if (tag === 'ActivityManager') remember(targetPid, targetName, record);
  }
  for (const { record, pid } of cleanExits) {
    if (!events.some((line) => androidEventDetails(line).pid === pid)) events.push(record.line);
  }
  return events;
}

export function androidEventDetails(line: string): { kind: 'process-death' | 'module-config' | 'package-replacement'; line: string; pid?: string; processName?: string; uid?: string; reason?: string; initiatorPid?: string } {
  const record = parseRecord(line);
  const message = record?.message || '';
  const killed = message.match(/^Killing (\d+):([^/\s]+)\/(u0[a-z0-9]+(?:-\d+)?)\s+[^:]*:\s*(.*)$/u);
  const died = message.match(/^Process (\S+) \(pid (\d+)\) has died(?::|\s|$)\s*(.*)$/u);
  const pid = killed?.[1] || died?.[2] || message.match(/^(?:Sending signal\. PID:|Process|Killing) (\d+)/u)?.[1];
  if (pid) return {
    kind: 'process-death', line, pid, processName: killed?.[2] || died?.[1], uid: killed?.[3],
    reason: killed?.[4] || died?.[3] || message,
    initiatorPid: message.match(/\bfrom pid (\d+)/u)?.[1],
  };
  return { kind: /^(?:DynamiteLoaderV2Impl|ChimeraCfgMgr)$/u.test(record?.tag || '') ? 'module-config' : 'package-replacement', line };
}

export interface AndroidAuditSubject {
  attribution: 'logd-audit-subject';
  subjectPid: string;
  time: number;
  serial: string;
  sourceContext: string;
  line: string;
}

export function parseAndroidAuditSubject(line: string): AndroidAuditSubject | undefined {
  const record = parseRecord(line);
  if (!record || record.pid !== record.tid || record.priority !== 'W'
    || !/^[A-Za-z0-9_.:-]{1,15}$/u.test(record.tag)) return undefined;
  const match = record.message.match(/^type=1400 audit\(0\.0:([1-9]\d*)\): avc: {2}denied {2}\{ ([a-z_]+(?: [a-z_]+)*) \} for {2}name="[\x20-\x21\x23-\x5b\x5d-\x7e]+" dev="[A-Za-z0-9_-]+" ino=[1-9]\d* scontext=(u:r:isolated_app:s0:c512,c768) tcontext=u:object_r:app_data_file:s0:c\d+,c\d+,c512,c768 tclass=file permissive=0$/u);
  if (!match) return undefined;
  return { attribution: 'logd-audit-subject', subjectPid: record.pid, time: record.time,
    serial: match[1], sourceContext: match[3], line: record.line };
}

export interface AndroidNormalRetirement {
  pid: string;
  processName: string;
  packageName: 'com.android.chrome';
  uid: number;
  auditSubjects: (AndroidAuditSubject & { lineNumber: number })[];
  proof: { lineNumber: number; line: string }[];
  events: string[];
}

function normalRetirements(records: LogRecord[], events: string[], processes: Record<string, string>, remaining: Record<string, string>, bounded: (record: LogRecord) => boolean): AndroidNormalRetirement[] {
  const result: AndroidNormalRetirement[] = [];
  const proof = (record: LogRecord) => ({ lineNumber: record.lineNumber, line: record.line });
  for (const birth of records) {
    if (birth.tag !== 'ActivityManager') continue;
    const start = birth.message.match(/^Start proc ([1-9]\d*):(com\.android\.chrome:sandboxed_process0:org\.chromium\.content\.app\.SandboxedProcessService0:(\d+))\/u0ai(0|[1-9]\d{0,3}) for {2}\{com\.android\.chrome\/org\.chromium\.content\.app\.SandboxedProcessService0:(\d+)\}$/u);
    if (!start || start[3] !== start[5] || processes[start[1]] || remaining[start[1]] || Number(start[4]) > 8999
      || processes[birth.pid] !== 'system_server') continue;
    const [, pid, processName, , isolated] = start;
    const births = records.filter((record) => record.tag === 'ActivityManager' && record.message.startsWith(`Start proc ${pid}:`));
    const forks = records.filter((record) => record.tag === 'Zygote' && record.message === `Forked child process ${pid}`);
    const exits = records.filter((record) => record.tag === 'Zygote' && record.message.startsWith(`Process ${pid} exited`));
    const children = records.filter((record) => record.pid === pid && record.tag === 'chromium'
      && /^\[INFO:child_process_service\.cc\(\d+\)\] ChildProcessService: Exiting child process\.$/u.test(record.message));
    if (births.length !== 1 || forks.length !== 1 || exits.length !== 1 || children.length !== 1) continue;
    const fork = forks[0];
    const exit = exits[0];
    const child = children[0];
    if (exit.message !== `Process ${pid} exited cleanly (0)` || fork.pid !== exit.pid
      || processes[fork.pid] !== 'com.android.chrome_zygote'
      || !(fork.time <= birth.time && birth.time < child.time && child.time <= exit.time)
      || !(fork.lineNumber < birth.lineNumber && birth.lineNumber < child.lineNumber && child.lineNumber < exit.lineNumber)) continue;
    const conflictingAuditSubject = records.some((record) => record.pid === pid
      && /^type=1400\s+audit\(/u.test(record.message)
      && [...record.message.matchAll(/\sscontext=(\S*)/gu)].some((context) => context[1] !== 'u:r:isolated_app:s0:c512,c768'));
    if (conflictingAuditSubject) continue;
    const auditSubjects = records.filter((record) => record.pid === pid)
      .flatMap((record) => {
        const subject = parseAndroidAuditSubject(record.line);
        return subject ? [{ ...subject, lineNumber: record.lineNumber }] : [];
      });
    const uidRecords = records.filter((record) => record.pid === pid && record.tag === 'CompatChangeReporter');
    if (uidRecords.some((record) => {
      const uid = record.message.match(/^Compat change id reported: \d+; UID (\d+); state: (?:ENABLED|DISABLED)$/u)?.[1];
      return !uid || Number(uid) !== Number(isolated) + 90000;
    })) continue;
    if (auditSubjects.length && (!uidRecords.length || auditSubjects.some((subject) => records.filter((record) =>
      record.message.includes(`audit(0.0:${subject.serial}):`)).length !== 1))) continue;
    if (records.some((record) => record.pid === pid && record.tag === 'cr_SplitCompatApp'
      && !record.message.endsWith(` processName=${processName} isIsolatedProcess=true`))) continue;
    if ([fork, birth, child, exit, ...records.filter((record) => record.pid === pid)].some((record) => !bounded(record))) continue;
    const auditLines = new Set(auditSubjects.map((subject) => subject.lineNumber));
    if (records.some((record) => (record.tag === 'ActivityManager'
      && [fork.pid, birth.pid].some((producerPid) => record.message.startsWith(`Start proc ${producerPid}:`)))
      || (record.pid === pid && (record.time < fork.time || record.time > exit.time
        || record.lineNumber < fork.lineNumber
        || (record.lineNumber > exit.lineNumber && !auditLines.has(record.lineNumber)))))) continue;
    const deaths = events.filter((line) => androidEventDetails(line).pid === pid);
    if (deaths.length !== 1) continue;
    const deathRecords = deaths.map((line) => records.find((record) => record.line === line));
    if (deathRecords.some((record) => !record || !bounded(record) || record.lineNumber <= birth.lineNumber || record.time < birth.time || record.time > exit.time + 1000
      || record.tag !== 'ActivityManager' || record.pid !== birth.pid
      || (() => {
        const detail = androidEventDetails(record.line);
        if (detail.processName !== processName) return true;
        if (record.message.startsWith('Process ')) return !/^Process \S+ \(pid \d+\) has died: (?:vis(?:\+\d+)? BTOP)$/u.test(record.message);
        const uid = detail.uid?.match(/^u0a\d+i(-?\d+)$/u);
        return detail.reason !== 'isolated not needed' || !uid || Number(uid[1]) + 99000 !== Number(isolated) + 90000;
      })())) continue;
    const producerProofs = [exit, ...deathRecords as LogRecord[]];
    if (producerProofs.some((lastProof) => records.some((record) => {
      if (record.time > lastProof.time && record.lineNumber > lastProof.lineNumber) return false;
      const producerPid = lastProof.pid;
      if (record.tag === 'Zygote' && record.message.startsWith(`Process ${producerPid} exited`)) return true;
      if (record.tag === 'Process' && record.message.startsWith(`Sending signal. PID: ${producerPid} `)) return true;
      if (record.tag === 'ActivityManager' && (record.message.startsWith(`Killing ${producerPid}:`)
        || new RegExp(`^Process \\S+ \\(pid ${producerPid}\\) has died:`, 'u').test(record.message))) return true;
      return record.pid === producerPid && record.tag === 'AndroidRuntime' && /crash|fatal/iu.test(record.message);
    }))) continue;
    const ended = Math.max(exit.time, ...deathRecords.map((record) => record!.time));
    const endedLine = Math.max(exit.lineNumber, ...deathRecords.map((record) => record!.lineNumber));
    const adverse = records.some((record) => {
      const possiblyDuringLifetime = (record.time >= fork.time || record.lineNumber >= fork.lineNumber)
        && (record.time <= ended || record.lineNumber <= endedLine);
      if (possiblyDuringLifetime && record.tag === 'ActivityManager' && /^Force stopping com\.android\.chrome(?:\s|$)/u.test(record.message)
        && !/^Force stopping com\.android\.chrome appid=\d+ user=[1-9]\d*: \S.*$/u.test(record.message)) return true;
      if (possiblyDuringLifetime && /^(?:PackageManager|PackageInstaller)$/u.test(record.tag) && record.message.includes('com.android.chrome')) return true;
      const mentionsPid = new RegExp(`\\b${pid}\\b`, 'u').test(record.message);
      if (mentionsPid && !bounded(record)) return true;
      if (record.tag === 'ActivityManager' && mentionsPid && /^(?:Killing |Process .* has died:)/u.test(record.message)
        && !deaths.includes(record.line)) return true;
      if (record.tag === 'Process' && mentionsPid && /Sending signal/u.test(record.message)) return true;
      if ((record.pid === pid || mentionsPid)
        && /crash|fatal|signal|replacement|replacing|force.stop/iu.test(record.message)) return true;
      return false;
    });
    if (adverse) continue;
    result.push({ pid, processName, packageName: 'com.android.chrome', uid: Number(isolated) + 90000, auditSubjects,
      proof: [fork, birth, child, exit, ...deathRecords as LogRecord[]].sort((a, b) => a.lineNumber - b.lineNumber).map(proof), events: deaths });
  }
  return result;
}

export function measuredAndroidEvents(
  log: string,
  before: AndroidEnvironmentSnapshot,
  after: AndroidEnvironmentSnapshot,
  operations: AndroidPlannedTermination[],
): { events: string[]; fatalEvents: string[]; normalRetirements: AndroidNormalRetirement[]; issues: string[]; boundaryDiscordances: { lineNumber: number; line: string }[] } {
  const issues: string[] = [];
  const boundaryDiscordances: { lineNumber: number; line: string }[] = [];
  const first = before.measurement;
  const last = after.measurement;
  if (!first || !last || first.boundary !== 'start' || last.boundary !== 'end' || first.id !== last.id || !/^[A-Za-z0-9-]{1,80}$/u.test(first.id)) {
    return { events: [], fatalEvents: [], normalRetirements: [], boundaryDiscordances, issues: ['measurement snapshot boundaries are missing or inconsistent'] };
  }
  if (!log.endsWith('\n') || /(?:logcat:|Unexpected EOF|dropped \d+|chatty\s*:.*expire)/iu.test(log)) issues.push('measurement log is truncated or reports lost records');
  const records = log.split(/\r?\n/u).map(parseRecord).filter((record): record is LogRecord => Boolean(record));
  const marker = (message: string) => records.filter((record) => record.tag === 'HerdrMeasure' && record.message === `${first.id} ${message}`);
  const starts = marker('START');
  const ends = marker('END');
  if (starts.length !== 1 || ends.length !== 1 || starts[0].time >= ends[0].time || records.indexOf(starts[0]) >= records.indexOf(ends[0])) {
    return { events: [], fatalEvents: [], normalRetirements: [], boundaryDiscordances, issues: [...issues, 'measurement start/end markers are missing, ambiguous or out of order'] };
  }
  const rawLines = log.split(/\r?\n/u);
  if (rawLines.some((line) => line && !parseRecord(line) && !/^--------- (?:beginning of|switch to) (?:main|system)$/u.test(line))) {
    issues.push('measurement capture contains malformed log records');
  }
  const bounded = (record: LogRecord) => record.time >= starts[0].time && record.time <= ends[0].time
    && record.lineNumber >= starts[0].lineNumber && record.lineNumber <= ends[0].lineNumber;
  boundaryDiscordances.push(...records.filter((record) => {
    const received = record.lineNumber >= starts[0].lineNumber && record.lineNumber <= ends[0].lineNumber;
    const timestamped = record.time >= starts[0].time && record.time <= ends[0].time;
    return received !== timestamped;
  }).map(({ lineNumber, line }) => ({ lineNumber, line })));
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
      || !bounded(begin[0]) || !bounded(end[0]) || begin[0].lineNumber >= end[0].lineNumber
      || begin[0].time >= end[0].time || end[0].time - begin[0].time > 30_000) {
      issues.push('planned termination lacks a successful operation, observed process set or exact PID/package interval');
      continue;
    }
    if (planned.some((previous) => (begin[0].time <= previous.end && end[0].time >= previous.start)
      || (begin[0].lineNumber <= previous.endLine && end[0].lineNumber >= previous.startLine))) {
      issues.push('planned termination intervals overlap');
      continue;
    }
    seen.add(operation.id);
    planned.push({ packageName: operation.packageName, processes: operation.processes, start: begin[0].time, end: end[0].time, startLine: begin[0].lineNumber, endLine: end[0].lineNumber });
  }
  const operationMarkers = records.filter((record) => record.tag === 'HerdrMeasure' && record.message.startsWith(`${first.id} OP_`));
  if (operationMarkers.length !== operations.length * 2) issues.push('unrecorded or incomplete planned termination markers');
  const events = androidLogEvents(log, first.processes, planned, starts[0]);
  const retirements = issues.length ? [] : normalRetirements(records, events, first.processes, last.processes, bounded);
  const normalEvents = new Set(retirements.flatMap((retirement) => retirement.events));
  return { events, fatalEvents: events.filter((event) => !normalEvents.has(event)), normalRetirements: retirements, issues, boundaryDiscordances };
}
