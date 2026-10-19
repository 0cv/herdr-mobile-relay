#!/bin/sh
set -eu
# Admission precedes discovery, builds and temporary roots; never forge flags.
[ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] && [ "$(id -u)" -ne 0 ] || {
    echo 'release fixtures require a genuine disposable non-root GitHub-hosted runner' >&2; exit 1;
}
umask 077
check_release() {
    python3 - "$REPO_DIR/scripts/check-installed-release.sh" "$@" <<'PY'
import os, signal, subprocess, sys
p = subprocess.Popen(sys.argv[1:], start_new_session=True)
try:
    status = p.wait(timeout=90)
finally:
    try: os.killpg(p.pid, signal.SIGKILL)
    except ProcessLookupError: pass
    p.wait(timeout=2)
sys.exit(status)
PY
}

SCRIPT_DIR=${0%/*}
if [ "$SCRIPT_DIR" = "$0" ]; then
    SCRIPT_DIR=.
fi
REPO_DIR=$(CDPATH='' cd "$SCRIPT_DIR/.." && pwd)
WORK_DIR=$(mktemp -d "${TMPDIR:-/tmp}/herdr-release-script-test.XXXXXX")
trap 'status=$?; rm -rf "$WORK_DIR"; exit $status' EXIT INT TERM

case $(uname -s) in
    Linux) HOST_OS=linux; WRONG_OS=darwin ;;
    Darwin) HOST_OS=darwin; WRONG_OS=linux ;;
    *)
        echo "unsupported test operating system: $(uname -s)" >&2
        exit 1
        ;;
esac
case $(uname -m) in
    x86_64|amd64) HOST_ARCH=amd64 ;;
    arm64|aarch64) HOST_ARCH=arm64 ;;
    *)
        echo "unsupported test architecture: $(uname -m)" >&2
        exit 1
        ;;
esac

CHECKSUMS="$WORK_DIR/checksums.txt"
ARCHIVE="$WORK_DIR/herdr-mobile-relay_0.0.0_${WRONG_OS}_${HOST_ARCH}.tar.gz"
: > "$ARCHIVE"
printf '%064d  %s\n' 0 "${ARCHIVE##*/}" > "$CHECKSUMS"
if OUTPUT=$(
    check_release \
        "$ARCHIVE" "$CHECKSUMS" 0.0.0 test-revision "$WRONG_OS/$HOST_ARCH" 2>&1
); then
    echo "installed-release check accepted a non-native archive" >&2
    exit 1
fi
printf '%s\n' "$OUTPUT" | grep -q "does not match native target"

ARCHIVE="$WORK_DIR/herdr-mobile-relay_0.0.0_${HOST_OS}_${HOST_ARCH}.tar.gz"
: > "$ARCHIVE"
printf '%064d  %s\n' 0 "${ARCHIVE##*/}" > "$CHECKSUMS"
if OUTPUT=$(
    check_release \
        "$ARCHIVE" "$CHECKSUMS" 0.0.0 test-revision "$HOST_OS/$HOST_ARCH" 2>&1
); then
    echo "installed-release check accepted a checksum mismatch" >&2
    exit 1
fi
printf '%s\n' "$OUTPUT" | grep -q "checksum mismatch"

printf '%064d  %s  unexpected-field\n' 0 "${ARCHIVE##*/}" > "$CHECKSUMS"
if OUTPUT=$(
    check_release \
        "$ARCHIVE" "$CHECKSUMS" 0.0.0 test-revision "$HOST_OS/$HOST_ARCH" 2>&1
); then
    echo "installed-release check accepted a malformed checksum entry" >&2
    exit 1
fi
printf '%s\n' "$OUTPUT" | grep -q "must contain exactly one entry"

BINARY_VERSION=0.0.0
MANIFEST_VERSION=9.9.9
REVISION=test-revision
HOST_TARGET="$HOST_OS/$HOST_ARCH"
WRONG_TARGET="$WRONG_OS/$HOST_ARCH"
RELEASE_DIR="$WORK_DIR/release"
mkdir -p "$RELEASE_DIR/web" "$RELEASE_DIR/relay"
CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags "-s -w -X main.version=$BINARY_VERSION -X main.revision=$REVISION" \
    -o "$RELEASE_DIR/herdr-mobile-relay" \
    "$REPO_DIR/cmd/herdr-mobile-relay"
