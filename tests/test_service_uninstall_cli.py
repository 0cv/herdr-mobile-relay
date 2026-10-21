#!/usr/bin/env python3
"""Hosted-only actual-entrypoint removal fixtures; source authored, not qualification."""
from __future__ import annotations
import os
import sys

# Genuine admission BEFORE discovery, temp roots, helpers and subprocesses.
if os.environ.get("GITHUB_ACTIONS") != "true" or os.environ.get("RUNNER_ENVIRONMENT") != "github-hosted":
    raise SystemExit("service-removal fixtures require a genuine disposable GitHub-hosted runner")

import hashlib
import json
import plistlib
import pty
import select
import shutil
import signal
import stat
import subprocess
import tempfile
import threading
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[1]
TIMEOUT = 40  # Finite invocation containment including repeated configuration/metadata probes.
ESCAPED_ENV_NAME = "relay\\x%%.env"
DOUBLE_DECODED_ENV_NAME = "relayx%.env"

# MODEL_CHECK's whole acceptance test (see the FAST_STUB comment): ONE text, shared by the validator and by the adapters' save().
# `text` is canonical json.dumps of an object with a 'fault' key, holds no newline but one optional trailing one, and passes the
# shape check that install_stub bakes in as model_shape in place of the placeholder on the first line.
ACCEPTABLE = r'''def shape(model): return True
def acceptable(text):
    model = json.loads(text); body = text[:-1] if text.endswith('\n') else text
    return isinstance(model, dict) and 'fault' in model and '\n' not in body and text == json.dumps(model) and shape(model)
'''

# Strict finite synthetic controller/tool. No host service/network access.
ADAPTER = r'''import json, os, pathlib, sys, time
root = pathlib.Path(os.environ['FIXTURE_ROOT']); control = root / 'control'
model_path = control / 'model.json'; model = json.loads(model_path.read_text())
name = pathlib.Path(sys.argv[0]).name; args = sys.argv[1:]
events = control / 'events.jsonl'
# Diagnostic only: one line per stub process, so a run can report how many it spawned.
with (control / 'spawns.log').open('a') as stream: stream.write(name + '\n')
def event(op, label='', **extra):
    row = dict(op=op, label=label, **extra)
    with events.open('a') as stream: stream.write(json.dumps(row) + '\n')
''' + ACCEPTABLE + r'''def save():
    # What was just written is the exact copy the fast stat/head/wc/uname stubs may trust (control/model.valid) IF MODEL_CHECK's own
    # test holds for these very bytes, so the next fast call need not re-run MODEL_CHECK. Otherwise model.valid stays as it was, no
    # longer equal to model.json, and that call runs MODEL_CHECK exactly as after a foreign rewrite.
    text = json.dumps(model); model_path.write_text(text)
    try:
        if acceptable(text): (control / 'model.valid').write_text(text)
    except Exception: pass
def refuse(): event('forbidden', tool=name); raise SystemExit(97)
def fault(label, *kinds): return label == model['fault_label'] and model['fault'] in kinds
def pause(label):
    (control / 'ready').write_text(label)
    end = time.monotonic() + 6
    while not (control / 'release').exists():
        if time.monotonic() >= end: refuse()
        time.sleep(.01)
def observe(label):
    s = model['states'][label]; s['queries'] += 1
    if s.get('remaining', 0):
        s['remaining'] -= 1
        if not s['remaining']: s['state'] = s.pop('desired')
    if fault(label, 'reactivate') and s['queries'] == 4:
        s['state'] = 'active' if model['platform'] == 'Linux' else 'loaded'; s['enabled'] = True
    if fault(label, 'replace-file') and s['queries'] == 3:
        q = pathlib.Path(model['files'][label]); t = q.with_suffix('.replacement')
        t.write_text(q.read_text() + '# replacement\n'); t.chmod(0o600); t.replace(q)
    save(); event('observe', label, state=s['state'], enabled=s['enabled'])
    if fault(label, 'pause-observe') and s['queries'] == 3: pause(label)
    return s
def configuration(label):
    s = model['states'][label]
    s['configuration_queries'] = s.get('configuration_queries', 0) + 1
    event('configuration-observe', label, query=s['configuration_queries']); save()
    # Configuration queries do NOT advance lifecycle polls, delayed stops,
    # reactivation/replacement triggers or cancellation handshakes.
    present = pathlib.Path(model['files'][label]).is_file()
    loaded = present or s['state'] != 'absent'
    values = dict(Id=label, LoadState='loaded' if loaded else 'not-found',
                  ActiveState='inactive' if s['state'] == 'absent' else s['state'],
                  UnitFileState=('enabled' if s['enabled'] else 'disabled') if loaded else '',
                  FragmentPath=model['files'][label] if loaded else '',
                  DropInPaths=model.get('overlays', {}).get(label, ''), NeedDaemonReload='no')
    values.update(model.get('metadata_overrides', {}).get(label, {}))
    if label == model.get('configuration_drift_label') and s['configuration_queries'] >= model['configuration_drift_at']:
        values.update(model['configuration_drift'])
    if fault(label, 'meta-query-fail'): raise SystemExit(1)
    if fault(label, 'meta-partial-fail'): print('Id=' + label); raise SystemExit(1)
    if fault(label, 'meta-missing'): del values[model['metadata_field']]
    output = '\n'.join(key + '=' + value for key, value in values.items())
    if fault(label, 'meta-duplicate'): output += '\n' + model['metadata_field'] + '=' + values[model['metadata_field']]
    if fault(label, 'meta-malformed'): output = 'invalid metadata'
    if fault(label, 'meta-extra'): output += '\nUnexpectedProperty=value'
    if fault(label, 'meta-oversize'): output += '\n' + 'X' * 4097
    if fault(label, 'meta-nul'): output = output.replace('DropInPaths=', 'DropInPaths=\x00')
    print(output); raise SystemExit(0)
def lifecycle_output(label, seam, output, code=0):
    if label == model['fault_label'] and seam == model.get('lifecycle_seam') and model['fault'] == 'lifecycle-output' and model['states'][label]['queries'] >= model.get('lifecycle_fault_at', 0):
        event('lifecycle-capture', label, seam=seam)
        output = model['lifecycle_text']; code = model['lifecycle_status']
        if model.get('lifecycle_stderr'):
            print(output, file=sys.stderr); raise SystemExit(code)
    print(output); raise SystemExit(code)
def transition(label, op):
    s = model['states'][label]; event(op, label)
    if fault(label, op + '-fail'): raise SystemExit(1)
    if op == 'disable': s['enabled'] = False
    elif not fault(label, 'stop-noop'):
        desired = 'inactive' if model['platform'] == 'Linux' else 'absent'
        if fault(label, 'delayed'): s.update(remaining=3, desired=desired)
        else: s['state'] = desired
    save()
    if fault(label, 'pause-stop') and op in ('stop', 'bootout'): pause(label)
    raise SystemExit(0)
if name == 'uname':
    if args != ['-s']: refuse()
    print(model['platform']); raise SystemExit(0)
if name == 'sleep':
    if args != ['0.1']: refuse()
    event('poll-delay'); raise SystemExit(0)
if name == 'plutil':
    import plistlib  # imported here only: every other stub spawn skips its start-up cost
    if len(args) != 6 or args[0] != '-extract' or args[1] not in ('Label', 'EnvironmentVariables.HERDR_RELAY_ENV') or args[2:5] != ['raw', '-o', '-']: refuse()
    if model['fault'] == 'decode-fail' and args[1] != 'Label': raise SystemExit(1)
    if model['fault'] == 'label-decode-fail' and args[1] == 'Label': raise SystemExit(1)
    if model['fault'] == 'label-malformed-output' and args[1] == 'Label': print('invalid\nlabel'); raise SystemExit(0)
    try:
        data = plistlib.loads(pathlib.Path(args[5]).read_bytes())
        value = data['Label'] if args[1] == 'Label' else data['EnvironmentVariables']['HERDR_RELAY_ENV']
    except (KeyError, TypeError, ValueError, OSError, plistlib.InvalidFileException): raise SystemExit(1)
    if not isinstance(value, str): raise SystemExit(1)
    print(value); raise SystemExit(0)
if name in ('head', 'wc', 'stat'):
    if name == 'stat':
        formats = {('-c', '%u:%a:%h'), ('-f', '%u:%Lp:%l'), ('-c', '%d:%i:%s:%Y'), ('-f', '%d:%i:%z:%m'), ('-c', '%d:%i'), ('-f', '%d:%i')}
        # Only this combined form may name several paths (one spawn for an ancestor walk); every other form takes exactly one.
        batch = {('-c', '%d:%i:%s:%Y:%u:%a:%h'), ('-f', '%d:%i:%z:%m:%u:%Lp:%l')}
        if len(args) < 3 or tuple(args[:2]) not in formats | batch or (len(args) > 3 and tuple(args[:2]) not in batch): refuse()
        if model['fault'] == 'stat-fail': raise SystemExit(1)
        if model['fault'] == 'stat-malformed': print('invalid identity'); raise SystemExit(0)
        if args[1] == '%d:%i':
            if args[2] == model.get('role_identity_fault_path'):
                if model['fault'] == 'role-identity-fail': raise SystemExit(1)
                if model['fault'] == 'role-identity-malformed': print('invalid identity'); raise SystemExit(0)
            equivalent = model.get('identity_aliases', {}).get(args[2])
            if equivalent:
                info = pathlib.Path(equivalent).stat()
                print('%s:%s' % (info.st_dev, info.st_ino)); raise SystemExit(0)
    if name != 'stat' and model['fault'] == 'read-fail': raise SystemExit(1)
    os.execv('/usr/bin/' + name, [name] + args)
if name == 'route-adapter':
    if args != ['unpublish']: refuse()
    event('unpublish', env=os.environ.get('HERDR_RELAY_ENV', ''))
    if model['route_status']: raise SystemExit(model['route_status'])
    (root / 'private' / 'route').write_text('explicitly removed fixture route\n'); raise SystemExit(0)
if name == 'rm':
    if len(args) != 3 or args[:2] != ['-f', '--']: refuse()
    matches = [k for k, v in model['files'].items() if v == args[2]]
    if len(matches) != 1: refuse()
    label = matches[0]; s = model['states'][label]
    event('remove', label, state=s['state'], enabled=s['enabled'])
    if s['state'] not in ('inactive', 'absent') or s['enabled']: refuse()
    if fault(label, 'rm-fail'): raise SystemExit(1)
    pathlib.Path(args[2]).unlink(missing_ok=True); raise SystemExit(0)
if name == 'systemctl':
    if args == ['--user', 'daemon-reload']:
        event('reload'); raise SystemExit(1 if model['fault'] == 'reload-fail' else 0)
    if len(args) == 3 and args[0] == '--user' and args[1] in ('stop', 'disable') and args[2] in model['states']:
        transition(args[2], args[1])
    if len(args) == 12 and args[:2] == ['--user', 'show'] and args[2] in model['files'] and args[3:] == ['--all', '--property=Id', '--property=LoadState', '--property=ActiveState', '--property=UnitFileState', '--property=FragmentPath', '--property=DropInPaths', '--property=NeedDaemonReload', '--no-pager']:
        configuration(args[2])
    if len(args) != 7 or args[:2] != ['--user', 'show'] or args[2] not in model['states'] or args[3:] != ['--property=LoadState', '--property=ActiveState', '--property=UnitFileState', '--no-pager']: refuse()
    label = args[2]; s = observe(label)
    if fault(label, 'query-fail'): raise SystemExit(1)
    loaded = pathlib.Path(model['files'][label]).is_file() or s['state'] != 'absent'
    output = 'LoadState=%s\nActiveState=%s\nUnitFileState=%s' % ('loaded' if loaded else 'not-found', 'inactive' if s['state'] == 'absent' else s['state'], ('enabled' if s['enabled'] else 'disabled') if loaded else '')
    if fault(label, 'contradictory'): output = 'LoadState=not-found\nActiveState=active\nUnitFileState=enabled'
    if fault(label, 'duplicate'): output += '\nLoadState=loaded'
    if fault(label, 'missing'): output = 'LoadState=loaded\nActiveState=inactive'
    if fault(label, 'unknown'): output = 'LoadState=loaded\nActiveState=mystery\nUnitFileState=disabled'
    if fault(label, 'unsupported-enable'): output = 'LoadState=loaded\nActiveState=inactive\nUnitFileState=enabled-runtime'
    if fault(label, 'malformed'): output = ''
    lifecycle_output(label, 'systemd', output)
if name != 'launchctl': refuse()
domain = 'gui/' + str(os.getuid())
if args == ['print', domain]:
    if model['fault'] == 'domain-fail': event('domain-error'); raise SystemExit(1)
    # The product reads the domain listing by status alone, so a malformed answer refuses only as a FAILING query (status 1).
    if model['fault'] == 'domain-malformed': print('wrong-domain = {}'); raise SystemExit(1)
    lifecycle_output(model['fault_label'], 'domain', domain + ' = {}')
if args == ['print-disabled', domain]:
    if model['fault'] == 'disabled-fail': raise SystemExit(1)
    if model['fault'] == 'disabled-malformed': print('unknown disabled map'); raise SystemExit(0)
    # A faulted model answers in the macOS 13+ layout (indented, keyword states, a trailing login-item section); an unfaulted one, which the sh stub answers, in macOS 12's.
    modern = model['fault'] != ''
    rows = ['\tdisabled services = {' if modern else 'disabled services = {']
    for label, s in model['states'].items():
        if fault(label, 'disabled-missing'): continue
        row = '\t\t"%s" => %s' % (label, 'enabled' if s['enabled'] else 'disabled') if modern else '    "%s" => %s' % (label, 'false' if s['enabled'] else 'true')
        rows.append(row)
        if fault(label, 'disabled-duplicate'): rows.append(row)
    lifecycle_output(model['fault_label'], 'disabled', '\n'.join(rows + (['\t}', '', '\tlogin item associations = {', '\t}'] if modern else ['}'])))
if len(args) != 2 or not args[1].startswith(domain + '/'): refuse()
label = args[1][len(domain) + 1:]
if label not in model['states']: refuse()
if args[0] in ('bootout', 'disable'): transition(label, args[0])
if args[0] != 'print': refuse()
s = observe(label)
if fault(label, 'query-fail'): print('controller unavailable'); raise SystemExit(1)
if fault(label, 'wrong-label'): print(domain + '/unrelated = {}'); raise SystemExit(0)
if fault(label, 'malformed'): print(''); raise SystemExit(0)
if s['state'] == 'absent':
    lifecycle_output(label, 'label', 'Could not find service "%s" in domain for user gui: %s' % (label, os.getuid()), 113)
lifecycle_output(label, 'label', domain + '/' + label + ' = {\n    state = waiting\n}')
'''


