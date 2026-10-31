#!/usr/bin/env bash
# This script puts the edge's Cloudflare tunnel token in the environment's secret,
# <app>/<environment>, as Edge_TUNNEL_TOKEN. An Org Admin runs it once after the first infra
# apply, and again if the tunnel is ever replaced. Until the token is there, the edge waits
# for it; once it is, the edge starts within a minute, and a new token replaces the edge
# within about three minutes.
#
# The tunnel is the one the platform made, protostrap-<app>-<environment>. Its token is
# fetched from Cloudflare's API, so it never passes through Terraform's state or a command
# line.
#
# Usage:
#   scripts/set-edge-token.sh <app> <environment>        # For example, pass "sample test".
#
# Set CLOUDFLARE_ACCOUNT_ID to the Cloudflare account the tunnel is in, and
# CLOUDFLARE_API_TOKEN to an API token with Account > Cloudflare Tunnel > Edit (the token
# the infra workflow uses has it). Set AWS_PROFILE to credentials that can read and write
# the secret, which means an Org Admin's, and AWS_REGION to the account's region.
#
# The script needs curl, jq and the AWS CLI.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: scripts/set-edge-token.sh <app> <environment>    # e.g. sample test" >&2
  exit 2
fi
app=$1 environment=$2
: "${CLOUDFLARE_ACCOUNT_ID:?set CLOUDFLARE_ACCOUNT_ID}"
: "${CLOUDFLARE_API_TOKEN:?set CLOUDFLARE_API_TOKEN}"
for tool in curl jq aws; do
  command -v "$tool" >/dev/null || { echo "$tool isn't installed" >&2; exit 1; }
done

cloudflare() {
  curl -sS --fail-with-body -H "Authorization: Bearer $CLOUDFLARE_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/$1"
}

name="protostrap-$app-$environment"
tunnel=$(cloudflare "cfd_tunnel?name=$name&is_deleted=false" | jq -r '.result[0].id // empty')
if [[ -z "$tunnel" ]]; then
  echo "no tunnel named $name in Cloudflare account $CLOUDFLARE_ACCOUNT_ID: has the infra workflow applied?" >&2
  exit 1
fi

# The token goes through a file only this user can read, not the command line, where ps
# would show it.
umask 077
file=$(mktemp)
trap 'rm -f "$file"' EXIT
cloudflare "cfd_tunnel/$tunnel/token" | jq -re '.result' >"$file"
"$(dirname "$0")/set-secret.sh" "$app/$environment" "Edge_TUNNEL_TOKEN=@$file"
echo "$app/$environment now holds the token of tunnel $name ($tunnel)"
