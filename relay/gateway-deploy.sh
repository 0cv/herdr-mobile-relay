#!/usr/bin/env bash
# Deploys a gateway of your own: a docker compose stack with Caddy terminating
# TLS in front of the blind WSS gateway, driven from this menu over SSH.
#
# The bundle carries the gateway source it builds from, so the server needs
# nothing but Docker: no checkout, no Go toolchain, no registry credentials, and
# nothing downloaded from GitHub. The relay key never leaves this computer, which
# keeps the secret-free gateway and the key-holding relay apart.
#
# Non-interactive: set HERDR_GATEWAY_DEPLOY_HOST to skip the hostname prompt,
# HERDR_GATEWAY_DEPLOY_EMAIL for the ACME contact, HERDR_GATEWAY_DEPLOY_DIR for
# the bundle directory, HERDR_GATEWAY_DEPLOY_FORCE=true to reuse a directory that
# already has files in it, HERDR_GATEWAY_DEPLOY_SERVER for the SSH address to
# deploy to, HERDR_GATEWAY_DEPLOY_REMOTE_DIR for the directory on that server,
# and HERDR_GATEWAY_DEPLOY_INSTALL_DOCKER=true to install Docker there when it is
# missing. Answers are remembered beside the relay environment, so a rerun offers
# them as defaults and a non-interactive rerun reuses them outright: a redeploy
# after changing the gateway source is `bash relay/gateway-deploy.sh` and Enter.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ENV_FILE="$(relay_env_file "$SCRIPT_DIR")"
refuse_if_tailscale_selected "$ENV_FILE" || exit 1
# Answers from the last run, kept beside the relay environment like the recorded
# phone app origin. Rerunning to redeploy should not re-type a hostname, an SSH
# address, and a directory that have not changed. Precedence everywhere below is
# explicit environment variable, then remembered answer, then built-in default.
DEPLOY_STATE_FILE="$(dirname "$ENV_FILE")/gateway-deploy"

DEFAULT_BUNDLE_DIR="./herdr-gateway-deploy"
DEFAULT_REMOTE_DIR="/opt/herdr-gateway"
# Exactly what Dockerfile.gateway builds from: the gateway command and the
# internal packages it imports. Everything else in the repository stays on this
# computer.
GATEWAY_SOURCE_PATHS=(
    Dockerfile.gateway
    go.mod
    go.sum
    cmd/herdr-gateway
    internal/atomicfile
    internal/gateway
    internal/gatewaywire
)
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
SSH_BIN=()
read -r -a SSH_BIN <<<"${HERDR_GATEWAY_DEPLOY_SSH:-ssh}"
SSH_ARGS=()
SSH_CONTROL_DIR=""
REMOTE_SUDO=""
# Seconds to wait for the gateway on the server, then for its first certificate.
# A small server compiling the gateway and a slow ACME round trip both live here.
REMOTE_HEALTH_TIMEOUT=120
PUBLIC_HEALTH_TIMEOUT="${HERDR_GATEWAY_DEPLOY_PUBLIC_TIMEOUT:-240}"


gateway_source_version() {
    sed -n 's/^version = "\([^"]*\)"/\1/p' "$REPO_DIR/herdr-plugin.toml" | head -1
}

gateway_source_revision() {
    git -C "$REPO_DIR" rev-parse HEAD 2>/dev/null || printf 'unknown\n'
}

GATEWAY_BUILD_VERSION="$(gateway_source_version)"
GATEWAY_BUILD_REVISION="$(gateway_source_revision)"
GATEWAY_BUILD_VERSION="${GATEWAY_BUILD_VERSION:-dev}"
GATEWAY_BUILD_REVISION="${GATEWAY_BUILD_REVISION:-unknown}"
have_tty() {
    [ -t 0 ]
}

# A bundle serves one public name with TLS on 443, so a port, a loopback
# address, or a bare IP cannot work: ACME will not issue a certificate for any
# of them. Rejecting them here beats a Caddy retry loop on the server.
public_gateway_host() {
    local input="$1"
    local url
    local host

    if ! url="$(normalize_gateway_url "$input")"; then
        echo "✗ Enter a hostname or URL with no path, such as gw.example.com." >&2
        return 1
    fi
    case "$url" in
        wss://*)
            host="${url#wss://}"
            ;;
        *)
            echo "✗ $input is a loopback or plain-HTTP address." >&2
            echo "  A deployment needs a public hostname so Caddy can get a certificate." >&2
            return 1
            ;;
    esac
    case "$host" in
        *:*)
            echo "✗ Drop the port: the bundle serves TLS on 443, which is what phones dial." >&2
            return 1
            ;;
    esac
    case "$host" in
        *[!0-9.]*) ;;
        *)
            echo "✗ $host is an IP address. Certificates need a DNS name." >&2
            return 1
            ;;
    esac
    case "$host" in
        *.*) ;;
        *)
            echo "✗ $host is not a fully qualified name, such as gw.example.com." >&2
            return 1
            ;;
    esac
    printf '%s\n' "$host"
}

