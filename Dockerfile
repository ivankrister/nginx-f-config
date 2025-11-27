FROM nginx:alpine

RUN apk add --no-cache gettext inotify-tools

COPY nginx.edge.http.conf.template /etc/nginx/templates/nginx.edge.http.conf.template
COPY nginx.edge.https.conf.template /etc/nginx/templates/nginx.edge.https.conf.template
COPY entrypoint.sh /entrypoint.sh

RUN chmod +x /entrypoint.sh

CMD ["/entrypoint.sh"]