# Fast path for the tools that make up 98% of all stub spawns (hosted run 38837569325: 880 of the 903
# spawns of one macOS removal invocation were stat; then uname, systemctl and launchctl). Each Python adapter pays an
# interpreter start plus the json, pathlib and plistlib imports (about 30-43 ms hosted) only to exec
# the real tool, or to answer from the model. On the steady path this bash stub forks nothing before its exec (not a Python
# interpreter, and no cmp: it compares the two files itself, see below; it was a dash script plus one /usr/bin/cmp, one
# process more per call, of which a macOS hosted runner pays about 5 ms), and a stat call may name SEVERAL paths in the combined
# '%d:%i:%s:%Y:%u:%a:%h' ('%d:%i:%z:%m:%u:%Lp:%l' on BSD) form only, so one ancestor walk is one spawn
# (every other form takes exactly one path, or it is delegated and refused). ANY doubt delegates to the unchanged Python adapter installed
# beside it in bin/.python (the same file name, so the adapter sees the same tool name): a model fault
# or alias that could change the adapter's answer, an argument form outside the adapter's stat
# allowlist (the adapter then refuses and records its 'forbidden' event), head and wc under a
# read-fail fault, and a control model that is not PROVEN valid. A shape test in sh cannot prove that
# ({broken}, {not json}, a trailing comma and trailing data all look like {...}), and a sh string
# cannot even hold a NUL byte, so the proof is a real parse plus an exact byte comparison: MODEL_CHECK
# (bin/.python/.model-ok) json.loads the whole control/model.json and stores an exact byte copy as
# control/model.valid only if ALL of these hold: it is an object with a 'fault' key; it passes the shape
# check that install_stub bakes in as model_shape (nothing for ADAPTER, which reads no more than 'fault'
# for stat, head and wc; for FULL_ADAPTER, whose prefix dereferences files, shared and states on every
# call: files an object of strings, shared an object, states an object of objects with a numeric
# 'queries', so the prefix cannot raise where the stub would have run the real tool); it has no newline
# except one optional trailing newline; and its text equals
# json.dumps(model) byte for byte. The last condition is what makes the raw-text globs below sound.
# The adapter decides faults from the DECODED model, and valid JSON can spell a fault so that the raw
# text holds no fault token (an escaped value such as stat\u002dfail, an escaped key such as
# identity\u005faliases, duplicate keys). json.dumps never escapes letters, '-' or '_', so in canonical
# text the raw ASCII tokens and the decoded values coincide; any other spelling (escapes, other
# whitespace, duplicate keys, raw non-ASCII, a trailing newline) fails MODEL_CHECK and is delegated.
# Key ORDER is not a spelling: json.dumps(json.loads(text)) keeps parse order, so a reordered object is
# still canonical, decodes identically, and the globs do not depend on order. Every legitimate writer
# (the adapters' save(), Fixture.save, the FULL_ADAPTER prefix's store() and this control) writes plain
# json.dumps(model). The stub takes the
# shell path only when model.json is byte-identical to that non-empty copy (the stub reads both with `read -d ''`, which answers 0
# at a NUL and 1 at the end of the file, so two files are equal iff neither holds a NUL and the strings are equal; a model of
# more than 4096 bytes, which bash would read byte by byte, takes cmp and cat instead), and then reads
# the fault substrings from the copy, never from model.json. Any other model (missing, unreadable,
# empty, malformed, with trailing data or a NUL byte, or rewritten since: the adapters rewrite it on
# every observe) runs MODEL_CHECK once; if that rejects it, or the copy cannot be read, the stub
# delegates, and the adapter aborts before it spawns anything, so a broken fixture never becomes a run
# of the real host tool. A missing model.big only makes a big model read slowly; the marker never decides what a stub answers.
# Otherwise the stub appends the same tool name to spawns.log, so the spawn diagnostic stays truthful
# (MODEL_CHECK and cmp runs are not tool spawns and are not counted), and execs the real tool exactly
# as the adapter does. MODEL_CHECK does not run after every observation any more: the adapters' save() and the sh
# answers below leave control/model.valid equal to the model.json they have just written, under the very test MODEL_CHECK
# applies (ACCEPTABLE; a model a FULL adapter's stat call would ARM is not refreshed), and a foreign rewrite still is not trusted.
# uname answers the platform of the canonical first key (a plain token) for exactly `-s`. systemctl and launchctl answer, from
# canonical two-label models only, one observation of a label, a systemd configuration query, stop, disable, bootout,
# daemon-reload, `launchctl print DOMAIN` and `print-disabled DOMAIN`: the head of the model is parsed in sh and REBUILT from
# what was parsed (a text that is not exactly its rebuild, or whose head holds an escape, is the adapter's), only a model without
# a fault of that label (or any fault, for the calls on no label) is answered, and everything else, such as effective-configuration
# extras, a native_done gate, other labels, other argument forms and every fault, is the adapter's. These answers mirror
# the adapter text: model_stub_control pins it by digest, runs chains of calls through both implementations and compares
# every output, event, tally and model byte.
# MIRRORED holds the two adapters in use: 1f47fddd... is ADAPTER (this file), a59e54e3... is the FULL_ADAPTER of tests/test_uninstall.sh. The
# earlier pair (c476dcb2..., d1bbd99a...; 7ec0920f... before it) is retired, no suite uses that text any more. a59e54e3... and 1f47fddd... changed only the
# launchd `print-disabled` answer of a FAULTED model (the macOS 13+ layout) and the `domain-malformed` status (a failing query: the product reads the domain
# by status alone). The sh stub answers `print-disabled` and the domain only for an UNFAULTED model (any fault delegates to the adapter), so the stub text
# is unchanged (d1bbd99a... had re-derived FAST_STUB against the prefix that Linux mount confinement extended with a staged-guard `rm` branch and a
# `late_kind == 'fifo'` gate arm; neither touches a call the stub answers itself).
MIRRORED = {'1f47fddd726e058c1482cbfd68debd552ee7c09c64e7842cf9cdcf6ce843e99b', 'a59e54e332d0c08252558f836c9c6d935a2ccc2cbeb00f2ca791dc8b3ec88720'}
FAST_TOOLS = ('stat', 'head', 'wc', 'uname', 'systemctl', 'launchctl', 'plutil')
MODEL_CHECK = r'''import json, pathlib, sys
''' + ACCEPTABLE + r'''try:
    control = pathlib.Path(sys.argv[1]); raw = (control / 'model.json').read_bytes()
    ok = acceptable(raw.decode('utf-8'))
    if ok: (control / 'model.valid').write_bytes(raw)
    if len(raw) > 4096: (control / 'model.big').write_bytes(b'')   # speed only (see FAST_STUB): the marker never decides what a stub answers
    else: (control / 'model.big').unlink(missing_ok=True)
except Exception: ok = False
sys.exit(0 if ok else 1)
'''
FAST_STUB = r"""#!/bin/bash
name=${0##*/}
delegate() { exec "$FIXTURE_ROOT/bin/.python/$name" "$@"; }
control=$FIXTURE_ROOT/control
domain=@DOMAIN@
full=@FULL@
case "$name:$#:$1" in
    systemctl:2:--user|systemctl:3:--user|systemctl:7:--user|systemctl:12:--user|launchctl:2:*) ;;
    systemctl:*|launchctl:*|plutil:[!6]*) delegate "$@" ;;
esac
model=; now=
if [ -f "$control/model.big" ]; then   # bash reads a long line byte by byte (26 ms for 65 KB), cmp and cat do not
    { [ -s "$control/model.valid" ] && /usr/bin/cmp -s "$control/model.json" "$control/model.valid" 2>/dev/null; } || "$FIXTURE_ROOT/bin/.python/.model-ok" "$control" || delegate "$@"
    model=$(/bin/cat "$control/model.valid")
else   # the two files are the same bytes iff both read to their end without meeting a NUL (read -d '' answers 0 at a NUL, 1 at the end) and are equal
    IFS= read -r -d '' model 2>/dev/null < "$control/model.valid"; copy=$?
    IFS= read -r -d '' now 2>/dev/null < "$control/model.json"; live=$?
    if [ "$copy" -ne 1 ] || [ "$live" -ne 1 ] || [ "$now" != "$model" ]; then
        "$FIXTURE_ROOT/bin/.python/.model-ok" "$control" || delegate "$@"
        model=; IFS= read -r -d '' model < "$control/model.valid" || :
    fi
fi
[ -n "$model" ] || delegate "$@"
case "$name" in
    stat)
        case "$model" in
            *stat-fail*|*stat-malformed*|*role-identity*|*identity_aliases*|*role_identity_fault_path*) delegate "$@" ;;
        esac
        [ "$#" -ge 3 ] || delegate "$@"
        case "$1" in
            -c) case "$2" in '%u:%a:%h'|'%d:%i:%s:%Y'|'%d:%i') [ "$#" -eq 3 ] || delegate "$@" ;; '%d:%i:%s:%Y:%u:%a:%h') ;; *) delegate "$@" ;; esac ;;
            -f) case "$2" in '%u:%Lp:%l'|'%d:%i:%z:%m'|'%d:%i') [ "$#" -eq 3 ] || delegate "$@" ;; '%d:%i:%z:%m:%u:%Lp:%l') ;; *) delegate "$@" ;; esac ;;
            *) delegate "$@" ;;
        esac ;;
    uname)
        [ "$#" -eq 1 ] && [ "$1" = -s ] || delegate "$@"
        platform=${model#'{"platform": "'}
        [ "$platform" != "$model" ] || delegate "$@"
        platform=${platform%%\"*}
        case "$platform" in ''|*[!A-Za-z0-9._-]*) delegate "$@" ;; esac
        printf '%s\n' "$name" >> "$FIXTURE_ROOT/control/spawns.log"
        printf '%s\n' "$platform"; exit 0 ;;
    plutil)
        # `plutil -extract Label|EnvironmentVariables.HERDR_RELAY_ENV raw -o - FILE` on a definition that is, byte for byte, plistlib's
        # dump of {Label, EnvironmentVariables: {HERDR_RELAY_ENV}} (the template is plistlib's own, cut at the two values by install_stub):
        # the values are cut out of the file, the template is REBUILT around them and `cmp` proves the file is exactly that text.
        case "$model" in *decode-fail*|*label-malformed-output*) delegate "$@" ;; esac
        [ "$#" -eq 6 ] && [ "$1" = -extract ] || delegate "$@"
        [ "$2" = Label ] || [ "$2" = EnvironmentVariables.HERDR_RELAY_ENV ] || delegate "$@"
        [ "$3:$4:$5" = raw:-o:- ] && [ -f "$6" ] && [ -r "$6" ] || delegate "$@"
        t1='@T1@'; t2='@T2@'; t3='@T3@'; c=
        while IFS= read -r line; do c=$c$line'
'; done < "$6"
        v=${c#"$t1"}; one=${v%%"$t2"*}; v=${v#*"$t2"}; two=${v%%"$t3"*}
        case "$one$two" in ''|*[!A-Za-z0-9._/+=@:,~%-]*) delegate "$@" ;; esac
        printf '%s' "$t1$one$t2$two$t3" > "$control/plist.expect"
        /usr/bin/cmp -s "$6" "$control/plist.expect" || delegate "$@"
        printf '%s\n' "$name" >> "$control/spawns.log"
        if [ "$2" = @FIRST@ ]; then printf '%s\n' "$one"; else printf '%s\n' "$two"; fi; exit 0 ;;
    systemctl|launchctl)
        # The canonical two-label head is parsed here and REBUILT from the parsed values: a text that is not exactly the rebuilt
        # head, or whose head holds a backslash (any JSON escape), is the adapter's. Whatever follows the head stays opaque ($rest).
        build() {
            XA=; [ -z "$CA" ] || XA=', "configuration_queries": '$CA; XB=; [ -z "$CB" ] || XB=', "configuration_queries": '$CB
            head='{"platform": "'$P'", "files": {"'$LA'": "'$FA'", "'$LB'": "'$FB'"}, "fault": "'$X'", "fault_label": "'$FL'", "route_status": '$R', "states": {"'$LA'": {"state": "'$SA'", "enabled": '$EA', "queries": '$QA$XA'}, "'$LB'": {"state": "'$SB'", "enabled": '$EB', "queries": '$QB$XB'}}'
        }
        close='}'; cq=', "configuration_queries": '; CA=; CB=
        r=${model#'{"platform": "'}; P=${r%%\"*}
        r=${r#*'", "files": {"'}; LA=${r%%\"*}; r=${r#*'": "'}; FA=${r%%\"*}
        r=${r#*'", "'}; LB=${r%%\"*}; r=${r#*'": "'}; FB=${r%%\"*}
        r=${r#*'"}, "fault": "'}; X=${r%%\"*}; r=${r#*'", "fault_label": "'}; FL=${r%%\"*}
        r=${r#*'", "route_status": '}; R=${r%%,*}
        r=${r#*', "states": {"'}; r=${r#*'": {"state": "'}; SA=${r%%\"*}
        r=${r#*'", "enabled": '}; EA=${r%%,*}; r=${r#*', "queries": '}; QA=${r%%"$close"*}
        case "$QA" in *"$cq"*) CA=${QA#*"$cq"}; QA=${QA%%,*} ;; esac
        r=${r#*'}, "'}; r=${r#*'": {"state": "'}; SB=${r%%\"*}
        r=${r#*'", "enabled": '}; EB=${r%%,*}; r=${r#*', "queries": '}; QB=${r%%"$close"*}
        case "$QB" in *"$cq"*) CB=${QB#*"$cq"}; QB=${QB%%,*} ;; esac
        build
        case "$head" in *\\*) delegate "$@" ;; esac
        case "$model" in "$head"*) rest=${model#"$head"} ;; *) delegate "$@" ;; esac
        for v in "$EA" "$EB"; do case "$v" in true|false) ;; *) delegate "$@" ;; esac; done
        for v in "$QA" "$QB"; do case "$v" in ''|*[!0-9]*) delegate "$@" ;; esac; done
        for v in "$CA" "$CB"; do case "$v" in *[!0-9]*) delegate "$@" ;; esac; done
        L=; op=
        if [ "$name" = systemctl ]; then
            L=$3
            case "$#:$2" in
                7:show) [ "$4" = --property=LoadState ] && [ "$5" = --property=ActiveState ] && [ "$6" = --property=UnitFileState ] && [ "$7" = --no-pager ] && op=observe ;;
                12:show) [ "$4" = --all ] && [ "$5" = --property=Id ] && [ "$6" = --property=LoadState ] && [ "$7" = --property=ActiveState ] && [ "$8" = --property=UnitFileState ] &&
                         [ "$9" = --property=FragmentPath ] && [ "${10}" = --property=DropInPaths ] && [ "${11}" = --property=NeedDaemonReload ] && [ "${12}" = --no-pager ] && op=config ;;
                3:stop|3:disable) op=$2 ;;
                2:daemon-reload) op=reload ;;
            esac
        else
            case "$1:$2" in
                print:"$domain") op=domain ;;
                print-disabled:"$domain") op=disabled ;;
                print:"$domain"/*|bootout:"$domain"/*|disable:"$domain"/*) L=${2#"$domain/"}; op=$1; [ "$op" != print ] || op=observe ;;
            esac
        fi
        [ -n "$op" ] || delegate "$@"
        # Nothing may apply to the call: no fault at all for the calls on no label, none of this label's own otherwise (a fault of
        # the other label does not touch it), no effective-configuration extras for a configuration query, no native_done gate for
        # an observation under a FULL adapter (full=1). Every doubt is the adapter's.
        case "$op" in
            domain|disabled|reload) [ -z "$X" ] || delegate "$@" ;;
            *) [ -z "$X" ] || [ "$FL" != "$L" ] || delegate "$@"
               if [ "$L" = "$LA" ]; then S=$SA; E=$EA; F=$FA; Q=$QA; C=$CA; elif [ "$L" = "$LB" ]; then S=$SB; E=$EB; F=$FB; Q=$QB; C=$CB; else delegate "$@"; fi ;;
        esac
        case "$op:$full:$rest" in observe:1:*native_done*) delegate "$@" ;; config:*:*overlays*|config:*:*metadata_overrides*|config:*:*configuration_drift*) delegate "$@" ;; esac
        printf '%s\n' "$name" >> "$control/spawns.log"
        event=
        case "$op" in
            domain) printf '%s = {}\n' "$domain"; exit 0 ;;
            disabled)
                printf 'disabled services = {\n'
                for row in "$LA:$EA" "$LB:$EB"; do if [ "${row#*:}" = true ]; then v=false; else v=true; fi; printf '    "%s" => %s\n' "${row%:*}" "$v"; done
                printf '}\n'; exit 0 ;;
            reload) printf '{"op": "reload", "label": ""}\n' >> "$control/events.jsonl"; exit 0 ;;
            observe) Q=$((Q + 1)); event='{"op": "observe", "label": "'$L'", "state": "'$S'", "enabled": '$E'}' ;;
            config) C=$((${C:-0} + 1)); event='{"op": "configuration-observe", "label": "'$L'", "query": '$C'}' ;;
            disable) E=false; event='{"op": "disable", "label": "'$L'"}' ;;
            *) if [ "$P" = Linux ]; then S=inactive; else S=absent; fi; event='{"op": "'$op'", "label": "'$L'"}' ;;
        esac
        if [ "$L" = "$LA" ]; then SA=$S; EA=$E; QA=$Q; CA=$C; else SB=$S; EB=$E; QB=$Q; CB=$C; fi
        build
        printf '%s' "$head$rest" > "$control/model.json"
        # the copy the other fast stubs trust follows, unless a FULL adapter's stat call would now ARM this model
        if [ "$full" = 0 ] || [ "$op" != observe ] || [ "$QA" -lt 4 ] || [ "$QB" -lt 4 ] || [ -e "$FA" ] || [ -e "$FB" ]; then printf '%s' "$head$rest" > "$control/model.valid"; fi
        printf '%s\n' "$event" >> "$control/events.jsonl"
        if [ -f "$F" ] || [ "$S" != absent ]; then loaded=loaded; if [ "$E" = true ]; then unit=enabled; else unit=disabled; fi; frag=$F; else loaded=not-found; unit=; frag=; fi
        if [ "$S" = absent ]; then active=inactive; else active=$S; fi
        case "$op:$name" in
            observe:systemctl) printf 'LoadState=%s\nActiveState=%s\nUnitFileState=%s\n' "$loaded" "$active" "$unit" ;;
            config:*) printf 'Id=%s\nLoadState=%s\nActiveState=%s\nUnitFileState=%s\nFragmentPath=%s\nDropInPaths=\nNeedDaemonReload=no\n' "$L" "$loaded" "$active" "$unit" "$frag" ;;
            observe:launchctl)
                if [ "$S" = absent ]; then printf 'Could not find service "%s" in domain for user gui: %s\n' "$L" "${domain#gui/}"; exit 113; fi
                printf '%s/%s = {\n    state = waiting\n}\n' "$domain" "$L" ;;
        esac
        exit 0 ;;
    *)
        case "$model" in *read-fail*) delegate "$@" ;; esac ;;
esac
printf '%s\n' "$name" >> "$FIXTURE_ROOT/control/spawns.log"
exec "/usr/bin/$name" "$@"
"""


