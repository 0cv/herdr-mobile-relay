import { strict as assert } from 'node:assert';
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { EventEmitter } from 'node:events';
import { cp, mkdtemp, mkdir, readFile, writeFile, rm, realpath, symlink } from 'node:fs/promises';
import { existsSync, readFileSync, writeFileSync, renameSync, statSync, openSync, fstatSync, readSync, closeSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname, isAbsolute } from 'node:path';
import { createServer as createNetServer, type AddressInfo } from 'node:net';
import type { Readable, Writable } from 'node:stream';
import { managedWdaCapabilities } from '../support/ios-xctest';
import { IOSPlatform } from '../platforms/ios';

const credentialBinaryPlistBase64 = 'YnBsaXN0MDDUAQIDBAUGBwhYUEFTU1dPUkRbUFJJVkFURV9VUkxcQVBJX1BBU1NXT1JEXxASQ0ZCdW5kbGVJZGVudGlmaWVyXxAWYmFyZS1wYXNzd29yZC1zZW50aW5lbF8QN2h0dHBzOi8vcHJpdmF0ZS5leGFtcGxlL2luc3RhbGxlZD9yZWY9cHJpdmF0ZS1yZWZlcmVuY2VecGxhaW4tcGFzc3dvcmRfECtjb20uZmFjZWJvb2suV2ViRHJpdmVyQWdlbnRSdW5uZXIueGN0cnVubmVyCBEaJjNIYZuqAAAAAAAAAQEAAAAAAAAACQAAAAAAAAAAAAAAAAAAANg=';

// Shared with the generated observer; closed over no test/runtime state.
// This encodes a new observation, never rewrites historical raw receipts.
function encodeXctestExitProbe(probe: unknown) {
  type Kind = 'absent' | 'undefined' | 'null' | 'number' | 'string' | 'buffer' | 'object' | 'invalid';
  const own = (record: unknown, key: string): {own: boolean; kind: Kind; value?: unknown} => {
    if (!record || typeof record !== 'object') return {own: false, kind: 'invalid'};
    try {
      const descriptor = Object.getOwnPropertyDescriptor(record, key);
      if (!descriptor) return {own: false, kind: 'absent'};
      if (!('value' in descriptor)) return {own: true, kind: 'invalid'};
      const value: unknown = descriptor.value;
      const kind: Kind = value === undefined ? 'undefined' : value === null ? 'null'
        : typeof value === 'number' ? 'number' : typeof value === 'string' ? 'string'
        : Buffer.isBuffer(value) ? 'buffer' : typeof value === 'object' ? 'object' : 'invalid';
      return {own: true, kind, value};
    } catch { return {own: false, kind: 'invalid'}; }
  };
  const status = own(probe, 'status');
  const numericStatus = typeof status.value === 'number' && Number.isSafeInteger(status.value) && status.value >= 0;
  const statusKind = numericStatus ? 'number' : status.kind === 'absent' || status.kind === 'undefined' || status.kind === 'null' ? status.kind : 'invalid';
  const error = own(probe, 'error');
  const errorPresent = !['absent', 'undefined', 'null'].includes(error.kind);
  const errorCode = errorPresent ? own(error.value, 'code') : {own: false, kind: 'absent' as const, value: undefined};
  const errorName = own(error.value, 'name');
  const errorNames = ['Error', 'TypeError', 'RangeError'] as const;
  const errorCodes = ['EPERM', 'EACCES', 'ESRCH', 'ENOENT', 'ETIMEDOUT', 'ENOBUFS', 'EIO', 'EINVAL'] as const;
  let errorCategory: typeof errorNames[number] | 'unknown' | 'none' | 'other' = errorPresent ? 'unknown' : 'none';
  if (errorPresent) {
    try {
      errorCategory = errorName.kind === 'string'
        ? errorNames.find(name => name === errorName.value) ?? 'other'
        : error.value instanceof TypeError ? 'TypeError' : error.value instanceof RangeError ? 'RangeError'
        : error.value instanceof Error ? 'Error' : 'unknown';
    } catch { errorCategory = 'unknown'; }
  }
  const output = (key: 'stdout' | 'stderr') => {
    const field = own(probe, key);
    let bytes: number | null = null;
    let state: string | null = null;
    let classification: 'unavailable' | 'empty' | 'over-limit' | 'suppressed' | 'ps-state' = 'unavailable';
    try {
      if (field.kind === 'string' || field.kind === 'buffer') {
        const value = field.value as string | Buffer;
        const length = typeof value === 'string' ? Buffer.byteLength(value) : value.length;
        bytes = Math.min(length, 65);
        classification = length === 0 ? 'empty' : length > 64 ? 'over-limit' : 'suppressed';
        if (key === 'stdout' && length > 0 && length <= 64) {
          const text = typeof value === 'string' ? value : Buffer.prototype.toString.call(value, 'utf8');
          // Single Linux/Darwin ps state token only; never arbitrary output.
          if (/^[ \t]*[RSDTtXZIUWPK][<NLEsl+WX-]*[ \t]*(?:\r?\n)?$/u.test(text)) {
            state = text.trim();
            classification = 'ps-state';
          }
        }
      }
    } catch { classification = 'unavailable'; bytes = null; state = null; }
    return {own: field.own, kind: field.kind, bytes, classification, state};
  };
  const signal = own(probe, 'signal');
  const stdout = output('stdout');
  const stderr = output('stderr');
  const successful = numericStatus && status.value === 0 && !errorPresent && signal.kind === 'null'
    && stdout.classification === 'ps-state' && stderr.classification === 'empty';
  const classification = !successful ? 'indeterminate'
    : stdout.state?.startsWith('Z') ? 'terminal-observation'
    : stdout.state?.startsWith('X') ? 'indeterminate' : 'live-observation';
  return {
    schema: 2, encoding: 'loss-aware-ps-observation', classification,
    witnessGuarantee: 'numeric-pid-only',
    status: numericStatus ? Number(status.value) : null, statusOwn: status.own, statusKind,
    statusInvalidCategory: statusKind !== 'invalid' ? null : status.kind === 'number' ? 'invalid-number' : 'invalid-type',
    error: {
      own: error.own, kind: error.kind, present: errorPresent, category: errorCategory,
      codeOwn: errorCode.own, codeKind: errorCode.kind,
      code: errorCode.kind === 'string'
        ? errorCodes.find(code => code === errorCode.value) ?? 'other'
        : null,
    },
    signal: {own: signal.own, kind: signal.kind, value: signal.kind === 'null' ? null : 'suppressed'},
    stdout, stderr,
  } as const;
}

const terminalMainGo = String.raw`package main

import (
 "bytes"
 "crypto/sha256"
 "encoding/hex"
 "encoding/json"
 "errors"
 "io"
 "os"
 "path/filepath"
 "time"
)

type record struct {
 Schema int
 Phase string
 Nonce string
 TargetPID int
 Previous string
 Data map[string]any
}
type config struct {
 Root string
 Nonce string
 TargetPID int
 Binding string
 Deadline int64
 Fault bool
 Binary string
 Sources string
}
type observation struct {
 config
 end time.Time
 terminal string
 stage string
 callResult any
 callResultKind string
 cleanup error
}
func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func bounded(path string) ([]byte, error) {
 f, err := os.Open(path); if err != nil { return nil, err }; defer f.Close()
 b, err := io.ReadAll(io.LimitReader(f, 8193)); if err != nil { return nil, err }
 if len(b) == 0 || len(b) > 8192 { return nil, errors.New("record-size") }; return b, nil
}
func decode(b []byte, v any) error {
 d := json.NewDecoder(bytes.NewReader(b)); d.DisallowUnknownFields()
 if err := d.Decode(v); err != nil { return err }
 var extra any; if d.Decode(&extra) != io.EOF { return errors.New("record-tail") }; return nil
}
func (o *observation) check() error {
 if !time.Now().Before(o.end) { return errors.New("deadline") }
 for _, name := range []string{"cancel", "child-exit-probe-error", "target-error", "state/stop"} {
  if _, err := os.Stat(filepath.Join(o.Root, name)); err == nil { return errors.New("cancelled") } else if !os.IsNotExist(err) { return err }
 }; return nil
}
func (o *observation) publish(phase, previous string, data map[string]any) (string, error) {
 if err := o.check(); err != nil { return "", err }
 b, err := json.Marshal(record{1, phase, o.Nonce, o.TargetPID, previous, data}); if err != nil { return "", err }; b = append(b, '\n')
 if len(b) > 8192 { return "", errors.New("record-size") }
 path := filepath.Join(o.Root, phase+".json")
 if _, err := os.Lstat(path); !os.IsNotExist(err) { return "", errors.New("record-exists") }
 if err := os.WriteFile(path+".next", b, 0600); err != nil { return "", err }
 if err := os.Rename(path+".next", path); err != nil { return "", err }; return digest(b), nil
}
func (o *observation) event(previous string, data map[string]any) error {
 for _, phase := range []string{"challenge", "live", "ps-blocked", "intent", "continue"} {
  b, err := bounded(filepath.Join(o.Root, phase+".json")); if err != nil { return errors.New("early-event") }
  var r record; if decode(b, &r) != nil || r.Schema != 1 || r.Phase != phase || r.Nonce != o.Nonce || r.TargetPID != o.TargetPID || r.Previous != previous { return errors.New("event-order") }
  previous = digest(b)
 }
 var err error; o.terminal, err = o.publish("terminal", previous, data); return err
}
func main() {
 var o observation
 o.stage = "input"
 o.callResultKind = "unavailable"
 fail := func(err error) {
  receipt := map[string]any{"result":"ERROR", "stage":o.stage, "category":category(err), "errno":errno(err), "errnoKnown":errno(err)!=nil, "callResult":o.callResult, "callResultKind":o.callResultKind, "cleanup":nil}
  if o.cleanup != nil { receipt["cleanup"] = category(o.cleanup) }
  b, _ := json.Marshal(receipt)
  path := filepath.Join(o.Root, "observer-error.json")
  if os.WriteFile(path+".next", append(b, '\n'), 0600) != nil || os.Rename(path+".next", path) != nil { os.Stderr.WriteString("terminal-witness ERROR publication\n") }
  os.Exit(2)
 }
 if len(os.Args)!=2 { os.Exit(2) }
 b, err := bounded(os.Args[1]); if err != nil { os.Exit(2) }
 if decode(b, &o.config)!=nil || o.Root=="" || len(o.Nonce)!=32 || o.TargetPID<=0 || o.TargetPID>2147483647 || len(o.Binding)!=64 || len(o.Binary)!=64 || len(o.Sources)!=64 { os.Exit(2) }
 remaining := time.Until(time.UnixMilli(o.Deadline)); if remaining<=0 || remaining>10*time.Second { fail(errors.New("deadline")) }
 o.end=time.Now().Add(remaining)
 binding, err := bounded(filepath.Join(o.Root,"binding.json")); if err!=nil || digest(binding)!=o.Binding { fail(errors.New("binding")) }
 if err:=o.check(); err!=nil { fail(err) }
 if err:=observe(&o); err!=nil { fail(err) }
 if o.cleanup!=nil { o.stage="cleanup"; fail(o.cleanup) }
 if o.terminal=="" { fail(errors.New("missing-terminal")) }
 o.stage="completion"
 if _,err:=o.publish("complete",o.terminal,map[string]any{"result":"SUCCESS","observerPID":os.Getpid(),"cleanup":nil,"binary":o.Binary,"sources":o.Sources}); err!=nil { fail(err) }
}
`;
const terminalLinuxGo = String.raw`package main

import (
 "errors"
 "os"
 "syscall"
 "time"
)
func errno(err error) any { var e syscall.Errno; if errors.As(err,&e) { return uint64(e) }; return nil }
func category(err error) string {
 if errors.Is(err,os.ErrNoHandle) { return "handle-unavailable" }
 if errors.Is(err,os.ErrProcessDone) { return "process-done-api" }
 var e syscall.Errno; if errors.As(err,&e) { switch e { case syscall.EBADF:return "EBADF"; case syscall.EPERM:return "EPERM"; case syscall.EACCES:return "EACCES"; case syscall.ESRCH:return "ESRCH"; case syscall.EINTR:return "EINTR" }; return "syscall" }
 return "protocol-or-deadline"
}
func observe(o *observation) error {
 o.stage="handle"
 p,err:=os.FindProcess(o.TargetPID); if err!=nil { return err }
 entered:=false
 var operation error
 handleErr:=p.WithHandle(func(handle uintptr) {
  entered=true
  operation=func() error {
   if handle>2147483647 { return errors.New("descriptor-range") }
   o.stage="queue"
   queue,err:=syscall.EpollCreate1(syscall.EPOLL_CLOEXEC); if err!=nil { return err }
   defer func(){ if queue>=0 { o.cleanup=syscall.Close(queue) } }()
   if o.Fault { if err:=syscall.Close(queue);err!=nil { queue=-1;return err };queue=-1 }
   o.stage="registration"
   fd:=int32(handle)
   requested:=syscall.EpollEvent{Events:syscall.EPOLLIN,Fd:fd,Pad:0}
   o.callResultKind="error-only"
   if err:=syscall.EpollCtl(queue,syscall.EPOLL_CTL_ADD,int(handle),&requested);err!=nil { return err }
   armed,err:=o.publish("armed",o.Binding,map[string]any{"method":"linux-pidfd-epoll","observerPID":os.Getpid(),"queue":queue,"fd":fd,"pad":0,"requested":1,"result":0,"error":nil})
   if err!=nil { return err }; if err:=o.check();err!=nil { return err }
   o.stage="wait"
   ms:=time.Until(o.end).Milliseconds(); if ms<=0 { return errors.New("deadline") }
   events:=make([]syscall.EpollEvent,2)
   n,err:=syscall.EpollWait(queue,events,int(ms)); o.callResult=n; o.callResultKind="number"; if err!=nil { return err }
   if n!=1 { return errors.New("event-count") }
   event:=events[0]
   if event.Fd!=fd || event.Pad!=0 || (event.Events!=1 && event.Events!=17) { return errors.New("event-data") }
   o.stage="terminal"
   return o.event(armed,map[string]any{"method":"linux-pidfd-epoll","observerPID":os.Getpid(),"queue":queue,"fd":event.Fd,"pad":event.Pad,"events":event.Events,"count":n,"error":nil})
  }()
 })
 releaseErr:=p.Release()
 if o.cleanup==nil { o.cleanup=releaseErr }
 if handleErr!=nil { return handleErr }
 if !entered { return errors.New("callback-not-entered") }
 return operation
}
`;
const terminalDarwinGo = String.raw`package main

import (
 "errors"
 "os"
 "strconv"
 "syscall"
 "time"
)
func errno(err error) any { var e syscall.Errno; if errors.As(err,&e) { return uint64(e) }; return nil }
func category(err error) string {
 var e syscall.Errno; if errors.As(err,&e) { switch e { case syscall.EBADF:return "EBADF"; case syscall.EPERM:return "EPERM"; case syscall.EACCES:return "EACCES"; case syscall.ESRCH:return "ESRCH"; case syscall.EINTR:return "EINTR" }; return "syscall" }
 return "protocol-or-deadline"
}
func observe(o *observation) error {
 o.stage="queue"
 queue,err:=syscall.Kqueue(); if err!=nil { return err }
 defer func(){ if queue>=0 { o.cleanup=syscall.Close(queue) } }()
 if o.Fault { if err:=syscall.Close(queue);err!=nil { queue=-1;return err };queue=-1 }
 o.stage="registration"
 var change syscall.Kevent_t
 syscall.SetKevent(&change,o.TargetPID,syscall.EVFILT_PROC,syscall.EV_ADD|syscall.EV_ONESHOT|syscall.EV_RECEIPT)
 change.Fflags=syscall.NOTE_EXIT
 events:=make([]syscall.Kevent_t,2)
 zero:=syscall.Timespec{}
 n,err:=syscall.Kevent(queue,[]syscall.Kevent_t{change},events,&zero); o.callResult=n; o.callResultKind="number"; if err!=nil { return err }
 if n!=1 { return errors.New("registration-count") }
 receipt:=events[0]
 if receipt.Ident!=uint64(o.TargetPID) || receipt.Filter!=syscall.EVFILT_PROC || receipt.Flags&syscall.EV_ERROR==0 || receipt.Data!=0 { return errors.New("registration-receipt") }
 armed,err:=o.publish("armed",o.Binding,map[string]any{"method":"darwin-kqueue","observerPID":os.Getpid(),"queue":queue,"ident":strconv.FormatUint(receipt.Ident,10),"filter":receipt.Filter,"flags":receipt.Flags,"fflags":receipt.Fflags,"data":strconv.FormatInt(receipt.Data,10),"requestedFlags":change.Flags,"requestedFflags":change.Fflags,"count":n,"error":nil})
 if err!=nil { return err }; if err:=o.check();err!=nil { return err }
 o.stage="wait"
 remaining:=time.Until(o.end); if remaining<=0 { return errors.New("deadline") }
 timeout:=syscall.NsecToTimespec(remaining.Nanoseconds())
 n,err=syscall.Kevent(queue,nil,events,&timeout); o.callResult=n; o.callResultKind="number"; if err!=nil { return err }
 if n!=1 { return errors.New("event-count") }
 event:=events[0]
 if event.Ident!=uint64(o.TargetPID) || event.Filter!=syscall.EVFILT_PROC || event.Flags&syscall.EV_ERROR!=0 || event.Fflags!=uint32(syscall.NOTE_EXIT) { return errors.New("event-data") }
 o.stage="terminal"
 return o.event(armed,map[string]any{"method":"darwin-kqueue","observerPID":os.Getpid(),"queue":queue,"ident":strconv.FormatUint(event.Ident,10),"filter":event.Filter,"flags":event.Flags,"fflags":event.Fflags,"data":strconv.FormatInt(event.Data,10),"count":n,"error":nil})
}
`;

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
wait_for_fixture_file() {
  fixture_path=$1
  fixture_error=\${2:-}
  fixture_deadline=$(cat "$root/state/deadline" 2>/dev/null || printf '')
  while ! [ -e "$fixture_path" ]; do
    if [ -n "$fixture_error" ] && [ -e "$fixture_error" ]; then return 2; fi
    if [ -e "$root/stop" ] || grep -q '"firstFailure"' "$root/state/owner.json" 2>/dev/null; then return 2; fi
    case "$fixture_deadline" in
      ''|*[!0-9]*) return 2 ;;
    esac
    fixture_now=$(date +%s%3N 2>/dev/null || printf '')
    case "$fixture_now" in
      ''|*[!0-9]*) fixture_now=$(date +%s)000 ;;
    esac
    if [ "$fixture_now" -ge "$fixture_deadline" ]; then return 2; fi
    /bin/sleep 0.01
  done
  return 0
}
wait_for_fixture_pid() {
  fixture_path=$1
  if [ -n "\${2:-}" ]; then
    fixture_pid=$("$STARTUP_TEST_EXECUTABLE" "$root/bin/pending-invalid-pid-wait.ts" "$2") || return 2
    return 0
  fi
  fixture_deadline=$(cat "$root/state/deadline" 2>/dev/null || printf '')
  while :; do
    if [ -s "$fixture_path" ]; then
      fixture_pid=$(cat "$fixture_path" 2>/dev/null || printf '')
      case "$fixture_pid" in
        ''|0*|*[!0-9]*) ;;
        *) return 0 ;;
      esac
    fi
    if [ -e "$root/stop" ] || grep -q '"firstFailure"' "$root/state/owner.json" 2>/dev/null; then return 2; fi
    case "$fixture_deadline" in
      ''|*[!0-9]*) return 2 ;;
    esac
    fixture_now=$(date +%s%3N 2>/dev/null || printf '')
    case "$fixture_now" in
      ''|*[!0-9]*) fixture_now=$(date +%s)000 ;;
    esac
    if [ "$fixture_now" -ge "$fixture_deadline" ]; then return 2; fi
    /bin/sleep 0.01
  done
}
publish_fixture_pid() {
  fixture_path=$1
  fixture_pid=$2
  case "$fixture_pid" in
    ''|0*|*[!0-9]*) return 2 ;;
  esac
  printf '%s\n' "$fixture_pid" > "$fixture_path.$$" && mv "$fixture_path.$$" "$fixture_path"
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
    receipt_info=false
    case "$path" in
      "$STARTUP_TEST_PRODUCT/Info.plist"|/private$STARTUP_TEST_PRODUCT/Info.plist) product_info=true ;;
      "$STARTUP_TEST_RECEIPT/Info.plist"|/private$STARTUP_TEST_RECEIPT/Info.plist) receipt_info=true ;;
    esac
    if [ "$mode" = "listener-initial-race-receipt-command-error" ] && [ "$receipt_info" = true ] && [ -e "$root/receipt-queries" ] && [ "$(cat "$root/receipt-queries")" -ge 2 ]; then
      exit 78
    fi
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
      if { [ "$mode" = "listener-bundle-mismatch" ] || { [ "$mode" = "listener-pending-bundle" ] && [ "$(cat "$root/pending-evidence-count" 2>/dev/null || printf 0)" -ge 2 ]; }; } && [ "$bundle_mismatch" = true ]; then
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
  if { [ "$mode" = "listener-initial-race" ] || [ "$mode" = "listener-initial-race-birth" ] || [ "$mode" = "listener-initial-race-executable" ] || [ "$mode" = "listener-initial-race-hash" ] || [ "$mode" = "listener-initial-race-product" ] || [ "$mode" = "listener-initial-race-receipt" ] || [ "$mode" = "listener-initial-race-command-error" ] || [ "$mode" = "listener-initial-race-budget" ]; } && ! has_arg -d "$@" && ([ -e "$root/stop" ] || grep -q 'firstFailure' "$root/state/owner.json" 2>/dev/null); then
    printf '%s\\n' "$(cat "$root/runner.pid")"
    exit 0
  fi
  if { [ "$mode" = "noisy-cleanup" ] || [ "$mode" = "receipt-product-changed" ] || [ "$mode" = "listener-hash-after-freeze" ]; } && ! has_arg -d "$@" && ! [ -e "$root/stop" ] && ! grep -q '"firstFailure"' "$root/state/owner.json" 2>/dev/null && grep -q '^  "pid"' "$root/state/owner.json" 2>/dev/null; then
    if ! wait_for_fixture_pid "$root/runner.pid"; then
      printf '%s\\n' 'stable listener publication unavailable' >&2
      exit 2
    fi
    printf '%s\\n' "$(cat "$root/runner.pid")"
    exit 0
  fi
  if { [ "$mode" = "listener-initial-race" ] || [ "$mode" = "listener-initial-race-birth" ] || [ "$mode" = "listener-initial-race-executable" ] || [ "$mode" = "listener-initial-race-hash" ] || [ "$mode" = "listener-initial-race-product" ] || [ "$mode" = "listener-initial-race-receipt" ] || [ "$mode" = "listener-initial-race-receipt-command-error" ] || [ "$mode" = "listener-initial-race-receipt-hash-error" ] || [ "$mode" = "listener-initial-race-command-error" ] || [ "$mode" = "listener-initial-race-command-error-late" ] || [ "$mode" = "listener-initial-race-budget" ] || [ "$mode" = "listener-initial-race-hash-budget" ] || [ "$mode" = "listener-initial-race-cleanup-identity" ]; } && ! has_arg -d "$@" && grep -q '"firstFailure"' "$root/state/owner.json" 2>/dev/null; then
    [ "$mode" = "listener-initial-race-cleanup-identity" ] && touch "$root/cleanup-boundary"
    printf '%s\\n' "$(cat "$root/runner.pid")"
    exit 0
  fi
  if { [ "$mode" = "listener-initial-race" ] || [ "$mode" = "listener-initial-race-birth" ] || [ "$mode" = "listener-initial-race-executable" ] || [ "$mode" = "listener-initial-race-hash" ] || [ "$mode" = "listener-initial-race-product" ] || [ "$mode" = "listener-initial-race-command-error" ] || [ "$mode" = "listener-initial-race-budget" ] || [ "$mode" = "listener-initial-race-cleanup-identity" ] || [ "$mode" = "listener-initial-race-command-error-late" ] || [ "$mode" = "listener-initial-race-hash-budget" ]; } && ! has_arg -d "$@" && ! [ -e "$root/stop" ] && has_arg "-iTCP:$IOS_WDA_PORT" "$@"; then
    exit 1
  fi
  if { [ "$mode" = "listener-initial-race-receipt" ] || [ "$mode" = "listener-initial-race-receipt-command-error" ] || [ "$mode" = "listener-initial-race-receipt-hash-error" ]; } && ! has_arg -d "$@" && ! [ -e "$root/stop" ] && ! grep -q '"firstFailure"' "$root/state/owner.json" 2>/dev/null && grep -q '^  "pid"' "$root/state/owner.json" 2>/dev/null; then
    receipt_pair_file=$root/initial-race-receipt-pair
    receipt_pair_trace=$root/initial-race-receipt-pair-trace
    if has_arg "-iTCP:$IOS_WDA_PORT" "$@" && ! [ -e "$receipt_pair_file" ]; then
      printf '%s\\n' '1 EMPTY' > "$receipt_pair_file.$$" && mv "$receipt_pair_file.$$" "$receipt_pair_file"
      touch "$root/initial-race-receipt-http-observed"
      printf '%s\\n' 'http 1 EMPTY' >> "$receipt_pair_trace"
      touch "$root/initial-race-receipt-fatal"
      printf '%s\\n' 'historical partial listener diagnostic' >&2
      exit 1
    fi
    if has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@" && [ -e "$receipt_pair_file" ]; then
      receipt_pair_latch=$(cat "$receipt_pair_file")
      receipt_pair_value=\${receipt_pair_latch#* }
      if [ "\${receipt_pair_latch%% *}" != 1 ] || [ "$receipt_pair_value" != EMPTY ]; then
        printf '%s\\n' 'historical receipt endpoint pair malformed' >&2
        exit 2
      fi
      printf '%s\\n' 'mjpeg-entry 1' >> "$receipt_pair_trace"
      touch "$root/initial-race-receipt-publication-release"
      if ! wait_for_fixture_file "$root/initial-race-receipt-publication-ack"; then
        printf '%s\\n' 'historical receipt publication acknowledgement failed' >&2
        exit 2
      fi
      receipt_pair_pid=$(cat "$root/runner.pid" 2>/dev/null || printf '')
      case "$receipt_pair_pid" in
        ''|*[!0-9]*) printf '%s\\n' 'historical receipt listener PID unavailable' >&2; exit 2 ;;
      esac
      printf '%s\\n' 'mjpeg-ack 1' >> "$receipt_pair_trace"
      printf 'mjpeg 1 %s\\n' "$receipt_pair_pid" >> "$receipt_pair_trace"
      printf '%s\\n' "$receipt_pair_pid"
      exit 0
    fi
  fi
  if { [ "$mode" = "credential" ] || [ "$mode" = "credential-binary" ] || [ "$mode" = "credential-binary-failure" ] || [ "$mode" = "listener-mjpeg-duplicate" ] || [ "$mode" = "listener-bundle-mismatch" ] || [ "$mode" = "listener-hash-mismatch" ]; } && ! has_arg -d "$@" && ! [ -e "$root/stop" ] && grep -q '^  "pid"' "$root/state/owner.json" 2>/dev/null; then
    coordinated_pair_file=$root/coordinated-listener-pair
    coordinated_pair_trace=$root/coordinated-listener-pair-trace
    if has_arg "-iTCP:$IOS_WDA_PORT" "$@"; then
      if ! [ -e "$coordinated_pair_file" ]; then
        printf '%s\\n' 'http-enter' >> "$coordinated_pair_trace"
        touch "$root/listener-publication-release"
        if ! wait_for_fixture_file "$root/listener-publication-ack"; then
          printf '%s\\n' 'coordinated listener publication acknowledgement failed' >&2
          exit 2
        fi
        coordinated_pid=$(cat "$root/runner.pid" 2>/dev/null || printf '')
        case "$coordinated_pid" in
          ''|*[!0-9]*) printf '%s\\n' 'coordinated listener PID unavailable' >&2; exit 2 ;;
        esac
        printf '%s %s\\n' 1 "$coordinated_pid" > "$coordinated_pair_file.$$" && mv "$coordinated_pair_file.$$" "$coordinated_pair_file"
        printf 'http 1 %s\\n' "$coordinated_pid" >> "$coordinated_pair_trace"
      fi
      coordinated_pair_latch=$(cat "$coordinated_pair_file")
      coordinated_pid=\${coordinated_pair_latch#* }
      printf '%s\\n' "$coordinated_pid"
      exit 0
    fi
    if has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@" && [ -e "$coordinated_pair_file" ]; then
      coordinated_pair_latch=$(cat "$coordinated_pair_file")
      if [ "\${coordinated_pair_latch%% *}" != 1 ]; then
        printf '%s\\n' 'coordinated listener pair malformed' >&2
        exit 2
      fi
      coordinated_pid=\${coordinated_pair_latch#* }
      case "$coordinated_pid" in
        ''|*[!0-9]*) printf '%s\\n' 'coordinated listener pair PID malformed' >&2; exit 2 ;;
      esac
      printf '%s\\n' 'mjpeg-entry 1' >> "$coordinated_pair_trace"
      if [ "$mode" = "listener-mjpeg-duplicate" ]; then
        printf '%s\\n' 'mjpeg 1 DUPLICATE' >> "$coordinated_pair_trace"
        printf '%s\\n' 11111 22222
      else
        printf 'mjpeg 1 %s\\n' "$coordinated_pid" >> "$coordinated_pair_trace"
        printf '%s\\n' "$coordinated_pid"
      fi
      exit 0
    fi
  fi
  if [ "$mode" = "listener-http-only" ] && ! has_arg -d "$@" && [ -e "$root/runner.pid" ] && has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@"; then
    exit 1
  fi
  if [ "$mode" = "listener-uncoordinated-negative" ] && grep -q '^  "pid"' "$root/state/owner.json" 2>/dev/null && ! [ -e "$root/uncoordinated-wda-observed" ] && ! has_arg -d "$@" && has_arg "-iTCP:$IOS_WDA_PORT" "$@"; then
    if ! wait_for_fixture_pid "$root/xcode.pid"; then
      printf '%s\\n' 'uncoordinated publication source unavailable' >&2
      exit 2
    fi
    touch "$root/uncoordinated-wda-observed"
    printf '%s\\n' 'uncoordinated-wda-empty' >> "$root/listener-pair-trace"
    exit 0
  fi
  if [ "$mode" = "listener-uncoordinated-negative" ] && grep -q '^  "pid"' "$root/state/owner.json" 2>/dev/null && ! [ -e "$root/uncoordinated-mjpeg-observed" ] && ! has_arg -d "$@" && has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@"; then
    if ! wait_for_fixture_file "$root/uncoordinated-wda-observed" || ! wait_for_fixture_pid "$root/xcode.pid"; then
      printf '%s\\n' 'uncoordinated publication source unavailable' >&2
      exit 2
    fi
    uncoordinated_pid=$(cat "$root/xcode.pid")
    if ! publish_fixture_pid "$root/runner.pid" "$uncoordinated_pid"; then
      printf '%s\\n' 'uncoordinated publication PID unavailable' >&2
      exit 2
    fi
    printf 'uncoordinated-publication %s\\n' "$uncoordinated_pid" >> "$root/listener-pair-trace"
    touch "$root/uncoordinated-mjpeg-observed"
    printf 'uncoordinated-mjpeg %s\\n' "$uncoordinated_pid" >> "$root/listener-pair-trace"
    printf '%s\\n' 'listener command diagnostic' >&2
    exit 2
  fi
  if { [ "$mode" = "listener-first-failure" ] || [ "$mode" = "listener-second-failure" ]; } && ! has_arg -d "$@"; then
    listener_pair_file=$root/listener-pair
    listener_pair_count_file=$root/listener-pair-count
    listener_pair_trace=$root/listener-pair-trace
    listener_publication_release=$root/listener-publication-release
    listener_publication_ack=$root/listener-publication-ack
    listener_http=false
    listener_mjpeg=false
    has_arg "-iTCP:$IOS_WDA_PORT" "$@" && listener_http=true
    has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@" && listener_mjpeg=true
    if [ "$listener_http" = true ]; then
      if ! [ -e "$root/xcode.pid" ] && grep -q '^  "pid"' "$root/state/owner.json" 2>/dev/null; then
        listener_start_deadline=$(cat "$root/state/deadline" 2>/dev/null || printf '')
        while ! [ -e "$root/xcode.pid" ]; do
          if [ -e "$root/stop" ] || grep -q '"firstFailure"' "$root/state/owner.json" 2>/dev/null; then
            printf '%s\\n' 'listener server start cancelled' >&2
            exit 2
          fi
          case "$listener_start_deadline" in
            ''|*[!0-9]*) printf '%s\\n' 'listener server start deadline unavailable' >&2; exit 2 ;;
          esac
          listener_now=$(date +%s%3N 2>/dev/null || printf '')
          case "$listener_now" in
            ''|*[!0-9]*) listener_now=$(date +%s)000 ;;
          esac
          if [ "$listener_now" -ge "$listener_start_deadline" ]; then
            printf '%s\\n' 'listener server start deadline' >&2
            exit 2
          fi
          /bin/sleep 0.01
        done
      fi
      listener_pair_number=0
      [ -e "$listener_pair_count_file" ] && listener_pair_number=$(cat "$listener_pair_count_file")
      case "$listener_pair_number" in
        ''|*[!0-9]*) printf '%s\\n' 'listener pair counter malformed' >&2; exit 2 ;;
      esac
      listener_pair_number=$((listener_pair_number + 1))
      printf '%s\\n' "$listener_pair_number" > "$listener_pair_count_file.$$" && mv "$listener_pair_count_file.$$" "$listener_pair_count_file"
      listener_pair_value=EMPTY
      if [ -e "$listener_publication_ack" ] && [ -e "$root/runner.pid" ]; then
        listener_candidate_pid=$(cat "$root/runner.pid")
        case "$listener_candidate_pid" in
          ''|*[!0-9]*) ;;
          *) listener_pair_value=$listener_candidate_pid ;;
        esac
      fi
      printf '%s %s\\n' "$listener_pair_number" "$listener_pair_value" > "$listener_pair_file.$$" && mv "$listener_pair_file.$$" "$listener_pair_file"
      printf 'http %s %s\\n' "$listener_pair_number" "$listener_pair_value" >> "$listener_pair_trace"
      if [ "$listener_pair_value" = EMPTY ]; then exit 1; fi
      if [ "$mode" = "listener-first-failure" ]; then
        printf '%s\\n' 'listener command diagnostic' >&2
        exit 2
      fi
      printf '%s\\n' "$listener_pair_value"
      exit 0
    fi
    if [ "$listener_mjpeg" = true ]; then
      if ! [ -e "$listener_pair_file" ]; then
        printf '%s\\n' 'listener pair latch missing' >&2
        exit 2
      fi
      listener_entry_latch=$(cat "$listener_pair_file")
      listener_entry_number=\${listener_entry_latch%% *}
      case "$listener_entry_number" in
        ''|*[!0-9]*) printf '%s\\n' 'listener pair latch malformed' >&2; exit 2 ;;
      esac
      printf 'mjpeg-entry %s\\n' "$listener_entry_number" >> "$listener_pair_trace"
      if [ -e "$root/xcode.pid" ] && ! [ -e "$listener_publication_ack" ] && ! [ -e "$listener_publication_release" ]; then
        touch "$listener_publication_release"
      fi
      if [ -e "$listener_publication_release" ] && ! [ -e "$listener_publication_ack" ]; then
        listener_ack_deadline=$(cat "$root/state/deadline" 2>/dev/null || printf '')
        while ! [ -e "$listener_publication_ack" ]; do
          if [ -e "$root/stop" ] || grep -q '"firstFailure"' "$root/state/owner.json" 2>/dev/null; then
            printf '%s\\n' 'listener publication acknowledgement cancelled' >&2
            exit 2
          fi
          case "$listener_ack_deadline" in
            ''|*[!0-9]*) printf '%s\\n' 'listener publication deadline unavailable' >&2; exit 2 ;;
          esac
          listener_now=$(date +%s%3N 2>/dev/null || printf '')
          case "$listener_now" in
            ''|*[!0-9]*) listener_now=$(date +%s)000 ;;
          esac
          if [ "$listener_now" -ge "$listener_ack_deadline" ]; then
            printf '%s\\n' 'listener publication acknowledgement deadline' >&2
            exit 2
          fi
          /bin/sleep 0.01
        done
      fi
      if [ -e "$listener_publication_ack" ]; then
        printf 'mjpeg-ack %s\\n' "$listener_entry_number" >> "$listener_pair_trace"
      fi
      if ! [ -e "$listener_pair_file" ]; then
        printf '%s\\n' 'listener pair latch missing' >&2
        exit 2
      fi
      listener_pair_latch=$(cat "$listener_pair_file")
      listener_pair_number=\${listener_pair_latch%% *}
      listener_pair_value=\${listener_pair_latch#* }
      case "$listener_pair_number" in
        ''|*[!0-9]*) printf '%s\\n' 'listener pair latch malformed' >&2; exit 2 ;;
      esac
      if [ "$listener_pair_number" != "$listener_entry_number" ]; then
        printf '%s\\n' 'listener pair latch replaced before consumption' >&2
        exit 2
      fi
      case "$listener_pair_value" in
        EMPTY) ;;
        ''|*[!0-9]*) printf '%s\\n' 'listener pair latch malformed' >&2; exit 2 ;;
      esac
      printf 'mjpeg %s %s\\n' "$listener_pair_number" "$listener_pair_value" >> "$listener_pair_trace"
      if [ "$listener_pair_value" = EMPTY ]; then exit 1; fi
      if [ "$mode" = "listener-second-failure" ]; then
        printf '%s\\n' 'listener command diagnostic' >&2
        exit 2
      fi
      printf '%s\\n' "$listener_pair_value"
      exit 0
    fi
  fi
  if { [ "$mode" = "swap" ] || [ "$mode" = "pid-reuse" ] || [ "$mode" = "listener-invalid-pid" ]; } && ! has_arg -d "$@"; then
    listener_control_trace=$root/listener-control-trace
    listener_control_http=false
    listener_control_mjpeg=false
    has_arg "-iTCP:$IOS_WDA_PORT" "$@" && listener_control_http=true
    has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@" && listener_control_mjpeg=true
    if [ "$listener_control_http" = true ]; then
      if ! [ -e "$root/xcode.pid" ] && grep -q '^  "pid"' "$root/state/owner.json" 2>/dev/null; then
        if ! wait_for_fixture_file "$root/xcode.pid"; then
          printf '%s\\n' 'listener control server start failed' >&2
          exit 2
        fi
      fi
      if [ "$mode" = "listener-invalid-pid" ]; then
        if ! [ -e "$root/xcode.pid" ]; then
          printf 'http EMPTY\\n' >> "$listener_control_trace"
          exit 1
        fi
        if ! wait_for_fixture_file "$root/listener-publication-ack"; then
          printf '%s\\n' 'listener control publication failed' >&2
          exit 2
        fi
        printf 'http INVALID\\n' >> "$listener_control_trace"
        printf '%s\\n' 2147483648
        exit 0
      fi
      if ! [ -e "$root/runner.pid" ]; then
        printf 'http EMPTY\\n' >> "$listener_control_trace"
        exit 1
      fi
      listener_control_count=0
      [ -e "$root/swap-lsof-count" ] && listener_control_count=$(cat "$root/swap-lsof-count")
      case "$listener_control_count" in
        ''|*[!0-9]*) printf '%s\\n' 'listener control counter malformed' >&2; exit 2 ;;
      esac
      listener_control_next=$((listener_control_count + 1))
      printf '%s' "$listener_control_next" > "$root/swap-lsof-count.$$" && mv "$root/swap-lsof-count.$$" "$root/swap-lsof-count"
      listener_control_pid=11111
      if [ "$mode" = "swap" ] && [ "$listener_control_count" -ge 5 ]; then listener_control_pid=22222; fi
      printf 'http %s %s\\n' "$listener_control_next" "$listener_control_pid" >> "$listener_control_trace"
      printf '%s\\n' "$listener_control_pid"
      exit 0
    fi
    if [ "$listener_control_mjpeg" = true ]; then
      if [ "$mode" = "listener-invalid-pid" ]; then
        if ! [ -e "$root/xcode.pid" ]; then
          printf 'mjpeg EMPTY\\n' >> "$listener_control_trace"
          exit 1
        fi
        if ! wait_for_fixture_file "$root/listener-publication-ack"; then
          printf '%s\\n' 'listener control publication failed' >&2
          exit 2
        fi
        printf 'mjpeg INVALID\\n' >> "$listener_control_trace"
        printf '%s\\n' 2147483648
        exit 0
      fi
      if ! [ -e "$root/xcode.pid" ]; then
        printf 'mjpeg EMPTY\\n' >> "$listener_control_trace"
        exit 1
      fi
      if ! [ -e "$root/runner.pid" ]; then
        touch "$root/listener-publication-release"
        if ! wait_for_fixture_file "$root/listener-publication-ack"; then
          printf '%s\\n' 'listener control publication failed' >&2
          exit 2
        fi
      fi
      listener_control_count=0
      [ -e "$root/swap-lsof-count" ] && listener_control_count=$(cat "$root/swap-lsof-count")
      case "$listener_control_count" in
        ''|*[!0-9]*) printf '%s\\n' 'listener control counter malformed' >&2; exit 2 ;;
      esac
      listener_control_next=$((listener_control_count + 1))
      printf '%s' "$listener_control_next" > "$root/swap-lsof-count.$$" && mv "$root/swap-lsof-count.$$" "$root/swap-lsof-count"
      listener_control_pid=11111
      if [ "$mode" = "swap" ] && [ "$listener_control_count" -ge 5 ]; then listener_control_pid=22222; fi
      printf 'mjpeg %s %s\\n' "$listener_control_next" "$listener_control_pid" >> "$listener_control_trace"
      printf '%s\\n' "$listener_control_pid"
      exit 0
    fi
  fi
  if [ "$mode" = "listener-concurrent-exit-before-close" ] && [ -e "$root/xcode.pid" ] && ! [ -e "$root/runner.pid" ]; then
    while ! [ -e "$root/runner.pid" ] && ! [ -e "$root/stop" ] && ! grep -q '"firstFailure"' "$root/state/owner.json"; do /bin/sleep 0.01; done
  fi
  pending_runner_pid=
  if [ -e "$root/runner.pid" ]; then
    pending_runner_pid=$(cat "$root/runner.pid")
  elif [ -e "$root/state/owner.json" ]; then
    pending_runner_pid=$(grep -m 1 '"pid"' "$root/state/owner.json" | tr -cd '0-9')
  fi
  if { [ "$mode" = "listener-pending-ready" ] || [ "$mode" = "listener-pending-deadline" ] || [ "$mode" = "listener-pending-ambiguous" ] || [ "$mode" = "listener-pending-pid" ] || [ "$mode" = "listener-pending-http-ambiguous" ] || [ "$mode" = "listener-pending-http-swap" ] || [ "$mode" = "listener-pending-http-only" ] || [ "$mode" = "listener-pending-invalid-pid" ] || [ "$mode" = "listener-pending-executable" ] || [ "$mode" = "listener-pending-birth" ] || [ "$mode" = "listener-pending-command-error" ] || [ "$mode" = "listener-pending-disappear" ] || [ "$mode" = "listener-pending-product" ] || [ "$mode" = "listener-pending-xctestrun" ] || [ "$mode" = "listener-pending-ownership" ] || [ "$mode" = "listener-pending-hash" ] || [ "$mode" = "listener-pending-bundle" ] || [ "$mode" = "listener-pending-simulator" ] || [ "$mode" = "listener-pending-receipt" ] || [ "$mode" = "listener-pending-exit" ] || [ "$mode" = "listener-pending-stop" ] || [ "$mode" = "listener-concurrent-exit-before-close" ] || [ "$mode" = "listener-pending-child-error" ] || [ "$mode" = "listener-pending-output-limit" ] || [ "$mode" = "listener-pending-owner-evidence-write" ]; } && ! has_arg -d "$@" && [ -n "$pending_runner_pid" ]; then
    pending_wda_count=0
    [ -e "$root/pending-wda-count" ] && pending_wda_count=$(cat "$root/pending-wda-count")
    if has_arg "-iTCP:$IOS_WDA_PORT" "$@"; then
      pending_wda_count=$((pending_wda_count + 1))
      printf '%s' "$pending_wda_count" > "$root/pending-wda-count"
      if [ "$pending_wda_count" -eq 3 ] && [ "$mode" != "listener-pending-exit" ] && [ "$mode" != "listener-pending-child-error" ] && ! [ -e "$root/pending-candidate-continue" ] && ! [ -e "$root/stop" ]; then
        touch "$root/pending-candidate-boundary"
        while ! [ -e "$root/pending-candidate-continue" ] && ! [ -e "$root/stop" ]; do /bin/sleep 0.01; done
      fi
      if [ "$mode" = "listener-pending-child-error" ] && [ "$pending_wda_count" -ge 2 ]; then
        touch "$root/pending-child-error"
        exit 1
      fi
      if [ "$mode" = "listener-pending-deadline" ] && [ -e "$root/pending-deadline-command" ]; then
        rm -f "$root/pending-deadline-command"
        /bin/sleep 10
      fi
      if [ "$mode" = "listener-pending-deadline" ] || [ "$pending_wda_count" -lt 4 ]; then exit 1; fi
      if [ "$mode" = "listener-pending-http-ambiguous" ]; then
        printf '%s\\n' "$pending_wda_count/2" >> "$root/pending-observations"
        printf '%s\\n' "$pending_runner_pid" 22222
        exit 0
      fi
      if [ "$mode" = "listener-pending-http-swap" ]; then
        printf '%s\\n' "$pending_wda_count/1" >> "$root/pending-observations"
        printf '%s\\n' 22222
        exit 0
      fi
      printf '%s\\n' "$pending_runner_pid"
      exit 0
    fi
    if has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@"; then
      pending_mjpeg_count=0
      [ -e "$root/pending-mjpeg-count" ] && pending_mjpeg_count=$(cat "$root/pending-mjpeg-count")
      pending_mjpeg_count=$((pending_mjpeg_count + 1))
      printf '%s' "$pending_mjpeg_count" > "$root/pending-mjpeg-count"
      if [ "$mode" = "listener-pending-deadline" ] && [ "$pending_wda_count" -eq 2 ]; then
        touch "$root/pending-deadline-command"
      fi
      if [ "$mode" = "listener-pending-invalid-pid" ] && [ "$pending_wda_count" -eq 2 ]; then
        touch "$root/pending-invalid-boundary" || exit 2
        pending_child_pid=$(grep '^  "pid":' "$root/state/owner.json" | cut -d : -f 2 | tr -d ' ,')
        case "$pending_child_pid" in
          ''|0*|*[!0-9]*) pending_child_pid= ;;
        esac
        if [ -z "$pending_child_pid" ] || [ "\${#pending_child_pid}" -gt 10 ] || [ "$pending_child_pid" -gt 2147483647 ] || ! wait_for_fixture_pid "$root/xcode.pid" "$pending_child_pid"; then
          printf '%s\\n' PREREQUISITE > "$root/pending-invalid-prerequisite"
          printf '%s\\n' 'pending invalid PID launch prerequisite failed' >&2
          exit 2
        fi
        printf '%s\\n' "$fixture_pid" > "$root/pending-invalid-acknowledged" || exit 2
        printf '%s/%s\\n' "$pending_wda_count" invalid >> "$root/pending-observations"
        printf '%s\\n' 2147483648
        exit 0
      fi
      if { [ "$pending_wda_count" -eq 1 ] || { [ "$mode" = "listener-pending-disappear" ] && [ "$pending_wda_count" -ge 3 ]; } || { [ "$mode" = "listener-pending-http-only" ] && [ "$pending_wda_count" -ge 4 ]; }; }; then
        printf '%s/%s\n' "$pending_wda_count" 0 >> "$root/pending-observations"
        exit 1
      fi
      if [ "$mode" = "listener-pending-ambiguous" ] && [ "$pending_wda_count" -eq 2 ]; then
        printf '%s/%s\n' "$pending_wda_count" 2 >> "$root/pending-observations"
        printf '11111\n22222\n'
        exit 0
      fi
      if [ "$mode" = "listener-pending-pid" ] && [ "$pending_wda_count" -ge 3 ]; then
        printf '%s/%s\n' "$pending_wda_count" 1 >> "$root/pending-observations"
        printf '%s\n' 22222
        exit 0
      fi
      printf '%s/%s\n' "$pending_wda_count" 1 >> "$root/pending-observations"
      printf '%s\n' "$pending_runner_pid"
      exit 0
    fi
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
    if { [ "$mode" = "listener-pending-executable" ] || [ "$mode" = "listener-pending-birth" ] || [ "$mode" = "listener-pending-simulator" ] || [ "$mode" = "listener-pending-command-error" ] || [ "$mode" = "listener-pending-product" ] || [ "$mode" = "listener-pending-xctestrun" ] || [ "$mode" = "listener-pending-ownership" ] || [ "$mode" = "listener-pending-hash" ] || [ "$mode" = "listener-pending-bundle" ] || [ "$mode" = "listener-pending-stop" ]; }; then
      pending_evidence_count=0
      [ -e "$root/pending-evidence-count" ] && pending_evidence_count=$(cat "$root/pending-evidence-count")
      pending_evidence_count=$((pending_evidence_count + 1))
      printf '%s' "$pending_evidence_count" > "$root/pending-evidence-count"
      if [ "$mode" = "listener-pending-command-error" ]; then exit 23; fi
      if [ "$mode" = "listener-pending-product" ] && [ "$pending_evidence_count" -ge 2 ]; then
        printf '%s' 'changed pending product' > "$STARTUP_TEST_PRODUCT/WebDriverAgentRunner-Runner"
      fi
      if [ "$mode" = "listener-pending-xctestrun" ] && [ "$pending_evidence_count" -ge 2 ]; then
        printf '%s' 'changed pending xctestrun' >> "$STARTUP_TEST_XCTESTRUN"
      fi
      if [ "$mode" = "listener-pending-ownership" ] && [ "$pending_evidence_count" -ge 2 ]; then
        printf 'ios:wrong' > "$root/owned"
      fi
      if [ "$mode" = "listener-pending-stop" ] && [ "$pending_evidence_count" -ge 1 ]; then
        touch "$root/pending-stop-observed"
      fi
    fi
    if { [ "$mode" = "listener-initial-race" ] || [ "$mode" = "listener-initial-race-receipt" ] || [ "$mode" = "listener-initial-race-receipt-command-error" ] || [ "$mode" = "listener-initial-race-receipt-hash-error" ] || [ "$mode" = "listener-initial-race-birth" ] || [ "$mode" = "listener-initial-race-executable" ] || [ "$mode" = "listener-initial-race-hash" ] || [ "$mode" = "listener-initial-race-command-error" ] || [ "$mode" = "listener-initial-race-budget" ] || [ "$mode" = "listener-initial-race-cleanup-identity" ] || [ "$mode" = "listener-initial-race-command-error-late" ] || [ "$mode" = "listener-initial-race-hash-budget" ]; }; then
      race_inspection_count=0
      [ -e "$root/race-inspection-count" ] && race_inspection_count=$(cat "$root/race-inspection-count")
      printf '%s' "$((race_inspection_count + 1))" > "$root/race-inspection-count"
      if { [ "$mode" = "listener-initial-race-receipt" ] || [ "$mode" = "listener-initial-race-receipt-command-error" ] || [ "$mode" = "listener-initial-race-receipt-hash-error" ]; } && [ "$race_inspection_count" -ge 1 ] && ! grep -q '"firstFailure"' "$root/state/owner.json" 2>/dev/null; then
        exit 23
      fi
      if [ "$mode" = "listener-initial-race-birth" ] && [ "$race_inspection_count" -eq 0 ]; then
        touch "$root/initial-inspection"
      fi
      if [ "$mode" = "listener-initial-race-command-error" ] && [ "$race_inspection_count" -ge 1 ]; then
        exit 23
      fi
      if [ "$mode" = "listener-initial-race-budget" ] && [ "$race_inspection_count" -ge 1 ]; then
        /bin/sleep 2
      fi
      if [ "$mode" = "listener-initial-race-hash" ] && [ "$race_inspection_count" -ge 1 ]; then
        printf '%s' 'different diagnostic listener' > "$STARTUP_TEST_RUNNER_RECEIPT/WebDriverAgentRunner-Runner"
      fi
      if [ "$mode" = "listener-initial-race-hash-budget" ] && [ "$race_inspection_count" -ge 1 ]; then
        rm -f "$STARTUP_TEST_RUNNER_RECEIPT/WebDriverAgentRunner-Runner"
        mkfifo "$STARTUP_TEST_RUNNER_RECEIPT/WebDriverAgentRunner-Runner"
      fi
      if [ "$mode" = "listener-initial-race" ] && [ "$race_inspection_count" -ge 1 ]; then
        printf '%s' 'changed initial race xctestrun' >> "$STARTUP_TEST_XCTESTRUN"
      fi
    fi
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
    if [ "$mode" = "listener-pending-executable" ] && [ "\${pending_evidence_count:-0}" -ge 2 ]; then
      executable=$STARTUP_TEST_RUNNER_RECEIPT/unexpected-listener
    fi
    if [ "$mode" = "listener-pending-hash" ] && [ "\${pending_evidence_count:-0}" -ge 2 ] && [ -e "$root/pending-candidate-continue" ]; then
      printf '%s' 'different pending listener' > "$STARTUP_TEST_RUNNER_RECEIPT/WebDriverAgentRunner-Runner"
    fi
    if [ "$mode" = "listener-pending-simulator" ] && [ "\${pending_evidence_count:-0}" -ge 2 ]; then
      executable=$root/Devices/WRONG/data/Containers/Bundle/Application/1C1FB5F7-2D0E-49AC-BE09-62816F229E5C/WebDriverAgentRunner-Runner.app/WebDriverAgentRunner-Runner
    fi
    if [ "$mode" = "listener-initial-race-executable" ] && [ "\${race_inspection_count:-0}" -ge 1 ]; then
      executable=$STARTUP_TEST_RUNNER_RECEIPT/unexpected-listener
    fi
    if [ "$mode" = "listener-initial-race-cleanup-identity" ] && [ -e "$root/cleanup-boundary" ]; then
      executable=$STARTUP_TEST_RUNNER_RECEIPT/unexpected-listener
    fi
    printf 'p%s\\nftxt\\nn%s\\n' "$requested" "$executable"
    exit 0
  fi
  if [ "$mode" = "listener-mjpeg-duplicate" ] && has_arg "-iTCP:$IOS_WDA_MJPEG_PORT" "$@"; then
    printf '11111\\n22222\\n'
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
  if [ "$mode" = "listener-concurrent-exit-before-close" ] && [ "$format" = "stat=" ]; then
    printf '%s' S > "$root/child-state-probe"
    printf '%s\n' S
    if [ -e "$root/status-queries" ] && ! [ -e "$root/exit-child-requested" ]; then
      touch "$root/exit-child-requested"
      if ! wait_for_fixture_file "$root/child-exit-observed" "$root/child-exit-probe-error"; then
        printf '%s\\n' 'XCTest observer ERROR: terminal observation unavailable' >&2
        exit 2
      fi
    fi
  elif [ "$mode" = "listener-initial-race-birth" ] && [ "$format" = "lstart=" ]; then
    if [ ! -e "$root/initial-inspection" ]; then
      printf '%s\\n' child-birth
    elif [ ! -e "$root/race-birth-observed" ]; then
      touch "$root/race-birth-observed"
      printf '%s\\n' birth-a
    else
      printf '%s\\n' birth-b
    fi
  elif [ "$mode" = "listener-initial-race-cleanup-identity" ] && [ "$format" = "lstart=" ]; then
    if [ -e "$root/cleanup-boundary" ] || [ "$(cat "$root/race-inspection-count" 2>/dev/null || printf 0)" -ge 2 ]; then printf '%s\\n' birth-b; else printf '%s\\n' birth-a; fi
  elif [ "$mode" = "listener-initial-race-command-error-late" ] && [ "$format" = "command=" ]; then
    race_inspection_count=0
    [ -e "$root/race-inspection-count" ] && race_inspection_count=$(cat "$root/race-inspection-count")
    if [ "$race_inspection_count" -ge 2 ]; then exit 23; fi
    printf 'xcodebuild test-without-building -xctestrun %s -destination id=%s\\n' "$STARTUP_TEST_XCTESTRUN" "$IOS_SIMULATOR_UDID"
  elif [ "$mode" = "listener-pending-birth" ] && [ "$format" = "lstart=" ] && [ "$(cat "$root/pending-evidence-count" 2>/dev/null || printf 0)" -ge 2 ]; then
    printf '%s\\n' birth-b
  elif [ "$mode" = "listener-pending-command-error" ] && [ "$format" = "command=" ]; then
    exit 23
  elif { [ "$mode" = "swap" ] || [ "$mode" = "pid-reuse" ]; } && [ "$pid" != "$xcode_pid" ]; then
    generation=0
    [ -e "$root/swap-lsof-count" ] && generation=$(cat "$root/swap-lsof-count")
    reused=false
    [ "$mode" = "pid-reuse" ] && [ "$generation" -ge 7 ] && reused=true
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
    elif [ "$mode" = "listener-pending-receipt" ] && [ "$count" -ge 1 ]; then
      printf '%s\\n' "$root/invalid-receipt"
    elif [ "$mode" = "listener-initial-race-receipt" ] && [ "$count" -ge 1 ]; then
      printf '%s\\n' "$STARTUP_TEST_RUNNER_RECEIPT"
    elif [ "$mode" = "listener-initial-race-receipt-hash-error" ] && [ "$count" -ge 1 ]; then
      rm -f "$STARTUP_TEST_PRODUCT/WebDriverAgentRunner-Runner"
      printf '%s\\n' "$STARTUP_TEST_RECEIPT"
    else
      printf '%s\\n' "$STARTUP_TEST_RECEIPT"
    fi
  elif [ "$2" = "spawn" ]; then
    printf '%s\\n' 'retained simulator diagnostic'
  elif [ "$2" = "terminate" ] && [ "$mode" = "noisy-cleanup" ]; then
    cat "$root/runner.pid" > "$root/cleanup-pid"
    touch "$root/cleanup-marker"
  elif [ "$2" = "terminate" ] && { [ "$mode" = "listener-concurrent-exit-before-close" ] || [ "\${mode#listener-exit-after-ready}" != "$mode" ]; }; then
    kill -TERM "$(cat "$root/endpoint.pid")" 2>/dev/null || true
  fi
  ;;
