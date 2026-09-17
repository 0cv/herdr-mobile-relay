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
wait_for_fixture_file() {
  fixture_path=$1
  fixture_deadline=$(cat "$root/state/deadline" 2>/dev/null || printf '')
  while ! [ -e "$fixture_path" ]; do
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
      while ! [ -e "$root/child-exit-observed" ]; do /bin/sleep 0.01; done
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
  elif [ "$2" = "terminate" ] && [ "$mode" = "listener-concurrent-exit-before-close" ]; then
    kill -TERM "$(cat "$root/endpoint.pid")" 2>/dev/null || true
  fi
  ;;
xcodebuild)
  case "$mode" in
    listener-pending-*) publish_fixture_pid "$root/runner.pid" "$$" || exit 2 ;;
    listener-concurrent-exit-before-close) publish_fixture_pid "$root/xcode.pid" "$$" || exit 2 ;;
    listener-initial-race-receipt|listener-initial-race-receipt-command-error|listener-initial-race-receipt-hash-error)
      touch "$root/initial-race-receipt-child-started"
      ;;
  esac
  exec "$STARTUP_TEST_EXECUTABLE" "$STARTUP_TEST_WDA_SERVER" xcodebuild "$@"
  ;;
esac
exit 0
`;
const wdaServer = `import { createServer } from 'node:http';
import { spawn, spawnSync } from 'node:child_process';
import { appendFileSync, existsSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
const root = process.env.STARTUP_TEST_ROOT;
const mode = process.env.STARTUP_TEST_MODE;
const role = process.env.STARTUP_TEST_WDA_SERVER_ROLE || 'owner';
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
if (role !== 'endpoint' || !existsSync(xcodePid)) publishPid(xcodePid);
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
if (mode !== 'listener-concurrent-exit-before-close' || role === 'endpoint') {
  process.on('SIGTERM', stop);
  process.on('SIGINT', stop);
  process.on('exit', () => { remove(runnerPid); remove(xcodePid); });
  if (mode === 'listener-concurrent-exit-before-close' && role === 'endpoint') {
    const managedChildPid = readFileSync(xcodePid, 'utf8').trim();
    const actualExitObserver = setInterval(() => {
      if (!existsSync(join(root, 'exit-child-requested')) || existsSync(join(root, 'child-exit-observed'))) return;
      const probe = spawnSync('/bin/ps', ['-p', managedChildPid, '-o', 'stat='], {encoding: 'utf8'});
      if (probe.status !== 0 || /^Z\\s*$/u.test(probe.stdout.trim())) {
        clearInterval(actualExitObserver);
        writeFileSync(join(root, 'child-exit-observed'), String(Date.now()));
      }
    }, 1);
  }
}
if (mode !== 'listener-concurrent-exit-before-close' || role === 'endpoint') {
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

export const xctestOwnerTests: Array<[string, () => Promise<void>]> = [];
for (const mode of ['ready', 'ready-then-oversized', 'early', 'exit-before-close', 'invalid', 'occupied', 'ownership', 'ambiguous', 'product', 'receipt-failure', 'receipt-missing-then-valid', 'receipt-product-changed', 'swap', 'pid-reuse', 'oversized', 'delayed', 'credential', 'credential-binary', 'credential-binary-failure', 'stream-framing', 'stream-framing-reversed', 'stream-utf8', 'stream-eof', 'stream-finalization-failure', 'output-below', 'output-equal', 'output-above', 'output-combined', 'output-shrinking', 'output-expanding', 'noisy-cleanup', 'status-noisy-first-failure', 'status-first-failure', 'status-forged-markers', 'status-evidence-disappear', 'listener-mjpeg-duplicate', 'listener-bundle-mismatch', 'listener-hash-mismatch', 'listener-invalid-pid', 'listener-ready-owner-evidence-write', 'listener-first-failure', 'listener-status-one-stderr', 'listener-second-failure', 'listener-uncoordinated-negative', 'listener-endpoints-disappear', 'listener-hash-after-freeze', 'listener-initial-race', 'listener-initial-race-birth', 'listener-initial-race-executable', 'listener-initial-race-hash', 'listener-initial-race-product', 'listener-initial-race-receipt', 'listener-initial-race-receipt-command-error', 'listener-initial-race-receipt-hash-error', 'listener-initial-race-command-error', 'listener-initial-race-command-error-late', 'listener-initial-race-budget', 'listener-initial-race-hash-budget', 'listener-initial-race-cleanup-identity', 'listener-http-only', ...pendingModes]) {
  xctestOwnerTests.push([`Native startup actual XCTest supervisor ${mode}`, async () => {
    const root = await mkdtemp(join(tmpdir(), 'herdr-xctest-test-'));
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
    const child = spawn(process.execPath, [supervisorPath, 'supervise'], { env, stdio: ['ignore', 'ignore', 'pipe'] });
    const supervisorStarted = Date.now();
    let errors = '';
    child.stderr.on('data', chunk => { errors += chunk.toString(); });
    let done = false;
    let assertionsPassed = false;
    let initialRaceFirstFailure: unknown;
    let firstFailureObservation: Record<string, unknown> | undefined;
    const exited = new Promise<void>(resolve => child.on('close', () => { done = true; resolve(); }));
    try {
      const credentialMode = ['credential', 'credential-binary', 'credential-binary-failure'].includes(mode);
      const streamMode = ['stream-framing', 'stream-framing-reversed', 'stream-utf8', 'stream-eof', 'stream-finalization-failure'].includes(mode);
      const outputBoundaryMode = ['output-below', 'output-equal', 'output-above', 'output-combined', 'output-shrinking', 'output-expanding'].includes(mode);
      if (listenerEvidenceModes.includes(mode)) {
        const observationDeadline = Date.now() + 15_000;
        while (Date.now() < observationDeadline && !firstFailureObservation) {
          if (existsSync(join(state, 'owner.json'))) {
            const current = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8')) as {
              firstFailure?: unknown;
              endedAt?: string;
              listenerValidation?: unknown;
              diagnostics?: {lifecycle?: Array<{event?: string; detail?: {first?: boolean}}>};
            };
            const firstFailureEvent = current.diagnostics?.lifecycle?.find((event: {event?: string; detail?: {first?: boolean}}) => event.event === 'failure-observed' && event.detail?.first === true);
            if (current.firstFailure && firstFailureEvent && !current.endedAt) {
              firstFailureObservation = {
                capturedBeforeOwnerEnded: true,
                capturedAt: new Date().toISOString(),
                firstFailure: JSON.parse(JSON.stringify(current.firstFailure)),
                listenerValidation: current.listenerValidation ? JSON.parse(JSON.stringify(current.listenerValidation)) : undefined,
                failureEvent: JSON.parse(JSON.stringify(firstFailureEvent)),
              };
              await writeFile(join(root, 'first-failure-observation.json'), `${JSON.stringify(firstFailureObservation, null, 2)}\n`);
            }
          }
          if (!firstFailureObservation) await pause();
        }
        assert.ok(firstFailureObservation, `${mode} did not retain a pre-cleanup first-failure observation`);
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
        while (!done && Date.now() < pendingDeadline) {
          if (existsSync(join(state, 'owner.json'))) {
            const current = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
            const lifecycle = current.diagnostics?.lifecycle || [];
            if (current.firstFailure || lifecycle.some((event: {event: string}) => event.event === 'runner-candidate-pinned')) break;
          }
          await pause();
        }
        const pendingOwner = JSON.parse(await readFile(join(state, 'owner.json'), 'utf8'));
        const pendingLifecycle = pendingOwner.diagnostics?.lifecycle || [];
        const pinned = pendingLifecycle.find((event: {event: string}) => event.event === 'runner-candidate-pinned');
        if (mode === 'listener-pending-ambiguous' || mode === 'listener-pending-command-error' || mode === 'listener-pending-invalid-pid') {
          assert.equal(pinned, undefined);
        } else {
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
          while (mode !== 'listener-pending-stop' && mode !== 'listener-pending-child-error' && !existsSync(join(root, 'pending-candidate-boundary')) && Date.now() < boundaryDeadline) await pause();
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
        const concurrentDeadline = Date.now() + 15_000;
        while (!done && Date.now() < concurrentDeadline && !existsSync(join(root, 'child-exit-intent'))) await pause();
        assert.equal(existsSync(join(root, 'child-exit-intent')), true);
        await writeFile(join(root, 'child-exit-continue'), 'release actual child exit');
        while (!done && Date.now() < concurrentDeadline && !existsSync(join(root, 'child-exit-observed'))) await pause();
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
      assert.ok(Buffer.byteLength(await readFile(join(state, 'owner-private.json'), 'utf8')) <= 1048576);
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
      if (mode === 'listener-first-failure' || mode === 'listener-second-failure') {
        const childSignal = lifecycle.find((event: {event: string; detail?: {signal?: string; accepted?: boolean}}) => event.event === 'child-signal-requested');
        const childExit = lifecycle.find((event: {event: string}) => event.event === 'child-exit');
        const childClose = lifecycle.find((event: {event: string}) => event.event === 'child-close');
        const childStopFinished = lifecycle.find((event: {event: string}) => event.event === 'child-stop-finished');
        assert.ok(childSignal && childExit && childClose && childStopFinished);
        assert.equal(childSignal.detail?.signal, 'SIGTERM');
        assert.equal(childSignal.detail?.accepted, true);
        assert.ok(childSignal.id < childExit.id && childExit.id < childClose.id && childClose.id < childStopFinished.id);
        assert.equal(lifecycle.some((event: {event: string}) => event.event === 'owned-listener-termination-requested'), false);
      }
      if (!['ready', 'ready-then-oversized', 'status-first-failure', 'listener-http-only', 'listener-pending-ready', 'listener-pending-http-only', 'stream-framing', 'stream-framing-reversed', 'stream-utf8', 'stream-eof', 'output-below', 'output-equal', 'output-shrinking', 'output-expanding'].includes(mode)) assert.ok(owner.firstFailure);
      if (owner.firstFailure) assert.ok(Array.isArray(owner.firstFailure.causalCommands));
      if (listenerEvidenceModes.includes(mode)) {
        const retainedObservation = JSON.parse(await readFile(join(root, 'first-failure-observation.json'), 'utf8')) as {
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
      if (mode === 'listener-uncoordinated-negative') {
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
      if (listenerEvidenceModes.includes(mode)) {
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
        const preCleanupObservation = JSON.parse(await readFile(join(root, 'first-failure-observation.json'), 'utf8')) as {
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
      if (mode === 'ready' || mode === 'status-first-failure' || mode === 'listener-http-only') assert.equal(owner.error, undefined);
      if (mode !== 'ownership' && mode !== 'listener-pending-ownership') assert.match(await readFile(join(state, 'ios-wda-system.log'), 'utf8'), /output":"suppressed"/u);
      assert.equal(existsSync(join(root, 'runner.pid')), false);
      assertionsPassed = true;
    } finally {
      if (!done) {
        if (mode === 'listener-ready-owner-evidence-write') await writeFile(join(root, 'ready-failure-fallback-continue'), 'failed test cleanup');
        await writeFile(join(state, 'stop'), 'failed test cleanup');
        child.kill('SIGTERM');
        await exited;
      }
      const diagnosticRootBase = process.env.XCTEST_DIAGNOSTIC_ROOT;
      if (diagnosticRootBase && (!assertionsPassed || listenerEvidenceModes.includes(mode))) {
        const diagnosticRoot = listenerEvidenceModes.includes(mode) ? join(diagnosticRootBase, mode) : diagnosticRootBase;
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
