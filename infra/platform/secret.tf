# This file holds the environment's secret (docs/secrets.md): one secret,
# <app>/<environment>, whose keys start with who they're for, such as Api_StripeKey,
# Shared_ConnectionStrings__db or Edge_TUNNEL_TOKEN. Terraform creates it empty and never
# writes a value: an Org Admin sets the keys (scripts/set-secret.sh).
#
# Every component's role reads it, and so do the edge's role and the data VMs', for the
# reporter and the scheduler. Secrets Manager can't grant one key alone, so any of them can
# read every key (docs/secrets.md#who-can-read-it).

resource "aws_secretsmanager_secret" "app" {
  name                    = "${local.app}/${var.environment}"
  description             = "The ${var.environment} environment's secret: every component's keys, each starting with the component it's for. Set by an Org Admin."
  recovery_window_in_days = 7
  tags                    = local.tags
}

# The secrets each component, the edge and the reporter had before are kept, not deleted:
# scripts/merge-secrets.sh copies their keys into the environment's secret, and an Org
# Admin deletes them after. Until every replica runs a ProtoStrap that reads the
# environment's secret, each role can still read its old one (old_secret).
locals {
  old_secret = "arn:${local.partition}:secretsmanager:${local.region}:${local.account}:secret:protostrap/"
}

removed {
  from = aws_secretsmanager_secret.component
  lifecycle {
    destroy = false
  }
}

removed {
  from = aws_secretsmanager_secret.edge
  lifecycle {
    destroy = false
  }
}

removed {
  from = aws_secretsmanager_secret.reporter
  lifecycle {
    destroy = false
  }
}
