#!/bin/sh
set -e

TEMPLATE_DIR=/etc/nginx/templates
CONF_DIR=/etc/nginx/conf.d

if [ "${SSL_ENABLE:-false}" = "true" ]; then
    cp "${TEMPLATE_DIR}/nginx.edge.https.conf.template" "${CONF_DIR}/default.conf"
else
    cp "${TEMPLATE_DIR}/nginx.edge.http.conf.template" "${CONF_DIR}/default.conf"
fi

exec nginx -g "daemon off;"
