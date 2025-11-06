#!/usr/bin/env sh
set -e

TEMPLATE="/etc/nginx/templates/default.conf.template"
TARGET="/etc/nginx/conf.d/default.conf"

if [ -f "${TEMPLATE}" ]; then
    mkdir -p "$(dirname "${TARGET}")"
    # Allow `$` inside the template when needed
    export DOLLAR="$"
    envsubst < "${TEMPLATE}" > "${TARGET}"
fi

exec "$@"