def install_stub(bin_dir, name, adapter, fast=None, model_shape='True'):
    # Every stub is the Python adapter, except that the FAST_TOOLS get FAST_STUB in front of it,
    # with the adapter and the MODEL_CHECK validator beside it in bin/.python. model_shape is
    # one Python expression of `model` (a decoded JSON object) that must hold for the validator to cache a
    # model: it states what this adapter dereferences for stat, head and wc beyond 'fault'. 'True' for
    # ADAPTER; the FULL_ADAPTER of tests/test_uninstall.sh passes the structure of files, shared and
    # states that its prefix indexes on every call. A model that fails it is delegated, so the adapter
    # fails as before instead of the stub running the real tool. A cached control/model.valid was
    # validated by the previously installed validator, so it is dropped.
    fast = name in FAST_TOOLS if fast is None else fast
    assert 'def shape(model): return True' in MODEL_CHECK and '\n' not in model_shape
    # -S: the adapters import only the standard library, so the interpreter skips the site module (about 6 ms of 29 locally).
    # Every adapter bakes the same shape check into the save() that keeps control/model.valid in step.
    python_stub = '#!' + sys.executable + ' -S\n' + adapter.replace('def shape(model): return True', 'def shape(model): return ' + model_shape, 1)
    if fast:
        hidden = bin_dir / '.python'; hidden.mkdir(mode=0o700, exist_ok=True)
        (hidden / name).write_text(python_stub); (hidden / name).chmod(0o700)
        check = MODEL_CHECK.replace('def shape(model): return True', 'def shape(model): return ' + model_shape, 1)
        if not (hidden / '.model-ok').exists() or (hidden / '.model-ok').read_text() != '#!' + sys.executable + ' -S\n' + check:   # once per bin directory
            (hidden / '.model-ok').write_text('#!' + sys.executable + ' -S\n' + check); (hidden / '.model-ok').chmod(0o700)
        (bin_dir.parent / 'control' / 'model.valid').unlink(missing_ok=True)
        dumped = plistlib.dumps({'Label': '@L@', 'EnvironmentVariables': {'HERDR_RELAY_ENV': '@E@'}}).decode()   # plistlib sorts the keys: its order is the template's
        template = dumped.replace('@L@', '\0').replace('@E@', '\0').split('\0')
        stub = FAST_STUB.replace('@DOMAIN@', 'gui/%d' % os.getuid()).replace('@FULL@', '0' if model_shape == 'True' else '1')
        stub = stub.replace('@FIRST@', 'Label' if dumped.index('@L@') < dumped.index('@E@') else 'EnvironmentVariables.HERDR_RELAY_ENV')
        for number, text in enumerate(template, 1): stub = stub.replace('@T%d@' % number, text)
        (bin_dir / name).write_text(stub); (bin_dir / name).chmod(0o700)
    else:
        (bin_dir / name).write_text(python_stub); (bin_dir / name).chmod(0o700)


def effects(events):
    # Read-only admission is allowed before route disposition. Unexpected
    # effects remain visible, including forbidden calls with swallowed stderr.
    return [e for e in events if e['op'] not in ('configuration-observe', 'observe', 'lifecycle-capture', 'domain-error', 'poll-delay')]


def snapshot(q: Path) -> tuple:
    if q.is_symlink(): return ('link', stat.S_IMODE(q.lstat().st_mode), os.readlink(q))
    if not q.exists(): return ('absent',)
    if q.is_file():
        original_mode = stat.S_IMODE(q.stat().st_mode)
        # Snapshot-only owner-read borrowing is restored BEFORE invocation.
        if not original_mode & stat.S_IRUSR: q.chmod(original_mode | stat.S_IRUSR)
        try: data = q.read_bytes()
        finally: q.chmod(original_mode)
        return ('file', original_mode, data)
    return ('directory', stat.S_IMODE(q.stat().st_mode), tuple((x.name, snapshot(x)) for x in sorted(q.iterdir())))


INVOCATIONS = [0]


def spawn_tally(control):
    # Diagnostic only, never an assertion. One record per requested tool process,
    # whether FAST_STUB execs it or the Python adapter handles it. Validator/cmp
    # overhead is additional; this is not a count of all underlying OS processes.
    counts = {}
    try: entries = (Path(control) / 'spawns.log').read_text().splitlines()
    except OSError: return counts
    for entry in entries: counts[entry] = counts.get(entry, 0) + 1
    return counts


def spawn_report(before, after):
    delta = {name: after[name] - before.get(name, 0) for name in sorted(after) if after[name] != before.get(name, 0)}
    return 'stub spawns %d %s' % (sum(delta.values()), delta)
# Every synthetic credential sentinel the removal fixtures seed, including the full-uninstall
# suite's source-checkout one. The no-leak checks compare output with these after a normal
# return; the timeout diagnostic below must honour the same list.
CANARIES = (b'fixture-saved-canary', b'fixture-ambient-canary', b'fixture-ambient-github-canary',
            b'private-source-canary')


def group_rows(pid):
    # One bounded ps: the invocation's group and its members' descendants as bytes fields pid ppid pgid stat uid [command].
    listing = subprocess.run(['/bin/ps', 'axww', '-o', 'pid=,ppid=,pgid=,stat=,uid=,command='], stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, timeout=5).stdout
    rows = [f for f in (line.split(None, 5) for line in listing.splitlines()) if len(f) >= 5 and all(x.isdigit() for x in f[:3])]
    members = {str(pid).encode()}
    while True:
        grown = members | {r[0] for r in rows if r[1] in members or r[2] in members}
        if grown == members: break
        members = grown
    return [r for r in rows if r[0] in members]


def group_states(pid):
    # Timeout evidence, never the environment: pid ppid pgid stat uid and command of the invocation's group and of its
    # members' descendants. A row holding a fixture canary is withheld like a trace line; an unusable ps is reported
    # and changes nothing.
    try: mine = group_rows(pid)
    except (OSError, subprocess.SubprocessError) as error: return 'process states unavailable (%s)\n' % type(error).__name__
    shown = [r for r in mine if not any(canary in b' '.join(r) for canary in CANARIES)]
    return 'invocation group %d states, pid ppid pgid stat uid command (%d row(s) withheld):\n%s\n' % (
        pid, len(mine) - len(shown), '\n'.join(b' '.join(r[:5] + [r[5][:160] if len(r) > 5 else b'']).decode('utf-8', 'replace') for r in shown))


def failure_states(process):
    # Called first in each invocation's finally: whatever failure is propagating (a timeout, a handshake or prompt
    # deadline, a failed assertion), the group's states are recorded before the master is released or anything is
    # signalled. A normal return records nothing.
    if sys.exc_info()[0] is not None: sys.stderr.write(group_states(process.pid)); sys.stderr.flush()


def contain(process, bystander=False):
    # Fallback containment of one invocation's own group, run whether or not its leader is still alive (a descendant can
    # outlive the leader and hold the output pipe); never a pass and never counted as manager cleanup. macOS answers
    # killpg with EPERM while the leader is still exiting (ps state E) and ESRCH once nothing is left. A refusal is
    # reported instead of aborting the suite: every listed member is then signalled by pid, the leader is reaped within
    # the same 2s bound, survivors are reported, and the failure in flight (a timeout, a failed assertion) keeps
    # propagating unchanged. A bystander is meant to be live here, so only its refusals are reported.
    live = process.poll() is None; failing = sys.exc_info()[0] is not None; problem = live and not bystander; note = 'killpg ok'
    refused = False; left = []
    try: os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError: note = 'killpg ESRCH (group already gone)'
    except PermissionError:
        note = 'killpg EPERM (leader still exiting?)'; problem = refused = True
        try: pids = [int(r[0]) for r in group_rows(process.pid)]
        except (OSError, subprocess.SubprocessError): pids = []
        for pid in {process.pid, *pids}:
            try: os.kill(pid, signal.SIGKILL)
            except (ProcessLookupError, PermissionError) as error: note += '; kill %d %s' % (pid, type(error).__name__)
    try: process.wait(timeout=2)
    except subprocess.TimeoutExpired: note += '; NOT reaped within 2s'; problem = True
    deadline = time.monotonic() + 2
    while refused:
        try: left = [r[0].decode() for r in group_rows(process.pid) if not r[3].startswith(b'Z')]
        except (OSError, subprocess.SubprocessError): break
        if not left or time.monotonic() > deadline: break
        time.sleep(.05)
    if left: note += '; still listed: ' + ','.join(left); problem = True
    if problem or failing: sys.stderr.write('removal fixture containment of group %d (leader %s): %s\n' % (
        process.pid, 'still running' if live else 'already reaped', note)); sys.stderr.flush()


def drain_pty(master):
    # The answer written to a PTY master is echoed by the slave's line discipline into a queue nobody reads. On macOS
    # the reader's LAST close of the slave then blocks inside its exit (ps state E, stdout already at EOF, wait() not
    # returning, killpg answering EPERM) until that queue is read or the master closes. Read and discard it: prompts
    # and output travel on the stdout pipe and nothing asserts on the echo. Returns the stop-and-join callable.
    stop = threading.Event()
    def pump():
        while not stop.is_set():
            try:
                if select.select([master], [], [], .05)[0] and not os.read(master, 4096): return
            except OSError: return
    thread = threading.Thread(target=pump, daemon=True); thread.start()
    def finish(): stop.set(); thread.join()
    return finish


def bounded_communicate(process, data=None, timeout=None):
    # The same bound and the same failure as process.communicate(timeout=TIMEOUT).
    # Evidence only: when the bound expires, show the tail of the traced invocation
    # captured so far (the traceback alone omits it), then re-raise unchanged. The
    # callers' canary check runs only after a NORMAL return, so this timeout path keeps
    # its own guard: a line holding any fixture canary is withheld from the diagnostic,
    # and a canary anywhere in the captured output fails exactly like the normal check.
    timeout = TIMEOUT if timeout is None else timeout
    try:
        return process.communicate(data, timeout=timeout)
    except subprocess.TimeoutExpired as expired:
        captured = expired.output or b''
        tail = captured.splitlines()[-40:]
        shown = [line for line in tail if not any(canary in line for canary in CANARIES)]
        sys.stderr.write('removal fixture invocation exceeded %ds; trace tail (%d line(s) withheld):\n%s\n' % (
            timeout, len(tail) - len(shown), b'\n'.join(shown).decode('utf-8', 'replace')))
        sys.stderr.write(group_states(process.pid))
        sys.stderr.flush()
        if any(canary in captured for canary in CANARIES): raise AssertionError('tracing/output leaked a canary') from expired
        raise


def timeout_diagnostics_control():
    # The timeout diagnostic is evidence, not a leak path: an ordinary traced line is shown, a
    # line holding a canary is never written and fails like the normal no-leak check. Each
    # child prints once, flushes and then idles; it is killed by its own process group.
    import contextlib, io
    code = 'import sys, time; sys.stdout.write(sys.argv[1] + chr(10)); sys.stdout.flush(); time.sleep(30)'
    for text, expected in (('ordinary-trace-line', subprocess.TimeoutExpired), ('GH_TOKEN=' + CANARIES[0].decode(), AssertionError)):
        child = subprocess.Popen([sys.executable, '-c', code, text], stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                 stdin=subprocess.DEVNULL, start_new_session=True)
        captured = io.StringIO()
        try:
            time.sleep(1)
            with contextlib.redirect_stderr(captured):
                try: bounded_communicate(child, timeout=1)
                except expected: pass
                else: raise AssertionError('timeout diagnostic control did not time out')
        finally:
            os.killpg(child.pid, signal.SIGKILL); child.wait(timeout=5)
        written = captured.getvalue()
        assert 'fixture-saved-canary' not in written, 'timeout diagnostic wrote a canary'
        assert ('ordinary-trace-line' in written) == (expected is subprocess.TimeoutExpired)