xcodebuild)
  case "$mode" in
    listener-pending-*) publish_fixture_pid "$root/runner.pid" "$$" || exit 2 ;;
    listener-concurrent-exit-before-close|listener-exit-after-ready*) publish_fixture_pid "$root/xcode.pid" "$$" || exit 2 ;;
    listener-initial-race-receipt|listener-initial-race-receipt-command-error|listener-initial-race-receipt-hash-error)
      touch "$root/initial-race-receipt-child-started"
      ;;
  esac
  exec "$STARTUP_TEST_EXECUTABLE" "$STARTUP_TEST_WDA_SERVER" xcodebuild "$@"
  ;;
esac
exit 0
`;
const pendingInvalidPidWait = String.raw`import { existsSync, readFileSync, renameSync, statSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
const startedNs = process.hrtime.bigint();
const root = process.env.STARTUP_TEST_ROOT;
let result = 'READ_ERROR';
const fail = reason => { result = reason; throw new Error('pending invalid PID prerequisite'); };
const read = (name, limit) => {
  const path = join(root, name);
  if (statSync(path).size > limit) fail('MALFORMED');
  const text = readFileSync(path, 'utf8');
  if (Buffer.byteLength(text) > limit) fail('MALFORMED');
  return text;
};
const publish = (name, text) => {
  const path = join(root, name);
  writeFileSync(path + '.next', text);
  renameSync(path + '.next', path);
};
try {
  const expected = process.argv[2];
  if (!/^[1-9]\d{0,9}$/.test(expected) || Number(expected) > 2147483647) fail('IDENTITY');
  const deadlineText = read('state/deadline', 16);
  if (!/^\d{13}$/.test(deadlineText)) fail('BUDGET');
  const startupDeadline = Number(deadlineText);
  const budgetMs = Math.min(2000, startupDeadline - Date.now());
  if (budgetMs <= 0) fail('BUDGET');
  const endNs = startedNs + BigInt(budgetMs) * 1000000n;
  const check = () => {
    if (process.hrtime.bigint() >= endNs || Date.now() >= startupDeadline) fail('BUDGET');
    if (['stop', 'state/stop', 'pending-invalid-abort'].some(name => existsSync(join(root, name)))) fail('CANCELLED');
    const owner = JSON.parse(read('state/owner.json', 1048576));
    if (owner.firstFailure || owner.diagnostics?.lifecycle?.some(event => ['child-error', 'child-exit', 'child-close'].includes(event.event))) fail('CHILD_FAILURE');
    if (String(owner.pid) !== expected) fail('IDENTITY');
    if (process.hrtime.bigint() >= endNs || Date.now() >= startupDeadline) fail('BUDGET');
    if (existsSync(join(root, 'pending-invalid-withheld'))) fail('WITHHELD');
  };
  check();
  publish('pending-invalid-budget', JSON.stringify({startedNs: String(startedNs), endNs: String(endNs), startupDeadline, budgetMs}));
  while (true) {
    check();
    if (existsSync(join(root, 'xcode.pid'))) {
      if (read('xcode.pid', 11) !== expected + '\n') fail('MALFORMED');
      check();
      publish('pending-invalid-result', JSON.stringify({result: 'ACKNOWLEDGED', observedNs: String(process.hrtime.bigint())}));
      check();
      process.stdout.write(expected + '\n');
      break;
    }
    if (!existsSync(join(root, 'pending-invalid-waiting'))) publish('pending-invalid-waiting', expected + '\n');
    await new Promise(resolve => setTimeout(resolve, 1));
  }
} catch {
  process.exitCode = 2;
  try { publish('pending-invalid-result', JSON.stringify({result, observedNs: String(process.hrtime.bigint())})); }
  catch { result = 'RECEIPT_UNAVAILABLE'; }
  process.stderr.write('pending invalid PID prerequisite ' + result + '\n');
}
`;
function listenerFailureFields(value: unknown, mode: string, producerPid: number, parentPid: number, live: boolean) {
  const record = (input: unknown): Record<string, unknown> => input && typeof input === 'object' && !Array.isArray(input) ? input as Record<string, unknown> : {};
  const owner = record(value);
  const lifecycle = record(owner.diagnostics).lifecycle;
  if (!Array.isArray(lifecycle) || !Number.isSafeInteger(producerPid) || producerPid <= 0 || !Number.isSafeInteger(parentPid) || parentPid <= 0) throw new Error('IDENTITY');
  const events = lifecycle.map(record);
  const spawned = events.filter(event => event.event === 'child-spawned');
  const started = events.filter(event => event.event === 'supervisor-start');
  if (spawned.length !== 1 || started.length !== 1 || owner.pid !== producerPid
    || record(spawned[0].detail).pid !== producerPid || record(spawned[0].detail).phase !== 'startup'
    || record(started[0].detail).pid !== parentPid || record(started[0].detail).phase !== 'startup'
    || !Number.isSafeInteger(spawned[0].id) || Number(spawned[0].id) <= 0) throw new Error('IDENTITY');
  const first = events.filter(event => event.event === 'failure-observed' && record(event.detail).first === true);
  const failure = record(owner.firstFailure);
  const stage = mode === 'listener-first-failure' ? 'wda-listener-command' : 'mjpeg-listener-command';
  if (!['listener-first-failure', 'listener-second-failure', 'listener-uncoordinated-negative'].includes(mode)
    || owner.ready !== false || (live && Object.hasOwn(owner, 'endedAt')) || first.length !== 1
    || failure.phase !== 'initial' || failure.stage !== stage || failure.category !== 'listener-command-error'
    || failure.source !== 'tests/mobile/support/ios-xctest.ts' || failure.message !== 'XCTEST: listener-command-error'
    || failure.frozenOwner !== false || record(first[0].detail).first !== true
    || record(first[0].detail).stage !== stage || record(first[0].detail).category !== failure.category
    || record(first[0].detail).phase !== failure.phase || record(first[0].detail).frozenOwner !== false) throw new Error('MALFORMED');
  const positiveId = (id: unknown) => Number.isSafeInteger(id) && Number(id) > 0;
  const ids = failure.commandIds, commands = failure.causalCommands;
  if (!positiveId(first[0].id) || Number(first[0].id) <= Number(spawned[0].id) || !positiveId(failure.inspectionId)
    || typeof failure.monotonicMs !== 'number' || !Number.isFinite(failure.monotonicMs) || failure.monotonicMs < 0
    || !Array.isArray(ids) || ids.length < 1 || ids.length > 16 || !ids.every(positiveId)
    || !Array.isArray(commands) || commands.length !== (mode === 'listener-first-failure' ? 1 : 2)
    || commands.length !== ids.length || record(failure.predicate).status !== 'evaluated'
    || JSON.stringify(record(failure.predicate).listenerValidation) !== JSON.stringify(owner.listenerValidation)) throw new Error('MALFORMED');
  for (const input of commands) {
    const command = record(input), stdout = record(command.stdout), stderr = record(command.stderr);
    if (!ids.includes(command.id) || command.phase !== 'initial' || command.operation !== 'inspect-listener'
      || command.source !== failure.source || command.signal !== null || command.error !== undefined
      || !Number.isSafeInteger(command.port) || Number(command.port) < 1 || Number(command.port) > 65535
      || JSON.stringify(command.command) !== JSON.stringify(['lsof', '-nP', `-iTCP:${command.port}`, '-sTCP:LISTEN', '-t'])
      || ![0, 1, 2].includes(Number(command.status)) || typeof command.status !== 'number'
      || !Number.isSafeInteger(stdout.bytes) || Number(stdout.bytes) < 0 || !Number.isSafeInteger(stderr.bytes) || Number(stderr.bytes) < 0
      || stdout.content !== (stdout.bytes ? '[listener command output suppressed]\n' : '')
      || stderr.content !== (stderr.bytes ? '[listener command output suppressed]\n' : '')) throw new Error('MALFORMED');
  }
  const keys = ['at', 'monotonicMs', 'phase', 'source', 'stage', 'category', 'message', 'frozenOwner', 'predicate', 'status', 'listenerValidation', 'commandIds', 'causalCommands', 'inspectionId',
    'checkedAt', 'endpoints', 'wda', 'mjpeg', 'count', 'pids', 'errorCategory', 'commandStatus', 'port', 'stderrBytes', 'runnerEvidencePresent', 'runnerAssociation', 'mjpegAssociation', 'bundleId', 'executableHash', 'cachedProductExecutableHash', 'cachedProductExecutableHashAt', 'failureStage',
    'id', 'operation', 'command', 'timeoutMs', 'startedAt', 'endedAt', 'monotonicStartMs', 'monotonicEndMs', 'durationMs', 'signal', 'endpoint', 'stdout', 'stderr', 'content', 'bytes', 'truncated', 'suppressed', 'classification', 'event', 'detail', 'first'];
  const strings = ['initial', 'tests/mobile/support/ios-xctest.ts', stage, 'listener-command-error', 'XCTEST: listener-command-error', 'evaluated', 'not-evaluated', 'error', 'command-error', 'inspect-listener', 'wda', 'mjpeg', 'empty', 'empty-or-no-listener', 'present', '', '[listener command output suppressed]\n', 'failure-observed'];
  const copy = (input: unknown, key = '', depth = 0): unknown => {
    if (depth > 9) throw new Error('MALFORMED');
    if (input === null || typeof input === 'boolean') return input;
    if (typeof input === 'number' && Number.isFinite(input) && input >= 0 && input <= Number.MAX_SAFE_INTEGER) return input;
    if (typeof input === 'string') {
      if (['at', 'checkedAt', 'cachedProductExecutableHashAt', 'startedAt', 'endedAt'].includes(key)
        && /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/u.test(input) && Number.isFinite(Date.parse(input))) return input;
      if (key === 'cachedProductExecutableHash' && /^[0-9a-f]{64}$/u.test(input)) return input;
      if (key === 'pids' && input === String(producerPid)) return input;
      if (key === 'command' && (['lsof', '-nP', '-sTCP:LISTEN', '-t'].includes(input) || /^-iTCP:[1-9]\d{0,4}$/u.test(input))) return input;
      if (strings.includes(input)) return input;
      throw new Error('MALFORMED');
    }
    if (Array.isArray(input)) {
      if (input.length > 16) throw new Error('MALFORMED');
      return input.map(entry => copy(entry, key, depth + 1));
    }
    if (!input || typeof input !== 'object') throw new Error('MALFORMED');
    return Object.fromEntries(Object.entries(input).map(([name, entry]) => {
      if (!keys.includes(name)) throw new Error('MALFORMED');
      return [name, copy(entry, name, depth + 1)];
    }));
  };
  return {
    spawnEventId: Number(spawned[0].id),
    firstFailure: copy(failure) as Record<string, unknown>,
    listenerValidation: copy(owner.listenerValidation) as Record<string, unknown>,
    failureEvent: copy(first[0]) as {id: number; detail: {first: boolean; frozenOwner: boolean}},
  };
}

function readListenerText(root: string, name: string, limit: number): string {
  const path = join(root, name);
  const advertised = statSync(path);
  if (!advertised.isFile() || advertised.size > limit) throw new Error('READ_ERROR');
  const fd = openSync(path, 'r');
  try {
    const before = fstatSync(fd);
    if (!before.isFile() || before.size > limit) throw new Error('READ_ERROR');
    const bytes = Buffer.alloc(before.size);
    const length = readSync(fd, bytes, 0, bytes.length, 0);
    const after = fstatSync(fd);
    if (after.size > limit || length !== bytes.length || before.size !== after.size || before.mtimeMs !== after.mtimeMs) throw new Error('READ_ERROR');
    return bytes.toString('utf8');
  } finally { closeSync(fd); }
}

function readListenerRecord(root: string, name: string, limit: number): unknown {
  return JSON.parse(readListenerText(root, name, limit));
}

const wdaServer = `import { createServer } from 'node:http';
import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { appendFileSync, existsSync, readFileSync, renameSync, statSync, unlinkSync, writeFileSync, openSync, fstatSync, readSync, closeSync } from 'node:fs';
import { join } from 'node:path';
const encodeXctestExitProbe = ${encodeXctestExitProbe.toString()};
const listenerFailureFields = ${listenerFailureFields.toString()};
const readListenerText = ${readListenerText.toString()};
const readListenerRecord = ${readListenerRecord.toString()};
const root = process.env.STARTUP_TEST_ROOT;
const mode = process.env.STARTUP_TEST_MODE;
const role = process.env.STARTUP_TEST_WDA_SERVER_ROLE || 'owner';
const listenerWitnessMode = ['listener-first-failure', 'listener-second-failure', 'listener-uncoordinated-negative'].includes(mode);
const captureControl = process.env.STARTUP_TEST_LISTENER_CAPTURE_CONTROL || '';
const holdsDescendant = mode === 'listener-concurrent-exit-before-close' || mode.startsWith('listener-exit-after-ready');
const args = process.argv.slice(3);
const runnerPid = join(root, 'runner.pid');
const xcodePid = join(root, 'xcode.pid');
const publicationRelease = join(root, 'listener-publication-release');
const publicationAck = join(root, 'listener-publication-ack');
const publicationTrace = join(root, 'listener-publication-trace');
const endpointReady = join(root, 'endpoint-ready');
const receiptPublicationRelease = join(root, 'initial-race-receipt-publication-release');
const receiptPublicationAck = join(root, 'initial-race-receipt-publication-ack');
const receiptPublicationTrace = join(root, 'initial-race-receipt-publication-trace');
const receiptListenerReady = join(root, 'initial-race-receipt-listener-ready');
const receiptRaceModes = ['listener-initial-race-receipt', 'listener-initial-race-receipt-command-error', 'listener-initial-race-receipt-hash-error'];
const coordinatedListenerModes = ['credential', 'credential-binary', 'credential-binary-failure', 'listener-mjpeg-duplicate', 'listener-bundle-mismatch', 'listener-hash-mismatch'];
const publishPid = path => {
  const nextPath = path + '.next';
  writeFileSync(nextPath, String(process.pid) + '\\n');
  renameSync(nextPath, path);
};
const remove = path => { try { unlinkSync(path); } catch {} };
if (mode === 'listener-pending-invalid-pid' && process.env.STARTUP_TEST_INVALID_PID_CONTROL) {
  const abortStart = () => { remove(runnerPid); remove(xcodePid); process.exit(48); };
  process.on('SIGTERM', abortStart);
  process.on('SIGINT', abortStart);
  publishPid(join(root, 'pending-invalid-held'));
  const startDeadline = Number(readFileSync(join(root, 'state/deadline'), 'utf8'));
  while (!existsSync(join(root, 'pending-invalid-release'))) {
    if (!Number.isSafeInteger(startDeadline) || Date.now() >= startDeadline
      || existsSync(join(root, 'pending-invalid-abort')) || existsSync(join(root, 'state/stop'))) abortStart();
    await new Promise(resolve => setTimeout(resolve, 1));
  }
  if (existsSync(join(root, 'pending-invalid-abort')) || existsSync(join(root, 'pending-invalid-withheld'))) abortStart();
  process.off('SIGTERM', abortStart);
  process.off('SIGINT', abortStart);
}
if (role !== 'endpoint') appendFileSync(join(root, 'launches'), JSON.stringify(args) + '\\n');
if (mode === 'early') process.exit(43);
if (mode === 'exit-before-close') {
  spawn(process.execPath, ['-e', 'setTimeout(() => {}, 500)'], { stdio: ['ignore', 'inherit', 'inherit'] });
  process.exit(44);
}
if (mode === 'receipt-product-changed' || mode === 'listener-initial-race-product') {
  writeFileSync(join(process.env.STARTUP_TEST_PRODUCT, 'WebDriverAgentRunner-Runner'), 'changed');
  writeFileSync(join(root, 'product-changed-at'), String(Date.now()));
}
if (!listenerWitnessMode && (role !== 'endpoint' || !existsSync(xcodePid))) publishPid(xcodePid);
if (mode === 'credential' || mode === 'credential-binary' || mode === 'credential-binary-failure') {
  process.stdout.write('PASSWORD=bare-password-sentinel API_PASSWORD=plain-password PRIVATE_URL=https://private.example/internal/path?ref=private-reference\\n');
  process.stderr.write('{"PASSWORD":"bare-password-sentinel","api_token":"plain-password","private_url":"https://private.example/internal/path"}\\n');
}
if (mode === 'listener-status-one-stderr') publishPid(runnerPid);
const server = createServer((_request, response) => {
  const statusPath = join(root, 'status-queries');
  const count = existsSync(statusPath) ? Number(readFileSync(statusPath, 'utf8')) : 0;
  writeFileSync(statusPath, String(count + 1));
  if (mode === 'status-first-failure' && count === 0) {
    response.destroy();
    return;
  }
  const body = mode === 'status-forged-markers'
    ? { payload: 'forged-status-payload', classification: 'forged-status-classification', note: 'status-opaque-secret', PASSWORD: 'status-password-sentinel', PRIVATE_URL: 'https://status.private.example/internal' }
    : { value: { ready: mode !== 'invalid', state: 'success', build: { version: '16.12.8', productBundleIdentifier: 'com.facebook.WebDriverAgentRunner' }, os: { version: '18.6' } } };
  const oversized = mode === 'oversized' || (mode === 'ready-then-oversized' && count >= 1);
  const responseBody = mode === 'status-noisy-first-failure' ? '\\0'.repeat(64000) : JSON.stringify(body) + (oversized ? ' '.repeat(70000) : '');
  response.setHeader('content-type', 'application/json');
  response.end(responseBody);
});
const ownerState = join(root, 'state/owner.json');
if (mode === 'listener-pending-exit') {
  const pendingExitPoll = setInterval(() => {
    try {
      const owner = JSON.parse(readFileSync(ownerState, 'utf8'));
      if (owner.diagnostics?.lifecycle?.some(event => event.event === 'runner-candidate-pinned')) {
        clearInterval(pendingExitPoll);
        server.close(() => process.exit(45));
      }
    } catch {}
  }, 5);
}
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
let publicationPoll;
let stopping = false;
const stop = () => {
  if (stopping) return;
  stopping = true;
  if (mode === 'listener-pending-child-error' && role !== 'endpoint') writeFileSync(join(root, 'child-cleanup-requested'), String(Date.now()));
  if (keepAlive) clearInterval(keepAlive);
  if (publicationPoll) clearInterval(publicationPoll);
  server.closeAllConnections?.();
  if (!server.listening) process.exit(0);
  server.close(() => process.exit(0));
};
if (!holdsDescendant || role === 'endpoint') {
  let captureStarted = false;
  const publishCaptureRecord = (name, record, limit) => {
    const text = JSON.stringify(record);
    const path = join(root, name);
    if (Buffer.byteLength(text) > limit || existsSync(path)) throw new Error('WRITE_ERROR');
    writeFileSync(path + '.next', text, {flag: 'wx', mode: 0o600});
    renameSync(path + '.next', path);
  };
  const captureError = reason => {
    try { publishCaptureRecord('listener-capture-error.json', {schema: 1, reason}, 512); } catch {}
  };
  const capture = () => {
    let owner;
    try { owner = readListenerRecord(root, 'state/owner.json', 1048576); }
    catch { captureError('READ_ERROR'); return; }
    let fields;
    try { fields = listenerFailureFields(owner, mode, process.pid, process.ppid, true); }
    catch (error) { captureError(error.message === 'IDENTITY' ? 'IDENTITY' : 'MALFORMED'); return; }
    const receipt = {schema: 1, mode, trigger: 'SIGTERM', producerPid: process.pid, producerParentPid: process.ppid,
      spawnEventId: fields.spawnEventId, capturedAt: new Date().toISOString(), capturedBeforeOwnerEnded: true,
      firstFailure: fields.firstFailure, listenerValidation: fields.listenerValidation, failureEvent: fields.failureEvent};
    try { publishCaptureRecord('first-failure-observation.json', receipt, 65536); }
    catch { captureError('WRITE_ERROR'); }
  };
  const controlledCapture = async () => {
    const startedNs = process.hrtime.bigint();
    let startupDeadline;
    try { startupDeadline = readListenerRecord(root, 'state/deadline', 32); }
    catch { captureError('READ_ERROR'); return; }
    const budgetMs = Math.min(2000, startupDeadline - Date.now());
    if (!Number.isSafeInteger(startupDeadline) || !Number.isSafeInteger(budgetMs) || budgetMs <= 0) { captureError('BUDGET'); return; }
    const endNs = startedNs + BigInt(budgetMs) * 1000000n;
    let owner;
    try { owner = readListenerRecord(root, 'state/owner.json', 1048576); }
    catch { captureError('READ_ERROR'); return; }
    let fields;
    try { fields = listenerFailureFields(owner, mode, process.pid, process.ppid, true); }
    catch (error) { captureError(error.message === 'IDENTITY' ? 'IDENTITY' : 'MALFORMED'); return; }
    try {
      publishCaptureRecord('listener-capture-held.json', {schema: 1, mode, control: captureControl, producerPid: process.pid,
        producerParentPid: process.ppid, spawnEventId: fields.spawnEventId, startupDeadline, budgetMs, startedNs: String(startedNs), endNs: String(endNs)}, 512);
    } catch { captureError('WRITE_ERROR'); return; }
    while (true) {
      if (existsSync(join(root, 'listener-capture-abort')) || existsSync(join(root, 'state/stop'))) { captureError('CANCELLED'); return; }
      if (process.hrtime.bigint() >= endNs || Date.now() >= startupDeadline) { captureError('BUDGET'); return; }
      if (existsSync(join(root, 'listener-capture-decision.json'))) {
        let decision;
        try { decision = readListenerRecord(root, 'listener-capture-decision.json', 512); }
        catch { captureError('READ_ERROR'); return; }
        if (decision?.schema !== 1 || Object.keys(decision).sort().join(',') !== 'decision,schema' || !['CAPTURE', 'WITHHELD'].includes(decision?.decision)
          || decision.decision !== (captureControl === 'held-capture' ? 'CAPTURE' : 'WITHHELD')) { captureError('MALFORMED'); return; }
        const observedNs = process.hrtime.bigint();
        if (observedNs >= endNs || Date.now() >= startupDeadline) { captureError('BUDGET'); return; }
        try { publishCaptureRecord('listener-capture-result.json', {schema: 1, decision: decision.decision, observedNs: String(observedNs)}, 512); }
        catch { captureError('WRITE_ERROR'); return; }
        if (process.hrtime.bigint() >= endNs || Date.now() >= startupDeadline) { captureError('BUDGET'); return; }
        if (decision.decision === 'WITHHELD') captureError('WITHHELD');
        else capture();
        return;
      }
      await new Promise(resolve => setTimeout(resolve, 1));
    }
  };
  process.on('SIGTERM', () => {
    if (!listenerWitnessMode) { stop(); return; }
    if (captureStarted || stopping) return;
    captureStarted = true;
    if (role !== 'owner') { captureError('IDENTITY'); stop(); return; }
    if (captureControl) {
      if (mode !== 'listener-first-failure' || !['held-capture', 'withheld-capture'].includes(captureControl)) { captureError('MALFORMED'); stop(); return; }
      void controlledCapture().catch(() => captureError('MALFORMED')).finally(stop);
      return;
    }
    try { capture(); } finally { stop(); }
  });
  process.on('SIGINT', stop);
  process.on('exit', () => { remove(runnerPid); remove(xcodePid); });
  if (listenerWitnessMode) publishPid(xcodePid);
  if (mode === 'listener-concurrent-exit-before-close' && role === 'endpoint') {
    const managedChildPid = readFileSync(xcodePid, 'utf8').trim();
    const observerParentPid = process.ppid;
    const actualExitObserver = setInterval(() => {
      if (!existsSync(join(root, 'exit-child-requested')) || existsSync(join(root, 'child-exit-observed'))) return;
      const startedAtMs = Date.now();
      let probe;
      try {
        const remaining = Number(readFileSync(join(root, 'state/deadline'), 'utf8')) - startedAtMs;
        if (!Number.isFinite(remaining) || remaining <= 0) throw new Error('observer deadline');
        probe = spawnSync('/bin/ps', ['-p', managedChildPid, '-o', 'stat='], {
          encoding: 'utf8', timeout: Math.min(2000, remaining), maxBuffer: 4096,
        });
      } catch (error) { probe = {error}; }
      const observation = encodeXctestExitProbe(probe);
      if (observation.classification === 'live-observation') return;
      // No retry after an error/indeterminate result. Z is a terminal-shaped
      // receipt only: stable owned registration is still an unresolved gate.
      clearInterval(actualExitObserver);
      const receiptPath = join(root, 'child-exit-probe.json');
      const failurePath = join(root, 'child-exit-probe-error');
      try {
        const binding = {
          ownerPid: null, spawnId: null, spawnPid: null, spawnPhase: null, ownerRead: 'unavailable',
          exitRequested: existsSync(join(root, 'exit-child-requested')),
          exitIntent: existsSync(join(root, 'child-exit-intent')),
          exitContinue: existsSync(join(root, 'child-exit-continue')),
        };
        try {
          const owner = JSON.parse(readFileSync(ownerState, 'utf8'));
          const spawned = owner.diagnostics?.lifecycle?.find(event => event.event === 'child-spawned');
          const pid = value => Number.isSafeInteger(value) && value > 0 ? value : null;
          binding.ownerPid = pid(owner.pid);
          binding.spawnId = pid(spawned?.id);
          binding.spawnPid = pid(spawned?.detail?.pid);
          binding.spawnPhase = spawned?.detail?.phase === 'startup' ? 'startup' : null;
          binding.ownerRead = 'read';
        } catch { /* Keep only fixed unavailable binding fields, never raw errors. */ }
        const receipt = JSON.stringify({
          ...observation, observerPid: process.pid, observerParentPid, targetPid: Number(managedChildPid),
          argv: ['/bin/ps', '-p', managedChildPid, '-o', 'stat='], startedAtMs, endedAtMs: Date.now(), binding,
        }) + '\\n';
        writeFileSync(receiptPath + '.next', receipt);
        renameSync(receiptPath + '.next', receiptPath);
        const marker = observation.classification === 'terminal-observation' ? 'child-exit-observed' : 'child-exit-probe-error';
        writeFileSync(join(root, marker + '.next'), JSON.stringify({
          result: marker === 'child-exit-observed' ? 'terminal-observation' : 'ERROR',
          receipt: 'child-exit-probe.json', receiptSha256: createHash('sha256').update(receipt).digest('hex'),
          targetPid: Number(managedChildPid), witnessGuarantee: 'numeric-pid-only',
        }) + '\\n');
        renameSync(join(root, marker + '.next'), join(root, marker));
      } catch {
        // Failure publication does not turn into a death marker. If even this
        // owned write is denied, the existing shell deadline fails closed.
        try { writeFileSync(failurePath, '{"result":"ERROR","category":"receipt-write"}\\n'); }
        catch { process.stderr.write('XCTest observer ERROR: receipt-write\\n'); }
      }
    }, 1);
  }
}
if (!holdsDescendant || role === 'endpoint') {
  if (outputModes.includes(mode)) {
    keepAlive = setInterval(() => {}, 1000);
    emitFixtureOutput().then(() => {
      if (mode === 'stream-eof') clearInterval(keepAlive);
    }).catch(error => { process.stderr.write(String(error)); process.exit(1); });
  } else {
    if (mode === 'listener-uncoordinated-negative') {
      server.listen(Number(process.env.IOS_WDA_PORT), '127.0.0.1', () => {});
    } else if (mode === 'listener-first-failure' || mode === 'listener-second-failure' || mode === 'swap' || mode === 'pid-reuse') {
      server.listen(Number(process.env.IOS_WDA_PORT), '127.0.0.1', () => {
        const publish = () => {
          if (stopping || existsSync(join(root, 'stop'))) {
            stop();
            return;
          }
          if (!existsSync(publicationRelease) || existsSync(publicationAck)) return;
          publishPid(runnerPid);
          appendFileSync(publicationTrace, 'runner-pid-written\\n');
          writeFileSync(publicationAck, 'published\\n');
          appendFileSync(publicationTrace, 'publication-ack\\n');
          writeFileSync(endpointReady, 'published\\n');
          appendFileSync(publicationTrace, 'endpoint-ready\\n');
          if (publicationPoll) clearInterval(publicationPoll);
        };
        publicationPoll = setInterval(publish, 1);
        publish();
      });
    } else if (mode === 'listener-invalid-pid') {
      server.listen(Number(process.env.IOS_WDA_PORT), '127.0.0.1', () => {
        publishPid(runnerPid);
        appendFileSync(publicationTrace, 'runner-pid-written\\n');
        writeFileSync(publicationAck, 'published\\n');
        appendFileSync(publicationTrace, 'publication-ack\\n');
        writeFileSync(endpointReady, 'published\\n');
        appendFileSync(publicationTrace, 'endpoint-ready\\n');
      });
    } else if (receiptRaceModes.includes(mode)) {
      server.listen(Number(process.env.IOS_WDA_PORT), '127.0.0.1', () => {
        const publishReceiptRace = () => {
          if (stopping || existsSync(join(root, 'stop'))) {
            stop();
            return;
          }
          if (!existsSync(receiptPublicationRelease) || existsSync(receiptPublicationAck)) return;
          writeFileSync(runnerPid + '.next', String(process.pid));
          renameSync(runnerPid + '.next', runnerPid);
          appendFileSync(receiptPublicationTrace, 'runner-pid-written\\n');
          writeFileSync(receiptListenerReady + '.next', 'ready');
          renameSync(receiptListenerReady + '.next', receiptListenerReady);
          appendFileSync(receiptPublicationTrace, 'listener-ready\\n');
          writeFileSync(receiptPublicationAck, 'published\\n');
          appendFileSync(receiptPublicationTrace, 'publication-ack\\n');
          if (publicationPoll) clearInterval(publicationPoll);
        };
        publicationPoll = setInterval(publishReceiptRace, 1);
        publishReceiptRace();
      });
    } else if (coordinatedListenerModes.includes(mode)) {
      server.listen(Number(process.env.IOS_WDA_PORT), '127.0.0.1', () => {
        const publishCoordinatedListener = () => {
          if (stopping || existsSync(join(root, 'stop'))) {
            stop();
            return;
          }
          if (!existsSync(publicationRelease) || existsSync(publicationAck)) return;
          writeFileSync(runnerPid + '.next', String(process.pid));
          renameSync(runnerPid + '.next', runnerPid);
          appendFileSync(publicationTrace, 'runner-pid-written\\n');
          writeFileSync(publicationAck, 'published\\n');
          appendFileSync(publicationTrace, 'publication-ack\\n');
          writeFileSync(endpointReady, 'published\\n');
          appendFileSync(publicationTrace, 'endpoint-ready\\n');
          if (publicationPoll) clearInterval(publicationPoll);
        };
        publicationPoll = setInterval(publishCoordinatedListener, 1);
        publishCoordinatedListener();
      });
    } else {
      publishPid(runnerPid);
      server.listen(Number(process.env.IOS_WDA_PORT), '127.0.0.1');
    }
    if (mode === 'listener-pending-output-limit') {
      const pendingOutputPoll = setInterval(() => {
        try {
          const owner = JSON.parse(readFileSync(ownerState, 'utf8'));
          if (!owner.diagnostics?.lifecycle?.some(event => event.event === 'runner-candidate-pinned')) return;
          clearInterval(pendingOutputPoll);
          writeFilled(process.stdout, 104857601).catch(error => { process.stderr.write(String(error)); process.exit(1); });
        } catch {}
      }, 5);
    }
  }
} else {
  const endpoint = spawn(process.execPath, [process.argv[1], ...process.argv.slice(2)], {
    stdio: 'inherit',
    env: {...process.env, STARTUP_TEST_WDA_SERVER_ROLE: 'endpoint'},
  });
  if (!endpoint.pid) process.exit(47);
  writeFileSync(join(root, 'endpoint.pid'), String(endpoint.pid));
  const stopEndpoint = (): void => {
    try { process.kill(endpoint.pid!, 'SIGTERM'); } catch {}
    process.exit(0);
  };
  process.on('SIGTERM', stopEndpoint);
  process.on('SIGINT', stopEndpoint);
  let exitIntentWritten = false;
  let exitPoll;
  const observer = setInterval(() => {
    if (!existsSync(join(root, 'exit-child-requested'))) return;
    if (!exitIntentWritten) {
      writeFileSync(join(root, 'child-exit-intent'), String(Date.now()));
      exitIntentWritten = true;
      exitPoll = setInterval(() => {
        if (existsSync(join(root, 'child-exit-continue'))) {
          clearInterval(exitPoll);
          process.exit(46);
        }
      }, 1);
    }
  }, 1);
}
`;
const terminalGroupOwner = String.raw`const {writeSync} = require('node:fs');
let authority = true;
let termSent = false;
const revoke = () => { authority = false; process.exit(2); };
const report = value => { try { writeSync(4, value + '\n'); } catch { revoke(); } };
const remaining = Number(process.argv.at(-1)) - Date.now();
if (!Number.isSafeInteger(remaining) || remaining <= 0 || remaining > 40000) revoke();
process.on('SIGTERM', () => {});
process.on('uncaughtException', revoke);
process.on('unhandledRejection', revoke);
process.stdin.on('error', revoke);
process.stdin.on('end', revoke);
let received = 0;
process.stdin.on('data', bytes => {
  received += bytes.length;
  if (received > 2) revoke();
  for (const byte of bytes) {
    if (!authority) revoke();
    if (byte === 82) { authority = false; process.exit(0); }
    if (byte === 84 && !termSent) {
      termSent = true;
      try { process.kill(0, 'SIGTERM'); } catch { revoke(); }
      report('T');
      continue;
    }
    if (byte === 75) {
      authority = false;
      try { process.kill(0, 'SIGKILL'); } catch { revoke(); }
      process.exit(2);
    }
    revoke();
  }
});
setTimeout(revoke, remaining);
report('A');
`;
const terminalGroupBootstrap = String.raw`(
  trap '' TERM
  "$1" -e "$2" "$3" <&3 3<&- >/dev/null 2>/dev/null
  result=$?
  printf 'X%s\n' "$result" >&4
) </dev/null >/dev/null 2>/dev/null &
shift 3
exec 3<&- 4>&-
exec "$@"
`;
const terminalSpawn = (executable: string, args: string[], options: {cwd?: string; env: NodeJS.ProcessEnv; stdio: ['ignore', 'ignore' | 'pipe', 'ignore' | 'pipe']}, ownerDeadline: number): ChildProcess =>
  spawn('/bin/sh', ['-c', terminalGroupBootstrap, 'terminal-group', process.execPath, terminalGroupOwner, String(ownerDeadline), executable, ...args], {...options, detached: true, stdio: [...options.stdio, 'pipe', 'pipe']});
type TerminalResult = {code: number | null; signal: NodeJS.Signals | null; error: boolean};
type TerminalSettlement = {
  closed: Promise<TerminalResult>; joined: Promise<TerminalResult>; failed: Promise<Error>; error?: Error; result?: TerminalResult;
  emptyCheckPID?: number; ownerDeadline: number; groupComplete: boolean; groupUncertain: boolean; forced: boolean;
  control: Writable | null; ownerReady: boolean; ownerRetiring: boolean; ownerRetired: boolean;
  ownerEnded: boolean; ownerClosed: boolean; controlClosed: boolean;
  termRequested: boolean; termAcknowledged: boolean; fail: (error: Error) => void;
};
const terminalSettled = (owned: TerminalSettlement): boolean =>
  Boolean(owned.result && owned.ownerRetired && owned.ownerEnded && owned.ownerClosed && owned.controlClosed && !owned.error && !owned.groupUncertain);
const terminalRevokeGroup = (owned: TerminalSettlement): void => {
  if (owned.groupUncertain) return;
  owned.groupUncertain = true;
  owned.control?.destroy();
};
const terminalOwnerRequest = (owned: TerminalSettlement, command: 'T' | 'K' | 'R'): void => {
  if (owned.groupUncertain || owned.ownerRetiring || owned.forced) return;
  if (Date.now() >= owned.ownerDeadline) { owned.fail(new Error('terminal group owner deadline')); return; }
  if (!owned.control || owned.control.destroyed) { owned.fail(new Error('terminal group owner control unavailable')); return; }
  if (command === 'R') owned.ownerRetiring = true;
  if (command === 'K') owned.forced = true;
  if (command === 'T') {
    if (owned.termRequested) return;
    owned.termRequested = true;
  }
  try { owned.control.write(command, error => { if (error) owned.fail(error); }); }
  catch (error) { owned.fail(error as Error); }
};
const terminalGroupEmpty = (owned: TerminalSettlement): boolean => {
  if (owned.groupUncertain) return false;
  if (owned.groupComplete) return true;
  if (!terminalSettled(owned) || !owned.emptyCheckPID) return false;
  try { process.kill(-owned.emptyCheckPID, 0); }
  catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ESRCH') { owned.groupComplete = true; return true; }
    owned.fail(error as Error);
  }
  return false;
};
const terminalSignalGroup = (owned: TerminalSettlement, signal: 'SIGTERM' | 'SIGKILL'): void => {
  terminalOwnerRequest(owned, signal === 'SIGTERM' ? 'T' : 'K');
};
const terminalSettlement = (child: ChildProcess, ownerDeadline: number, onError?: () => void): TerminalSettlement => {
  let close!: (result: TerminalResult) => void;
  let join!: (result: TerminalResult) => void;
  let fail!: (error: Error) => void;
  const owned: TerminalSettlement = {
    closed: new Promise(resolve => { close = resolve; }), joined: new Promise(resolve => { join = resolve; }), failed: new Promise(resolve => { fail = resolve; }),
    emptyCheckPID: child.pid, ownerDeadline, groupComplete: false, groupUncertain: false, forced: false,
    control: child.stdio[3] as Writable | null, ownerReady: false, ownerRetiring: false, ownerRetired: false,
    ownerEnded: false, ownerClosed: false, controlClosed: false,
    termRequested: false, termAcknowledged: false,
    fail: error => {
      const first = !owned.error;
      owned.error ??= error;
      if (owned.result) owned.result.error = true;
      terminalRevokeGroup(owned);
      if (!first) return;
      fail(owned.error);
      onError?.();
    },
  };
  const complete = (): void => { if (terminalSettled(owned)) join(owned.result!); };
  const ownerOutput = child.stdio[4] as Readable | null;
  let output = '';
  let outputBytes = 0;
  ownerOutput?.on('data', chunk => {
    if (owned.groupUncertain) return;
    if (owned.ownerEnded || owned.ownerClosed) { owned.fail(new Error('terminal group owner output after EOF')); return; }
    outputBytes += chunk.length;
    if (outputBytes > 32) { owned.fail(new Error('terminal group owner output')); return; }
    output += chunk.toString();
    for (;;) {
      const end = output.indexOf('\n');
      if (end < 0) return;
      const line = output.slice(0, end);
      output = output.slice(end + 1);
      if (owned.ownerRetired) { owned.fail(new Error('terminal group owner output after retirement')); return; }
      if (line === 'A' && !owned.ownerReady) owned.ownerReady = true;
      else if (line === 'T' && owned.ownerReady && owned.termRequested && !owned.termAcknowledged) owned.termAcknowledged = true;
      else if (line === 'X0' && owned.ownerReady && owned.ownerRetiring && !owned.ownerRetired && (!owned.termRequested || owned.termAcknowledged)) owned.ownerRetired = true;
      else { owned.fail(new Error('terminal group owner failed')); return; }
    }
  });
  ownerOutput?.on('error', owned.fail);
  ownerOutput?.once('end', () => {
    owned.ownerEnded = true;
    if (!owned.ownerRetired || output !== '') { owned.fail(new Error('terminal group owner EOF unproved')); return; }
    owned.control?.destroy();
  });
  ownerOutput?.once('close', () => {
    owned.ownerClosed = true;
    if (!owned.ownerEnded || !owned.ownerRetired || output !== '') { owned.fail(new Error('terminal group owner close unproved')); return; }
    complete();
  });
  owned.control?.on('error', owned.fail);
  owned.control?.once('close', () => {
    owned.controlClosed = true;
    if (!owned.ownerRetiring) { owned.fail(new Error('terminal group owner control lost')); return; }
    complete();
  });
  let exited = false;
  const streams = [child.stdout, child.stderr].filter(stream => stream !== null);
  let pending = streams.length;
  const retire = (): void => { if (exited && pending === 0) terminalOwnerRequest(owned, 'R'); };
  for (const stream of streams) {
    stream.on('error', owned.fail);
    stream.once('close', () => { pending--; retire(); });
  }
  child.once('exit', () => { exited = true; retire(); });
  child.on('error', owned.fail);
  child.once('close', (code, signal) => {
    owned.result = {code, signal, error: Boolean(owned.error)};
    close(owned.result);
    complete();
  });
  if (!owned.control || !ownerOutput) owned.fail(new Error('terminal group owner pipes missing'));
  return owned;
};
const terminalBound = async <T>(promise: Promise<T>, deadline: number): Promise<T> => {
  assert.ok(Date.now() < deadline, 'terminal operation deadline');
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([promise, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error('terminal operation deadline')), deadline - Date.now());
    })]);
  } finally { if (timer) clearTimeout(timer); }
};
const terminalResult = async (owned: TerminalSettlement, deadline: number): Promise<TerminalResult> => {
  try {
    if (owned.error) throw owned.error;
    const result = await terminalBound(Promise.race([owned.joined, owned.failed]), deadline);
    if (owned.error) throw owned.error;
    if (result instanceof Error) throw result;
    return result;
  } catch (error) {
    owned.fail(error as Error);
    throw owned.error;
  }
};
const terminalOwnedClose = async (owned: TerminalSettlement[], deadline: number): Promise<void> => {
  try {
    await Promise.all(owned.map(child => terminalResult(child, deadline)));
    for (;;) {
      assert.ok(Date.now() < deadline, 'terminal cleanup deadline');
      const empty = owned.map(terminalGroupEmpty).every(Boolean);
      const error = owned.find(child => child.error)?.error;
      if (error) throw error;
      assert.equal(owned.some(child => child.groupUncertain), false, 'terminal group settlement unproved');
      if (empty) return;
      await new Promise(resolve => setTimeout(resolve, 1));
    }
  } catch (error) {
    for (const child of owned) child.fail(error as Error);
    throw error;
  }
};
type TerminalBuild = {root: string; binary: string; binaryHash: string; sources: string; env: NodeJS.ProcessEnv};
let terminalBuildPromise: Promise<TerminalBuild> | undefined;
const terminalBuild = (): Promise<TerminalBuild> => terminalBuildPromise ??= (async () => {
  assert.ok(process.platform === 'linux' || process.platform === 'darwin', 'terminal observer host');
  assert.ok(process.arch === 'arm64' || process.arch === 'x64', 'terminal observer architecture');
  const root = await mkdtemp(join(tmpdir(), 'herdr-terminal-build-'));
  const deadline = Date.now() + 15_000;
  let active: ChildProcess | undefined;
  let settled: TerminalSettlement | undefined;
  let stage = 'setup';
  try {
  const sourceRoot = join(root, 'source');
  await mkdir(sourceRoot);
  const sourceFiles = {'main.go': terminalMainGo, 'observer_linux.go': terminalLinuxGo, 'observer_darwin.go': terminalDarwinGo};
  const hashes: Record<string, string> = {};
  for (const [name, text] of Object.entries(sourceFiles)) {
    await writeFile(join(sourceRoot, name), text, {flag: 'wx', mode: 0o600});
    hashes[name] = terminalHash(await terminalBound(readFile(join(sourceRoot, name)), deadline));
    assert.equal(hashes[name], terminalHash(text), 'materialized observer source');
  }
  const sources = terminalHash(JSON.stringify(hashes));
  const selected = (process.env.PATH || '').split(':').filter(isAbsolute).map(path => join(path, 'go')).find(path => existsSync(path));
  assert.ok(selected, 'pinned Go prerequisite missing');
  const go = await realpath(selected);
  const goroot = dirname(dirname(go));
  const version = await readFile(join(goroot, 'VERSION'), 'utf8');
  assert.equal(version.split('\n')[0], 'go1.27.1', 'pinned Go VERSION prerequisite');
  assert.match(await readFile(join(import.meta.dirname, '../../../go.mod'), 'utf8'), /^go 1\.27\.1$/mu);
  const goos = process.platform;
  const goarch = process.arch === 'x64' ? 'amd64' : 'arm64';
  const env: NodeJS.ProcessEnv = {
    PATH: `${dirname(go)}:/usr/bin:/bin`, HOME: join(root, 'home'), TMPDIR: join(root, 'tmp'),
    GOCACHE: join(root, 'cache'), GOMODCACHE: join(root, 'modcache'), GOPATH: join(root, 'gopath'),
    XDG_CONFIG_HOME: join(root, 'config'), XDG_CACHE_HOME: join(root, 'xdg-cache'),
    GO111MODULE: 'off', GOENV: 'off', GOTOOLCHAIN: 'local', GOWORK: 'off', CGO_ENABLED: '0',
    GOPROXY: 'off', GOSUMDB: 'off', GOTELEMETRY: 'off', GOROOT: goroot, GOOS: goos, GOARCH: goarch,
    LANG: 'C', LC_ALL: 'C',
  };
  if (process.env.__CF_USER_TEXT_ENCODING !== undefined) env.__CF_USER_TEXT_ENCODING = process.env.__CF_USER_TEXT_ENCODING;
  for (const name of ['HOME', 'TMPDIR', 'GOCACHE', 'GOMODCACHE', 'GOPATH', 'XDG_CONFIG_HOME', 'XDG_CACHE_HOME']) await mkdir(env[name]!);
  const tools: Record<string, {sha256: string; mode: number}> = {};
  for (const path of [go, join(goroot, 'VERSION'), ...['compile', 'link', 'asm'].map(name => join(goroot, `pkg/tool/${goos}_${goarch}`, name))]) tools[path] = {sha256: terminalHash(await terminalBound(readFile(path), deadline)), mode: statSync(path).mode & 0o777};
  const binary = join(root, 'observer');
  const argv = ['build', '-trimpath', '-buildvcs=false', '-o', binary, '.'];
  terminalWrite(root, 'build-input.json', {schema: 1, go, goroot, tools, hashes, sources, argv, cwd: sourceRoot, env, deadline});
  let output = '';
  let oversized = false;
    stage = 'metadata';
    assert.ok(Date.now() < deadline, 'terminal build deadline');
    active = terminalSpawn(go, ['env', 'GOVERSION', 'GOROOT', 'GOOS', 'GOARCH'], {cwd: sourceRoot, env, stdio: ['ignore', 'pipe', 'ignore']}, deadline + 15_000);
    settled = terminalSettlement(active, deadline + 15_000);
    active.stdout!.on('data', chunk => { if (Buffer.byteLength(output) + chunk.length > 4096) oversized = true; else output += chunk.toString(); });
    const metadata = await terminalResult(settled, deadline);
    assert.deepEqual(metadata, {code: 0, signal: null, error: false});
    await terminalOwnedClose([settled], deadline);
    assert.equal(oversized, false);
    assert.deepEqual(output.trim().split('\n'), ['go1.27.1', goroot, goos, goarch]);
    stage = 'compile';
    assert.ok(Date.now() < deadline, 'terminal build deadline');
    active = terminalSpawn(go, argv, {cwd: sourceRoot, env, stdio: ['ignore', 'ignore', 'ignore']}, deadline + 15_000);
    settled = terminalSettlement(active, deadline + 15_000);
    const result = await terminalResult(settled, deadline);
    assert.deepEqual(result, {code: 0, signal: null, error: false});
    await terminalOwnedClose([settled], deadline);
    const binaryHash = terminalHash(await terminalBound(readFile(binary), deadline));
    assert.ok((statSync(binary).mode & 0o111) !== 0);
    terminalWrite(root, 'build-result.json', {result, binaryHash, mode: statSync(binary).mode & 0o777, sources, cleanup: null});
    return {root, binary, binaryHash, sources, env};
  } catch (error) {
    let cleanupFailed = false;
    if (settled) {
      terminalSignalGroup(settled, 'SIGKILL');
      try { await terminalOwnedClose([settled], Date.now() + 15_000); }
      catch { cleanupFailed = true; }
    }
    const directSettlement = settled?.result ?? null;
    const groupsStopped = Boolean(settled?.groupComplete && !settled.groupUncertain);
    try { terminalWrite(root, 'build-error.json', {result: 'ERROR', stage, directSettlement, groupsStopped, groupUncertain: settled?.groupUncertain ?? true, forced: settled?.forced ?? false, processError: Boolean(settled?.error), cleanupFailed, descendantsStopped: directSettlement && groupsStopped ? 'owned-groups-empty-and-stdio-close' : 'unproved', retry: false}); }
    catch { process.stderr.write('terminal-witness ERROR build receipt\n'); }
    throw error;
  }
})();

type TerminalRecord = { Schema: 1; Phase: string; Nonce: string; TargetPID: number; Previous: string; Data: Record<string, unknown> };
const terminalHash = (value: string | Buffer): string => createHash('sha256').update(value).digest('hex');
const terminalPid = (value: unknown): number => {
  assert.ok(typeof value === 'number' && Number.isSafeInteger(value) && value > 0 && value <= 2147483647, 'terminal PID');
  return value;
};
const terminalKeys = (value: Record<string, unknown>, keys: string[]): void => {
  assert.deepEqual(Object.keys(value).sort(), [...keys].sort(), 'terminal record fields');
};
const terminalParse = (bytes: Buffer, phase: string, nonce: string, pid: number, previous: string): TerminalRecord => {
  assert.ok(bytes.length > 0 && bytes.length <= 8192, 'terminal record size');
  const value = JSON.parse(bytes.toString('utf8')) as TerminalRecord;
  assert.equal(bytes.toString('utf8'), JSON.stringify(value) + '\n', 'terminal canonical record');
  terminalKeys(value, ['Schema', 'Phase', 'Nonce', 'TargetPID', 'Previous', 'Data']);
  assert.equal(value.Schema, 1);
  assert.equal(value.Phase, phase);
  assert.equal(value.Nonce, nonce);
  assert.match(nonce, /^[a-f0-9]{32}$/u);
  assert.equal(terminalPid(value.TargetPID), pid);
  assert.equal(value.Previous, previous);
  assert.ok(value.Data && typeof value.Data === 'object' && !Array.isArray(value.Data));
  return value;
};
const terminalRead = (root: string, name: string): Buffer => {
  const path = join(root, name);
  const stat = statSync(path);
  assert.ok(stat.isFile() && stat.size > 0 && stat.size <= 8192, 'terminal record size');
  const bytes = readFileSync(path);
  assert.equal(bytes.length, stat.size, 'terminal record changed');
  return bytes;
};
const terminalWrite = (root: string, name: string, value: unknown): string => {
  const path = join(root, name);
  assert.equal(existsSync(path), false, 'terminal phase already published');
  const bytes = Buffer.from(JSON.stringify(value) + '\n');
  assert.ok(bytes.length <= 8192);
  writeFileSync(path + '.next', bytes, {flag: 'wx', mode: 0o600});
  renameSync(path + '.next', path);
  return terminalHash(bytes);
};
const terminalCheck = (root: string, deadline: number): void => {
  for (const name of ['cancel', 'observer-error.json', 'target-error', 'child-exit-probe-error', 'state/stop']) {
    assert.equal(existsSync(join(root, name)), false, `terminal failure: ${name}`);
  }
  assert.ok(Date.now() < deadline, 'terminal deadline');
};
const terminalWait = async (root: string, name: string, deadline: number, forbidden: string[] = []): Promise<Buffer> => {
  for (;;) {
    terminalCheck(root, deadline);
    for (const early of forbidden) assert.equal(existsSync(join(root, early)), false, `early terminal phase: ${early}`);
    if (existsSync(join(root, name))) {
      const bytes = terminalRead(root, name);
      terminalCheck(root, deadline);
      return bytes;
    }
    await new Promise(resolve => setTimeout(resolve, 1));
  }
};
const terminalValidateKernel = (armed: TerminalRecord, event?: TerminalRecord): void => {
  const a = armed.Data;
  terminalPid(a.observerPID);
  assert.ok(Number.isInteger(a.queue) && Number(a.queue) >= 0);
  assert.equal(a.error, null);
  if (a.method === 'linux-pidfd-epoll') {
    terminalKeys(a, ['method', 'observerPID', 'queue', 'fd', 'pad', 'requested', 'result', 'error']);
    assert.ok(Number.isInteger(a.fd) && Number(a.fd) >= 0 && Number(a.fd) <= 2147483647);
    assert.equal(a.pad, 0);
    assert.equal(a.requested, 1);
    assert.equal(a.result, 0);
    if (!event) return;
    const e = event.Data;
    terminalKeys(e, ['method', 'observerPID', 'queue', 'fd', 'pad', 'events', 'count', 'error']);
    for (const key of ['method', 'observerPID', 'queue', 'fd', 'pad']) assert.equal(e[key], a[key]);
    assert.ok(e.events === 1 || e.events === 17);
    assert.equal(e.count, 1);
    assert.equal(e.error, null);
    return;
  }
  assert.equal(a.method, 'darwin-kqueue');
  terminalKeys(a, ['method', 'observerPID', 'queue', 'ident', 'filter', 'flags', 'fflags', 'data', 'requestedFlags', 'requestedFflags', 'count', 'error']);
  assert.equal(a.ident, String(armed.TargetPID));
  assert.equal(a.filter, -5);
  assert.ok(Number.isInteger(a.flags) && Number(a.flags) >= 0 && Number(a.flags) <= 65535 && (Number(a.flags) & 0x4000) !== 0);
  assert.ok(Number.isInteger(a.fflags) && Number(a.fflags) >= 0 && Number(a.fflags) <= 0xffffffff);
  assert.equal(a.data, '0');
  assert.equal(a.requestedFlags, 0x51);
  assert.equal(a.requestedFflags, 0x80000000);
  assert.equal(a.count, 1);
  if (!event) return;
  const e = event.Data;
  terminalKeys(e, ['method', 'observerPID', 'queue', 'ident', 'filter', 'flags', 'fflags', 'data', 'count', 'error']);
  for (const key of ['method', 'observerPID', 'queue', 'ident', 'filter']) assert.equal(e[key], a[key]);
  assert.ok(Number.isInteger(e.flags) && Number(e.flags) >= 0 && Number(e.flags) <= 65535 && (Number(e.flags) & 0x4000) === 0);
  assert.equal(e.fflags, 0x80000000);
  assert.equal(typeof e.data, 'string');
  assert.match(String(e.data), /^(?:0|-?[1-9]\d*)$/u);
  assert.ok(BigInt(String(e.data)) >= -(1n << 63n) && BigInt(String(e.data)) < (1n << 63n));
  assert.equal(e.count, 1);
  assert.equal(e.error, null);
};

type TerminalSession = {
  root: string; nonce: string; pid: number; parentPID: number; endpointPID: number; deadline: number;
  observer: ChildProcess; settled: TerminalSettlement; build: TerminalBuild;
  armed?: TerminalRecord; armedHash?: string; liveHash?: string;
};
const terminalRecord = (phase: string, nonce: string, pid: number, previous: string, Data: Record<string, unknown>): TerminalRecord => ({Schema: 1, Phase: phase, Nonce: nonce, TargetPID: pid, Previous: previous, Data});
const terminalPublicSpawn = (root: string): {pid: number; id: number; event: string; phase: string} | undefined => {
  const path = join(root, 'state/owner.json');
  if (!existsSync(path)) return;
  assert.ok(statSync(path).size <= 1_048_576);
  const owner = JSON.parse(readFileSync(path, 'utf8'));
  const events = owner.diagnostics?.lifecycle?.filter((event: {event: string}) => event.event === 'child-spawned') || [];
  assert.ok(events.length <= 1, 'duplicate child spawn');
  if (events.length === 0) return;
  const event = events[0];
  const pid = terminalPid(owner.pid);
  assert.equal(event.detail.pid, pid);
  assert.equal(event.detail.phase, 'startup');
  return {pid, id: terminalPid(event.id), event: 'child-spawned', phase: 'startup'};
};
const terminalStart = async (root: string, nonce: string, parentPID: number, deadline: number, build: TerminalBuild, directPID?: number, fault = false): Promise<TerminalSession> => {
  assert.ok(!fault || directPID, 'registration fault is control-only');
  const heldBytes = await terminalWait(root, 'held.json', deadline, ['armed.json', 'terminal.json', 'complete.json']);
  let binding: {pid: number; id: number | null; event: string; phase: string} | undefined;
  if (directPID) binding = {pid: directPID, id: null, event: 'fixture-parent-spawn', phase: 'fixture'};
  while (!binding) {
    terminalCheck(root, deadline);
    binding = terminalPublicSpawn(root);
    if (!binding) await new Promise(resolve => setTimeout(resolve, 1));
  }
  const pid = terminalPid(binding.pid);
  assert.notEqual(pid, parentPID);
  const epBytes = terminalRead(root, 'endpoint.json');
  const ep = terminalParse(epBytes, 'endpoint', nonce, pid, '');
  terminalKeys(ep.Data, ['pid', 'parentPID']);
  const endpointPID = terminalPid(ep.Data.pid);
  assert.notEqual(endpointPID, pid);
  assert.notEqual(endpointPID, parentPID);
  assert.equal(ep.Data.parentPID, pid);
  const heldHash = terminalHash(heldBytes);
  const held = terminalParse(heldBytes, 'held', nonce, pid, terminalHash(epBytes));
  terminalKeys(held.Data, ['pid', 'parentPID', 'endpointPID']);
  assert.deepEqual(held.Data, {pid, parentPID, endpointPID});
  assert.equal(Number(readFileSync(join(root, 'xcode.pid'), 'utf8')), pid);
  assert.equal(Number(readFileSync(join(root, 'endpoint.pid'), 'utf8')), endpointPID);
  const bindingHash = terminalWrite(root, 'binding.json', terminalRecord('binding', nonce, pid, heldHash, {parentPID, endpointPID, spawn: binding, binary: build.binaryHash, sources: build.sources}));
  terminalWrite(root, 'observer-input.json', {Root: root, Nonce: nonce, TargetPID: pid, Binding: bindingHash, Deadline: deadline, Fault: fault, Binary: build.binaryHash, Sources: build.sources});
  assert.equal(terminalHash(await readFile(build.binary)), build.binaryHash, 'observer pre-launch binary');
  terminalCheck(root, deadline);
  const observer = terminalSpawn(build.binary, [join(root, 'observer-input.json')], {cwd: root, env: build.env, stdio: ['ignore', 'ignore', 'pipe']}, deadline + 30_000);
  observer.stderr!.on('data', () => {
    process.stderr.write('terminal-witness ERROR helper-stderr\n');
    terminalFailure(root);
  });
  const settled = terminalSettlement(observer, deadline + 30_000, () => terminalFailure(root));
  if (!fault) void settled.closed.then(result => {
    if (result.error || result.code !== 0 || result.signal !== null || !existsSync(join(root, 'complete.json'))) terminalFailure(root);
  });
  const session = {root, nonce, pid, parentPID, endpointPID, deadline, observer, settled, build};
  return session;
};
const terminalArm = async (session: TerminalSession): Promise<void> => {
  const {root, nonce, pid, parentPID, endpointPID, deadline, observer} = session;
  const bindingHash = terminalHash(terminalRead(root, 'binding.json'));
  const armedBytes = await terminalWait(root, 'armed.json', deadline, ['live.json', 'ps-blocked.json', 'terminal.json', 'complete.json']);
  const armedHash = terminalHash(armedBytes);
  const armed = terminalParse(armedBytes, 'armed', nonce, pid, bindingHash);
  terminalValidateKernel(armed);
  assert.equal(armed.Data.observerPID, terminalPid(observer.pid));
  assert.notEqual(observer.pid, pid);
  assert.notEqual(observer.pid, endpointPID);
  session.armed = armed;
  session.armedHash = armedHash;
  const heldHash = terminalHash(terminalRead(root, 'held.json'));
  const challengeHash = terminalWrite(root, 'challenge.json', terminalRecord('challenge', nonce, pid, armedHash, {held: heldHash}));
  const liveBytes = await terminalWait(root, 'live.json', deadline, ['ps-blocked.json', 'terminal.json', 'complete.json']);
  const live = terminalParse(liveBytes, 'live', nonce, pid, challengeHash);
  terminalKeys(live.Data, ['pid', 'parentPID', 'endpointPID', 'registration']);
  assert.deepEqual(live.Data, {pid, parentPID, endpointPID, registration: armedHash});
  session.liveHash = terminalHash(liveBytes);
  for (const [name, value] of Object.entries({'witness-target': pid, 'witness-parent': parentPID, 'witness-live-hash': session.liveHash})) writeFileSync(join(root, name), String(value), {flag: 'wx', mode: 0o600});
};
const terminalEndpoint = async (session: TerminalSession, port: number, deadline: number): Promise<void> => {
  terminalCheck(session.root, deadline);
  const response = await fetch(`http://127.0.0.1:${port}/witness`, {redirect: 'error', signal: AbortSignal.timeout(Math.max(1, deadline - Date.now()))});
  assert.equal(response.status, 200);
  const reader = response.body?.getReader();
  assert.ok(reader);
  let bytes = Buffer.alloc(0);
  try {
    for (;;) {
      const part = await terminalBound(reader.read(), deadline);
      if (part.done) break;
      assert.ok(bytes.length + part.value.length <= 1024, 'endpoint response size');
      bytes = Buffer.concat([bytes, part.value]);
    }
  } finally { void reader.cancel().catch(() => {}); }
  assert.deepEqual(JSON.parse(bytes.toString()), {nonce: session.nonce, pid: session.endpointPID, parentPID: session.pid});
  terminalCheck(session.root, deadline);
};
const terminalRelease = async (session: TerminalSession, port: number, production: boolean): Promise<void> => {
  const {root, nonce, pid, parentPID, deadline, armed, armedHash, liveHash} = session;
  assert.ok(armed && armedHash && liveHash);
  const blockedBytes = await terminalWait(root, 'ps-blocked.json', deadline, ['continue.json', 'terminal.json', 'complete.json']);
  const blocked = terminalParse(blockedBytes, 'ps-blocked', nonce, pid, liveHash);
  terminalKeys(blocked.Data, ['shellPID', 'parentPID', 'startedAt']);
  terminalPid(blocked.Data.shellPID);
  assert.equal(blocked.Data.parentPID, parentPID);
  assert.ok(Number.isSafeInteger(blocked.Data.startedAt) && Number(blocked.Data.startedAt) <= Date.now());
  const commandDeadline = Math.min(deadline, Number(blocked.Data.startedAt) + 2000);
  terminalCheck(root, commandDeadline);
  const intentBytes = await terminalWait(root, 'intent.json', commandDeadline, ['continue.json', 'terminal.json', 'complete.json']);
  const intent = terminalParse(intentBytes, 'intent', nonce, pid, terminalHash(blockedBytes));
  assert.deepEqual(intent.Data, {pid, code: 46});
  const continueHash = terminalWrite(root, 'continue.json', terminalRecord('continue', nonce, pid, terminalHash(intentBytes), {code: 46}));
  const eventBytes = await terminalWait(root, 'terminal.json', commandDeadline);
  const event = terminalParse(eventBytes, 'terminal', nonce, pid, continueHash);
  terminalValidateKernel(armed, event);
  const completeBytes = await terminalWait(root, 'complete.json', commandDeadline);
  const complete = terminalParse(completeBytes, 'complete', nonce, pid, terminalHash(eventBytes));
  assert.deepEqual(complete.Data, {result: 'SUCCESS', observerPID: session.observer.pid, cleanup: null, binary: session.build.binaryHash, sources: session.build.sources});
  const result = await terminalResult(session.settled, commandDeadline);
  assert.deepEqual(result, {code: 0, signal: null, error: false});
  await terminalOwnedClose([session.settled], commandDeadline);
  terminalWrite(root, 'observer-result.json', result);
  await terminalEndpoint(session, port, commandDeadline);
  assert.equal(terminalHash(terminalRead(root, 'armed.json')), armedHash);
  assert.equal(terminalHash(terminalRead(root, 'live.json')), liveHash);
  assert.equal(terminalHash(terminalRead(root, 'ps-blocked.json')), terminalHash(blockedBytes));
  assert.equal(terminalHash(terminalRead(root, 'continue.json')), continueHash);
  if (production) {
    const owner = JSON.parse(readFileSync(join(root, 'state/owner.json'), 'utf8'));
    assert.equal(owner.pid, pid);
    assert.equal(owner.endedAt, undefined);
    assert.equal(owner.firstFailure, undefined);
    assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => ['child-exit', 'child-close', 'cleanup-enter', 'owner-ready-cleared', 'owner-ended'].includes(event.event)), false);
    assert.equal(terminalPublicSpawn(root)?.pid, pid);
  }
  terminalCheck(root, commandDeadline);
  terminalWrite(root, 'child-exit-observed', terminalRecord('release', nonce, pid, terminalHash(completeBytes), {result: 'SUCCESS', endpointPID: session.endpointPID, observerResult: terminalHash(terminalRead(root, 'observer-result.json'))}));
};
const terminalFailure = (root: string): void => {
  for (const name of ['child-exit-probe-error', 'cancel']) {
    if (existsSync(join(root, name))) continue;
    try { terminalWrite(root, name, {result: 'ERROR', stage: 'fixture', category: 'fixture-failure'}); }
    catch { process.stderr.write('terminal-witness ERROR publication\n'); }
  }
};
const terminalTeardown = async (root: string, session: TerminalSession | undefined, child: TerminalSettlement, failed: boolean, shell?: TerminalSettlement): Promise<void> => {
  if (failed) terminalFailure(root);
  const deadline = Date.now() + 15_000;
  const owned = [child, ...(session ? [session.settled] : []), ...(shell ? [shell] : [])];
  let publicationFailed = false;
  for (const name of ['endpoint-stop', 'state/stop']) {
    try { writeFileSync(join(root, name), 'fixture finalization'); }
    catch { publicationFailed = true; terminalFailure(root); }
  }
  for (const process of owned) terminalSignalGroup(process, 'SIGTERM');
  let cleanupFailed = false;
  let forcedCleanupFailed = false;
  try { await terminalOwnedClose(owned, Math.min(deadline, Date.now() + 2000)); }
  catch {
    cleanupFailed = true;
    terminalFailure(root);
    for (const process of owned) terminalSignalGroup(process, 'SIGKILL');
    try { await terminalOwnedClose(owned, deadline); }
    catch { forcedCleanupFailed = true; }
  }
  const settled = owned.every(terminalSettled);
  const groupsStopped = owned.every(process => process.groupComplete && !process.groupUncertain);
  const groupUncertain = owned.some(process => process.groupUncertain);
  const processError = owned.some(process => Boolean(process.error));
  const forced = owned.some(process => process.forced);
  const endpointStopped = existsSync(join(root, 'endpoint-stopped'));
  const targetError = existsSync(join(root, 'target-error'));
  terminalWrite(root, 'teardown.json', {settled, groupsStopped, groupUncertain, processError, cleanupFailed, cleanup: forcedCleanupFailed ? 'ERROR' : null, endpointStopped, publicationFailed, targetError, forced, capabilityProbeReap: 'stdlib-normal-path-only', descendantEvidence: settled && groupsStopped ? 'owned-groups-empty-and-stdio-close' : 'unproved'});
  assert.ok(settled && groupsStopped && endpointStopped && !processError && !publicationFailed && !targetError && !cleanupFailed && !forced, 'terminal owned teardown incomplete');
};
const terminalExportSafe = (bytes: Buffer, root: string, buildRoot: string): boolean => {
  if (bytes.length === 0 || bytes.length > 8192) return false;
  const keys = new Set('Schema Phase Nonce TargetPID Previous Data pid parentPID endpointPID spawn id event events method phase binary sources observerPID queue fd pad requested result error ident filter flags fflags data requestedFlags requestedFflags count held registration shellPID startedAt code cleanup observerResult signal errno errnoKnown callResult callResultKind category stage release settled groupsStopped groupUncertain processError cleanupFailed endpointStopped publicationFailed targetError forced capabilityProbeReap descendantEvidence dispatcher server buildRoot binding closeBeforeProof operation heldBeforeCleanup shellCode Root Binding Deadline Fault Binary Sources'.split(' '));
  const literals = new Set(['', 'endpoint', 'held', 'binding', 'armed', 'challenge', 'live', 'ps-blocked', 'intent', 'continue', 'terminal', 'complete', 'release', 'failure-held', 'SUCCESS', 'ERROR', 'fixture-failure', 'fixture-parent-spawn', 'child-spawned', 'startup', 'fixture', 'linux-pidfd-epoll', 'darwin-kqueue', 'registration', 'input', 'handle', 'queue', 'wait', 'cleanup', 'completion', 'EBADF', 'EPERM', 'EACCES', 'ESRCH', 'EINTR', 'syscall', 'handle-unavailable', 'process-done-api', 'protocol-or-deadline', 'unavailable', 'error-only', 'number', 'stdlib-normal-path-only', 'owned-groups-empty-and-stdio-close', 'unproved', 'SIGTERM', 'SIGKILL']);
  const safe = (value: unknown, depth = 0): boolean => {
    if (depth > 8) return false;
    if (value === null || typeof value === 'boolean') return true;
    if (typeof value === 'number') return Number.isSafeInteger(value);
    if (typeof value === 'string') return literals.has(value) || /^[a-f0-9]{32}(?:[a-f0-9]{32})?$/u.test(value) || /^-?\d{1,20}$/u.test(value) || value === root || value === buildRoot;
    if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
    return Object.entries(value).every(([key, item]) => keys.has(key) && safe(item, depth + 1));
  };
  try { return safe(JSON.parse(bytes.toString('utf8'))); } catch { return false; }
};
const terminalExport = async (root: string, mode: string, build: TerminalBuild): Promise<void> => {
  const base = process.env.XCTEST_DIAGNOSTIC_ROOT;
  if (!base) return;
  await mkdir(base, {recursive: true});
  const destination = await mkdtemp(join(base, `${mode}-`));
  for (const name of ['endpoint.json', 'held.json', 'binding.json', 'observer-input.json', 'armed.json', 'challenge.json', 'live.json', 'ps-blocked.json', 'intent.json', 'continue.json', 'terminal.json', 'complete.json', 'observer-result.json', 'child-exit-observed', 'observer-error.json', 'child-exit-probe-error', 'cancel', 'teardown.json', 'source-binding.json', 'callback.json', 'control-result.json', 'failure-held.json']) {
    if (!existsSync(join(root, name))) continue;
    const bytes = terminalRead(root, name);
    const valid = terminalExportSafe(bytes, root, build.root);
    await writeFile(join(destination, valid ? name : name + '.rejected.json'), valid ? bytes : '{"result":"ERROR","category":"unsafe-receipt"}\n', {flag: 'wx', mode: 0o600});
    assert.equal(valid, true, 'terminal export rejected a non-protocol record');
  }
};

