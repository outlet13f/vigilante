#!/usr/bin/env bash
# Local Podman lab for Vigilante: real Nginx, HAProxy, Envoy, SSH hosts,
# PostgreSQL, etcd (gRPC health) and the Podman API. Test plan: README.md.
#
#   ./lab.sh build      Linux binaries, SSH key, images
#   ./lab.sh up         network and containers
#   ./lab.sh status     containers and quick health
#   ./lab.sh vig ARGS   run vigilante in the controller (cwd /lab)
#   ./lab.sh sh         shell in the controller
#   ./lab.sh down       remove containers and network (images stay)
#   ./lab.sh clean      down, then remove the lab images and .run/
set -euo pipefail
cd "$(dirname "$0")"

NET=vgl-lab
SUBNET=10.89.50.0/24
RUN=.run
PG_IMAGE=docker.io/library/postgres:16-alpine
ETCD_IMAGE=quay.io/coreos/etcd:v3.5.17
CONTAINERS=(vgl-ctl vgl-lb vgl-app1 vgl-app2 vgl-app3 vgl-pg vgl-etcd)

arch() { podman info --format '{{.Host.Arch}}'; }
# The Podman API socket as seen inside the Podman machine (or on Linux, the host).
socket() { podman info --format '{{.Host.RemoteSocket.Path}}' | sed 's#^unix://##'; }

build() {
  mkdir -p "$RUN/bin"
  [ -f "$RUN/id_ed25519" ] || ssh-keygen -q -t ed25519 -N '' -C vgl-lab -f "$RUN/id_ed25519"
  local a; a=$(arch)
  (cd ../.. && GOOS=linux GOARCH="$a" CGO_ENABLED=0 go build -o "test/podman-lab/$RUN/bin/vigilante" ./cmd/vigilante \
            && GOOS=linux GOARCH="$a" CGO_ENABLED=0 go build -o "test/podman-lab/$RUN/bin/fakeapp" ./cmd/fakeapp)
  podman build -q -t localhost/vgl-target:lab -f images/target/Containerfile .
  podman build -q -t localhost/vgl-lb:lab     -f images/lb/Containerfile .
  podman build -q -t localhost/vgl-ctl:lab    -f images/ctl/Containerfile .
  for v in v1 v1.1 v2; do
    podman build -q -t "localhost/vgl-app:$v" --build-arg VERSION="$v" -f images/app/Containerfile .
  done
  podman pull -q "$PG_IMAGE" "$ETCD_IMAGE" >/dev/null
}

up() {
  podman network exists "$NET" || podman network create --subnet "$SUBNET" "$NET" >/dev/null
  run() { local name=$1 ip=$2; shift 2; podman run -d --name "$name" --network "$NET" --ip "$ip" "$@" >/dev/null; echo "started $name ($ip)"; }
  run vgl-lb   10.89.50.10 --hostname lb   localhost/vgl-lb:lab
  run vgl-app1 10.89.50.11 --hostname app1 localhost/vgl-target:lab
  run vgl-app2 10.89.50.12 --hostname app2 localhost/vgl-target:lab
  run vgl-app3 10.89.50.13 --hostname app3 localhost/vgl-app:v1
  run vgl-pg   10.89.50.20 -e POSTGRES_PASSWORD=lab "$PG_IMAGE" -c max_connections=30
  run vgl-etcd 10.89.50.22 "$ETCD_IMAGE" etcd --name lab \
      --listen-client-urls http://0.0.0.0:2379 --advertise-client-urls http://10.89.50.22:2379
  # label=disable: the controller talks to the Podman socket and reads /lab.
  run vgl-ctl  10.89.50.2 --security-opt label=disable \
      -v "$PWD:/lab" -v "$(socket):/var/run/docker.sock" localhost/vgl-ctl:lab
}

status() {
  podman ps -a --filter name=vgl- --format '{{.Names}}\t{{.Status}}'
  echo '--- from the controller'
  podman exec vgl-ctl sh -c '
    for u in http://10.89.50.11:18080/health http://10.89.50.12:18080/health http://10.89.50.13:18080/health \
             http://10.89.50.10:8080/health http://10.89.50.10:8081/health http://10.89.50.10:8082/health; do
      printf "%-40s %s\n" "$u" "$(curl -s -m 2 -o /dev/null -w %{http_code} "$u")"
    done
    printf "%-40s %s\n" "ssh deploy@app1" "$(ssh -o ConnectTimeout=3 10.89.50.11 readlink /opt/app/current 2>&1)"
    printf "%-40s %s\n" "podman API" "$(curl -fs --unix-socket /var/run/docker.sock http://d/version | head -c 60)"'
}

down() {
  podman rm -f "${CONTAINERS[@]}" >/dev/null 2>&1 || true
  podman network exists "$NET" && podman network rm "$NET" >/dev/null || true
  echo "lab removed"
}

clean() {
  down
  podman rmi -f localhost/vgl-target:lab localhost/vgl-lb:lab localhost/vgl-ctl:lab \
    localhost/vgl-app:v1 localhost/vgl-app:v1.1 localhost/vgl-app:v2 >/dev/null 2>&1 || true
  rm -rf "$RUN"
}

case "${1:-}" in
  build) build ;;
  up) up ;;
  status) status ;;
  vig) shift; podman exec vgl-ctl vigilante "$@" ;;
  sh) podman exec -it vgl-ctl bash ;;
  down) down ;;
  clean) clean ;;
  *) sed -n '2,12p' "$0"; exit 1 ;;
esac
