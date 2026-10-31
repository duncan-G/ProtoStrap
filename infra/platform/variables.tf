# These variables are the small per-account config (docs/protostrap.md#infra-workflow):
# everything that differs between accounts. Everything about the application itself comes
# from its spec.

variable "spec" {
  description = "The contents of the application's protostrap.yaml: file(\"protostrap.yaml\")."
  type        = string
}

variable "environment" {
  description = "The environment this account runs, such as prod."
  type        = string
}

variable "hosts" {
  description = "Each of the spec's hosts' hostname in this account, by the spec's name, such as { app = \"app.staging.example.com\" }. It names every host the spec has, and only those. Null: the spec's hostnames."
  type        = map(string)
  default     = null
}

variable "zones" {
  description = "Availability zones to run in. Empty: the region's first. Each zone gets its own subnets, ingress VM and pool groups (docs/vms.md#multiple-zones)."
  type        = list(string)
  default     = []

  validation {
    condition     = length(var.zones) <= 4
    error_message = "At most 4 zones: each kind of subnet reserves a /26 per zone in its /24."
  }
}

variable "vpc_cidr" {
  description = "The VPC's range, a /16 (docs/network.md#addresses). It must not overlap Docker's ranges, 10.200.0.0/15 and 172.16.0.0/12, or any network it may be connected to."
  type        = string
  default     = "10.20.0.0/16"

  validation {
    condition     = can(cidrnetmask(var.vpc_cidr)) && endswith(var.vpc_cidr, "/16")
    error_message = "vpc_cidr must be a /16."
  }
}

variable "cloudflare_account_id" {
  description = "The Cloudflare account that owns the zone and the tunnel."
  type        = string
}

variable "cloudflare_zone" {
  description = "The application's domain on Cloudflare, such as example.com. It must already exist there: it is referenced, never created."
  type        = string
}

variable "email_domains" {
  description = "The domains this account sends mail from with SES. Null: the spec's email domains. Two accounts can't share a domain, since both would write its bounce. records, so an account that shares the spec with another sends from a domain of its own, such as staging.example.com."
  type        = list(string)
  default     = null
}

variable "email_dmarc" {
  description = "The DMARC record published at _dmarc.<domain> for each email domain, such as \"v=DMARC1; p=none; rua=mailto:dmarc@example.com\". Null publishes none, for a domain that has one already."
  type        = string
  default     = null
}

variable "operator_hosts" {
  description = "Host names for operator UIs, each with the emails Cloudflare Access lets through (docs/network.md#operator-uis). Each also needs a route in the spec."
  type        = map(list(string))
  default     = {}
}

variable "client_rate_limit" {
  description = "Requests a client may send in 10 seconds before Cloudflare blocks it for 10 seconds. 0: no limit. Envoy applies each route's own rate."
  type        = number
  default     = 0
}

variable "alert_emails" {
  description = "Emails that get operator alerts: AWS Health events, data VM failures, ingress VM replacements."
  type        = list(string)
  default     = []
}

variable "retired_components" {
  description = "Components removed from the spec whose ECR repository, secret, log group and role are kept, so a rollback still works (docs/protostrap-aspire.md#change-classes). Remove a name to delete them."
  type        = list(string)
  default     = []
}

variable "retired_queues" {
  description = "Queues removed from the spec that are kept with their jobs. Remove a name to delete the queue."
  type        = list(string)
  default     = []
}

variable "retired_buckets" {
  description = "Buckets removed from the spec that are kept with their objects, each with the settings it had in the spec, such as { documents = { versions = \"7d\", lock = true } }: a locked bucket can't stop being versioned. Remove one to delete the bucket."
  type = map(object({
    browser  = optional(bool, false)
    versions = optional(string)
    lock     = optional(bool, false)
    expire   = optional(string)
  }))
  default = {}

  validation {
    condition     = alltrue([for b in var.retired_buckets : !b.lock || b.versions != null])
    error_message = "A locked bucket is versioned: give it versions too."
  }
}

variable "ami" {
  description = "The image pool VMs boot from, which must be arm64: every pool is Graviton. The default resolves to the latest Amazon Linux 2023 each time a VM launches; set an AMI ID to pin one."
  type        = string
  default     = "resolve:ssm:/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-arm64"
}

variable "root_volume_gb" {
  description = "Each VM's root volume, which holds Docker's images."
  type        = number
  default     = 30
}

variable "control_volume_gb" {
  description = "data-1's control volume, which holds the swarm's state and the scheduler's key."
  type        = number
  default     = 4
}

variable "log_retention_days" {
  description = "How long components' and ProtoStrap's logs are kept."
  type        = number
  default     = 30
}

variable "backup_retention_days" {
  description = "How long the daily backups of stateful components are kept in the backups bucket."
  type        = number
  default     = 35
}

variable "snapshot_retention" {
  description = "How many daily EBS snapshots of each volume are kept."
  type        = number
  default     = 7
}

variable "backup_hour" {
  description = "The UTC hour the daily backups start."
  type        = number
  default     = 3
}

variable "cors_origins" {
  description = "Origins allowed to use signed URLs on the files bucket and every bucket with browser set, besides https:// on each host."
  type        = list(string)
  default     = []
}

variable "extra_egress" {
  description = "Outbound rules for third-party APIs on ports other than 443 (docs/network.md#security-groups)."
  type = list(object({
    description = string
    protocol    = string
    port        = number
    cidr        = string
  }))
  default = []
}

variable "protect_data_vms" {
  description = "Stop data VMs being terminated by API, Terraform included. Turn off before destroying the environment."
  type        = bool
  default     = true
}

variable "force_destroy" {
  description = "Let a destroy delete the buckets and image repositories while they still hold objects and images. For test environments only: in production, losing them by accident loses backups, files and the application's buckets. The deploy bucket is bootstrap's, and stays."
  type        = bool
  default     = false
}

variable "record_security_groups" {
  description = "Record security group changes with AWS Config and flag any that admits the internet (docs/network.md#only-ingress-vms-are-public). Off if the account already runs a Config recorder."
  type        = bool
  default     = false
}
