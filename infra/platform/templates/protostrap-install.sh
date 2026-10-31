#!/bin/bash
# This script installs the protostrap binary from the deploy bucket: the version named by
# protostrap/current, checked against its published SHA-256. The infra workflow puts it
# there before an apply makes VMs, so the script normally finds it at once. If it isn't
# there yet, as after an apply run by hand, the script waits, and on a pool VM keeps the
# launch hook alive meanwhile, so the Auto Scaling group doesn't discard the VM for
# waiting.
#
# Once installed, the agent updates itself to whatever version the scheduler names.
# Settings come from /etc/protostrap/install.env.
set -euo pipefail

: "${DEPLOY_BUCKET:?}" "${AWS_DEFAULT_REGION:?}"
ASG=${ASG:-}
LAUNCH_HOOK=${LAUNCH_HOOK:-}
export AWS_DEFAULT_REGION

imds() {
  local token
  token=$(curl -sf -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')
  curl -sf -H "X-aws-ec2-metadata-token: $token" "http://169.254.169.254/latest/$1"
}

instance=$(imds meta-data/instance-id)
deadline=$((SECONDS + 6 * 3600))
beat=$SECONDS
version=""
while :; do
  version=$(aws s3 cp --only-show-errors "s3://$DEPLOY_BUCKET/protostrap/current" - 2>/dev/null | tr -d '[:space:]') || version=""
  [ -n "$version" ] && break
  if [ -n "$ASG" ] && ((SECONDS - beat >= 240)); then
    aws autoscaling record-lifecycle-action-heartbeat --auto-scaling-group-name "$ASG" \
      --lifecycle-hook-name "$LAUNCH_HOOK" --instance-id "$instance" >/dev/null 2>&1 || true
    beat=$SECONDS
  fi
  if ((SECONDS > deadline)); then
    echo "protostrap-install: no protostrap/current in s3://$DEPLOY_BUCKET after 6 hours" >&2
    exit 1
  fi
  sleep 15
done

if ! [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]]; then
  echo "protostrap-install: protostrap/current holds $version, which isn't a version" >&2
  exit 1
fi

# Every VM is Graviton, which runs the arm64 build.
key="protostrap/$version/protostrap-linux-arm64"
tmp=$(mktemp /usr/local/bin/.protostrap.XXXXXX)
trap 'rm -f "$tmp"' EXIT
aws s3 cp --only-show-errors "s3://$DEPLOY_BUCKET/$key" "$tmp"
want=$(aws s3 cp --only-show-errors "s3://$DEPLOY_BUCKET/$key.sha256" - | awk '{print $1}')
got=$(sha256sum "$tmp" | awk '{print $1}')
if [ "$want" != "$got" ]; then
  echo "protostrap-install: $key has SHA-256 $got, but $key.sha256 says $want" >&2
  exit 1
fi
chmod 0755 "$tmp"
mv -f "$tmp" /usr/local/bin/protostrap
trap - EXIT
echo "protostrap-install: installed protostrap $version"
