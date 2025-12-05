#!/bin/sh
set -e

USER_NAME="smart-git-proxy"
GROUP_NAME="smart-git-proxy"
APP_NAME="smart-git-proxy"

# Create system user/group if not exists
if ! getent group $GROUP_NAME >/dev/null; then
    groupadd --system $GROUP_NAME
fi

if ! getent passwd $USER_NAME >/dev/null; then
    useradd --system --gid $GROUP_NAME --home-dir /var/lib/$APP_NAME --shell /usr/sbin/nologin $USER_NAME
fi

# Reload systemd and enable service
systemctl daemon-reload
systemctl enable $APP_NAME
systemctl start $APP_NAME || true
