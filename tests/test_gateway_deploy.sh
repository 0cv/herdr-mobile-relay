#!/bin/bash
# Covers relay/gateway-deploy.sh: the bundle it writes and the SSH deployment it
# drives. Every remote step runs against stub ssh/curl binaries, so the test
# never touches a network or a real server.
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-gateway-deploy-test.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

STUB_DIR="$WORK_DIR/bin"
mkdir -p "$STUB_DIR"

SSH_LOG="$WORK_DIR/ssh.log"
export SSH_LOG
cat > "$STUB_DIR/ssh" <<'EOF'
#!/bin/bash
# Records the remote command and answers the handful of probes the deployment
# makes. The tar step must drain stdin or the local tar reports a broken pipe.
set -uo pipefail
target=""
command=""
skip_value=false
for argument in "$@"; do
    if [ "$skip_value" = true ]; then
        skip_value=false
        printf '%s\n' "$argument" > "$STUB_STATE/identity"
        continue
    fi
    case "$argument" in
        -i) skip_value=true ;;
        -o | -O) continue ;;
        -*) continue ;;
        ControlMaster=* | ControlPath=* | ControlPersist=* | ConnectTimeout=* | exit) continue ;;
        *)
            if [ -z "$target" ]; then
                target="$argument"
            else
                command="$argument"
            fi
            ;;
    esac
done
[ -n "$command" ] || exit 0
printf '%s\t%s\n' "$target" "$command" >> "$SSH_LOG"
case "$command" in
    'id -u') printf '1000\n'; exit 0 ;;
    *'docker compose version'*)
        if [ -f "$STUB_STATE/docker-missing" ]; then
            exit 127
        fi
        exit 0
        ;;
    *get.docker.com*)
        rm -f "$STUB_STATE/docker-missing"
        exit 0
        ;;
    *'tar -xzf -'*)
        cat > "$STUB_STATE/uploaded.tar.gz"
        exit 0
        ;;
    *'docker compose up -d --build'*)
        : > "$STUB_STATE/started"
        exit 0
        ;;
    *healthz*)
        [ -f "$STUB_STATE/started" ] || exit 7
        exit 0
        ;;
esac
exit 0
EOF
chmod 700 "$STUB_DIR/ssh"

STUB_STATE="$WORK_DIR/state"
mkdir -p "$STUB_STATE"
export STUB_STATE

cat > "$STUB_DIR/curl" <<'EOF'
#!/bin/sh
# The public /healthz probe. Answers only once the stub server is "started", so
# the script cannot record a gateway URL it never reached.
for argument in "$@"; do
    case "$argument" in
        *"/healthz") [ -f "$STUB_STATE/started" ] || exit 7 ;;
    esac
done
printf '{"ok":true,"relays":0,"clients":0,"uptime_seconds":1}\n'
EOF
chmod 700 "$STUB_DIR/curl"

PATH="$STUB_DIR:$PATH"
export PATH

NORMALIZE_BIN="$STUB_DIR/herdr-mobile-relay"
cat > "$NORMALIZE_BIN" <<'EOF'
#!/bin/sh
# Isolates this shell test from any relay binary in the developer's data dir.
test "$1" = "normalize-origin" || exit 2
test "$2" = "--allow-loopback-http" || exit 2
test "$3" = "gw.example.test" || exit 2
printf 'https://gw.example.test\n'
EOF
chmod 700 "$NORMALIZE_BIN"
export HERDR_RELAY_BIN="$NORMALIZE_BIN"

HOSTNAME_UNDER_TEST="gw.example.test"
export HERDR_GATEWAY_DEPLOY_HOST="$HOSTNAME_UNDER_TEST"
export HERDR_GATEWAY_DEPLOY_EMAIL="ops@example.test"

run_deploy() {
    OUTPUT="$WORK_DIR/output.log"
    set +e
    (cd "$WORK_DIR" && bash "$REPO_DIR/relay/gateway-deploy.sh" </dev/null) \
        > "$OUTPUT" 2>&1
    STATUS=$?
    set -e
}

fail() {
    echo "FAIL: $1" >&2
    sed -n '1,120p' "$OUTPUT" >&2
    exit 1
}

