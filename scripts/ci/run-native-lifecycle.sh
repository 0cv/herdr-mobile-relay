#!/bin/bash
# Native-manager lifecycle driver (partial Q1, not M3): GitHub-hosted DISPOSABLE runners only (the guards below stop casual and accidental runs elsewhere; they are not a security boundary). Modes, one CI job each: probe (its receipt is written even on failure), producer
# (make cross-build + the unchanged package-release.sh) and lifecycle (real A install/start/stop/relaunch/uninstall and refusals behind runner-local HTTPS). Product commands run as the SUBJECT; root runs none. Bash 3.2.
set -eu

refuse() { printf 'native lifecycle refused: %s\n' "$1" >&2; exit 2; }
# GITHUB_ACTIONS and RUNNER_ENVIRONMENT are values the caller exports, so they prove nothing alone: a shell on a developer's Mac can set both. The GitHub runner's own worker process must also be an
# ancestor of this shell (the step's shell is its child); a process list is not an environment value. The process list is read with the ABSOLUTE /bin/ps: a bare `ps` is resolved through the caller's
# PATH or an exported shell function, and either can print `Runner.Worker`. The guard stops casual and accidental runs, including a caller who shims or exports a `ps`; it cannot stop an unprivileged
# same-user caller who forges a process named Runner.Worker (or runs code before this script starts), and such a caller can already run sudo commands directly.
runner_ancestor() {
    local ps=/bin/ps pid=$$ args hops=0
    while [ "$hops" -lt 12 ]; do
        hops=$((hops + 1))
        args=$("$ps" -o args= -p "$pid" 2> /dev/null) || return 1
        case "${args%% *}" in */Runner.Worker | Runner.Worker) return 0 ;; esac
        pid=$("$ps" -o ppid= -p "$pid" 2> /dev/null) || return 1
        pid=${pid//[[:space:]]/}
        case "$pid" in '' | *[!0-9]*) return 1 ;; esac
        [ "$pid" -gt 1 ] || return 1
    done
    return 1
}
mode=${1:-}
case "$mode" in probe | producer | lifecycle) [ "$#" -eq 1 ] || refuse 'unexpected arguments' ;; *) refuse 'usage: probe|producer|lifecycle' ;; esac
[ "${GITHUB_ACTIONS:-}" = true ] || refuse 'requires a GitHub Actions run'
[ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] || refuse 'requires a GitHub-hosted runner'
runner_ancestor || refuse 'requires the GitHub Actions runner process (Runner.Worker) as an ancestor; exported variables alone are not enough'
[ "$(id -u)" -ne 0 ] || refuse 'must not run as root'
OS=$(uname -s)
case "$OS" in Darwin | Linux) ;; *) refuse 'requires a Darwin or Linux runner' ;; esac
printf '%s' "${GITHUB_SHA:-}" | grep -Eq '^[0-9a-f]{40}$' || refuse 'GITHUB_SHA is not a full commit'
case "${GITHUB_RUN_ID:-x}${GITHUB_RUN_ATTEMPT:-x}" in *[!0-9]*) refuse 'run identity is not numeric' ;; esac
[ -n "${HOME:-}" ] && [ -d "$HOME" ] || refuse 'HOME is not an existing directory'
case "${RUNNER_TEMP:-}" in /*) ;; *) refuse 'RUNNER_TEMP is not an absolute path' ;; esac
[ -d "$RUNNER_TEMP" ] && [ ! -L "$RUNNER_TEMP" ] || refuse 'RUNNER_TEMP is not a real directory'
HERE=$(git rev-parse --show-toplevel 2> /dev/null) || refuse 'not inside a Git checkout'
[ "$(pwd -P)" = "$HERE" ] || refuse 'must run from the repository root'
[ "$(git rev-parse HEAD 2> /dev/null)" = "$GITHUB_SHA" ] || refuse 'checkout HEAD does not match GITHUB_SHA'
env | grep -Eiq '^(http_proxy|https_proxy|all_proxy|no_proxy|curl_ca_bundle|ssl_cert_file|ssl_cert_dir|curl_ssl_backend|curl_home|curl_config|requests_ca_bundle)=' &&
    refuse 'an inherited proxy or CA override would reach the product curl; the caller environment must be clean'
PYO=$(command -v python3 2> /dev/null) || refuse 'python3 was not found'
"$PYO" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 11) else 1)' || refuse 'python3 3.11 or newer is required'

SHA=$GITHUB_SHA RUN=$GITHUB_RUN_ID ATT=$GITHUB_RUN_ATTEMPT
HOSTPY="$HERE/scripts/ci/native-lifecycle-host.py"
MATRIX="$HERE/tests/native_manager_lifecycle_matrix.py"
EV="$RUNNER_TEMP/native-evidence"
BUNDLE="$RUNNER_TEMP/native-bundle"
PRODJ="$RUNNER_TEMP/native-producer/producer.json"
VER=$(sed -n 's/^version = "\([^"]*\)"/\1/p' herdr-plugin.toml)
RELAY_PORT=18375 CHILD_PORT=18376
HOSTN="nlt-$RUN-$ATT.test"
if [ "$OS" = Linux ]; then
    [ "$(uname -m)" = x86_64 ] || refuse 'requires an x86_64 Linux runner'
    SUBJ="nlt-$RUN-$ATT" THOME="/home/nlt-$RUN-$ATT" TARGET=linux_amd64 MGR=systemd MGRPATH=/usr/bin/systemctl LABEL=herdr-mobile-relay.service LEGACY=herdr-remote.service
    DEF="$THOME/.config/systemd/user/$LABEL" LDEF="$THOME/.config/systemd/user/$LEGACY" PY=/usr/bin/python3 S="$THOME/nlt-stage" SHIMDIR="$THOME/.local/bin"
else
    [ "$(uname -m)" = arm64 ] || refuse 'requires an arm64 macOS runner'
    SUBJ=$(id -un) THOME=$HOME TARGET=darwin_arm64 MGR=launchd MGRPATH=/bin/launchctl LABEL=com.herdr-mobile-relay.service LEGACY=com.herdr-remote.service
    DEF="$HOME/Library/LaunchAgents/$LABEL.plist" LDEF="$HOME/Library/LaunchAgents/$LEGACY.plist" PY=$PYO S="$RUNNER_TEMP/nlt-stage" SHIMDIR="$RUNNER_TEMP/nlt-stage/bin"
fi
CONF="$THOME/.config/herdr-mobile-relay" REL="$THOME/.local/share/herdr-mobile-relay"
SPATH="$S/bin:$THOME/.local/bin:/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"
SUID=$(id -u) SGID=$(id -g) SVC="$REL/current/relay/service.sh"
SENV=("PATH=$SPATH")
TSTATE=n/a ASTO=120
FOREIGN_DEF='' STUB='' BYL='' BYD='' PRODUCT='' LINGER_ON='' TRUSTED='' STAGE_OWNED='' MID='' HOSTS_LINE='' FRONT_JOB='' CA_SHA1='' RC=0 BEFORE='' BYP0='' BY_BASE='' CNT0='' ACCT_CREATED=''

# The guards above refuse (exit 2, nothing written); from here, before any manager query or sudo, the finalizer cleans up and writes the receipt for EVERY outcome.
[ ! -L "$EV" ] || refuse 'evidence directory is a symlink'
(umask 077 && mkdir -p "$EV") && chmod 0700 "$EV" || refuse 'cannot create the private evidence directory'
FACTS="$EV/facts.txt" CASES="$EV/cases.jsonl" CLEANUPS='' PHASE=setup CASE='' FAILED=none IDENT=pending
: > "$FACTS"
: > "$CASES"
fact() { printf '%s=%s\n' "$1" "$2" >> "$FACTS"; }
phase() { PHASE=$1; fact phase "$1"; }
fail() { printf 'native lifecycle FAILED in %s: %s\n' "${CASE:-$PHASE}" "$1" >&2; FAILED=${CASE:-$PHASE}; exit 1; }
on_cleanup() { CLEANUPS="$1 $CLEANUPS"; }
mx() { "$PYO" -I -B "$MATRIX" "$@"; }
OX() { "$PYO" -I -B "$HOSTPY" "$@"; }
jget() { "$PYO" -I -B -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"; }
# Runs on EXIT, INT and TERM for every outcome after the hosted guards. Cleanup callbacks run last-registered-first, each in its own subshell,
# and $clean already holds the result of every earlier callback when the next one starts: cl_user and cl_stage use that to refuse to delete a
# home or stage that holds a definition an earlier callback retained. The receipt is then built from the facts, the cases and $clean, and the
# original exit status is preserved (a receipt failure only matters when the driver itself succeeded).
finish() {
    rc=$?
    trap - EXIT INT TERM
    set +e
    clean=pass
    for fn in $CLEANUPS; do ( "$fn" ) || clean=fail; done
    fact cleanup "$clean"
    [ "$rc" -eq 0 ] || [ "$FAILED" != none ] || FAILED=unexpected
    id_args="--sha $SHA --run-id $RUN --run-attempt $ATT"
    # shellcheck disable=SC2086  # fixed, space-free values
    case "$mode" in
        probe) mx probe $id_args --os "$OS" --uid "$(id -u)" --facts "$FACTS" --exit "$rc" --output "$EV/probe.json" ;;
        producer) mx producer $id_args --release-dir dist/release --version "$VER" --artifact "lifecycle-bundle-$SHA-$ATT" --exit "$rc" --output "$EV/producer.json" ;;
        lifecycle) mx cases $id_args --os "$OS" --records "$CASES" --producer "$PRODJ" --cleanup "$clean" --failed "$FAILED" --exit "$rc" --output "$EV/cases.json" ;;
    esac
    receipt=$?
    [ "$rc" -ne 0 ] || rc=$receipt
    exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for() { local n=$1; shift; while ! "$@" > /dev/null 2>&1; do n=$((n - 1)); [ "$n" -gt 0 ] || return 1; sleep 1; done; }
bsudo() { sudo -n "$PY" -I -B "$HOSTPY" run --timeout 120 -- "$@"; }  # root runs the bounded runner, so a timeout kills the whole root-owned group

priv() {  # The ONLY sudo sites (the long-lived TLS front is the one unbounded call). Names are validated first; user operations touch only the disposable test user.
    local op=$1
    shift
    case "$op" in useradd | userdel | linger-on | linger-off | terminate | mgr-start | mgr-stop)
        { [ "$OS" = Linux ] && printf '%s' "$SUBJ" | grep -Eq '^nlt-[0-9]+-[0-9]+$' && [ "$SUID" -ge 1000 ] && [ "$SUID" -ne "$(id -u)" ]; } ||
            [ "$op" = useradd ] || fail "refused privileged $op" ;;
    esac
    # userdel --remove deletes the home recursively: only the passwd entry this job created may be removed.
    [ "$op" != userdel ] || owned_account || fail 'refused privileged userdel: the account is not the one this job created'
    case "$op" in
        useradd) [ "$OS" = Linux ] && no_such_user "$SUBJ" && gone "$THOME" && bsudo useradd --create-home --shell /bin/bash --user-group "$SUBJ" && ACCT_CREATED=yes ;;
        userdel) bsudo userdel --remove "$SUBJ" ;;
        linger-on) bsudo loginctl enable-linger "$SUBJ" ;;
        linger-off) bsudo loginctl disable-linger "$SUBJ" ;;
        terminate) bsudo loginctl terminate-user "$SUBJ" ;;
        mgr-start) bsudo systemctl start "user@$SUID.service" ;;
        mgr-stop) bsudo systemctl stop "user@$SUID.service" ;;
        hosts-add) printf '%s\n' "$HOSTS_LINE" | bsudo tee -a /etc/hosts > /dev/null ;;
        hosts-del) bsudo tee /etc/hosts < "$EV/hosts.new" > /dev/null ;;
        trust-add)
            if [ "$OS" = Linux ]; then
                bsudo install -m 0644 "$S/pki/ca.pem" "/usr/local/share/ca-certificates/herdr-nlt-$RUN-$ATT.crt" && bsudo update-ca-certificates
            else
                bsudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain "$S/pki/ca.pem"
            fi ;;
        trust-del)
            if [ "$OS" = Linux ]; then
                bsudo rm -f "/usr/local/share/ca-certificates/herdr-nlt-$RUN-$ATT.crt" && bsudo update-ca-certificates --fresh
            else
                bsudo security remove-trusted-cert -d "$S/pki/ca.pem" && bsudo security delete-certificate -Z "$CA_SHA1" /Library/Keychains/System.keychain
            fi ;;
        front) sudo -n "$PY" -I -B "$S/native-lifecycle-host.py" tls-front --cert "$S/pki/leaf.pem" --key "$S/pki/leaf.key" --uid "$SUID" --gid "$SGID" \
            --upstream "$1" --log "$S/run/hits.log" --run "$S/run" ;;
        *) fail "unlisted privileged operation $op" ;;
    esac
}

senv_init() {
    SENV=("HOME=$THOME" "USER=$SUBJ" "LOGNAME=$SUBJ" "LANG=C" "LC_ALL=C" "TMPDIR=$S/tmp" "HERDR_RELAY_ENV=$CONF/relay.env" "PATH=$SPATH" "BIN_DIR=$SHIMDIR" "HERDR_RELAY_BIN_DIR=$SHIMDIR")  # the product shim goes to SHIMDIR, never the runner's ~/.local/bin
    [ "$OS" != Linux ] || SENV+=("XDG_RUNTIME_DIR=/run/user/$SUID")
}
as_subject() {
    if [ "$OS" = Linux ]; then
        sudo -n -u "$SUBJ" /usr/bin/timeout -k 5 "$ASTO" env -i "${SENV[@]}" /bin/sh -c 'cd "$HOME" && exec "$@"' _ "$@"  # timeout runs as the test UID, so it can kill its own group
    else
        env -i "${SENV[@]}" /bin/sh -c 'cd "$HOME" && exec "$@"' _ "$@"
    fi
}
senv_init
HX() { as_subject "$PY" -I -B "$S/native-lifecycle-host.py" "$@"; }
RUNB() { local t=$1; shift; ASTO=$((t + 30)) HX run --timeout "$t" -- "$@"; }
P() { RUNB "$@" < /dev/null; }
sha_file() { as_subject /bin/sh -c 'if command -v sha256sum > /dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi' _ "$1" | cut -d' ' -f1; }
stage_put() { as_subject /bin/sh -c '[ ! -L "$1" ] && umask 077 && cat > "$1" && chmod "$2" "$1"' _ "$2" "$3" < "$1"; }  # never through a symlink

cl_user() {
    local bad=0 linger="/var/lib/systemd/linger/$SUBJ" rc=0
    [ "$OS" = Linux ] || return 0
    getent passwd "$SUBJ" > /dev/null 2>&1 || rc=$?
    case "$rc" in 2) return 0 ;; 0) ;; *) return 1 ;; esac  # exit 2 (key not found) alone proves the account absent; any other failed lookup is unknown and fails the cleanup
    owned_account || return 1  # nothing runs as, or is removed from, an account this job did not create (HX stop executes a file in its home)
    [ "$clean" = pass ] || return 1  # an earlier callback retained a definition (unknown identity): never delete the home that holds it
    HX stop --run "$S/run" > /dev/null 2>&1 || true
    if [ "$LINGER_ON" = yes ]; then priv linger-off && gone "$linger" || bad=1; fi  # restore only what this job changed, and prove it
    priv terminate > /dev/null 2>&1 || true
    priv mgr-stop > /dev/null 2>&1 || true
    wait_for 20 priv userdel || return 1
    no_such_user "$SUBJ" && [ "$bad" = 0 ] && gone "$linger"
}
user_manager_ok() { as_subject systemctl --user show-environment; }
unit_state() {  # `systemctl is-active` prints the state and exits 0 for `active`, 3 for `inactive`; any other pair (a timeout, 1, 4, 127, a signal, a state with the wrong status) is an unanswered query: unknown
    local out rc=0
    out=$("$@" 2> /dev/null) || rc=$?
    case "$rc:$out" in 0:active) echo active ;; 3:inactive) echo inactive ;; *) echo unknown ;; esac
}
mgr_down() { [ "$(unit_state systemctl is-active "user@$SUID.service")" = inactive ]; }
user_up() {
    local original
    original=$(loginctl show-user "$(id -un)" -p Linger --value 2> /dev/null) || original=unknown
    case "$original" in yes | no) fact linger_initial "$original" ;; *) fact linger_initial unknown ;; esac
    fact linger_changed no
    [ "$(systemctl --user show-environment > /dev/null 2>&1 && echo available || echo unavailable)" = available ] && fact manager_pre available || fact manager_pre unavailable
    on_cleanup cl_user
    phase user-create
    priv useradd || fail 'cannot create the disposable test user (its account or home directory already exists, or useradd failed)'
    SUID=$(id -u "$SUBJ") SGID=$(id -g "$SUBJ")
    [ "$SUID" -ge 1000 ] && [ "$SUID" -ne "$(id -u)" ] || fail 'the test user has an unexpected uid'
    senv_init
    none pgrep -u "$SUID" && sess=$(loginctl list-sessions --no-legend) && printf '%s\n' "$sess" | absent "$SUBJ" || fail 'the new test user already has processes or sessions, or they cannot be inspected'
    phase user-manager
    priv mgr-start || true
    if ! wait_for 30 user_manager_ok; then
        priv linger-on || fail 'cannot enable linger for the test user'
        LINGER_ON=yes
        fact linger_changed yes
        priv mgr-start || true
        wait_for 30 user_manager_ok || fail 'the test user manager is unavailable'
    fi
    fact isolated_uid available
    fact manager_post available
    fact default_target "$(unit_state as_subject systemctl --user is-active default.target)"
    [ "$(ps -u "$SUID" -o args= | grep -c 'systemd --user')" -ge 1 ] || fail 'no user manager process belongs to the test user'
}

none() { local rc=0; "$@" > /dev/null 2>&1 || rc=$?; [ "$rc" -eq 1 ]; }  # exit 1 (grep/pgrep: no match) alone proves absence; any other status is an inspection failure, unknown, never absent
absent() { none grep -qiF -- "$@"; }
gone() { [ "$(OX exists --path "$1" 2> /dev/null)" = no ]; }  # a path is absent only when the helper's lstat says ENOENT/ENOTDIR; permission denied or any other failure is unknown (as the Actions user)
sgone() { [ "$(HX exists --path "$1" 2> /dev/null)" = no ]; }  # the same, as the subject, for paths under the test user's home
no_such_user() { local rc=0; getent passwd "$1" > /dev/null 2>&1 || rc=$?; [ "$rc" -eq 2 ]; }  # getent exits 2 only for "key not found"; any other status is a failed lookup, unknown, never an absent account
owned_account() { local row; [ "$ACCT_CREATED" = yes ] && row=$(getent passwd "$SUBJ") && [ "$(printf '%s\n' "$row" | cut -d: -f1,3,6)" = "$SUBJ:$SUID:$THOME" ]; }  # only after this job's own successful useradd (ACCT_CREATED), only with a lookup that itself succeeded, and then only the name, uid and home it chose
residue_gone() {  # every path the product owns is definitely absent; an uninspectable path is residue
    local p
    for p in "$REL" "$CONF" "$THOME/.cache/herdr-mobile-relay" "$SHIMDIR/herdr-mobile-relay" "$SHIMDIR/herdr" "$DEF" "$LDEF"; do sgone "$p" || return 1; done
}
cl_hosts() {
    local rc=0
    [ -n "$HOSTS_LINE" ] || return 0
    grep -qF "$HOSTN" /etc/hosts || rc=$?
    case "$rc" in 1) return 0 ;; 0) ;; *) return 1 ;; esac  # exit 1 is the only proof of absence; a grep error is unknown and fails the cleanup
    grep -vxF "$HOSTS_LINE" /etc/hosts > "$EV/hosts.new" || rc=$?  # 1 is an empty result, 2 an error; the replacement must lose exactly the marked line or nothing is written
    [ "$rc" -le 1 ] && [ $(($(grep -c '' /etc/hosts) - $(grep -c '' "$EV/hosts.new"))) -eq 1 ] || return 1
    priv hosts-del && absent "$HOSTN" /etc/hosts
}
cl_trust() {
    local keys
    [ "$TRUSTED" = yes ] || return 0
    priv trust-del || return 1
    if [ "$OS" = Linux ]; then gone "/usr/local/share/ca-certificates/herdr-nlt-$RUN-$ATT.crt"; return; fi
    keys=$(security find-certificate -a -Z /Library/Keychains/System.keychain 2> /dev/null) || [ "$?" -eq 44 ] || return 1  # 44: the keychain holds no certificate; any other failure is unknown
    printf '%s\n' "$keys" | absent "$CA_SHA1"
}
unlistened() { "$PYO" -I -B -c 'import errno, socket; raise SystemExit(socket.socket().connect_ex(("127.0.0.1", 443)) != errno.ECONNREFUSED)'; }  # only a refused connection proves nothing listens
cl_front() {
    [ -n "$FRONT_JOB" ] || return 0
    HX stop --run "$S/run" > /dev/null 2>&1 || true
    wait_for 20 none pgrep -f 'native-lifecycle-hos[t].py' && unlistened
}
cl_stage() {  # the one recursive delete of the run: only the stage this job created, and never after another callback retained something
    [ "$STAGE_OWNED" = yes ] || return 0  # a pre-existing or refused stage was never adopted, so it is never removed
    [ "$clean" = pass ] || return 1  # retained fixtures (the foreign and bystander definitions on macOS) live under $S
    case "$S" in "$RUNNER_TEMP"/nlt-stage) /bin/rm -rf "$S" ;; esac
}
public_curl() { as_subject /usr/bin/curl --disable -fsS --proto '=https' --max-time 5 "https://$HOSTN/readyz"; }
negative_trust() {  # $1: the exit status of the CA-less request. Only curl 60 (the peer certificate failed verification) proves the CA is untrusted
    case "$1" in
        60) fact trust_negative refused ;;
        0 | 22) fact trust_negative accepted; fail 'the CA-less request passed TLS verification (the CA was already trusted)' ;;  # 22: TLS verified, then an HTTP error
        *) fact trust_negative unknown; fail "the CA-less request failed for a reason other than certificate verification (curl status $1)" ;;
    esac
}
tls_up() {  # $1: upstream port (0 = probe-only static body); $2 probe: also prove the negative (CA-less) and positive trust paths
    HOSTS_LINE="127.0.0.1 $HOSTN # herdr-nlt-$RUN-$ATT"
    last=$(tail -c1 /etc/hosts) && [ -z "$last" ] && absent "$HOSTN" /etc/hosts || fail 'cannot prove /etc/hosts ends with a newline and the per-run hostname is unmapped'
    unlistened || fail 'port 443 is occupied or cannot be probed'
    phase tls-pki
    CA_SHA1=$(as_subject /bin/sh -c '
        set -eu; umask 077; cd "$1"
        printf "[req]\ndistinguished_name=dn\nprompt=no\nx509_extensions=ca\n[dn]\nCN=herdr-nlt-ca-%s\n[ca]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n" "$3" > ca.cnf
        printf "basicConstraints=CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:%s\n" "$2" > leaf.cnf
        openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.pem -days 1 -config ca.cnf > /dev/null 2>&1
        openssl req -newkey rsa:2048 -nodes -keyout leaf.key -out leaf.csr -subj "/CN=$2" > /dev/null 2>&1
        openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial -out leaf.pem -days 1 -extfile leaf.cnf > /dev/null 2>&1
        /bin/rm -f ca.key ca.srl leaf.csr ca.cnf leaf.cnf
        openssl x509 -in ca.pem -noout -fingerprint -sha1' _ "$S/pki" "$HOSTN" "$RUN-$ATT" | tail -1 | sed 's/^.*=//; s/://g')
    [ -n "$CA_SHA1" ] && as_subject test -s "$S/pki/ca.pem" && as_subject test -s "$S/pki/leaf.key" || fail 'cannot create the per-run CA and leaf'
    phase tls-hosts
    on_cleanup cl_hosts
    priv hosts-add || fail 'cannot map the per-run hostname'
    [ "$(HX resolve "$HOSTN")" = 127.0.0.1 ] || fail 'the per-run hostname does not resolve to loopback only'
    phase tls-front
    on_cleanup cl_front
    ( priv front "$1" ) > "$EV/front.log" 2>&1 &
    FRONT_JOB=$!
    wait_for 20 as_subject test -s "$S/run/front.pid" || { cat "$EV/front.log" >&2; fail 'the TLS front did not start'; }
    [ -z "${2:-}" ] || { nrc=0; public_curl > /dev/null 2>&1 || nrc=$?; negative_trust "$nrc"; }
    phase tls-trust
    on_cleanup cl_trust
    TRUSTED=yes
    priv trust-add > "$EV/trust.log" 2>&1 || fail 'cannot trust the per-run CA on this runner'
    if [ -n "${2:-}" ]; then
        probe_body=$(public_curl 2> /dev/null) && [ "$probe_body" = '{"probe":"ok"}' ] && fact trust_positive accepted || { fact trust_positive refused; fail 'the trusted request failed'; }
    else
        probe_code=$(as_subject /usr/bin/curl --disable -sS -o /dev/null -w '%{http_code}' --proto '=https' --max-time 5 "https://$HOSTN/readyz" 2> /dev/null) && [ "$probe_code" = 502 ] ||
            fail 'trusted TLS did not reach the front (expected its 502 while no tunnel child runs)'
    fi
}

probe_gui() {  # macOS: ONE captured listing of the gui domain; a failed query is unavailable, never a zero-byte listing (its size is a diagnostic: the product reads only the status)
    local size avail=no rc gs=not-run gn=not-run gb=not-run ge=none ds=not-run db=not-run da=not-run dh=none dt=none de=none
    # Both answers are written to files with their own statuses. The table is then admitted by the helper as the product admits it (relay/native-install-transaction.sh): no NUL (a shell substitution would drop
    # it), at most 65536 bytes and the text in the product's print-disabled grammar (blank lines ignored, `disabled services = {` ... `}` holding rows of `"<label>" => <state>` with a state
    # of true, false, enabled or disabled, further sections allowed). A failed, NUL-bearing, over-limit or malformed table is unavailable. The product discards the domain listing and reads only its status, so the listing's
    # NUL and size (89,044 bytes on macos-15) are diagnostics below (`gui_nul`, `gui_bytes`), never a stage. The stages run in this order and a stage that is not reached stays `not-run`, as the earlier `&&` chain stopped at the first refusal.
    # Diagnostics (Q1D): each stage's status, size and shape is recorded further down as evidence for the receipt, whatever the result; only the statuses and helper answers here decide.
    # Each query's stdout goes to its file as before; its STDERR goes through a capture that writes at most 4096 bytes to the evidence file and drains (never writes) the rest, so a runaway stream can neither
    # fill the runner nor kill the query with SIGPIPE; the query's own status is the first PIPESTATUS element. A stderr file that cannot be written is diagnostic-only (the hint is `none`; the query's
    # status and the verdict are untouched); a STDOUT file that cannot be opened is the fail-closed case: the group's redirect fails, the status is 1 and the manager is unavailable, as the old redirect failed.
    rc=1
    {
        launchctl print "gui/$SUID" 2>&1 1>&3 3>&- \
            | { /usr/bin/head -c 4096 > "$EV/gui.err"; /bin/cat > /dev/null; } 3>&-
        rc=${PIPESTATUS[0]}
    } 3> "$EV/gui.txt" || rc=$?
    gs=$rc
    if [ "$gs" -eq 0 ]; then
        rc=0
        OX nul-free --path "$EV/gui.txt" || rc=$?
        case "$rc" in 0) gn=clean ;; 1) gn=nul ;; esac  # evidence only (any other status, unreadable, is recorded as not-run): the product discards the listing, so a NUL in it fails nothing
        rc=1
        {
            launchctl print-disabled "gui/$SUID" 2>&1 1>&3 3>&- \
                | { /usr/bin/head -c 4096 > "$EV/disabled.err"; /bin/cat > /dev/null; } 3>&-
            rc=${PIPESTATUS[0]}
        } 3> "$EV/disabled.txt" || rc=$?
        ds=$rc
        if [ "$ds" -eq 0 ] && OX table-ok --path "$EV/disabled.txt"; then avail=yes; fi
    fi
    # Evidence only: read-only helper answers over the captured files (each read bounded by the helper, each hint reduced to the sanitized class with every path-like token replaced); a helper that fails
    # leaves the stage's `not-run` / `none`. The table's frame is judged on the captured bytes whatever its status was (the status is its own fact); a stage that never ran has no files to read.
    gb=$(OX shape --path "$EV/gui.txt" --field bytes 2> /dev/null || echo not-run)
    ge=$(OX shape --path "$EV/gui.err" --field head 2> /dev/null || echo none)
    if [ "$ds" != not-run ]; then
        db=$(OX shape --path "$EV/disabled.txt" --field bytes 2> /dev/null || echo not-run)
        da=$(OX table-reason --path "$EV/disabled.txt" 2> /dev/null || echo not-run)
        dh=$(OX shape --path "$EV/disabled.txt" --field head 2> /dev/null || echo none)
        dt=$(OX shape --path "$EV/disabled.txt" --field tail 2> /dev/null || echo none)
        de=$(OX shape --path "$EV/disabled.err" --field head 2> /dev/null || echo none)
    fi
    fact gui_print_status "$gs"
    fact gui_nul "$gn"
    fact gui_bytes "$gb"
    fact gui_print_err "$ge"
    fact disabled_status "$ds"
    fact disabled_bytes "$db"
    fact disabled_admission "$da"
    fact disabled_head "$dh"
    fact disabled_tail "$dt"
    fact disabled_err "$de"
    printf 'gui probe: status=%s nul=%s bytes=%s err=[%s]; table: status=%s bytes=%s admission=%s head=[%s] tail=[%s] err=[%s]\n' "$gs" "$gn" "$gb" "$ge" "$ds" "$db" "$da" "$dh" "$dt" "$de"
    if [ "$avail" = yes ]; then
        fact manager_pre available
        size=$(wc -c < "$EV/gui.txt") || fail 'cannot measure the gui domain listing'  # wc's own status, not a pipeline's; padding blanks are dropped below
        size=${size//[[:space:]]/}
        printf 'gui domain listing: %s bytes (diagnostic only: the product reads the domain by status)\n' "$size"
    else
        fact manager_pre unavailable
    fi
}
probe_main() {
    phase manager
    fact curl_backend "$(/usr/bin/curl --version 2> /dev/null | head -1 | tr -cd 'A-Za-z0-9._/ -' | cut -c1-80)"
    if [ "$OS" = Darwin ]; then
        probe_gui
        for k in isolated_uid default_target linger_initial linger_changed; do fact "$k" not-applicable; done
    else
        user_up
    fi
    phase stage
    stage_base
    phase baseline
    baseline_clean || fail "a product definition, release or mapping already exists on this runner (baseline sub-check: ${BASEFAIL:-unknown})"
    tls_up 0 probe
    phase manager-post
    if [ "$OS" = Darwin ]; then
        launchctl print "gui/$SUID" > /dev/null 2>&1 && fact manager_post available || fact manager_post unavailable
    fi
    phase complete
}

stage_base() {
    [ "$OS" != Darwin ] || on_cleanup cl_stage  # registered first, but it removes nothing until the atomic mkdir below proves this run created the stage
    as_subject /bin/sh -c 'umask 077 && mkdir "$1"' _ "$S" && STAGE_OWNED=yes || fail 'cannot create a fresh stage (an existing path, even a symlink, is refused and never removed)'
    as_subject /bin/sh -c 'umask 077 && mkdir -p "$@"' _ "$S/bin" "$S/run" "$S/tmp" "$S/art" "$S/pki" "$S/src" "$S/cf" "$S/fix" "$SHIMDIR" || fail 'cannot create the stage directories'
    stage_put "$HOSTPY" "$S/native-lifecycle-host.py" 600
    # curl lives in SHIMDIR: ~/.local/bin of the fresh test user on Linux (the installer prepends it to PATH), a private directory on macOS
    for t in curl cloudflared; do
        extra=''
        [ "$t" != curl ] || extra="--art $S/art --version $VER --sha $SHA --target $TARGET --host $HOSTN --ports $RELAY_PORT"
        printf '#!/bin/sh\nexec %s -I -B %s/native-lifecycle-host.py %s --log %s/run/hits.log %s -- "$@"\n' "$PY" "$S" "$t" "$S" "$extra" > "$EV/shim.$t"
        stage_put "$EV/shim.$t" "$([ "$t" = curl ] && echo "$SHIMDIR/curl" || echo "$S/bin/$t")" 700
    done
}
state_fact() {  # KEY VALUE: a manager `active` value as a diagnostic fact: only [a-z-] is kept (LC_ALL=C: bytes, never a locale's ranges), at most 24 characters, `unknown` for nothing; `not-run` stays itself
    local LC_ALL=C v
    v=${2//[!a-z-]/}
    v=${v:0:24}
    fact "$1" "${v:-unknown}"
}
baseline_note() {  # FIRST-FAILING-SUB-CHECK|none LABEL-STATE LEGACY-STATE: the three diagnostic facts of ONE baseline_clean outcome (evidence only; the return status of baseline_clean decides)
    BASEFAIL=$1
    fact baseline_failed "$1"
    state_fact baseline_label_state "$2"
    state_fact baseline_legacy_state "$3"
}
baseline_clean() {
    local mpath p la ll i=0 names=(conf release cache definition legacy-definition curlrc shim)  # names, in order, of the seven paths below
    for p in "$CONF" "$REL" "$THOME/.cache/herdr-mobile-relay" "$DEF" "$LDEF" "$THOME/.curlrc" "$SHIMDIR/herdr-mobile-relay"; do
        sgone "$p" || { baseline_note "path-${names[i]}" not-run not-run; return 1; }  # absent only on a definite ENOENT/ENOTDIR (a symlink, even a dangling one, exists); an uninspectable path fails the baseline
        i=$((i + 1))
    done
    # The service inherits its manager's PATH and the wrapper (relay/herdr-mobile-relay-service.sh) appends it to its own list, so read it
    # from the manager: a failed query fails the baseline (an unobserved PATH is not an empty one). systemd's built-in default adds /usr/local/sbin.
    if [ "$OS" = Linux ]; then
        mpath=$(as_subject systemctl --user show-environment) || { echo 'baseline: cannot read the user manager environment' >&2; baseline_note manager-path-unread not-run not-run; return 1; }
        mpath="$(printf '%s\n' "$mpath" | sed -n 's/^PATH=//p'):/usr/local/sbin"
    else
        mpath=$(as_subject launchctl getenv PATH) || { echo 'baseline: cannot read the launchd PATH' >&2; baseline_note manager-path-unread not-run not-run; return 1; }
    fi
    # Rebuild the wrapper's own PATH as the subject (its list, the inherited PATH, then every per-tool bin directory), then require on it:
    # (1) no real `herdr`, so the wrapper exports no Herdr CLI and the socket fixture stays the only Herdr the relay can see; (2) the manager
    # that `command -v` finds is the system one ($MGRPATH), not a runner-home or Homebrew executable of the same name.
    as_subject /bin/sh -c '
        P=/opt/homebrew/bin:/usr/local/bin:/home/linuxbrew/.linuxbrew/bin:$HOME/.local/bin:/usr/bin:/bin:/usr/sbin:/sbin
        [ -z "$2" ] || P="$P:$2"
        for d in "$HOME"/.[!.]*/bin; do [ ! -d "$d" ] || P="$P:$d"; done
        PATH=$P
        [ -z "$(command -v herdr)" ] && [ "$(command -v "${1##*/}")" = "$1" ]' _ "$MGRPATH" "$mpath" || { baseline_note command-provenance not-run not-run; return 1; }
    la=$(snap | jget active)  # the value of the earlier `[ "$(snap | jget active)" = absent ]`: nothing (a failed observation) is not `absent`
    [ "$la" = absent ] || { baseline_note label-active "$la" not-run; return 1; }
    ll=$(snap_of "$LEGACY" "$LDEF" | jget active)
    [ "$ll" = absent ] || { baseline_note legacy-active "$la" "$ll"; return 1; }
    absent herdr-nlt- /etc/hosts || { baseline_note hosts "$la" "$ll"; return 1; }
    baseline_note none "$la" "$ll"
}

snap_of() { HX observe --manager "$MGR" --label "$1" --definition "$2" --release "$REL" "${@:3}"; }
snap() { snap_of "$LABEL" "$DEF"; }
bystander_running() { local d; d=$(snap_of "$BYL" "$BYD" | jget owned_pid_digest) && printf '%s\n' "$d" | grep -Eq '^[0-9a-f]{64}$'; }  # `none` and `unknown` are not "running"
bystander() { snap_of "$BYL" "$BYD" --release /nonexistent --digest "$1" --file "$S/fix/bystander.txt"; }  # the later --release wins: the product's release must not move this digest
counts() { HX counts --log "$S/run/hits.log" --host "$HOSTN" --ident "$IDENT"; }
readyz_local() { as_subject curl -fsS --max-time 3 "http://127.0.0.1:$RELAY_PORT/readyz"; }
local_ident() { local body; body=$(readyz_local 2> /dev/null) && printf '%s' "$body" | OX ident || echo none; }  # the identity of a body only after curl itself succeeded; any failure is `none`, never an empty answer
public_ident() { local body; body=$(as_subject curl -fsS --proto '=https' --max-time 5 "https://$HOSTN/readyz" 2> /dev/null) && printf '%s' "$body" | OX ident || echo none; }
ready_ok() { [ "$(local_ident)" != none ]; }
public_ok() { known_digest "$IDENT" && [ "$(public_ident)" = "$IDENT" ]; }  # two `none` answers are not the same identity
procs_gone() { none pgrep -f 'herdr-mobile-relay serve-install[e]d|native-lifecycle-hos[t].py cloudflared'; }
expect() { [ "$1" = "$2" ] || fail "$3 (expected '$2', saw '$1')"; }
case_begin() { CASE=$1; BEFORE=$(snap); BYP0=$(bystander full); CNT0=$(counts); }
case_end() {  # PHASE PRODUCT-EXIT [MANUAL-START-COUNT] [login]
    local after tally by byp stable=true
    after=$(snap)  # one statement each: a failed observation aborts here instead of leaving a masked, empty value
    tally=$(counts)
    by=$(bystander stable)
    byp=$(bystander full)
    [ "$by" = "$BY_BASE" ] && [ "$by" != unknown ] || stable=false  # the helper prints `unknown` when any bystander field was unobserved: two unknowns are not "unchanged"
    [ "${4:-}" = login ] || { [ "$byp" = "$BYP0" ] && [ "$byp" != unknown ]; } || stable=false
    mx case-add --file "$CASES" --os "$OS" --sha "$SHA" --id "$CASE" --phase "$1" --exit "$2" --before "$BEFORE" --after "$after" --readiness "$(local_ident)" \
        --counts "$tally" --base "$CNT0" --manual-start "${3:-0}" --target "$TSTATE" --bystander "$by" --stable "$stable" --mid "${MID:-n/a}" || fail 'the case evidence was refused'
    CASE='' TSTATE=n/a MID=''
}
write_env() {  # TRANSPORT [extra literal assignments]
    local t=$1
    shift
    { printf 'HERDR_RELAY_TRANSPORT=%s\nHERDR_RELAY_PORT=%s\n' "$t" "$RELAY_PORT"
      printf "HERDR_SOCKET_PATH='%s'\nCLOUDFLARED_BIN='%s'\nCLOUDFLARED_CONFIG='%s'\n" "$S/run/h.sock" "$S/bin/cloudflared" "$S/cf/config.yml"
      for line in "$@"; do printf '%s\n' "$line"; done; } > "$EV/relay.env"
    stage_put "$EV/relay.env" "$CONF/relay.env" 600
}
# bytes, modes and link targets of everything a refusal could touch: the product's roots, the foreign fragment's directory (case_foreign_systemd plants it there) and the cache
treeh() { HX tree --paths "$CONF,$REL,$S/cf,$S/fix,$S/bin,$THOME/.local/bin,$(dirname "$DEF"),$THOME/.local/share/systemd/user,$THOME/.cache/herdr-mobile-relay"; }
expect_refusal() {  # MESSAGE command...: the product must exit nonzero, say MESSAGE, and leave every byte, mode and link under treeh untouched
    local msg=$1 before out after
    shift
    before=$(treeh) && [ -n "$before" ] || fail 'cannot digest the paths a refusal must not change'  # a failed digest prints nothing: two empty digests are not "unchanged"
    RC=0
    out=$(P 120 "$@" 2>&1) || RC=$?
    case "$RC" in 0 | 124 | 125 | 137) fail "the product accepted what it must refuse, or timed out (status $RC)" ;; esac
    [ -z "$msg" ] || case "$out" in *"$msg"*) ;; *) printf '%s\n' "$out" | tail -4 >&2; fail "the refusal did not say: $msg" ;; esac
    after=$(treeh) && [ -n "$after" ] || fail 'cannot digest the paths a refusal must not change'
    expect "$after" "$before" 'a file, mode or link under the config, release, tunnel, definition or shim paths changed'
}
install_release() { PRODUCT=yes; P 240 /bin/sh "$S/src/install.sh" "$VER" || fail 'install.sh failed'; }
service() { P 300 "$SVC" "$@"; }

