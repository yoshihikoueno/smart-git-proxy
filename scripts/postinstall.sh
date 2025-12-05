#!/bin/sh
set -e

APP_NAME="smart-git-proxy"

# Reload systemd and enable service
systemctl daemon-reload
systemctl enable $APP_NAME
systemctl start $APP_NAME || true
