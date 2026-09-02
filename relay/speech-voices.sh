#!/bin/bash
# Manage the neural voices the relay reads responses with. The pinned catalog
# and every download live in the relay binary, which the phone drives too, so
# this script only translates its flags into that subcommand. Voices land in
# the cache directory, which relay updates never touch, so they are downloaded
# once per computer rather than once per release.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

COMMAND="install"
LANGUAGES=""

usage() {
    echo "Usage: $0 [--languages en,fr,de,es,zh] [--missing | --remove]"
    echo "  --missing   List what is not cached yet and exit"
    echo "  --remove    Delete the cached voices for --languages"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --languages)
            [ "$#" -ge 2 ] || { usage >&2; exit 2; }
            LANGUAGES="$2"
            shift 2
            ;;
        --missing)
            COMMAND="missing"
            shift
            ;;
        --remove)
            COMMAND="remove"
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

if ! BINARY="$(relay_binary)"; then
    echo "  Speech voices are downloaded by the relay release, so install it first." >&2
    exit 1
fi

args=(speech-voices "$COMMAND")
if [ -n "$LANGUAGES" ]; then
    args+=(--languages "$LANGUAGES")
fi

exec "$BINARY" "${args[@]}"