valid_acme_email() {
    case "$1" in
        *[[:space:]]* | *@*@* | *'|'*) return 1 ;;
        ?*@?*.?*) return 0 ;;
    esac
    return 1
}

# The remembered host is the default, so Enter reuses it. It is validated exactly
# like a typed one: a recorded value can be stale, or hand-edited.
prompt_gateway_host() {
    local default_host="${1:-}"
    local label="Public hostname for the gateway, or q to cancel"
    local entered
    local host

    echo "" >&2
    echo "The public hostname is what phones dial and what the certificate is" >&2
    echo "issued for. It has to resolve to the server." >&2
    echo "" >&2
    if [ -n "$default_host" ]; then
        label="Public hostname for the gateway [$default_host], or q to cancel"
    fi
    while true; do
        if ! read -r -p "$label: " entered; then
            echo "" >&2
            return 1
        fi
        case "$entered" in
            q | Q) return 1 ;;
        esac
        entered="${entered:-$default_host}"
        [ -n "$entered" ] || continue
        if host="$(public_gateway_host "$entered")"; then
            printf '%s\n' "$host"
            return 0
        fi
    done
}

prompt_acme_email() {
    local default_email="${1:-}"
    local label="Email for Let's Encrypt (optional, press enter to skip)"
    local entered

    echo "" >&2
    echo "Let's Encrypt can send certificate expiry warnings to an address." >&2
    echo "It is optional: without one, Caddy still issues and renews." >&2
    if [ -n "$default_email" ]; then
        label="Email for Let's Encrypt [$default_email] (enter to keep, - to clear)"
    fi
    while true; do
        if ! read -r -p "$label: " entered; then
            echo "" >&2
            return 0
        fi
        if [ -z "$entered" ]; then
            printf '%s' "$default_email"
            [ -z "$default_email" ] || printf '\n'
            return 0
        fi
        if [ "$entered" = "-" ]; then
            return 0
        fi
        if valid_acme_email "$entered"; then
            printf '%s\n' "$entered"
            return 0
        fi
        echo "✗ That does not look like an email address. Press enter to skip it." >&2
    done
}

prompt_bundle_dir() {
    local default_dir="${1:-$DEFAULT_BUNDLE_DIR}"
    local entered

    echo "" >&2
    if ! read -r -p "Write the bundle to [$default_dir]: " entered; then
        echo "" >&2
        return 1
    fi
    printf '%s\n' "${entered:-$default_dir}"
}

# Reads one remembered answer. Missing file or key prints nothing.
remembered_answer() {
    env_file_value "$DEPLOY_STATE_FILE" "$1"
}

accept_unchanged() {
    printf '%s\n' "$1"
}

accept_ssh_target() {
    if ! valid_ssh_target "$1"; then
        echo "✗ HERDR_GATEWAY_DEPLOY_SERVER is not an SSH address: $1" >&2
        return 1
    fi
    printf '%s\n' "$1"
}

accept_remote_dir() {
    local directory="${1%/}"

    if ! valid_remote_dir "$directory"; then
        echo "✗ HERDR_GATEWAY_DEPLOY_REMOTE_DIR is not a usable absolute path: $1" >&2
        return 1
    fi
    printf '%s\n' "$directory"
}

accept_acme_email() {
    if ! valid_acme_email "$1"; then
        echo "✗ HERDR_GATEWAY_DEPLOY_EMAIL is not an email address: $1" >&2
        return 1
    fi
    printf '%s\n' "$1"
}

# Sets the variable $1 from, in order: the environment variable $2, the answer
# typed at the prompt function $4 (empty for none), the remembered answer $5.
# $3 prints the normalized value of an answer and fails when it is unusable.
# A remembered answer that is no longer usable is ignored. $6 names the answer
# in the "Reusing" message printed when the remembered one is taken without asking.
resolve_answer() {
    local result="$1"
    local key="$2"
    local accept="$3"
    local prompt="$4"
    local remembered="$5"
    local label="${6:-}"
    local explicit="${!key:-}"
    local value=""

    remembered="$("$accept" "$remembered" 2>/dev/null)" || remembered=""
    if [ -n "$explicit" ]; then
        value="$("$accept" "$explicit")" || return 1
    elif [ -n "$prompt" ] && have_tty; then
        value="$("$prompt" "$remembered")" || return 1
    else
        value="$remembered"
        if [ -n "$value" ] && [ -n "$label" ]; then
            echo "▸ Reusing the remembered $label $value"
        fi
    fi
    printf -v "$result" '%s' "$value"
}

