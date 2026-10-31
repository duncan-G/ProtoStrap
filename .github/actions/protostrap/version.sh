#!/bin/bash
# This script prints the ProtoStrap version a spec's releases run: the spec's protostrap:,
# or with dev versions <that>-dev.<hash of ProtoStrap's source>, so a change to ProtoStrap
# ships with the sample even when the spec's version doesn't change. The plan and infra
# workflows share it, so the binary the infra workflow publishes is the one releases name.
# It needs the protostrap CLI on the PATH.
#
#   version.sh <spec> <ProtoStrap source> <dev version: true or false>
set -euo pipefail
spec=$1 source=$2 dev=$3

version=$(protostrap plan --spec "$spec" --json | jq -r .protostrap)
if [ "$dev" = true ]; then
  hash=$(git -C "$source" ls-files -s -- cmd internal edge go.mod | git hash-object --stdin | cut -c1-12)
  version="$version-dev.$hash"
fi
echo "$version"
