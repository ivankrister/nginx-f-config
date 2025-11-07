#!/usr/bin/env sh
set -e

TEMPLATE="/etc/nginx/templates/default.conf.template"
TARGET="/etc/nginx/conf.d/default.conf"

generate_upstream_servers() {
    for var in "$@"; do
        value=$(eval "printf '%s' \"\${$var}\"")
        if [ -n "${value}" ]; then
            printf '    server %s:443;\n' "${value}"
        fi
    done
}

if [ -f "${TEMPLATE}" ]; then
    mkdir -p "$(dirname "${TARGET}")"
    # Allow `$` inside the template when needed
    export DOLLAR="$"
    ORYX_UPSTREAM_SERVERS="$(generate_upstream_servers ORYX_SERVER ORYX_SERVER2)"
    if [ -z "${ORYX_UPSTREAM_SERVERS}" ]; then
        echo "Error: no ORYX upstream servers are defined. Set ORYX_SERVER and/or ORYX_SERVER2." >&2
        exit 1
    fi
    PERYA_UPSTREAM_SERVERS="$(generate_upstream_servers PERYA_SERVER)"
    if [ -z "${PERYA_UPSTREAM_SERVERS}" ]; then
        echo "Error: no PERYA upstream server is defined. Set PERYA_SERVER." >&2
        exit 1
    fi
    export ORYX_UPSTREAM_SERVERS
    export PERYA_UPSTREAM_SERVERS
    envsubst < "${TEMPLATE}" > "${TARGET}"
fi

exec "$@"