printf '%s\n' '<html></html>' > "$RELEASE_DIR/web/index.html"
printf '%s\n' license > "$RELEASE_DIR/LICENSE"
printf '%s\n' readme > "$RELEASE_DIR/README.md"
for WRAPPER in \
    common.sh \
    herdr-mobile-relay-service.sh \
    plugin-on-event.sh \
    setup-link.sh \
    stable-setup.sh \
    stable-teardown.sh \
    start.sh \
    tailscale.sh \
    tailscale-external.sh \
    tailscale-cli.sh \
    tailscale-cli-service.sh \
    transport-switch-locked.sh; do
    case "$WRAPPER" in
        common.sh|herdr-mobile-relay-service.sh|tailscale-cli.sh|tailscale-cli-service.sh|transport-switch-locked.sh)
            cp "$REPO_DIR/relay/$WRAPPER" "$RELEASE_DIR/relay/$WRAPPER"
            ;;
        *) printf '%s\n' '#!/bin/sh' > "$RELEASE_DIR/relay/$WRAPPER" ;;
    esac
    chmod 755 "$RELEASE_DIR/relay/$WRAPPER"
done

cp "$REPO_DIR/relay/pi-commands.sh" "$RELEASE_DIR/relay/pi-commands.sh"
cp -R "$REPO_DIR/relay/pi-command-bridge" "$RELEASE_DIR/relay/pi-command-bridge"

"$RELEASE_DIR/herdr-mobile-relay" release-manifest \
    "$RELEASE_DIR" "$MANIFEST_VERSION" "$REVISION" "$HOST_TARGET" >/dev/null
ARCHIVE="$WORK_DIR/herdr-mobile-relay_${MANIFEST_VERSION}_${HOST_OS}_${HOST_ARCH}.tar.gz"
tar -C "$RELEASE_DIR" -czf "$ARCHIVE" .
if command -v sha256sum >/dev/null 2>&1; then
    HASH=$(sha256sum "$ARCHIVE" | awk '{print $1}')
else
    HASH=$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')
fi
printf '%s  %s\n' "$HASH" "${ARCHIVE##*/}" > "$CHECKSUMS"
if OUTPUT=$(
    check_release \
        "$ARCHIVE" "$CHECKSUMS" "$MANIFEST_VERSION" "$REVISION" "$HOST_TARGET" 2>&1
); then
    echo "installed-release check accepted a manifest/binary identity mismatch" >&2
    exit 1
fi
printf '%s\n' "$OUTPUT" | grep -q "does not match binary version"

"$RELEASE_DIR/herdr-mobile-relay" release-manifest \
    "$RELEASE_DIR" "$BINARY_VERSION" "$REVISION" "$WRONG_TARGET" >/dev/null
if OUTPUT=$(
    "$RELEASE_DIR/herdr-mobile-relay" verify-release \
        --target "$WRONG_TARGET" "$RELEASE_DIR" 2>&1
); then
    echo "verify-release accepted a manifest/binary target mismatch" >&2
    exit 1
fi
printf '%s\n' "$OUTPUT" | grep -q "does not match binary target"
"$RELEASE_DIR/herdr-mobile-relay" verify-release \
    --allow-cross-target --target "$WRONG_TARGET" "$RELEASE_DIR" >/dev/null

"$RELEASE_DIR/herdr-mobile-relay" release-manifest \
    "$RELEASE_DIR" "$BINARY_VERSION" "$REVISION" "$HOST_TARGET" >/dev/null
ARCHIVE="$WORK_DIR/herdr-mobile-relay_${BINARY_VERSION}_${HOST_OS}_${HOST_ARCH}.tar.gz"
tar -C "$RELEASE_DIR" -czf "$ARCHIVE" .
if command -v sha256sum >/dev/null 2>&1; then
    HASH=$(sha256sum "$ARCHIVE" | awk '{print $1}')
else
    HASH=$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')
fi
printf '%s  %s\n' "$HASH" "${ARCHIVE##*/}" > "$CHECKSUMS"