const terminalShellWait = String.raw`
terminal_check() {
  for failure in cancel observer-error.json target-error child-exit-probe-error state/stop; do
    [ ! -e "$root/$failure" ] || return 2
  done
  terminal_deadline=$(cat "$root/state/deadline") || return 2
  terminal_now=$(date +%s%3N)
  case "$terminal_now" in ''|*[!0-9]*) terminal_now=$(date +%s)000 ;; esac
  case "$terminal_deadline" in ''|*[!0-9]*) return 2 ;; esac
  [ "$terminal_now" -lt "$terminal_deadline" ] || return 2
}
terminal_wait() {
  terminal_path=$1
  terminal_check || return 2
  while [ ! -e "$terminal_path" ]; do
    terminal_check || return 2
    /bin/sleep 0.01
  done
  terminal_check || return 2
}
`;
const terminalShim = (): string => {
  let source = shim;
  const replaceOnce = (marker: string, replacement: string): void => {
    assert.equal(source.split(marker).length, 2, 'terminal source marker must be unique');
    source = source.replace(marker, () => replacement);
  };
  replaceOnce('has_arg() {', terminalShellWait + '\nhas_arg() {');
  replaceOnce('    while ! [ -e "$root/runner.pid" ] && ! [ -e "$root/stop" ] && ! grep -q \'"firstFailure"\' "$root/state/owner.json"; do /bin/sleep 0.01; done',
    '    terminal_wait "$root/runner.pid" || exit 2');
  replaceOnce('      if [ "$pending_wda_count" -eq 3 ] && [ "$mode" != "listener-pending-exit" ] && [ "$mode" != "listener-pending-child-error" ] && ! [ -e "$root/pending-candidate-continue" ] && ! [ -e "$root/stop" ]; then',
    '      if [ "$pending_wda_count" -eq 3 ]; then\n        terminal_check || exit 2');
  replaceOnce('        while ! [ -e "$root/pending-candidate-continue" ] && ! [ -e "$root/stop" ]; do /bin/sleep 0.01; done',
    '        terminal_wait "$root/pending-candidate-continue" || exit 2');
  replaceOnce('      touch "$root/exit-child-requested"\n      if ! wait_for_fixture_file "$root/child-exit-observed" "$root/child-exit-probe-error"; then', String.raw`      terminal_check || exit 2
      [ "$pid" = "$(cat "$root/witness-target")" ] || exit 2
      [ "$PPID" = "$(cat "$root/witness-parent")" ] || exit 2
      witness_nonce=$(cat "$root/witness-nonce") || exit 2
      witness_live=$(cat "$root/witness-live-hash") || exit 2
      printf '{"Schema":1,"Phase":"ps-blocked","Nonce":"%s","TargetPID":%s,"Previous":"%s","Data":{"shellPID":%s,"parentPID":%s,"startedAt":%s}}\n' "$witness_nonce" "$pid" "$witness_live" "$$" "$PPID" "$terminal_now" > "$root/ps-blocked.json.next" || exit 2
      mv "$root/ps-blocked.json.next" "$root/ps-blocked.json" || exit 2
      touch "$root/exit-child-requested" || exit 2
      if ! terminal_wait "$root/child-exit-observed"; then`);
  replaceOnce('    kill -TERM "$(cat "$root/endpoint.pid")" 2>/dev/null || true', '    touch "$root/endpoint-stop"');
  assert.notEqual(source, shim);
  return source;
};

