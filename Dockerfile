FROM nginx:1.27-alpine

ENV ORYX_SERVER="" \
    ORYX_SERVER2=""

# Provide our template for envsubst processing into conf.d/default.conf
COPY nginx.edge.http.conf.template /etc/nginx/templates/default.conf.template

# Ensure cache directories exist and writable by nginx user
RUN mkdir -p /data/nginx-cache/tmp && \
    chown -R nginx:nginx /data/nginx-cache
