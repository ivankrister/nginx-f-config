#!/bin/sh
set -e

# Clear nginx cache on startup
echo "Clearing nginx cache..."
rm -rf /var/cache/nginx/* 2>/dev/null || true
rm -rf /tmp/nginx/* 2>/dev/null || true
echo "Nginx cache cleared"

TEMPLATE_DIR=/etc/nginx/templates
CONF_DIR=/etc/nginx/conf.d
KEYS_DIR=/etc/nginx/keys

: "${NGINX_SERVER_DEFAULT:=_}"
: "${NGINX_SERVER_APL:=apl.example.com}"
: "${AUTO_RELOAD_SSL:=false}"

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

if [ "${SSL_ENABLE:-false}" = "true" ]; then
    envsubst '$DEFAULT_SERVER_NAMES $APL_SERVER_NAMES' < "${TEMPLATE_DIR}/nginx.edge.https.conf.template" > "${CONF_DIR}/default.conf"
else
    cp "${TEMPLATE_DIR}/nginx.edge.http.conf.template" "${CONF_DIR}/default.conf"
fi

if [ "${AUTO_RELOAD_SSL}" = "true" ]; then
    watch_certificates &
fi

exec nginx -g "daemon off;"
