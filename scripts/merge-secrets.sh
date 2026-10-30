#!/usr/bin/env bash
# This script copies the secrets ProtoStrap had before the environment's secret, one for
# each component (protostrap/<component>), the edge's (protostrap/edge) and the reporter's
# (protostrap/reporter), into the environment's secret, <app>/<environment>, with each key
# under its owner's prefix: protostrap/api's StripeKey becomes Api_StripeKey,
# protostrap/edge's TUNNEL_TOKEN becomes Edge_TUNNEL_TOKEN. An Org Admin runs it once, after
# the platform apply that creates the environment's secret and before the release that
# reads it (docs/secrets.md#upgrading).
#
# Keys already in the environment's secret are kept, and named. Only key names are printed.
# The old secrets are left as they are: delete them once everything runs from the new one.
#
# Usage:
#   scripts/merge-secrets.sh <app> <environment>              # This writes one new version.
#   scripts/merge-secrets.sh <app> <environment> --dry-run    # This only prints what it would write.
#
# A component with secret_prefix in protostrap.yaml reads its keys under that prefix, not
# its name's: rename them afterwards with scripts/set-secret.sh.
#
# Set AWS_PROFILE to an Org Admin's credentials for the account, and AWS_REGION to its
# region. The script needs jq and the AWS CLI.
set -euo pipefail

if [[ $# -lt 2 || $# -gt 3 || ( $# -eq 3 && "$3" != "--dry-run" ) ]]; then
  sed -n '13,16p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
fi
target="$1/$2" dry=${3:-}
for tool in jq aws; do
  command -v "$tool" >/dev/null || { echo "$tool isn't installed" >&2; exit 1; }
done

# prefix is a secret's owner, in PascalCase, then _: api-gateway's is ApiGateway_.
prefix() { jq -rn --arg n "${1#protostrap/}" '$n | split("-") | map((.[:1] | ascii_upcase) + .[1:]) | join("") + "_"'; }

if ! merged=$(aws secretsmanager get-secret-value --secret-id "$target" --query SecretString --output text 2>&1); then
  if [[ "$merged" != *ResourceNotFoundException* ]] || ! aws secretsmanager describe-secret --secret-id "$target" >/dev/null; then
    echo "$merged" >&2
    exit 1
  fi
  merged='{}'
fi

# Values go to jq and the AWS CLI in files only this user can read, never on a command line.
umask 077
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
printf '%s' "$merged" >"$work/merged"

names=$(aws secretsmanager list-secrets --filters Key=name,Values=protostrap/ --query 'SecretList[].Name' --output text | tr '\t' '\n' | sort)
for name in $names; do
  [[ "$name" == protostrap/* ]] || continue
  if ! aws secretsmanager get-secret-value --secret-id "$name" --query SecretString --output text >"$work/old" 2>/dev/null; then
    echo "$name: no value, skipped"
    continue
  fi
  p=$(prefix "$name")
  if ! jq -e 'type == "object"' "$work/old" >/dev/null 2>&1; then
    echo "$name doesn't hold a JSON object of keys: copy its value by hand" >&2
    exit 1
  fi
  for key in $(jq -r 'keys[]' "$work/old"); do
    if jq -e --arg k "$p$key" 'has($k)' "$work/merged" >/dev/null; then
      echo "$name: $p$key is already set, kept"
      continue
    fi
    jq --arg k "$p$key" --arg o "$key" --slurpfile old "$work/old" '.[$k] = $old[0][$o]' "$work/merged" >"$work/next"
    mv "$work/next" "$work/merged"
    echo "$name: $key -> $p$key"
  done
done

if [[ -n "$dry" ]]; then
  echo "dry run: $target is unchanged"
  exit 0
fi
version=$(aws secretsmanager put-secret-value --secret-id "$target" --secret-string "file://$work/merged" --query VersionId --output text)
echo "$target now has: $(jq -r 'keys | join(", ")' "$work/merged") (version $version)"
