#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
exec bun --no-env-file --no-install tests/pi-scroll-qualification.mjs