# Rebind a synthetic archive to a delegating relay: real serve/verify-release
# still run, while selected scalar or support acquisitions are corrupted.
# Exercise the installed checker's decisions, not a copied comparison.
cp "$ARCHIVE" "$WORK_DIR/positive.tar.gz"
# Exercise the real command from an extracted native archive, not a checkout
# binary or a parser test. This does not invoke uninstall or authorize deletion.
GUARD_EXTRACT="$WORK_DIR/guard-extract"
GUARD_HOME="$WORK_DIR/guard-home"
mkdir -p "$GUARD_EXTRACT" "$GUARD_HOME/releases" "$GUARD_HOME/config" "$GUARD_HOME/cache"
GUARD_HOME=$(CDPATH='' cd "$GUARD_HOME" && pwd -P)
tar -C "$GUARD_EXTRACT" -xzf "$WORK_DIR/positive.tar.gz"
GUARD_RELAY="$GUARD_EXTRACT/herdr-mobile-relay"
# Classify the complete namespace independently; never use the guard verdict.
# fdinfo mnt_id is the descriptor-bound ID also returned by statx.
GUARD_SUPPORTED=false
if [ "$HOST_OS" = linux ] && python3 - "$GUARD_HOME" <<'PY'
import os, re, sys
try:
    def number(raw):
        if not raw or not raw.isdigit(): raise ValueError()
        value = int(raw)
        if value > (1 << 64) - 1: raise ValueError()
        return value
    def path(raw):
        if re.search(rb'\\(?!040|011|012|134)', raw): raise ValueError()
        for code, char in ((b'040', b' '), (b'011', b'\t'), (b'012', b'\n'), (b'134', b'\\')):
            raw = raw.replace(b'\\' + code, char)
        if not raw.startswith(b'/') or raw != os.path.normpath(raw) or b'//' in raw or \
                any(c in raw for c in (b'\x00', b'\r', b'\t', b'\n', b'\\')): raise ValueError()
        return raw
    def under(child, parent):
        return child == parent or parent == b'/' and child.startswith(b'/') or child.startswith(parent + b'/')
    def mounted(pathname):
        fd = os.open(pathname, os.O_PATH | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
        try:
            with open(f'/proc/self/fdinfo/{fd}', encoding='ascii') as info:
                ids = [line[7:].strip() for line in info if line.startswith('mnt_id:')]
            if len(ids) != 1: raise ValueError()
            dev = os.fstat(fd).st_dev
            return number(ids[0].encode('ascii')), (os.major(dev), os.minor(dev))
        finally:
            os.close(fd)
    home = path(os.fsencode(sys.argv[1]))
    base_id, base_dev = mounted('/')
    selected_id, selected_dev = mounted(sys.argv[1])
    with open('/proc/self/mountinfo', 'rb') as inventory:
        data = inventory.read((4 << 20) + 1)
    if not data or len(data) > 4 << 20 or not data.endswith(b'\n') or b'\x00' in data:
        raise ValueError()
    mounts, points = {}, set()
    for line in data.splitlines():
        fields = line.split()
        if len(line) > 4096 or len(mounts) >= 16384 or len(fields) < 10 or \
                fields.count(b'-') != 1 or b' '.join(fields) != line: raise ValueError()
        sep = fields.index(b'-')
        if sep < 6 or len(fields) - sep != 4 or fields[sep+1] == b'autofs' or \
                any(not field for field in (fields[5], fields[sep+1], fields[sep+2], fields[sep+3])):
            raise ValueError()
        if any(re.search(rb'\\(?!040|011|012|134)', field) for field in fields): raise ValueError()
        for tag in fields[6:sep]:
            if tag == b'unbindable': continue
            key, _, value = tag.partition(b':')
            if key not in (b'shared', b'master', b'propagate_from') or number(value) == 0:
                raise ValueError()
        ident, parent = number(fields[0]), number(fields[1])
        major, minor = (number(part) for part in fields[2].split(b':'))
        origin, point = path(fields[3]), path(fields[4])
        if ident == 0 or ident == parent or ident in mounts or point in points: raise ValueError()
        mounts[ident] = (parent, (major, minor), origin, point)
        points.add(point)
    base = mounts.get(base_id)
    if base is None or base[2:] != (b'/', b'/') or base[1] != base_dev or \
            selected_id != base_id or selected_dev != base_dev or len(mounts) != len(points):
        raise ValueError()
    roots = tuple(home + b'/' + name for name in (b'releases', b'config', b'cache'))
    for ident, (parent, dev, origin, point) in mounts.items():
        if ident == base_id: continue
        ancestor = mounts.get(parent)
        if ancestor is None or point == ancestor[3] or not under(point, ancestor[3]): raise ValueError()
        for root in roots:
            if under(root, point) or under(point, root) or \
                    dev == base_dev and (under(root, origin) or under(origin, root)):
                raise ValueError()
except (OSError, ValueError, IndexError, UnicodeError, AttributeError):
    sys.exit(1)
PY
then
    GUARD_SUPPORTED=true
fi
printf '%s\n' fixture-private-canary > "$GUARD_HOME/config/keep"
GUARD_BEFORE=$(find "$GUARD_HOME" -print | sort)
GUARD_MODES=$(ls -ld "$GUARD_HOME" "$GUARD_HOME"/* "$GUARD_HOME/config/keep")
if [ "$GUARD_SUPPORTED" = true ]; then
    if ! OUTPUT=$(HOME="$GUARD_HOME" "$GUARD_RELAY" __private-uninstall-mount-guard \
        "$GUARD_HOME/releases" "$GUARD_HOME/config" "$GUARD_HOME/cache" 2>&1); then
        echo 'supported Linux base-mount fixture refused' >&2; exit 1
    fi
    [ -z "$OUTPUT" ] || { echo 'mount guard emitted data on success' >&2; exit 1; }
else
    if OUTPUT=$(HOME="$GUARD_HOME" "$GUARD_RELAY" __private-uninstall-mount-guard \
        "$GUARD_HOME/releases" "$GUARD_HOME/config" "$GUARD_HOME/cache" 2>&1); then
        echo 'unsupported mount topology or Darwin claimed safe' >&2; exit 1
    fi
    [ "$OUTPUT" = 'herdr-mobile-relay: uninstall mount guard refused' ] || {
        echo 'unsupported topology or Darwin disclosed data' >&2; exit 1;
    }
fi
if OUTPUT=$(HOME="$GUARD_HOME" "$GUARD_RELAY" __private-uninstall-mount-guard \
    "$GUARD_HOME/releases" "$GUARD_HOME/config" "$GUARD_HOME/cache" fixture-private-canary 2>&1); then
    echo 'mount guard accepted extra root' >&2; exit 1
fi
[ "$OUTPUT" = 'herdr-mobile-relay: private uninstall mount guard requires exactly three roots' ] || {
    echo 'mount guard disclosed argument or used wrong arity' >&2; exit 1;
}
if OUTPUT=$(HOME="$GUARD_HOME" "$GUARD_RELAY" __private-uninstall-mount-guard \
    "$GUARD_HOME/releases" "$GUARD_HOME/config" '/fixture-private-canary' 2>&1); then
    echo 'mount guard accepted a foreign root' >&2; exit 1
fi
[ "$OUTPUT" = 'herdr-mobile-relay: uninstall mount guard refused' ] || {
    echo 'mount guard disclosed a selected path' >&2; exit 1;
}
[ "$(ls -ld "$GUARD_HOME" "$GUARD_HOME"/* "$GUARD_HOME/config/keep")" = "$GUARD_MODES" ] &&
    [ "$(cat "$GUARD_HOME/config/keep")" = fixture-private-canary ] &&
    [ "$(find "$GUARD_HOME" -print | sort)" = "$GUARD_BEFORE" ] || {
    echo 'mount guard changed fixture tree' >&2; exit 1;
}
# Snapshot the complete private tree and its link inode immediately before
# rejection; merely recreating a symlink with the same target must not pass.
rmdir "$GUARD_HOME/cache"
ln -s config "$GUARD_HOME/cache"
GUARD_LINK_TREE=$(find "$GUARD_HOME" -print | sort)
GUARD_LINK_STAT=$(ls -ldi "$GUARD_HOME" "$GUARD_HOME"/* "$GUARD_HOME/config/keep")
cp "$GUARD_HOME/config/keep" "$WORK_DIR/guard-keep-before"
if OUTPUT=$(HOME="$GUARD_HOME" "$GUARD_RELAY" __private-uninstall-mount-guard \
    "$GUARD_HOME/releases" "$GUARD_HOME/config" "$GUARD_HOME/cache" 2>&1); then
    echo 'mount guard admitted selected symlink root' >&2; exit 1
fi
[ "$OUTPUT" = 'herdr-mobile-relay: uninstall mount guard refused' ] &&
    [ "$(find "$GUARD_HOME" -print | sort)" = "$GUARD_LINK_TREE" ] &&
    [ "$(ls -ldi "$GUARD_HOME" "$GUARD_HOME"/* "$GUARD_HOME/config/keep")" = "$GUARD_LINK_STAT" ] &&
    [ -L "$GUARD_HOME/cache" ] && [ "$(readlink "$GUARD_HOME/cache")" = config ] &&
    cmp -s "$WORK_DIR/guard-keep-before" "$GUARD_HOME/config/keep" || {
    echo 'symlink-root refusal disclosed data or changed fixture' >&2; exit 1;
}
FAULT_CHECKSUMS="$WORK_DIR/fault-checksums"
cp "$RELEASE_DIR/herdr-mobile-relay" "$RELEASE_DIR/native-relay"
cat > "$RELEASE_DIR/herdr-mobile-relay" <<'EOF'
#!/bin/sh
if [ "$1" = json-field ]; then
    case "$HERDR_RELEASE_FAULT:$2:$3" in
        protocol:number:protocol) printf '2\n'; exit 0 ;;
        identity:string:release_directory) printf '%s\n' '/fixture-private-canary'; exit 0 ;;
    esac
fi
if [ "$1" = support ]; then
    case "$HERDR_RELEASE_FAULT" in
        empty) exit 97 ;;
        payload) printf '%s\n' 'fixture-private-canary'; exit 97 ;;
        signal) kill -TERM "$$" ;;
    esac
fi
exec "$(dirname "$0")/native-relay" "$@"
EOF
chmod 755 "$RELEASE_DIR/herdr-mobile-relay"
"$RELEASE_DIR/herdr-mobile-relay" release-manifest \
    "$RELEASE_DIR" "$BINARY_VERSION" "$REVISION" "$HOST_TARGET" >/dev/null
tar -C "$RELEASE_DIR" -czf "$ARCHIVE" .
if command -v sha256sum >/dev/null 2>&1; then
    HASH=$(sha256sum "$ARCHIVE" | awk '{print $1}')
else
    HASH=$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')
fi
printf '%s  %s\n' "$HASH" "${ARCHIVE##*/}" > "$FAULT_CHECKSUMS"
for FAULT in identity protocol empty payload signal; do
    if OUTPUT=$(HERDR_RELEASE_FAULT="$FAULT" check_release \
        "$ARCHIVE" "$FAULT_CHECKSUMS" "$BINARY_VERSION" "$REVISION" "$HOST_TARGET" 2>&1); then
        echo "installed checker accepted $FAULT support fault" >&2; exit 1
    fi
    case "$FAULT" in
        identity|protocol) EXPECTED='installed support fixture protocol or canonical release identity mismatch' ;;
        *) EXPECTED='installed support acquisition failed or disclosed private state' ;;
    esac
    printf '%s\n' "$OUTPUT" | grep -Fq "$EXPECTED" || {
        echo "installed checker did not reach $FAULT rejection" >&2; exit 1;
    }
    if printf '%s\n' "$OUTPUT" | grep -Fq 'fixture-private-canary'; then
        echo 'installed checker disclosed private fixture data' >&2; exit 1
    fi