const terminalServer = String.raw`import {createServer} from 'node:http';
import {spawn} from 'node:child_process';
import {createHash} from 'node:crypto';
import {existsSync,statSync,readFileSync,writeFileSync,renameSync,unlinkSync,appendFileSync} from 'node:fs';
import {join} from 'node:path';
const root=process.env.STARTUP_TEST_ROOT;
const nonce=readFileSync(join(root,'witness-nonce'),'utf8');
const endpointRole=process.env.STARTUP_TEST_WDA_SERVER_ROLE==='endpoint';
const parentPID=process.ppid;
const targetPID=endpointRole?parentPID:process.pid;
const hash=b=>createHash('sha256').update(b).digest('hex');
const read=name=>{const path=join(root,name);const size=statSync(path).size;if(size<=0||size>8192)throw Error('size');const b=readFileSync(path);if(b.length!==size)throw Error('changed');return b;};
const get=(phase,previous)=>{const b=read(phase+'.json');const r=JSON.parse(b);if(Object.keys(r).sort().join(',')!=='Data,Nonce,Phase,Previous,Schema,TargetPID'||r.Schema!==1||r.Phase!==phase||r.Nonce!==nonce||r.TargetPID!==targetPID||r.Previous!==previous)throw Error('chain');return {r,hash:hash(b)};};
const put=(phase,previous,Data)=>{const path=join(root,phase+'.json');if(existsSync(path))throw Error('duplicate');const b=JSON.stringify({Schema:1,Phase:phase,Nonce:nonce,TargetPID:targetPID,Previous:previous,Data})+'\n';writeFileSync(path+'.next',b,{flag:'wx',mode:0o600});renameSync(path+'.next',path);return hash(b);};
const cancelled=()=>['cancel','child-exit-probe-error','state/stop'].some(n=>existsSync(join(root,n)));
const deadline=Number(readFileSync(join(root,'state/deadline'),'utf8'));
let endpoint;
let server;
let stopping=false;
const stop=()=>{if(stopping)return;stopping=true;if(endpoint&&endpoint.exitCode===null)endpoint.kill('SIGTERM');if(server){server.closeAllConnections();server.close(()=>{for(const name of ['runner.pid','xcode.pid']){try{unlinkSync(join(root,name));}catch{}}writeFileSync(join(root,'endpoint-stopped'),'normal');process.exit(0);});}else process.exit(48);};
process.on('SIGTERM',stop);process.on('SIGINT',stop);
const fail=()=>{try{writeFileSync(join(root,'target-error'),'ERROR',{flag:'wx'});}catch{}stop();};
if(endpointRole){
 server=createServer((request,response)=>{if(request.url==='/witness'){response.setHeader('content-type','application/json');response.end(JSON.stringify({nonce,pid:process.pid,parentPID}));return;}const p=join(root,'status-queries');const count=existsSync(p)?Number(readFileSync(p,'utf8')):0;writeFileSync(p,String(count+1));response.setHeader('content-type','application/json');response.end(JSON.stringify({value:{ready:true,state:'success',build:{version:'16.12.8',productBundleIdentifier:'com.facebook.WebDriverAgentRunner'},os:{version:'18.6'}}}));});
 server.listen(Number(process.env.IOS_WDA_PORT),'127.0.0.1',()=>{try{put('endpoint','',{pid:process.pid,parentPID});writeFileSync(join(root,'runner.pid'),String(process.pid));}catch{fail();}});
 server.on('error',fail);
 setInterval(()=>{if(cancelled()||existsSync(join(root,'endpoint-stop')))stop();else if(Date.now()>=deadline)fail();},1);
}else{
 appendFileSync(join(root,'launches'),JSON.stringify(process.argv.slice(3))+'\n');
 writeFileSync(join(root,'xcode.pid'),String(process.pid));
 endpoint=spawn(process.execPath,[process.argv[1],...process.argv.slice(2)],{stdio:'inherit',env:{...process.env,STARTUP_TEST_WDA_SERVER_ROLE:'endpoint'}});
 if(!endpoint.pid)fail();
 writeFileSync(join(root,'endpoint.pid'),String(endpoint.pid));
 endpoint.on('error',fail);
 endpoint.on('exit',()=>{if(!stopping)fail();});
 let held,live,intent,failureHeld;
 setInterval(()=>{try{
  if(cancelled()){stop();return;}
  if(Date.now()>=deadline){fail();return;}
  if(existsSync(join(root,'observer-error.json'))){if(!failureHeld){failureHeld=put('failure-held',hash(read('observer-error.json')),{pid:process.pid,parentPID,endpointPID:endpoint.pid});}return;}
  if(!held&&existsSync(join(root,'endpoint.json'))){const ep=get('endpoint','');if(ep.r.Data.pid!==endpoint.pid||ep.r.Data.parentPID!==process.pid)throw Error('endpoint-role');held=put('held',ep.hash,{pid:process.pid,parentPID,endpointPID:endpoint.pid});}
  if(!live&&existsSync(join(root,'challenge.json'))){if(!held)throw Error('early-challenge');const binding=get('binding',held);const armed=get('armed',binding.hash);const challenge=get('challenge',armed.hash);if(challenge.r.Data.held!==held)throw Error('challenge');live=put('live',challenge.hash,{pid:process.pid,parentPID,endpointPID:endpoint.pid,registration:armed.hash});}
  if(!intent&&existsSync(join(root,'exit-child-requested'))){if(!live)throw Error('early-exit');const blocked=get('ps-blocked',live);intent=put('intent',blocked.hash,{pid:process.pid,code:46});writeFileSync(join(root,'child-exit-intent'),'held');}
  if(existsSync(join(root,'continue.json'))){if(!intent)throw Error('early-continue');const continuation=get('continue',intent);if(continuation.r.Data.code!==46)throw Error('continue-code');process.exit(46);}
 }catch{fail();}},1);
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
    buildVersion: '16.12.8',
    productBundleIdentifier: 'com.facebook.WebDriverAgentRunner',
    osVersion: '18.6',
    payload: 'suppressed',
    diagnostic: 'suppressed',
    diagnosticBytes: status.bytes,
    diagnosticTruncated: false,
    bodyBytes: status.bytes,
  });
};

const initialRaceModes = ['listener-initial-race', 'listener-initial-race-birth', 'listener-initial-race-executable', 'listener-initial-race-hash', 'listener-initial-race-product', 'listener-initial-race-receipt', 'listener-initial-race-receipt-command-error', 'listener-initial-race-receipt-hash-error', 'listener-initial-race-command-error', 'listener-initial-race-command-error-late', 'listener-initial-race-budget', 'listener-initial-race-hash-budget', 'listener-initial-race-cleanup-identity'];
const pendingModes = ['listener-pending-ready', 'listener-pending-http-only', 'listener-pending-deadline', 'listener-pending-invalid-pid', 'listener-pending-ambiguous', 'listener-pending-pid', 'listener-pending-http-ambiguous', 'listener-pending-http-swap', 'listener-pending-executable', 'listener-pending-birth', 'listener-pending-command-error', 'listener-pending-disappear', 'listener-pending-product', 'listener-pending-xctestrun', 'listener-pending-ownership', 'listener-pending-hash', 'listener-pending-bundle', 'listener-pending-simulator', 'listener-pending-receipt', 'listener-pending-exit', 'listener-pending-stop', 'listener-concurrent-exit-before-close', 'listener-pending-child-error', 'listener-pending-output-limit', 'listener-pending-owner-evidence-write'];
const listenerEvidenceModes = ['listener-first-failure', 'listener-second-failure', 'listener-uncoordinated-negative'];
const initialRaceStages: Record<string, string> = {
  'listener-initial-race': 'pending-xctestrun-hash',
  'listener-initial-race-birth': 'pending-runner-birth',
  'listener-initial-race-executable': 'pending-runner-executable',
  'listener-initial-race-hash': 'pending-runner-executable-hash',
  'listener-initial-race-product': 'pending-product-hash',
  'listener-initial-race-receipt': 'wda-pid-count',
  'listener-initial-race-receipt-command-error': 'wda-pid-count',
  'listener-initial-race-receipt-hash-error': 'wda-pid-count',
  'listener-initial-race-command-error': 'pending-mjpeg-executable-evidence',
  'listener-initial-race-command-error-late': 'pending-mjpeg-executable-evidence',
  'listener-initial-race-budget': 'listener-process-evidence',
  'listener-initial-race-hash-budget': 'startup-deadline',
  'listener-initial-race-cleanup-identity': 'pending-runner-birth',
};

const revocationModes = ['listener-exit-after-ready', 'listener-exit-after-ready-private-write-failure', 'listener-exit-after-ready-public-write-failure', 'listener-exit-after-ready-first-failure'];
export const xctestOwnerTests: Array<[string, () => Promise<void>]> = [];
xctestOwnerTests.push(['Native startup XCTest exit witness classification', async () => {
  const success = {status: 0, signal: null, stdout: 'S\n', stderr: ''};
  const live = encodeXctestExitProbe(success);
  assert.equal(live.classification, 'live-observation');
  assert.equal(live.status, 0);
  assert.equal(live.statusKind, 'number');
  assert.equal(live.error.own, false);
  for (const stdout of ['Z\n', ' Z+ \n', Buffer.from('Z\n')]) {
    const terminal = encodeXctestExitProbe({...success, stdout});
    assert.equal(terminal.classification, 'terminal-observation');
    assert.equal(terminal.stdout.kind, Buffer.isBuffer(stdout) ? 'buffer' : 'string');
    assert.equal(terminal.witnessGuarantee, 'numeric-pid-only');
  }
  for (const [record, kind] of [
    [{signal: null, stdout: 'Z\n', stderr: ''}, 'absent'],
    [{...success, status: undefined}, 'undefined'], [{...success, status: null}, 'null'],
    ...['0', NaN, Infinity, -1, 0.5, Number.MAX_SAFE_INTEGER + 1, {}, []].map(status => [{...success, status}, 'invalid']),
  ] as Array<[unknown, string]>) {
    const observation = JSON.parse(JSON.stringify(encodeXctestExitProbe(record)));
    assert.equal(Object.hasOwn(observation, 'status'), true);
    assert.equal(observation.status, null);
    assert.equal(observation.statusKind, kind);
    assert.equal(observation.statusOwn, kind !== 'absent');
    assert.equal(observation.classification, 'indeterminate');
  }
  for (const record of [undefined, null, false, 0, 'private-record']) assert.equal(encodeXctestExitProbe(record).classification, 'indeterminate');
  const eperm = Object.assign(new Error('private-error-sentinel'), {code: 'EPERM', arbitrary: 'private-object-sentinel'});
  const denied = encodeXctestExitProbe({...success, stdout: 'Z\n', error: eperm});
  assert.equal(denied.classification, 'indeterminate');
  assert.deepEqual(denied.error, {own: true, kind: 'object', present: true, category: 'Error', codeOwn: true, codeKind: 'string', code: 'EPERM'});
  for (const error of [eperm, new TypeError('private-error-sentinel'), {code: 'ESRCH'}, {code: 'private-code-sentinel'}, false, 0, 'private-error-sentinel']) {
    const encoded = encodeXctestExitProbe({...success, stdout: 'Z\n', error});
    assert.equal(encoded.classification, 'indeterminate');
    assert.equal(JSON.stringify(encoded).includes('private-'), false);
  }
  for (const [error, kind] of [[undefined, 'undefined'], [null, 'null']] as const) {
    const encoded = encodeXctestExitProbe({...success, error});
    assert.equal(encoded.error.own, true);
    assert.equal(encoded.error.kind, kind);
    assert.equal(encoded.error.present, false);
    assert.equal(encoded.classification, 'live-observation');
  }
  for (const key of ['stdout', 'stderr', 'signal'] as const) {
    for (const value of [undefined, null, {}, 1, 'private-output-sentinel'.repeat(100)]) {
      if (key === 'signal' && value === null) continue;
      const encoded = encodeXctestExitProbe({...success, stdout: 'Z\n', [key]: value});
      assert.equal(encoded[key].own, true);
      assert.equal(encoded[key].kind, value === undefined ? 'undefined' : value === null ? 'null' : typeof value);
      assert.equal(encoded.classification, 'indeterminate');
      assert.equal(JSON.stringify(encoded).includes('private-'), false);
      assert.ok(JSON.stringify(encoded).length < 1500);
    }
    const absent: Record<string, unknown> = {...success, stdout: 'Z\n'};
    delete absent[key];
    const encoded = encodeXctestExitProbe(absent);
    assert.equal(encoded[key].own, false);
    assert.equal(encoded[key].kind, 'absent');
    assert.equal(encoded.classification, 'indeterminate');
  }
  for (const status of [1, 23]) assert.equal(encodeXctestExitProbe({...success, status, stdout: 'Z\n'}).classification, 'indeterminate');
  for (const stdout of ['', 'not-a-state', 'S\nZ\n', 'X\n']) assert.equal(encodeXctestExitProbe({...success, stdout}).classification, 'indeterminate');
  for (const key of ['status', 'error', 'stdout', 'stderr', 'signal']) {
    let getterCalled = false;
    const accessor = {...success};
    Object.defineProperty(accessor, key, {get: () => { getterCalled = true; throw new Error('must not read accessor'); }});
    assert.equal(encodeXctestExitProbe(accessor).classification, 'indeterminate');
    assert.equal(getterCalled, false);
  }
}]);
function listenerOwnerProjection(value: unknown, supervisorPid: number | undefined) {
  const record = (input: unknown): Record<string, unknown> => input && typeof input === 'object' ? input as Record<string, unknown> : {};
  const list = (input: unknown): unknown[] => Array.isArray(input) ? input : [];
  const number = (input: unknown) => typeof input === 'number' && Number.isFinite(input) && input >= 0 && input <= Number.MAX_SAFE_INTEGER ? input : null;
  const pick = (input: unknown, allowed: string[]) => typeof input === 'string' && allowed.includes(input) ? input : 'other';
  const boolean = (input: unknown) => typeof input === 'boolean' ? input : null;
  const phases = ['startup', 'preflight', 'initial', 'after-initial', 'readiness', 'status', 'scenario', 'supervisor', 'cleanup'];
  const stages = ['wda-listener-command', 'mjpeg-listener-command', 'child-spawn', 'child-process', 'child-pid-birth', 'child-exit', 'child-exit-callback', 'owner-evidence-write', 'startup-deadline', 'startup-log-write', 'startup-log-limit', 'supervisor-error'];
  const categories = [...stages, 'listener-command-error', 'listener-invalid-process-id', 'child-spawn-error', 'child-process-error', 'output-limit'];
  const events = ['supervisor-start', 'child-spawn-requested', 'child-spawned', 'child-birth-observed', 'failure-observed', 'cleanup-enter', 'owner-ready-cleared', 'child-signal-requested', 'child-exit', 'child-close', 'child-stop-skipped', 'child-stop-finished', 'system-log-collected', 'owner-ended'];
  const owner = record(value), failure = record(owner.firstFailure), validation = record(owner.listenerValidation);
  const lifecycle = list(record(owner.diagnostics).lifecycle).map(record);
  const spawned = lifecycle.filter(event => event.event === 'child-spawned');
  const started = lifecycle.filter(event => event.event === 'supervisor-start');
  const owned = Number.isSafeInteger(owner.pid) && Number(owner.pid) > 0 && spawned.length === 1 && owner.pid === record(spawned[0].detail).pid;
  const endpoint = (input: unknown) => {
    const entry = record(input);
    return {status: pick(entry.status, ['evaluated', 'not-evaluated', 'error']), count: number(entry.count), errorCategory: pick(entry.errorCategory, ['command-error', 'invalid-process-id', 'timeout'])};
  };
  const selected = lifecycle.filter(event => events.includes(String(event.event)));
  const causal = list(failure.causalCommands);
  return {
    ownerRead: value ? 'read' : 'unavailable', clock: 'supervisor-process-relative',
    identity: {ownedChildMatchesSpawn: owned, parentMatchesSupervisor: started.length === 1 && Number.isSafeInteger(supervisorPid) && record(started[0].detail).pid === supervisorPid},
    ready: boolean(owner.ready), ended: Object.hasOwn(owner, 'endedAt'), firstFailurePresent: Boolean(owner.firstFailure),
    pinned: lifecycle.some(event => event.event === 'runner-candidate-pinned'), frozen: lifecycle.some(event => event.event === 'runner-frozen'),
    readyAdmitted: lifecycle.some(event => event.event === 'ready-admitted'), statusPublished: Boolean(owner.status),
    firstFailure: {source: pick(failure.source, ['tests/mobile/support/ios-xctest.ts']), phase: pick(failure.phase, phases), stage: pick(failure.stage, stages), category: pick(failure.category, categories),
      frozenOwner: boolean(failure.frozenOwner), commandIds: list(failure.commandIds).slice(0, 16).map(number), inspectionId: number(failure.inspectionId), monotonicMs: number(failure.monotonicMs)},
    listener: {failureStage: pick(validation.failureStage, stages), errorCategory: pick(validation.errorCategory, categories),
      wda: endpoint(record(validation.endpoints).wda), mjpeg: endpoint(record(validation.endpoints).mjpeg),
      runnerEvidencePresent: pick(validation.runnerEvidencePresent, ['not-evaluated', 'present', 'absent']),
      runnerAssociation: pick(validation.runnerAssociation, ['not-evaluated', 'match', 'mismatch']), mjpegAssociation: pick(validation.mjpegAssociation, ['not-evaluated', 'match', 'mismatch'])},
    lifecycle: selected.slice(0, 16).map(event => {
      const detail = record(event.detail);
      return {id: number(event.id), event: pick(event.event, events), phase: pick(detail.phase, phases), monotonicMs: number(event.monotonicMs),
        ...(Object.hasOwn(detail, 'first') ? {first: boolean(detail.first)} : {}),
        ...(Object.hasOwn(detail, 'frozenOwner') ? {frozenOwner: boolean(detail.frozenOwner)} : {}),
        ...(Object.hasOwn(detail, 'signal') ? {signal: detail.signal === null ? null : pick(detail.signal, ['SIGTERM', 'SIGINT', 'SIGKILL'])} : {}),
        ...(Object.hasOwn(detail, 'accepted') ? {accepted: boolean(detail.accepted)} : {}),
        ...(Object.hasOwn(detail, 'exitCode') ? {exitCode: number(detail.exitCode)} : {}),
        ...(Object.hasOwn(detail, 'childEnded') ? {childEnded: boolean(detail.childEnded)} : {})};
    }),
    causalCommands: causal.slice(0, 4).map(input => {
      const command = record(input), error = record(command.error);
      return {id: number(command.id), phase: pick(command.phase, phases), operation: pick(command.operation, ['inspect-listener', 'inspect-process-command', 'inspect-process-birth', 'inspect-process-executable', 'inspect-child-state', 'read-install-receipt', 'select-xctestrun', 'validate-product-bundle-id']),
        endpoint: pick(command.endpoint, ['wda', 'mjpeg', 'simulator']), status: number(command.status), signal: command.signal === null ? null : pick(command.signal, ['SIGTERM', 'SIGKILL', 'SIGABRT', 'SIGSEGV']),
        errorCategory: pick(error.category, ['timeout', 'spawn-error', 'nonzero-exit', 'output-limit']), errorCode: pick(error.code, ['ENOENT', 'EIO', 'EPERM', 'EACCES', 'ETIMEDOUT', 'ENOBUFS']),
        stdoutBytes: number(record(command.stdout).bytes), stderrBytes: number(record(command.stderr).bytes), timeoutMs: number(command.timeoutMs), durationMs: number(command.durationMs), monotonicStartMs: number(command.monotonicStartMs), monotonicEndMs: number(command.monotonicEndMs)};
    }),
    truncated: selected.length > 16 || causal.length > 4,
  };
}

function listenerFailureAttachment(root: string, mode: string, control: string | undefined, child: ChildProcess, closed: boolean, outcome: string, views: Record<string, unknown>, primary: unknown, cleanup: unknown): string {
  const readText = (name: string, limit: number) => readListenerText(root, name, limit);
  const markers = ['xcode.pid', 'runner.pid', 'listener-pair', 'listener-pair-count', 'listener-publication-release', 'listener-publication-ack', 'endpoint-ready', 'uncoordinated-wda-observed', 'uncoordinated-mjpeg-observed', 'first-failure-observation.json', 'listener-capture-held.json', 'listener-capture-decision.json', 'listener-capture-result.json', 'listener-capture-error.json', 'listener-capture-abort'];
  const captureReasons = ['WITHHELD', 'CANCELLED', 'BUDGET', 'MALFORMED', 'IDENTITY', 'READ_ERROR', 'WRITE_ERROR'];
  let capture = 'unavailable';
  try {
    const result = readListenerRecord(root, 'listener-capture-error.json', 512) as {reason?: string};
    capture = captureReasons.includes(result.reason || '') ? result.reason! : 'other';
  } catch { /* Collection is secondary to the original assertion. */ }
  let ownedPid: number | undefined;
  try {
    const owner = readListenerRecord(root, 'state/owner.json', 1_048_576) as {pid?: number};
    const proof = listenerFailureFields(owner, mode, owner.pid!, child.pid!, false);
    if (proof.spawnEventId > 0) ownedPid = owner.pid;
  } catch { /* Unknown identity must not render an arbitrary PID. */ }
  const traces = ['listener-pair-trace', 'listener-publication-trace'].map(name => {
    try {
      const lines = readText(name, 4096).trim().split(/\r?\n/u);
      return {name, truncated: lines.length > 24, entries: lines.slice(0, 24).map(line => {
        if (['runner-pid-written', 'publication-ack', 'endpoint-ready', 'uncoordinated-wda-empty'].includes(line)) return {event: line};
        const pair = /^(http|mjpeg|mjpeg-entry|mjpeg-ack) ([1-9]\d{0,2})(?: (EMPTY|[1-9]\d{0,9}))?$/u.exec(line);
        if (pair) return {event: pair[1], ordinal: Number(pair[2]), empty: pair[3] === 'EMPTY', matchesOwnedChild: ownedPid !== undefined && pair[3] === String(ownedPid)};
        const negative = /^(uncoordinated-publication|uncoordinated-mjpeg) ([1-9]\d{0,9})$/u.exec(line);
        if (negative) return {event: negative[1], matchesOwnedChild: ownedPid !== undefined && negative[2] === String(ownedPid)};
        return {event: 'other'};
      })};
    } catch { return {name, unavailable: true}; }
  });
  let launches: unknown, receiptQueries: unknown = 'unavailable';
  try { launches = {present: true, lines: readText('launches', 4096).trim().split(/\r?\n/u).length}; } catch { launches = {present: existsSync(join(root, 'launches')), unavailable: true}; }
  try { const text = readText('receipt-queries', 16); if (/^\d{1,6}$/u.test(text)) receiptQueries = Number(text); } catch { receiptQueries = 'unavailable'; }
  const boundedViews: Record<string, unknown> = {};
  for (const name of ['firstLive', 'producerReceipt', 'lastPoll', 'preFinalizer', 'settledFinal']) {
    const view = views[name];
    boundedViews[name] = view || {unavailable: true};
    if (Buffer.byteLength(JSON.stringify(boundedViews[name])) > 1150 && view && typeof view === 'object') {
      const projection = view as ReturnType<typeof listenerOwnerProjection>;
      boundedViews[name] = {truncated: true, ownerRead: projection.ownerRead, clock: projection.clock, identity: projection.identity, ready: projection.ready, ended: projection.ended,
        firstFailurePresent: projection.firstFailurePresent, firstFailure: projection.firstFailure, pinned: projection.pinned, frozen: projection.frozen, readyAdmitted: projection.readyAdmitted,
        firstEventId: projection.lifecycle?.find(event => event.first === true)?.id};
    }
  }
  const detailed = (views.settledFinal || views.preFinalizer || views.lastPoll) as Partial<ReturnType<typeof listenerOwnerProjection>> | undefined;
  const attachment = {schema: 1, mode: listenerEvidenceModes.includes(mode) ? mode : 'other', control: control === 'held-capture' || control === 'withheld-capture' ? control : 'none',
    observation: ['validated', 'unavailable', ...captureReasons].includes(outcome) ? outcome : 'other', capture,
    originalCode: primary instanceof assert.AssertionError ? 'ERR_ASSERTION' : 'other', primaryReason: primary instanceof assert.AssertionError && primary.message === `${mode} did not retain a pre-cleanup first-failure observation` ? 'missing-observation' : 'other',
    cleanup: cleanup ? 'failed' : 'none', supervisor: {closed, exitCode: child.exitCode, signal: child.signalCode === null ? null : ['SIGTERM', 'SIGINT', 'SIGKILL'].includes(child.signalCode) ? child.signalCode : 'other'},
    clocks: {owner: 'supervisor-process-relative', controlDecision: 'child-local', controlRelease: 'coordinator-local'}, views: boundedViews,
    markers: Object.fromEntries(markers.map(name => [name, existsSync(join(root, name))])), traces, launches, receiptQueries,
    settledDetail: {clock: 'supervisor-process-relative', listener: detailed?.listener, lifecycle: detailed?.lifecycle, causalCommands: detailed?.causalCommands, truncated: detailed?.truncated ?? false}};
  const text = JSON.stringify(attachment);
  if (Buffer.byteLength(text) <= 8000) return text;
  const reduced = {...attachment, truncated: true, traces: {truncated: true}, settledDetail: {truncated: true, listener: detailed?.listener, lifecycle: detailed?.lifecycle?.slice(0, 8), causalCommands: detailed?.causalCommands?.slice(0, 2)}};
  const reducedText = JSON.stringify(reduced);
  if (Buffer.byteLength(reducedText) <= 8000) return reducedText;
  const truncatedText = JSON.stringify({...reduced, settledDetail: {truncated: true, unavailable: true}});
  if (Buffer.byteLength(truncatedText) <= 8000) return truncatedText;
  return JSON.stringify({schema: 1, mode: attachment.mode, control: attachment.control, originalCode: attachment.originalCode, primaryReason: attachment.primaryReason,
    capture, cleanup: attachment.cleanup, truncated: true, views: Object.fromEntries(Object.keys(boundedViews).map(name => [name, {truncated: true, unavailable: true}]))});
}

function pendingInvalidSnapshot(root: string, mode: string, child: ChildProcess, closed: boolean): string {
  const record = (value: unknown): Record<string, unknown> => value && typeof value === 'object' ? value as Record<string, unknown> : {};
  const list = (value: unknown): unknown[] => Array.isArray(value) ? value : [];
  const number = (value: unknown) => typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : 'unavailable';
  const pick = (value: unknown, allowed: string[]) => typeof value === 'string' && allowed.includes(value) ? value : 'unavailable';
  const read = (name: string, limit: number) => {
    if (statSync(join(root, name)).size > limit) throw new Error('snapshot size limit');
    const text = readFileSync(join(root, name), 'utf8');
    if (Buffer.byteLength(text) > limit) throw new Error('snapshot size limit');
    return text;
  };
  let owner: Record<string, unknown> = {};
  let ownerRead = 'unavailable';
  try { owner = record(JSON.parse(read('state/owner.json', 1_048_576))); ownerRead = 'read'; } catch { ownerRead = 'read-error'; }
  let launchRead: Record<string, unknown> = {presence: existsSync(join(root, 'launches')), result: 'unavailable'};
  try {
    const text = read('launches', 8192);
    launchRead = {...launchRead, result: 'read', bytes: Buffer.byteLength(text), lines: text.trim().split(/\r?\n/u).length};
  } catch (error) { launchRead.result = pick(record(error).code, ['ENOENT', 'EACCES', 'EIO']); }
  let observations: string[] | string;
  let receiptQueryCount: number | string = 'unavailable';
  try { observations = read('pending-observations', 4096).trim().split(/\r?\n/u).slice(0, 8).map(value => /^\d{1,3}\/(?:0|1|2|invalid)$/u.test(value) ? value : 'unavailable'); } catch { observations = 'read-error'; }
  try { const text = read('receipt-queries', 16); if (/^\d{1,6}$/u.test(text)) receiptQueryCount = Number(text); } catch { receiptQueryCount = 'read-error'; }
  const failure = record(owner.firstFailure);
  const diagnostics = record(owner.diagnostics);
  const lifecycle = list(diagnostics.lifecycle).map(record);
  const events = ['child-spawned', 'child-error', 'child-exit', 'child-close', 'child-signal-requested', 'child-stop-finished', 'cleanup-enter', 'owner-ended'];
  const phases = ['preflight', 'startup', 'initial', 'after-initial', 'readiness', 'status', 'scenario', 'supervisor', 'cleanup'];
  const stages = ['mjpeg-listener-command', 'wda-listener-command', 'child-spawn', 'child-process', 'child-pid-birth', 'child-exit', 'child-exit-callback', 'owner-evidence-write', 'startup-deadline', 'startup-log-write', 'startup-log-limit', 'supervisor-error'];
  const categories = [...stages, 'listener-invalid-process-id', 'listener-command-error', 'child-spawn-error', 'child-process-error', 'output-limit'];
  const flag = (event: string) => ownerRead === 'read' && Array.isArray(diagnostics.lifecycle) ? lifecycle.some(entry => entry.event === event) : 'unavailable';
  const snapshot = {
    mode, ownerRead, launchRead, observations, receiptQueryCount,
    supervisor: {closed, exit: child.exitCode, signal: child.signalCode},
    firstFailure: {
      source: pick(failure.source, ['tests/mobile/support/ios-xctest.ts']), phase: pick(failure.phase, phases),
      stage: pick(failure.stage, stages), category: pick(failure.category, categories),
      commandIds: Array.isArray(failure.commandIds) ? failure.commandIds.slice(0, 8).map(number) : 'unavailable',
    },
    lifecycle: Array.isArray(diagnostics.lifecycle) ? lifecycle.filter(entry => events.includes(String(entry.event))).slice(0, 12).map(entry => ({id: number(entry.id), event: pick(entry.event, events)})) : 'unavailable',
    ready: typeof owner.ready === 'boolean' ? owner.ready : 'unavailable',
    pinned: flag('runner-candidate-pinned'), frozen: flag('runner-frozen'), readyAdmitted: flag('ready-admitted'),
    causalCommands: Array.isArray(failure.causalCommands) ? failure.causalCommands.slice(0, 4).map(value => {
      const command = record(value);
      return {
        id: number(command.id), phase: pick(command.phase, phases),
        operation: pick(command.operation, ['inspect-listener', 'inspect-process-command', 'inspect-process-birth', 'inspect-process-executable', 'inspect-child-state', 'read-install-receipt', 'select-xctestrun', 'validate-product-bundle-id']),
        endpoint: pick(command.endpoint, ['wda', 'mjpeg', 'simulator']),
        status: command.status === null ? null : number(command.status),
        signal: command.signal === null ? null : pick(command.signal, ['SIGTERM', 'SIGKILL', 'SIGABRT', 'SIGSEGV']),
        errorCategory: pick(record(command.error).category, ['timeout', 'spawn-error', 'nonzero-exit', 'output-limit']),
        stdoutBytes: number(record(command.stdout).bytes), stderrBytes: number(record(command.stderr).bytes),
      };
    }) : 'unavailable',
  };
  const text = JSON.stringify(snapshot);
  if (Buffer.byteLength(text) > 8192) throw new Error('snapshot size limit');
  return text;
}

const listenerCaptureControls = [
  ['Native startup actual XCTest supervisor listener-first-failure held-capture ordering', 'held-capture'],
  ['Native startup actual XCTest supervisor listener-first-failure withheld-capture prerequisite', 'withheld-capture'],
] as const;
const pendingInvalidControls = [
  ['Native startup actual XCTest supervisor listener-pending-invalid-pid held-start ordering', 'held-start'],
  ['Native startup actual XCTest supervisor listener-pending-invalid-pid withheld-start prerequisite', 'withheld-start'],
] as const;
for (const testMode of [...listenerCaptureControls.map(([name]) => name), ...pendingInvalidControls.map(([name]) => name), ...revocationModes, 'ready', 'ready-then-oversized', 'early', 'exit-before-close', 'invalid', 'occupied', 'ownership', 'ambiguous', 'product', 'receipt-failure', 'receipt-missing-then-valid', 'receipt-product-changed', 'swap', 'pid-reuse', 'oversized', 'delayed', 'credential', 'credential-binary', 'credential-binary-failure', 'stream-framing', 'stream-framing-reversed', 'stream-utf8', 'stream-eof', 'stream-finalization-failure', 'output-below', 'output-equal', 'output-above', 'output-combined', 'output-shrinking', 'output-expanding', 'noisy-cleanup', 'status-noisy-first-failure', 'status-first-failure', 'status-forged-markers', 'status-evidence-disappear', 'listener-mjpeg-duplicate', 'listener-bundle-mismatch', 'listener-hash-mismatch', 'listener-invalid-pid', 'listener-ready-owner-evidence-write', 'listener-first-failure', 'listener-status-one-stderr', 'listener-second-failure', 'listener-uncoordinated-negative', 'listener-endpoints-disappear', 'listener-hash-after-freeze', 'listener-initial-race', 'listener-initial-race-birth', 'listener-initial-race-executable', 'listener-initial-race-hash', 'listener-initial-race-product', 'listener-initial-race-receipt', 'listener-initial-race-receipt-command-error', 'listener-initial-race-receipt-hash-error', 'listener-initial-race-command-error', 'listener-initial-race-command-error-late', 'listener-initial-race-budget', 'listener-initial-race-hash-budget', 'listener-initial-race-cleanup-identity', 'listener-http-only', ...pendingModes]) {
  const invalidPidControl = pendingInvalidControls.find(([name]) => name === testMode)?.[1];
  const captureControl = listenerCaptureControls.find(([name]) => name === testMode)?.[1];
  const mode = invalidPidControl ? 'listener-pending-invalid-pid' : captureControl ? 'listener-first-failure' : testMode;
  xctestOwnerTests.push([invalidPidControl || captureControl ? testMode : `Native startup actual XCTest supervisor ${mode}`, async () => {
    const witnessBuild = mode === 'listener-concurrent-exit-before-close' ? await terminalBuild() : undefined;
    const witnessNonce = witnessBuild ? randomBytes(16).toString('hex') : undefined;
    const root = await mkdtemp(join(tmpdir(), 'herdr-xctest-test-'));
    if (witnessNonce) writeFileSync(join(root, 'witness-nonce'), witnessNonce, {flag: 'wx', mode: 0o600});
    const udid = '82342155-D8BD-4C4D-BD5E-1EDCDF9CFB40';
    const product = join(root, 'products/Debug-iphonesimulator/WebDriverAgentRunner-Runner.app');
    const receipt = join(root, `Devices/${udid}/data/Containers/Bundle/Application/10C0E9C0-50FD-4C3E-AC55-AC1158A00568/WebDriverAgentRunner-Runner.app`);
    const runnerReceipt = join(root, `Devices/${udid}/data/Containers/Bundle/Application/1C1FB5F7-2D0E-49AC-BE09-62816F229E5C/WebDriverAgentRunner-Runner.app`);
    const wrongRunnerReceipt = join(root, 'Devices/WRONG/data/Containers/Bundle/Application/1C1FB5F7-2D0E-49AC-BE09-62816F229E5C/WebDriverAgentRunner-Runner.app');
    const state = join(root, 'state');
    const bin = join(root, 'bin');
    const xctestrun = join(root, 'products/WebDriverAgentRunner_test.xctestrun');
    await Promise.all([mkdir(bin), mkdir(state), mkdir(join(product, 'PlugIns/WebDriverAgentRunner.xctest'), { recursive: true }), mkdir(receipt, { recursive: true }), mkdir(runnerReceipt, { recursive: true }), mkdir(wrongRunnerReceipt, { recursive: true }), mkdir(join(root, 'wda'))]);
    if (mode === 'listener-pending-receipt') {
      const invalidReceipt = join(root, 'invalid-receipt');
      await mkdir(invalidReceipt);
      await writeFile(join(invalidReceipt, 'Info.plist'), JSON.stringify({ CFBundleIdentifier: 'com.facebook.WebDriverAgentRunner.xctrunner' }));
      await writeFile(join(invalidReceipt, 'WebDriverAgentRunner-Runner'), 'exact built runner');
    }
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
    await writeFile(join(wrongRunnerReceipt, 'Info.plist'), JSON.stringify({ CFBundleIdentifier: 'com.facebook.WebDriverAgentRunner.xctrunner' }));
    await writeFile(join(wrongRunnerReceipt, 'WebDriverAgentRunner-Runner'), 'exact built runner');
    if (mode === 'credential-binary' || mode === 'credential-binary-failure') {
      await writeFile(join(receipt, 'Info.plist'), Buffer.from(credentialBinaryPlistBase64, 'base64'));
      if (process.platform === 'darwin') {
        const converted = spawnSync('/usr/bin/plutil', ['-convert', 'json', '-o', '-', join(receipt, 'Info.plist')], { encoding: 'utf8' });
        assert.equal(converted.status, 0, converted.stderr || 'Apple plutil binary fixture verification failed');
        assert.equal(JSON.parse(converted.stdout).CFBundleIdentifier, 'com.facebook.WebDriverAgentRunner.xctrunner');
      }
    }
    await writeFile(join(root, 'wda/package.json'), JSON.stringify({ version: '16.12.8' }));
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
    if (mode === 'credential' || mode === 'credential-binary' || mode === 'credential-binary-failure' || mode === 'listener-initial-race-executable' || mode === 'listener-initial-race-cleanup-identity' || mode === 'listener-pending-executable') await writeFile(join(runnerReceipt, 'unexpected-listener'), mode === 'listener-pending-executable' ? 'exact built runner' : 'unexpected listener');
    await writeFile(join(root, 'owned'), `ios:${mode === 'ownership' ? 'wrong' : udid}`);
    const dispatcher = join(bin, 'xctest-command-dispatcher');
    await writeFile(dispatcher, witnessBuild ? terminalShim() : shim, { mode: 0o700 });
    for (const cmd of ['plutil', 'lsof', 'ps', 'xcrun', 'xcodebuild']) await symlink(dispatcher, join(bin, cmd));
    const wdaServerFile = join(bin, 'xctest-wda-server.ts');
    await writeFile(wdaServerFile, witnessBuild ? terminalServer : wdaServer, { mode: 0o700 });
    if (mode === 'listener-pending-invalid-pid') await writeFile(join(bin, 'pending-invalid-pid-wait.ts'), pendingInvalidPidWait, {mode: 0o600});
    if (witnessBuild) terminalWrite(root, 'source-binding.json', {dispatcher: terminalHash(await readFile(dispatcher)), server: terminalHash(await readFile(wdaServerFile)), binary: witnessBuild.binaryHash, sources: witnessBuild.sources, buildRoot: witnessBuild.root});
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
      STARTUP_TEST_INVALID_PID_CONTROL: invalidPidControl || '',
      STARTUP_TEST_LISTENER_CAPTURE_CONTROL: captureControl || '',
      ...(mode === 'stream-finalization-failure' ? { STARTUP_TEST_LOG_FINALIZATION_TARGET: join(root, 'blocked-log-target') } : {}),
      ...(mode === 'listener-pending-deadline' || mode === 'listener-pending-command-error' || mode === 'listener-pending-child-error' || mode === 'listener-pending-owner-evidence-write' || mode === 'listener-initial-race-receipt' || mode === 'listener-initial-race-receipt-command-error' || mode === 'listener-initial-race-receipt-hash-error' || mode === 'listener-ready-owner-evidence-write' ? { XCTEST_COMMAND_TRACE: join(root, 'command-trace') } : {}) };
    const initialProductHash = createHash('sha256').update(await readFile(join(product, 'WebDriverAgentRunner-Runner'))).digest('hex');
    const deadline = Date.now() + (mode === 'invalid' || mode === 'oversized' ? 2000 : mode === 'delayed' || mode === 'listener-initial-race-budget' || mode === 'listener-initial-race-hash-budget' ? 1500 : mode === 'listener-pending-deadline' ? 5000 : 10_000);
    await writeFile(join(state, 'deadline'), String(deadline));
    let supervisorPath = join(import.meta.dirname, '../support/ios-xctest.ts');
    if (mode === 'listener-pending-child-error') {
      const source = await readFile(supervisorPath, 'utf8');
      const marker = "        const initial = inspectListeners(deadline, 'initial');\n";
      const replacement = "        if (existsSync(join(required('STARTUP_TEST_ROOT'), 'pending-child-error')) && owner.diagnostics.lifecycle.some(event => event.event === 'runner-candidate-pinned')) {\n          child?.emit('error', new Error('XCTEST: pending child error'));\n          throw new Error('XCTEST: child process error');\n        }\n" + marker;
      const variant = source.replace(marker, replacement);
      assert.notEqual(variant, source);
      const support = join(root, 'supervisor-support');
      await mkdir(support);
      await writeFile(join(support, 'diagnostics.ts'), await readFile(join(import.meta.dirname, '../support/diagnostics.ts')));
      supervisorPath = join(support, 'ios-xctest.ts');
      await writeFile(supervisorPath, variant);
    }
    if (mode === 'listener-ready-owner-evidence-write') {
      const source = await readFile(supervisorPath, 'utf8');
      const marker = '          owner.ready = true;\n';
      const replacement = "          writeFileSync(join(required('STARTUP_TEST_ROOT'), 'ready-evidence-boundary'), 'ready');\n          while (!existsSync(join(required('STARTUP_TEST_ROOT'), 'ready-evidence-continue'))) {\n            if (stop || owner.firstFailure || childExited || existsSync(join(required('STARTUP_TEST_ROOT'), 'stop'))) throw new Error('XCTEST: readiness evidence boundary cancelled');\n            await sleep(1);\n          }\n" + marker;
      const readinessVariant = source.replace(marker, replacement);
      assert.notEqual(readinessVariant, source);
      const fallbackMarker = "      writeFileSync(join(root, 'owner.json'), ownerContent(owner, true), { mode: 0o600 });\n";
      const fallbackReplacement = fallbackMarker + "      writeFileSync(join(required('STARTUP_TEST_ROOT'), 'ready-failure-fallback-written'), 'written');\n      while (!existsSync(join(required('STARTUP_TEST_ROOT'), 'ready-failure-fallback-continue'))) {}\n";
      const variant = readinessVariant.replace(fallbackMarker, fallbackReplacement);
      assert.notEqual(variant, readinessVariant);
      const support = join(root, 'supervisor-support');
      await mkdir(support);
      await writeFile(join(support, 'diagnostics.ts'), await readFile(join(import.meta.dirname, '../support/diagnostics.ts')));
      supervisorPath = join(support, 'ios-xctest.ts');
      await writeFile(supervisorPath, variant);
    }
    if (revocationModes.includes(mode)) {
      const source = await readFile(supervisorPath, 'utf8');
      const replaceOnce = (text: string, marker: string, replacement: string): string => {
        assert.equal(text.split(marker).length, 2, 'revocation source marker must be unique');
        return text.replace(marker, replacement);
      };
      const promiseMarker = '    childExitObserved = new Promise(resolve => { resolveChildExit = resolve; });\n';
      let variant = replaceOnce(source, promiseMarker, promiseMarker + `    let exitResolverReturned = false;
    const productionExitResolver = resolveChildExit;
    resolveChildExit = () => { productionExitResolver(); exitResolverReturned = true; };
    childExitObserved.then(() => writeFileSync(join(required('STARTUP_TEST_ROOT'), 'exit-promise-resolved'), 'resolved'));
