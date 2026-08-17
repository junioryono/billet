#!/bin/sh
set -eu

case "${1:-}" in
    -version)
        printf '%s\n' 'v1.4.4'
        ;;
    version)
        if [ "${2:-}" = -json ]; then
            test "${CHECKPOINT_DISABLE:-}" = 1
            printf '%s\n' '{"terraform_version":"1.12.0"}'
        else
            printf '%s\n' 'v2.7.0'
        fi
        ;;
    *)
        exit 2
        ;;
esac
