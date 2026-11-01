# This test applies the platform to mocked providers with the sample spec, so the spec
# decoding, the names, the per-zone layout and the rendered user data can be checked
# without AWS:
#
#   terraform -chdir=infra/platform init -backend=false && terraform -chdir=infra/platform test

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }
  mock_data "aws_region" {
    defaults = { name = "us-east-1" }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
  mock_data "aws_availability_zones" {
    defaults = { names = ["us-east-1a", "us-east-1b", "us-east-1c"] }
  }
  mock_data "aws_ssm_parameter" {
    defaults = { insecure_value = "ami-0123456789abcdef0", value = "ami-0123456789abcdef0" }
  }
  mock_data "aws_iam_policy_document" {
    defaults = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::123456789012:role/protostrap/mock" }
  }
  mock_resource "aws_sqs_queue" {
    defaults = { arn = "arn:aws:sqs:us-east-1:123456789012:protostrap-mock", url = "https://sqs.us-east-1.amazonaws.com/123456789012/protostrap-mock" }
  }
  mock_resource "aws_s3_bucket" {
    defaults = { arn = "arn:aws:s3:::mock" }
  }
  mock_data "aws_s3_bucket" {
    defaults = { arn = "arn:aws:s3:::mock-deploy" }
  }
  mock_resource "aws_eip" {
    defaults = { allocation_id = "eipalloc-0123456789abcdef0" }
  }
  mock_resource "aws_sns_topic" {
    defaults = { arn = "arn:aws:sns:us-east-1:123456789012:protostrap-mock" }
  }
  mock_resource "aws_iam_instance_profile" {
    defaults = { arn = "arn:aws:iam::123456789012:instance-profile/protostrap/mock" }
  }
  mock_resource "aws_cloudwatch_log_group" {
    defaults = { arn = "arn:aws:logs:us-east-1:123456789012:log-group:/protostrap/mock" }
  }
  mock_resource "aws_secretsmanager_secret" {
    defaults = { arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:protostrap/mock-AbCdEf" }
  }
  mock_resource "aws_ebs_volume" {
    defaults = { id = "vol-0123456789abcdef0", arn = "arn:aws:ec2:us-east-1:123456789012:volume/vol-0123456789abcdef0" }
  }
  mock_resource "aws_instance" {
    defaults = { id = "i-0123456789abcdef0", arn = "arn:aws:ec2:us-east-1:123456789012:instance/i-0123456789abcdef0" }
  }
  mock_resource "aws_launch_template" {
    defaults = { id = "lt-0123456789abcdef0", latest_version = 1 }
  }
  mock_resource "aws_route_table" {
    defaults = { id = "rtb-0123456789abcdef0", arn = "arn:aws:ec2:us-east-1:123456789012:route-table/rtb-0123456789abcdef0" }
  }
}

mock_provider "cloudflare" {
  mock_data "cloudflare_zone" {
    defaults = { id = "0123456789abcdef0123456789abcdef" }
  }
}

mock_provider "random" {}

variables {
  spec                  = file("../../sample/protostrap.yaml")
  environment           = "test"
  hosts                 = { sample = "sample.example.com" }
  cloudflare_account_id = "0123456789abcdef0123456789abcdef"
  cloudflare_zone       = "example.com"
}

run "sample" {
  command = apply

  assert {
    condition     = local.app == "sample" && tolist(local.zones) == tolist(["us-east-1a"])
    error_message = "One zone by default, the region's first."
  }

  assert {
    condition = (aws_subnet.public["us-east-1a"].cidr_block == "10.20.0.0/26" &&
      aws_subnet.data["us-east-1a"].cidr_block == "10.20.1.0/26" &&
      aws_subnet.managed["us-east-1a"].cidr_block == "10.20.2.0/26" &&
    aws_subnet.nodes["us-east-1a"].cidr_block == "10.20.16.0/20")
    error_message = "The subnets follow docs/network.md#subnets."
  }

  assert {
    condition     = aws_instance.data["data-1"].private_ip == "10.20.1.10" && local.scheduler_addr == "10.20.1.10:7443"
    error_message = "data-1 is at 10.20.1.10, where the agents reach the scheduler."
  }

  assert {
    condition     = toset(keys(aws_ecr_repository.component)) == toset(["api", "web", "py", "jobs", "migrate", "postgres", "redis"])
    error_message = "Every component gets a repository."
  }

  assert {
    condition     = aws_ecr_repository.component["api"].name == "protostrap/api" && aws_iam_role.component["api"].name == "protostrap-c-api"
    error_message = "Repositories are protostrap/<component>, roles protostrap-c-<component>."
  }

  assert {
    condition     = aws_secretsmanager_secret.app.name == "sample/test"
    error_message = "The environment has one secret, <app>/<environment>, as the scheduler names it in SECRET_ID."
  }

  assert {
    condition     = aws_cloudwatch_log_group.component["web"].name == "/protostrap/sample/web"
    error_message = "Log groups are /protostrap/<app>/<component>, where the agent ships replicas' logs."
  }

  assert {
    condition = (toset(keys(aws_ebs_volume.stateful)) == toset(["postgres", "redis"]) &&
      aws_ebs_volume.stateful["postgres"].size == 20 && aws_ebs_volume.stateful["redis"].size == 5 &&
    aws_ebs_volume.stateful["postgres"].tags["protostrap:mount"] == "/data/postgres")
    error_message = "Each stateful component gets its volume, sized from the spec, mounted at /data/<component>."
  }

  assert {
    condition     = aws_volume_attachment.stateful["postgres"].device_name != aws_volume_attachment.stateful["redis"].device_name
    error_message = "Two volumes on one data VM need two devices."
  }

  assert {
    condition = (aws_sqs_queue.main["jobs"].name == "protostrap-jobs" &&
      aws_sqs_queue.main["jobs"].visibility_timeout_seconds == 120 &&
      aws_sqs_queue.main["jobs"].message_retention_seconds == 345600 &&
    aws_sqs_queue.dead["jobs"].name == "protostrap-jobs-dlq")
    error_message = "The jobs queue has the spec's defaults and a dead-letter queue."
  }

  assert {
    condition = (tolist(local.component_access["api"].send) == tolist(["jobs"]) &&
      tolist(local.component_access["jobs"].receive) == tolist(["jobs"]) &&
    length(local.component_access["web"].send) == 0 && length(local.component_access["web"].receive) == 0)
    error_message = "The api sends to jobs, the worker receives from it, and web touches no queue."
  }

  assert {
    condition = (toset(keys(aws_autoscaling_group.general)) == toset(["us-east-1a"]) &&
      aws_autoscaling_group.general["us-east-1a"].min_size == 1 &&
      aws_autoscaling_group.general["us-east-1a"].max_size == 5 &&
    aws_autoscaling_group.general["us-east-1a"].protect_from_scale_in)
    error_message = "General: the pool's minimum, its maximum plus one, protected from scale-in."
  }

  assert {
    condition = (toset([for g in aws_autoscaling_group.compute : g.name]) == toset(["protostrap-compute-spot-us-east-1a", "protostrap-compute-ondemand-us-east-1a"]) &&
      aws_autoscaling_group.compute["us-east-1a/spot"].capacity_rebalance &&
    !aws_autoscaling_group.compute["us-east-1a/on-demand"].capacity_rebalance)
    error_message = "Compute: a Spot group with capacity rebalancing and an On-Demand fallback."
  }

  assert {
    condition     = aws_autoscaling_group.ingress["us-east-1a"].min_size == 1 && aws_autoscaling_group.ingress["us-east-1a"].max_size == 2
    error_message = "Ingress: one VM, and room for its replacement."
  }

  assert {
    condition     = strcontains(base64decode(aws_launch_template.general["us-east-1a"].user_data), "\"auto_scaling_group\":\"protostrap-general-us-east-1a\"")
    error_message = "A general VM's agent knows its group."
  }

  assert {
    condition     = strcontains(base64decode(aws_launch_template.ingress["us-east-1a"].user_data), "\"elastic_ip_allocation\":\"eipalloc-0123456789abcdef0\"")
    error_message = "An ingress VM's agent knows its zone's Elastic IP."
  }

  assert {
    condition = (strcontains(base64decode(aws_instance.data["data-1"].user_data_base64), "\"swarm_manager\":true") &&
      strcontains(base64decode(aws_instance.data["data-1"].user_data_base64), "protostrap-scheduler.service") &&
      strcontains(base64decode(aws_instance.data["data-1"].user_data_base64), "--wait /var/lib/protostrap/control") &&
      strcontains(base64decode(aws_instance.data["data-1"].user_data_base64), "VOLUME_LIST=s3://protostrap-123456789012-us-east-1-deploy/volumes/data-1") &&
    !strcontains(base64decode(aws_instance.data["data-1"].user_data_base64), "\"launch_hook\""))
    error_message = "data-1 runs the swarm manager and the scheduler, waits for its control volume, finds its volumes in its list, and has no lifecycle hooks."
  }

  assert {
    condition = (aws_s3_object.volumes["data-1"].bucket == data.aws_s3_bucket.deploy.bucket &&
      aws_s3_object.volumes["data-1"].key == "volumes/data-1" &&
      toset([for line in split("\n", trimspace(aws_s3_object.volumes["data-1"].content)) : split(" ", line)[1]]) ==
    toset(["/data/postgres", "/data/redis", "/data/telemetry", "/var/lib/protostrap/control"]))
    error_message = "data-1's volume list in the deploy bucket names each of its volumes and where it mounts: the stateful components', telemetry's and the control volume."
  }

  # A data VM mounts its volumes before its zone has a way out to the internet, so it reads
  # the list through the S3 gateway endpoint rather than asking the EC2 API.
  assert {
    condition = (!contains(flatten([for s in data.aws_iam_policy_document.data_vm.statement : tolist(s.actions)]), "ec2:DescribeVolumes") &&
      anytrue([for s in data.aws_iam_policy_document.data_vm.statement :
    contains(s.actions, "s3:GetObject") && contains(s.resources, "${data.aws_s3_bucket.deploy.arn}/volumes/*")]))
    error_message = "Data VMs read their volume lists from the deploy bucket, and don't describe volumes."
  }

  assert {
    condition     = !strcontains(base64decode(aws_launch_template.general["us-east-1a"].user_data), "protostrap-scheduler")
    error_message = "Only data-1 runs the scheduler."
  }

  assert {
    condition     = jsondecode(local.scheduler_config).address == "10.20.1.10" && jsondecode(local.scheduler_config).deploy_bucket == "protostrap-123456789012-us-east-1-deploy"
    error_message = "The scheduler's config."
  }

  # Bootstrap makes the deploy bucket, so the protostrap binary is in it before any VM
  # boots. The platform finds it by name and adds the lifecycle rules.
  assert {
    condition = (data.aws_s3_bucket.deploy.bucket == "protostrap-123456789012-us-east-1-deploy" &&
    aws_s3_bucket_lifecycle_configuration.deploy.bucket == data.aws_s3_bucket.deploy.bucket)
    error_message = "The platform uses bootstrap's deploy bucket, and sets its lifecycle rules."
  }

  assert {
    condition     = toset(local.tunnel_hosts) == toset(["sample.example.com"]) && toset(keys(cloudflare_dns_record.host)) == toset(["sample.example.com"])
    error_message = "Every host points at the tunnel."
  }

  assert {
    condition = (aws_s3_object.hosts.bucket == data.aws_s3_bucket.deploy.bucket && aws_s3_object.hosts.key == "hosts.json" &&
    jsondecode(aws_s3_object.hosts.content) == { sample = "sample.example.com" })
    error_message = "hosts.json in the deploy bucket gives the scheduler each host's hostname in this account."
  }
}

# Each data VM's list names only the volumes attached to it, one "<volume id> <directory>"
# per line. Its own state gives each volume the ID it is overridden with.
run "volume_lists_on_two_data_vms" {
  command   = apply
  state_key = "two_data_vms"

  variables {
    spec = replace(replace(file("../../sample/protostrap.yaml"),
      "data: { vm: m7g.large, count: 1 }", "data: { vm: m7g.large, count: 2 }"),
    "image: redis:7.4\n    vm: data-1", "image: redis:7.4\n    vm: data-2")
  }

  override_resource {
    target = aws_ebs_volume.control
    values = { id = "vol-0c" }
  }
  override_resource {
    target = aws_ebs_volume.telemetry[0]
    values = { id = "vol-0t" }
  }
  override_resource {
    target = aws_ebs_volume.stateful["postgres"]
    values = { id = "vol-0p" }
  }
  override_resource {
    target = aws_ebs_volume.stateful["redis"]
    values = { id = "vol-0r" }
  }

  assert {
    condition     = aws_s3_object.volumes["data-1"].content == "vol-0c /var/lib/protostrap/control\nvol-0p /data/postgres\nvol-0t /data/telemetry\n"
    error_message = "data-1 mounts the control volume, postgres's and telemetry's: ${aws_s3_object.volumes["data-1"].content}"
  }

  assert {
    condition     = aws_s3_object.volumes["data-2"].content == "vol-0r /data/redis\n" && aws_s3_object.volumes["data-2"].key == "volumes/data-2"
    error_message = "data-2 mounts only redis's volume: ${aws_s3_object.volumes["data-2"].content}"
  }

  assert {
    condition     = strcontains(base64decode(aws_instance.data["data-2"].user_data_base64), "VOLUME_LIST=s3://protostrap-123456789012-us-east-1-deploy/volumes/data-2")
    error_message = "data-2 reads its own list."
  }
}

run "hosts_must_match_the_spec" {
  command = plan

  variables {
    hosts = { sample = "sample.example.com", blog = "blog.example.com" }
  }

  expect_failures = [aws_s3_object.hosts]
}

run "hosts_must_name_every_host" {
  command = plan

  variables {
    hosts = {}
  }

  expect_failures = [aws_s3_object.hosts]
}

run "three_zones_and_retired" {
  command = apply

  variables {
    zones              = ["us-east-1a", "us-east-1b", "us-east-1c"]
    retired_components = ["old-worker"]
    retired_queues     = ["old-jobs"]
  }

  assert {
    condition = (aws_subnet.nodes["us-east-1b"].cidr_block == "10.20.32.0/20" &&
      aws_subnet.public["us-east-1c"].cidr_block == "10.20.0.128/26" &&
    aws_subnet.data["us-east-1b"].cidr_block == "10.20.1.64/26")
    error_message = "Each zone has its own range in every kind of subnet."
  }

  assert {
    condition     = length(aws_autoscaling_group.ingress) == 3 && length(aws_eip.ingress) == 3
    error_message = "An ingress VM and Elastic IP per zone."
  }

  assert {
    condition     = sum([for g in aws_autoscaling_group.general : g.min_size]) == 1
    error_message = "The general pool's minimum of 1 is split across zones, not repeated."
  }

  assert {
    condition     = aws_instance.data["data-1"].private_ip == "10.20.1.10"
    error_message = "data-1 stays in the first zone."
  }

  assert {
    condition     = contains(keys(aws_ecr_repository.component), "old-worker") && contains(keys(aws_sqs_queue.main), "old-jobs")
    error_message = "Retired components and queues are kept."
  }
}

run "no_compute_pool" {
  command = apply
  # "tiny" is another application, with state of its own: in the sample's, dropping
  # PostgreSQL and Redis would plan to delete their volumes, which prevent_destroy refuses.
  state_key = "tiny"

  variables {
    spec  = <<-EOT
      app: tiny
      protostrap: 0.1.0
      pools:
        ingress: { vm: c8g.medium }
        general: { vm: [m7g.large], max: 2 }
        data: { vm: m7g.large }
      gateway:
        hosts:
          tiny: tiny.example.com
        upstreams:
          web:
            routes: [/]
      components:
        web:
          kind: service
          build: web
          pool: general
          request: { cpu: 100m, memory: 128Mi }
          port: 80
          ready: /healthz
    EOT
    hosts = null
  }

  assert {
    condition     = length(aws_autoscaling_group.compute) == 0 && length(aws_sqs_queue.main) == 0 && length(aws_ebs_volume.stateful) == 0
    error_message = "No compute pool, queues or stateful volumes unless the spec has them."
  }

  assert {
    condition     = tolist(local.hosts) == tolist(["tiny.example.com"]) && jsondecode(aws_s3_object.hosts.content) == { tiny = "tiny.example.com" }
    error_message = "Without hosts in the config, the spec's hostnames, in hosts.json too."
  }

  assert {
    condition     = length(aws_s3_bucket.app) == 0 && length(aws_s3_bucket_cors_configuration.files) == 1
    error_message = "No buckets but files unless the spec has them; files takes signed URLs from the hosts."
  }
}

run "buckets" {
  command = apply
  # "tiny" is another application, with state of its own: in the sample's, dropping
  # PostgreSQL and Redis would plan to delete their volumes, which prevent_destroy refuses.
  state_key = "tiny"

  variables {
    spec         = <<-EOT
      app: tiny
      protostrap: 0.1.0
      pools:
        ingress: { vm: c8g.medium }
        general: { vm: [m7g.large], max: 2 }
        data: { vm: m7g.large }
      gateway:
        hosts:
          tiny: tiny.example.com
        upstreams:
          web:
            routes: [/]
      queues:
        jobs: {}
      buckets:
        documents: { browser: true, versions: 7d, lock: true }
        reports: { expire: 30d }
        scratch: {}
      components:
        web:
          kind: service
          build: web
          pool: general
          request: { cpu: 100m, memory: 128Mi }
          port: 80
          ready: /healthz
          uses: [documents, files, jobs]
        worker:
          kind: worker
          build: worker
          pool: general
          request: { cpu: 100m, memory: 128Mi }
          uses: [reports]
          scale: { max: 2, demand: [{ queue: jobs, rate: 1/s, drain: 1m }] }
    EOT
    hosts        = null
    cors_origins = ["http://localhost:4200"]
    retired_buckets = {
      old-docs    = { versions = "7d", lock = true }
      old-exports = { expire = "7d" }
    }
  }

  assert {
    condition = (toset(keys(aws_s3_bucket.app)) == toset(["documents", "reports", "scratch", "old-docs", "old-exports"]) &&
    aws_s3_bucket.app["documents"].bucket == "protostrap-123456789012-us-east-1-documents")
    error_message = "Every bucket in the spec, and every retired one, is protostrap-<account>-<region>-<name>."
  }

  assert {
    condition = (length(aws_s3_bucket_public_access_block.app) == 5 &&
      alltrue([for b in aws_s3_bucket_public_access_block.app : b.block_public_acls && b.block_public_policy && b.ignore_public_acls && b.restrict_public_buckets]) &&
      length(aws_s3_bucket_ownership_controls.app) == 5 &&
    length(aws_s3_bucket_server_side_encryption_configuration.app) == 5)
    error_message = "Every bucket is private, owned by the account and encrypted."
  }

  assert {
    condition = (length(aws_s3_bucket_policy.app) == 5 &&
      jsondecode(aws_s3_bucket_policy.app["scratch"].policy).Statement[0].Effect == "Deny" &&
      jsondecode(aws_s3_bucket_policy.app["scratch"].policy).Statement[0].Condition.Bool["aws:SecureTransport"] == "false" &&
    jsondecode(aws_s3_bucket_policy.files.policy).Statement[0].Sid == "TLSOnly")
    error_message = "Every bucket, files included, refuses plain HTTP."
  }

  assert {
    condition = (toset(keys(aws_s3_bucket_versioning.app)) == toset(["documents", "old-docs"]) &&
      aws_s3_bucket_versioning.app["documents"].versioning_configuration[0].status == "Enabled" &&
    toset(keys(aws_s3_bucket_object_lock_configuration.app)) == toset(["documents", "old-docs"]))
    error_message = "Only buckets with versions are versioned, and only locked ones have object lock, retired ones as they were."
  }

  assert {
    condition = (toset(keys(aws_s3_bucket_cors_configuration.app)) == toset(["documents"]) &&
      toset([for r in aws_s3_bucket_cors_configuration.app["documents"].cors_rule : r][0].allowed_origins) == toset(["https://tiny.example.com", "http://localhost:4200"]) &&
    toset([for r in aws_s3_bucket_cors_configuration.files[0].cors_rule : r][0].allowed_origins) == toset(["https://tiny.example.com", "http://localhost:4200"]))
    error_message = "Browser buckets and files take signed URLs from the hosts and cors_origins; other buckets take none."
  }

  assert {
    condition = (toset([for r in aws_s3_bucket_lifecycle_configuration.app["scratch"].rule : r.id]) == toset(["abandoned-uploads"]) &&
      toset([for r in aws_s3_bucket_lifecycle_configuration.app["documents"].rule : r.id]) == toset(["abandoned-uploads", "old-versions"]) &&
      toset([for r in aws_s3_bucket_lifecycle_configuration.app["reports"].rule : r.id]) == toset(["abandoned-uploads", "expire"]) &&
      [for r in aws_s3_bucket_lifecycle_configuration.app["documents"].rule : r.noncurrent_version_expiration[0].noncurrent_days if r.id == "old-versions"][0] == 7 &&
    [for r in aws_s3_bucket_lifecycle_configuration.app["reports"].rule : r.expiration[0].days if r.id == "expire"][0] == 30)
    error_message = "Every bucket aborts abandoned uploads; versions and expire add their own rules, in days."
  }

  assert {
    condition = (tolist(local.component_access["web"].buckets) == tolist(["documents"]) &&
      tolist(local.component_access["web"].locked) == tolist(["documents"]) &&
      local.component_access["web"].files &&
      tolist(local.component_access["worker"].buckets) == tolist(["reports"]) &&
    length(local.component_access["worker"].locked) == 0)
    error_message = "A component may use the buckets in its uses, and set retention only on locked ones."
  }

  assert {
    condition     = output.buckets["documents"] == "protostrap-123456789012-us-east-1-documents" && output.buckets["files"] == "protostrap-123456789012-us-east-1-files"
    error_message = "The buckets output names every bucket."
  }
}

run "retired_locked_bucket_without_versions" {
  command = plan

  variables {
    retired_buckets = { old-docs = { lock = true } }
  }

  expect_failures = [var.retired_buckets]
}

run "operator_ui_and_infra_log" {
  command = apply

  assert {
    condition = (jsondecode(aws_ssm_document.ui["protostrap-ui"].content).properties.portNumber == "7480" &&
    jsondecode(aws_ssm_document.ui["protostrap-ui-admin"].content).properties.portNumber == "7481")
    error_message = "Platform Admins' session document forwards the UI's port 7480; Org Admins' forwards 7481, where their actions will be."
  }

  assert {
    condition = alltrue([for d in aws_ssm_document.ui : d.document_type == "Session" &&
      jsondecode(d.content).sessionType == "Port" &&
      jsondecode(d.content).properties.type == "LocalPortForwarding" &&
    keys(jsondecode(d.content).parameters) == ["localPortNumber"]])
    error_message = "Each is a port forward to data-1's own loopback, and the person starting it picks only the local port."
  }

  assert {
    condition = ([for r in aws_s3_bucket_lifecycle_configuration.deploy.rule : r.filter[0].prefix if r.id == "infra-logs"] == ["logs/"] &&
      [for r in aws_s3_bucket_lifecycle_configuration.deploy.rule : r.expiration[0].days if r.id == "infra-logs"] == [30] &&
    [for r in aws_s3_bucket_lifecycle_configuration.deploy.rule : r.noncurrent_version_expiration[0].noncurrent_days if r.id == "infra-logs"] == [1])
    error_message = "The infra log's archive expires with the other logs, and the scheduler's rewrites of an hour don't pile up as old versions."
  }

  assert {
    condition     = contains(flatten([for s in data.aws_iam_policy_document.scheduler.statement : s.resources if s.sid == "WriteState"]), "${data.aws_s3_bucket.deploy.arn}/logs/*")
    error_message = "The scheduler writes the infra log's archive to the deploy bucket."
  }
}

run "secret" {
  command = apply

  assert {
    condition = alltrue([for name, d in data.aws_iam_policy_document.component :
    contains(flatten([for st in d.statement : st.resources if st.sid == "EnvironmentSecret"]), aws_secretsmanager_secret.app.arn)])
    error_message = "Every component's role reads the environment's secret, for its own keys."
  }

  assert {
    condition = (contains(flatten([for st in data.aws_iam_policy_document.edge.statement : st.resources if st.sid == "EnvironmentSecret"]), aws_secretsmanager_secret.app.arn) &&
    contains(flatten([for st in data.aws_iam_policy_document.reporter.statement : st.resources if st.sid == "TheLLMKey"]), aws_secretsmanager_secret.app.arn))
    error_message = "The edge reads its tunnel token, and the reporter its LLM key, from the environment's secret."
  }

  assert {
    condition = (jsonencode(sort(flatten([for st in data.aws_iam_policy_document.scheduler.statement : st.actions if st.sid == "WatchSecret"]))) == jsonencode(["secretsmanager:DescribeSecret", "secretsmanager:GetSecretValue"]) &&
    jsonencode(flatten([for st in data.aws_iam_policy_document.scheduler.statement : st.resources if st.sid == "WatchSecret"])) == jsonencode([aws_secretsmanager_secret.app.arn]))
    error_message = "The scheduler reads the environment's secret, and no other, to hash each component's keys."
  }

  assert {
    condition     = !strcontains(local.scheduler_config, "report_secret")
    error_message = "The reporter finds the environment's secret from the app and environment, as the scheduler does."
  }

  assert {
    condition     = aws_secretsmanager_secret.app.recovery_window_in_days == 7 && aws_secretsmanager_secret.app.tags["protostrap:app"] == "sample"
    error_message = "The secret can be restored for a week after it is deleted, and is tagged as ProtoStrap's."
  }
}

run "telemetry" {
  command = apply

  variables {
    spec = replace(file("../../sample/protostrap.yaml"), "telemetry: {}\n", "telemetry:\n  volume: 30Gi\n  retention: { logs: 14d, traces: 3d }\n")
  }

  assert {
    condition = (aws_ebs_volume.telemetry[0].size == 30 &&
      aws_ebs_volume.telemetry[0].tags["protostrap:mount"] == "/data/telemetry" &&
      aws_ebs_volume.telemetry[0].tags["protostrap:snapshot"] == "daily" &&
    aws_volume_attachment.telemetry[0].instance_id == aws_instance.data["data-1"].id)
    error_message = "Telemetry's volume is on data-1, sized from the spec, mounted at /data/telemetry and snapshotted like the others."
  }

  assert {
    condition     = !contains(values(aws_volume_attachment.stateful)[*].device_name, aws_volume_attachment.telemetry[0].device_name) && aws_volume_attachment.telemetry[0].device_name != aws_volume_attachment.control.device_name
    error_message = "Telemetry's volume has a device of its own on data-1."
  }

  assert {
    condition = (aws_s3_bucket.telemetry.bucket == "protostrap-123456789012-us-east-1-telemetry" &&
    [for r in aws_s3_bucket_lifecycle_configuration.telemetry.rule : r.expiration[0].days if r.id == "expire"] == [21])
    error_message = "The telemetry bucket, as the scheduler names it, expires objects a week after the longer of logs and traces, in case Loki or Tempo misses one."
  }

  assert {
    condition = (aws_iam_role.telemetry.name == "protostrap-c-telemetry" &&
      jsonencode(sort(flatten([for st in data.aws_iam_policy_document.telemetry.statement : st.resources]))) == jsonencode(sort([aws_s3_bucket.telemetry.arn, "${aws_s3_bucket.telemetry.arn}/*"])) &&
    !strcontains(jsonencode(data.aws_iam_policy_document.telemetry.statement), "secretsmanager"))
    error_message = "Telemetry's role uses its bucket and nothing else: no secret."
  }

  assert {
    condition     = aws_ecr_repository.telemetry.name == "protostrap/telemetry"
    error_message = "The telemetry image's repository, which the deploy workflow pushes each version to."
  }

  assert {
    condition = (aws_ssm_document.telemetry[0].name == "protostrap-telemetry" &&
      jsondecode(aws_ssm_document.telemetry[0].content).properties.portNumber == "7482" &&
      jsondecode(aws_ssm_document.telemetry[0].content).properties.type == "LocalPortForwarding" &&
    keys(jsondecode(aws_ssm_document.telemetry[0].content).parameters) == ["localPortNumber"])
    error_message = "Grafana's session document forwards port 7482 and nothing else, apart from the operator UI's."
  }

  assert {
    condition = (aws_vpc_security_group_ingress_rule.node["telemetry"].from_port == 24224 &&
      aws_vpc_security_group_ingress_rule.node["telemetry"].ip_protocol == "tcp" &&
    aws_vpc_security_group_ingress_rule.node["telemetry"].referenced_security_group_id == aws_security_group.node.id)
    error_message = "Docker on every VM sends replicas' output to data-1, port 24224, from other VMs only."
  }

  assert {
    condition     = length([for r in aws_network_acl.public.ingress : r if r.action == "deny" && r.from_port == 24224 && r.cidr_block == "0.0.0.0/0"]) == 1
    error_message = "The public subnet's network ACL refuses telemetry's port from the internet."
  }
}

run "telemetry_defaults" {
  command = apply

  assert {
    condition = (aws_ebs_volume.telemetry[0].size == 20 &&
    [for r in aws_s3_bucket_lifecycle_configuration.telemetry.rule : r.expiration[0].days if r.id == "expire"] == [37])
    error_message = "With telemetry: {}: a 20Gi volume, and 30 days of logs and traces, plus a week."
  }
}

# Without telemetry, the volume, its port and Grafana's session document go: replicas' output
# goes to their log groups. The bucket stays, and its lifecycle rule expires what is in it.
run "no_telemetry" {
  command = apply

  variables {
    spec = replace(file("../../sample/protostrap.yaml"), "telemetry: {}\n", "")
  }

  assert {
    condition     = length(aws_ebs_volume.telemetry) == 0 && length(aws_volume_attachment.telemetry) == 0 && length(aws_ssm_document.telemetry) == 0
    error_message = "No telemetry volume or session document without telemetry."
  }

  assert {
    condition     = !contains(keys(aws_vpc_security_group_ingress_rule.node), "telemetry")
    error_message = "No VM sends to port 24224 without telemetry."
  }

  assert {
    condition     = !strcontains(aws_s3_object.volumes["data-1"].content, "/data/telemetry")
    error_message = "data-1 mounts no telemetry volume: ${aws_s3_object.volumes["data-1"].content}"
  }

  assert {
    condition     = aws_s3_bucket.telemetry.bucket == "protostrap-123456789012-us-east-1-telemetry" && contains(keys(aws_cloudwatch_log_group.component), "api")
    error_message = "The telemetry bucket stays, and every component keeps its log group, where its output goes."
  }
}

# Without a tunnel, Cloudflare has no tunnel, hostnames or rate limit, and the ingress VMs
# reach no tunnel port. The gateway's hosts still give replicas their HOST_ variables.
run "no_tunnel" {
  command = apply

  variables {
    spec = replace(file("../../sample/protostrap.yaml"), "tunnel: {}\n", "")
  }

  assert {
    condition = (length(cloudflare_zero_trust_tunnel_cloudflared.edge) == 0 && length(cloudflare_zero_trust_tunnel_cloudflared_config.edge) == 0 &&
    length(cloudflare_dns_record.host) == 0 && length(cloudflare_ruleset.rate_limit) == 0 && length(aws_vpc_security_group_egress_rule.ingress_tunnel) == 0)
    error_message = "No tunnel, hostnames, rate limit or tunnel port without a tunnel."
  }

  assert {
    condition     = jsondecode(aws_s3_object.hosts.content) == { sample = "sample.example.com" } && output.tunnel_id == null
    error_message = "hosts.json still names the gateway's hosts; there's no tunnel to name."
  }
}

# Operator UIs reach their apps through the tunnel, so they need one.
run "operator_hosts_need_the_tunnel" {
  command = plan

  variables {
    spec           = replace(file("../../sample/protostrap.yaml"), "tunnel: {}\n", "")
    operator_hosts = { "ops.example.com" = ["ops@example.com"] }
  }

  expect_failures = [aws_s3_object.hosts]
}

# Without a gateway, there are no hosts: hosts.json is empty, and the account's config gives
# none.
run "no_gateway" {
  command = apply

  variables {
    spec  = replace(replace(replace(file("../../sample/protostrap.yaml"), "tunnel: {}\n", ""), "/(?s)gateway:\\n.*?\\n\\n/", ""), "{ requests: 20/s }, ", "")
    hosts = null
  }

  assert {
    condition     = jsondecode(aws_s3_object.hosts.content) == {} && length(cloudflare_dns_record.host) == 0 && length(local.hosts) == 0
    error_message = "No hosts without a gateway."
  }
}