# Records the answers this run used, so the next one only needs Enter. Failing to
# remember must never fail a working deployment, hence the tolerated write error.
remember_answers() {
    local key
    local value

    for key in "$@"; do
        value="${!key}"
        if [ -z "$value" ]; then
            continue
        fi
        set_env_value_atomic "$DEPLOY_STATE_FILE" "$key" "$value" || return 0
    done
}

directory_has_files() {
    [ -d "$1" ] || return 1
    [ -n "$(find "$1" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" ]
}

# A directory this script already filled is safe to rewrite: refreshing the
# bundle is the whole point of a rerun. Anything else still asks first, so a
# mistyped path can never quietly lose a user's files.
previous_bundle() {
    [ -f "$1/docker-compose.yml" ] &&
        [ -f "$1/Caddyfile" ] &&
        [ -d "$1/gateway-source" ]
}

confirm_reuse_directory() {
    local directory="$1"
    local answer

    if [ "${HERDR_GATEWAY_DEPLOY_FORCE:-}" = "true" ]; then
        return 0
    fi
    if ! have_tty; then
        echo "✗ $directory already has files in it." >&2
        echo "  Pick an empty directory, or set HERDR_GATEWAY_DEPLOY_FORCE=true to overwrite." >&2
        return 1
    fi
    echo ""
    echo "▸ $directory already has files in it."
    read -r -p "  Overwrite the bundle files in it? [y/N]: " answer || answer=""
    case "${answer:-n}" in
        y | Y | yes | YES) return 0 ;;
    esac
    echo "✗ Left alone. Nothing was written."
    return 1
}

sed_replacement() {
    # The templates use | as sed's delimiter. Escaping the three replacement
    # metacharacters keeps a valid address or ACME email from changing the
    # generated YAML/Caddyfile.
    printf '%s' "$1" | sed 's/[\\&|]/\\&/g'
}

# Every bundle file is a literal template in gateway-bundle/; the deployment's
# own values are substituted here so the templates stay readable and
# quoting-proof.
render_bundle_file() {
    local host
    local wss
    local https
    local bundle
    local gateway_version
    local gateway_revision
    local email_line
    local email_note

    host="$(sed_replacement "$GATEWAY_HOST")"
    wss="$(sed_replacement "$GATEWAY_WSS")"
    https="$(sed_replacement "$GATEWAY_HTTPS")"
    bundle="$(sed_replacement "$BUNDLE_NAME")"
    gateway_version="$(sed_replacement "$GATEWAY_BUILD_VERSION")"
    gateway_revision="$(sed_replacement "$GATEWAY_BUILD_REVISION")"
    email_line="$(sed_replacement "$ACME_EMAIL_LINE")"
    email_note="$(sed_replacement "$ACME_EMAIL_NOTE")"

    sed \
        -e "s|@HOST@|$host|g" \
        -e "s|@WSS@|$wss|g" \
        -e "s|@HTTPS@|$https|g" \
        -e "s|@BUNDLE@|$bundle|g" \
        -e "s|@GATEWAY_VERSION@|$gateway_version|g" \
        -e "s|@GATEWAY_REVISION@|$gateway_revision|g" \
        -e "s|@EMAIL_LINE@|$email_line|g" \
        -e "s|@EMAIL_NOTE@|$email_note|g" \
        "$SCRIPT_DIR/gateway-bundle/$1" >"$2"
}

