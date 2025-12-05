#!/bin/sh
set -e

# Stop and disable service before removal
systemctl stop smart-git-proxy || true
systemctl disable smart-git-proxy || true
