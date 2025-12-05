#!/bin/sh
set -e

# Create system user/group if not exists
if ! getent group gitproxy >/dev/null; then
    groupadd --system gitproxy
fi

if ! getent passwd gitproxy >/dev/null; then
    useradd --system --gid gitproxy --home-dir /var/lib/gitproxy --shell /usr/sbin/nologin gitproxy
fi

# Create default mirror directory
mkdir -p /var/lib/gitproxy/mirrors
chown -R gitproxy:gitproxy /var/lib/gitproxy

# Reload systemd and enable service
systemctl daemon-reload
systemctl enable smart-git-proxy
systemctl start smart-git-proxy || true
