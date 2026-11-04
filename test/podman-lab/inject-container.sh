#!/bin/sh
# Lab "deployment" for the container service: replace vgl-app3 with another
# image tag through the Podman Docker-compatible API, keeping its address.
#   inject-container.sh v2
set -e
API="curl -fsS --unix-socket /var/run/docker.sock"
$API -X POST "http://d/containers/vgl-app3/stop?t=2" >/dev/null || true
$API -X DELETE "http://d/containers/vgl-app3?force=true" >/dev/null || true
$API -X POST -H 'Content-Type: application/json' "http://d/containers/create?name=vgl-app3" -d '{
  "Image": "localhost/vgl-app:'"$1"'", "Hostname": "app3",
  "HostConfig": {"NetworkMode": "vgl-lab"},
  "NetworkingConfig": {"EndpointsConfig": {"vgl-lab": {"IPAMConfig": {"IPv4Address": "10.89.50.13"}}}}
}' >/dev/null
$API -X POST "http://d/containers/vgl-app3/start" >/dev/null
