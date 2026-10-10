#!/bin/sh
# State in /var/lib/vigilante (journal, audit trail) is kept on purpose: the
# audit trail may be subject to retention rules. Remove it by hand.
set -e
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi
