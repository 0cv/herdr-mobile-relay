#!/bin/bash
# Download the neural voices the relay reads responses with. Everything lands
# in the cache directory, which relay updates never touch, so the voices are
# downloaded once per computer rather than once per release.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

SPEECH_DIR="$(relay_speech_dir)"
VOICE_DIR="$SPEECH_DIR/voices"
RUNTIME_DIR="$SPEECH_DIR/runtime"
VOICE_BASE_URL="${HERDR_PIPER_VOICE_BASE_URL:-https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0}"
RUNTIME_BASE_URL="${HERDR_PIPER_RUNTIME_BASE_URL:-https://github.com/rhasspy/piper/releases/download/2023.11.14-2}"
LANGUAGES="en,fr,de,es,zh"
MODE="install"
TEMP_DIR=""

cleanup() {
    if [ -n "$TEMP_DIR" ] && [ -d "$TEMP_DIR" ]; then
        rm -rf "$TEMP_DIR"
    fi
}
trap cleanup EXIT

usage() {
    echo "Usage: $0 [--languages en,fr,de,es,zh] [--missing]"
    echo "  --missing   List what is not cached yet and exit"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --languages)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            LANGUAGES="$2"
            shift 2
            ;;
        --missing)
            MODE="missing"
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            usage >&2
            exit 2
            ;;
    esac
done

# Each voice is one published Piper model plus its sidecar config, pinned to
# the revision the digests below were taken from.
voice_files() {
    case "$1" in
        en) echo "en/en_US/lessac/medium en_US-lessac-medium 5efe09e69902187827af646e1a6e9d269dee769f9877d17b16b1b46eeaaf019f efe19c417bed055f2d69908248c6ba650fa135bc868b0e6abb3da181dab690a0" ;;
        fr) echo "fr/fr_FR/siwis/medium fr_FR-siwis-medium 641d1ab097da2b81128c076810edb052b385decc8be3381814802a64a73baf99 39479916c2db192b5ac9764daddd0c744d83e023ad890c6976c0633ae4df8959" ;;
        de) echo "de/de_DE/thorsten/medium de_DE-thorsten-medium 7e64762d8e5118bb578f2eea6207e1a35a8e0c30595010b666f983fc87bb7819 974adee790533adb273a1ac88f49027d2a1b8f0f2cf4905954a4791e79264e85" ;;
        es) echo "es/es_ES/davefx/medium es_ES-davefx-medium 6658b03b1a6c316ee4c265a9896abc1393353c2d9e1bca7d66c2c442e222a917 0e0dda87c732f6f38771ff274a6380d9252f327dca77aa2963d5fbdf9ec54842" ;;
        zh) echo "zh/zh_CN/huayan/medium zh_CN-huayan-medium 9929917bf8cabb26fd528ea44d3a6699c11e87317a14765312420be230be0f3d d521dc45504a8ccc99e325822b35946dd701840bfb07e3dbb31a40929ed6a82b" ;;
        *) return 1 ;;
    esac
}

# Apple ships no arm64 build of the standalone engine, so Apple Silicon runs
# the Intel one through Rosetta.
runtime_asset() {
    case "$(uname -s):$(uname -m)" in
        Linux:x86_64|Linux:amd64) echo "piper_linux_x86_64.tar.gz a50cb45f355b7af1f6d758c1b360717877ba0a398cc8cbe6d2a7a3a26e225992" ;;
        Linux:aarch64|Linux:arm64) echo "piper_linux_aarch64.tar.gz fea0fd2d87c54dbc7078d0f878289f404bd4d6eea6e7444a77835d1537ab88eb" ;;
        Darwin:*) echo "piper_macos_x64.tar.gz ced85c0a3df13945b1e623b878a48fdc2854d5c485b4b67f62857cf551deaf8b" ;;
        *) return 1 ;;
    esac
}

file_digest() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    else
        shasum -a 256 "$1" | cut -d' ' -f1
    fi
}

cached() {
    [ -f "$1" ] && [ "$(file_digest "$1")" = "$2" ]
}