`);
      const closeMarker = "      child!.on('close', (code, signal) => {\n";
      variant = replaceOnce(variant, closeMarker, `      // Second listener: genuine event only, registered AFTER production exit.
      child!.on('exit', (code, signal) => {
        const fixture = required('STARTUP_TEST_ROOT');
        const ack = join(fixture, 'exit-callback-returned');
        writeFileSync(ack + '.next', JSON.stringify({schema: 1, code, signal, exitResolverReturned, childExited, childEnded}));
        renameSync(ack + '.next', ack);
        while (!existsSync(join(fixture, 'exit-callback-continue')) && Date.now() < deadline) {}
        if (!existsSync(join(fixture, 'exit-callback-continue'))) {
          writeFileSync(join(fixture, 'exit-callback-timeout'), 'ERROR');
          process.exitCode = 1;
        }
      });
` + closeMarker);
      const readyMarker = "          diagnostics.lifecycle('ready-admitted', {phase: 'readiness', endpoint: 'wda', port, frozenOwner: true});\n";
      variant = replaceOnce(variant, readyMarker, readyMarker + `          writeFileSync(join(required('STARTUP_TEST_ROOT'), 'exit-ready-ack'), 'ready');
` + (mode === 'listener-exit-after-ready-first-failure' ? `          while (!existsSync(join(required('STARTUP_TEST_ROOT'), 'exit-first-failure-request')) && Date.now() < deadline) await sleep(1);
          if (Date.now() >= deadline) throw new Error('XCTEST: first failure handshake deadline');
          if (save()) throw new Error('XCTEST: expected actual owner writer failure');
          writeFileSync(join(required('STARTUP_TEST_ROOT'), 'exit-first-failure-ack'), 'failed');
          // Keep cleanup from doing the revocation/exit bookkeeping under test.
          while (!existsSync(join(required('STARTUP_TEST_ROOT'), 'exit-callback-continue')) && Date.now() < deadline) await sleep(1);