# --- Bundle only: no server given, nothing deployed ---------------------------
BUNDLE_DIR="$WORK_DIR/bundle-only"
export HERDR_GATEWAY_DEPLOY_DIR="$BUNDLE_DIR"
export HERDR_RELAY_ENV="$WORK_DIR/relay-bundle-only.env"
: > "$HERDR_RELAY_ENV"
run_deploy
[ "$STATUS" -eq 0 ] || fail "bundle-only run exited $STATUS"

for FILE in docker-compose.yml Caddyfile .env README.md; do
    [ -f "$BUNDLE_DIR/$FILE" ] || fail "bundle is missing $FILE"
done
for SOURCE in Dockerfile.gateway go.mod go.sum \
    cmd/herdr-gateway/main.go internal/gateway/server.go \
    internal/gatewaywire/gatewaywire.go; do
    [ -f "$BUNDLE_DIR/gateway-source/$SOURCE" ] ||
        fail "bundle is missing gateway-source/$SOURCE"
done

# The server builds from the bundled source: no GitHub context, no Go toolchain.
if grep -Fq 'github.com/0cv/herdr-mobile-relay.git' "$BUNDLE_DIR/docker-compose.yml"; then
    fail "compose file still builds from a GitHub context"
fi
grep -Fq 'context: ${HERDR_GATEWAY_BUILD_CONTEXT:-./gateway-source}' \
    "$BUNDLE_DIR/docker-compose.yml" || fail "compose file lost the bundled build context"
grep -Fq 'HERDR_GATEWAY_BUILD_CONTEXT="./gateway-source"' "$BUNDLE_DIR/.env" ||
    fail ".env lost the bundled build context"
grep -Fq "email ops@example.test" "$BUNDLE_DIR/Caddyfile" || fail "Caddyfile lost the ACME contact"

# Address discovery is published directly on the host: Caddy cannot carry raw
# UDP, so a bundle that loses this port loses off-LAN direct connections.
grep -Fq '"3478:3478/udp"' "$BUNDLE_DIR/docker-compose.yml" ||
    fail "compose file does not publish the address discovery port"
grep -Fq 'HERDR_GATEWAY_STUN_ADDR' "$BUNDLE_DIR/.env" ||
    fail ".env lost the address discovery knob"

# A shared instance needs whole-gateway ceilings; every other limit is per-relay
# or per-IP and cannot bound total memory.
for KNOB in HERDR_GATEWAY_MAX_RELAYS HERDR_GATEWAY_MAX_CLIENTS; do
    grep -Fq "$KNOB" "$BUNDLE_DIR/.env" || fail ".env lost the $KNOB ceiling"
done
grep -Fq 'UDP 3478' "$BUNDLE_DIR/README.md" ||
    fail "bundle README does not mention UDP 3478"

# Nothing may be recorded until a gateway actually answers.
if grep -q '^HERDR_GATEWAY_URL=' "$HERDR_RELAY_ENV"; then
    fail "bundle-only run recorded a gateway URL"
fi
[ ! -s "$SSH_LOG" ] || fail "bundle-only run contacted a server"

# --- Deploy over SSH ----------------------------------------------------------
: > "$SSH_LOG"
rm -f "$STUB_STATE/started" "$STUB_STATE/uploaded.tar.gz"
BUNDLE_DIR="$WORK_DIR/bundle-deploy"
export HERDR_GATEWAY_DEPLOY_DIR="$BUNDLE_DIR"
export HERDR_GATEWAY_DEPLOY_SERVER="deploy@gw.example.test"
export HERDR_GATEWAY_DEPLOY_REMOTE_DIR="/opt/herdr-gateway"
export HERDR_RELAY_ENV="$WORK_DIR/relay-deploy.env"
: > "$HERDR_RELAY_ENV"
run_deploy
[ "$STATUS" -eq 0 ] || fail "deploy run exited $STATUS"

grep -Fq "deploy@gw.example.test	true" "$SSH_LOG" || fail "deployment never opened an SSH session"
grep -Fq 'mkdir -p /opt/herdr-gateway && tar -xzf - -C /opt/herdr-gateway' "$SSH_LOG" ||
    fail "deployment never copied the bundle"
grep -Fq 'cd /opt/herdr-gateway && docker compose up -d --build' "$SSH_LOG" ||
    fail "deployment never started the stack"