done
cp "$RELEASE_DIR/native-relay" "$RELEASE_DIR/herdr-mobile-relay"
rm "$RELEASE_DIR/native-relay"
cp "$WORK_DIR/positive.tar.gz" "$ARCHIVE"
mkdir "$WORK_DIR/physical-tmp"
ln -s "$WORK_DIR/physical-tmp" "$WORK_DIR/logical-tmp"
TMPDIR="$WORK_DIR/logical-tmp" \
    check_release \
    "$ARCHIVE" "$CHECKSUMS" "$BINARY_VERSION" "$REVISION" "$HOST_TARGET"

# The installed checker must retain both fixture identity comparisons even
# though release_directory is forbidden in public support output. Exercise the
# packaged exact-scalar extractor with a nested decoy and a mismatched path.
grep -Fq '[ "$FIXTURE_PROTOCOL" = 3 ] && [ "$FIXTURE_RELEASE" = "$RELEASE_DIR" ]' \
    "$REPO_DIR/scripts/check-installed-release.sh" || {
    echo 'installed checker lost private-fixture identity oracle' >&2; exit 1;
}
FIXTURE_JSON="$WORK_DIR/identity-fixture.json"
printf '%s\n' '{"protocol":3,"release_directory":"/fixture/release","nested":{"release_directory":"wrong"}}' > "$FIXTURE_JSON"
IDENTITY=$("$RELEASE_DIR/herdr-mobile-relay" json-field string release_directory < "$FIXTURE_JSON")
[ "$IDENTITY" = /fixture/release ] || {
    echo 'packaged scalar extractor lost exact release identity' >&2; exit 1;
}
printf '%s\n' '{"protocol":3,"release_directory":"/fixture/foreign"}' > "$FIXTURE_JSON"
IDENTITY=$("$RELEASE_DIR/herdr-mobile-relay" json-field string release_directory < "$FIXTURE_JSON")
[ "$IDENTITY" != /fixture/release ] || {
    echo 'foreign fixture identity accepted' >&2; exit 1;
}