def fast_stub_equivalence_control(adapter=None, model_shape='True'):
    # Hosted-only control, not a product assertion. On the same model, the fast stat/head/wc stub and
    # the Python adapter must give the same stdout, exit status, events and spawn tally: every stat
    # format of this host's family on a fixture file, argument forms the adapter refuses, each model
    # fault and alias that changes its answer, and head and wc with and without read-fail. A control
    # model that is missing, empty, not JSON, malformed brace-delimited JSON, a trailing comma, has
    # trailing data on the same or on a second line, or ends in a NUL byte must fail closed in BOTH
    # implementations: a nonzero status, no output, no event and no spawn record, never a run of the
    # real tool. A model that is valid for one call and then overwritten must not ride on the earlier
    # validation: with broken bytes, or the same bytes plus a NUL byte, the second call fails closed
    # with no new spawn record; with another VALID model that carries a fault, the stub re-validates
    # and delegates, and both implementations give the same answer. Valid JSON that is not canonical
    # json.dumps text (escaped fault values and keys, duplicate keys, other whitespace) must be rejected
    # by the validator and delegated, so the stub never answers from raw text where the adapter decodes
    # a fault; a reordered but canonical model may take the fast path and only has to agree.
    adapter = ADAPTER if adapter is None else adapter
    def scrub(data, root):
        # A traceback names its script: the delegated copy sits in bin/.python, the other in bin, each
        # under its own root. Scrubbed, the two tracebacks are the same text.
        return data.replace(str(root).encode(), b'ROOT').replace(b'/bin/.python/', b'/bin/').replace(b'/usr/bin/', b'')
    with tempfile.TemporaryDirectory(prefix='herdr-fast-stub-') as temporary:
        base = Path(temporary).resolve()
        subject = base / 'subject.txt'; subject.write_text('fast stub subject\n'); subject.chmod(0o600)
        alias = base / 'alias.txt'; alias.write_text('fast stub alias\n'); alias.chmod(0o600)
        darwin = sys.platform == 'darwin'
        flag = '-f' if darwin else '-c'
        formats = ('%u:%Lp:%l', '%d:%i:%z:%m', '%d:%i', '%d:%i:%z:%m:%u:%Lp:%l') if darwin else ('%u:%a:%h', '%d:%i:%s:%Y', '%d:%i', '%d:%i:%s:%Y:%u:%a:%h')
        plain = {'platform': 'Linux', 'fault': '', 'files': {}, 'states': {}, 'fault_label': '', 'shared': {}}
        # FULL_ADAPTER's prefix arms and STORES native_done on every call, stat, head and wc included, once all model
        # files are absent and every state was queried 4 times (this empty model is that case). The shape check bakes
        # the same condition in, so the stub delegates such a model and the adapter arms it. `settled` is the model
        # already armed: it stays fast-path eligible. The final model.json is compared too, not only the output.
        settled = dict(plain, native_done=True) if model_shape != 'True' else plain
        identity = [flag, '%d:%i', str(subject)]
        cases = [('stat', [flag, fmt, str(subject)], plain) for fmt in formats]
        cases += [('stat', [flag, '%U', str(subject)], plain), ('stat', [flag, formats[0]], plain),
                  ('stat', [flag, formats[2], str(subject), 'extra'], plain)]
        cases += [('stat', [flag, formats[0], str(subject)], dict(plain, fault=fault)) for fault in ('stat-fail', 'stat-malformed')]
        # The combined form may name several paths (an ancestor walk); with a fault it must still fail like the adapter.
        fixed = base / 'fixed'; fixed.mkdir()  # a directory nothing else touches, so both runs see the same size and mtime
        walk = [flag, formats[3], str(subject), str(alias), str(fixed)]
        cases += [('stat', walk, plain), ('stat', walk + [str(base / 'missing')], plain)]
        cases += [('stat', walk, dict(plain, fault=fault)) for fault in ('stat-fail', 'stat-malformed')]
        cases += [('stat', identity, dict(plain, fault=fault, role_identity_fault_path=str(subject)))
                  for fault in ('role-identity-fail', 'role-identity-malformed')]
        cases += [('stat', identity, dict(plain, identity_aliases={str(subject): str(alias)}))]
        for tool, args in (('head', ['-n', '1', str(subject)]), ('wc', ['-c', str(subject)])):
            cases += [(tool, args, plain), (tool, args, dict(plain, fault='read-fail'))]
        # uname answers the decoded model's platform for exactly `-s`; the adapter refuses every other argument form and ignores faults.
        cases += [('uname', args, plain) for args in (['-s'], ['-a'], [], ['-s', '-s'], ['-m'], ['-s', 'extra'], ['--', '-s'])]
        cases += [('uname', ['-s'], dict(plain, platform=value)) for value in ('Darwin', 'Unsupported')]
        cases += [('uname', ['-s'], dict(plain, fault='read-fail'))]
        if settled is not plain: cases += [(tool, args, settled) for tool, args, model in cases if model is plain]
        cases += [(tool, args, dict(settled, extra='X' * 70000)) for tool, args in (('stat', [flag, formats[0], str(subject)]), ('uname', ['-s']), ('head', ['-n', '1', str(subject)]))]  # read with cat
        # plutil: a definition that is exactly plistlib's dump of the two values is answered by the stub, every other file, key, argument
        # form and fault is the adapter's (the stub cuts the values out, rebuilds the dump around them and proves it with cmp).
        plists = base / 'plists'; plists.mkdir()
        def dump(label='com.herdr-mobile-relay.service', env='/p/relay.env', **options): return plistlib.dumps({'Label': label, 'EnvironmentVariables': {'HERDR_RELAY_ENV': env}}, **options)
        good = dump()
        variants = [good, dump(label='com herdr'), dump(env='/a&b'), dump(env='/p/\u00e9'), good[:-1], good + b'\n', good + b'\0', good + b' ', b'', b'not a plist', dump(fmt=plistlib.FMT_BINARY),
                    plistlib.dumps({'Label': 'com.herdr-mobile-relay.service'}), plistlib.dumps({'Label': 7, 'EnvironmentVariables': {'HERDR_RELAY_ENV': '/x'}}),
                    plistlib.dumps({'EnvironmentVariables': {'HERDR_RELAY_ENV': '/p/relay.env'}, 'Label': 'com.herdr-mobile-relay.service'}), dump(label='\u00e9')]
        for number, data in enumerate(variants): (plists / str(number)).write_bytes(data)
        extract = ['-extract', 'Label', 'raw', '-o', '-']
        cases += [('plutil', ['-extract', key, 'raw', '-o', '-', str(plists / str(number))], settled) for number in range(len(variants)) for key in ('Label', 'EnvironmentVariables.HERDR_RELAY_ENV')[:2 if number in (0, 13) else 1]]
        cases += [('plutil', extract + [str(plists / '0')], dict(settled, fault=fault)) for fault in ('decode-fail', 'label-decode-fail', 'label-malformed-output')]
        cases += [('plutil', args, settled) for args in (['-extract', 'Other', 'raw', '-o', '-', str(plists / '0')], ['-extract', 'Label', 'xml1', '-o', '-', str(plists / '0')], extract,
                                                         extract + [str(plists / 'missing')], extract + [str(plists)], extract + [str(plists / '0'), 'extra'],
                                                         ['-remove', 'Label', 'raw', '-o', '-', str(plists / '0')])]
        broken_models = (None, '', 'not json', '{broken}', '{not json}', '{"fault": "",}',
                         '{"fault": ""}{"fault": ""}', '{"fault": ""}\ntrailing', json.dumps(plain) + '\0')
        for tool, args in (('head', ['-n', '1', str(subject)]), ('wc', ['-c', str(subject)]), ('stat', [flag, formats[0], str(subject)]), ('uname', ['-s'])):
            cases += [(tool, args, text) for text in broken_models]
        for index, (tool, args, model) in enumerate(cases):
            results = []
            broken = model is None or isinstance(model, str)
            for fast in (True, False):
                root = base / ('case-%d-%s' % (index, 'fast' if fast else 'python'))
                (root / 'control').mkdir(parents=True); (root / 'bin').mkdir()
                if model is not None: (root / 'control' / 'model.json').write_text(model if broken else json.dumps(model))
                install_stub(root / 'bin', tool, adapter, fast=fast, model_shape=model_shape)
                done = subprocess.run([str(root / 'bin' / tool)] + args, env={'FIXTURE_ROOT': str(root), 'PATH': '/usr/bin:/bin'},
                                      stdin=subprocess.DEVNULL, capture_output=True, timeout=30)
                def read(name):
                    try: return (root / 'control' / name).read_text()
                    except OSError: return ''
                results.append((done.returncode, done.stdout, scrub(done.stderr, root), read('events.jsonl'), read('spawns.log'), read('model.json')))
            if broken:
                for status, output, _, events, spawns, _ in results:
                    assert status != 0 and output == b'' and events == '' and spawns == '', 'broken control model did not fail closed for %s: %r' % (tool, results)
                continue
            assert results[0] == results[1], 'fast stub differs from the Python adapter for %s %r: %r != %r' % (tool, args, results[0], results[1])
        stale = (('head', ['-n', '1', str(subject)]), ('wc', ['-c', str(subject)]), ('stat', [flag, formats[0], str(subject)]), ('uname', ['-s']))
        for tool, args in stale:
            for kind in ('broken', 'nul', 'fault', 'big'):
                # 'fault' is a different VALID model: the stub must re-validate it and delegate, so the adapter
                # records its own spawn before it fails. The other two abort before any spawn record. 'big' is 'fault' on a model of
                # more than 4096 bytes, which the stub compares with cmp instead of in bash.
                initial = dict(settled, extra='X' * 70000) if kind == 'big' else settled
                if kind == 'broken': rewritten, spawned = b'{broken}', 1
                elif kind == 'nul': rewritten, spawned = json.dumps(settled).encode() + b'\x00', 1
                elif tool == 'uname': rewritten, spawned = json.dumps({k: v for k, v in initial.items() if k != 'platform'}).encode(), 2  # uname ignores faults
                else: rewritten, spawned = json.dumps(dict(initial, fault='stat-fail' if tool == 'stat' else 'read-fail')).encode(), 2
                results = []
                for fast in (True, False):
                    root = base / ('stale-%s-%s-%s' % (tool, kind, 'fast' if fast else 'python'))
                    (root / 'control').mkdir(parents=True); (root / 'bin').mkdir()
                    model_path = root / 'control' / 'model.json'; model_path.write_text(json.dumps(initial))
                    install_stub(root / 'bin', tool, adapter, fast=fast, model_shape=model_shape)
                    calls = []
                    for data in (None, rewritten):
                        if data is not None: model_path.write_bytes(data)
                        done = subprocess.run([str(root / 'bin' / tool)] + args, env={'FIXTURE_ROOT': str(root), 'PATH': '/usr/bin:/bin'},
                                              stdin=subprocess.DEVNULL, capture_output=True, timeout=30)
                        calls.append((done.returncode, done.stdout, done.stderr.replace(b'/usr/bin/', b'')))
                    def control_text(name):
                        try: return (root / 'control' / name).read_text()
                        except OSError: return ''
                    first, second = calls
                    assert first[0] == 0, 'valid model did not run the real %s: %r' % (tool, calls)
                    assert second[0] != 0 and second[1] == b'', 'a model rewritten as %s after a valid call did not fail closed for %s: %r' % (kind, tool, calls)
                    assert control_text('events.jsonl') == '' and control_text('spawns.log') == (tool + '\n') * spawned, 'second call left an event or an unexpected spawn record for %s (%s)' % (tool, kind)
                    results.append((first, second[:2], control_text('events.jsonl'), control_text('spawns.log'), control_text('model.json')))
                assert results[0] == results[1], 'fast stub differs from the Python adapter after a model was rewritten as %s for %s: %r != %r' % (kind, tool, results[0], results[1])
        # Spellings that are valid JSON but not canonical json.dumps text, built from the same plain model.
        # The adapter decides faults from the DECODED model; the stub's raw-text globs see ASCII tokens only,
        # so each of these must be rejected by MODEL_CHECK and delegated, and the stub and the adapter must
        # give the same answer (the oracle is the adapter on the same model). expect: 'fail' = the adapter
        # refuses or fails (nonzero status, no stdout, so no host tool ran); 'differs' = the adapter answers
        # differently from the real tool on a plain model (its output is not the real tool's); 'same' = only
        # agreement. A reordered but canonical model may legitimately take the fast path: agreement only.
        def escaped(model, *tokens, separators=None):
            text = json.dumps(model, separators=separators)
            for token in tokens: text = text.replace(token, ''.join('\\u%04x' % ord(c) if c in '-_' else c for c in token))
            return text
        canonical = json.dumps(settled)
        stat_args = [flag, formats[0], str(subject)]
        tools = (('stat', stat_args), ('head', ['-n', '1', str(subject)]), ('wc', ['-c', str(subject)]))
        fault_of = {'stat': 'stat-fail', 'head': 'read-fail', 'wc': 'read-fail'}
        strict = model_shape != 'True'  # a FULL_ADAPTER-style shape check is in force
        spelled = [('stat', stat_args, escaped(dict(settled, fault='stat-malformed'), 'stat-malformed'), 'differs')]
        spelled += [('stat', identity, escaped(dict(settled, fault=fault, role_identity_fault_path=str(subject)), *tokens), expect)
                    for fault, expect in (('role-identity-fail', 'fail'), ('role-identity-malformed', 'differs'))
                    for tokens in ((fault,), (fault, 'role_identity_fault_path'))]
        spelled += [('stat', identity, escaped(dict(settled, identity_aliases={str(subject): str(alias)}), 'identity_aliases'), 'differs')]
        for tool, args in tools:
            fault = fault_of[tool]
            spelled += [(tool, args, escaped(dict(settled, fault=fault), fault), 'fail'),
                        (tool, args, escaped(dict(settled, fault=fault), fault, separators=(',', ':')), 'fail'),
                        (tool, args, canonical[:-1] + ', "fault": "%s"}' % fault, 'fail'),
                        (tool, args, '{"fault": "%s", ' % fault + canonical[1:], 'same'),
                        (tool, args, json.dumps(settled, separators=(',', ':')), 'same'),
                        (tool, args, ' ' + canonical, 'same'),
                        (tool, args, json.dumps(dict(reversed(list(settled.items())))), 'same'),
                        (tool, args, json.dumps(dict(reversed(list(dict(settled, fault=fault).items())))), 'fail'),
                        (tool, args, '{"fault": ""}', 'crash' if strict else 'same')]
        # uname: the platform is read from the raw text only when it is the canonical first key holding a plain token, and decoded
        # by the adapter otherwise (a value with a quote, backslash, space, newline or non-ASCII, a number, null, a list, a different
        # key order, an escaped spelling, a duplicate key); a model without a platform fails in both after the spawn record.
        def named(value): return json.dumps(dict(settled, platform=value))
        spelled += [('uname', ['-s'], text, 'same') for text in (
            [named(value) for value in ('Linux', 'Darwin', 'Unsupported', '', 'a b', 'Lin"ux', 'Lin\\ux', 'x\ny', '\u00dcn\u00efx', 5, None, ['Linux'])]
            + [canonical.replace('"Linux"', '"Lin\\u0075x"'), canonical[:-1] + ', "platform": "Darwin"}', ' ' + canonical,
               json.dumps(settled, separators=(',', ':')), json.dumps(dict(reversed(list(settled.items())))),
               json.dumps(dict(settled, fault='read-fail')), json.dumps(dict(settled, fault='stat-fail'))])]
        spelled += [('uname', ['-s'], json.dumps({k: v for k, v in settled.items() if k != 'platform'}), 'fail')]
        # Structures the FULL_ADAPTER prefix dereferences on every call. With the shape check in force the
        # prefix raises before its own spawn record ('crash') and the stub must delegate, not run the tool;
        # for the plain ADAPTER they are irrelevant and both run the real tool. A shared object of the wrong
        # type is delegated conservatively although the prefix does not dereference it for these tools,
        # and a realistic non-empty model must still be accepted.
        every = tools + (('uname', ['-s']),)  # uname takes the same shape check and the same arming rule
        for tool, args in every:
            spelled += [(tool, args, json.dumps(model), 'crash' if strict else 'same') for model in (
                            dict(plain, files=[]), dict(plain, states={'fixture': {}}),
                            dict(plain, states={'fixture': {'queries': '4'}}), dict(plain, files={'fixture': 5}))]
            spelled += [(tool, args, json.dumps(dict(plain, shared=[])), 'same'),
                        (tool, args, json.dumps(dict(plain, files={'fixture.service': str(subject)},
                                                     states={'fixture.service': {'state': 'active', 'enabled': True, 'queries': 0}})), 'same')]
        if strict:
            # The prefix's one mutation on a stat, head or wc call. Armed when no model file exists and every state was
            # queried 4 times and native_done is unset: the stub must delegate so the adapter arms and stores it.
            # Any other model leaves model.json byte for byte as written, on the fast path as well.
            def arming(queries=4, present=False, **extra):
                path = str(subject) if present else str(base / 'absent.service')
                return json.dumps(dict(plain, files={'fixture.service': path}, **extra,
                                       states={'fixture.service': {'state': 'absent', 'enabled': False, 'queries': queries}}))
            for tool, args in every:
                spelled += [(tool, args, arming(), 'armed')]
            spelled += [(tool, args, text, 'untouched') for tool, args in (('stat', stat_args), ('uname', ['-s']))
                        for text in (arming(3), arming(present=True), arming(native_done=True))]
        real = {}
        def real_output(tool, args):
            if (tool, tuple(args)) not in real:
                real[(tool, tuple(args))] = subprocess.run(['/usr/bin/' + tool] + args, stdin=subprocess.DEVNULL, capture_output=True, timeout=30).stdout
            return real[(tool, tuple(args))]
        for index, (tool, args, text, expect) in enumerate(spelled):
            results = []
            for fast in (True, False):
                root = base / ('spelled-%d-%s' % (index, 'fast' if fast else 'python'))
                (root / 'control').mkdir(parents=True); (root / 'bin').mkdir()
                (root / 'control' / 'model.json').write_text(text)
                install_stub(root / 'bin', tool, adapter, fast=fast, model_shape=model_shape)
                done = subprocess.run([str(root / 'bin' / tool)] + args, env={'FIXTURE_ROOT': str(root), 'PATH': '/usr/bin:/bin'},
                                      stdin=subprocess.DEVNULL, capture_output=True, timeout=30)
                def read(name):
                    try: return (root / 'control' / name).read_text()
                    except OSError: return ''
                results.append((done.returncode, done.stdout, scrub(done.stderr, root), read('events.jsonl'), read('spawns.log'), read('model.json')))
            # The adapter on the same model is the oracle: the COMPLETE result tuples, stderr included, must agree.
            assert results[0] == results[1], 'fast stub differs from the Python adapter for %s on %s: %r != %r' % (tool, text, results[0], results[1])
            # Whichever path is taken, fast or delegated, exactly one spawn is recorded, unless the adapter's
            # prefix raises before its own spawn record ('crash'); no model here may produce an event.
            spawned = 0 if expect == 'crash' else 1
            assert all(r[3] == '' and r[4] == (tool + '\n') * spawned for r in results), 'unexpected events or spawn tally for %s on %s: %r' % (tool, text, results)
            if expect in ('fail', 'crash'):
                assert all(r[0] != 0 and r[1] == b'' for r in results), 'a model the adapter refuses ran the real %s: %r for %s' % (tool, results, text)
            else:
                assert results[0][0] == 0, 'the real %s did not run for %s: %r' % (tool, text, results)
            if expect == 'differs': assert results[0][1] != real_output(tool, args), 'the real %s answered where the adapter answers differently, for %s' % (tool, text)
            if expect == 'armed': assert all(json.loads(r[5]).get('native_done') is True for r in results), 'native_done was not armed and stored for %s on %s: %r' % (tool, text, results)
            if expect == 'untouched': assert all(r[5] == text for r in results), 'model.json changed for %s on %s: %r' % (tool, text, results)
    print('fast stat/head/wc/uname/plutil stubs match the Python adapter on %d cases' % (len(cases) + 4 * len(stale) + len(spelled)), file=sys.stderr, flush=True)
    model_stub_control(adapter, model_shape)


