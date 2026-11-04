#!/bin/sh
# sshd plus a restart loop for whatever release /opt/app/current points at.
/usr/sbin/sshd -D -e &
runuser -u deploy -- sh -c 'while true; do /opt/app/current/run.sh; sleep 0.5; done' &
wait
