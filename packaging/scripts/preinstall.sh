#!/bin/sh
# Create the service account the units run as.
set -e
if ! getent group vigilante >/dev/null 2>&1; then
    groupadd --system vigilante
fi
if ! getent passwd vigilante >/dev/null 2>&1; then
    useradd --system --gid vigilante --home-dir /var/lib/vigilante --no-create-home \
        --shell /usr/sbin/nologin --comment "Vigilante rollback orchestrator" vigilante
fi
