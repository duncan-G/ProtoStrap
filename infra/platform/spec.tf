# This file reads the spec and derives the names everything else is built from.
#
# Terraform reads only the infra projection of the spec (docs/protostrap-aspire.md,
# "Infrastructure projection"): whether it has the tunnel, the gateway and telemetry, the
# gateway's hosts and their hostnames, each pool's VM types and size limits, the queues, the
# buckets, each component's kind, uses, queue signals, data VM and volume, and telemetry's
# volume and the retention of its logs and traces. The infra
# workflow has already run `protostrap check` on the whole file, so Terraform reads it
# without second-guessing it.

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}
data "aws_partition" "current" {}
data "aws_availability_zones" "available" {
  state = "available"
}

locals {
  spec = yamldecode(var.spec)
  app  = local.spec.app

  account   = data.aws_caller_identity.current.account_id
  region    = data.aws_region.current.name
  partition = data.aws_partition.current.partition
  zones     = length(var.zones) > 0 ? var.zones : [data.aws_availability_zones.available.names[0]]

  # These are ProtoStrap's own components (docs/protostrap.md): each runs only when the spec
  # has its section. `tunnel: {}` reads as an empty object, which isn't null.
  has_tunnel    = try(local.spec.tunnel, null) != null
  has_gateway   = try(local.spec.gateway, null) != null
  has_telemetry = try(local.spec.telemetry, null) != null

  # These are the application's hosts (docs/protostrap.md, "Hosts"): each of the gateway's
  # names with its hostname in this account, and the hostnames alone.
  spec_hosts = try(local.spec.gateway.hosts, null) == null ? {} : { for name, hostname in local.spec.gateway.hosts : name => hostname }
  hostnames  = var.hosts != null ? var.hosts : local.spec_hosts
  hosts      = sort(values(local.hostnames))

  # These names are shared with infra/bootstrap: keep them in step.
  bucket_prefix = "protostrap-${local.account}-${local.region}"
  path          = "/protostrap/"
  registry      = "${local.account}.dkr.ecr.${local.region}.amazonaws.com"
  role_boundary = "arn:${local.partition}:iam::${local.account}:policy/protostrap/protostrap-role-boundary"
  log_prefix    = "/protostrap/${local.app}/"

  tags = {
    "protostrap:app"         = local.app
    "protostrap:environment" = var.environment
  }

  # These are the pools, with the spec's defaults.
  ingress_vm = local.spec.pools.ingress.vm
  general = {
    vm  = flatten([local.spec.pools.general.vm])
    min = try(local.spec.pools.general.min, 1)
    max = local.spec.pools.general.max
  }
  compute = try(local.spec.pools.compute, null) == null ? null : {
    vm  = flatten([local.spec.pools.compute.vm])
    min = try(local.spec.pools.compute.min, 0)
    max = local.spec.pools.compute.max
  }
  data_vm    = local.spec.pools.data.vm
  data_count = try(local.spec.pools.data.count, 1)
  data_vms   = [for i in range(local.data_count) : "data-${i + 1}"]

  # These are the queues, with the spec's defaults (internal/spec).
  duration_units = { ms = 0.001, s = 1, m = 60, h = 3600, d = 86400 }
  queues = {
    for name, q in try(local.spec.queues, {}) : name => {
      fifo         = try(q.fifo, false)
      visibility   = floor(sum(concat([0], [for m in regexall("([0-9]+)(ms|s|m|h|d)", try(q.visibility, "2m")) : tonumber(m[0]) * local.duration_units[m[1]]])))
      retention    = floor(sum(concat([0], [for m in regexall("([0-9]+)(ms|s|m|h|d)", try(q.retention, "4d")) : tonumber(m[0]) * local.duration_units[m[1]]])))
      max_receives = try(q.max_receives, 5)
    }
  }

  # These are the buckets whose bucket exists: the spec's, and those retired but kept, which
  # keep the settings they had in the spec. Durations are in days, which `protostrap check`
  # makes them whole; null versions leaves versioning off, and null expire keeps objects.
  kept_buckets = {
    for name, b in merge(var.retired_buckets, try(local.spec.buckets, {})) : name => {
      browser  = try(b.browser, null) == true
      versions = try(b.versions, null) == null ? null : floor(sum(concat([0], [for m in regexall("([0-9]+)(ms|s|m|h|d)", b.versions) : tonumber(m[0]) * local.duration_units[m[1]]])) / 86400)
      lock     = try(b.lock, null) == true
      expire   = try(b.expire, null) == null ? null : floor(sum(concat([0], [for m in regexall("([0-9]+)(ms|s|m|h|d)", b.expire) : tonumber(m[0]) * local.duration_units[m[1]]])) / 86400)
    }
  }
  buckets = { for name, b in local.kept_buckets : name => b if contains(keys(try(local.spec.buckets, {})), name) }

  # These are the components: what each one is allowed and, for a stateful one, its volume.
  components = {
    for name, c in local.spec.components : name => {
      kind          = c.kind
      uses          = try(c.uses, [])
      queue_signals = [for d in try(c.scale.demand, []) : d.queue if try(d.queue, null) != null]
      vm            = try(c.vm, null)
      volume = c.kind != "stateful" ? null : {
        path = c.volume.path
        size = (tonumber(regex("^([0-9.]+)(Gi|Ti)$", c.volume.size)[0]) *
        (regex("^([0-9.]+)(Gi|Ti)$", c.volume.size)[1] == "Ti" ? 1024 : 1))
        iops       = try(c.volume.iops, 3000)
        throughput = try(c.volume.throughput, 125)
      }
    }
  }
  stateful = { for name, c in local.components : name => c if c.kind == "stateful" }

  # This is telemetry (docs/telemetry.md#settings), with the spec's defaults: its volume's
  # size in GiB, and how many days Loki and Tempo keep logs and traces.
  telemetry_volume = try(local.spec.telemetry.volume, "20Gi")
  telemetry = {
    volume = (tonumber(regex("^([0-9.]+)(Gi|Ti)$", local.telemetry_volume)[0]) *
    (regex("^([0-9.]+)(Gi|Ti)$", local.telemetry_volume)[1] == "Ti" ? 1024 : 1))
    logs_days   = floor(sum(concat([0], [for m in regexall("([0-9]+)(ms|s|m|h|d)", try(local.spec.telemetry.retention.logs, "30d")) : tonumber(m[0]) * local.duration_units[m[1]]])) / 86400)
    traces_days = floor(sum(concat([0], [for m in regexall("([0-9]+)(ms|s|m|h|d)", try(local.spec.telemetry.retention.traces, "30d")) : tonumber(m[0]) * local.duration_units[m[1]]])) / 86400)
  }

  # These are the components whose repository, secret, log group and role exist: the
  # spec's, and those retired but kept.
  kept_components = toset(concat(keys(local.components), var.retired_components))
}