cl_fixtures() {
    local o
    HX stop --run "$S/run" > /dev/null 2>&1 || true
    [ -n "$BYL" ] || return 0
    o=$(snap_of "$BYL" "$BYD" | jget origin)
    case "$o" in
        "$BYD") if [ "$OS" = Darwin ]; then as_subject "$MGRPATH" bootout "gui/$SUID/$BYL"; else as_subject "$MGRPATH" --user disable --now "$BYL"; fi || return 1 ;;
        absent) ;;
        *) return 1 ;;  # an unknown or foreign identity: the definition is retained and the cleanup fails
    esac
    [ "$(snap_of "$BYL" "$BYD" | jget owned_pid_digest)" = none ] && as_subject /bin/rm -f "$BYD" && { [ "$OS" = Darwin ] || as_subject "$MGRPATH" --user daemon-reload; }
}
cl_product() {  # a failure mid-lifecycle: use the product's own uninstall, and only on a KNOWN owned identity; success needs every owned path definitely gone
    local a o
    [ -n "$PRODUCT" ] || return 0
    as_subject /bin/rm -f "$SHIMDIR/herdr" "${STUB:-$SHIMDIR/herdr}"
    a=$(snap | jget active) o=$(snap | jget origin)
    if [ "$a" = absent ] && residue_gone; then return 0; fi  # nothing is loaded and no release, config, cache, link or definition remains (an unanswered probe is not "gone")
    [ "$a" = absent ] || [ "$o" = "$DEF" ] || return 1
    as_subject /bin/sh -c '[ -x "$1" ]' _ "$REL/current/relay/uninstall.sh" || return 1
    printf 'y\n' | RUNB 300 "$REL/current/relay/uninstall.sh" > /dev/null && [ "$(snap | jget active)" = absent ] && residue_gone
}
cl_foreign() {  # removes only the planted fixture: its origin and definition must still equal the record taken after planting and nothing may run; any other identity is retained
    local f v now
    [ -n "$FOREIGN_DEF" ] || return 0
    now=$(snap)
    if [ "$(echo "$now" | jget origin)" != absent ]; then
        for f in origin definition_hash; do
            v=$(echo "$now" | jget "$f")
            [ "$v" != unknown ] && [ "$v" = "$(echo "$BEFORE" | jget "$f")" ] || return 1  # two identical `unknown` observations are not an identity
        done
        case "$(echo "$now" | jget active)" in inactive | loaded) ;; *) return 1 ;; esac  # the planted unit must be provably not running (systemd inactive, launchd loaded)
        [ "$(echo "$now" | jget owned_pid_digest)" = none ] || return 1
        [ "$OS" != Darwin ] || as_subject "$MGRPATH" bootout "gui/$SUID/$LABEL" || return 1
    fi
    as_subject /bin/sh -c '/bin/rm -rf "$@"' _ "$FOREIGN_DEF" "$FOREIGN_DEF.d" && { [ "$OS" = Darwin ] || as_subject "$MGRPATH" --user daemon-reload; } && [ "$(snap | jget active)" = absent ]
}
bystander_up() {
    BYL="herdr-nlt-bystander-$RUN.service" BYD="$THOME/.config/systemd/user/$BYL" FOREIGN_DEF=''
    if [ "$OS" = Darwin ]; then
        BYL="com.herdr-nlt-bystander.$RUN" BYD="$HOME/Library/LaunchAgents/$BYL.plist"
        printf '<?xml version="1.0" encoding="UTF-8"?>\n<plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>/bin/sleep</string><string>86400</string></array><key>RunAtLoad</key><true/></dict></plist>\n' "$BYL" > "$EV/by.def"
    else
        printf '[Service]\nExecStart=/bin/sleep 86400\n[Install]\nWantedBy=default.target\n' > "$EV/by.def"
    fi
    on_cleanup cl_fixtures
    as_subject /bin/sh -c 'mkdir -p "$(dirname "$1")"' _ "$BYD"
    stage_put "$EV/by.def" "$BYD" 600
    printf 'bystander %s\n' "$RUN" > "$EV/by.txt"
    stage_put "$EV/by.txt" "$S/fix/bystander.txt" 600
    if [ "$OS" = Darwin ]; then
        as_subject launchctl bootstrap "gui/$SUID" "$BYD" || fail 'cannot load the bystander job'
    else
        as_subject systemctl --user daemon-reload && as_subject systemctl --user enable --now "$BYL" || fail 'cannot start the bystander unit'
    fi
    wait_for 20 bystander_running || fail 'the bystander is not running'
    BY_BASE=$(bystander stable)
    case "$BY_BASE" in '' | unknown) fail 'the bystander state cannot be observed' ;; esac
}
fixtures_up() {
    as_subject /bin/rm -f "$S/run/stop" "$S/run/socket.pid"
    ( ASTO=86400 HX herdr-socket --path "$S/run/h.sock" --log "$S/run/hits.log" --run "$S/run" ) > "$EV/socket.log" 2>&1 &
    wait_for 20 as_subject test -S "$S/run/h.sock" || { cat "$EV/socket.log" >&2; fail 'the Herdr socket fixture did not start'; }
}
manager_start() { if [ "$OS" = Darwin ]; then as_subject launchctl bootstrap "gui/$SUID" "$DEF"; else as_subject systemctl --user start "$LABEL"; fi; }
manager_restart() { if [ "$OS" = Darwin ]; then as_subject launchctl kickstart -k "gui/$SUID/$LABEL"; else as_subject systemctl --user restart "$LABEL"; fi; }
known_digest() { printf '%s' "$1" | grep -Eq '^[0-9a-f]{64}$'; }  # a real process digest: neither `none` nor `unknown`
default_target_lists() {  # the query itself must succeed (its status is checked, not grep's), and the unit must be an EXACT entry of the plain listing, not a substring of another line
    local deps
    deps=$(as_subject systemctl --user list-dependencies --plain default.target) || fail 'cannot list the dependencies of default.target'
    printf '%s\n' "$deps" | sed 's/^[[:space:]]*//' | grep -qxF "$LABEL"
}
manager_stopped() { [ "$(snap | jget owned_pid_digest)" = none ] && procs_gone; }
owned_origin() { [ "$(snap | jget origin)" = "$DEF" ]; }
stage_release() {
    mx bundle --sha "$SHA" --run-id "$RUN" --run-attempt "$ATT" --producer "$PRODJ" --bundle "$BUNDLE" --version "$VER" || fail 'the downloaded bundle differs from the producer receipt'
    stage_put "$BUNDLE/checksums.txt" "$S/art/checksums.txt" 600
    stage_put "$BUNDLE/herdr-mobile-relay_${VER}_$TARGET.tar.gz" "$S/art/herdr-mobile-relay_${VER}_$TARGET.tar.gz" 600
    COPYFILE_DISABLE=1 tar -c -C "$HERE" install.sh relay | as_subject tar -x -C "$S/src"
    printf 'tunnel: herdr-nlt\ningress:\n  - hostname: %s\n    service: http://127.0.0.1:%s\nfake-listen: %s\n' "$HOSTN" "$RELAY_PORT" "$CHILD_PORT" > "$EV/cf.yml"
    stage_put "$EV/cf.yml" "$S/cf/config.yml" 600
}

