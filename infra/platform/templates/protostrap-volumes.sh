#!/bin/bash
# This script mounts a data VM's EBS volumes (docs/vms.md#data): each stateful component's
# at /data/<component>, and on data-1 the control volume, which holds the swarm's state and
# the scheduler's key. The VM's volume list, $VOLUME_LIST, says which volume goes where, one
# "<volume id> <directory>" per line. Terraform writes it to the deploy bucket
# (infra/platform/data.tf), which the VM reaches through the S3 gateway endpoint, so
# mounting needs no way out to the internet. The script runs every minute, so a new
# component's volume is mounted soon after it is attached, with no change here.
# A new volume gets an XFS filesystem; one with a filesystem is never formatted.
#
#   protostrap-volumes                   mounts whatever is attached.
#   protostrap-volumes --wait <dir>      first waits for <dir> to be mounted, however long
#                                        that takes: until then the VM has nothing to run.
#
# The control volume's swarm/ directory is bind-mounted at /var/lib/docker/swarm, so the
# swarm survives the instance. Docker must not start before that bind mount is in place,
# which the unit's ordering ensures.
set -euo pipefail
export AWS_DEFAULT_REGION=${AWS_DEFAULT_REGION:?}
VOLUME_LIST=${VOLUME_LIST:?}

wait_for=""
if [ "${1:-}" = "--wait" ]; then
  wait_for=$2
fi

# Calling mount_volume <volume id> <dir> mounts the volume at <dir>, and returns false
# until the volume's device appears.
mount_volume() {
  local vol=$1 dir=$2 dev
  dev=/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_${vol//-/}
  [ -e "$dev" ] || return 1
  mkdir -p "$dir"
  if mountpoint -q "$dir"; then
    return 0
  fi
  if ! blkid "$dev" >/dev/null 2>&1; then
    echo "protostrap-volumes: formatting $vol for $dir"
    mkfs.xfs -q "$dev"
  fi
  local uuid
  uuid=$(blkid -s UUID -o value "$dev")
  grep -q "UUID=$uuid " /etc/fstab || echo "UUID=$uuid $dir xfs defaults,noatime,nofail 0 2" >>/etc/fstab
  mount "$dir"
  echo "protostrap-volumes: mounted $vol at $dir"
}

bind_swarm() {
  local control=$1
  mkdir -p "$control/swarm" "$control/scheduler" /var/lib/docker/swarm
  if ! mountpoint -q /var/lib/docker/swarm; then
    grep -q " /var/lib/docker/swarm " /etc/fstab ||
      echo "$control/swarm /var/lib/docker/swarm none bind,nofail,x-systemd.requires-mounts-for=$control 0 0" >>/etc/fstab
    mount /var/lib/docker/swarm
    echo "protostrap-volumes: the swarm's state is on the control volume"
  fi
}

mount_all() {
  local volumes
  volumes=$(aws s3 cp --only-show-errors "$VOLUME_LIST" -) || return 1
  local ok=0
  while read -r vol dir; do
    [ -n "$vol" ] || continue
    mount_volume "$vol" "$dir" || ok=1
    # A volume made bigger in the spec grows in place; the filesystem follows.
    mountpoint -q "$dir" && xfs_growfs -d "$dir" >/dev/null 2>&1 || true
    if [ "$dir" = /var/lib/protostrap/control ] && mountpoint -q "$dir"; then
      bind_swarm "$dir"
    fi
  done <<<"$volumes"
  return $ok
}

if [ -n "$wait_for" ]; then
  # A volume is attached by the same apply that creates the VM, so this normally takes a
  # minute. If that apply failed before attaching it, the VM waits for the next one.
  waited=0
  until mountpoint -q "$wait_for"; do
    mount_all || true
    if mountpoint -q "$wait_for"; then
      break
    fi
    if ((SECONDS - waited >= 60)); then
      echo "protostrap-volumes: still waiting for $wait_for, named in $VOLUME_LIST, to be attached"
      waited=$SECONDS
    fi
    sleep 5
  done
  if [ "$wait_for" = /var/lib/protostrap/control ]; then
    bind_swarm "$wait_for"
  fi
fi
mount_all || true
