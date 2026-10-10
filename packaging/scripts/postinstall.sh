#!/bin/sh
# Units are installed but not enabled or started: the server needs a
# configuration first. Upgrades restart a running server.
set -e
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
    for unit in vigilante-server vigilante-agent; do
        if systemctl is-active --quiet "$unit"; then
            systemctl restart "$unit" || true
        fi
    done
fi
cat <<'MSG'
vigilante installed.
  1. Edit /etc/vigilante/vigilante.yaml (server) or /etc/vigilante/agent.env (agent)
  2. vigilante validate -c /etc/vigilante/vigilante.yaml
  3. systemctl enable --now vigilante-server   (or vigilante-agent)
MSG
