#!/bin/bash
# This script puts a version's linux/arm64 protostrap binary in the deploy bucket, at
# protostrap/<version>/protostrap-linux-arm64 with its SHA-256 beside it, unless it is
# there: a version's binary is written once. The SHA-256 goes last, so a run that stops
# halfway is finished by the next. If protostrap/current names no version yet, it names
# this one: VMs install that version when they boot, and the scheduler keeps it current
# from then on.
#
# The infra workflow runs it before an apply, so every VM the apply makes boots with its
# agent, and the deploy workflow before a release.
#
#   publish.sh <binary> <version> <deploy bucket>
set -euo pipefail
binary=$1 version=$2 bucket=$3

key="protostrap/$version/protostrap-linux-arm64"
if aws s3api head-object --bucket "$bucket" --key "$key.sha256" >/dev/null 2>&1; then
  echo "protostrap $version is in the deploy bucket"
else
  sum=$(sha256sum "$binary" | awk '{print $1}')
  aws s3 cp --only-show-errors "$binary" "s3://$bucket/$key"
  echo "$sum" | aws s3 cp --only-show-errors - "s3://$bucket/$key.sha256"
  echo "protostrap $version copied to the deploy bucket"
fi
if ! aws s3api head-object --bucket "$bucket" --key protostrap/current >/dev/null 2>&1; then
  echo "$version" | aws s3 cp --only-show-errors - "s3://$bucket/protostrap/current"
  echo "protostrap/current names $version"
fi