[ -s "$STUB_STATE/uploaded.tar.gz" ] || fail "no bundle archive reached the server"
# Listed to a file, not piped into grep: `grep -q` exits at the first match and
# `tar` then dies of SIGPIPE, which `pipefail` reports as a failed pipeline even
# though the entry was found. That race made this assertion flake.
tar -tzf "$STUB_STATE/uploaded.tar.gz" > "$WORK_DIR/uploaded.list"
grep -Fq './gateway-source/Dockerfile.gateway' "$WORK_DIR/uploaded.list" ||
    fail "uploaded archive carries no gateway source"

# A non-root address must be verified for passwordless sudo, and every remote
# command has to carry that prefix.
grep -Fq 'sudo -n true' "$SSH_LOG" || fail "deployment never checked passwordless sudo"
grep -Fq 'sudo -n sh -c ' "$SSH_LOG" || fail "remote commands dropped the sudo prefix"

# Only a verified public /healthz may switch this relay onto the gateway.
grep -Fq "HERDR_GATEWAY_URL='wss://gw.example.test'" "$HERDR_RELAY_ENV" ||
    fail "verified deployment did not record the gateway URL"

# --- Public endpoint never answers: keep the transport unchanged --------------
: > "$SSH_LOG"
rm -f "$STUB_STATE/started" "$STUB_STATE/uploaded.tar.gz"
cat > "$STUB_DIR/curl" <<'EOF'
#!/bin/sh
exit 7
EOF
chmod 700 "$STUB_DIR/curl"
BUNDLE_DIR="$WORK_DIR/bundle-unreachable"
export HERDR_GATEWAY_DEPLOY_DIR="$BUNDLE_DIR"
export HERDR_RELAY_ENV="$WORK_DIR/relay-unreachable.env"
: > "$HERDR_RELAY_ENV"
export HERDR_GATEWAY_DEPLOY_PUBLIC_TIMEOUT=0
run_deploy
unset HERDR_GATEWAY_DEPLOY_PUBLIC_TIMEOUT
[ "$STATUS" -eq 0 ] || fail "unreachable-public run exited $STATUS"
if grep -q '^HERDR_GATEWAY_URL=' "$HERDR_RELAY_ENV"; then
    fail "unverified deployment recorded a gateway URL"
fi
grep -Fq 'did not' "$OUTPUT" || fail "unverified deployment printed no diagnosis"

# --- Rerun with nothing configured reuses the remembered answers -------------
# Redeploying after a source change must not mean re-typing a hostname, an SSH
# address, and two paths, so a bare rerun repeats the last deployment.
: > "$SSH_LOG"
rm -f "$STUB_STATE/started" "$STUB_STATE/uploaded.tar.gz"
cat > "$STUB_DIR/curl" <<'EOF'
#!/bin/sh
for argument in "$@"; do
    case "$argument" in
        *"/healthz") [ -f "$STUB_STATE/started" ] || exit 7 ;;
    esac
done
printf '{"ok":true,"relays":0,"clients":0,"uptime_seconds":1,"stun_port":3478}\n'
EOF
chmod 700 "$STUB_DIR/curl"
export HERDR_RELAY_ENV="$WORK_DIR/relay-remembered.env"
: > "$HERDR_RELAY_ENV"

REMEMBERED_BUNDLE="$WORK_DIR/bundle-remembered"
export HERDR_GATEWAY_DEPLOY_DIR="$REMEMBERED_BUNDLE"
export HERDR_GATEWAY_DEPLOY_SERVER="deploy@gw.example.test"
export HERDR_GATEWAY_DEPLOY_REMOTE_DIR="/srv/herdr-gateway"
run_deploy
[ "$STATUS" -eq 0 ] || fail "seeding run exited $STATUS"

STATE_FILE="$WORK_DIR/gateway-deploy"
[ -f "$STATE_FILE" ] || fail "answers were not remembered beside the relay environment"
for EXPECTED in \
    "HERDR_GATEWAY_DEPLOY_HOST='gw.example.test'" \
    "HERDR_GATEWAY_DEPLOY_SERVER='deploy@gw.example.test'" \
    "HERDR_GATEWAY_DEPLOY_REMOTE_DIR='/srv/herdr-gateway'" \
    "HERDR_GATEWAY_DEPLOY_EMAIL='ops@example.test'" \
    "HERDR_GATEWAY_DEPLOY_DIR='$REMEMBERED_BUNDLE'"; do
    grep -Fq "$EXPECTED" "$STATE_FILE" || fail "remembered answers lack $EXPECTED"
