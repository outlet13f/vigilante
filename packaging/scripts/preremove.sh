#!/bin/sh
# Stop the units on removal, not on upgrade (deb: $1=remove, rpm: $1=0).
set -e
case "$1" in
    remove|0)
        if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
            systemctl disable --now vigilante-server vigilante-agent >/dev/null 2>&1 || true
        fi
        ;;
esac