` : ''));
      const support = join(root, 'supervisor-support');
      await mkdir(support);
      const diagnosticsSource = await readFile(join(import.meta.dirname, '../support/diagnostics.ts'));
      await writeFile(join(support, 'diagnostics.ts'), diagnosticsSource);
      supervisorPath = join(support, 'ios-xctest.ts');
      await writeFile(supervisorPath, variant);
      const hash = (text: string | Buffer): string => createHash('sha256').update(text).digest('hex');
      await writeFile(join(root, 'revocation-source-binding.json'), JSON.stringify({
        schema: 1, source: hash(source), variant: hash(variant), diagnostics: hash(diagnosticsSource),
        dispatcher: hash(shim), server: hash(wdaServer), mode,
      }));
    }
    const child = witnessBuild
      ? terminalSpawn(process.execPath, [supervisorPath, 'supervise'], {env, stdio: ['ignore', 'ignore', 'pipe']}, deadline + 30_000)
      : spawn(process.execPath, [supervisorPath, 'supervise'], { env, detached: false, stdio: ['ignore', 'ignore', 'pipe'] });
    const witnessSettlement = witnessBuild ? terminalSettlement(child, deadline + 30_000, () => terminalFailure(root)) : undefined;
    const supervisorStarted = Date.now();
    let errors = '';
    child.stderr!.on('data', chunk => { errors += chunk.toString(); });
    let done = false;
    let assertionsPassed = false;
    let initialRaceFirstFailure: unknown;
    let firstFailureObservation: Record<string, unknown> | undefined;
    let heldFirstFailure: ReturnType<typeof listenerFailureFields> | undefined;
    let withheldObservationFailure: unknown;
    let listenerFailure: {error: unknown} | undefined;
    let listenerDiagnostic = '{"collection":"unavailable"}';
    let observationOutcome = 'unavailable';
    const listenerViews: Record<string, unknown> = {};
    let lastListenerOwner: unknown;
    const readListenerOwner = () => {
      lastListenerOwner = readListenerRecord(root, 'state/owner.json', 1_048_576);
      listenerViews.lastPoll = listenerOwnerProjection(lastListenerOwner, child.pid);
      return lastListenerOwner as Record<string, unknown>;
    };
    let revocationFirstFailure: unknown;
    let revocationPrivateFirstFailure: unknown;
    let revocationError: unknown;
    let pendingInvalidFailure: {error: unknown; diagnostic: string} | undefined;
    let cleanupFailure: {error: unknown} | undefined;
    let pendingInvalidFirstFailure: unknown;
    const assertInvalidFailure = (owner: {firstFailure?: {stage?: string; category?: string; phase?: string}}) => {
      assert.equal(owner.firstFailure?.stage, 'mjpeg-listener-command', 'fixture prerequisite: unexpected pending invalid PID failure stage');
      assert.equal(owner.firstFailure?.category, 'listener-invalid-process-id', 'fixture prerequisite: unexpected pending invalid PID failure category');
      assert.equal(owner.firstFailure?.phase, 'initial', 'fixture prerequisite: unexpected pending invalid PID failure phase');
    };
    const readInvalidPidBudget = () => {
      const path = join(root, 'pending-invalid-budget');
      assert.ok(statSync(path).size <= 512, 'fixture prerequisite: budget receipt size');
      const budget = JSON.parse(readFileSync(path, 'utf8'));
      assert.match(budget.startedNs, /^\d{1,24}$/u);
      assert.match(budget.endNs, /^\d{1,24}$/u);
      assert.equal(budget.startupDeadline, deadline);
      assert.ok(Number.isSafeInteger(budget.budgetMs) && budget.budgetMs > 0 && budget.budgetMs <= 2000);
      assert.equal(BigInt(budget.endNs) - BigInt(budget.startedNs), BigInt(budget.budgetMs) * 1_000_000n);
      return budget as {startedNs: string; endNs: string; startupDeadline: number; budgetMs: number};
    };
    const assertInvalidPidResult = (expected: 'WITHHELD' | 'ACKNOWLEDGED') => {
      const budget = readInvalidPidBudget();
      const path = join(root, 'pending-invalid-result');
      assert.ok(statSync(path).size <= 512, 'fixture prerequisite: result receipt size');
      const result = JSON.parse(readFileSync(path, 'utf8'));
      assert.equal(result.result, expected, 'fixture prerequisite: unexpected acknowledgment result');
      assert.match(result.observedNs, /^\d{1,24}$/u);
      assert.ok(BigInt(result.observedNs) >= BigInt(budget.startedNs) && BigInt(result.observedNs) < BigInt(budget.endNs), 'fixture prerequisite: acknowledgment decision exceeded local budget');
    };
    const exited = new Promise<void>(resolve => child.on('close', () => { done = true; resolve(); }));
    let witness: TerminalSession | undefined;
    let witnessFailure: unknown;
    if (witnessBuild) {
      child.once('exit', () => { if (!existsSync(join(root, 'child-exit-observed'))) terminalFailure(root); });
    }
    const runAssertions = async () => {
      if (witnessBuild && witnessNonce) {
        witness = await terminalStart(root, witnessNonce, terminalPid(child.pid), deadline, witnessBuild);
        await terminalArm(witness);
      }
      if (revocationModes.includes(mode)) {
        const waitFor = async (name: string): Promise<void> => {
          while (!done && Date.now() < deadline && !existsSync(join(root, name))) await pause();
          assert.equal(existsSync(join(root, name)), true, `revocation handshake missing: ${name}`);
          assert.equal(done, false, 'supervisor exited before revocation assertions');
        };
        await waitFor('exit-ready-ack');
        for (const name of ['owner.json', 'owner-private.json']) {
          const ready = JSON.parse(await readFile(join(state, name), 'utf8'));
          assert.equal(ready.ready, true);
          assert.equal(ready.exitCode, undefined);
          assert.equal(ready.firstFailure, undefined);
          assert.equal(ready.endedAt, undefined);
        }
        const fault = mode === 'listener-exit-after-ready-public-write-failure' ? 'owner.next.json'
          : mode === 'listener-exit-after-ready' ? undefined : 'owner-private.next.json';
        if (fault) await mkdir(join(state, fault));
        if (mode === 'listener-exit-after-ready-first-failure') {
          await writeFile(join(root, 'exit-first-failure-request'), 'perform real save');
          await waitFor('exit-first-failure-ack');
          const first = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
          const privateFirst = JSON.parse(await readFile(join(state, 'owner-private.json'), 'utf8'));
          assert.equal(first.firstFailure.stage, 'owner-evidence-write');
          assert.equal(first.error, 'XCTEST: owner-evidence-write');
          assert.equal(first.exitCode, undefined);
          assert.equal(first.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'cleanup-enter'), false);
          revocationFirstFailure = first.firstFailure;
          revocationPrivateFirstFailure = privateFirst.firstFailure;
          revocationError = first.error;
        }
        await writeFile(join(root, 'exit-child-requested'), 'request real child exit');
        await waitFor('child-exit-intent');
        await writeFile(join(root, 'child-exit-continue'), 'release actual child exit');
        await waitFor('exit-callback-returned');
        const ack = JSON.parse(await readFile(join(root, 'exit-callback-returned'), 'utf8'));
        assert.deepEqual(ack, {schema: 1, code: 46, signal: null, exitResolverReturned: true, childExited: true, childEnded: false});
        const views = [];
        for (const name of ['owner.json', 'owner-private.json']) {
          const revoked = JSON.parse(await readFile(join(state, name), 'utf8'));
          assert.equal(revoked.ready, false, `${name} must revoke in real callback, not cleanup`);
          assert.equal(revoked.exitCode, 46);
          assert.equal(revoked.signal, null);
          assert.equal(revoked.endedAt, undefined);
          const lifecycle: Array<{id: number; event: string; detail?: {exitCode?: number; signal?: string | null}}> = revoked.diagnostics.lifecycle;
          assert.equal(lifecycle.some(event => ['cleanup-enter', 'child-close', 'owner-ready-cleared', 'owner-ended'].includes(event.event)), false);
          const admitted = lifecycle.find(event => event.event === 'ready-admitted');
          const exit = lifecycle.find(event => event.event === 'child-exit');
          assert.ok(admitted && exit && admitted.id < exit.id);
          assert.equal(exit.detail?.exitCode, 46);
          assert.equal(exit.detail?.signal, null);
          assert.equal(revoked.exitRevocation?.primaryWriteFailed, fault ? true : undefined);
          assert.equal(revoked.exitRevocation?.callbackFailed, undefined);
          assert.equal(revoked.exitRevocation?.persistencePrerequisite, undefined);
          if (revocationFirstFailure) {
            assert.deepEqual(revoked.firstFailure, name === 'owner.json' ? revocationFirstFailure : revocationPrivateFirstFailure);
            assert.equal(revoked.error, revocationError);
          } else {
            assert.equal(revoked.firstFailure.stage, 'child-exit');
            assert.equal(revoked.firstFailure.category, 'child-exit');
            assert.equal(revoked.error, 'XCTEST: child-exit');
          }
          // Export only these fixed fields, not either owner or its credentials.
          views.push({view: name === 'owner.json' ? 'public' : 'private', ready: revoked.ready,
            exitCode: revoked.exitCode, signal: revoked.signal, admittedId: admitted.id, exitId: exit.id,
            firstFailureStage: revoked.firstFailure.stage, firstFailurePreserved: true});
        }
        // The descendant is independent and still serves while both supervisor
        // cleanup and close callbacks are excluded by the synchronous latch.
        const response = await fetch(`http://127.0.0.1:${port}/status`, {signal: AbortSignal.timeout(Math.max(1, deadline - Date.now()))});
        assert.equal(response.status, 200);
        assert.equal((await response.json() as {value: {ready: boolean}}).value.ready, true);
        assert.equal(existsSync(join(root, 'exit-callback-timeout')), false);
        await writeFile(join(root, 'revocation-receipt.json'), JSON.stringify({schema: 1, mode, ack, views, descendantResponded: true}));
        if (fault) await rm(join(state, fault), {recursive: true});
        await writeFile(join(root, 'exit-callback-continue'), 'revocation assertions complete');
        // Native Promise fulfillment, as well as the synchronous resolver ack.
        while (Date.now() < deadline && !existsSync(join(root, 'exit-promise-resolved'))) await pause();
        assert.equal(existsSync(join(root, 'exit-promise-resolved')), true);
      }
      const credentialMode = ['credential', 'credential-binary', 'credential-binary-failure'].includes(mode);
      const streamMode = ['stream-framing', 'stream-framing-reversed', 'stream-utf8', 'stream-eof', 'stream-finalization-failure'].includes(mode);
      const outputBoundaryMode = ['output-below', 'output-equal', 'output-above', 'output-combined', 'output-shrinking', 'output-expanding'].includes(mode);
      if (listenerEvidenceModes.includes(mode)) {
        const observationDeadline = Date.now() + 15_000;
        if (captureControl) {
          while (!done && Date.now() < Math.min(deadline, observationDeadline) && !existsSync(join(root, 'listener-capture-held.json'))) await pause();
          const coordinatorBudgetMs = Math.min(2000, deadline - Date.now());
          assert.ok(coordinatorBudgetMs > 0, 'listener capture coordinator budget');
          const coordinatorEnd = process.hrtime.bigint() + BigInt(coordinatorBudgetMs) * 1_000_000n;
          const held = readListenerRecord(root, 'listener-capture-held.json', 512) as Record<string, unknown>;
          assert.deepEqual(Object.keys(held).sort(), ['schema', 'mode', 'control', 'producerPid', 'producerParentPid', 'spawnEventId', 'startupDeadline', 'budgetMs', 'startedNs', 'endNs'].sort());
          assert.equal(held.schema, 1);
          assert.equal(held.mode, mode);
          assert.equal(held.control, captureControl);
          assert.equal(held.startupDeadline, deadline);
          assert.ok(Number.isSafeInteger(held.budgetMs) && Number(held.budgetMs) > 0 && Number(held.budgetMs) <= 2000);
          assert.match(String(held.startedNs), /^\d{1,24}$/u);
          assert.match(String(held.endNs), /^\d{1,24}$/u);
          assert.equal(BigInt(String(held.endNs)) - BigInt(String(held.startedNs)), BigInt(Number(held.budgetMs)) * 1_000_000n);
          const liveOwner = readListenerOwner();
          assert.equal(held.producerPid, liveOwner.pid);
          heldFirstFailure = listenerFailureFields(liveOwner, mode, Number(held.producerPid), child.pid!, true);
          assert.equal(held.producerParentPid, child.pid);
          assert.equal(held.spawnEventId, heldFirstFailure.spawnEventId);
          listenerViews.firstLive ??= listenerOwnerProjection(liveOwner, child.pid);
          const events = (liveOwner.diagnostics as {lifecycle: Array<{event: string}>}).lifecycle;
          assert.equal(events.some(event => ['child-exit', 'child-close', 'owner-ended'].includes(event.event)), false);
          assert.equal(done, false);
          assert.equal(existsSync(join(root, 'first-failure-observation.json')), false);
          assert.equal(existsSync(join(root, 'listener-capture-error.json')), false);
          assert.ok(process.hrtime.bigint() < coordinatorEnd && Date.now() < Math.min(deadline, observationDeadline), 'listener capture coordinator decision budget');
          writeFileSync(join(root, 'listener-capture-decision.json.next'), JSON.stringify({schema: 1, decision: captureControl === 'held-capture' ? 'CAPTURE' : 'WITHHELD'}), {flag: 'wx', mode: 0o600});
          assert.ok(process.hrtime.bigint() < coordinatorEnd && Date.now() < Math.min(deadline, observationDeadline), 'listener capture coordinator publication budget');
          renameSync(join(root, 'listener-capture-decision.json.next'), join(root, 'listener-capture-decision.json'));
          let timer: ReturnType<typeof setTimeout> | undefined;
          try {
            await Promise.race([exited, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('listener capture supervisor deadline')), Math.max(0, observationDeadline - Date.now())); })]);
          } finally { clearTimeout(timer); }
          assert.equal(done, true);
          const result = readListenerRecord(root, 'listener-capture-result.json', 512) as Record<string, unknown>;
          assert.deepEqual(Object.keys(result).sort(), ['schema', 'decision', 'observedNs'].sort());
          assert.equal(result.schema, 1);
          assert.equal(result.decision, captureControl === 'held-capture' ? 'CAPTURE' : 'WITHHELD');
          assert.match(String(result.observedNs), /^\d{1,24}$/u);
          assert.ok(BigInt(String(result.observedNs)) >= BigInt(String(held.startedNs)) && BigInt(String(result.observedNs)) < BigInt(String(held.endNs)), 'listener capture child-local decision budget');
          if (captureControl === 'withheld-capture') {
            assert.deepEqual(readListenerRecord(root, 'listener-capture-error.json', 512), {schema: 1, reason: 'WITHHELD'});
            assert.equal(existsSync(join(root, 'first-failure-observation.json')), false);
          } else assert.equal(existsSync(join(root, 'listener-capture-error.json')), false);
        }
        const observe = async () => {
          while (Date.now() < observationDeadline && !firstFailureObservation) {
            try {
              if (existsSync(join(state, 'owner.json'))) {
                const current = readListenerOwner();
                try {
                  listenerFailureFields(current, mode, Number(current.pid), child.pid!, true);
                  listenerViews.firstLive ??= listenerOwnerProjection(current, child.pid);
                } catch { /* A finalized owner is diagnostic only, never a witness. */ }
              }
            } catch { listenerViews.lastPoll = {unavailable: true, reason: 'READ_ERROR'}; }
            try {
              if (existsSync(join(root, 'first-failure-observation.json'))) {
                const receipt = readListenerRecord(root, 'first-failure-observation.json', 65_536) as Record<string, unknown>;
                const current = readListenerOwner();
                assert.equal(receipt.producerPid, current.pid);
                const fields = listenerFailureFields(current, mode, Number(receipt.producerPid), child.pid!, false);
                assert.deepEqual(Object.keys(receipt).sort(), ['schema', 'mode', 'trigger', 'producerPid', 'producerParentPid', 'spawnEventId', 'capturedAt', 'capturedBeforeOwnerEnded', 'firstFailure', 'listenerValidation', 'failureEvent'].sort());
                assert.equal(receipt.schema, 1);
                assert.equal(receipt.mode, mode);
                assert.equal(receipt.trigger, 'SIGTERM');
                assert.equal(receipt.producerParentPid, child.pid);
                assert.equal(receipt.spawnEventId, fields.spawnEventId);
                assert.equal(receipt.capturedBeforeOwnerEnded, true);
                assert.match(String(receipt.capturedAt), /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/u);
                assert.deepEqual(receipt.firstFailure, fields.firstFailure);
                assert.deepEqual(receipt.listenerValidation, fields.listenerValidation);
                assert.deepEqual(receipt.failureEvent, fields.failureEvent);
                assert.equal(existsSync(join(root, 'listener-capture-error.json')), false);
                firstFailureObservation = receipt;
                observationOutcome = 'validated';
                listenerViews.producerReceipt = {schema: 1, trigger: 'SIGTERM', producerPid: receipt.producerPid,
                  parentMatchesSupervisor: receipt.producerParentPid === child.pid, spawnMatchesOwner: true,
                  capturedBeforeOwnerEnded: true, spawnEventId: fields.spawnEventId, firstFailureEventId: fields.failureEvent.id};
              }
            } catch (error) {
              observationOutcome = error instanceof Error && error.message === 'IDENTITY' ? 'IDENTITY' : error instanceof Error && error.message === 'READ_ERROR' ? 'READ_ERROR' : 'MALFORMED';
              break;
            }
            if (!firstFailureObservation) await pause();
          }
          assert.ok(firstFailureObservation, `${mode} did not retain a pre-cleanup first-failure observation`);
        };
        if (captureControl === 'withheld-capture') {
          await assert.rejects(observe, error => {
            withheldObservationFailure = error;
            return error instanceof assert.AssertionError && error.code === 'ERR_ASSERTION'
              && error.message === `${mode} did not retain a pre-cleanup first-failure observation`
              && observationOutcome === 'unavailable';
          });
          assert.equal(existsSync(join(root, 'first-failure-observation.json')), false);
        } else await observe();
      }
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
      if (initialRaceModes.includes(mode)) {
        const evidenceDeadline = Date.now() + 15_000;
        while (Date.now() < evidenceDeadline) {
          if (existsSync(join(state, 'owner.json'))) {
            const current = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
            if (current.firstFailure && current.initialCandidateDiagnostic) break;
          }
          await pause();
        }
        const raceOwner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
        const racePrivateOwner = JSON.parse(await readFile(join(state, 'owner-private.json'), 'utf8'));
        assert.equal(raceOwner.firstFailure.stage, initialRaceStages[mode]);
        assert.equal(raceOwner.firstFailure.phase, 'initial');
        assert.equal(raceOwner.firstFailure.frozenOwner, false);
        assert.equal(raceOwner.initialCandidateDiagnostic.phase, 'initial-diagnostic');
        assert.equal(raceOwner.initialCandidateDiagnostic.schema, 1);
        assert.match(raceOwner.initialCandidateDiagnostic.candidatePid, /^[1-9]\d{0,9}$/u);
        for (const candidate of [raceOwner, racePrivateOwner]) {
          const text = JSON.stringify(candidate.initialCandidateDiagnostic);
          assert.equal(text.includes('http://'), false);
          assert.equal(text.includes('/tmp/'), false);
          assert.equal(text.includes('/private/'), false);
          assert.ok(Buffer.byteLength(JSON.stringify(candidate)) <= 1_048_576);
        }
        if (mode === 'listener-initial-race') await assertSanitizedExports(root, state, [], ['owner.json']);
        assert.equal(raceOwner.ready, false);
        assert.equal(raceOwner.runnerPid, undefined);
        assert.equal(raceOwner.runnerBirth, undefined);
        assert.equal(raceOwner.runnerExecutable, undefined);
        assert.equal(raceOwner.status, undefined);
        assert.equal(existsSync(join(state, 'wda-status.json')), false);
        assert.equal(existsSync(join(root, 'status-queries')), false);
        initialRaceFirstFailure = JSON.parse(JSON.stringify(raceOwner.firstFailure));
        if (!raceOwner.endedAt) await writeFile(join(state, 'stop'), 'initial race diagnostic finalization');
      }
      if (pendingModes.includes(mode)) {
        const pendingDeadline = Date.now() + 15_000;
        if (invalidPidControl) {
          while (!done && Date.now() < deadline && !existsSync(join(root, 'pending-invalid-boundary'))) await pause();
          const controlEndNs = process.hrtime.bigint() + 2_000_000_000n;
          const checkControlBudget = () => {
            if (existsSync(join(root, 'pending-invalid-budget'))) readInvalidPidBudget();
            assert.ok(process.hrtime.bigint() < controlEndNs && Date.now() < deadline, 'fixture prerequisite: held start acknowledgment budget exhausted');
          };
          while (!done && !(existsSync(join(root, 'pending-invalid-held')) && existsSync(join(root, 'pending-invalid-waiting')) && existsSync(join(root, 'pending-invalid-budget')))) {
            checkControlBudget();
            if (existsSync(join(root, 'pending-observations'))) {
              assert.equal((await readFile(join(root, 'pending-observations'), 'utf8')).includes('invalid'), false, 'invalid response preceded actual launch publication');
            }
            await pause();
          }
          checkControlBudget();
          assert.equal(done, false, 'fixture prerequisite: supervisor closed during held start');
          assert.equal(existsSync(join(root, 'pending-invalid-boundary')), true, 'fixture prerequisite: invalid injection boundary missing');
          assert.equal(existsSync(join(root, 'pending-invalid-held')), true, 'fixture prerequisite: actual mock did not reach held start');
          assert.equal(existsSync(join(root, 'pending-invalid-waiting')), true, 'fixture prerequisite: launch acknowledgment wait missing');
          const heldOwner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
          const heldPid = (await readFile(join(root, 'pending-invalid-held'), 'utf8')).trim();
          assert.match(heldPid, /^[1-9]\d{0,9}$/u);
          assert.ok(Number(heldPid) <= 2147483647);
          assert.equal(heldPid, String(heldOwner.pid));
          assert.equal(heldOwner.diagnostics.lifecycle.find((event: {event: string}) => event.event === 'child-spawned').detail.pid, Number(heldPid));
          assert.equal((await readFile(join(root, 'pending-invalid-waiting'), 'utf8')).trim(), heldPid);
          assert.equal(heldOwner.firstFailure, undefined, 'fixture prerequisite: supervisor failed during held start');
          assert.equal(heldOwner.ready, false);
          assert.equal(heldOwner.diagnostics.lifecycle.some((event: {event: string}) => ['runner-candidate-pinned', 'runner-frozen', 'ready-admitted'].includes(event.event)), false);
          assert.equal(existsSync(join(root, 'status-queries')), false);
          assert.equal(existsSync(join(root, 'launches')), false);
          assert.equal(existsSync(join(root, 'xcode.pid')), false);
          assert.equal(existsSync(join(root, 'pending-invalid-acknowledged')), false);
          assert.deepEqual((await readFile(join(root, 'pending-observations'), 'utf8')).trim().split(/\r?\n/u), ['1/0']);
          checkControlBudget();
          writeFileSync(join(root, invalidPidControl === 'held-start' ? 'pending-invalid-release' : 'pending-invalid-withheld'), 'test decision');
        }
        while (!done && Date.now() < pendingDeadline) {
          if (witness) terminalCheck(root, deadline);
          if (existsSync(join(state, 'owner.json'))) {
            const current = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
            const lifecycle = current.diagnostics?.lifecycle || [];
            if (current.firstFailure || lifecycle.some((event: {event: string}) => event.event === 'runner-candidate-pinned')) break;
          }
          await pause();
        }
        const pendingOwner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
        if (mode === 'listener-pending-invalid-pid') {
          await writeFile(join(root, 'pending-candidate-continue'), 'invalid response observation complete');
          if (invalidPidControl === 'withheld-start') {
            assert.throws(() => assertInvalidFailure(pendingOwner), /fixture prerequisite/u);
            assert.equal(await readFile(join(root, 'pending-invalid-prerequisite'), 'utf8'), 'PREREQUISITE\n');
            assertInvalidPidResult('WITHHELD');
            assert.equal(pendingOwner.firstFailure.stage, 'mjpeg-listener-command');
            assert.equal(pendingOwner.firstFailure.category, 'listener-command-error');
            assert.equal(pendingOwner.firstFailure.phase, 'initial');
            const failedCommand = pendingOwner.firstFailure.causalCommands.find((command: {endpoint?: string}) => command.endpoint === 'mjpeg');
            assert.equal(failedCommand.status, 2);
            assert.equal(failedCommand.signal, null);
            assert.equal(failedCommand.error, undefined);
            assert.equal(failedCommand.stdout.bytes, 0);
            assert.ok(failedCommand.stderr.bytes > 0);
            let timer: ReturnType<typeof setTimeout> | undefined;
            try {
              await Promise.race([exited, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('fixture prerequisite: withheld start cleanup deadline')), 15_000); })]);
            } finally { clearTimeout(timer); }
            const settled = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
            assert.deepEqual(settled.firstFailure, pendingOwner.firstFailure);
            assert.ok(settled.endedAt);
            assert.equal(settled.ready, false);
            assert.equal(settled.runnerPid, undefined);
            assert.equal(settled.runnerBirth, undefined);
            assert.equal(settled.runnerExecutable, undefined);
            assert.equal(settled.diagnostics.lifecycle.some((event: {event: string}) => ['runner-candidate-pinned', 'runner-frozen', 'ready-admitted', 'owned-listener-termination-requested'].includes(event.event)), false);
            for (const event of ['child-spawned', 'child-exit', 'child-close', 'child-stop-finished', 'owner-ended']) {
              assert.equal(settled.diagnostics.lifecycle.some((entry: {event: string}) => entry.event === event), true);
            }
            for (const name of ['launches', 'xcode.pid', 'runner.pid', 'pending-invalid-acknowledged', 'status-queries', 'state/wda-status.json']) assert.equal(existsSync(join(root, name)), false);
            const withheldObservations = (await readFile(join(root, 'pending-observations'), 'utf8')).trim().split(/\r?\n/u);
            assert.equal(withheldObservations[0], '1/0');
            assert.equal(withheldObservations.some(value => value.includes('invalid')), false);
            assert.equal(await readFile(join(root, 'receipt-queries'), 'utf8'), '1');
            assertionsPassed = true;
            return;
          }
          assertInvalidFailure(pendingOwner);
          assertInvalidPidResult('ACKNOWLEDGED');
          pendingInvalidFirstFailure = pendingOwner.firstFailure;
          assert.equal((await readFile(join(root, 'pending-invalid-acknowledged'), 'utf8')).trim(), String(pendingOwner.pid));
          const invalidCommand = pendingOwner.firstFailure.causalCommands.find((command: {endpoint?: string}) => command.endpoint === 'mjpeg');
          assert.equal(invalidCommand.status, 0);
          assert.equal(invalidCommand.signal, null);
          assert.equal(invalidCommand.stdout.bytes, 11);
          assert.equal(invalidCommand.stderr.bytes, 0);
        }
        const pendingLifecycle = pendingOwner.diagnostics?.lifecycle || [];
        const pinned = pendingLifecycle.find((event: {event: string}) => event.event === 'runner-candidate-pinned');
        if (mode === 'listener-pending-ambiguous' || mode === 'listener-pending-command-error' || mode === 'listener-pending-invalid-pid') {
          assert.equal(pinned, undefined);
        } else {
          if (!pinned && mode === 'listener-pending-stop') {
            let diagnostic: string;
            try { diagnostic = pendingInvalidSnapshot(root, testMode, child, done); }
            catch { diagnostic = '{"collection":"unavailable"}'; }
            assert.fail(`listener-pending-stop pin not observed: ${diagnostic}`);
          }
          assert.ok(pinned);
          assert.match(String(pinned.detail?.pid), /^[1-9]\d{0,9}$/u);
          if (!pendingOwner.firstFailure) assert.equal(pendingOwner.listenerEvidence.some((entry: {pid: string}) => entry.pid === pinned.detail.pid), true);
        }
        const observations = (await readFile(join(root, 'pending-observations'), 'utf8')).trim().split(/\r?\n/u);
        assert.equal(observations[0], '1/0');
        if (mode === 'listener-pending-invalid-pid') assert.equal(observations[1], '2/invalid');
        else if (mode !== 'listener-pending-ambiguous') assert.equal(observations[1], '2/1');
        assert.equal(pendingOwner.ready, false);
        assert.equal(pendingOwner.runnerPid, undefined);
        assert.equal(pendingOwner.runnerBirth, undefined);
        assert.equal(pendingOwner.runnerExecutable, undefined);
        assert.equal(pendingOwner.udid, udid);
        assert.equal(pendingOwner.product, await realpath(product));
        assert.equal(pendingOwner.cachedProductExecutableHash, initialProductHash);
        assert.equal(pendingOwner.xctestrun, await realpath(xctestrun));
        assert.equal(pendingOwner.url, `http://127.0.0.1:${port}`);
        const pendingInitialCommands = pendingOwner.diagnostics.commands.filter((command: {phase: string; operation: string}) => command.phase === 'initial' && command.operation === 'inspect-listener');
        assert.equal(pendingInitialCommands[0].port, port);
        assert.equal(pendingInitialCommands[1].port, Number(env.IOS_WDA_MJPEG_PORT));
        assert.equal(pendingLifecycle.some((event: {event: string}) => event.event === 'runner-frozen'), false);
        assert.equal(existsSync(join(root, 'status-queries')), false);
        assert.equal(await readFile(join(root, 'receipt-queries'), 'utf8'), '1');
        assert.equal((await readFile(join(root, 'launches'), 'utf8')).trim().split(/\r?\n/u).length, 1);
        if (pinned && mode !== 'listener-pending-exit') {
          const boundaryDeadline = Date.now() + 15_000;
          while (mode !== 'listener-pending-stop' && mode !== 'listener-pending-child-error' && !existsSync(join(root, 'pending-candidate-boundary')) && Date.now() < boundaryDeadline) {
            if (witness) terminalCheck(root, deadline);
            await pause();
          }
          if (witness) terminalCheck(root, deadline);
          if (mode !== 'listener-pending-stop' && mode !== 'listener-pending-child-error') assert.equal(existsSync(join(root, 'pending-candidate-boundary')), true);
          if (mode === 'listener-pending-owner-evidence-write') await mkdir(join(state, 'owner-private.next.json'));
          if (mode !== 'listener-pending-stop' && mode !== 'listener-pending-child-error') await writeFile(join(root, 'pending-candidate-continue'), 'candidate assertions complete');
        }
        if (mode === 'listener-pending-ready' || mode === 'listener-pending-http-only') {
          assert.deepEqual(observations.slice(0, 2), ['1/0', '2/1']);
          assert.ok(pendingLifecycle.some((event: {event: string}) => event.event === 'runner-candidate-pinned'));
          assert.equal(pendingLifecycle.some((event: {event: string}) => event.event === 'runner-frozen'), false);
          assert.equal(pendingOwner.listenerValidation.runnerAssociation, 'not-evaluated');
          assert.equal(pendingOwner.listenerValidation.mjpegAssociation, 'match');
          while (!done && Date.now() < pendingDeadline && !(JSON.parse(await readFile(join(state, 'owner.json'), 'utf8')).ready)) await pause();
          const readyOwner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
          assert.ok(readyOwner.ready, errors || readyOwner.error);
          const readyLifecycle = readyOwner.diagnostics?.lifecycle || [];
          const readyPinned = readyLifecycle.find((event: {id: number; event: string; detail?: {pid?: string; birth?: string; executable?: string}}) => event.event === 'runner-candidate-pinned');
          const readyFrozen = readyLifecycle.find((event: {id: number; event: string}) => event.event === 'runner-frozen');
          const readyRevalidations = readyLifecycle.filter((event: {id: number; event: string; detail?: {pid?: string; birth?: string; executable?: string}}) => event.event === 'runner-candidate-revalidated');
          assert.ok(readyPinned && readyFrozen);
          assert.ok(readyRevalidations.some((event: {id: number}) => event.id < readyFrozen.id));
          assert.equal(readyOwner.runnerPid, readyPinned.detail.pid);
          assert.equal(readyOwner.runnerBirth, readyPinned.detail.birth);
          assert.equal(readyOwner.runnerExecutable, readyPinned.detail.executable);
          for (const event of readyRevalidations) {
            assert.equal(event.detail.pid, readyPinned.detail.pid);
            assert.equal(event.detail.birth, readyPinned.detail.birth);
            assert.equal(event.detail.executable, readyPinned.detail.executable);
          }
          const readyObservations = (await readFile(join(root, 'pending-observations'), 'utf8')).trim().split(/\r?\n/u);
          assert.deepEqual(readyObservations.slice(0, 4), mode === 'listener-pending-ready' ? ['1/0', '2/1', '3/1', '4/1'] : ['1/0', '2/1', '3/1', '4/0']);
          assert.equal(await readFile(join(root, 'receipt-queries'), 'utf8'), '2');
          assert.ok(existsSync(join(root, 'status-queries')));
          await writeFile(join(state, 'stop'), 'pending candidate finalization');
        } else if (mode === 'listener-pending-stop') {
          assert.ok(pendingLifecycle.some((event: {event: string}) => event.event === 'runner-candidate-pinned'));
          await writeFile(join(state, 'stop'), 'pending candidate stop');
        } else if (mode !== 'listener-pending-ambiguous' && mode !== 'listener-pending-command-error' && mode !== 'listener-pending-invalid-pid') {
          assert.ok(pendingLifecycle.some((event: {event: string}) => event.event === 'runner-candidate-pinned'));
        }
      }
      if (mode === 'listener-ready-owner-evidence-write') {
        const boundaryDeadline = Date.now() + 15_000;
        while (!done && Date.now() < boundaryDeadline && !existsSync(join(root, 'ready-evidence-boundary'))) await pause();
        assert.equal(existsSync(join(root, 'ready-evidence-boundary')), true, errors || 'readiness boundary was not reached');
        const boundaryOwner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
        assert.equal(boundaryOwner.ready, false);
        assert.equal(boundaryOwner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'ready-admitted'), false);
        await mkdir(join(state, 'owner-private.next.json'));
        await writeFile(join(root, 'ready-evidence-continue'), 'final readiness write failure');
        const fallbackDeadline = Date.now() + 15_000;
        while (!done && Date.now() < fallbackDeadline && !existsSync(join(root, 'ready-failure-fallback-written'))) await pause();
        assert.equal(existsSync(join(root, 'ready-failure-fallback-written')), true);
        const fallbackOwner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
        assert.equal(fallbackOwner.ready, false);
        assert.equal(fallbackOwner.firstFailure.stage, 'owner-evidence-write');
        assert.equal(fallbackOwner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'ready-admitted'), false);
        await writeFile(join(root, 'ready-failure-fallback-continue'), 'fallback assertions complete');
      }
      if (mode === 'listener-concurrent-exit-before-close') {
        assert.ok(witness);
        await terminalRelease(witness, port, true);
        assert.equal(existsSync(join(root, 'child-exit-observed')), true);
        assert.equal(existsSync(join(root, 'runner.pid')), true);
        const boundaryOwner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
        assert.equal(boundaryOwner.ready, false);
        assert.equal(boundaryOwner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'ready-admitted'), false);
      }
      if (mode === 'ready' || mode === 'ready-then-oversized' || mode === 'status-first-failure' || mode === 'listener-http-only') {
        while (Date.now() < deadline && !done) {
          if (existsSync(join(state, 'owner.json')) && JSON.parse(await readFile(join(state, 'owner.json'), 'utf8')).ready) break;
          await pause();
        }
        const owner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
        assert.ok(owner.ready, errors || owner.error);
        if (mode === 'ready' || mode === 'status-first-failure' || mode === 'listener-http-only') {
          const privateOwner = JSON.parse(await readFile(join(state, 'owner-private.json'), 'utf8'));
          const standaloneStatus = JSON.parse(await readFile(join(state, 'wda-status.json'), 'utf8'));
          for (const status of [owner.status, privateOwner.status, standaloneStatus]) assertReadyStatus(status);
        }
        if (mode === 'status-first-failure') {
          assert.ok(Number(await readFile(join(root, 'status-queries'), 'utf8')) >= 2);
          assert.ok(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'status-error'));
          assert.equal(owner.diagnostics.lifecycle.some((event: {event: string; detail?: {stage?: string}}) => event.event === 'failure-observed' && event.detail?.stage === 'status-command'), false);
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
      if (!listenerEvidenceModes.includes(mode)) assert.ok(Buffer.byteLength(await readFile(join(state, 'owner-private.json'), 'utf8')) <= 1048576);
      if (mode === 'ready' || mode === 'status-first-failure' || mode === 'listener-http-only') {
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
      if (listenerEvidenceModes.includes(mode)) {
        assert.equal(owner.runnerPid, undefined);
        assert.equal(owner.runnerBirth, undefined);
        assert.equal(owner.runnerExecutable, undefined);
        assert.equal(owner.status, undefined);
        assert.equal(existsSync(join(state, 'wda-status.json')), false);
        assert.equal(existsSync(join(root, 'status-queries')), false);
        assert.equal(lifecycle.some((event: {event: string}) => ['runner-candidate-pinned', 'runner-frozen', 'ready-admitted'].includes(event.event)), false);
        assert.equal(done, true);
        assert.equal(child.exitCode, 0);
        assert.equal(child.signalCode, null);
        assert.equal(owner.exitCode, 0);
        assert.equal(owner.signal, null);
        const signals = lifecycle.filter((event: {event: string}) => event.event === 'child-signal-requested');
        assert.equal(signals.length, 1);
        const childSignal = lifecycle.find((event: {event: string; detail?: {signal?: string; accepted?: boolean}}) => event.event === 'child-signal-requested');
        const childExit = lifecycle.find((event: {event: string}) => event.event === 'child-exit');
        const childClose = lifecycle.find((event: {event: string}) => event.event === 'child-close');
        const childStopFinished = lifecycle.find((event: {event: string}) => event.event === 'child-stop-finished');
        assert.ok(childSignal && childExit && childClose && childStopFinished);
        assert.equal(childSignal.detail?.signal, 'SIGTERM');
        assert.equal(childSignal.detail?.accepted, true);
        assert.ok(childSignal.id < childExit.id && childExit.id < childClose.id && childClose.id < childStopFinished.id && childStopFinished.id < endedEntry.id);
        assert.equal(childStopFinished.detail?.childEnded, true);
        for (const event of [childSignal, childExit, childClose, childStopFinished]) assert.equal(event.detail?.pid, owner.pid);
        assert.equal(childExit.detail?.exitCode, 0);
        assert.equal(childExit.detail?.signal, null);
        assert.equal(childClose.detail?.exitCode, 0);
        assert.equal(childClose.detail?.signal, null);
        assert.equal(lifecycle.some((event: {event: string}) => event.event === 'owned-listener-termination-requested'), false);
      }
      if (!['ready', 'ready-then-oversized', 'status-first-failure', 'listener-http-only', 'listener-pending-ready', 'listener-pending-http-only', 'stream-framing', 'stream-framing-reversed', 'stream-utf8', 'stream-eof', 'output-below', 'output-equal', 'output-shrinking', 'output-expanding'].includes(mode)) assert.ok(owner.firstFailure);
      if (owner.firstFailure) assert.ok(Array.isArray(owner.firstFailure.causalCommands));
      if (listenerEvidenceModes.includes(mode) && captureControl !== 'withheld-capture') {
        const retainedObservation = readListenerRecord(root, 'first-failure-observation.json', 65_536) as {
          capturedBeforeOwnerEnded: boolean;
          firstFailure: Record<string, unknown>;
          listenerValidation?: Record<string, unknown>;
          failureEvent: {detail?: {first?: boolean; frozenOwner?: boolean}};
        };
        assert.equal(retainedObservation.capturedBeforeOwnerEnded, true);
        const firstFailureSnapshot = retainedObservation.firstFailure;
        const failureEvents = lifecycle.filter((event: { event: string }) => event.event === 'failure-observed');
        const firstFailureEvents = failureEvents.filter((event: { detail?: { first?: boolean; frozenOwner?: boolean } }) => event.detail?.first === true);
        assert.equal(firstFailureEvents.length, 1);
        assert.ok(failureEvents.every((event: { detail?: { first?: boolean; frozenOwner?: boolean } }) => event.detail?.first === true || event.detail?.first === false));
        assert.equal(firstFailureEvents[0].detail?.frozenOwner, false);
        assert.deepEqual(firstFailureEvents[0], retainedObservation.failureEvent);
        assert.equal(retainedObservation.failureEvent.detail?.first, true);
        assert.equal(retainedObservation.failureEvent.detail?.frozenOwner, false);
        assert.equal(firstFailureSnapshot.frozenOwner, false);
        assert.deepEqual(owner.firstFailure, firstFailureSnapshot);
        assert.deepEqual(owner.listenerValidation, retainedObservation.listenerValidation);
        assert.equal(owner.runnerPid, undefined);
        assert.equal(owner.runnerBirth, undefined);
        assert.equal(owner.runnerExecutable, undefined);
        assert.equal(owner.status, undefined);
        assert.equal(existsSync(join(state, 'wda-status.json')), false);
        assert.equal(owner.ready, false);
        assert.equal(existsSync(join(root, 'runner.pid')), false);
        assert.equal(existsSync(join(root, 'xcode.pid')), false);
      }
      if (captureControl) {
        assert.ok(heldFirstFailure);
        assert.deepEqual(owner.firstFailure, heldFirstFailure.firstFailure);
        assert.deepEqual(owner.listenerValidation, heldFirstFailure.listenerValidation);
        assert.deepEqual(lifecycle.filter((event: {event: string; detail?: {first?: boolean}}) => event.event === 'failure-observed' && event.detail?.first === true), [heldFirstFailure.failureEvent]);
        assert.equal(existsSync(join(root, 'runner.pid')), false);
        assert.equal(existsSync(join(root, 'xcode.pid')), false);
        if (captureControl === 'withheld-capture') {
          assert.ok(withheldObservationFailure instanceof assert.AssertionError);
          assert.equal(existsSync(join(root, 'first-failure-observation.json')), false);
          assert.deepEqual(readListenerRecord(root, 'listener-capture-error.json', 512), {schema: 1, reason: 'WITHHELD'});
        }
      }
      if (mode === 'listener-uncoordinated-negative') {
        for (const marker of ['listener-publication-release', 'listener-publication-ack', 'endpoint-ready']) assert.equal(existsSync(join(root, marker)), false);
        assert.equal(owner.firstFailure.stage, 'mjpeg-listener-command');
        assert.equal(owner.firstFailure.category, 'listener-command-error');
        assert.equal(owner.firstFailure.frozenOwner, false);
        assert.equal(owner.firstFailure.predicate.listenerValidation.failureStage, 'mjpeg-listener-command');
        assert.equal(owner.firstFailure.predicate.listenerValidation.errorCategory, 'listener-command-error');
        assert.deepEqual(owner.firstFailure.predicate.listenerValidation.endpoints.wda, {status: 'evaluated', count: 0, pids: []});
        assert.deepEqual(owner.firstFailure.predicate.listenerValidation.endpoints.mjpeg, {status: 'error', errorCategory: 'command-error'});
        assert.deepEqual(owner.listenerValidation.endpoints.wda, {status: 'evaluated', count: 0, pids: []});
        assert.deepEqual(owner.listenerValidation.endpoints.mjpeg, {status: 'error', errorCategory: 'command-error'});
        assert.equal(owner.listenerValidation.runnerAssociation, 'not-evaluated');
        assert.equal(owner.listenerValidation.mjpegAssociation, 'not-evaluated');
      }
      if (initialRaceModes.includes(mode)) {
        assert.deepEqual(owner.firstFailure, initialRaceFirstFailure);
        const diagnostic = owner.initialCandidateDiagnostic;
        const cleanup = owner.cleanupListenerObservation;
        const finalPrivateOwner = JSON.parse(await readFile(join(state, 'owner-private.json'), 'utf8'));
        const finalDiagnosticText = JSON.stringify(finalPrivateOwner.initialCandidateDiagnostic);
        assert.equal(finalDiagnosticText.includes('http://'), false);
        assert.equal(finalDiagnosticText.includes('/tmp/'), false);
        assert.equal(finalDiagnosticText.includes('/private/'), false);
        if (mode === 'listener-initial-race') await assertSanitizedExports(root, state, [], ['owner.json']);
        assert.ok(diagnostic);
        assert.ok(cleanup);
        assert.ok(diagnostic.startedAtMonotonicMs >= cleanup.monotonicMs);
        assert.equal(diagnostic.candidatePid, owner.listenerEvidence.find((entry: {pid: string}) => entry.pid === diagnostic.candidatePid)?.pid);
        const failureListener = owner.firstFailure.predicate.listenerValidation.endpoints;
        assert.equal(failureListener.wda.count, 0);
        assert.equal(failureListener.mjpeg.count, 1);
        if (['listener-initial-race-receipt', 'listener-initial-race-receipt-command-error', 'listener-initial-race-receipt-hash-error'].includes(mode)) {
          const wdaCommand = owner.diagnostics.commands.find((command: {phase: string; endpoint?: string; operation: string; status: number | null; stderr?: {bytes?: number}}) =>
            command.phase === 'initial' && command.endpoint === 'wda' && command.operation === 'inspect-listener' && (command.stderr?.bytes || 0) > 0);
          assert.equal(wdaCommand?.status, 1);
          assert.ok((wdaCommand?.stderr?.bytes || 0) > 0);
          assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'runner-candidate-pinned'), false);
          assert.equal(owner.diagnostics.commands.some((command: {phase: string; operation: string}) => command.phase === 'after-initial' && command.operation === 'read-install-receipt'), false);
          assert.equal(existsSync(join(root, 'initial-race-receipt-fatal')), true);
          assert.equal(existsSync(join(root, 'initial-race-receipt-http-observed')), true);
          assert.equal(existsSync(join(root, 'initial-race-receipt-publication-release')), true);
          assert.equal(existsSync(join(root, 'initial-race-receipt-publication-ack')), true);
          assert.equal(existsSync(join(root, 'initial-race-receipt-listener-ready')), true);
          const receiptPairTrace = (await readFile(join(root, 'initial-race-receipt-pair-trace'), 'utf8')).trim().split(/\r?\n/u);
          assert.equal(receiptPairTrace[0], 'http 1 EMPTY');
          assert.equal(receiptPairTrace[1], 'mjpeg-entry 1');
          assert.equal(receiptPairTrace[2], 'mjpeg-ack 1');
          assert.match(receiptPairTrace[3], /^mjpeg 1 [1-9]\d*$/u);
          assert.deepEqual((await readFile(join(root, 'initial-race-receipt-publication-trace'), 'utf8')).trim().split(/\r?\n/u), ['runner-pid-written', 'listener-ready', 'publication-ack']);
          assert.match(await readFile(join(root, 'command-trace'), 'utf8'), /start \d+ lsof[\s\S]*\[-iTCP:/u);
        }
        assert.deepEqual(cleanup.endpoints.wda.observation, {status: 'evaluated', count: 1, pids: [diagnostic.candidatePid]});
        assert.deepEqual(cleanup.endpoints.mjpeg.observation, {status: 'evaluated', count: 1, pids: [diagnostic.candidatePid]});
        assert.equal(diagnostic.cleanup.status, 'evaluated');
        assert.deepEqual(diagnostic.cleanup.wda, {status: 'evaluated', matches: true});
        assert.deepEqual(diagnostic.cleanup.mjpeg, {status: 'evaluated', matches: true});
        assert.deepEqual(cleanup.commandIds, [cleanup.endpoints.wda.commandId, cleanup.endpoints.mjpeg.commandId]);
        assert.ok(cleanup.commandIds.every((id: number) => owner.diagnostics.commands.some((command: {id: number; phase: string; operation: string}) => command.id === id && command.phase === 'cleanup' && command.operation === 'inspect-listener')));
        assert.ok(Buffer.byteLength(await readFile(join(state, 'owner.json'), 'utf8')) <= 1048576);
        assert.ok(Buffer.byteLength(await readFile(join(state, 'owner-private.json'), 'utf8')) <= 1048576);
        assert.equal(JSON.stringify(diagnostic).includes('http://'), false);
        assert.equal(JSON.stringify(diagnostic).includes('/tmp/'), false);
        const evaluatedChecks = [
          diagnostic.initial.status,
          diagnostic.current.status,
          diagnostic.comparisons.pid.status,
          diagnostic.comparisons.birth.status,
          diagnostic.comparisons.executable.status,
          diagnostic.comparisons.pathShape.applicationNameMatches.status,
          diagnostic.comparisons.pathShape.containerUuid.status,
          diagnostic.comparisons.pathShape.simulatorPath.status,
          diagnostic.comparisons.pathShape.executableName.status,
          diagnostic.comparisons.runnerBundleId.status,
          diagnostic.comparisons.runnerExecutableHash.status,
          diagnostic.comparisons.productExecutableHash.status,
          diagnostic.comparisons.refreshedReceipt.status,
          diagnostic.comparisons.refreshedReceipt.sameAsInstallReceipt.status,
          diagnostic.comparisons.refreshedReceipt.applicationPath.status,
          diagnostic.comparisons.refreshedReceipt.bundleId.status,
          diagnostic.comparisons.refreshedReceipt.executableHash.status,
          diagnostic.cleanup.status,
          diagnostic.cleanup.wda.status,
          diagnostic.cleanup.mjpeg.status,
        ];
        if (mode === 'listener-initial-race') for (const status of evaluatedChecks) assert.equal(status, 'evaluated');
        if (mode === 'listener-initial-race-birth') assert.equal(diagnostic.comparisons.birth.status, 'rejected');
        if (mode === 'listener-initial-race-executable') {
          assert.equal(diagnostic.comparisons.executable.status, 'rejected');
          assert.equal(diagnostic.comparisons.pathShape.executableName.status, 'rejected');
        }
        if (mode === 'listener-initial-race-hash') {
          assert.equal(diagnostic.comparisons.runnerExecutableHash.status, 'rejected');
          assert.equal(diagnostic.comparisons.runnerExecutableHash.matches, false);
        }
        if (mode === 'listener-initial-race-product') {
          assert.equal(diagnostic.comparisons.productExecutableHash.status, 'rejected');
          assert.equal(diagnostic.comparisons.refreshedReceipt.status, 'rejected');
        }
        if (mode === 'listener-initial-race-receipt') {
          assert.equal(diagnostic.comparisons.refreshedReceipt.sameAsInstallReceipt.status, 'rejected');
          assert.equal(diagnostic.comparisons.refreshedReceipt.status, 'evaluated');
        }
        if (mode === 'listener-initial-race-receipt-command-error') {
          assert.equal(diagnostic.comparisons.refreshedReceipt.applicationPath.status, 'evaluated');
          assert.equal(diagnostic.comparisons.refreshedReceipt.bundleId.status, 'rejected');
          assert.equal(diagnostic.comparisons.refreshedReceipt.executableHash.status, 'not-evaluated');
        }
        if (mode === 'listener-initial-race-receipt-hash-error') {
          assert.equal(diagnostic.comparisons.refreshedReceipt.applicationPath.status, 'evaluated');
          assert.equal(diagnostic.comparisons.refreshedReceipt.bundleId.status, 'evaluated');
          assert.equal(diagnostic.comparisons.refreshedReceipt.executableHash.status, 'evaluated');
          assert.equal(diagnostic.comparisons.refreshedReceipt.status, 'rejected');
        }
        if (mode === 'listener-initial-race-command-error') {
          assert.equal(diagnostic.current.status, 'rejected');
          assert.equal(diagnostic.current.category, 'process-evidence-command-error');
          assert.equal(diagnostic.current.executable.status, 'not-evaluated');
          assert.equal(diagnostic.comparisons.pid.status, 'not-evaluated');
          assert.equal(diagnostic.comparisons.birth.status, 'not-evaluated');
          assert.equal(diagnostic.comparisons.executable.status, 'not-evaluated');
          assert.equal(diagnostic.comparisons.pathShape.applicationNameMatches.status, 'not-evaluated');
        }
        if (mode === 'listener-initial-race-command-error-late') {
          assert.equal(diagnostic.current.status, 'rejected');
          assert.equal(diagnostic.current.executable.status, 'evaluated');
          assert.equal(diagnostic.comparisons.executable.status, 'evaluated');
          assert.equal(diagnostic.comparisons.birth.status, 'not-evaluated');
        }
        if (mode === 'listener-initial-race-budget') {
          assert.equal(diagnostic.current.status, 'budget-exhausted');
          assert.equal(diagnostic.comparisons.pathShape.applicationNameMatches.status, 'budget-exhausted');
          assert.equal(diagnostic.comparisons.refreshedReceipt.status, 'budget-exhausted');
        }
        if (mode === 'listener-initial-race-hash-budget') {
          assert.equal(diagnostic.comparisons.runnerExecutableHash.status, 'budget-exhausted');
          assert.equal(diagnostic.comparisons.productExecutableHash.status, 'budget-exhausted');
          assert.equal(diagnostic.comparisons.refreshedReceipt.status, 'budget-exhausted');
        }
        if (mode === 'listener-initial-race-cleanup-identity') {
          assert.equal(diagnostic.cleanup.wda.matches, true);
          assert.equal(diagnostic.cleanup.mjpeg.matches, true);
          assert.equal(diagnostic.comparisons.birth.status, 'rejected');
          assert.equal(diagnostic.comparisons.executable.status, 'rejected');
          assert.equal(diagnostic.comparisons.pathShape.executableName.status, 'rejected');
        }
        if (mode === 'listener-initial-race-hash-budget') {
          assert.ok(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'child-stop-finished'));
          assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'owned-listener-termination-requested'), false);
        }
        assert.equal(owner.receipt, await realpath(receipt));
        assert.equal(owner.installReceipt, await realpath(receipt));
        assert.equal(await readFile(join(root, 'receipt-queries'), 'utf8'), mode === 'listener-initial-race-budget' || mode === 'listener-initial-race-hash-budget' ? '1' : '2');
      }
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
      if (mode === 'listener-first-failure' || mode === 'listener-second-failure') {
        const pairTrace = (await readFile(join(root, 'listener-pair-trace'), 'utf8')).trim().split(/\r?\n/u);
        const runnerPid = String(owner.pid);
        const initialPairTrace = [
          'http 1 EMPTY',
          'mjpeg-entry 1',
          'mjpeg 1 EMPTY',
          'http 2 EMPTY',
          'mjpeg-entry 2',
          'mjpeg-ack 2',
          'mjpeg 2 EMPTY',
        ];
        const expectedPairTrace = mode === 'listener-first-failure'
          ? [...initialPairTrace, `http 3 ${runnerPid}`, `http 4 ${runnerPid}`]
          : [...initialPairTrace, `http 3 ${runnerPid}`, 'mjpeg-entry 3', 'mjpeg-ack 3', `mjpeg 3 ${runnerPid}`, `http 4 ${runnerPid}`, 'mjpeg-entry 4', 'mjpeg-ack 4', `mjpeg 4 ${runnerPid}`];
        assert.deepEqual(pairTrace, expectedPairTrace);
        assert.deepEqual((await readFile(join(root, 'listener-publication-trace'), 'utf8')).trim().split(/\r?\n/u), ['runner-pid-written', 'publication-ack', 'endpoint-ready']);
        assert.equal(existsSync(join(root, 'listener-publication-release')), true);
        assert.equal(existsSync(join(root, 'listener-publication-ack')), true);
        assert.equal(existsSync(join(root, 'endpoint-ready')), true);
        const preflightCommands = owner.diagnostics.commands.filter((command: {phase: string; operation: string}) => command.phase === 'preflight' && command.operation === 'inspect-listener');
        assert.equal(preflightCommands.length, 2);
        assert.deepEqual(preflightCommands.map((command: {endpoint: string; status: number}) => [command.endpoint, command.status]), [['wda', 1], ['mjpeg', 1]]);
        assert.ok(preflightCommands.every((command: {stderr?: {bytes?: number}}) => command.stderr?.bytes === 0));
        const initialCommands = owner.diagnostics.commands.filter((command: {phase: string; operation: string}) => command.phase === 'initial' && command.operation === 'inspect-listener');
        const expectedInitialCommands = mode === 'listener-first-failure' ? [['wda', 1], ['mjpeg', 1], ['wda', 2]] : [['wda', 1], ['mjpeg', 1], ['wda', 0], ['mjpeg', 2]];
        assert.deepEqual(initialCommands.map((command: {endpoint: string; status: number}) => [command.endpoint, command.status]), expectedInitialCommands);
        assert.equal(owner.firstFailure.phase, 'initial');
        assert.equal(owner.firstFailure.frozenOwner, false);
        assert.equal(owner.firstFailure.stage, mode === 'listener-first-failure' ? 'wda-listener-command' : 'mjpeg-listener-command');
        assert.equal(owner.ready, false);
        assert.equal(owner.runnerPid, undefined);
        assert.equal(owner.runnerBirth, undefined);
        assert.equal(owner.runnerExecutable, undefined);
        assert.equal(owner.status, undefined);
        assert.equal(existsSync(join(state, 'wda-status.json')), false);
        assert.equal(existsSync(join(root, 'status-queries')), false);
        assert.equal(await readFile(join(root, 'receipt-queries'), 'utf8'), '1');
        assert.equal(owner.diagnostics.commands.some((command: {phase: string; operation: string}) => command.phase === 'after-initial' && command.operation === 'read-install-receipt'), false);
        assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'runner-frozen'), false);
        assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'ready-admitted'), false);
      }
      if (listenerEvidenceModes.includes(mode) && captureControl !== 'withheld-capture') {
        const pairTrace = (await readFile(join(root, 'listener-pair-trace'), 'utf8')).trim().split(/\r?\n/u);
        const runnerPid = String(owner.pid);
        if (mode === 'listener-uncoordinated-negative') {
          assert.deepEqual(pairTrace, ['uncoordinated-wda-empty', `uncoordinated-publication ${runnerPid}`, `uncoordinated-mjpeg ${runnerPid}`]);
        } else {
          const initialPairTrace = [
            'http 1 EMPTY',
            'mjpeg-entry 1',
            'mjpeg 1 EMPTY',
            'http 2 EMPTY',
            'mjpeg-entry 2',
            'mjpeg-ack 2',
            'mjpeg 2 EMPTY',
          ];
          const expectedPairTrace = mode === 'listener-first-failure'
            ? [...initialPairTrace, `http 3 ${runnerPid}`, `http 4 ${runnerPid}`]
            : [...initialPairTrace, `http 3 ${runnerPid}`, 'mjpeg-entry 3', 'mjpeg-ack 3', `mjpeg 3 ${runnerPid}`, `http 4 ${runnerPid}`, 'mjpeg-entry 4', 'mjpeg-ack 4', `mjpeg 4 ${runnerPid}`];
          assert.deepEqual(pairTrace, expectedPairTrace);
          assert.deepEqual((await readFile(join(root, 'listener-publication-trace'), 'utf8')).trim().split(/\r?\n/u), ['runner-pid-written', 'publication-ack', 'endpoint-ready']);
          assert.equal(existsSync(join(root, 'listener-publication-release')), true);
          assert.equal(existsSync(join(root, 'listener-publication-ack')), true);
          assert.equal(existsSync(join(root, 'endpoint-ready')), true);
        }
        const preCleanupObservation = readListenerRecord(root, 'first-failure-observation.json', 65_536) as {
          capturedBeforeOwnerEnded: boolean;
          failureEvent: Record<string, unknown>;
          firstFailure: Record<string, unknown>;
        };
        const firstFailureUnchanged = JSON.stringify(owner.firstFailure) === JSON.stringify(preCleanupObservation.firstFailure);
        assert.equal(firstFailureUnchanged, true);
        const candidateEvidence = {
          schema: 1,
          mode,
          expectedNegative: mode === 'listener-uncoordinated-negative',
          forcedInterleaving: mode === 'listener-uncoordinated-negative',
          trace: pairTrace,
          firstFailure: {
            phase: owner.firstFailure.phase,
            stage: owner.firstFailure.stage,
            category: owner.firstFailure.category,
            frozenOwner: owner.firstFailure.frozenOwner,
            predicate: owner.firstFailure.predicate,
          },
          firstFailureObservation: {
            capturedBeforeOwnerEnded: preCleanupObservation.capturedBeforeOwnerEnded,
            failureEvent: preCleanupObservation.failureEvent,
            unchangedAfterCleanup: firstFailureUnchanged,
          },
          ...(mode === 'listener-uncoordinated-negative' ? {
            forcedPublicationComparison: {
              candidateFailureStage: 'mjpeg-listener-command',
              candidateWdaCount: 1,
              observedWdaCount: owner.listenerValidation.endpoints.wda.count,
              observedFailureStage: owner.firstFailure.stage,
              observedFailureCategory: owner.firstFailure.category,
              originalAssertion: {expectedWdaCount: 1, actualWdaCount: owner.listenerValidation.endpoints.wda.count},
            },
          } : {}),
          nonAdmission: {
            runnerPid: owner.runnerPid ?? null,
            runnerBirth: owner.runnerBirth ?? null,
            runnerExecutable: owner.runnerExecutable ?? null,
            statusPublished: existsSync(join(state, 'wda-status.json')),
            ready: owner.ready,
          },
          cleanupSettled: Boolean(owner.endedAt) && !existsSync(join(root, 'runner.pid')) && !existsSync(join(root, 'xcode.pid')),
        };
        await writeFile(join(root, 'listener-candidate-evidence.json'), `${JSON.stringify(candidateEvidence, null, 2)}\n`);
        assert.deepEqual(JSON.parse(await readFile(join(root, 'listener-candidate-evidence.json'), 'utf8')), candidateEvidence);
      }
      if (mode === 'listener-pending-invalid-pid') {
        assertInvalidFailure(owner);
        assert.deepEqual(owner.firstFailure, pendingInvalidFirstFailure);
      }
      if (['occupied', 'ownership', 'ambiguous', 'product'].includes(mode)) assert.equal(existsSync(join(root, 'launches')), false);
      else {
        const commands = (await readFile(join(root, 'launches'), 'utf8')).trim().split('\n').map(line => JSON.parse(line));
        assert.deepEqual(commands, [['test-without-building', '-xctestrun', owner.xctestrun, '-destination', `id=${udid}`]]);
      }
      if (mode === 'listener-pending-invalid-pid') {
        assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'runner-candidate-pinned' || event.event === 'owned-listener-termination-requested'), false);
        for (const event of ['child-spawned', 'child-exit', 'child-close', 'child-stop-finished', 'owner-ended']) {
          assert.equal(owner.diagnostics.lifecycle.some((entry: {event: string}) => entry.event === event), true);
        }
        assert.equal(existsSync(join(root, 'xcode.pid')), false);
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
        const controlTrace = (await readFile(join(root, 'listener-control-trace'), 'utf8')).trim().split(/\r?\n/u);
        const expectedControlTrace = [
          'http EMPTY', 'mjpeg EMPTY', 'http EMPTY', 'mjpeg 1 11111',
          'http 2 11111', 'mjpeg 3 11111', 'http 4 11111', 'mjpeg 5 11111',
          mode === 'swap' ? 'http 6 22222' : 'http 6 11111',
          mode === 'swap' ? 'mjpeg 7 22222' : 'mjpeg 7 11111',
        ];
        assert.deepEqual(controlTrace.slice(0, expectedControlTrace.length), expectedControlTrace);
        assert.deepEqual((await readFile(join(root, 'listener-publication-trace'), 'utf8')).trim().split(/\r?\n/u), ['runner-pid-written', 'publication-ack', 'endpoint-ready']);
        assert.equal(existsSync(join(root, 'listener-publication-release')), true);
        assert.equal(existsSync(join(root, 'listener-publication-ack')), true);
        assert.equal(existsSync(join(root, 'endpoint-ready')), true);
        const frozen = owner.diagnostics.lifecycle.find((event: {id: number; event: string}) => event.event === 'runner-frozen');
        const changed = owner.diagnostics.lifecycle.find((event: {id: number; event: string; detail?: {stage?: string}}) => event.event === 'failure-observed' && String(event.detail?.stage).startsWith('managed-'));
        assert.ok(frozen && changed && frozen.id < changed.id);
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
        if (mode === 'credential' || mode === 'credential-binary' || mode === 'credential-binary-failure' || mode === 'listener-mjpeg-duplicate' || mode === 'listener-bundle-mismatch' || mode === 'listener-hash-mismatch') {
          const coordinatedTrace = (await readFile(join(root, 'coordinated-listener-pair-trace'), 'utf8')).trim().split(/\r?\n/u);
          assert.equal(coordinatedTrace[0], 'http-enter');
          assert.match(coordinatedTrace[1], /^http 1 [1-9]\d*$/u);
          assert.equal(coordinatedTrace[2], 'mjpeg-entry 1');
          assert.equal(coordinatedTrace[3], mode === 'listener-mjpeg-duplicate' ? 'mjpeg 1 DUPLICATE' : `mjpeg 1 ${String(owner.pid)}`);
          assert.deepEqual((await readFile(join(root, 'listener-publication-trace'), 'utf8')).trim().split(/\r?\n/u), ['runner-pid-written', 'publication-ack', 'endpoint-ready']);
          assert.equal(existsSync(join(root, 'listener-publication-release')), true);
          assert.equal(existsSync(join(root, 'listener-publication-ack')), true);
          assert.equal(existsSync(join(root, 'endpoint-ready')), true);
        }
        if (mode === 'listener-mjpeg-duplicate') {
          assert.equal(validation.endpoints.mjpeg.count, 2);
          assert.deepEqual(validation.endpoints.mjpeg.pids, ['11111', '22222']);
          assert.equal(validation.failureStage, 'mjpeg-pid-count');
          assert.equal(validation.errorCategory, 'listener-endpoint-cardinality');
          assert.deepEqual(owner.cleanupListenerObservation.endpoints.wda.observation, {status: 'evaluated', count: 1, pids: [String(owner.pid)]});
          assert.deepEqual(owner.cleanupListenerObservation.endpoints.mjpeg.observation, {status: 'evaluated', count: 2, pids: ['11111', '22222']});
          assert.equal(owner.cleanupListenerObservation.endpoints.wda.commandId !== owner.cleanupListenerObservation.endpoints.mjpeg.commandId, true);
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
          const controlTrace = (await readFile(join(root, 'listener-control-trace'), 'utf8')).trim().split(/\r?\n/u);
          assert.deepEqual(controlTrace.slice(0, 3), ['http EMPTY', 'mjpeg EMPTY', 'http INVALID']);
          assert.deepEqual((await readFile(join(root, 'listener-publication-trace'), 'utf8')).trim().split(/\r?\n/u), ['runner-pid-written', 'publication-ack', 'endpoint-ready']);
          assert.equal(existsSync(join(root, 'listener-publication-release')), false);
          assert.equal(existsSync(join(root, 'listener-publication-ack')), true);
          assert.equal(existsSync(join(root, 'endpoint-ready')), true);
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
      if (pendingModes.includes(mode) && mode !== 'listener-pending-ready' && mode !== 'listener-pending-http-only') {
        const pendingFailureStages: Record<string, string> = {
          'listener-pending-deadline': 'startup-deadline',
          'listener-pending-invalid-pid': 'mjpeg-listener-command',
          'listener-pending-ambiguous': 'pending-mjpeg-pid-count',
          'listener-pending-pid': 'pending-mjpeg-runner-evidence',
          'listener-pending-http-ambiguous': 'pending-wda-pid-count',
          'listener-pending-http-swap': 'pending-runner-pid',
          'listener-pending-executable': 'pending-runner-executable',
          'listener-pending-birth': 'pending-runner-birth',
          'listener-pending-command-error': 'pending-mjpeg-executable-evidence',
          'listener-pending-disappear': 'pending-mjpeg-pid-count',
          'listener-pending-product': 'pending-product-hash',
          'listener-pending-xctestrun': 'pending-xctestrun-hash',
          'listener-pending-ownership': 'pending-simulator-ownership',
          'listener-pending-hash': 'pending-runner-executable-hash',
          'listener-pending-bundle': 'pending-runner-bundle-id',
          'listener-pending-simulator': 'pending-runner-executable',
          'listener-pending-receipt': 'registration-receipt',
          'listener-pending-exit': 'child-exit',
          'listener-pending-stop': 'supervisor-error',
          'listener-concurrent-exit-before-close': 'child-exit',
          'listener-pending-child-error': 'child-process',
          'listener-pending-output-limit': 'startup-log-limit',
          'listener-pending-owner-evidence-write': 'owner-evidence-write',
        };
        assert.equal(owner.firstFailure.stage, pendingFailureStages[mode]);
        const pendingFailureCategories: Record<string, string> = {
          'listener-pending-deadline': 'startup-deadline',
          'listener-pending-invalid-pid': 'listener-invalid-process-id',
          'listener-pending-ambiguous': 'listener-endpoint-cardinality',
          'listener-pending-pid': 'listener-process-evidence',
          'listener-pending-http-ambiguous': 'listener-endpoint-cardinality',
          'listener-pending-http-swap': 'managed-listener-identity-mismatch',
          'listener-pending-executable': 'managed-listener-identity-mismatch',
          'listener-pending-birth': 'managed-listener-identity-mismatch',
          'listener-pending-command-error': 'listener-process-evidence',
          'listener-pending-disappear': 'listener-endpoint-cardinality',
          'listener-pending-product': 'product-identity-mismatch',
          'listener-pending-xctestrun': 'xctestrun-identity-mismatch',
          'listener-pending-ownership': 'simulator-ownership-mismatch',
          'listener-pending-hash': 'runner-hash-mismatch',
          'listener-pending-bundle': 'runner-bundle-mismatch',
          'listener-pending-simulator': 'managed-listener-identity-mismatch',
        };
        const pendingFailureCategory = pendingFailureCategories[mode];
        if (pendingFailureCategory) {
          assert.equal(owner.firstFailure.category, pendingFailureCategory);
          assert.equal(owner.firstFailure.frozenOwner, false);
          assert.equal(owner.listenerValidation.errorCategory, pendingFailureCategory);
          assert.equal(owner.listenerValidation.failureStage, pendingFailureStages[mode]);
          assert.equal(owner.ready, false);
          assert.equal(owner.runnerPid, undefined);
          assert.equal(owner.runnerBirth, undefined);
          assert.equal(owner.runnerExecutable, undefined);
          assert.equal(owner.status, undefined);
          assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'runner-frozen'), false);
          assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'ready-admitted'), false);
          assert.equal(existsSync(join(state, 'wda-status.json')), false);
          assert.equal(existsSync(join(root, 'status-queries')), false);
        }
        if (mode === 'listener-pending-receipt') {
          assert.equal(owner.firstFailure.phase, 'after-initial');
          assert.equal(owner.firstFailure.category, 'registration-receipt-validation');
          assert.equal(owner.receipt, await realpath(join(root, 'invalid-receipt')));
          assert.equal(owner.installReceipt, await realpath(receipt));
          assert.equal(await readFile(join(root, 'receipt-queries'), 'utf8'), '3');
          assert.equal(owner.listenerValidation.failureStage, 'validated');
          assert.equal(owner.listenerValidation.errorCategory, 'none');
          assert.equal(owner.firstFailure.frozenOwner, false);
          assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'runner-frozen'), false);
          assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'ready-admitted'), false);
          assert.equal(existsSync(join(state, 'wda-status.json')), false);
          assert.equal(existsSync(join(root, 'status-queries')), false);
        }
        if (mode === 'listener-pending-deadline') {
          const deadline = Number(await readFile(join(state, 'deadline'), 'utf8'));
          const timedOut = owner.diagnostics.commands.find((command: { operation: string; endpoint?: string; error?: { category?: string; code?: string }; status: number | null }) =>
            command.operation === 'inspect-listener' && command.endpoint === 'wda' && command.error?.category === 'timeout');
          assert.ok(timedOut);
          assert.equal(timedOut.status, null);
          assert.equal(timedOut.error.code, 'ETIMEDOUT');
          assert.ok(Date.parse(timedOut.startedAt) < deadline);
          const timedOutStart = Date.parse(timedOut.startedAt);
          const timedOutEnd = Date.parse(timedOut.endedAt);
          assert.ok(timedOutEnd >= deadline);
          assert.ok(timedOutEnd <= deadline + 1_000);
          assert.ok(timedOut.timeoutMs <= deadline - timedOutStart + 25);
          assert.ok(timedOut.durationMs > 0);
          assert.ok(timedOut.durationMs <= timedOut.timeoutMs + 1_000);
          assert.equal(owner.firstFailure.category, 'startup-deadline');
          assert.equal(owner.firstFailure.message, 'XCTEST: startup deadline');
          assert.equal(owner.listenerValidation.failureStage, 'startup-deadline');
          assert.equal(owner.listenerValidation.errorCategory, 'startup-deadline');
          assert.ok(owner.firstFailure.commandIds.includes(timedOut.id));
          const firstFailure = owner.diagnostics.lifecycle.find((event: { event: string; detail?: { first?: boolean; stage?: string } }) =>
            event.event === 'failure-observed' && event.detail?.first === true);
          assert.equal(firstFailure?.detail?.stage, 'startup-deadline');
          assert.ok(Number(await readFile(join(root, 'pending-wda-count'), 'utf8')) >= 3);
          assert.match(await readFile(join(root, 'command-trace'), 'utf8'), /start \d+ lsof[\s\S]*-iTCP:/u);
          assert.match(await readFile(join(root, 'command-trace'), 'utf8'), /end \d+ lsof/u);
        }
        if (mode === 'listener-pending-command-error') {
          const commandFailure = owner.diagnostics.commands.find((command: { operation: string; status: number | null; error?: { category?: string } }) =>
            command.operation === 'inspect-process-executable' && command.status === 23);
          assert.ok(commandFailure);
          assert.equal(owner.firstFailure.category, 'listener-process-evidence');
          assert.ok(owner.firstFailure.commandIds.includes(commandFailure.id));
          assert.notEqual(owner.firstFailure.category, 'startup-deadline');
          assert.match(await readFile(join(root, 'command-trace'), 'utf8'), /start \d+ lsof[\s\S]*\[-d\] \[txt\]/u);
          assert.match(await readFile(join(root, 'command-trace'), 'utf8'), /end \d+ lsof/u);
        }
        if (['listener-pending-child-error', 'listener-pending-output-limit', 'listener-pending-owner-evidence-write'].includes(mode)) {
          assert.equal(owner.ready, false);
          assert.equal(owner.runnerPid, undefined);
          assert.equal(owner.runnerBirth, undefined);
          assert.equal(owner.runnerExecutable, undefined);
          assert.equal(owner.status, undefined);
          assert.equal(existsSync(join(state, 'wda-status.json')), false);
          assert.equal(existsSync(join(root, 'status-queries')), false);
          assert.equal(await readFile(join(root, 'receipt-queries'), 'utf8'), mode === 'listener-pending-owner-evidence-write' ? '2' : '1');
          if (mode === 'listener-pending-owner-evidence-write') assert.equal(owner.diagnostics.commands.some((command: {phase: string; operation: string}) => command.phase === 'after-initial' && command.operation === 'read-install-receipt'), false);
          assert.equal(owner.firstFailure.frozenOwner, false);
          assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'runner-frozen'), false);
          assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'ready-admitted'), false);
        }
        if (mode === 'listener-pending-output-limit') {
          assert.ok(owner.receivedLogBytes > 104857600);
          assert.equal(owner.firstFailure.stage, 'startup-log-limit');
          assert.ok(owner.startupOutputLimit);
          assert.ok(owner.startupOutputLimit.receivedBytes > 104857600);
        }
        if (mode === 'listener-pending-owner-evidence-write') {
          assert.equal(owner.firstFailure.stage, 'owner-evidence-write');
          assert.equal(owner.error, 'XCTEST: owner-evidence-write');
        }
        if (mode === 'listener-pending-child-error') {
          assert.equal(owner.firstFailure.stage, 'child-process');
          assert.equal(owner.firstFailure.category, 'child-process-error');
          const childError = owner.diagnostics.lifecycle.find((event: {id: number; event: string; detail?: {childExited?: boolean}}) => event.event === 'child-error');
          const childSignal = owner.diagnostics.lifecycle.find((event: {id: number; event: string; detail?: {signal?: string; accepted?: boolean}}) => event.event === 'child-signal-requested');
          const childExit = owner.diagnostics.lifecycle.find((event: {id: number; event: string}) => event.event === 'child-exit');
          const childClose = owner.diagnostics.lifecycle.find((event: {id: number; event: string}) => event.event === 'child-close');
          assert.ok(childError && childSignal && childExit && childClose);
          assert.equal(childError.detail?.childExited, false);
          assert.ok(childError.id < childSignal.id && childSignal.id < childExit.id && childExit.id < childClose.id);
          assert.equal(childSignal.detail?.signal, 'SIGTERM');
          assert.equal(childSignal.detail?.accepted, true);
          assert.equal(existsSync(join(root, 'child-cleanup-requested')), true);
        }
      }
      if (mode === 'listener-concurrent-exit-before-close') {
        const childStates = owner.diagnostics.commands.filter((command: {operation: string; stdout?: {content?: string; bytes?: number; suppressed?: boolean}; status: number | null}) => command.operation === 'inspect-child-state');
        const childState = childStates.at(-1);
        assert.ok(childState);
        assert.equal(childState.status, 0);
        assert.equal(childState.stdout?.bytes, 2);
        assert.equal(childState.stdout?.suppressed, true);
        assert.equal(await readFile(join(root, 'child-state-probe'), 'utf8'), 'S');
        const childExit = owner.diagnostics.lifecycle.find((event: {id: number; event: string; detail?: {exitCode?: number; frozenOwner?: boolean}}) => event.event === 'child-exit');
        const childClose = owner.diagnostics.lifecycle.find((event: {id: number; event: string}) => event.event === 'child-close');
        assert.ok(childExit && childClose && childExit.id < childClose.id);
        assert.equal(childExit.detail?.exitCode, 46);
        assert.equal(childExit.detail?.frozenOwner, true);
        assert.equal(owner.ready, false);
        assert.equal(owner.diagnostics.lifecycle.some((event: {event: string}) => event.event === 'ready-admitted'), false);
      }
      if (witness) {
        const command = owner.diagnostics.commands.filter((entry: {operation: string}) => entry.operation === 'inspect-child-state').at(-1);
        assert.ok(command.timeoutMs > 0 && command.timeoutMs <= 2000);
        assert.equal(command.error, undefined);
        assert.equal(command.signal, null);
        assert.equal(owner.exitCode, 46);
        assert.equal(owner.signal, null);
        terminalWrite(root, 'callback.json', {code: owner.exitCode, signal: owner.signal, release: terminalHash(terminalRead(root, 'child-exit-observed'))});
      }
      if (revocationModes.includes(mode)) {
        assert.equal(child.exitCode, 0, 'event listener must not cause an uncaught exception');
        assert.equal(errors, '');
        assert.equal(existsSync(join(root, 'exit-callback-timeout')), false);
        assert.equal(existsSync(join(root, 'exit-promise-resolved')), true);
        assert.equal(owner.exitCode, 46);
        assert.equal(owner.signal, null);
        if (revocationFirstFailure) {
          assert.deepEqual(owner.firstFailure, revocationFirstFailure);
          assert.equal(owner.error, revocationError);
        } else assert.equal(owner.firstFailure.stage, 'child-exit');
      }
      if (mode === 'ready' || mode === 'status-first-failure' || mode === 'listener-http-only') assert.equal(owner.error, undefined);
      if (mode !== 'ownership' && mode !== 'listener-pending-ownership') assert.match(await readFile(join(state, 'ios-wda-system.log'), 'utf8'), /output":"suppressed"/u);
      assert.equal(existsSync(join(root, 'runner.pid')), false);
      assertionsPassed = true;
    };
    try {
      await runAssertions();
    } catch (error) {
      if (listenerEvidenceModes.includes(mode)) {
        listenerFailure = {error};
      } else if (mode === 'listener-pending-invalid-pid') {
        pendingInvalidFailure = {error, diagnostic: 'unavailable'};
      } else {
        if (witnessBuild) { witnessFailure = error; terminalFailure(root); }
        throw error;
      }
    } finally {
      if (listenerEvidenceModes.includes(mode)) {
        try { listenerViews.preFinalizer = listenerOwnerProjection(readListenerRecord(root, 'state/owner.json', 1_048_576), child.pid); }
        catch { listenerViews.preFinalizer = {unavailable: true}; }
        if (captureControl) {
          for (const name of ['listener-capture-abort', 'listener-capture-decision.json']) {
            try {
              if (!existsSync(join(root, name))) writeFileSync(join(root, name), JSON.stringify({schema: 1, decision: 'CANCELLED'}), {flag: 'wx', mode: 0o600});
            } catch (error) { cleanupFailure ??= {error}; }
          }
        }
        if (!done) {
          try { await writeFile(join(state, 'stop'), 'failed test cleanup'); } catch (error) { cleanupFailure ??= {error}; }
          try {
            if (!child.kill('SIGTERM') && child.exitCode === null && child.signalCode === null) cleanupFailure ??= {error: new Error('listener fixture supervisor signal not accepted')};
          } catch (error) { cleanupFailure ??= {error}; }
          let timer: ReturnType<typeof setTimeout> | undefined;
          try {
            await Promise.race([exited, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('listener fixture cleanup deadline')), 15_000); })]);
          } catch (error) { cleanupFailure ??= {error}; }
          finally { clearTimeout(timer); }
        }
        try { listenerViews.settledFinal = done ? listenerOwnerProjection(readListenerRecord(root, 'state/owner.json', 1_048_576), child.pid) : {unavailable: true, closed: false}; }
        catch { listenerViews.settledFinal = {unavailable: true, closed: done}; }
      }
      if (mode === 'listener-pending-invalid-pid') {
        for (const name of ['pending-invalid-abort', 'pending-invalid-release', 'pending-candidate-continue', 'state/stop']) {
          try { await writeFile(join(root, name), 'fixture finalization'); } catch (error) { cleanupFailure ??= {error}; }
        }
        if (!done) {
          try {
            if (!child.kill('SIGTERM') && child.exitCode === null && child.signalCode === null) {
              cleanupFailure ??= {error: new Error('pending invalid fixture supervisor signal not accepted')};
            }
          } catch (error) { cleanupFailure ??= {error}; }
          let timer: ReturnType<typeof setTimeout> | undefined;
          try {
            await Promise.race([exited, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('pending invalid fixture cleanup deadline')), 15_000); })]);
          } catch (error) { cleanupFailure ??= {error}; }
          finally { clearTimeout(timer); }
        }
      }
      if (witnessBuild) {
        try { await terminalTeardown(root, witness, witnessSettlement!, !assertionsPassed); }
        catch (error) { if (!witnessFailure) witnessFailure = error; }
        try { await terminalExport(root, mode, witnessBuild); }
        catch (error) { if (!witnessFailure) witnessFailure = error; }
      }
      if (revocationModes.includes(mode)) {
        // Always release both test latches; ordinary supervisor cleanup owns
        // descendant termination. No copied private owner on pass OR failure.
        for (const name of ['exit-first-failure-request', 'child-exit-continue', 'exit-callback-continue']) {
          await writeFile(join(root, name), 'fixture finalization');
        }
        for (const name of ['owner-private.next.json', 'owner.next.json']) {
          await rm(join(state, name), {recursive: true, force: true});
        }
      }
      if (!done && !witnessBuild && mode !== 'listener-pending-invalid-pid' && !listenerEvidenceModes.includes(mode)) {
        if (mode === 'listener-ready-owner-evidence-write') await writeFile(join(root, 'ready-failure-fallback-continue'), 'failed test cleanup');
        await writeFile(join(state, 'stop'), 'failed test cleanup');
        child.kill('SIGTERM');
        await exited;
      }
      const diagnosticRootBase = process.env.XCTEST_DIAGNOSTIC_ROOT;
      if (diagnosticRootBase && revocationModes.includes(mode)) {
        const diagnosticRoot = join(diagnosticRootBase, mode);
        await mkdir(diagnosticRoot, {recursive: true});
        for (const name of ['revocation-source-binding.json', 'revocation-receipt.json', 'exit-callback-returned', 'exit-callback-timeout', 'exit-promise-resolved']) {
          if (existsSync(join(root, name))) await writeFile(join(diagnosticRoot, name), await readFile(join(root, name)));
        }
      }
      if (diagnosticRootBase && !witnessBuild && !revocationModes.includes(mode) && mode !== 'listener-pending-invalid-pid' && !listenerEvidenceModes.includes(mode) && (!assertionsPassed || mode === 'listener-concurrent-exit-before-close')) {
        const diagnosticRoot = diagnosticRootBase;
        await rm(diagnosticRoot, { recursive: true, force: true });
        await cp(root, diagnosticRoot, { recursive: true, force: true });
      }
      if (mode === 'listener-pending-invalid-pid' && (pendingInvalidFailure || cleanupFailure)) {
        pendingInvalidFailure ??= {error: cleanupFailure!.error, diagnostic: 'unavailable'};
        try { pendingInvalidFailure.diagnostic = pendingInvalidSnapshot(root, testMode, child, done); }
        catch { pendingInvalidFailure.diagnostic = '{"collection":"unavailable"}'; }
      }
      if (listenerEvidenceModes.includes(mode)) {
        if (!listenerFailure && cleanupFailure) listenerFailure = {error: cleanupFailure.error};
        try { listenerDiagnostic = listenerFailureAttachment(root, mode, captureControl, child, done, observationOutcome, listenerViews, listenerFailure?.error, cleanupFailure); }
        catch { listenerDiagnostic = '{"collection":"unavailable","secondary":"READ_ERROR"}'; }
        try {
          const error = listenerFailure?.error;
          if (error instanceof Error) {
            const suffix = `\nlistener-fixture ${listenerDiagnostic}`;
            const stack = error.stack;
            error.message += suffix;
            if (stack) error.stack = stack + suffix;
          }
        } catch { listenerViews.attachment = {unavailable: true}; }
      }
      if (!witnessBuild && (!listenerEvidenceModes.includes(mode) || done)) {
        try { await rm(root, { recursive: true, force: true }); }
        catch (error) {
          cleanupFailure ??= {error};
          const firstCleanupFailure = listenerEvidenceModes.includes(mode) && !listenerFailure;
          if (firstCleanupFailure) listenerFailure = {error};
          if (listenerFailure?.error instanceof Error) {
            try {
              const suffix = (firstCleanupFailure ? `\nlistener-fixture ${listenerDiagnostic}` : '') + '\nlistener-fixture-cleanup {"rootRemoval":"failed"}';
              const stack = listenerFailure.error.stack;
              listenerFailure.error.message += suffix;
              if (stack) listenerFailure.error.stack = stack + suffix;
            } catch { listenerViews.attachment = {unavailable: true}; }
          }
        }
      }
    }
    if (listenerFailure) throw listenerFailure.error;
    if (mode === 'listener-pending-invalid-pid') {
      if (!pendingInvalidFailure && cleanupFailure) pendingInvalidFailure = {error: cleanupFailure.error, diagnostic: '{"collection":"unavailable","cleanup":"root-removal-failed"}'};
      if (pendingInvalidFailure) {
        const {error, diagnostic} = pendingInvalidFailure;
        try {
          if (error instanceof Error) {
            const snapshot = `\npending-invalid-fixture ${diagnostic}`;
            const stack = error.stack;
            error.message += snapshot;
            if (stack) error.stack = stack + snapshot;
          }
        } catch { pendingInvalidFailure.diagnostic = '{"attachment":"unavailable"}'; }
        throw error;
      }
    }
    if (cleanupFailure) throw cleanupFailure.error;
    if (assertionsPassed && witnessFailure) throw witnessFailure;
  }]);
}

