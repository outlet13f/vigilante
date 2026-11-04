#!/bin/sh
/usr/sbin/sshd -D -e &
nginx
haproxy -db -f /etc/haproxy/haproxy.cfg &
envoy -c /etc/envoy/envoy.yaml --log-level warn &
wait
