# The platform root creates every AWS and Cloudflare resource an application needs from its
# protostrap.yaml (docs/protostrap.md#infra-workflow). The infra workflow applies it, with
# the Infra role, inside the boundaries bootstrap created.
#
# It never touches replicas or VM counts: those belong to the scheduler.
#
# The files are split by component, each holding everything that component needs, whatever
# the AWS service: spec.tf reads the spec; network.tf is the VPC; agent.tf is what every VM
# runs; ingress.tf, general.tf, compute.tf and data.tf are the four pools; scheduler.tf and
# reporter.tf are the programs on data-1; edge.tf is cloudflared and Envoy, and cdn.tf is
# Cloudflare in front of them; components.tf, queues.tf, files.tf, buckets.tf, backups.tf
# and deploy.tf are what the application's components, its jobs, its files, its own buckets,
# its backups and its releases need; alerts.tf tells operators when something needs them.

terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
