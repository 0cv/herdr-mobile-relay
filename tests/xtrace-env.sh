#!/bin/bash

if [ "${HERDR_SHELL_COVERAGE:-}" = 1 ]; then
    PS4='+HERDR_XTRACE|${BASHPID:-$$}|${BASH_SOURCE[0]:-}|${LINENO}|${?}|'
    set -x
fi