def model_stub_control(adapter, model_shape):
    # Hosted-only control for the systemctl/launchctl calls the sh stub answers itself (observation, configuration query, stop,
    # disable, bootout, daemon-reload, launchctl print DOMAIN and print-disabled DOMAIN). Each model gets a CHAIN of calls run
    # through the stub and through the adapter alone, from the same bytes; after EVERY call stdout, status, stderr, events, spawn
    # tally and model.json must be equal (the chain makes each call see the state the last one left), and a model.valid either
    # implementation leaves (the stub, or the adapter's save()) must be one MODEL_CHECK accepts, never an armable model of a FULL
    # adapter. Delegations are counted by a wrapper around the adapter beside the stub: a plain model delegates only the chain's
    # deliberately bad calls (a dead fast path would pass everything else), while a fault of a label,
    # effective-configuration extras, a native_done gate, an escape, a non-canonical text or any shape but the canonical two labels
    # must delegate what it touches. The adapter text the stub mirrors is pinned by digest: edit it and this fails until the stub is
    # re-derived.
    assert hashlib.sha256(adapter.encode()).hexdigest() in MIRRORED, 'the adapter the sh stub mirrors has changed (digest %s): re-derive FAST_STUB, then update MIRRORED' % hashlib.sha256(adapter.encode()).hexdigest()
    full = model_shape != 'True'; domain = 'gui/%d' % os.getuid(); steps = 0
    with tempfile.TemporaryDirectory(prefix='herdr-model-stub-') as temporary:
        base = Path(temporary).resolve(); exists = base / 'exists.txt'; exists.write_text('exists\n')
        for platform in ('Linux', 'Darwin'):
            labels = ['herdr-mobile-relay.service', 'herdr-remote.service'] if platform == 'Linux' else ['com.herdr-mobile-relay.service', 'com.herdr-remote.service']
            on, off = ('active', 'inactive') if platform == 'Linux' else ('loaded', 'absent')
            def make(states=((on, True), (on, True)), queries=(0, 0), present=(True, True), fault='', fault_label=1, **extra):
                files = {label: str(exists if here else base / 'absent' / label) for label, here in zip(labels, present)}
                model = dict(platform=platform, files=files, fault=fault, fault_label=labels[fault_label], route_status=0,
                             states={label: dict(state=st, enabled=en, queries=q) for label, (st, en), q in zip(labels, states, queries)})
                if full: model['shared'] = dict(release='/r', config='/c', cache='/k', link='/l')
                model.update(extra); return model
            if platform == 'Linux':
                shows = [['--user', 'show', label, '--property=LoadState', '--property=ActiveState', '--property=UnitFileState', '--no-pager'] for label in labels]
                alls = [['--user', 'show', label, '--all', '--property=Id', '--property=LoadState', '--property=ActiveState', '--property=UnitFileState', '--property=FragmentPath',
                         '--property=DropInPaths', '--property=NeedDaemonReload', '--no-pager'] for label in labels]
                good = [shows[0], shows[1], alls[0], alls[1], ['--user', 'stop', labels[1]], ['--user', 'disable', labels[0]], ['--user', 'daemon-reload'], shows[0], shows[1]]
                bad = [['--user', 'show', labels[0]], ['--user', 'show', 'other.service'] + shows[0][3:]]
                cross = [('launchctl', ['bootout', domain + '/' + labels[1]]), ('launchctl', ['print', domain + '/' + labels[0]])]
            else:
                good = [['print', domain + '/' + labels[0]], ['print', domain + '/' + labels[1]], ['print', domain], ['print-disabled', domain], ['bootout', domain + '/' + labels[1]],
                        ['disable', domain + '/' + labels[0]], ['print', domain + '/' + labels[0]], ['print', domain + '/' + labels[1]]]
                bad = [['print', 'gui/0'], ['disable', domain], ['print', domain + '/']]
                cross = [('systemctl', ['--user', 'stop', labels[1]]), ('systemctl', ['--user', 'show', labels[0], '--property=LoadState', '--property=ActiveState', '--property=UnitFileState', '--no-pager'])]
            tool = 'systemctl' if platform == 'Linux' else 'launchctl'
            chain = [(tool, args) for args in good + bad]
            def pick(*indexes): return [chain[i] for i in indexes]
            odd = dict(reversed(list(make().items()))); third = make(); third['states']['x.service'] = dict(state=on, enabled=True, queries=0)
            single = make(); single['states'].popitem(); queued = make(); queued['states'][labels[1]].update(remaining=2, desired=off)
            counted = make(); [state.update(configuration_queries=1) for state in counted['states'].values()]; extras = [3] if platform == 'Linux' else []
            # (model or its text, delegated calls expected: 0 = only the deliberately bad ones of the full chain, 1 = more than those, calls)
            models = [(make(), 0, chain), (make(), 0, cross), (make(states=((off, False), (off, False)), present=(False, False), queries=(3, 3)), 1 if full else 0, pick(0, 1, 0, 1)),  # a FULL adapter ARMS it at (4, 4)
                      (make(queries=(3, 4), present=(False, False), states=((on, True), (off, False))), 1 if full else 0, pick(0, 1, 4, 0)), (counted, 0, pick(*([2, 3, 2] if extras else [0, 4]))),
                      (make(fault='query-fail', fault_label=0), 1, pick(0, 1, 2, 3)), (make(fault='stop-noop'), 1, pick(4, 1, 0)),
                      (make(fault='lifecycle-output', lifecycle_seam='systemd', lifecycle_text='X' * 70000, lifecycle_status=0), 1, pick(0, 1, 2)),
                      (make(lifecycle_text='X' * 70000), 0, pick(0, 1, 2, 3)),   # a big model is read with cat
                      (make(fault='domain-fail' if platform == 'Darwin' else 'reload-fail'), 1, pick(0, 2, 3, 6)), (make(fault='disabled-missing', fault_label=0), 1, pick(3, 0, 1)),
                      (make(native_done=True, late_label=labels[0], late_at=1, late_kind='active'), 1 if full else 0, pick(0, 1, 4)),
                      (odd, 1, pick(0, 1, 2)), (third, 1, pick(0, 1, 2)), (single, 1, pick(0, 1, 2)), (queued, 1, pick(0, 1, 2)), (make(platform='Odd\\x'), 1, pick(0, 1, 2)),
                      (json.dumps(make()).replace(', ', ','), 1, pick(0, 1)), (json.dumps(make()).replace('"fault": ""', '"fault":  ""'), 1, pick(0, 1))]
            if platform == 'Linux':   # effective-configuration extras are read by a systemd configuration query only
                models += [(make(overlays={labels[0]: '/o'}), 1, pick(2, 3, 0)), (make(metadata_overrides={labels[1]: dict(FragmentPath='')}), 1, pick(2, 3, 0)),
                           (make(configuration_drift_label=labels[0], configuration_drift_at=2, configuration_drift=dict(NeedDaemonReload='yes')), 1, pick(2, 3, 0))]
            roots = {}; checked = {}   # one fixture per implementation and platform; each model resets the control files
            for fast in (True, False):
                root = roots[fast] = base / ('chain-%s-%s' % (platform, 'fast' if fast else 'python')); (root / 'control').mkdir(parents=True); (root / 'bin').mkdir()
                for name in ('systemctl', 'launchctl'): install_stub(root / 'bin', name, adapter, fast=fast, model_shape=model_shape)
                install_stub(root / 'bin', 'uname', adapter, model_shape=model_shape)   # its .python/.model-ok is the validator the soundness check below runs
                for name in ('systemctl', 'launchctl') if fast else ():   # the adapter keeps its name (it reads argv[0]) one directory down
                    hidden = root / 'bin' / '.python'; (hidden / 'counted').mkdir(exist_ok=True); (hidden / name).rename(hidden / 'counted' / name)
                    (hidden / name).write_text('#!/bin/sh\nprintf d >> "$FIXTURE_ROOT/control/delegated"\nexec "$FIXTURE_ROOT/bin/.python/counted/' + name + '" "$@"\n'); (hidden / name).chmod(0o700)
            for index, (model, delegating, calls) in enumerate(models):
                results = []
                for fast in (True, False):
                    root = roots[fast]; hidden = root / 'bin' / '.python'
                    for name in ('events.jsonl', 'spawns.log', 'delegated', 'model.valid'): (root / 'control' / name).unlink(missing_ok=True)
                    checked.pop(fast, None)
                    (root / 'control' / 'model.json').write_text(model if isinstance(model, str) else json.dumps(model))
                    def read(name):
                        try: return (root / 'control' / name).read_text()
                        except OSError: return ''
                    run = []
                    for name, args in calls:
                        done = subprocess.run([str(root / 'bin' / name)] + args, env={'FIXTURE_ROOT': str(root), 'PATH': '/usr/bin:/bin'}, stdin=subprocess.DEVNULL, capture_output=True, timeout=30)
                        run.append((done.returncode, done.stdout, done.stderr.replace(str(root).encode(), b'ROOT').replace(b'/bin/.python/counted/', b'/bin/').replace(b'/bin/.python/', b'/bin/'),
                                    read('events.jsonl'), read('spawns.log'), read('model.json')))
                        if read('model.valid') == read('model.json') != '' and read('model.json') != checked.get(fast):   # a copy left by the stub, or by the adapter's own save(), is trusted by the others
                            checked[fast] = read('model.json')
                            assert subprocess.run([str(hidden / '.model-ok'), str(root / 'control')], stdin=subprocess.DEVNULL, timeout=30).returncode == 0, 'unsound model.valid after %s %r' % (name, args)
                    results.append((run, len(read('delegated'))))
                steps += len(calls)
                for step, (a, b) in enumerate(zip(results[0][0], results[1][0])):
                    assert a == b, 'the sh stub differs from the adapter on %s model %d call %d %r: %r != %r' % (platform, index, step, calls[step], a, b)
                expected = len(bad) if len(calls) == len(chain) else 0
                assert (results[0][1] > expected) if delegating else (results[0][1] == expected), '%s model %d: %d calls delegated, %d expected%s' % (platform, index, results[0][1], expected, ' or more' if delegating else '')
    print('sh answers for systemctl/launchctl match the adapter on %d chained calls' % steps, file=sys.stderr, flush=True)


def serialized_environment(env_file: Path, quoting: str, common: Path, environment: dict) -> str:
    commands = {
        'current': 'printf "Environment=%s" "$(systemd_quoted "HERDR_RELAY_ENV=$2")"',
        'legacy': 'printf "Environment=HERDR_RELAY_ENV=%s" "$(systemd_quote_value "$2")"',
    }
    result = subprocess.run(['/bin/bash', '-c', '. "$1" && ' + commands[quoting], '_', str(common), str(env_file)],
                            env=environment, capture_output=True, text=True, check=True, timeout=TIMEOUT)
    if '\\\\x' not in result.stdout or '%%%%' not in result.stdout: raise AssertionError('serializer fixture lost escape coverage')
    return result.stdout


