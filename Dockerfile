ARG ARCH=
FROM ${ARCH}openresty/openresty:1.25.3.2-alpine AS dist

ENV ORYX_SERVER="" \
    ORYX_SERVER2=""

# Required for envsubst in the entrypoint
RUN apk add --no-cache gettext

RUN if ! id nginx >/dev/null 2>&1; then \
        addgroup -S nginx && \
        adduser -S -G nginx nginx; \
    fi

# Provide our template for envsubst processing into conf.d/default.conf
COPY nginx.edge.http.conf.template /etc/nginx/templates/default.conf.template
COPY lua /etc/nginx/lua
COPY docker-entrypoint.sh /docker-entrypoint.sh

RUN chmod +x /docker-entrypoint.sh && \
    mkdir -p /data/nginx-cache/tmp && \
    chown -R nginx:nginx /data/nginx-cache

ENTRYPOINT ["/docker-entrypoint.sh"]
CMD ["openresty", "-g", "daemon off;"]
