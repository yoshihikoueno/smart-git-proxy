#!/bin/sh
set -e

# Setup ephemeral storage if available (NVMe instance store on EC2)
# This runs before package installation

MIRROR_PATH="/var/lib/gitproxy/mirrors"

setup_ephemeral_storage() {
    # Check if we have required tools
    if ! command -v mdadm >/dev/null 2>&1; then
        echo "mdadm not found, skipping ephemeral storage setup"
        return 0
    fi

    if ! command -v mkfs.xfs >/dev/null 2>&1; then
        echo "mkfs.xfs not found, skipping ephemeral storage setup"
        return 0
    fi

    # Find NVMe instance store devices (not EBS)
    EPHEMERAL_DEVICES=""
    DEVICE_COUNT=0

    for dev in /dev/nvme*n1; do
        [ -b "$dev" ] || continue
        # Check if it's instance storage (not EBS)
        if nvme id-ctrl "$dev" 2>/dev/null | grep -q "Amazon EC2 NVMe Instance Storage"; then
            EPHEMERAL_DEVICES="$EPHEMERAL_DEVICES $dev"
            DEVICE_COUNT=$((DEVICE_COUNT + 1))
        fi
    done

    # Also check for older xvd* instance store devices
    for dev in /dev/xvd[b-z]; do
        if [ -b "$dev" ]; then
            # Skip if it looks like it's already partitioned/in use
            if ! lsblk "$dev" 2>/dev/null | grep -q "part"; then
                EPHEMERAL_DEVICES="$EPHEMERAL_DEVICES $dev"
                DEVICE_COUNT=$((DEVICE_COUNT + 1))
            fi
        fi
    done

    if [ "$DEVICE_COUNT" -eq 0 ]; then
        echo "No ephemeral devices found, using default storage"
        return 0
    fi

    echo "Found $DEVICE_COUNT ephemeral device(s):$EPHEMERAL_DEVICES"

    # Check if mirror path is already mounted
    if mountpoint -q "$MIRROR_PATH" 2>/dev/null; then
        echo "$MIRROR_PATH is already mounted, skipping setup"
        return 0
    fi

    # Create mount point
    mkdir -p "$MIRROR_PATH"

    if [ "$DEVICE_COUNT" -eq 1 ]; then
        # Single device - just format and mount
        DEVICE=$(echo "$EPHEMERAL_DEVICES" | tr -d ' ')
        echo "Formatting single ephemeral device: $DEVICE"
        mkfs.xfs -f "$DEVICE"
        mount "$DEVICE" "$MIRROR_PATH"
        
        # Add to fstab for persistence across reboots (won't survive instance stop/start anyway)
        if ! grep -q "$MIRROR_PATH" /etc/fstab; then
            echo "$DEVICE $MIRROR_PATH xfs defaults,nofail 0 2" >> /etc/fstab
        fi
    else
        # Multiple devices - create RAID-0
        echo "Creating RAID-0 array from $DEVICE_COUNT devices"
        
        # Stop any existing array
        mdadm --stop /dev/md0 2>/dev/null || true
        
        # shellcheck disable=SC2086
        mdadm --create /dev/md0 --level=0 --raid-devices="$DEVICE_COUNT" $EPHEMERAL_DEVICES --force --run
        mkfs.xfs -f /dev/md0
        mount /dev/md0 "$MIRROR_PATH"
        
        # Save RAID config
        mdadm --detail --scan >> /etc/mdadm.conf 2>/dev/null || true
        
        if ! grep -q "$MIRROR_PATH" /etc/fstab; then
            echo "/dev/md0 $MIRROR_PATH xfs defaults,nofail 0 2" >> /etc/fstab
        fi
    fi

    echo "Ephemeral storage mounted at $MIRROR_PATH"
}

# Only run on Linux (EC2)
if [ "$(uname)" = "Linux" ]; then
    setup_ephemeral_storage
fi
