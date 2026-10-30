# The operator UI (docs/protostrap.md#operator-ui) is served by the scheduler on data-1's
# loopback and has no sign-in of its own. Org Admins and Platform Admins reach it through an
# SSM port forward, and these session documents are the only ones the people root lets
# them start for it: each forwards one fixed port on data-1, so the document a person may
# start is their role. Developers may start neither.
#
#   protostrap-ui        127.0.0.1:7480   Platform Admins and Org Admins: the read-only view and logs API
#   protostrap-ui-admin  127.0.0.1:7481   Org Admins only: the same view today, and their actions later
#
# The ports are the scheduler's defaults, ui_listen and ui_admin_listen, which Terraform
# doesn't write into scheduler.json (cmd/protostrap/scheduler.go says why).

locals {
  ui_ports = { "protostrap-ui" = 7480, "protostrap-ui-admin" = 7481 }
}

resource "aws_ssm_document" "ui" {
  for_each        = local.ui_ports
  name            = each.key
  document_type   = "Session"
  document_format = "JSON"
  tags            = local.tags
  content = jsonencode({
    schemaVersion = "1.0"
    description   = "Forward a local port to the ProtoStrap operator UI on data-1, port ${each.value}."
    sessionType   = "Port"
    # Only the local port is the caller's to pick; the remote port is fixed.
    parameters = {
      localPortNumber = {
        type           = "String"
        description    = "The port on your machine. An open one is picked if it is 0."
        allowedPattern = "^([0-9]|[1-9][0-9]{1,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])$"
        default        = "0"
      }
    }
    properties = {
      portNumber      = tostring(each.value)
      type            = "LocalPortForwarding"
      localPortNumber = "{{ localPortNumber }}"
    }
  })
}
