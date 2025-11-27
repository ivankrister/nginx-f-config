#!/bin/sh
set -e

TEMPLATE_DIR=/etc/nginx/templates
CONF_DIR=/etc/nginx/conf.d
KEYS_DIR=/etc/nginx/keys

: "${NGINX_SERVER_DEFAULT:=_}"
: "${NGINX_SERVER_APL:=apl.example.com}"
: "${AUTO_RELOAD_SSL:=false}"
: "${GENERATE_DHPARAM:=true}"
: "${DHPARAM_BITS:=2048}"

DEFAULT_SERVER_NAMES="${NGINX_SERVER_DEFAULT//,/ }"
APL_SERVER_NAMES="${NGINX_SERVER_APL//,/ }"

export DEFAULT_SERVER_NAMES APL_SERVER_NAMES

watch_certificates() {
    if [ ! -d "$KEYS_DIR" ]; then
        echo "SSL auto-reload enabled but directory $KEYS_DIR missing"
        return
    fi
    while inotifywait -e close_write,create,move,delete "$KEYS_DIR" >/dev/null 2>&1; do
        echo "Detected certificate change, reloading nginx..."
        nginx -s reload || true
    done
}

generate_dhparam() {
    if [ "${GENERATE_DHPARAM}" != "true" ]; then
        echo "Skipping dhparam generation (GENERATE_DHPARAM=${GENERATE_DHPARAM})"
        return
    fi
    mkdir -p "$KEYS_DIR"
    local dh_file="${KEYS_DIR}/dhparam.pem"
    if [ -f "$dh_file" ]; then
        echo "dhparam already present at ${dh_file}"
        return
    fi
    echo "Generating dhparam (${DHPARAM_BITS} bits) at ${dh_file} ..."
    if ! openssl dhparam -out "$dh_file" "$DHPARAM_BITS" 2>/tmp/dhparam.log; then
        echo "Failed to generate dhparam. Log:"
        cat /tmp/dhparam.log || true
        rm -f "$dh_file"
    fi
}

if [ "${SSL_ENABLE:-false}" = "true" ]; then
    generate_dhparam
    envsubst '$DEFAULT_SERVER_NAMES $APL_SERVER_NAMES' < "${TEMPLATE_DIR}/nginx.edge.https.conf.template" > "${CONF_DIR}/default.conf"
else
    cp "${TEMPLATE_DIR}/nginx.edge.http.conf.template" "${CONF_DIR}/default.conf"
fi

if [ "${AUTO_RELOAD_SSL}" = "true" ]; then
    watch_certificates &
fi

exec nginx -g "daemon off;"
