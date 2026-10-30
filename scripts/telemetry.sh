#!/usr/bin/env bash
# This script opens Grafana (docs/telemetry.md#reading-telemetry), the services' logs,
# traces and metrics, through an SSM port forward from this machine to the port the scheduler
# serves it on, on data-1's loopback. Grafana has no sign-in of its own: the SSM session
# document you are allowed to start is your access. Developers, Platform Admins and Org
# Admins may start protostrap-telemetry, which forwards Grafana's port and nothing else; it
# holds no infra log, which is in the operator UI (scripts/operator-ui.sh), for admins.
#
# Usage:
#   scripts/telemetry.sh [local port]   # The port defaults to 7482.
#
# Then open http://localhost:<port>, or hand an agent such as Claude its API:
# curl -s http://localhost:<port>/protostrap/help says how to use it.
#
# Set AWS_PROFILE to your Identity Center sign-in (Developer, PlatformAdmin or OrgAdmin),
# and AWS_REGION to data-1's region. If the account runs more than one application, set
# PROTOSTRAP_APP to the application.
#
# The script needs the AWS CLI and its Session Manager plugin.
set -euo pipefail

usage() {
  sed -n '9,13p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

document=protostrap-telemetry
port=${1:-7482}
if [[ $# -gt 1 || ! "$port" =~ ^[0-9]+$ ]]; then
  usage
fi
command -v aws >/dev/null || { echo "aws isn't installed" >&2; exit 1; }
command -v session-manager-plugin >/dev/null || {
  echo "the AWS CLI's Session Manager plugin isn't installed:" >&2
  echo "https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html" >&2
  exit 1
}

filters=("Name=tag:protostrap:vm,Values=data-1" "Name=instance-state-name,Values=running")
if [[ -n "${PROTOSTRAP_APP:-}" ]]; then
  filters+=("Name=tag:protostrap:app,Values=$PROTOSTRAP_APP")
fi
instances=$(aws ec2 describe-instances --filters "${filters[@]}" \
  --query 'Reservations[].Instances[].InstanceId' --output text)
read -r -a ids <<<"$instances"
if [[ ${#ids[@]} -eq 0 || "${ids[0]}" == "None" ]]; then
  echo "data-1 isn't running in this account and region" >&2
  exit 1
fi
if [[ ${#ids[@]} -gt 1 ]]; then
  echo "more than one data-1 (${ids[*]}): set PROTOSTRAP_APP to the application" >&2
  exit 1
fi

# The document fixes the port on data-1: 7482, the scheduler's telemetry_listen
# (infra/platform/telemetry.tf).
echo "Open http://localhost:$port for Grafana on data-1 (${ids[0]}), as $document."
echo "For an agent: curl -s http://localhost:$port/protostrap/help. Ctrl+C here closes it."
exec aws ssm start-session --target "${ids[0]}" --document-name "$document" \
  --parameters "localPortNumber=$port"
