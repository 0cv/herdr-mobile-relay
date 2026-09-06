#!/bin/bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-install-test.coverage.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT

sed '$d' "$ROOT_DIR/install.sh" > "$WORK_DIR/install-functions.sh"
# shellcheck source=/dev/null
. "$WORK_DIR/install-functions.sh"

ARCHIVE_NAME=herdr-mobile-relay_1.2.3_darwin_arm64.tar.gz
REVISION=0123456789abcdef0123456789abcdef01234567

if offline_release_requested; then
    echo "offline_release_requested accepted empty inputs" >&2
    exit 1
fi
offline_release_requested || true
(HERDR_RELEASE_ARCHIVE=archive offline_release_requested)
(HERDR_RELEASE_CHECKSUMS=checksums offline_release_requested)
(HERDR_EXPECTED_REVISION=revision offline_release_requested)
(HERDR_EXPECTED_ARCHIVE_SHA256=digest offline_release_requested)

detect_os() { printf '%s\n' darwin; }
detect_arch() { printf '%s\n' arm64; }
resolve_tag_revision() { printf '%s\n' "${MOCK_TAG_REVISION:-$REVISION}"; }
resolve_asset_url() {
    [ "${MOCK_MISSING_ASSET:-}" != "$2" ] || return 0
    printf 'mock://%s\n' "$2"
}
fetch_json() {
    case "$1" in
        */commits/*)
            [ "${MOCK_FETCH_JSON_FAILURE:-}" != commit ] || return 1
            printf '{"sha":"%s"}\n' "$REVISION"
            ;;
        *)
            [ "${MOCK_FETCH_JSON_FAILURE:-}" != release ] || return 1
            printf '{"assets":[]}\n'
            ;;
    esac
}
fetch() {
    if [ "${MOCK_VERIFY_FAILURE:-}" = 1 ]; then
        case "$1" in
            *checksums.txt) cp "$VERIFY_CHECKSUMS" "$2" ;;
            *) cp "$VERIFY_ARCHIVE" "$2" ;;
        esac
        return
    fi
    case "$1" in
        *checksums.txt) kind=checksums ;;
        *) kind=archive ;;
    esac
    [ "${MOCK_FETCH_FAILURE:-}" != "$kind" ] || return 1
    printf '%s\n' "$kind" > "$2"
}

expect_main_failure() {
    label=$1
    expected=$2
    log="$WORK_DIR/main-$label.log"
    if (main 1.2.3) >"$log" 2>&1; then
        echo "installer unexpectedly completed $label" >&2
        exit 1
    fi
    grep -F "$expected" "$log" >/dev/null || {
        echo "installer $label did not reach expected failure: $expected" >&2
        exit 1
    }
}

HERDR_RELEASE_ARCHIVE=archive expect_main_failure offline-checksums "offline install requires archive, checksums, exact revision, and independent archive SHA-256"
HERDR_RELEASE_ARCHIVE=archive HERDR_RELEASE_CHECKSUMS=checksums expect_main_failure offline-revision "offline install requires archive, checksums, exact revision, and independent archive SHA-256"
HERDR_RELEASE_ARCHIVE=archive HERDR_RELEASE_CHECKSUMS=checksums HERDR_EXPECTED_REVISION="$REVISION" expect_main_failure offline-digest "offline install requires archive, checksums, exact revision, and independent archive SHA-256"
HERDR_RELEASE_CHECKSUMS=checksums HERDR_EXPECTED_REVISION="$REVISION" HERDR_EXPECTED_ARCHIVE_SHA256=digest expect_main_failure offline-archive "offline install requires archive, checksums, exact revision, and independent archive SHA-256"
HERDR_RELEASE_BASE_URL=https://attacker.example.invalid/releases expect_main_failure release-base-url "HERDR_RELEASE_BASE_URL is not accepted"
expect_main_failure public "checksums.txt must contain one exact entry"
GH_TOKEN=coverage-sentinel expect_main_failure private "checksums.txt must contain one exact entry"
MOCK_FETCH_JSON_FAILURE=commit expect_main_failure commit-api "could not resolve release tag"
MOCK_TAG_REVISION=bad expect_main_failure invalid-tag "release tag did not resolve"
GH_TOKEN=coverage-sentinel MOCK_FETCH_JSON_FAILURE=release expect_main_failure release-api "could not fetch release metadata"
GH_TOKEN=coverage-sentinel MOCK_MISSING_ASSET="$ARCHIVE_NAME" expect_main_failure missing-archive-url "release has no asset named $ARCHIVE_NAME"
GH_TOKEN=coverage-sentinel MOCK_MISSING_ASSET=checksums.txt expect_main_failure missing-checksums-url "release has no asset named checksums.txt"
GH_TOKEN=coverage-sentinel MOCK_FETCH_FAILURE=checksums expect_main_failure private-checksums "required checksums.txt download failed"
GH_TOKEN=coverage-sentinel MOCK_FETCH_FAILURE=archive expect_main_failure private-archive "release archive download failed"
MOCK_FETCH_FAILURE=checksums expect_main_failure public-checksums "required checksums.txt download failed"
MOCK_FETCH_FAILURE=archive expect_main_failure public-archive "release archive download failed"

VERIFY_RELEASE="$WORK_DIR/verify-release"
VERIFY_ARCHIVE="$WORK_DIR/$ARCHIVE_NAME"
VERIFY_CHECKSUMS="$WORK_DIR/verify-checksums.txt"
VERIFY_TAR_RECORD="$WORK_DIR/verify-tar-environment"
VERIFY_EXEC_RECORD="$WORK_DIR/verify-exec-environment"
mkdir -p "$VERIFY_RELEASE"
cat > "$VERIFY_RELEASE/herdr-mobile-relay" <<'SH'
#!/bin/sh
leaked=
[ -z "${GH_TOKEN+x}" ] || leaked="${leaked}GH_TOKEN "
[ -z "${GITHUB_TOKEN+x}" ] || leaked="${leaked}GITHUB_TOKEN "
[ -z "${HERDR_GITHUB_TOKEN_FILE+x}" ] || leaked="${leaked}HERDR_GITHUB_TOKEN_FILE "
printf '%s\n' "${leaked:-clean}" > "$VERIFY_EXEC_RECORD"
exit 1
SH
chmod 700 "$VERIFY_RELEASE/herdr-mobile-relay"
printf '%s\n' '{"version":"1.2.3"}' > "$VERIFY_RELEASE/release-manifest.json"
tar -C "$VERIFY_RELEASE" -czf "$VERIFY_ARCHIVE" .
printf '%s  %s\n' "$(sha256_file "$VERIFY_ARCHIVE")" "$ARCHIVE_NAME" > "$VERIFY_CHECKSUMS"
export VERIFY_ARCHIVE VERIFY_CHECKSUMS VERIFY_TAR_RECORD VERIFY_EXEC_RECORD
tar() {
    leaked=
    [ -z "${GH_TOKEN+x}" ] || leaked="${leaked}GH_TOKEN "
    [ -z "${GITHUB_TOKEN+x}" ] || leaked="${leaked}GITHUB_TOKEN "
    [ -z "${HERDR_GITHUB_TOKEN_FILE+x}" ] || leaked="${leaked}HERDR_GITHUB_TOKEN_FILE "
    printf '%s\n' "${leaked:-clean}" > "$VERIFY_TAR_RECORD"
    command tar "$@"
}

VERIFY_SHA=$(sha256_file "$VERIFY_ARCHIVE")
BAD_REVISION="A${REVISION#?}"
BAD_SHA="A${VERIFY_SHA#?}"
WRONG_ARCHIVE="$WORK_DIR/wrong-name.tar.gz"
ARCHIVE_LINK="$WORK_DIR/$ARCHIVE_NAME.link"
CHECKSUMS_LINK="$WORK_DIR/checksums-link.txt"
command cp "$VERIFY_ARCHIVE" "$WRONG_ARCHIVE"
ln -s "$VERIFY_ARCHIVE" "$ARCHIVE_LINK"
ln -s "$VERIFY_CHECKSUMS" "$CHECKSUMS_LINK"

cp() {
    case "${MOCK_OFFLINE_COPY_FAILURE:-}:$1" in
        archive:"$VERIFY_ARCHIVE"|checksums:"$VERIFY_CHECKSUMS") return 1 ;;
    esac
    command cp "$@"
    if [ "${MOCK_OFFLINE_STAGE_MUTATION:-}" = 1 ] && [ "$1" = "$VERIFY_ARCHIVE" ]; then
        printf 'changed during staging\n' >> "$2"
    fi
}

expect_offline_failure() {
    HERDR_RELEASE_ARCHIVE="$3" HERDR_RELEASE_CHECKSUMS="$4" HERDR_EXPECTED_REVISION="$5" HERDR_EXPECTED_ARCHIVE_SHA256="$6" expect_main_failure "$1" "$2"
}

expect_offline_failure relative-archive "offline release archive and checksums paths must be absolute" relative-archive "$VERIFY_CHECKSUMS" "$REVISION" "$VERIFY_SHA"
expect_offline_failure relative-checksums "offline release archive and checksums paths must be absolute" "$VERIFY_ARCHIVE" relative-checksums "$REVISION" "$VERIFY_SHA"
expect_offline_failure missing-archive "offline release archive must be a regular non-symlink file" "$WORK_DIR/missing-archive" "$VERIFY_CHECKSUMS" "$REVISION" "$VERIFY_SHA"
expect_offline_failure symlink-archive "offline release archive must be a regular non-symlink file" "$ARCHIVE_LINK" "$VERIFY_CHECKSUMS" "$REVISION" "$VERIFY_SHA"
expect_offline_failure missing-checksums "offline release checksums must be a regular non-symlink file" "$VERIFY_ARCHIVE" "$WORK_DIR/missing-checksums" "$REVISION" "$VERIFY_SHA"
expect_offline_failure symlink-checksums "offline release checksums must be a regular non-symlink file" "$VERIFY_ARCHIVE" "$CHECKSUMS_LINK" "$REVISION" "$VERIFY_SHA"
expect_offline_failure short-revision "offline expected revision must be an exact lowercase commit" "$VERIFY_ARCHIVE" "$VERIFY_CHECKSUMS" short "$VERIFY_SHA"
expect_offline_failure invalid-revision "offline expected revision must be an exact lowercase commit" "$VERIFY_ARCHIVE" "$VERIFY_CHECKSUMS" "$BAD_REVISION" "$VERIFY_SHA"
expect_offline_failure short-sha "offline expected archive SHA-256 must be exact lowercase hex" "$VERIFY_ARCHIVE" "$VERIFY_CHECKSUMS" "$REVISION" short
expect_offline_failure invalid-sha "offline expected archive SHA-256 must be exact lowercase hex" "$VERIFY_ARCHIVE" "$VERIFY_CHECKSUMS" "$REVISION" "$BAD_SHA"
expect_offline_failure wrong-name "offline release archive name does not match version and native target" "$WRONG_ARCHIVE" "$VERIFY_CHECKSUMS" "$REVISION" "$(sha256_file "$WRONG_ARCHIVE")"
expect_offline_failure wrong-independent-sha "offline release archive does not match independent expected SHA-256" "$VERIFY_ARCHIVE" "$VERIFY_CHECKSUMS" "$REVISION" 0000000000000000000000000000000000000000000000000000000000000000
MOCK_OFFLINE_COPY_FAILURE=archive expect_offline_failure copy-archive "could not stage offline release archive" "$VERIFY_ARCHIVE" "$VERIFY_CHECKSUMS" "$REVISION" "$VERIFY_SHA"
MOCK_OFFLINE_COPY_FAILURE=checksums expect_offline_failure copy-checksums "could not stage offline release checksums" "$VERIFY_ARCHIVE" "$VERIFY_CHECKSUMS" "$REVISION" "$VERIFY_SHA"
MOCK_OFFLINE_STAGE_MUTATION=1 expect_offline_failure staging-race "offline release archive changed while being staged" "$VERIFY_ARCHIVE" "$VERIFY_CHECKSUMS" "$REVISION" "$VERIFY_SHA"
expect_offline_failure offline-verify-release "release verification failed" "$VERIFY_ARCHIVE" "$VERIFY_CHECKSUMS" "$REVISION" "$VERIFY_SHA"

GH_TOKEN=private-fetch-token \
    GITHUB_TOKEN=workflow-token \
    HERDR_GITHUB_TOKEN_FILE="$WORK_DIR/github-token" \
    MOCK_VERIFY_FAILURE=1 \
    expect_main_failure verify-release "release verification failed"
for record in "$VERIFY_TAR_RECORD" "$VERIFY_EXEC_RECORD"; do
    if [ "$(cat "$record")" != clean ]; then
        echo "downloaded release handling inherited GitHub credentials: $(cat "$record")" >&2
        exit 1
    fi
done

printf 'changed shell install tests passed\n'
