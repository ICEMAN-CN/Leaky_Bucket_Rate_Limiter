FROM haproxy:3.2.5-alpine
COPY ops/haproxy.cfg /usr/local/etc/haproxy/haproxy.cfg
RUN haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg
