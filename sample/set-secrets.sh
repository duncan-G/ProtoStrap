#!/usr/bin/env bash
# This script fills in the sample's secret, sample/<environment>: new passwords for
# PostgreSQL and Redis, and the connection strings the components that list them read. An
# Org Admin runs it once after the first infra apply (sample/README.md). Moving either to a
# managed service changes only these values.
#
# Running it again rotates both passwords, as one version of the secret: ProtoStrap has
# PostgreSQL and Redis apply theirs first, then replaces the components that connect to them.
#
# Usage:
#   sample/set-secrets.sh [environment]    # It is test unless you say otherwise.
#
# Set AWS_PROFILE to an Org Admin's credentials for the account, and AWS_REGION to the
# account's region.
set -euo pipefail
environment=${1:-test}
set_secret="$(cd "$(dirname "$0")/.." && pwd)/scripts/set-secret.sh"

# The values go to set-secret.sh in files only this user can read, never on a command line.
umask 077
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
password() { LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32; }
postgres=$(password) redis=$(password)
printf '%s' "$postgres" >"$work/postgres"
printf '%s' "$redis" >"$work/redis"
printf 'Host=postgres;Port=5432;Username=postgres;Password=%s;Database=postgres' "$postgres" >"$work/postgres-connection"
printf 'redis:6379,password=%s' "$redis" >"$work/redis-connection"

"$set_secret" "sample/$environment" \
  "Postgres_POSTGRES_PASSWORD=@$work/postgres" \
  "Redis_REDIS_PASSWORD=@$work/redis" \
  "Shared_ConnectionStrings__postgres=@$work/postgres-connection" \
  "Shared_ConnectionStrings__redis=@$work/redis-connection"