class Fixture:
    def __init__(self, parent: Path, platform: str, present=(0, 1), fault='', fault_index=0, mode='cloudflare', quoting=None):
        self.root = parent / ('case-' + str(time.monotonic_ns())); self.root.mkdir(mode=0o700)
        self.relay = self.root / 'relay'; self.relay.mkdir(mode=0o700)
        self.bin = self.root / 'bin'; self.bin.mkdir(mode=0o700)
        self.control = self.root / 'control'; self.control.mkdir(mode=0o700)
        self.home = self.root / 'home'; self.home.mkdir(mode=0o700)
        self.private = self.root / 'private'; self.private.mkdir(mode=0o700)
        self.env_file = self.root / (ESCAPED_ENV_NAME if quoting else 'relay.env')
        self.env_file.write_text(f'HERDR_RELAY_TRANSPORT={mode}\nGH_TOKEN=fixture-saved-canary\n'); self.env_file.chmod(0o600)
        if quoting: (self.root / DOUBLE_DECODED_ENV_NAME).write_text('HERDR_RELAY_TRANSPORT=cloudflare\n')
        for name in ('common.sh', 'service.sh', 'uninstall-service.sh', 'uninstall-systemd-user-service.sh'):
            shutil.copy2(REPO / 'relay' / name, self.relay / name)
            if (self.relay / name).read_bytes() != (REPO / 'relay' / name).read_bytes(): raise AssertionError('entrypoint copy mismatch')
        for name in ('route', 'journal', 'reservation', 'token', 'cloudflare-config', 'origin-cert', 'device', 'recovery', 'bystander', 'log'):
            q = self.private / name; q.write_text('protected fixture ' + name + '\n'); q.chmod(0o600)
        with self.env_file.open('a') as stream:
            stream.write('HERDR_GITHUB_TOKEN_FILE="' + str(self.private / 'token') + '"\n')
            stream.write('CLOUDFLARED_CONFIG="' + str(self.private / 'cloudflare-config') + '"\n')
            stream.write('TUNNEL_ORIGIN_CERT="' + str(self.private / 'origin-cert') + '"\n')
        self.labels = (['herdr-mobile-relay.service', 'herdr-remote.service'] if platform == 'Linux'
                       else ['com.herdr-mobile-relay.service', 'com.herdr-remote.service'])
        directory = self.home / ('.config/systemd/user' if platform == 'Linux' else 'Library/LaunchAgents'); directory.mkdir(parents=True)
        self.definitions = [directory / (label + ('.plist' if platform == 'Darwin' else '')) for label in self.labels]
        self.environment = {'HOME': str(self.home), 'PATH': str(self.bin) + ':/usr/bin:/bin', 'LC_ALL': 'C',
                            'FIXTURE_ROOT': str(self.root), 'HERDR_RELAY_ENV': str(self.env_file),
                            'GH_TOKEN': 'fixture-ambient-canary', 'GITHUB_TOKEN': 'fixture-ambient-github-canary',
                            'GITHUB_ACTIONS': os.environ['GITHUB_ACTIONS'], 'RUNNER_ENVIRONMENT': os.environ['RUNNER_ENVIRONMENT']}
        self.model = {'platform': platform, 'files': dict(zip(self.labels, map(str, self.definitions))),
                      'fault': fault, 'fault_label': self.labels[fault_index], 'route_status': 0,
                      'states': {label: {'state': ('active' if platform == 'Linux' else 'loaded') if i in present else 'absent',
                                         'enabled': i in present, 'queries': 0} for i, label in enumerate(self.labels)}}
        self.save()
        for name in ('systemctl', 'launchctl', 'uname', 'plutil', 'rm', 'sleep', 'wc', 'head', 'stat',
                     'curl', 'cloudflared', 'tailscale', 'herdr-mobile-relay', 'herdr', 'open', 'ssh', 'sudo'):
            install_stub(self.bin, name, ADAPTER)
        route = self.relay / 'tailscale-cli.sh'
        route.write_text('#!' + sys.executable + '\n' + ADAPTER.replace("name = pathlib.Path(sys.argv[0]).name", "name = 'route-adapter'")); route.chmod(0o700)
        for i in present: self.write_definition(i, self.env_file, quoting)
        self.events_path = self.control / 'events.jsonl'
        self.protected = [self.env_file, self.private, self.root / DOUBLE_DECODED_ENV_NAME,
                          self.root / 'never-created-state', self.root / 'never-created-cache']
        self.original = self.protected_snapshot()

    def save(self): (self.control / 'model.json').write_text(json.dumps(self.model))

    def write_definition(self, index, selected, quoting=None):
        q = self.definitions[index]
        if self.model['platform'] == 'Linux':
            line = serialized_environment(selected, quoting, self.relay / 'common.sh', self.environment) if quoting else f'Environment="HERDR_RELAY_ENV={selected}"'
            q.write_text('[Service]\n' + line + '\n')
        else: q.write_bytes(plistlib.dumps({'Label': self.labels[index], 'EnvironmentVariables': {'HERDR_RELAY_ENV': str(selected)}}))
        q.chmod(0o600)

    def protected_snapshot(self): return tuple(snapshot(q) for q in self.protected)

    def events(self): return [json.loads(line) for line in self.events_path.read_text().splitlines()] if self.events_path.exists() else []

    def run(self, args=('uninstall',), choice=None, requested_signal=None, direct=False, conditional=False, observation=False, label_index=0):
        script = self.relay / ('uninstall-systemd-user-service.sh' if self.model['platform'] == 'Linux' else 'uninstall-service.sh') if direct else self.relay / 'service.sh'
        if direct == 'opposite':
            script = self.relay / ('uninstall-service.sh' if self.model['platform'] == 'Linux' else 'uninstall-systemd-user-service.sh')
        started = time.monotonic(); spawns_before = spawn_tally(self.control)
        argv = ['/bin/bash', '-x', str(script)] + list(args if not direct else args[1:])
        if conditional:
            operation = 'service_removal_quiet' if observation else 'service_removal_one'
            failure = '[ "$SERVICE_REMOVAL_STATE:$SERVICE_REMOVAL_ENABLED" = unknown:unknown ] || exit 98; exit 1' if observation else 'exit 1'
            invocation = operation + ' "$2" "$3" || { ' + failure + '; }' if conditional == 'or' else 'if ' + operation + ' "$2" "$3"; then exit 0; else ' + failure + '; fi'
            argv = ['/bin/bash', '-c', '. "$1"; ' + invocation, '_',
                    str(self.relay / 'common.sh'), 'systemd' if self.model['platform'] == 'Linux' else 'launchd', self.labels[label_index]]
        master = slave = None
        if choice is not None: master, slave = pty.openpty()
        bystander = None
        process = subprocess.Popen(argv, env=self.environment, cwd=self.root, stdin=slave if slave is not None else subprocess.DEVNULL,
                                   stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
        if slave is not None: os.close(slave)
        stop_drain = drain_pty(master) if master is not None else None
        try:
            bystander = subprocess.Popen([sys.executable, '-c', f'import time; time.sleep({3 * TIMEOUT})'], env=self.environment,
                                         stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
            if requested_signal is not None:
                deadline = time.monotonic() + TIMEOUT
                while not (self.control / 'ready').exists():
                    if process.poll() is not None or time.monotonic() >= deadline: raise AssertionError('cancellation handshake did not arrive')
                    time.sleep(.01)
                os.kill(process.pid, requested_signal)
                (self.control / 'release').write_text('release owned adapter\n')
            if choice is not None:
                # Prompt is on merged stdout, while only stdin needs a PTY.
                prefix = bytearray(); deadline = time.monotonic() + TIMEOUT
                while b'Route disposition' not in prefix:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0: raise AssertionError('route prompt deadline')
                    ready, _, _ = select.select([process.stdout], [], [], remaining)
                    if not ready: continue
                    data = os.read(process.stdout.fileno(), 4096)
                    if not data: raise AssertionError('route prompt missing')
                    prefix.extend(data)
                os.write(master, choice + b'\n')
                output = bytes(prefix) + bounded_communicate(process)[0]
            else: output = bounded_communicate(process)[0]
            events = self.events()
            if bystander.poll() is not None: raise AssertionError('unrelated owned fixture bystander was disturbed')
            if any(row['op'] == 'forbidden' for row in events): raise AssertionError('forbidden fixture tool effect')
            for row in events:
                if row['op'] == 'remove' and (row['state'] not in ('inactive', 'absent') or row['enabled']): raise AssertionError('removal preceded quiescence')
            for canary in (b'fixture-saved-canary', b'fixture-ambient-canary', b'fixture-ambient-github-canary'):
                if canary in output: raise AssertionError('tracing/output leaked a canary')
            return process.returncode, output, events
        finally:
            INVOCATIONS[0] += 1
            print('removal fixture invocation %d (%s model) took %.1fs; %s' % (
                INVOCATIONS[0], self.model['platform'], time.monotonic() - started,
                spawn_report(spawns_before, spawn_tally(self.control))), file=sys.stderr, flush=True)
            failure_states(process)
            if stop_drain is not None: stop_drain()
            if master is not None: os.close(master)
            # Fallback containment targets only this invocation's group;
            # it is never counted as manager cleanup or a passing result.
            contain(process)
            if bystander is not None and bystander.poll() is None:
                os.killpg(bystander.pid, signal.SIGTERM); bystander.wait(timeout=2)

    def unchanged(self, allow_route=False):
        if allow_route:
            for q, expected in zip(self.protected, self.original):
                if q != self.private and snapshot(q) != expected: raise AssertionError('protected resource changed')
            for name, expected in dict(self.original[1][2]).items():
                if name != 'route' and snapshot(self.private / name) != expected: raise AssertionError('private evidence changed')
        elif self.protected_snapshot() != self.original: raise AssertionError('protected bytes/modes/absence changed')


def configuration_refusal(f, seams):
    before = tuple(snapshot(q) for q in f.definitions)
    f.environment['HERDR_CLI_SETUP_ROLLBACK'] = '1'
    for direct, args in seams:
        status, output, events = f.run(direct=direct, args=args)
        assert status == 1 and not effects(events)
        assert all(e['op'] == 'configuration-observe' for e in events)
        assert b'Route disposition' not in output and b'for both Herdr labels' not in output
        assert tuple(snapshot(q) for q in f.definitions) == before; f.unchanged()


def metadata_batch_control():
    # Read-only product helpers, through the same counted/strict stat adapter.
    # Two acquisitions must both stat afresh, even without a destructive seam.
    with tempfile.TemporaryDirectory(prefix='herdr-metadata-batch-') as temporary:
        f = Fixture(Path(temporary).resolve(), 'Linux')
        subject = f.root / 'subject'; subject.write_text('metadata control\n'); subject.chmod(0o600)
        link = f.root / 'link'; link.symlink_to(subject)
        hard = f.root / 'hard'; hard.write_text('multiply linked\n'); hard.chmod(0o600)
        os.link(hard, f.root / 'hard-other')
        unsafe = f.root / 'unsafe'; unsafe.write_text('unsafe mode\n'); unsafe.chmod(0o622)
        closed = f.root / 'closed'; closed.mkdir(); (closed / 'file').write_text('unsearchable\n'); closed.chmod(0o600)
        command = '. "$1"; service_removal_resource "$2" || exit 1; first=$SERVICE_REMOVAL_RESOURCE_ID; service_removal_resource "$2" || exit 1; [ "$first" = "$SERVICE_REMOVAL_RESOURCE_ID" ] || exit 98; printf "%s" "$first"'
        try:
            for path, accepted in ((subject, True), (f.root / 'missing', True), (link, False),
                                   (hard, False), (unsafe, False), (f.root, False), (closed / 'file', False)):
                before = spawn_tally(f.control)
                done = subprocess.run(['/bin/bash', '-eu', '-c', command, '_', str(f.relay / 'common.sh'), str(path)],
                                      env=f.environment, stdin=subprocess.DEVNULL, capture_output=True, timeout=TIMEOUT)
                assert (done.returncode == 0) == accepted, (path.name, done.returncode, done.stderr)
                if not accepted: continue
                info = subject.stat()
                expected = '%d:%d:%d:%d' % (info.st_dev, info.st_ino, info.st_size, int(info.st_mtime)) if path == subject else 'absent'
                assert done.stdout == expected.encode()
                # GNU: two calls. BSD: one initial rejected -c, then two -f.
                assert spawn_tally(f.control)['stat'] - before.get('stat', 0) == (3 if sys.platform == 'darwin' else 2)
            assert not f.events(); f.unchanged()
        finally: closed.chmod(0o700)


# Cell selection. main() opens every control and every fixture group with cell(id) (ids are manager/family/parameters; the pure helper
# tests/service_qualification_matrix.py enumerates them from THIS FILE's text without running it). With no HERDR_UNINSTALL_CLI_* variable
# every cell is selected: the whole suite, unsharded, exactly as before. TIER=fast|full, SHARD=i/n and CELL=<id> narrow it, and an invalid
# request refuses before any control or fixture. EVIDENCE=<new file> receives one line per COMPLETED cell: reaching the next gate proves
# the open cell's assertions passed, so only then is it recorded. The ids reached (selected or not) and each cell's Fixture.run count must
# equal that enumeration, so the loops cannot drift from it unnoticed.
SELECTION = {}


def select_cells():
    import service_qualification_matrix as matrix  # here, so the modules that import this one never need it
    try:
        plan = matrix.plan(Path(__file__).read_text(), (REPO / 'tests' / 'uninstall_cli_fast_cases.json').read_text(), os.environ, matrix.CLI)
        path = os.environ.get('HERDR_UNINSTALL_CLI_EVIDENCE')
        SELECTION.update(plan=plan, selected=set(plan['selected']), reached=[], completed=[], open=None, start=0, evidence=open(path, 'x', buffering=1) if path else None)
    except (matrix.Refusal, OSError) as error: raise SystemExit('service removal cell selection refused: %s' % error)


def close_cell():
    cell_id = SELECTION['open']
    if cell_id is None: return
    ran, expected = INVOCATIONS[0] - SELECTION['start'], SELECTION['plan']['runs'][cell_id]
    assert ran == expected, 'cell %s ran %d invocations; the enumeration says %d' % (cell_id, ran, expected)
    SELECTION['completed'].append(cell_id); SELECTION['open'] = None
    if SELECTION['evidence']: print(cell_id, file=SELECTION['evidence'], flush=True)


def cell(cell_id):
    close_cell(); SELECTION['reached'].append(cell_id)
    if cell_id not in SELECTION['selected']: return False
    SELECTION['open'], SELECTION['start'] = cell_id, INVOCATIONS[0]
    return True


def finish_cells():
    close_cell(); plan = SELECTION['plan']
    assert SELECTION['reached'] == plan['inventory'], 'the cells reached differ from the enumerated inventory'
    assert sorted(SELECTION['completed']) == sorted(plan['selected']), 'the completed cells differ from the selected cells'
    print('uninstall CLI cells completed: %d of %d (tier %s, shard %d/%d)' % (len(plan['selected']), len(plan['inventory']), plan['tier'], plan['shard_index'], plan['shard_count']), flush=True)


def launchd_not_found_control():
    # Task Z. Not a cell (the frozen inventories of tests/service_qualification_matrix.py do not grow, and the adapters above are digest-pinned): the REAL service_removal_observe, sourced from the repository,
    # reads modern launchctl's two-line answer for a label that is not loaded. A nonzero label query is absence only when its merged `2>&1` text, after ONE terminating LF, is exactly the diagnostic
    # (macOS 12 and earlier), `Bad request.` + LF + the diagnostic, or the diagnostic + LF + `Bad request.` (a stdout line buffered under `2>&1` can follow an unbuffered stderr one); every other text,
    # and any status of 0, is unknown.
    label, uid = 'com.herdr-mobile-relay.service', str(os.getuid())
    diag = lambda name, user=uid: 'Could not find service "' + name + '" in domain for user gui: ' + user
    ours, bad, framed = diag(label), 'Bad request.', 'gui/' + uid + '/' + label + ' = {\n}\n'
    script = r'''set +e
. "$REPO/relay/common.sh"
launchctl() { case "$1" in print) case "$2" in gui/*/*) printf '%s' "$OUT"; printf '%s' "$ERR" >&2; return "$CODE" ;; *) printf '%s = {}' "$2" ;; esac ;; print-disabled) printf 'disabled services = {}\n' ;; esac; }
if service_removal_observe launchd "$LABEL"; then echo "$SERVICE_REMOVAL_STATE:$SERVICE_REMOVAL_ENABLED"; else echo unknown:unknown; fi
'''
    # (stdout, stderr, status): the product merges the two streams, stdout first
    accepted = [('', ours + '\n', 113), (ours + '\n', '', 113), ('', bad + '\n' + ours + '\n', 113), (bad + '\n' + ours + '\n', '', 113), ('', ours + '\n' + bad + '\n', 113),
                ('', bad + '\n' + ours, 1), (bad + '\n', ours + '\n', 113), (ours + '\n', bad + '\n', 255)]
    refused = [('', bad + '\n\n' + ours + '\n', 113), (bad + '\n\n', ours + '\n', 113),  # a blank line between the two
               ('', bad + '\n' + diag('com.example.other') + '\n', 113), ('', bad + '\n' + diag(label, str(os.getuid() + 1)) + '\n', 113),  # another label, another uid
               ('', bad + '\n' + ours + '\n', 0), (bad + '\n' + ours + '\n', '', 0), ('', ours + '\n', 0),  # a status of 0 is no not-found diagnostic
               ('', bad + '\n' + bad + '\n' + ours + '\n', 113), ('', bad + '\r\n' + ours + '\n', 113), ('', bad + ' ' + ours + '\n', 113), ('', 'bad request.\n' + ours + '\n', 113),
               ('', bad + '\n' + ours + '\nextra\n', 113), ('', bad + '\n' + ours + '\n\n', 113), ('', bad + '\n', 113), ('', '', 113),
               (framed, '', 113), (bad + '\n' + framed, '', 113)]  # a loaded answer is never absence, whatever the status
    # The grammar variants, byte for byte those of tests/test_native_manager_lifecycle_matrix.py (NOT_FOUND_ACCEPTED and NOT_FOUND_REFUSED; a matrix test compares them, so the three never drift): {b} is `Bad request.`,
    # {d} the exact diagnostic of this label and uid, {dl} the diagnostic of another label, {du} of another uid, {dc} a cut diagnostic and {dlow} the lower-cased one.
    NOT_FOUND_ACCEPTED = ('{d}', '{d}\n', '{b}\n{d}', '{b}\n{d}\n', '{d}\n{b}', '{d}\n{b}\n')
    NOT_FOUND_REFUSED = (
        '{b}', '{b}\n', '', '\n', '\n\n',  # the other line alone, nothing at all
        '{b}\n\n{d}', '{b}\n\n{d}\n', '{d}\n\n{b}', '{d}\n\n{b}\n',  # a blank line between the two
        '{b}\n{b}\n{d}', '{b}\n{d}\n{d}', '{b}\n{d}\n{b}', '{d}\n{b}\n{d}', '{d}\n{d}', '{d}\n{d}\n{b}', '{b}\n{b}',  # a repeated line
        '{b}\r\n{d}', '{b}\n{d}\r', '{b}\r\n{d}\r\n', '{d}\r\n{b}', '{d}\n{b}\r', '{b}\r{d}',  # a CR
        '{b} {d}', '{b}{d}', '{d} {b}', '{d}{b}', '{b}\n{d} {b}',  # the two on one line
        'bad request.\n{d}', 'BAD REQUEST.\n{d}', 'Bad Request.\n{d}', '{b}\n{dlow}', '{dlow}', 'Bad request\n{d}', 'Bad request..\n{d}', '{b}.\n{d}',  # case and spelling
        '{b}\n{dl}', '{dl}\n{b}', '{b}\n{du}', '{du}\n{b}', '{b}\n{dc}', '{dc}',  # another label, another uid, a cut diagnostic
        '{b}\n{d}\nextra', 'extra\n{b}\n{d}', '{b}\nextra\n{d}', '{b}\n{d} ', ' {b}\n{d}', '{b} \n{d}', '{b}\n {d}', '\n{b}\n{d}', '{d}\n{b}\nextra',  # extra text or blanks
        '{b}\n{d}\n\n', '{d}\n{b}\n\n', '{b}\n{d}\n\n\n', '{d}\n\n')  # a second trailing LF
    fill = lambda template: template.format(b=bad, d=ours, dl=diag('com.example.other'), du=diag(label, str(os.getuid() + 1)), dc=ours[:-1], dlow=ours.lower())
    # The whole text arrives merged (here on stderr; the product reads `2>&1`): every accepted form at status 113 (and at 1 and 255 for three), none at status 0; every refused variant at 113, and five of them at 0, 2 and 255.
    accepted += [('', fill(x), code) for x in NOT_FOUND_ACCEPTED for code in ((113, 1, 255) if x in ('{d}', '{b}\n{d}\n', '{d}\n{b}\n') else (113,))]
    refused += [('', fill(x), 113) for x in NOT_FOUND_REFUSED] + [('', fill(x), 0) for x in NOT_FOUND_ACCEPTED]
    refused += [('', fill(x), code) for x in ('{b}\n\n{d}\n', '{b}\n{dl}', '{b}\n{du}', '{d}\n{b}\r', '{b}\n{d}\n\n') for code in (0, 2, 255)]
    # The same texts split across the two streams (stdout first, as the merge orders them): the accepted forms stay absence at the cut, the refused ones stay refused.
    for x, first in (('{b}\n{d}\n', '{b}\n'), ('{d}\n{b}\n', '{d}\n'), ('{b}\n{d}', '{b}\n'), ('{d}\n{b}', '{d}\n'), ('{d}\n', '{d}')):
        accepted.append((fill(x)[:len(fill(first))], fill(x)[len(fill(first)):], 113))
    for x in ('{b}\n\n{d}\n', '{b}\n{dl}\n', '{b}\n{du}\n', '{b}\n{d}\n\n'):
        refused.append((fill(x)[:fill(x).index('\n') + 1], fill(x)[fill(x).index('\n') + 1:], 113))
    for expected, cases in (('absent:true', accepted), ('unknown:unknown', refused)):
        for out, err, code in cases:
            with tempfile.TemporaryDirectory() as home:
                done = subprocess.run(['/bin/bash', '--noprofile', '--norc', '-c', script], capture_output=True, cwd=REPO, timeout=30,
                                      env={'PATH': '/usr/bin:/bin', 'HOME': home, 'REPO': str(REPO), 'LABEL': label, 'OUT': out, 'ERR': err, 'CODE': str(code)})
            assert done.stdout.decode().strip() == expected, ('modern not-found answer', out, err, code, done.stdout, done.stderr[:200])


def main():
    if os.getuid() == 0: raise SystemExit('removal fixtures must run as a non-root hosted user')
    select_cells()
    launchd_not_found_control()
    if cell('control/timeout-diagnostics'): timeout_diagnostics_control()
    if cell('control/fast-stub-equivalence'): fast_stub_equivalence_control()
    if cell('control/metadata-batch'): metadata_batch_control()
    with tempfile.TemporaryDirectory(prefix='herdr-removal-') as temporary:
        parent = Path(temporary).resolve()
        for platform in ('Linux', 'Darwin'):
            manager = 'systemd' if platform == 'Linux' else 'launchd'
            for present in ((), (0,), (1,), (0, 1)):
                if not cell(f"{manager}/present/{''.join(str(i) for i in present) or 'none'}"): continue
                f = Fixture(parent, platform, present=present)
                status, output, events = f.run()
                assert status == 0 and all(not q.exists() for q in f.definitions)
                f.unchanged()
            for initial in (('inactive',) if platform == 'Linux' else ('absent',)):
                if not cell(f'{manager}/initial/{initial}'): continue
                f = Fixture(parent, platform)
                for s in f.model['states'].values(): s.update(state=initial, enabled=True)
                f.save(); assert f.run()[0] == 0; f.unchanged()
            for fault in ('', 'query-fail'):
                if not cell(f"{manager}/direct/{fault or 'none'}"): continue
                f = Fixture(parent, platform, fault=fault)
                assert f.run(direct=True)[0] == (1 if fault else 0); f.unchanged()
            if cell(f'{manager}/opposite'):
                f = Fixture(parent, platform)
                definitions_before = tuple(snapshot(q) for q in f.definitions)
                assert f.run(direct='opposite')[0] == 1 and not f.events()
                assert tuple(snapshot(q) for q in f.definitions) == definitions_before; f.unchanged()
            faults = ['query-fail', 'malformed', 'reactivate', 'replace-file', 'rm-fail', 'disable-fail', 'delayed', 'stop-noop']
            faults += ['stop-fail', 'duplicate', 'missing', 'unknown', 'contradictory', 'unsupported-enable', 'reload-fail'] if platform == 'Linux' else ['bootout-fail', 'wrong-label', 'domain-fail', 'domain-malformed', 'disabled-fail', 'disabled-malformed', 'disabled-duplicate', 'disabled-missing']
            for bad_index in (0, 1):
                for fault in faults:
                    if not cell(f'{manager}/fault/{bad_index}/{fault}'): continue
                    f = Fixture(parent, platform, fault=fault, fault_index=bad_index)
                    original_definition = snapshot(f.definitions[bad_index])
                    status, output, events = f.run(); f.unchanged()
                    assert status == (0 if fault == 'delayed' else 1)
                    if fault not in ('delayed', 'reload-fail'):
                        assert f.definitions[bad_index].exists() and not any(e['op'] == 'remove' and e['label'] == f.labels[bad_index] for e in events if fault != 'rm-fail')
                        assert b'for both Herdr labels' not in output
                        if fault != 'replace-file': assert snapshot(f.definitions[bad_index]) == original_definition
                    if fault == 'reload-fail': assert b'definition, but systemd reload failed' in output
                    if fault in ('query-fail', 'stop-fail', 'bootout-fail', 'rm-fail', 'reactivate'):
                        assert not f.definitions[1 - bad_index].exists()  # both partial-progress directions
            # F005: lifecycle capture, NOT the independent configuration
            # query, must reject NUL before command substitution can erase it.
            # Native state may be active/loaded while malformed text claims
            # quiet. Global domain/disabled faults refuse both labels; a
            # label-local fault still permits honest safe sibling progress.
            lifecycle_seams = ('systemd',) if platform == 'Linux' else ('domain', 'label-absent', 'label-loaded', 'disabled')
            for index in (0, 1):
                for seam in lifecycle_seams:
                    domain = 'gui/' + str(os.getuid())
                    label = ('herdr-mobile-relay.service', 'herdr-remote.service')[index] if platform == 'Linux' else ('com.herdr-mobile-relay.service', 'com.herdr-remote.service')[index]
                    if seam == 'systemd':
                        text = 'LoadState=loaded\nActiveState=inactive\nUnitFileState=disabled'; code = 0
                        binary = text.replace('inactive', 'inac\x00tive')
                    elif seam == 'domain':
                        text = domain + ' = {}'; code = 0; binary = text.replace(' =', '\x00 =')
                    elif seam == 'disabled':
                        text = 'disabled services = {\n    "' + label + '" => true\n}'; code = 0
                        binary = text.replace('true', 'tr\x00ue')
                    elif seam == 'label-absent':
                        text = 'Could not find service "' + label + '" in domain for user gui: ' + str(os.getuid()); code = 113
                        binary = text.replace('service', 'serv\x00ice')
                    else:
                        text = domain + '/' + label + ' = {\n    state = waiting\n}'; code = 0
                        binary = text.replace(label, label + '\x00')
                    limit = 4096 if platform == 'Linux' else 65536
                    failures = ((binary, code), ('\x00' + text, code), (text + '\x00', code),
                                (text + 'X' * (limit + 1), code), (text[:-1] if seam == 'label-absent' else text, 1), ('', code))
                    for form, (payload, status_code) in zip(('middle-nul', 'leading-nul', 'trailing-nul', 'oversize', 'status-1', 'empty'), failures):
                        for action in ('uninstall', 'direct', 'assert-stopped', 'rollback-cli-setup', 'if', 'or', 'observe-if', 'observe-or'):
                            if action == 'rollback-cli-setup' and index == 1: continue  # current-only authority
                            if not cell(f'{manager}/capture/{index}/{seam}/{form}/{action}'): continue
                            f = Fixture(parent, platform, fault='lifecycle-output', fault_index=index)
                            # Safe sibling for assert-stopped, nonquiet target
                            # for the normalized quiet-claim counterexample.
                            for i, state in enumerate(f.model['states'].values()):
                                state.update(state=('active' if platform == 'Linux' else 'loaded') if i == index and seam in ('systemd', 'label-absent', 'label-loaded') else ('inactive' if platform == 'Linux' else 'absent'), enabled=False)
                            # The product reads the domain listing by status alone (its text is discarded): every fault of that seam is a FAILING query.
                            f.model.update(lifecycle_seam='label' if seam.startswith('label-') else seam,
                                           lifecycle_text=payload, lifecycle_status=1 if seam == 'domain' else status_code,
                                           lifecycle_stderr=seam == 'label-absent')
                            f.save(); f.environment['HERDR_CLI_SETUP_ROLLBACK'] = '1'
                            before = tuple(snapshot(q) for q in f.definitions)
                            conditional = 'or' if action.endswith('or') else True if action.endswith('if') else False
                            args = (action,) if action in ('assert-stopped', 'rollback-cli-setup') else ('uninstall',)
                            result, output, events = f.run(args=args, direct=action == 'direct', conditional=conditional,
                                                          observation=action.startswith('observe-'), label_index=index)
                            assert result == 1 and b'for both Herdr labels' not in output
                            assert any(e['op'] == 'lifecycle-capture' for e in events)
                            assert snapshot(f.definitions[index]) == before[index]
                            assert not any(e['label'] == f.labels[index] for e in effects(events)); f.unchanged()
                            if action in ('assert-stopped', 'rollback-cli-setup', 'if', 'or', 'observe-if', 'observe-or') or seam in ('domain', 'disabled'):
                                assert tuple(snapshot(q) for q in f.definitions) == before and not effects(events)
                            else: assert not f.definitions[1 - index].exists()
            # A binary quiet claim arising AFTER supported initial state and
            # stop/disable must still refuse at poll and both fresh seams.
            for index in (0, 1):
                for count in (2, 3, 4):
                    for action in ('uninstall', 'direct', 'if', 'or'):
                        if not cell(f'{manager}/late/{index}/{count}/{action}'): continue
                        f = Fixture(parent, platform, fault='lifecycle-output', fault_index=index)
                        text = 'LoadState=loaded\nActiveState=inac\x00tive\nUnitFileState=disabled' if platform == 'Linux' else 'Could not find serv\x00ice "' + f.labels[index] + '" in domain for user gui: ' + str(os.getuid())
                        f.model.update(lifecycle_seam='systemd' if platform == 'Linux' else 'label', lifecycle_text=text,
                                       lifecycle_status=0 if platform == 'Linux' else 113, lifecycle_fault_at=count,
                                       lifecycle_stderr=platform == 'Darwin')
                        f.save(); before = snapshot(f.definitions[index])
                        status, _, events = f.run(direct=action == 'direct', conditional='or' if action == 'or' else action == 'if', label_index=index)
                        assert status == 1 and any(e['op'] == 'lifecycle-capture' and e['label'] == f.labels[index] for e in events)
                        assert snapshot(f.definitions[index]) == before and not any(e['op'] == 'remove' and e['label'] == f.labels[index] for e in events)
                        assert any(e['op'] in ('stop', 'bootout') and e['label'] == f.labels[index] for e in events); f.unchanged()
            # Genuine EOF at the launchd acquisition bound remains supported;
            # its synthetic domain body is ASCII, not real-manager evidence.
            if platform == 'Darwin':
                for size in (65535, 65536):
                    for action in ('uninstall', 'assert-stopped'):
                        if not cell(f'{manager}/bound/{size}/{action}'): continue
                        f = Fixture(parent, platform, fault='lifecycle-output')
                        for state in f.model['states'].values(): state.update(state='absent', enabled=False)
                        prefix = 'gui/' + str(os.getuid()) + ' = {\n'
                        text = prefix + ' ' * (size - len(prefix) - 2) + '}'  # print adds one newline
                        f.model.update(lifecycle_seam='domain', lifecycle_text=text, lifecycle_status=0); f.save()
                        assert f.run(args=(action,))[0] == 0; f.unchanged()
            for shape in ('symlink', 'directory', 'hardlink', 'writable', 'unreadable'):
                if not cell(f'{manager}/definition/{shape}'): continue
                f = Fixture(parent, platform); q = f.definitions[0]
                if shape == 'symlink': q.unlink(); q.symlink_to(f.env_file)
                if shape == 'directory': q.unlink(); q.mkdir()
                if shape == 'hardlink': os.link(q, f.root / 'linked-definition')
                if shape == 'writable': q.chmod(0o666)
                if shape == 'unreadable': q.chmod(0)
                before = snapshot(q); status, _, events = f.run()
                assert status == 1 and not events and snapshot(q) == before; f.unchanged()
            # F003: genuinely missing ancestors remain distinct from existing
            # unsearchable ones. Restore traversal only AFTER each invocation.
            if cell(f'{manager}/ancestor-absent'):
                f = Fixture(parent, platform, present=()); f.definitions[0].parent.rmdir()
                assert f.run()[0] == 0; f.unchanged()
            for higher in (False, True):
                if not cell(f"{manager}/ancestor-unsearchable/{'grandparent' if higher else 'parent'}"): continue
                f = Fixture(parent, platform)
                for s in f.model['states'].values(): s.update(state='absent', enabled=False)
                f.save(); definitions_before = tuple(snapshot(q) for q in f.definitions)
                ancestor = f.definitions[0].parent.parent if higher else f.definitions[0].parent
                mode_before = stat.S_IMODE(ancestor.stat().st_mode)
                ancestor.chmod(0)
                try:
                    for direct, args, conditional in ((False, ('uninstall',), False), (True, ('uninstall',), False),
                                                      (False, ('rollback-cli-setup',), False), (False, ('uninstall',), True), (False, ('uninstall',), 'or')):
                        f.environment['HERDR_CLI_SETUP_ROLLBACK'] = '1'
                        status, output, events = f.run(args=args, direct=direct, conditional=conditional)
                        assert status == 1 and not events and b'Verified absent' not in output
                finally: ancestor.chmod(mode_before)
                assert tuple(snapshot(q) for q in f.definitions) == definitions_before; f.unchanged()
            for text in ("  export HERDR_RELAY_TRANSPORT='TaIlsCaLe-ClI'\n", 'HERDR_RELAY_TRANSPORT=tailscale-cli\nHERDR_RELAY_TRANSPORT=tailscale-cli\n'):
                if not cell(f"{manager}/route-spelling/{'export' if 'export' in text else 'duplicate'}"): continue
                f = Fixture(parent, platform, mode='tailscale-cli'); f.env_file.write_text(text); f.original = f.protected_snapshot()
                assert f.run(choice=b'k')[0] == 0; f.unchanged()
            for choice in (b'k', b'r', b'c', None):
                if not cell(f"{manager}/route-choice/{choice.decode() if choice else 'none'}"): continue
                f = Fixture(parent, platform, mode='tailscale-cli')
                status, output, events = f.run(choice=choice)
                assert status == (0 if choice in (b'k', b'r') else 1)
                routes = [e for e in events if e['op'] == 'unpublish']
                assert len(routes) == (1 if choice == b'r' else 0)
                if choice == b'r': assert effects(events)[0]['op'] == 'unpublish' and routes[0]['env'] == str(f.env_file)
                if choice in (None, b'c'): assert not effects(events)
                f.unchanged(allow_route=choice == b'r')
            if cell(f'{manager}/route-failure'):
                f = Fixture(parent, platform, mode='tailscale-cli'); f.model['route_status'] = 23; f.save()
                status, _, events = f.run(choice=b'r'); assert status == 1 and [e['op'] for e in effects(events)] == ['unpublish']; f.unchanged()
            for prior in ('retained', 'removed'):
                if not cell(f'{manager}/route-prior/{prior}'): continue
                f = Fixture(parent, platform, mode='tailscale-cli')
                assert f.run(args=('uninstall', '--cli-route-disposition=' + prior))[0] == 0; f.unchanged()
            for args in (('uninstall', ''), ('uninstall', '--unknown'), ('uninstall', '--cli-route-disposition=retained', '--cli-route-disposition=retained'), ('assert-stopped', 'extra'), ('rollback-cli-setup', 'extra'), ('--unknown',)):
                if not cell(f"{manager}/usage/{'+'.join(a or 'empty' for a in args).replace('--', '')}"): continue
                f = Fixture(parent, platform); assert f.run(args=args)[0] == 2 and not f.events(); f.unchanged()
            for text in ('HERDR_RELAY_TRANSPORT=bad\n', 'HERDR_RELAY_TRANSPORT=tailscale-cli\nHERDR_RELAY_TRANSPORT=cloudflare\n', 'HERDR_RELAY_TRANSPORT=$(touch canary)\n', 'GH_TOKEN=`touch canary`\n', 'HERDR_RELAY_TRANSPORT=cloudflare; touch canary\n', 'PATH=/untrusted\n', 'SERVICE_REMOVAL_STATE=absent\n'):
                if not cell(f"{manager}/env-text/{''.join(c if c.isalnum() else '_' for c in text.strip())}"): continue
                f = Fixture(parent, platform); f.env_file.write_text(text); f.original = f.protected_snapshot()
                assert f.run(args=('uninstall', '--cli-route-disposition=retained'))[0] == 1 and not effects(f.events())
                assert not (f.root / 'canary').exists(); f.unchanged()
            for key in ('HERDR_RELEASE_ROOT', 'HERDR_RELAY_BIN', 'HERDR_RELAY_BIN_DIR', 'HERDR_PLUTIL', 'HERDR_WEB_ROOT', 'HERDR_CLI_SETUP_ROLLBACK'):
                for value in ('', '/untrusted'):
                    if not cell(f"{manager}/env-key/{key}/{'empty' if not value else 'untrusted'}"): continue
                    f = Fixture(parent, platform, mode='tailscale-cli'); f.environment[key] = value
                    f.env_file.write_text('HERDR_RELAY_TRANSPORT=tailscale-cli\n' + key + '="' + value + '"\n'); f.original = f.protected_snapshot()
                    assert f.run(args=('uninstall', '--cli-route-disposition=retained'))[0] == 1 and not effects(f.events()); f.unchanged()
            for role in ('HERDR_GITHUB_TOKEN_FILE', 'CLOUDFLARED_CONFIG', 'TUNNEL_ORIGIN_CERT'):
                for index in (0, 1):
                    if not cell(f'{manager}/role-selected/{role}/{index}'): continue
                    f = Fixture(parent, platform)
                    f.env_file.write_text('HERDR_RELAY_TRANSPORT=cloudflare\n' + role + '="' + str(f.definitions[index]) + '"\n')
                    f.original = f.protected_snapshot()
                    for action in ('uninstall', 'rollback-cli-setup'):
                        f.environment['HERDR_CLI_SETUP_ROLLBACK'] = '1'
                        assert f.run(args=(action,))[0] == 1 and not effects(f.events()); f.unchanged()
                if cell(f'{manager}/role-alias/{role}'):
                    f = Fixture(parent, platform); alias = f.root / 'selected-alias'; alias.symlink_to(f.definitions[0])
                    f.env_file.write_text('HERDR_RELAY_TRANSPORT=cloudflare\n' + role + '="' + str(alias) + '"\n'); f.original = f.protected_snapshot()
                    assert f.run()[0] == 1 and not effects(f.events()); f.unchanged()
            # F001: an alternate uppercase spelling with the same checked
            # device/inode must refuse before effects. The adapter also makes
            # this cell meaningful on case-sensitive hosted filesystems; it
            # is NOT qualification of a real Darwin filesystem or race lock.
            for role in ('CLOUDFLARED_CONFIG', 'TUNNEL_ORIGIN_CERT'):
                for index in (0, 1):
                    for fault, side in (('', 'selected'), ('role-identity-fail', 'selected'), ('role-identity-malformed', 'selected'),
                                        ('role-identity-fail', 'destination'), ('role-identity-malformed', 'destination')):
                        if not cell(f"{manager}/identity-alias/{role}/{index}/{fault or 'none'}/{side}"): continue
                        f = Fixture(parent, platform, fault=fault, fault_index=index)
                        alias = f.definitions[index].with_name(f.definitions[index].name.upper())
                        if not alias.exists(): alias.write_bytes(f.definitions[index].read_bytes()); alias.chmod(0o600)
                        f.model['identity_aliases'] = {str(alias): str(f.definitions[index])}
                        f.model['role_identity_fault_path'] = str(alias if side == 'selected' else f.definitions[index]); f.save()
                        f.env_file.write_text('HERDR_RELAY_TRANSPORT=cloudflare\n' + role + '="' + str(alias) + '"\n')
                        f.protected.append(alias); f.original = f.protected_snapshot()
                        definitions_before = tuple(snapshot(q) for q in f.definitions)
                        for direct, action in ((False, 'uninstall'), (True, 'uninstall'), (False, 'rollback-cli-setup')):
                            f.environment['HERDR_CLI_SETUP_ROLLBACK'] = '1'
                            assert f.run(args=(action,), direct=direct)[0] == 1 and not effects(f.events()); f.unchanged()
                        assert tuple(snapshot(q) for q in f.definitions) == definitions_before
            # F002: expected Herdr labels are quiet/disabled, but a plist may
            # instead describe an unrelated loaded job. No label/route/manager
            # effect is permitted, including internal and conditional seams.
            for index in (0, 1) if platform == 'Darwin' else ():
                for kind in ('wrong', 'missing', 'empty', 'number', 'array', 'malformed'):
                    if not cell(f'{manager}/plist-label/{index}/{kind}'): continue
                    f = Fixture(parent, platform, mode='tailscale-cli')
                    for s in f.model['states'].values(): s.update(state='absent', enabled=False)
                    f.model['states']['com.unrelated.fixture'] = {'state': 'loaded', 'enabled': True, 'queries': 0}; f.save()
                    q = f.definitions[index]; data = plistlib.loads(q.read_bytes())
                    if kind == 'missing': del data['Label']
                    else: data['Label'] = {'wrong': 'com.unrelated.fixture', 'empty': '', 'number': 7, 'array': [f.labels[index]], 'malformed': 'ignored'}[kind]
                    q.write_bytes(b'not a plist' if kind == 'malformed' else plistlib.dumps(data)); q.chmod(0o600)
                    definitions_before = tuple(snapshot(q) for q in f.definitions)
                    for direct, action in ((False, 'uninstall'), (True, 'uninstall'), (False, 'rollback-cli-setup')):
                        f.environment['HERDR_CLI_SETUP_ROLLBACK'] = '1'
                        status, output, events = f.run(args=(action,), direct=direct)
                        assert status == 1 and not events and b'Route disposition' not in output; f.unchanged()
                    if index == 0:
                        for conditional in (True, 'or'): assert f.run(conditional=conditional)[0] == 1 and not f.events()
                    assert tuple(snapshot(q) for q in f.definitions) == definitions_before
                    assert json.loads((f.control / 'model.json').read_text())['states']['com.unrelated.fixture'] == {'state': 'loaded', 'enabled': True, 'queries': 0}
            for fault in ('stat-fail', 'stat-malformed', 'read-fail', 'decode-fail', 'label-decode-fail', 'label-malformed-output') if platform == 'Darwin' else ('stat-fail', 'stat-malformed', 'read-fail'):
                if not cell(f'{manager}/tool-fault/{fault}'): continue
                f = Fixture(parent, platform, fault=fault); assert f.run()[0] == 1 and not effects(f.events()); f.unchanged()
                if fault in ('stat-fail', 'stat-malformed'): assert not f.events()
            if cell(f'{manager}/legacy-selection'):
                f = Fixture(parent, platform, present=(1,), mode='tailscale-cli')
                assert f.run()[0] == 1 and not effects(f.events()); f.unchanged()  # surviving legacy selection
            for bad in ('missing-config', 'different-config', 'ambiguous-definition'):
                if not cell(f'{manager}/config-input/{bad}'): continue
                f = Fixture(parent, platform)
                if bad == 'missing-config': f.env_file.unlink()
                elif bad == 'different-config':
                    other = f.root / 'other.env'; other.write_text('HERDR_RELAY_TRANSPORT=cloudflare\n'); f.write_definition(1, other)
                elif platform == 'Linux': f.definitions[1].write_text('[Service]\nEnvironment=HERDR_RELAY_ENV="bad"\nEnvironment=HERDR_RELAY_ENV="other"\n')
                else: f.definitions[1].write_text('not a plist')
                f.original = f.protected_snapshot(); assert f.run()[0] == 1 and not effects(f.events()); f.unchanged()
            for args in (('assert-stopped',), ('rollback-cli-setup',)):
                for state in ('active', 'unknown', 'absent-loaded', 'quiet'):
                    if not cell(f'{manager}/stopped/{args[0]}/{state}'): continue
                    f = Fixture(parent, platform, present=() if state == 'absent-loaded' else (0, 1))
                    if state == 'absent-loaded':
                        f.model['states'][f.labels[0]].update(state='active' if platform == 'Linux' else 'loaded', enabled=True)
                        f.model['fault'] = 'stop-noop'; f.save()
                    if state == 'unknown': f.model['fault'] = 'query-fail'; f.save()
                    if state == 'quiet':
                        for s in f.model['states'].values(): s.update(state='inactive' if platform == 'Linux' else 'absent', enabled=False)
                        f.save()
                    legacy = snapshot(f.definitions[1])
                    if args[0] == 'rollback-cli-setup': f.environment['HERDR_CLI_SETUP_ROLLBACK'] = '1'
                    status, _, events = f.run(args=args); f.unchanged()
                    expected = 0 if state == 'quiet' or (args[0] == 'rollback-cli-setup' and state == 'active') else 1
                    assert status == expected and snapshot(f.definitions[1]) == legacy
                    if args[0] == 'assert-stopped': assert all(e['op'] == 'observe' for e in events)
            if cell(f'{manager}/rollback-unauthorized'): f = Fixture(parent, platform); assert f.run(args=('rollback-cli-setup',))[0] == 2 and not f.events(); f.unchanged()
            for requested in (signal.SIGINT, signal.SIGTERM):
                for seam in ('pause-stop', 'pause-observe'):
                    for index in (0, 1):
                        if not cell(f'{manager}/signal/{int(requested)}/{seam}/{index}'): continue
                        f = Fixture(parent, platform, fault=seam, fault_index=index)
                        status, _, events = f.run(requested_signal=requested)
                        assert status == 128 + requested and not any(e['op'] == 'remove' and e['label'] == f.labels[index] for e in events)
                        assert f.definitions[index].exists() and (f.definitions[0].exists() if index == 0 else not f.definitions[0].exists())
                        f.unchanged()
            for fault in ('rm-fail', 'query-fail'):
                for conditional in (True, 'or'):
                    if not cell(f"{manager}/caller/{fault}/{'or' if conditional == 'or' else 'if'}"): continue
                    f = Fixture(parent, platform, fault=fault); assert f.run(conditional=conditional)[0] == 1; f.unchanged()
            if cell(f'{manager}/config-absent'):
                f = Fixture(parent, platform, present=()); f.env_file.unlink(); f.original = f.protected_snapshot()
                assert f.run()[0] == 0; f.unchanged()  # genuinely absent fallback differs from installed missing input
            if cell(f'{manager}/ambient-transport'):
                f = Fixture(parent, platform); f.env_file.write_text('HERDR_RELAY_TRANSPORT=\n'); f.environment['HERDR_RELAY_TRANSPORT'] = 'tailscale-cli'; f.original = f.protected_snapshot()
                assert f.run()[0] == 1 and not effects(f.events()); f.unchanged()
            for q in ('current', 'legacy') if platform == 'Linux' else ():
                if not cell(f'{manager}/quoted-route/{q}'): continue
                f = Fixture(parent, platform, mode='tailscale-cli', quoting=q)
                status, _, events = f.run(choice=b'r'); assert status == 0
                assert [e['env'] for e in events if e['op'] == 'unpublish'] == [str(f.env_file)]; f.unchanged(allow_route=True)
        # F004: actual current/legacy serializers admit escaped literal %u,
        # not an unescaped specifier. Both literal and modeled expanded files
        # exist but select different transports, so fallback cannot hide it.
        for quoting in ('current', 'legacy'):
            for index in (0, 1):
                for unsupported in (False, True):
                    if not cell(f"systemd/percent-specifier/{quoting}/{index}/{'unsupported' if unsupported else 'escaped'}"): continue
                    f = Fixture(parent, 'Linux', mode='tailscale-cli', quoting=quoting)
                    literal_dir = f.root / 'selected-%u'; literal_dir.mkdir(mode=0o700)
                    expanded_dir = f.root / 'selected-fixture-user'; expanded_dir.mkdir(mode=0o700)
                    literal = literal_dir / ESCAPED_ENV_NAME; expanded = expanded_dir / ESCAPED_ENV_NAME
                    literal.write_text('HERDR_RELAY_TRANSPORT=cloudflare\n'); literal.chmod(0o600)
                    expanded.write_text('HERDR_RELAY_TRANSPORT=tailscale-cli\n'); expanded.chmod(0o600)
                    for i in (0, 1):
                        f.write_definition(i, literal, quoting)
                        if not unsupported:
                            q = f.definitions[i]
                            q.write_text(q.read_text() + '# Environment = ignored comment\n; EnvironmentFile = ignored comment\nType \t = simple\n')
                    if unsupported:
                        q = f.definitions[index]; text = q.read_text()
                        assert text.count('%%u') == 1
                        q.write_text(text.replace('%%u', '%u')); q.chmod(0o600)
                    f.protected.extend((literal_dir, expanded_dir)); f.original = f.protected_snapshot()
                    definitions_before = tuple(snapshot(q) for q in f.definitions)
                    status, output, events = f.run()
                    assert status == (1 if unsupported else 0); f.unchanged()
                    assert not any(e['op'] == 'unpublish' for e in events)
                    if unsupported:
                        assert not effects(events) and b'Route disposition' not in output
                        assert tuple(snapshot(q) for q in f.definitions) == definitions_before
                    else: assert all(not q.exists() for q in f.definitions)
        # F004 round 2: a canonical cloudflare selection must not hide a
        # manager-significant spaced override/file directive, reset, or key
        # split by continuation. The literal/expanded CLI files both exist;
        # this is source fixture modeling, NOT real systemd expansion.
        directives = (
            'Environment = "{assignment}"', 'Environment\t= "{assignment}"',
            ' \tEnvironment \t =\t"{assignment}"', '\tEnvironment= "{assignment}"',
            'Environment= \t"{assignment}"', 'Environment =',
            'EnvironmentFile = "{mapping}"', '\tEnvironmentFile\t = \t"{mapping}"',
            'Environment\\\n = "{assignment}"', 'Environment \\\n\t= "{assignment}"',
            'EnvironmentFile\\\n = "{mapping}"',
            'PassEnvironment = HERDR_RELAY_ENV', 'UnsetEnvironment \t= HERDR_RELAY_ENV',
        )
        seams = ((False, ('uninstall',)),
                 (True, ('uninstall', '--cli-route-disposition=removed')),
                 (False, ('rollback-cli-setup',)))
        for quoting in ('current', 'legacy'):
            for index in (0, 1):
                for directive in directives:
                    # Test a hidden directive on either side of the admitted
                    # assignment, not just the first Environment occurrence.
                    for before in (False, True) if directive in (directives[0], directives[6]) else (False,):
                        if not cell(f"systemd/override/{quoting}/{index}/{directives.index(directive)}/{'before' if before else 'after'}"): continue
                        f = Fixture(parent, 'Linux', mode='cloudflare', quoting=quoting)
                        literal_dir = f.root / 'override-%u'; literal_dir.mkdir(mode=0o700)
                        expanded_dir = f.root / 'override-fixture-user'; expanded_dir.mkdir(mode=0o700)
                        literal = literal_dir / 'cli.env'; expanded = expanded_dir / 'cli.env'
                        for selected in (literal, expanded):
                            selected.write_text('HERDR_RELAY_TRANSPORT=tailscale-cli\n'); selected.chmod(0o600)
                        mapping = f.root / 'override.environment'
                        mapping.write_text('HERDR_RELAY_ENV=' + str(expanded) + '\n'); mapping.chmod(0o600)
                        q = f.definitions[index]; canonical = q.read_text().removeprefix('[Service]\n')
                        hidden = directive.format(assignment='HERDR_RELAY_ENV=' + str(literal), mapping=str(mapping)) + '\n'
                        q.write_text('[Service]\n' + (hidden + canonical if before else canonical + hidden))
                        f.protected.extend((literal_dir, expanded_dir, mapping)); f.original = f.protected_snapshot()
                        f.environment['HERDR_CLI_SETUP_ROLLBACK'] = '1'
                        definitions_before = tuple(snapshot(q) for q in f.definitions)
                        for direct, args in seams:
                            status, output, events = f.run(direct=direct, args=args)
                            assert status == 1 and not effects(events) and b'Route disposition' not in output
                            assert b'for both Herdr labels' not in output
                            assert tuple(snapshot(q) for q in f.definitions) == definitions_before; f.unchanged()
                # A supported encoding outside [Service] is not a manager
                # environment assignment and cannot establish its selection.
                for section in ('', '[Unit]\n', '[Install]\n'):
                    if not cell(f"systemd/section/{quoting}/{index}/{section.strip().strip('[]').lower() or 'none'}"): continue
                    f = Fixture(parent, 'Linux', mode='cloudflare', quoting=quoting)
                    q = f.definitions[index]; q.write_text(section + q.read_text().removeprefix('[Service]\n'))
                    f.environment['HERDR_CLI_SETUP_ROLLBACK'] = '1'
                    definitions_before = tuple(snapshot(q) for q in f.definitions)
                    for direct, args in seams:
                        status, output, events = f.run(direct=direct, args=args)
                        assert status == 1 and not effects(events) and b'Route disposition' not in output
                        assert tuple(snapshot(q) for q in f.definitions) == definitions_before; f.unchanged()
        # F004 round 3: the primary file is NOT the effective configuration.
        # Strict --all metadata admits only its exact fragment, no overlays,
        # no pending reload. These are models, not native-manager qualification.
        all_seams = tuple((direct, ('uninstall',) + (() if prior is None else ('--cli-route-disposition=' + prior,)))
                          for direct in (False, True) for prior in (None, 'retained', 'removed'))
        all_seams += ((False, ('rollback-cli-setup',)),)
        for index in (0, 1):
            for present in ((0, 1), (index,)):  # includes legacy-only survivors
                for location in ('user', 'runtime', 'system', 'vendor', 'hierarchical'):
                    if not cell(f'systemd/overlay/{index}/{len(present)}/{location}'): continue
                    f = Fixture(parent, 'Linux', present=present, mode='cloudflare')
                    cli = f.root / 'effective-cli.env'; cli.write_text('HERDR_RELAY_TRANSPORT=tailscale-cli\n'); cli.chmod(0o600)
                    directory = (f.definitions[index].parent / (f.labels[index] + '.d') if location == 'user' else
                                 f.definitions[index].parent / 'herdr-.service.d' if location == 'hierarchical' else
                                 f.root / location / 'systemd/user' / (f.labels[index] + '.d'))
                    directory.mkdir(mode=0o700, parents=True)
                    overlay = directory / 'override.conf'
                    overlay.write_text('[Service]\nEnvironment="HERDR_RELAY_ENV=' + str(cli) + '"\n'); overlay.chmod(0o600)
                    f.model['overlays'] = {f.labels[index]: str(overlay)}; f.save()
                    f.protected.extend((cli, directory)); f.original = f.protected_snapshot()
                    configuration_refusal(f, all_seams)
            # Every required property must occur exactly once, INCLUDING
            # explicit empty FragmentPath/DropInPaths in absence observations.
            for present in ((0, 1), ()):
                for field in ('Id', 'LoadState', 'ActiveState', 'UnitFileState', 'FragmentPath', 'DropInPaths', 'NeedDaemonReload'):
                    for fault in ('meta-missing', 'meta-duplicate'):
                        if not cell(f'systemd/metadata-field/{index}/{len(present)}/{field}/{fault}'): continue
                        f = Fixture(parent, 'Linux', present=present, fault=fault, fault_index=index)
                        f.model['metadata_field'] = field; f.save()
                        configuration_refusal(f, seams)
            for fault in ('meta-query-fail', 'meta-partial-fail', 'meta-malformed', 'meta-extra', 'meta-oversize', 'meta-nul'):
                if not cell(f'systemd/metadata-fault/{index}/{fault}'): continue
                f = Fixture(parent, 'Linux', fault=fault, fault_index=index)
                configuration_refusal(f, seams)
            for override in ({'Id': 'unrelated.service'}, {'LoadState': 'masked'}, {'ActiveState': 'mystery'},
                             {'UnitFileState': 'enabled-runtime'}, {'NeedDaemonReload': 'yes'}, {'NeedDaemonReload': ''},
                             {'FragmentPath': ''}, {'FragmentPath': 'relative.service'}, {'LoadState': 'not-found'}):
                if not cell(f"systemd/metadata-override/{index}/{'+'.join(k + '=' + v for k, v in override.items()).replace('/', '_')}"): continue
                f = Fixture(parent, 'Linux'); f.model['metadata_overrides'] = {f.labels[index]: override}; f.save()
                configuration_refusal(f, seams)
            if cell(f'systemd/fragment-bystander/{index}'):
                f = Fixture(parent, 'Linux')
                f.model['metadata_overrides'] = {f.labels[index]: {'FragmentPath': str(f.private / 'bystander')}}; f.save()
                configuration_refusal(f, all_seams)  # never inspect the arbitrary fragment
            if cell(f'systemd/cached-unit/{index}'):
                f = Fixture(parent, 'Linux'); f.definitions[index].unlink()
                configuration_refusal(f, all_seams)  # cached loaded unit is NOT fresh absence
            for override in ({'FragmentPath': '/fixture/cached.service'}, {'DropInPaths': '/fixture/runtime/override.conf'},
                             {'LoadState': 'loaded'}, {'ActiveState': 'active'}, {'UnitFileState': 'enabled'}, {'NeedDaemonReload': 'yes'}):
                if not cell(f"systemd/absent-override/{index}/{'+'.join(k + '=' + v for k, v in override.items()).replace('/', '_')}"): continue
                f = Fixture(parent, 'Linux', present=())
                f.model['metadata_overrides'] = {f.labels[index]: override}; f.save()
                configuration_refusal(f, seams)
            # Explicit not-found enablement is also a coherent absence control.
            if cell(f'systemd/absent-not-found/{index}'):
                f = Fixture(parent, 'Linux', present=())
                f.model['metadata_overrides'] = {f.labels[index]: {'UnitFileState': 'not-found'}}; f.save()
                assert f.run()[0] == 0 and not effects(f.events()); f.unchanged()
            # Drift at initial removal admission or either existing fresh
            # resource/quiet seam refuses, with honest label-local progress.
            for at in (2, 3, 4):
                for field in ('FragmentPath', 'DropInPaths', 'NeedDaemonReload'):
                    if not cell(f'systemd/drift/{index}/{at}/{field}'): continue
                    f = Fixture(parent, 'Linux')
                    overlay = f.root / 'drift.conf'; overlay.write_text('[Service]\nEnvironment=HERDR_RELAY_ENV=/fixture/cli.env\n'); overlay.chmod(0o600)
                    f.protected.append(overlay); f.original = f.protected_snapshot()
                    f.model.update(configuration_drift_label=f.labels[index], configuration_drift_at=at,
                                   configuration_drift={field: 'yes' if field == 'NeedDaemonReload' else str(overlay)})
                    f.save(); before = snapshot(f.definitions[index])
                    status, output, events = f.run()
                    assert status == 1 and b'for both Herdr labels' not in output
                    assert snapshot(f.definitions[index]) == before and not f.definitions[1 - index].exists()
                    target_effects = [e['op'] for e in effects(events) if e['label'] == f.labels[index]]
                    assert target_effects == ([] if at == 2 else ['stop', 'disable']); f.unchanged()
        # Partial query output/binary/over-limit failure also propagates under
        # if/|| callers where errexit cannot supply the safety contract.
        for fault in ('meta-query-fail', 'meta-partial-fail', 'meta-nul', 'meta-oversize'):
            for conditional in (True, 'or'):
                if not cell(f"systemd/meta-caller/{fault}/{'or' if conditional == 'or' else 'if'}"): continue
                f = Fixture(parent, 'Linux', fault=fault)
                before = tuple(snapshot(q) for q in f.definitions)
                status, _, events = f.run(conditional=conditional)
                assert status == 1 and not effects(events)
                assert tuple(snapshot(q) for q in f.definitions) == before; f.unchanged()
        # No-overlay once-decoding and genuine absence remain positive controls
        # above. Configuration counters must never advance lifecycle polling.
    finish_cells()
    print('PASS actual service-removal fixture source: NUL-safe lifecycle capture, effective configuration, identities, labels, ancestors, once-decoding, quiescence and cancellation')


if __name__ == '__main__': main()
