#!/bin/sh
# Lab "deployment": point /opt/app/current at another release and restart.
#   deploy.sh v2      (the bad version)
set -e
[ -d "/opt/app/releases/$1" ] || { echo "no release $1" >&2; exit 1; }
ln -sfn "/opt/app/releases/$1" /opt/app/current.tmp
mv -Tf /opt/app/current.tmp /opt/app/current
pkill -x fakeapp || true
