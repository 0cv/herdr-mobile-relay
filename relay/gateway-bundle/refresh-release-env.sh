#!/bin/sh
# Refresh generated release identity without interpreting operator .env values.
set -eu
cd "$(dirname "$0")"
test -f .env && test -r .env
temporary="$(mktemp .env.release.XXXXXX)"
trap 'rm -f "$temporary"' EXIT HUP INT TERM
awk '
    /^HERDR_GATEWAY_VERSION=/ { next }
    /^HERDR_GATEWAY_REVISION=/ { next }
    /^HERDR_GATEWAY_IMAGE=/ { next }
    /^HERDR_CADDY_IMAGE=/ { next }
    { print }
    END {
        print "HERDR_GATEWAY_VERSION=@GATEWAY_VERSION@"
        print "HERDR_GATEWAY_REVISION=@GATEWAY_REVISION@"
        print "HERDR_GATEWAY_IMAGE=herdr-gateway:local"
        print "HERDR_CADDY_IMAGE=caddy:2-alpine"
    }
' .env > "$temporary"
chmod 600 "$temporary"
mv "$temporary" .env
