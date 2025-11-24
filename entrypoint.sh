#!/bin/sh
set -e

TEMPLATE_DIR=/etc/nginx/templates
CONF_DIR=/etc/nginx/conf.d

: "${NGINX_SERVER_DEFAULT:=_}"
: "${NGINX_SERVER_APL:=apl.example.com}"

DEFAULT_SERVER_NAMES="${NGINX_SERVER_DEFAULT//,/ }"
APL_SERVER_NAMES="${NGINX_SERVER_APL//,/ }"

export DEFAULT_SERVER_NAMES APL_SERVER_NAMES

if [ "${SSL_ENABLE:-false}" = "true" ]; then
    envsubst '$DEFAULT_SERVER_NAMES $APL_SERVER_NAMES' < "${TEMPLATE_DIR}/nginx.edge.https.conf.template" > "${CONF_DIR}/default.conf"
else
    cp "${TEMPLATE_DIR}/nginx.edge.http.conf.template" "${CONF_DIR}/default.conf"
fi

exec nginx -g "daemon off;"