case_install_start() {
    case_begin a-install-start
    install_release
    expect "$(as_subject sed -n 's/^[[:space:]]*"revision":[[:space:]]*"\([^"]*\)".*/\1/p' "$REL/current/release-manifest.json" | head -1)" "$SHA" 'installed revision'
    expect "$(as_subject sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)".*/\1/p' "$REL/current/release-manifest.json" | head -1)" "$VER" 'installed version'
    expect "$(sha_file "$REL/current/release-manifest.json")" "$("$PYO" -I -B -c 'import json,sys; print(json.load(open(sys.argv[1]))["a"]["targets"][sys.argv[2]]["manifest_sha256"])' "$PRODJ" "$TARGET")" 'installed manifest digest'
    case "$(as_subject readlink "$REL/current")" in *"${VER}-${SHA}-"*) ;; *) fail 'the current link does not name the installed release revision' ;; esac
    write_env cloudflare
    phase install
    service install || fail 'service.sh install failed'
    wait_for 60 ready_ok || fail 'the installed relay did not become ready'
    IDENT=$(local_ident)
    wait_for 30 public_ok || fail 'the public HTTPS identity does not match the installed relay'
    owned_origin || fail 'the loaded service is not defined by the owned definition path'
    expect "$(snap | jget enabled)" true 'enabled'
    [ "$(snap | jget owned_pid_digest)" != none ] || fail 'no owned process runs'
    case_end install-start 0
}
case_stop_start_restart() {
    local pid1 pid2
    case_begin a-stop-start-restart
    service stop || fail 'service.sh stop failed'
    wait_for 30 manager_stopped || fail 'the relay and its tunnel child did not stop'
    expect "$(snap | jget definition_hash)" "$(echo "$BEFORE" | jget definition_hash)" 'the definition changed on stop'
    ! readyz_local > /dev/null 2>&1 || fail 'the relay still answers after stop'
    ! public_curl > /dev/null 2>&1 || fail 'public HTTPS still answers with no tunnel child'
    manager_start || fail 'manual start failed'
    wait_for 60 ready_ok && wait_for 30 public_ok || fail 'not ready after manual start'
    pid1=$(snap | jget owned_pid_digest)
    known_digest "$pid1" || fail 'the manual start left no known process digest to compare the restart against'
    manager_restart || fail 'restart failed'
    wait_for 60 ready_ok && wait_for 30 public_ok || fail 'not ready after restart'
    pid2=$(snap | jget owned_pid_digest)
    known_digest "$pid2" && [ "$pid2" != "$pid1" ] || fail 'restart did not relaunch the owned process (or its process digest is unknown)'
    MID=$pid1  # recorded in the case: the gate re-checks it against the final digest
    expect "$(local_ident)" "$IDENT" 'readiness identity after restart'
    case_end stop-start-restart 0
}
case_login_relaunch() {
    local old
    case_begin login-style-relaunch
    owned_origin || fail 'unknown identity: refusing to relaunch'
    old=$(snap | jget manager_generation)
    if [ "$OS" = Darwin ]; then
        as_subject launchctl bootout "gui/$SUID/$LABEL" || fail 'bootout failed'
        wait_for 30 manager_stopped || fail 'the owned label did not stop'
        as_subject launchctl bootstrap "gui/$SUID" "$DEF" || fail 'bootstrap failed'
    else
        priv terminate > /dev/null 2>&1 || true
        wait_for 20 mgr_down || priv mgr-stop || fail 'the old user manager did not stop'
        wait_for 20 mgr_down || fail 'the old user manager generation is still active'
        none pgrep -u "$SUID" -f 'systemd --user|herdr-mobile-relay serve|sleep 86400' || fail 'old-generation processes remain, or cannot be inspected'
        priv mgr-start || fail 'cannot start the test user manager'
        wait_for 60 user_manager_ok || fail 'the fresh user manager is unavailable'
        wait_for 90 as_subject systemctl --user is-active --quiet "$LABEL" || fail 'default.target did not activate the installed unit by itself'
        TSTATE=$(as_subject systemctl --user is-active default.target 2> /dev/null) || fail 'default.target is not active on the fresh manager'  # a failed query is never "active"; recorded by case_end only if the case passes
        [ "$TSTATE" = active ] && default_target_lists || fail 'default.target is not active, or does not list the installed unit'
    fi
    wait_for 60 ready_ok && wait_for 30 public_ok || fail 'not ready after the login-style relaunch'
    owned_origin || fail 'the relaunched unit has the wrong origin'
    [ "$(snap | jget manager_generation)" != "$old" ] || [ "$OS" = Darwin ] || fail 'the user manager generation did not change'
    case_end login-relaunch 0 0 login
}
case_full_uninstall() {
    case_begin a-full-uninstall
    printf '#!/bin/sh\nexec %s -I -B %s/native-lifecycle-host.py herdr --log %s/run/hits.log -- "$@"\n' "$PY" "$S" "$S" > "$EV/herdr.shim"
    stage_put "$EV/herdr.shim" "$SHIMDIR/herdr" 700
    RC=0
    printf 'y\n' | RUNB 300 "$REL/current/relay/uninstall.sh" || RC=$?
    as_subject /bin/rm -f "$SHIMDIR/herdr"
    expect "$RC" 0 'full uninstall exit'
    expect "$(snap | jget active)" absent 'owned label after uninstall'
    expect "$(snap_of "$LEGACY" "$LDEF" | jget active)" absent 'legacy label after uninstall'
    residue_gone || fail 'full uninstall left a release, config, cache, link or definition, or it cannot be inspected'
    procs_gone || fail 'service children remain after uninstall'
    case_end full-uninstall "$RC"
}