download() {
    local url="$1" destination="$2" digest="$3" actual
    curl --fail --location --silent --show-error "$url" --output "$destination.part"
    actual="$(file_digest "$destination.part")"
    if [ "$actual" != "$digest" ]; then
        rm -f "$destination.part"
        echo "Checksum mismatch for $url" >&2
        return 1
    fi
    mv -f "$destination.part" "$destination"
}

runtime_installed() {
    command -v piper >/dev/null 2>&1 || [ -x "$RUNTIME_DIR/piper/piper" ]
}

voice_installed() {
    local fields name
    fields="$(voice_files "$1")"
    name="$(echo "$fields" | cut -d' ' -f2)"
    cached "$VOICE_DIR/$name.onnx" "$(echo "$fields" | cut -d' ' -f3)" &&
        cached "$VOICE_DIR/$name.onnx.json" "$(echo "$fields" | cut -d' ' -f4)"
}

requested_languages() {
    echo "$LANGUAGES" | tr ',' ' '
}

for language in $(requested_languages); do
    if ! voice_files "$language" >/dev/null; then
        echo "Unknown speech language: $language" >&2
        exit 2
    fi
done

missing=()
runtime_installed || missing+=(runtime)
for language in $(requested_languages); do
    voice_installed "$language" || missing+=("$language")
done

if [ "$MODE" = "missing" ]; then
    for item in ${missing[@]+"${missing[@]}"}; do
        echo "$item"
    done
    exit 0
fi

if [ "${#missing[@]}" -eq 0 ]; then
    echo "Speech voices are already cached in $SPEECH_DIR"
    exit 0
fi

command -v curl >/dev/null 2>&1 || { echo "curl is required to download speech voices" >&2; exit 1; }
mkdir -p "$VOICE_DIR"
TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/herdr-speech.XXXXXX")"

for item in "${missing[@]}"; do
    if [ "$item" = "runtime" ]; then
        if ! asset="$(runtime_asset)"; then
            echo "No prebuilt speech engine is published for $(uname -s) $(uname -m)."
            echo "Install piper manually, or the relay falls back to its system speech engine."
            continue
        fi
        name="$(echo "$asset" | cut -d' ' -f1)"
        echo "Downloading the speech engine ($name)..."
        download "$RUNTIME_BASE_URL/$name" "$TEMP_DIR/$name" "$(echo "$asset" | cut -d' ' -f2)"
        rm -rf "$TEMP_DIR/extract"
        mkdir -p "$TEMP_DIR/extract" "$RUNTIME_DIR"
        tar -xzf "$TEMP_DIR/$name" -C "$TEMP_DIR/extract"
        rm -rf "$RUNTIME_DIR/piper.previous"
        [ ! -d "$RUNTIME_DIR/piper" ] || mv "$RUNTIME_DIR/piper" "$RUNTIME_DIR/piper.previous"
        mv "$TEMP_DIR/extract/piper" "$RUNTIME_DIR/piper"
        rm -rf "$RUNTIME_DIR/piper.previous"
        continue
    fi
    fields="$(voice_files "$item")"
    path="$(echo "$fields" | cut -d' ' -f1)"
    name="$(echo "$fields" | cut -d' ' -f2)"
    echo "Downloading the $item voice ($name, about 63 MB)..."
    download "$VOICE_BASE_URL/$path/$name.onnx" "$VOICE_DIR/$name.onnx" "$(echo "$fields" | cut -d' ' -f3)"
    download "$VOICE_BASE_URL/$path/$name.onnx.json" "$VOICE_DIR/$name.onnx.json" "$(echo "$fields" | cut -d' ' -f4)"
done

if [ -x "$RUNTIME_DIR/piper/piper" ] && ! "$RUNTIME_DIR/piper/piper" --help >/dev/null 2>&1; then
    echo "The downloaded speech engine cannot run on this computer." >&2
    if [ "$(uname -s)" = "Darwin" ]; then
        echo "Apple Silicon runs it through Rosetta: softwareupdate --install-rosetta" >&2
    fi
    exit 1
fi

echo "Speech voices are cached in $SPEECH_DIR"
