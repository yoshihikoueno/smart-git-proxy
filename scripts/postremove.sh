#!/bin/sh
set -e

systemctl daemon-reload

# Don't remove user/group or data on normal remove (only purge)
if [ "$1" = "purge" ]; then
    rm -rf /var/lib/smart-git-proxy
    userdel smart-git-proxy 2>/dev/null || true
    groupdel smart-git-proxy 2>/dev/null || true
fi
