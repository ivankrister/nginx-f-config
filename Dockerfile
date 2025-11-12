FROM nginx:alpine

COPY nginx.edge.http.conf.template /etc/nginx/conf.d/default.conf