# The bundle builds the gateway on the server, so it has to carry exactly the
# source Dockerfile.gateway compiles. Nothing else from the checkout travels.
copy_gateway_source() {
    local destination="$1"
    local path

    for path in "${GATEWAY_SOURCE_PATHS[@]}"; do
        if [ ! -e "$REPO_DIR/$path" ]; then
            echo "✗ $REPO_DIR/$path is missing, so the bundle cannot build the gateway." >&2
            echo "  Run this from the plugin checkout that ships cmd/herdr-gateway." >&2
            return 1
        fi
    done
    rm -rf "$destination"
    mkdir -p "$destination"
    for path in "${GATEWAY_SOURCE_PATHS[@]}"; do
        case "$path" in
            */*) mkdir -p "$destination/${path%/*}" ;;
        esac
        cp -R "$REPO_DIR/$path" "$destination/$path"
    done
}

# An SSH destination, not a URL: user@host, user@ip, or a host from ~/.ssh/config.
valid_ssh_target() {
    case "$1" in
        '' | *@ | @* | *@*@*) return 1 ;;
        *[!A-Za-z0-9@._+-]*) return 1 ;;
    esac
    return 0
}

# Kept to a conservative character set: the remote commands below interpolate it
# into one shell string, and a path that needs quoting has no business here.
valid_remote_dir() {
    case "$1" in
        /*) ;;
        *) return 1 ;;
    esac
    case "$1" in
        *[!A-Za-z0-9._/-]*) return 1 ;;
    esac
    return 0
}

prompt_ssh_target() {
    local default_target="${1:-root@$GATEWAY_HOST}"
    local entered

    echo "" >&2
    echo "Where should it run? Give the SSH address of the server. Leave it empty" >&2
    echo "to only write the bundle here and copy it yourself." >&2
    echo "" >&2
    while true; do
        if ! read -r -p "Server SSH address [$default_target]: " entered; then
            echo "" >&2
            return 1
        fi
        entered="${entered:-$default_target}"
        if valid_ssh_target "$entered"; then
            printf '%s\n' "$entered"
            return 0
        fi
        echo "✗ Enter user@host, user@ip, or a host from your SSH config." >&2
    done
}

prompt_remote_dir() {
    local default_dir="${1:-$DEFAULT_REMOTE_DIR}"
    local entered

    while true; do
        if ! read -r -p "Directory on the server [$default_dir]: " entered; then
            echo "" >&2
            return 1
        fi
        entered="${entered:-$default_dir}"
        entered="${entered%/}"
        if valid_remote_dir "$entered"; then
            printf '%s\n' "$entered"
            return 0
        fi
        echo "✗ Enter an absolute path such as $DEFAULT_REMOTE_DIR." >&2
    done
}

# One authenticated connection for every remote step: a key passphrase or
# password is entered once and the master socket carries the rest, including the
# tarball on stdin.
ssh_session_stop() {
    [ -n "$SSH_CONTROL_DIR" ] || return 0
    if [ -S "$SSH_CONTROL_DIR/control" ]; then
        "${SSH_BIN[@]}" "${SSH_ARGS[@]}" -O exit "$SSH_TARGET" >/dev/null 2>&1 || true
    fi
    rm -rf "$SSH_CONTROL_DIR"
    SSH_CONTROL_DIR=""
}

ssh_session_start() {
    # Short path on purpose: a control socket lives under the ~104 byte sun_path
    # limit, which a macOS TMPDIR alone can exhaust.
    SSH_CONTROL_DIR="$(mktemp -d "/tmp/herdr-gw.XXXXXX")"
    SSH_ARGS=(
        -o ControlMaster=auto
        -o "ControlPath=$SSH_CONTROL_DIR/control"
        -o ControlPersist=180
        -o ConnectTimeout=15
    )
    trap ssh_session_stop EXIT
}

remote_run() {
    "${SSH_BIN[@]}" "${SSH_ARGS[@]}" "$SSH_TARGET" "$1"
}

remote_shell() {
    remote_run "${REMOTE_SUDO:+$REMOTE_SUDO }sh -c '$1'"
}

# Only privilege escalation is constrained, never authentication: the session is
# opened once and reused, so an SSH password prompt is answered once and costs
# nothing. A sudo password prompt is different - it would appear on the far side
# of a non-interactive command and hang instead of failing.
resolve_remote_privilege() {
    local uid

    if ! uid="$(remote_run 'id -u')"; then
        return 1
    fi
    uid="${uid//[$'\r\n']/}"
    if [ "$uid" = "0" ]; then
        REMOTE_SUDO=""
        return 0
    fi
    if remote_run 'sudo -n true' >/dev/null 2>&1; then
        REMOTE_SUDO="sudo -n"
        return 0
    fi
    echo "✗ $SSH_TARGET is not root and sudo there asks for a password." >&2
    echo "  Remote commands run non-interactively, so sudo cannot prompt." >&2
    echo "  Use a root address such as root@$GATEWAY_HOST, or give that account" >&2
    echo "  passwordless sudo. A password on the SSH login itself is fine." >&2
    return 1
}

remote_docker_ready() {
    remote_shell 'docker compose version >/dev/null 2>&1'
}

install_remote_docker() {
    remote_shell 'set -e; command -v curl >/dev/null 2>&1 || { echo "curl is missing on the server" >&2; exit 3; }; curl -fsSL https://get.docker.com -o /tmp/herdr-get-docker.sh; sh /tmp/herdr-get-docker.sh; rm -f /tmp/herdr-get-docker.sh'
}

upload_bundle() {
    tar -C "$BUNDLE_DIR" -czf - . |
        remote_shell "set -e; mkdir -p $REMOTE_DIR; if [ -e $REMOTE_DIR/.env ] || [ -L $REMOTE_DIR/.env ]; then [ -f $REMOTE_DIR/.env ] && [ -r $REMOTE_DIR/.env ] || { echo \"Existing gateway .env is not a readable regular file; refusing deployment.\" >&2; exit 1; }; tar --exclude=./.env -xzf - -C $REMOTE_DIR; else tar -xzf - -C $REMOTE_DIR; fi; sh $REMOTE_DIR/refresh-release-env.sh"
}

remote_compose_up() {
    remote_shell "cd $REMOTE_DIR && docker compose up -d --build --force-recreate gateway caddy"
}

remote_health_ready() {
    remote_shell 'curl -fsS --max-time 5 http://127.0.0.1:8443/healthz >/dev/null 2>&1 || wget -q -O- -T 5 http://127.0.0.1:8443/healthz >/dev/null 2>&1'
}

# The gateway answers on loopback as soon as it starts; the public name waits for
# Caddy's first certificate, which is the slow half.
wait_for_remote_health() {
    local deadline=$((SECONDS + REMOTE_HEALTH_TIMEOUT))

    while :; do
        if remote_health_ready; then
            return 0
        fi
        [ "$SECONDS" -lt "$deadline" ] || return 1
        printf '.'
        sleep 5
    done
}

wait_for_public_health() {
    local deadline=$((SECONDS + PUBLIC_HEALTH_TIMEOUT))

    while :; do
        if gateway_answers_healthz "$GATEWAY_WSS"; then
            return 0
        fi
        [ "$SECONDS" -lt "$deadline" ] || return 1
        printf '.'
        sleep 10
    done
}

remote_host_has_aaaa() {
    remote_shell "getent ahostsv6 $GATEWAY_HOST >/dev/null 2>&1"
}

# The bundle's network asks Docker to derive an IPv6 subnet for itself, which
# arrived in Engine 27 together with ip6tables on by default. An older engine
# rejects that network, so this is worth saying before a build rather than
# after one.
remote_docker_supports_ipv6() {
    local major

    major="$(remote_shell "docker version --format '{{.Server.Version}}' 2>/dev/null" | cut -d. -f1)"
    case "$major" in
        '' | *[!0-9]*) return 0 ;;
        *) [ "$major" -ge 27 ] ;;
    esac
}

# Let's Encrypt prefers IPv6 whenever the name publishes an AAAA record, so an
# address nothing answers on fails every challenge while every IPv4 check keeps
# looking perfect. The compose network publishes both families, so what remains
# is the server's own address or its firewall. The probe runs on the server,
# which is exactly where that address is supposed to be served.
report_ipv6_gap() {
    remote_host_has_aaaa || return 1
    remote_shell "curl -6 -fsS --max-time 8 http://$GATEWAY_HOST/ >/dev/null 2>&1" && return 1
    echo ""
    echo "▸ $GATEWAY_HOST publishes an AAAA record that answers nothing here."
    echo "  Let's Encrypt prefers IPv6 when a name has one, so the certificate"
    echo "  attempt fails over IPv6 while IPv4 keeps looking healthy. Give the"
    echo "  server that address and open TCP 80 and 443 on it, or remove the"
    echo "  AAAA record."
    return 0
}

# Caddy already logged why the certificate did not arrive. Printing the matching
# lines turns "it did not answer" into the actual obstacle.
report_caddy_acme_errors() {
    local lines

    lines="$(remote_shell "cd $REMOTE_DIR && docker compose logs --no-color --tail=200 caddy 2>/dev/null" |
        grep -Ei 'acme|challenge|certificate|obtain' |
        grep -Ei 'error|fail|problem|retry' | tail -8)"
    [ -n "$lines" ] || return 0
    echo "  Caddy reported:"
    printf '%s\n' "$lines" | sed 's/^/    /'
    echo ""
}


report_public_health_failure() {
    echo ""
    echo "▸ The gateway runs on the server, but $GATEWAY_HTTPS/healthz did not"
    echo "  answer yet, so the transport was left unchanged. The usual causes:"
    echo "    - DNS for $GATEWAY_HOST does not point at this server yet."
    echo "    - Inbound TCP 80 or 443 is blocked, so Caddy cannot finish ACME."
    echo "      ufw allow 80/tcp && ufw allow 443/tcp && ufw allow 3478/udp"
    echo "    - Another service already holds 80 or 443."
    echo "    - An AAAA record for $GATEWAY_HOST that nothing answers: ACME"
    echo "      validation prefers IPv6 and fails there while IPv4 looks fine."
    echo ""
    report_caddy_acme_errors
    echo "  Inbound UDP 3478 does not affect /healthz, but the gateway answers"
    echo "  address discovery there. Blocked, phones and computers never learn"
    echo "  their own address and every session stays on the relayed path."
    echo ""
    echo "  Watch the certificate attempt with:"
    echo "    $SSH_CMD_HINT \"cd $REMOTE_DIR && docker compose logs -f caddy\""
    echo ""
    echo "  Once it answers, choose WebRTC → Deploy or Upgrade Your Own WebRTC Gateway again. It reuses the remembered"
    echo "  answers, verifies $GATEWAY_HOST, and connects this relay."
}

# ssh only offers ~/.ssh/id_* and whatever ~/.ssh/config names, so a key kept
# anywhere else fails with a bare "Permission denied (publickey)" even though
# the same key works when the user passes -i by hand. Asking for it once beats
# making them find an environment variable, and the answer is remembered.
retry_with_identity() {
    local entered
    local key

    have_tty || return 1
    echo "  ssh offered no key this host accepts." >&2
    if ! read -r -p "  Key file to use, name or path (empty to give up): " entered; then
        echo "" >&2
        return 1
    fi
    [ -n "$entered" ] || return 1
    if ! key="$(ssh_key_path "$entered")"; then
        echo "✗ No readable key called $entered, in ~/.ssh or as a path." >&2
        return 1
    fi
    SSH_BIN=(ssh -i "$key")
    printf '▸ Retrying %s with %s..' "$SSH_TARGET" "$key"
    remote_run 'true'
}

explain_ssh_failure() {
    echo "✗ Could not open an SSH session to $SSH_TARGET." >&2
    echo "  Cloud images usually refuse root: the same server often answers as" >&2
    echo "  debian@, ubuntu@, admin@, or ec2-user@ instead." >&2
    echo "  ssh offers only the default ~/.ssh/id_* keys and whatever" >&2
    echo "  ~/.ssh/config names for this host. Any other key has to be named:" >&2
    echo "    HERDR_GATEWAY_DEPLOY_SSH=\"ssh -i ~/.ssh/yourkey\" rerun this" >&2
}

deploy_bundle() {
    ssh_session_start

    printf '▸ Connecting to %s..' "$SSH_TARGET"
    if ! remote_run 'true'; then
        echo ""
        if ! retry_with_identity; then
            explain_ssh_failure
            return 1
        fi
    fi
    echo " ✓"

    if ! resolve_remote_privilege; then
        return 1
    fi

    if remote_docker_ready; then
        echo "✓ Docker and the compose plugin are installed on the server."
    else
        echo "▸ The server has no working \"docker compose\"."
        if [ "$INSTALL_DOCKER" != "true" ]; then
            echo "✗ Install Docker and the compose plugin there, then run this again." >&2
            echo "  Debian/Ubuntu: apt-get install docker.io docker-compose-plugin" >&2
            echo "  Or rerun and allow the official get.docker.com installer." >&2
            return 1
        fi
        echo "▸ Installing Docker with the official get.docker.com script.."
        if ! install_remote_docker; then
            echo "✗ Docker installation failed on the server." >&2
            return 1
        fi
        if ! remote_docker_ready; then
            echo "✗ Docker still does not run on the server after installing it." >&2
            return 1
        fi
        echo "✓ Docker installed."
    fi

    if remote_host_has_aaaa && ! remote_docker_supports_ipv6; then
        echo "▸ $GATEWAY_HOST has an AAAA record, but this server runs a Docker"
        echo "  older than 27, which cannot give the stack an IPv6 network."
        echo "  Let's Encrypt prefers IPv6 and its challenge will fail there."
        echo "  Upgrade Docker on the server, or remove the AAAA record."
    fi

    printf '▸ Copying the bundle to %s:%s..' "$SSH_TARGET" "$REMOTE_DIR"
    if ! upload_bundle; then
        echo ""
        echo "✗ Could not copy the bundle to $REMOTE_DIR." >&2
        return 1
    fi
    echo " ✓"

    echo "▸ Building the gateway image and starting both containers."
    echo "  The first build compiles the gateway on the server; expect a minute or two."
    if ! remote_compose_up; then
        echo "✗ \"docker compose up -d --build --force-recreate gateway caddy\" failed on the server." >&2
        echo "  Inspect it with:" >&2
        echo "    $SSH_CMD_HINT \"cd $REMOTE_DIR && docker compose logs --tail=80\"" >&2
        return 1
    fi

    printf '▸ Waiting for the gateway on the server'
    if ! wait_for_remote_health; then
        echo ""
        echo "✗ http://127.0.0.1:8443/healthz never answered on the server." >&2
        echo "    $SSH_CMD_HINT \"cd $REMOTE_DIR && docker compose logs --tail=80\"" >&2
        return 1
    fi
    echo " ✓"

    report_ipv6_gap
    printf '▸ Waiting for the certificate and %s' "$GATEWAY_HTTPS/healthz"
    if ! wait_for_public_health; then
        report_public_health_failure
        return 2
    fi
    echo " ✓"
    return 0
}

echo "🐑 Deploy a gateway on your own server"
echo ""
echo "This writes a small docker compose bundle - the gateway plus Caddy for TLS -"
echo "and copies it to a server over SSH. Your own gateway means dedicated"
echo "bandwidth, and the only logs are on a machine you control."
echo ""

# A relay already pointed at a gateway records that URL, which is a better
# remembered host than anything this script keeps: it is the address the phone
# was paired against.
REMEMBERED_HOST="$(remembered_answer HERDR_GATEWAY_DEPLOY_HOST)"
if [ -z "$REMEMBERED_HOST" ]; then
    REMEMBERED_HOST="$(gateway_url "$ENV_FILE")"
    REMEMBERED_HOST="${REMEMBERED_HOST#wss://}"
    REMEMBERED_HOST="${REMEMBERED_HOST#ws://}"
fi

resolve_answer GATEWAY_HOST HERDR_GATEWAY_DEPLOY_HOST public_gateway_host prompt_gateway_host \
    "$REMEMBERED_HOST" hostname || exit 1
if [ -z "$GATEWAY_HOST" ]; then
    echo "✗ No hostname to generate for, and stdin is not a terminal." >&2
    echo "  Set HERDR_GATEWAY_DEPLOY_HOST=gw.example.com and run this again." >&2
    exit 1
fi

GATEWAY_WSS="wss://$GATEWAY_HOST"
GATEWAY_HTTPS="$(gateway_http_base "$GATEWAY_WSS")"
echo "✓ Phones will dial $GATEWAY_WSS"

resolve_answer SSH_TARGET HERDR_GATEWAY_DEPLOY_SERVER accept_ssh_target prompt_ssh_target \
    "$(remembered_answer HERDR_GATEWAY_DEPLOY_SERVER)" server || exit 1

# The ssh command itself is an answer like any other: a host reached with a
# named key stays reachable on the next run without retyping anything.
if [ -z "${HERDR_GATEWAY_DEPLOY_SSH:-}" ]; then
    REMEMBERED_SSH="$(remembered_answer HERDR_GATEWAY_DEPLOY_SSH)"
    if [ -n "$REMEMBERED_SSH" ]; then
        read -r -a SSH_BIN <<<"$REMEMBERED_SSH"
        echo "▸ Reusing the remembered SSH command ${SSH_BIN[*]}"
    fi
fi

resolve_answer REMOTE_DIR HERDR_GATEWAY_DEPLOY_REMOTE_DIR accept_remote_dir \
    "${SSH_TARGET:+prompt_remote_dir}" "$(remembered_answer HERDR_GATEWAY_DEPLOY_REMOTE_DIR)" || exit 1
REMOTE_DIR="${REMOTE_DIR:-$DEFAULT_REMOTE_DIR}"

INSTALL_DOCKER="${HERDR_GATEWAY_DEPLOY_INSTALL_DOCKER:-}"
if [ -z "$INSTALL_DOCKER" ]; then
    INSTALL_DOCKER="ask"
fi

resolve_answer ACME_EMAIL HERDR_GATEWAY_DEPLOY_EMAIL accept_acme_email prompt_acme_email \
    "$(remembered_answer HERDR_GATEWAY_DEPLOY_EMAIL)" || exit 1

if [ -n "$ACME_EMAIL" ]; then
    ACME_EMAIL_LINE="email $ACME_EMAIL"
    ACME_EMAIL_NOTE="Let's Encrypt registration uses \`$ACME_EMAIL\` as the ACME contact."
else
    ACME_EMAIL_LINE="# email you@example.com"
    ACME_EMAIL_NOTE="No ACME contact was set, which is fine: Caddy still issues and renews. Uncomment the \`email\` line in the \`Caddyfile\` to get expiry warnings."
fi

resolve_answer BUNDLE_DIR HERDR_GATEWAY_DEPLOY_DIR accept_unchanged prompt_bundle_dir \
    "$(remembered_answer HERDR_GATEWAY_DEPLOY_DIR)" || exit 1
BUNDLE_DIR="${BUNDLE_DIR%/}"
BUNDLE_DIR="${BUNDLE_DIR:-$DEFAULT_BUNDLE_DIR}"

if directory_has_files "$BUNDLE_DIR" && ! previous_bundle "$BUNDLE_DIR"; then
    if ! confirm_reuse_directory "$BUNDLE_DIR"; then
        exit 1
    fi
fi

mkdir -p "$BUNDLE_DIR"
BUNDLE_DIR="$(cd "$BUNDLE_DIR" && pwd)"
BUNDLE_NAME="$(basename "$BUNDLE_DIR")"

render_bundle_file docker-compose.yml "$BUNDLE_DIR/docker-compose.yml"
render_bundle_file Caddyfile "$BUNDLE_DIR/Caddyfile"
# Existing configuration belongs to the operator, including registration policy.
if [ -e "$BUNDLE_DIR/.env" ] || [ -L "$BUNDLE_DIR/.env" ]; then
    if [ ! -f "$BUNDLE_DIR/.env" ] || [ ! -r "$BUNDLE_DIR/.env" ]; then
        echo "Existing bundle .env is not a readable regular file; refusing deployment." >&2
        exit 1
    fi
else
    render_bundle_file env "$BUNDLE_DIR/.env"
fi
render_bundle_file refresh-release-env.sh "$BUNDLE_DIR/refresh-release-env.sh"
sh "$BUNDLE_DIR/refresh-release-env.sh"
render_bundle_file README.md "$BUNDLE_DIR/README.md"
if ! copy_gateway_source "$BUNDLE_DIR/gateway-source"; then
    exit 1
fi

# Remembered once the answers are known to be usable: the bundle rendered, the
# hostname validated, the paths accepted. A failed deployment further down still
# leaves them recorded, because retrying is exactly when re-typing hurts most.
HERDR_GATEWAY_DEPLOY_HOST="$GATEWAY_HOST"
HERDR_GATEWAY_DEPLOY_SERVER="$SSH_TARGET"
HERDR_GATEWAY_DEPLOY_REMOTE_DIR="$REMOTE_DIR"
HERDR_GATEWAY_DEPLOY_EMAIL="$ACME_EMAIL"
HERDR_GATEWAY_DEPLOY_DIR="$BUNDLE_DIR"
remember_answers \
    HERDR_GATEWAY_DEPLOY_HOST \
    HERDR_GATEWAY_DEPLOY_SERVER \
    HERDR_GATEWAY_DEPLOY_REMOTE_DIR \
    HERDR_GATEWAY_DEPLOY_EMAIL \
    HERDR_GATEWAY_DEPLOY_DIR

echo ""
echo "✓ Bundle written to $BUNDLE_DIR"
echo "    docker-compose.yml  gateway on loopback plus UDP 3478, Caddy on 80/443"
echo "    Caddyfile           TLS and reverse proxy for $GATEWAY_HOST"
echo "    .env                quotas, limits, image tags"
echo "    gateway-source/     the gateway source the server builds"
echo "    README.md           the deployment steps and day-to-day commands"

if [ -z "$SSH_TARGET" ]; then
    echo ""
    echo "Next:"
    echo "  1. Point DNS for $GATEWAY_HOST at the server."
    echo "  2. scp -r $BUNDLE_NAME root@$GATEWAY_HOST:$DEFAULT_REMOTE_DIR"
    echo "  3. cd $DEFAULT_REMOTE_DIR && docker compose up -d --build --force-recreate gateway caddy"
    echo "  4. curl $GATEWAY_HTTPS/healthz"
    echo ""
    echo "Open inbound TCP 80 and 443 and inbound UDP 3478, and allow outbound UDP"
    echo "for /probe. UDP 3478 is address discovery: without it phones and"
    echo "computers behind NAT stay on the relayed path."
    echo ""
    echo "▸ Once it answers, set HERDR_GATEWAY_URL=$GATEWAY_WSS in the relay"
    echo "  environment, or choose WebRTC → Deploy or Upgrade Your Own WebRTC Gateway to deploy and connect over SSH."
    exit 0
fi

SSH_CMD_HINT="${SSH_BIN[*]} $SSH_TARGET"

if [ "$INSTALL_DOCKER" = "ask" ]; then
    if have_tty; then
        echo ""
        read -r -p "Install Docker on the server if it is missing? [Y/n]: " INSTALL_ANSWER || INSTALL_ANSWER=""
        case "${INSTALL_ANSWER:-y}" in
            y | Y | yes | YES) INSTALL_DOCKER="true" ;;
            *) INSTALL_DOCKER="false" ;;
        esac
    else
        INSTALL_DOCKER="false"
    fi
fi

echo ""
echo "▸ Deploying to $SSH_TARGET:$REMOTE_DIR"

DEPLOY_STATUS=0
deploy_bundle || DEPLOY_STATUS=$?

# Recorded after the attempt, not before it: the key that finally opened the
# session is only known once one has.
if [ "${SSH_BIN[*]}" != "ssh" ]; then
    HERDR_GATEWAY_DEPLOY_SSH="${SSH_BIN[*]}"
    remember_answers HERDR_GATEWAY_DEPLOY_SSH
fi

case "$DEPLOY_STATUS" in
    0) ;;
    2) exit 0 ;;
    *) exit 1 ;;
esac

if ! GATEWAY_DEFAULTS="$(gateway_subscription_defaults "$GATEWAY_WSS")"; then
    echo "✗ Could not build the gateway candidate list." >&2
    exit 1
fi
if ! GATEWAY_LIST="$(prompt_gateway_subscriptions "$GATEWAY_DEFAULTS")"; then
    echo "✗ Gateway subscriptions were not changed." >&2
    exit 1
fi
set_gateway_url "$ENV_FILE" "$GATEWAY_LIST"
# The operator's order is authoritative: their own gateway stays preferred,
# while any later entries are cold fallbacks.
set_gateway_selection "$ENV_FILE" ordered
echo ""
echo "✓ $GATEWAY_HTTPS/healthz answered."
echo "  Saved gateway subscriptions, in priority order:"
printf '%s\n' "$GATEWAY_LIST" | tr ',' '\n' | sed 's/^/    /'
echo "  The relay will register with the first healthy candidate."
echo "  Returning to start or restart the relay and print its phone QR."
echo ""
echo "Day-to-day on the server:"
echo "  $SSH_CMD_HINT \"cd $REMOTE_DIR && docker compose logs -f --tail=50\""
echo "  $SSH_CMD_HINT \"cd $REMOTE_DIR && docker compose up -d\"   # after editing .env"
