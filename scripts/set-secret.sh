#!/usr/bin/env bash
# This script sets keys in the environment's secret, <app>/<environment>, keeping its other
# keys. Terraform makes the secret with no value; an Org Admin fills it in with this script,
# so values never pass through Terraform's state (docs/secrets.md).
#
# Each key starts with who it is for: a component's prefix, such as Api_ for the api,
# Shared_ for any component that lists the key, or ProtoStrap's Edge_ and Reporter_.
# Everything set in one run is one new version of the secret, which ProtoStrap applies
# within about three minutes, only to the components whose keys changed.
#
# Usage:
#   scripts/set-secret.sh <app>/<environment> KEY=value...   # e.g. shop/prod Api_StripeKey=sk_live_...
#   scripts/set-secret.sh <app>/<environment> KEY=-          # This reads KEY's value from the terminal without echoing it.
#   scripts/set-secret.sh <app>/<environment> KEY=@file      # This reads KEY's value from a file, such as a PEM key.
#   scripts/set-secret.sh <app>/<environment> -KEY           # This removes KEY.
#
#   scripts/set-secret.sh shop/prod Reporter_ANTHROPIC_API_KEY=-
#
# Set AWS_PROFILE to credentials that can read and write the secret, and AWS_REGION to
# the secret's region. The script needs jq and the AWS CLI.
set -euo pipefail

if [[ $# -lt 2 ]]; then
  sed -n '11,17p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
fi
secret=$1
shift
for tool in jq aws; do
  command -v "$tool" >/dev/null || { echo "$tool isn't installed" >&2; exit 1; }
done

# A key is a prefix, then a name a replica can read as a variable or a file.
valid_key() { [[ "$1" =~ ^[A-Z][A-Za-z0-9]*_[A-Za-z_][A-Za-z0-9_]*$ ]]; }

# A secret Terraform just made has no value yet; any other failure stops here, so a key
# is never lost by writing over a value that couldn't be read.
if ! current=$(aws secretsmanager get-secret-value --secret-id "$secret" --query SecretString --output text 2>&1); then
  if [[ "$current" != *ResourceNotFoundException* ]] || ! aws secretsmanager describe-secret --secret-id "$secret" >/dev/null; then
    echo "$current" >&2
    exit 1
  fi
  current='{}'
fi
echo "$current" | jq -e 'type == "object"' >/dev/null 2>&1 || { echo "$secret doesn't hold a JSON object" >&2; exit 1; }

# Values go to jq and the AWS CLI in files only this user can read, never on a command line,
# where ps would show them.
umask 077
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for arg in "$@"; do
  case "$arg" in
    -*)
      current=$(jq --arg k "${arg#-}" 'del(.[$k])' <<<"$current")
      ;;
    *=*)
      key=${arg%%=*} value=${arg#*=}
      if ! valid_key "$key"; then
        echo "$key: a key starts with who it is for, such as Api_StripeKey or Shared_ConnectionStrings__db" >&2
        exit 2
      fi
      case "$value" in
        -)
          read -rsp "$key: " value
          echo >&2
          printf '%s' "$value" >"$work/value"
          ;;
        @*)
          cat -- "${value#@}" >"$work/value"
          ;;
        *)
          printf '%s' "$value" >"$work/value"
          ;;
      esac
      current=$(jq --arg k "$key" --rawfile v "$work/value" '.[$k] = ($v | rtrimstr("\n"))' <<<"$current")
      ;;
    *)
      echo "want KEY=value, KEY=-, KEY=@file or -KEY, not $arg" >&2
      exit 2
      ;;
  esac
done

printf '%s' "$current" >"$work/secret"
version=$(aws secretsmanager put-secret-value --secret-id "$secret" --secret-string "file://$work/secret" \
  --query VersionId --output text)
echo "$secret now has: $(jq -r 'keys | join(", ")' <<<"$current")"
echo "Version $version: ProtoStrap applies it within about three minutes (protostrap status shows each component's)."