xctestOwnerTests.push(['Native startup XCTest terminal witness receipt validation', async () => {
  const nonce = 'a'.repeat(32);
  const previous = 'b'.repeat(64);
  const data = {method: 'linux-pidfd-epoll', observerPID: 100, queue: 3, fd: 4, pad: 0, requested: 1, result: 0, error: null};
  const armed = terminalRecord('armed', nonce, 200, previous, data);
  const bytes = (value: unknown): Buffer => Buffer.from(JSON.stringify(value) + '\n');
  assert.deepEqual(terminalParse(bytes(armed), 'armed', nonce, 200, previous), armed);
  terminalValidateKernel(armed);
  const event = terminalRecord('terminal', nonce, 200, previous, {method: data.method, observerPID: 100, queue: 3, fd: 4, pad: 0, events: 1, count: 1, error: null});
  terminalValidateKernel(armed, event);
  terminalValidateKernel(armed, {...event, Data: {...event.Data, events: 17}});
  for (const [key, value] of Object.entries({Nonce: 'c'.repeat(32), TargetPID: 201, Phase: 'terminal', Previous: 'd'.repeat(64), Schema: 2})) {
    assert.throws(() => terminalParse(bytes({...armed, [key]: value}), 'armed', nonce, 200, previous));
  }
  for (const value of [null, undefined, -1, '0', NaN]) assert.throws(() => terminalValidateKernel({...armed, Data: {...data, result: value}}));
  const absent = {...data} as Record<string, unknown>;
  delete absent.result;
  assert.throws(() => terminalValidateKernel({...armed, Data: absent}));
  for (const error of ['EPERM', 'ESRCH', 'ErrNoHandle']) assert.throws(() => terminalValidateKernel({...armed, Data: {...data, error}}));
  for (const patch of [{events: 16}, {events: 8}, {events: 9}, {events: 33}, {count: 0}, {count: 2}, {fd: 5}, {pad: 1}, {error: 'EPERM'}, {error: 'ESRCH'}, {error: 'ErrNoHandle'}]) {
    assert.throws(() => terminalValidateKernel(armed, {...event, Data: {...event.Data, ...patch}}));
  }
  const darwin = {...armed, Data: {method: 'darwin-kqueue', observerPID: 100, queue: 3, ident: '200', filter: -5, flags: 0x4000, fflags: 0x80000000, data: '0', requestedFlags: 0x51, requestedFflags: 0x80000000, count: 1, error: null}};
  terminalValidateKernel(darwin);
  const darwinEvent = {...event, Data: {method: 'darwin-kqueue', observerPID: 100, queue: 3, ident: '200', filter: -5, flags: 0x8010, fflags: 0x80000000, data: '9223372036854775807', count: 1, error: null}};
  terminalValidateKernel(darwin, darwinEvent);
  for (const patch of [{flags: 0x4000, data: '0'}, {filter: -1}, {ident: '201'}, {fflags: -2147483648}, {fflags: 0x80000001}, {data: '9223372036854775808'}, {data: 0}, {count: 0}, {error: 'EINTR'}]) {
    assert.throws(() => terminalValidateKernel(darwin, {...darwinEvent, Data: {...darwinEvent.Data, ...patch}}));
  }
  assert.throws(() => terminalValidateKernel(darwin, darwin));
  for (const malformed of [Buffer.alloc(8193, 32), Buffer.from('{'), Buffer.from(bytes(armed).toString().replace('"Schema":1', '"Schema":1,"Schema":1')), Buffer.concat([bytes(armed), bytes(armed)]), bytes({...armed, private: 'suppressed-sentinel'})]) {
    assert.throws(() => terminalParse(malformed, 'armed', nonce, 200, previous));
  }
  for (const record of [armed, event, {...event, Data: {...event.Data, events: 17}}, darwin, darwinEvent]) {
    assert.equal(terminalExportSafe(bytes(record), '/fixture', '/build'), true);
    assert.equal(terminalExportSafe(bytes({...record, private: 'SUCCESS'}), '/fixture', '/build'), false);
    assert.equal(terminalExportSafe(bytes({...record, Data: {...record.Data, private: 'SUCCESS'}}), '/fixture', '/build'), false);
    assert.equal(terminalExportSafe(bytes({...record, Data: {...record.Data, method: 'https://private.example/suppressed-sentinel'}}), '/fixture', '/build'), false);
  }
  for (const invalid of [Buffer.alloc(8193, 32), Buffer.from('{'), bytes({...event, Data: {...event.Data, events: 'suppressed-sentinel'}}), bytes({...armed, Data: {data: Array(2).fill('suppressed-sentinel')}})]) {
    assert.equal(terminalExportSafe(invalid, '/fixture', '/build'), false);
  }
  const dispatcher = terminalShim();
  assert.ok(dispatcher.includes('"$$" "$PPID" "$terminal_now" > "$root/ps-blocked.json.next"'));
  const blockedConstruction = String.raw`      printf '{"Schema":1,"Phase":"ps-blocked","Nonce":"%s","TargetPID":%s,"Previous":"%s","Data":{"shellPID":%s,"parentPID":%s,"startedAt":%s}}\n' "$witness_nonce" "$pid" "$witness_live" "$$" "$PPID" "$terminal_now" > "$root/ps-blocked.json.next" || exit 2
      mv "$root/ps-blocked.json.next" "$root/ps-blocked.json" || exit 2
      touch "$root/exit-child-requested" || exit 2
      if ! terminal_wait "$root/child-exit-observed"; then`;
  assert.equal(dispatcher.split(blockedConstruction).length, 2);
  assert.equal((terminalGroupOwner.match(/process\.kill\(/gu) || []).length, 2);
  assert.ok(terminalGroupOwner.includes("process.kill(0, 'SIGTERM')"));
  assert.ok(terminalGroupOwner.includes("process.kill(0, 'SIGKILL')"));
  assert.ok(terminalGroupOwner.includes("process.on('SIGTERM', () => {})"));
  assert.ok(terminalGroupOwner.includes('authority = false; process.exit(2)'));
  assert.ok(terminalGroupBootstrap.includes('exec 3<&- 4>&-\nexec "$@"'));
  assert.equal(terminalSignalGroup.toString().includes('process.kill'), false);
  assert.equal(terminalOwnerRequest.toString().includes('process.kill'), false);
  assert.ok(terminalOwnerRequest.toString().includes('owned.groupUncertain || owned.ownerRetiring || owned.forced'));
  assert.ok(terminalRevokeGroup.toString().includes('owned.control?.destroy()'));

  const syntheticSettlement = (withStdout = false) => {
    const commands: string[] = [];
    const control = Object.assign(new EventEmitter(), {
      destroyed: false,
      write(command: string, callback: (error?: Error | null) => void) { commands.push(command); callback(); return true; },
      destroy() { this.destroyed = true; return this; },
    });
    const output = new EventEmitter();
    const stdout = withStdout ? new EventEmitter() : null;
    const child = Object.assign(new EventEmitter(), {stdout, stderr: null, stdio: [null, stdout, null, control, output]});
    let failures = 0;
    const owned = terminalSettlement(child as unknown as ChildProcess, Date.now() + 15_000, () => { failures++; });
    const observed = {closed: false, joined: false};
    void owned.closed.then(() => { observed.closed = true; });
    void owned.joined.then(() => { observed.joined = true; });
    const frame = (text: string): void => { output.emit('data', Buffer.from(text)); };
    const finishOwner = (): void => { output.emit('end'); output.emit('close'); control.emit('close'); };
    return {child, control, output, stdout, commands, owned, observed, frame, finishOwner, failures: () => failures};
  };
  const closeFirst = syntheticSettlement(true);
  closeFirst.stdout!.emit('close');
  closeFirst.child.emit('exit', 0, null);
  closeFirst.child.emit('close', 0, null);
  await Promise.resolve();
  assert.deepEqual(closeFirst.commands, ['R']);
  assert.deepEqual(closeFirst.observed, {closed: true, joined: false});
  assert.equal(closeFirst.owned.error, undefined);
  assert.equal(terminalSettled(closeFirst.owned), false);
  closeFirst.frame('A\nX0\n');
  await Promise.resolve();
  assert.equal(closeFirst.owned.ownerRetired, true);
  assert.equal(closeFirst.observed.joined, false);
  closeFirst.output.emit('end');
  closeFirst.output.emit('close');
  await Promise.resolve();
  assert.equal(closeFirst.control.destroyed, true);
  assert.equal(closeFirst.observed.joined, false);
  closeFirst.control.emit('close');
  assert.deepEqual(await terminalResult(closeFirst.owned, Date.now() + 15_000), {code: 0, signal: null, error: false});
  assert.equal(terminalSettled(closeFirst.owned), true);
  assert.equal(closeFirst.failures(), 0);

  const ownerFirst = syntheticSettlement(true);
  ownerFirst.frame('A\n');
  terminalSignalGroup(ownerFirst.owned, 'SIGTERM');
  ownerFirst.child.emit('exit', 46, null);
  assert.deepEqual(ownerFirst.commands, ['T']);
  ownerFirst.stdout!.emit('close');
  assert.deepEqual(ownerFirst.commands, ['T', 'R']);
  ownerFirst.control.emit('close');
  ownerFirst.frame('T\nX0\n');
  ownerFirst.output.emit('end');
  ownerFirst.output.emit('close');
  await Promise.resolve();
  assert.deepEqual(ownerFirst.observed, {closed: false, joined: false});
  assert.equal(ownerFirst.owned.result, undefined);
  terminalSignalGroup(ownerFirst.owned, 'SIGKILL');
  assert.deepEqual(ownerFirst.commands, ['T', 'R']);
  assert.equal(ownerFirst.owned.forced, false);
  ownerFirst.child.emit('close', 46, null);
  assert.deepEqual(await terminalResult(ownerFirst.owned, Date.now() + 15_000), {code: 46, signal: null, error: false});
  assert.equal(ownerFirst.failures(), 0);

  for (const text of ['', 'X0\n', 'T\nA\nX0\n', 'A\nA\n', 'A\nX2\n', 'A\nX137\n', 'A\nX0\nX0\n', 'A\nX0\nT\n', 'A\nunknown\n', 'A\nX', 'A\nX0\npartial', 'A\n', 'A\nX0', 'A'.repeat(33)]) {
    const invalid = syntheticSettlement();
    invalid.child.emit('exit', 0, null);
    invalid.child.emit('close', 0, null);
    invalid.frame(text);
    invalid.finishOwner();
    const first = invalid.owned.error;
    assert.ok(first, text);
    invalid.frame('A\nX0\n');
    await assert.rejects(terminalResult(invalid.owned, Date.now() + 15_000), error => error === first);
    assert.equal(invalid.owned.result?.error, true);
    assert.equal(terminalSettled(invalid.owned), false);
    assert.equal(invalid.control.destroyed, true);
    assert.equal(invalid.failures(), 1);
  }
  const unacknowledged = syntheticSettlement();
  terminalSignalGroup(unacknowledged.owned, 'SIGTERM');
  unacknowledged.child.emit('exit', 0, null);
  unacknowledged.frame('A\nX0\n');
  assert.ok(unacknowledged.owned.error);
  assert.equal(unacknowledged.owned.ownerRetired, false);

  for (const closeBeforeError of [false, true]) {
    for (const channel of ['child', 'output', 'control', 'stdout'] as const) {
      const failed = syntheticSettlement(true);
      if (closeBeforeError) failed.child.emit('close', 0, null);
      const first = new Error(`synthetic ${channel} failure`);
      failed[channel]!.emit('error', first);
      failed.owned.fail(new Error('later failure'));
      failed.frame('A\nX0\n');
      await assert.rejects(terminalResult(failed.owned, Date.now() + 15_000), error => error === first);
      await assert.rejects(terminalResult(failed.owned, Date.now() - 1), error => error === first);
      assert.equal(failed.observed.closed, closeBeforeError);
      assert.equal(failed.observed.joined, false);
      assert.equal(failed.owned.result !== undefined, closeBeforeError);
      assert.equal(failed.owned.ownerRetired, false);
      assert.equal(failed.failures(), 1);
      terminalSignalGroup(failed.owned, 'SIGKILL');
      assert.deepEqual(failed.commands, []);
      assert.equal(failed.control.destroyed, true);
    }
  }
  for (const channel of ['output', 'control'] as const) {
    const lost = syntheticSettlement();
    lost[channel].emit('close');
    assert.ok(lost.owned.error);
    assert.equal(lost.owned.result, undefined);
    assert.equal(terminalSettled(lost.owned), false);
  }
  const noEOF = syntheticSettlement();
  noEOF.child.emit('exit', 0, null);
  noEOF.frame('A\nX0\n');
  noEOF.output.emit('close');
  assert.match(noEOF.owned.error!.message, /owner close unproved/u);

  for (const missing of ['direct-close', 'retirement', 'output-close', 'control-close'] as const) {
    const pending = syntheticSettlement();
    pending.child.emit('exit', 0, null);
    if (missing !== 'direct-close') pending.child.emit('close', 0, null);
    if (missing !== 'retirement') {
      pending.frame('A\nX0\n');
      pending.output.emit('end');
      if (missing !== 'output-close') pending.output.emit('close');
      if (missing !== 'control-close') pending.control.emit('close');
    }
    await assert.rejects(terminalResult(pending.owned, Date.now() - 1), /terminal operation deadline/u);
    const first = pending.owned.error;
    assert.ok(first);
    assert.equal(pending.owned.result !== undefined, missing !== 'direct-close');
    assert.equal(pending.control.destroyed, true);
    pending.frame('A\nX0\n');
    pending.finishOwner();
    pending.child.emit('close', 0, null);
    terminalSignalGroup(pending.owned, 'SIGKILL');
    await assert.rejects(terminalResult(pending.owned, Date.now() + 15_000), error => error === first);
    assert.deepEqual(pending.commands, ['R']);
    assert.equal(pending.observed.joined, false);
    assert.equal(pending.failures(), 1);
  }
  const missingPipes = terminalSettlement(Object.assign(new EventEmitter(), {stdout: null, stderr: null, stdio: [null, null, null, null, null]}) as unknown as ChildProcess, Date.now() + 15_000);
  assert.match(missingPipes.error!.message, /owner pipes missing/u);
  assert.equal(missingPipes.result, undefined);
  const timedOut = [syntheticSettlement(), syntheticSettlement()];
  await assert.rejects(terminalOwnedClose(timedOut.map(value => value.owned), Date.now() - 1), /terminal operation deadline/u);
  for (const pending of timedOut) {
    assert.ok(pending.owned.error);
    assert.equal(pending.control.destroyed, true);
    assert.equal(pending.owned.result, undefined);
    assert.deepEqual(pending.observed, {closed: false, joined: false});
  }
  const cancelled = [syntheticSettlement(), syntheticSettlement()];
  const first = new Error('synthetic cancellation');
  cancelled[0].owned.fail(first);
  await assert.rejects(terminalOwnedClose(cancelled.map(value => value.owned), Date.now() + 15_000), error => error === first);
  for (const pending of cancelled) {
    assert.equal(pending.owned.error, first);
    assert.equal(pending.control.destroyed, true);
    assert.equal(pending.owned.result, undefined);
    assert.deepEqual(pending.observed, {closed: false, joined: false});
  }
}]);

const terminalControl = async (fault: boolean): Promise<void> => {
  const build = await terminalBuild();
  const root = await mkdtemp(join(tmpdir(), 'herdr-terminal-control-'));
  await mkdir(join(root, 'state'));
  const nonce = randomBytes(16).toString('hex');
  writeFileSync(join(root, 'witness-nonce'), nonce, {flag: 'wx', mode: 0o600});
  const server = join(root, 'target.ts');
  await writeFile(server, terminalServer, {flag: 'wx', mode: 0o600});
  const ps = join(root, 'ps');
  await writeFile(ps, terminalShim(), {flag: 'wx', mode: 0o700});
  const reserve = createNetServer();
  await new Promise<void>((resolve, reject) => { reserve.once('error', reject); reserve.listen(0, '127.0.0.1', resolve); });
  const port = (reserve.address() as AddressInfo).port;
  await new Promise<void>((resolve, reject) => reserve.close(error => error ? reject(error) : resolve()));
  const deadline = Date.now() + 10_000;
  writeFileSync(join(root, 'state/deadline'), String(deadline), {flag: 'wx'});
  const env: NodeJS.ProcessEnv = {PATH: process.env.PATH, HOME: build.env.HOME, TMPDIR: build.env.TMPDIR, LANG: 'C', LC_ALL: 'C', STARTUP_TEST_ROOT: root, STARTUP_TEST_MODE: 'listener-concurrent-exit-before-close', IOS_WDA_PORT: String(port)};
  if (build.env.__CF_USER_TEXT_ENCODING !== undefined) env.__CF_USER_TEXT_ENCODING = build.env.__CF_USER_TEXT_ENCODING;
  terminalWrite(root, 'source-binding.json', {dispatcher: terminalHash(await readFile(ps)), server: terminalHash(await readFile(server)), binary: build.binaryHash, sources: build.sources, buildRoot: build.root, binding: 'fixture-parent-spawn'});
  const target = terminalSpawn(process.execPath, [server, 'xcodebuild'], {env, stdio: ['ignore', 'pipe', 'pipe']}, deadline + 30_000);
  target.stdout!.resume();
  target.stderr!.resume();
  const settled = terminalSettlement(target, deadline + 30_000, () => terminalFailure(root));
  let closed = false;
  target.once('close', () => { closed = true; });
  let callback: {code: number | null; signal: NodeJS.Signals | null} | undefined;
  const exit = new Promise<void>(resolve => target.once('exit', (code, signal) => { callback = {code, signal}; resolve(); }));
  let session: TerminalSession | undefined;
  let shell: ChildProcess | undefined;
  let shellSettled: TerminalSettlement | undefined;
  let primary: unknown;
  try {
    session = await terminalStart(root, nonce, process.pid, deadline, build, terminalPid(target.pid), fault);
    if (fault) {
      shell = terminalSpawn('/bin/sh', ['-c', 'root=$STARTUP_TEST_ROOT\n' + terminalShellWait + '\nterminal_wait "$root/child-exit-observed" || exit 2'], {env, stdio: ['ignore', 'ignore', 'ignore']}, deadline + 30_000);
      shellSettled = terminalSettlement(shell, deadline + 30_000, () => terminalFailure(root));
      while (!existsSync(join(root, 'observer-error.json'))) {
        for (const owned of [settled, session.settled, shellSettled]) if (owned.error) throw owned.error;
        assert.ok(Date.now() < deadline, 'negative registration deadline');
        assert.equal(callback, undefined, 'negative target was not held');
        assert.equal(existsSync(join(root, 'target-error')), false);
        await new Promise(resolve => setTimeout(resolve, 1));
      }
      assert.equal(callback, undefined, 'registration error must precede target cleanup');
      const error = JSON.parse(terminalRead(root, 'observer-error.json').toString());
      terminalKeys(error, ['result', 'stage', 'category', 'errno', 'errnoKnown', 'callResult', 'callResultKind', 'cleanup']);
      assert.deepEqual({result: error.result, stage: error.stage, category: error.category, errno: error.errno, errnoKnown: error.errnoKnown, cleanup: error.cleanup}, {result: 'ERROR', stage: 'registration', category: 'EBADF', errno: 9, errnoKnown: true, cleanup: null});
      if (process.platform === 'linux') {
        assert.equal(error.callResult, null);
        assert.equal(error.callResultKind, 'error-only');
      } else {
        assert.ok(Number.isInteger(error.callResult));
        assert.equal(error.callResultKind, 'number');
      }
      while (!existsSync(join(root, 'failure-held.json'))) {
        for (const owned of [settled, session.settled, shellSettled]) if (owned.error) throw owned.error;
        assert.ok(Date.now() < deadline, 'negative held acknowledgement deadline');
        assert.equal(callback, undefined);
        assert.equal(existsSync(join(root, 'target-error')), false);
        await new Promise(resolve => setTimeout(resolve, 1));
      }
      const failureHeld = terminalParse(terminalRead(root, 'failure-held.json'), 'failure-held', nonce, terminalPid(target.pid), terminalHash(terminalRead(root, 'observer-error.json')));
      assert.deepEqual(failureHeld.Data, {pid: target.pid, parentPID: process.pid, endpointPID: session.endpointPID});
      assert.deepEqual(await terminalResult(session.settled, deadline), {code: 2, signal: null, error: false});
      assert.deepEqual(await terminalResult(shellSettled, deadline), {code: 2, signal: null, error: false});
      for (const name of ['armed.json', 'challenge.json', 'live.json', 'continue.json', 'terminal.json', 'complete.json', 'child-exit-observed']) assert.equal(existsSync(join(root, name)), false);
      terminalWrite(root, 'control-result.json', {binding: 'fixture-parent-spawn', operation: 'registration', queue: -1, errno: 9, heldBeforeCleanup: true, shellCode: 2});
    } else {
      await terminalArm(session);
      writeFileSync(join(root, 'status-queries'), '1');
      let stdout = '';
      shell = terminalSpawn(ps, ['-p', String(target.pid), '-o', 'stat='], {env, stdio: ['ignore', 'pipe', 'ignore']}, deadline + 30_000);
      shellSettled = terminalSettlement(shell, deadline + 30_000, () => terminalFailure(root));
      shell.stdout!.on('data', chunk => { stdout += chunk.toString(); });
      await terminalRelease(session, port, false);
      assert.deepEqual(await terminalResult(shellSettled, deadline), {code: 0, signal: null, error: false});
      assert.equal(stdout, 'S\n');
      await terminalBound(Promise.race([exit, settled.failed.then(error => { throw error; })]), deadline);
      if (settled.error) throw settled.error;
      assert.deepEqual(callback, {code: 46, signal: null});
      assert.equal(closed, false, 'endpoint must retain target stdio');
      terminalWrite(root, 'callback.json', {binding: 'fixture-parent-spawn', code: target.exitCode, signal: target.signalCode, closeBeforeProof: closed});
    }
  } catch (error) { primary = error; terminalFailure(root); }
  finally {
    try { await terminalTeardown(root, session, settled, Boolean(primary) || fault, shellSettled); }
    catch (error) { if (!primary) primary = error; }
    try { await terminalExport(root, fault ? 'terminal-registration-failure' : 'owned-terminal-witness', build); }
    catch (error) { if (!primary) primary = error; }
  }
  if (primary) throw primary;
};
xctestOwnerTests.push(['Native startup XCTest terminal witness registration failure', async () => { await terminalControl(true); }]);
xctestOwnerTests.push(['Native startup XCTest owned terminal witness', async () => { await terminalControl(false); }]);

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
      await writeFile(join(root, 'wda/package.json'), JSON.stringify({ version: '16.12.8' }));
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
