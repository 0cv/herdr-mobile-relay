#!/bin/bash
# CLI-backed installed activation is deliberately unavailable, not a service
# that started or became ready. Do not load config/helpers or select a binary.
set +x
printf '%s\n' 'Installed-service CLI startup is disabled: unavailable/non-ready; no service was started.' >&2
exit 69