# Synthetic altered wrappers are deliberately rebound to EACH manifest and
# checksum: rejection must reach the CLI contract, not an unrelated hash gate.
for CLI_FAULT in zero wrong-status missing-diagnostic activation cli server other ready-claim ready-assignment ready-mapping ready-json; do
    CLI_WRAPPER="$RELEASE_DIR/relay/herdr-mobile-relay-service.sh"
    printf '%s\n' '#!/bin/bash' 'set +x' > "$CLI_WRAPPER"
    CLI_STATUS=69
    CLI_DIAGNOSTIC='Installed-service CLI startup is disabled: unavailable/non-ready; no service was started.'
    case "$CLI_FAULT" in
        zero) CLI_STATUS=0 ;;
        wrong-status) CLI_STATUS=23 ;;
        missing-diagnostic) CLI_DIAGNOSTIC='Unavailable.' ;;
        activation) printf '%s\n' '"$HERDR_RELAY_BIN" tailscale-cli activation-check || :' >> "$CLI_WRAPPER" ;;
        cli) printf '%s\n' '"$HERDR_TAILSCALE_CLI_BIN" status || :' >> "$CLI_WRAPPER" ;;
        server) printf '%s\n' '"$HERDR_RELAY_BIN" serve-installed || :' >> "$CLI_WRAPPER" ;;
        other) printf '%s\n' '"$HERDR_RELAY_BIN" version || :' >> "$CLI_WRAPPER" ;;
        ready-claim) printf '%s\n' 'echo "Service ready."' >> "$CLI_WRAPPER" ;;
        ready-assignment) printf '%s\n' 'echo "ready=true"' >> "$CLI_WRAPPER" ;;
        ready-mapping) printf '%s\n' 'echo "ready: true"' >> "$CLI_WRAPPER" ;;
        ready-json) printf '%s\n' 'echo "{\"ready\":true}"' >> "$CLI_WRAPPER" ;;
    esac
    printf 'printf "%%s\\n" "%s"\nexit %s\n' "$CLI_DIAGNOSTIC" "$CLI_STATUS" >> "$CLI_WRAPPER"
    chmod 755 "$CLI_WRAPPER"
    "$RELEASE_DIR/herdr-mobile-relay" release-manifest \
        "$RELEASE_DIR" "$BINARY_VERSION" "$REVISION" "$HOST_TARGET" >/dev/null
    tar -C "$RELEASE_DIR" -czf "$ARCHIVE" .
    if command -v sha256sum >/dev/null 2>&1; then
        HASH=$(sha256sum "$ARCHIVE" | awk '{print $1}')
    else
        HASH=$(shasum -a 256 "$ARCHIVE" | awk '{print $1}')
    fi
    printf '%s  %s\n' "$HASH" "${ARCHIVE##*/}" > "$CHECKSUMS"
    if OUTPUT=$(check_release "$ARCHIVE" "$CHECKSUMS" "$BINARY_VERSION" "$REVISION" "$HOST_TARGET" 2>&1); then
        echo "installed-release check accepted CLI contract fault: $CLI_FAULT" >&2; exit 1
    fi
    printf '%s\n' "$OUTPUT" | grep -Fq 'extracted CLI service must be unavailable/non-ready (69) with zero command effects'
done
cp "$REPO_DIR/relay/herdr-mobile-relay-service.sh" "$CLI_WRAPPER"
cmp "$REPO_DIR/relay/herdr-mobile-relay-service.sh" "$CLI_WRAPPER" >/dev/null
