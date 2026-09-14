FROM caddy:2-alpine

COPY Caddyfile /etc/caddy/Caddyfile
COPY support.js ios-frame.jsx /srv/
# Единственный .dc.html — главная страница (glob, т.к. macOS хранит кириллицу в имени в NFD)
COPY *.dc.html /srv/index.html
