#!/bin/sh
set -e

systemctl daemon-reload

# Don't remove user/group or data on normal remove (only purge)
if [ "$1" = "purge" ]; then
    rm -rf /var/lib/gitproxy
    userdel gitproxy 2>/dev/null || true
    groupdel gitproxy 2>/dev/null || true
fi