done

# The bare rerun: every override unset, so only the remembered answers can drive
# it. It must reach the same server and the same directory.
: > "$SSH_LOG"
rm -f "$STUB_STATE/started" "$STUB_STATE/uploaded.tar.gz"
unset HERDR_GATEWAY_DEPLOY_HOST HERDR_GATEWAY_DEPLOY_EMAIL \
    HERDR_GATEWAY_DEPLOY_DIR HERDR_GATEWAY_DEPLOY_SERVER \
    HERDR_GATEWAY_DEPLOY_REMOTE_DIR
run_deploy
[ "$STATUS" -eq 0 ] || fail "remembered rerun exited $STATUS"
grep -Fq 'Reusing the remembered hostname gw.example.test' "$OUTPUT" ||
    fail "rerun did not reuse the remembered hostname"
grep -Fq 'Reusing the remembered server deploy@gw.example.test' "$OUTPUT" ||
    fail "rerun did not reuse the remembered server"
grep -Fq 'mkdir -p /srv/herdr-gateway && tar -xzf - -C /srv/herdr-gateway' "$SSH_LOG" ||
    fail "rerun did not reuse the remembered directory on the server"
grep -Fq "email ops@example.test" "$REMEMBERED_BUNDLE/Caddyfile" ||
    fail "rerun did not reuse the remembered ACME contact"
grep -Fq "HERDR_GATEWAY_URL='wss://gw.example.test'" "$HERDR_RELAY_ENV" ||
    fail "remembered rerun did not record the verified gateway URL"

# --- A key ssh would never offer on its own ---------------------------------
# Hosts that only accept a key outside ~/.ssh/id_* are the common case for
# rented servers. Naming the ssh command has to reach the server, and the next
# run has to keep using it without being told again.
: > "$SSH_LOG"
rm -f "$STUB_STATE/started" "$STUB_STATE/uploaded.tar.gz" "$STUB_STATE/identity"
NAMED_KEY="$WORK_DIR/named-key"
printf 'not a real key\n' > "$NAMED_KEY"
export HERDR_GATEWAY_DEPLOY_SSH="ssh -i $NAMED_KEY"
run_deploy
[ "$STATUS" -eq 0 ] || fail "named-key run exited $STATUS"
test "$(cat "$STUB_STATE/identity")" = "$NAMED_KEY" ||
    fail "the named key never reached ssh"
grep -Fq "HERDR_GATEWAY_DEPLOY_SSH='ssh -i $NAMED_KEY'" "$STATE_FILE" ||
    fail "the working ssh command was not remembered"

: > "$SSH_LOG"
rm -f "$STUB_STATE/started" "$STUB_STATE/uploaded.tar.gz" "$STUB_STATE/identity"
unset HERDR_GATEWAY_DEPLOY_SSH
run_deploy
[ "$STATUS" -eq 0 ] || fail "remembered-key rerun exited $STATUS"
grep -Fq "Reusing the remembered SSH command ssh -i $NAMED_KEY" "$OUTPUT" ||
    fail "rerun did not reuse the remembered ssh command"
test "$(cat "$STUB_STATE/identity")" = "$NAMED_KEY" ||
    fail "rerun did not pass the remembered key to ssh"

# --- Unreachable without a key: the diagnosis has to name both causes --------
: > "$SSH_LOG"
rm -f "$STUB_STATE/started" "$STUB_STATE/uploaded.tar.gz"
cat > "$STUB_DIR/ssh" <<'EOF'
#!/bin/sh
exit 255
EOF
chmod 700 "$STUB_DIR/ssh"
export HERDR_GATEWAY_DEPLOY_SSH="ssh"
run_deploy
[ "$STATUS" -ne 0 ] || fail "unreachable server reported success"
grep -Fq 'Could not open an SSH session' "$OUTPUT" ||
    fail "unreachable server printed no diagnosis"
grep -Fq 'debian@' "$OUTPUT" ||
    fail "diagnosis does not mention the non-root cloud-image accounts"
grep -Fq 'HERDR_GATEWAY_DEPLOY_SSH="ssh -i ~/.ssh/yourkey"' "$OUTPUT" ||
    fail "diagnosis does not show how to name a key"
unset HERDR_GATEWAY_DEPLOY_SSH

echo "gateway deploy tests passed"