case_foreign_launchd() {
    case_begin foreign-launchd-path
    FOREIGN_DEF="$S/fix/$LABEL.plist"
    printf '<?xml version="1.0" encoding="UTF-8"?>\n<plist version="1.0"><dict><key>Label</key><string>%s</string><key>ProgramArguments</key><array><string>/bin/sleep</string><string>86400</string></array></dict></plist>\n' "$LABEL" > "$EV/foreign.def"
    stage_put "$EV/foreign.def" "$FOREIGN_DEF" 600
    as_subject launchctl enable "gui/$SUID/$LABEL"  # an earlier uninstall leaves the label disabled, which would refuse the foreign bootstrap itself
    as_subject launchctl bootstrap "gui/$SUID" "$FOREIGN_DEF" || fail 'cannot load the foreign same-label job'
    BEFORE=$(snap)
    expect "$(echo "$BEFORE" | jget origin)" "$FOREIGN_DEF" 'the foreign job origin'
    write_env cloudflare
    expect_refusal "not established to be defined by Herdr's own definition path" "$SVC" install
    expect "$(snap | jget origin)" "$FOREIGN_DEF" 'the foreign job was touched'
    case_end refuse-foreign-path "$RC"
    cl_foreign || fail 'cannot remove the foreign same-label job'
    FOREIGN_DEF=''
}
case_foreign_systemd() {  # ID: foreign-systemd-fragment (a same-label unit from another search directory) or foreign-systemd-dropin
    case_begin "$1"
    if [ "$1" = foreign-systemd-fragment ]; then
        FOREIGN_DEF="$THOME/.local/share/systemd/user/$LABEL"
    else
        FOREIGN_DEF="$DEF"
        as_subject /bin/sh -c 'mkdir -p "$1.d" && printf "[Service]\nEnvironment=NLT_FOREIGN=1\n" > "$1.d/10-foreign.conf"' _ "$DEF"
    fi
    printf '[Unit]\nDescription=foreign\n[Service]\nExecStart=/bin/sleep 86400\n[Install]\nWantedBy=default.target\n' > "$EV/foreign.def"
    as_subject /bin/sh -c 'mkdir -p "$(dirname "$1")"' _ "$FOREIGN_DEF"
    stage_put "$EV/foreign.def" "$FOREIGN_DEF" 600
    as_subject systemctl --user daemon-reload || fail 'daemon-reload failed'
    BEFORE=$(snap)
    case "$1:$(echo "$BEFORE" | jget origin)" in
        foreign-systemd-fragment:"$FOREIGN_DEF" | foreign-systemd-dropin:"$DEF|dropins=$DEF.d/10-foreign.conf") ;;
        *) fail 'the foreign systemd origin is not the planted one' ;;
    esac
    write_env cloudflare
    expect_refusal "not established to be defined by Herdr's own definition path" "$SVC" install
    expect "$(snap)" "$BEFORE" 'the foreign unit state changed'
    case_end refuse-foreign-unit "$RC"
    cl_foreign || fail 'cannot remove the foreign unit'
    FOREIGN_DEF=''
}
refusal_cases() {
    case_begin bad-token
    write_env cloudflare 'HERDR_RELAY_TOKEN=tooshort'
    expect_refusal 'Installed policy requires a 32-byte key.' "$SVC" install
    case_end invalid-token "$RC"
    case_begin bad-policy
    write_env cloudflare 'HERDR_RELAY_RUN_ID=foreground'
    expect_refusal 'Installed policy refuses foreground identities.' "$SVC" install
    write_env cloudflare 'HERDR_RELAY_REARM_BOOTSTRAP=1'
    expect_refusal 'Installed policy refuses rearm.' "$SVC" install
    case_end foreground-and-rearm-policy "$RC"
    case_begin public-wizard-premutation
    write_env cloudflare 'HERDR_RELAY_REARM_BOOTSTRAP=1'
    expect_refusal 'requires an unambiguous, background-compatible literal configuration' "$S/src/relay/plugin-install-service.sh"
    case_end wizard-before-any-effect "$RC"
    case_begin gateway-missing-app
    write_env gateway "HERDR_GATEWAY_URL='wss://gateway.nlt.test'"
    expect_refusal 'Select an installed HTTPS Herdr phone app first' "$SVC" install
    case_end gateway-without-app "$RC"
    case_begin manager-missing-injected
    write_env cloudflare
    STUB="$SHIMDIR/$([ "$OS" = Darwin ] && echo launchctl || echo systemctl)"  # first on the product's own PATH on both OSes
    printf '#!/bin/sh\necho invoked >> %s/run/nomgr.log\nexit 127\n' "$S" > "$EV/nomgr"
    stage_put "$EV/nomgr" "$STUB" 700
    expect_refusal '' "$SVC" install
    as_subject /bin/rm -f "$STUB"
    STUB=''
    as_subject test -s "$S/run/nomgr.log" || fail 'the injected manager stub was never consulted (simulated, not the real probe)'
    case_end simulated-missing-manager "$RC"
    if [ "$OS" = Darwin ]; then case_foreign_launchd; else case_foreign_systemd foreign-systemd-fragment; case_foreign_systemd foreign-systemd-dropin; fi
}
teardown_refusal_base() {  # no Herdr adapter is installed here: the product notes that plugin registration is unavailable and finishes
    RC=0
    printf 'y\n' | RUNB 300 "$REL/current/relay/uninstall.sh" || RC=$?
    expect "$RC" 0 'baseline teardown'
    expect "$(snap | jget active)" absent 'baseline label after teardown'
    residue_gone || fail 'the baseline teardown left a release, config, cache, link or definition, or it cannot be inspected'
}

