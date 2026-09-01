#!/bin/bash
set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-speech-test.XXXXXX")"
trap 'rm -rf "$WORK_DIR"' EXIT INT TERM

export XDG_CACHE_HOME="$WORK_DIR/cache"
export HERDR_PIPER_VOICE_BASE_URL="https://voices.test"
export HERDR_PIPER_RUNTIME_BASE_URL="https://runtime.test"
SPEECH_DIR="$XDG_CACHE_HOME/herdr-mobile-relay/speech"
REQUESTS="$WORK_DIR/requests.txt"

# A stub curl serves the published bytes from a local fixture tree and records
# every URL, so the test proves what would be downloaded without the network.
mkdir -p "$WORK_DIR/bin" "$WORK_DIR/serve"
cat > "$WORK_DIR/bin/curl" <<'STUB'
#!/bin/bash
url=""
output=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        --output) output="$2"; shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
echo "$url" >> "$REQUESTS"
name="${url##*/}"
[ -f "$SERVE_DIR/$name" ] || exit 22
cp "$SERVE_DIR/$name" "$output"
STUB
chmod +x "$WORK_DIR/bin/curl"
# A host-installed engine would satisfy the runtime check, so the script sees
# only the stub and system tools.
export REQUESTS SERVE_DIR="$WORK_DIR/serve"
export PATH="$WORK_DIR/bin:/usr/bin:/bin"

digest() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    else
        shasum -a 256 "$1" | cut -d' ' -f1
    fi
}

# Publish the exact bytes the script pins, so its digests decide the outcome.
publish() {
    local name="$1" body="$2"
    printf '%s' "$body" > "$WORK_DIR/serve/$name"
    digest "$WORK_DIR/serve/$name"
}

en_model_digest="$(publish en_US-lessac-medium.onnx 'english model')"
en_config_digest="$(publish en_US-lessac-medium.onnx.json '{"english":true}')"
fr_model_digest="$(publish fr_FR-siwis-medium.onnx 'french model')"
fr_config_digest="$(publish fr_FR-siwis-medium.onnx.json '{"french":true}')"

# The engine tarball unpacks to piper/piper, which is what the relay runs.
mkdir -p "$WORK_DIR/stage/piper"
cat > "$WORK_DIR/stage/piper/piper" <<'ENGINE'
#!/bin/sh
exit 0
ENGINE
chmod +x "$WORK_DIR/stage/piper/piper"
runtime_asset="piper_linux_x86_64.tar.gz"
case "$(uname -s):$(uname -m)" in
    Linux:x86_64|Linux:amd64) runtime_asset="piper_linux_x86_64.tar.gz" ;;
    Linux:aarch64|Linux:arm64) runtime_asset="piper_linux_aarch64.tar.gz" ;;
    Darwin:*) runtime_asset="piper_macos_x64.tar.gz" ;;
esac
tar -czf "$WORK_DIR/serve/$runtime_asset" -C "$WORK_DIR/stage" piper
runtime_digest="$(digest "$WORK_DIR/serve/$runtime_asset")"

# Point the script's pinned digests at the fixture bytes.
sed \
    -e "s/5efe09e69902187827af646e1a6e9d269dee769f9877d17b16b1b46eeaaf019f/$en_model_digest/" \
    -e "s/efe19c417bed055f2d69908248c6ba650fa135bc868b0e6abb3da181dab690a0/$en_config_digest/" \
    -e "s/641d1ab097da2b81128c076810edb052b385decc8be3381814802a64a73baf99/$fr_model_digest/" \
    -e "s/39479916c2db192b5ac9764daddd0c744d83e023ad890c6976c0633ae4df8959/$fr_config_digest/" \
    -e "s/a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992/$runtime_digest/" \
    -e "s/fea0fd2d87c54dbc7078d0f878289f404bd4d6eea6e7444a77835d1537ab88eb/$runtime_digest/" \
    -e "s/ced85c0a3df13945b1e623b878a48fdc2854d5c485b4b67f62857cf551deaf8b/$runtime_digest/" \
    "$REPO_DIR/relay/speech-voices.sh" > "$WORK_DIR/speech-voices.sh"
chmod +x "$WORK_DIR/speech-voices.sh"
cp "$REPO_DIR/relay/common.sh" "$WORK_DIR/common.sh"
SCRIPT="$WORK_DIR/speech-voices.sh"

missing="$("$SCRIPT" --missing --languages en,fr)"
test "$missing" = "runtime
en
fr"

"$SCRIPT" --languages en,fr > "$WORK_DIR/first.log"
test -x "$SPEECH_DIR/runtime/piper/piper"
test "$(cat "$SPEECH_DIR/voices/en_US-lessac-medium.onnx")" = "english model"
test "$(cat "$SPEECH_DIR/voices/fr_FR-siwis-medium.onnx")" = "french model"
grep -q "voices.test/fr/fr_FR/siwis/medium/fr_FR-siwis-medium.onnx" "$REQUESTS"
grep -q "runtime.test/$runtime_asset" "$REQUESTS"

# A second run downloads nothing: the cache is what survives relay updates.
: > "$REQUESTS"
"$SCRIPT" --languages en,fr > "$WORK_DIR/second.log"
test ! -s "$REQUESTS"
grep -q "already cached" "$WORK_DIR/second.log"
test -z "$("$SCRIPT" --missing --languages en,fr)"

# Languages nobody asked for stay out of the cache and out of the download.
test "$("$SCRIPT" --missing --languages de)" = "de"
if "$SCRIPT" --languages tlh 2>/dev/null; then
    echo "unknown language was accepted" >&2
    exit 1
fi

# Corrupted bytes are rejected, and no partial file is left behind.
printf 'tampered' > "$WORK_DIR/serve/de_DE-thorsten-medium.onnx"
if "$SCRIPT" --languages de >/dev/null 2>"$WORK_DIR/mismatch.log"; then
    echo "checksum mismatch was accepted" >&2
    exit 1
fi
grep -q "Checksum mismatch" "$WORK_DIR/mismatch.log"
test ! -e "$SPEECH_DIR/voices/de_DE-thorsten-medium.onnx"
test ! -e "$SPEECH_DIR/voices/de_DE-thorsten-medium.onnx.part"

# A truncated cached model is replaced instead of being trusted.
printf 'truncated' > "$SPEECH_DIR/voices/en_US-lessac-medium.onnx"
test "$("$SCRIPT" --missing --languages en)" = "en"
: > "$REQUESTS"
"$SCRIPT" --languages en > /dev/null
test "$(cat "$SPEECH_DIR/voices/en_US-lessac-medium.onnx")" = "english model"
grep -q "voices.test/en/en_US/lessac/medium/en_US-lessac-medium.onnx" "$REQUESTS"

echo "speech voice cache tests passed"