lifecycle_main() {
    phase setup
    [ "$OS" = Darwin ] || user_up
    stage_base
    baseline_clean || fail "a product definition, release or mapping already exists on this runner (baseline sub-check: ${BASEFAIL:-unknown})"
    RB0=$(OX tree --paths "$HOME/.local/bin") && [ -n "$RB0" ] || fail 'cannot digest the Actions user ~/.local/bin'  # its own ~/.local/bin: nothing here may touch it
    stage_release
    tls_up "$CHILD_PORT"
    phase fixtures
    fixtures_up
    bystander_up
    on_cleanup cl_product
    on_cleanup cl_foreign
    case_install_start
    case_stop_start_restart
    case_login_relaunch
    case_full_uninstall
    phase refusals
    install_release
    refusal_cases
    teardown_refusal_base
    tally=$(counts) || fail 'cannot read the hits log'
    [ "$(echo "$tally" | jget lines)" -gt 0 ] || fail 'the hits log is missing or empty after a whole lifecycle'  # an unread log is not "nothing was refused"
    expect "$(echo "$tally" | jget curl_refused)$(echo "$tally" | jget adapter_refused)" 00 'the strict curl shim or the Herdr adapter refused a product call'
    RB1=$(OX tree --paths "$HOME/.local/bin") && [ -n "$RB1" ] || fail 'cannot digest the Actions user ~/.local/bin'
    expect "$RB1" "$RB0" "the Actions user's ~/.local/bin changed"
    case_begin bystander-nonmutation
    expect "$(bystander stable)" "$BY_BASE" 'the bystander changed'
    case_end bystander-continuity 0
    phase complete
}

producer_main() {
    phase build
    command -v go > /dev/null 2>&1 && command -v bun > /dev/null 2>&1 || fail 'go and bun are required on the producer'
    make cross-build
    phase package
    scripts/package-release.sh "$VER" "$SHA" dist/release
    phase complete
}

"${mode}_main"
