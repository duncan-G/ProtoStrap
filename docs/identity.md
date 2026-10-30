# Identity

Who and what calls ProtoStrap's AWS cloud resources, how each proves who it is, and the most each
may do. The components are described in
[How ProtoStrap is built](protostrap.md#components).

- **Roles.** A role is a set of permissions with no password. A program allowed to take
  one on gets temporary credentials for it.
- **No stored keys.** No IAM users or access keys. All credentials are temporary and
  renewed automatically.
- **Least privilege.** Each role may do only what its job needs, on ProtoStrap's own
  resources.
- **Boundaries.** Every role has a boundary, the most it may ever do. Only an Org Admin
  can widen one (see [Identity boundaries](#identity-boundaries)).


## Roles

| Role                      | For                                                                              |
| ------------------------- | -------------------------------------------------------------------------------- |
| **GitHub Actions Deploy** | The deploy workflow uses it to ship a release.                                   |
| **GitHub Actions Infra**  | The infra workflow uses it to create and change ProtoStrap's AWS resources.      |
| **VM roles**              | Software on the VMs uses them: the ProtoStrap agent, Docker and the SSM Agent.   |
| **Component roles**       | Each component, such as `api`, a worker or `postgres`, has one for its replicas, as do the edge and telemetry. |

### GitHub Actions Roles

The Github workflows take on their roles through GitHub's OpenID Connect provider, so no AWS key
is stored in GitHub (see
[Use IAM roles to connect GitHub Actions to actions in AWS](https://aws.amazon.com/blogs/security/use-iam-roles-to-connect-github-actions-to-actions-in-aws/)).

- **Deploy** Workflow
  - Uploads images to ECR
  - Release artifacts to S3
  - Tells the scheduler to deploy the release through SSM.
- **Infra** worfklow
  - Creates and changes ProtoStrap's AWS resources, including every other role,
  within the [Infra boundary](#identity-boundaries).

### VM Roles

- **Used by:** the ProtoStrap agent, Docker and the SSM Agent.
- **Roles:** one for general and compute VMs, one for ingress VMs, and one for data VMs. The
  ingress role also lets an ingress VM take over its zone's Elastic IP and route.
  - Schedular and reporter live on a data VM and data VMs need to hold their needed permissions.
- **Not for individual services:** containers can't reach the metadata service.

### Component roles

- **One per component,** allowing only what it uses: a service that sends to one queue
  can't touch any other.
- **Built by Infra** from what the component declares in the [spec](protostrap.md#spec).
- **ProtoStrap's own:** the edge's reads the environment's secret, for the tunnel token.
  Telemetry's reads and writes the telemetry bucket, and nothing else: no secret.
- **Why:** components share VMs, so a role per VM would give every replica everything any
  of them needs.
- **Limit:** root on a VM can still take on every role that VM can.
- **Secrets:** every component role reads the environment's secret, which holds every
  component's keys: Secrets Manager can't grant one key alone (see
  [Who can read it](secrets.md#who-can-read-it)).

#### How replicas get credentials

- **The ProtoStrap agent takes on the component's role** with its VM's role when it starts a replica.
- **It serves the credentials to that replica alone,** at the address the AWS SDKs check
  for container credentials, as Amazon ECS does. No code changes.
- **They last at most an hour.** If the agent stops for longer, its replicas lose AWS
  access.
- **CloudTrail records each call** under its component.


## People

People sign in through IAM Identity Center, with MFA, for credentials that last the
session.

| Group              | Can                                                         | Session  |
| ------------------ | ----------------------------------------------------------- | -------- |
| **Org Admin**      | Everything (AdministratorAccess)                            | 4 hours  |
| **Platform Admin** | View every account read-only, and open the operator UI and Grafana | 12 hours |
| **Developer**      | View read-only, manage development secrets, and open Grafana | 12 hours |

- **Org Admins** set the platform's secret values, change boundaries and handle anything
  else, in every account.
- **Platform Admins** watch the platform, in every account: they see it read-only
  (ViewOnlyAccess) and open the operator UI and Grafana. They change nothing, and can't
  read or set secret values.
- **Developers** run services locally, which read development secrets with the
  developer's sign-in. Code reaches the platform only through GitHub Actions. They sign in
  only where they develop (`developer_environments` in `infra/people`). There they open
  Grafana, to read their services' logs, traces and metrics, but not the operator UI.
- **The operator UI** and its logs API are reached through an SSM port forward to data-1
  (see [Operator UI](protostrap.md#operator-ui)), and only Org Admins and Platform Admins
  may open it. The session document a person may start is their role there. Platform
  Admins may start only `protostrap-ui`, the read-only view. Org Admins may start any
  document, `protostrap-ui-admin` included, the port that will hold their actions.
- **Grafana** is reached the same way, through its own document, `protostrap-telemetry`,
  which forwards its port and nothing else (see
  [Reading telemetry](telemetry.md#reading-telemetry)). Developers, Platform Admins and Org
  Admins may start it. It shows service telemetry only: the infra log stays with the
  operator UI ([Two kinds of telemetry](telemetry.md#two-kinds-of-telemetry)).
- **The root user** has MFA and no access keys, and is used only for tasks that need it.


## Identity boundaries

- **A policy** says what a role may do. **A boundary** says the most it may ever do. AWS
  allows a call only if both allow it.
- **Only Org Admins change boundaries,** so a bad policy or a compromised workflow can't
  go beyond one.

| Boundary           | Applies to               | Allows                                                                                                                                                                       |
| ------------------ | ------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Infra boundary** | The Infra role           | Building ProtoStrap: its network and VMs, Auto Scaling, ECR, S3, SQS, SES identities, SSM, CloudWatch, EventBridge and SNS for alerts, and IAM roles. Creating secrets, but not reading them |
| **Role boundary**  | Every role Infra creates | Running ProtoStrap: ECR, S3, SQS, sending mail with SES, SSM, Auto Scaling, CloudWatch, reading secrets, and taking on component roles                                        |

- **Both** allow only ProtoStrap's own resources, and deny every other AWS service.
- **Kept separate,** so a role that runs ProtoStrap can never change its network or IAM.
- **Infra's IAM is limited too.** It creates and changes only ProtoStrap's roles, always
  with the role boundary attached. It can't change either boundary, or create IAM users or
  access keys.

### Adding to ProtoStrap

- **Within Infra's boundary:** change the infra config and merge. The infra workflow
  applies it.
- **Outside it:** an Org Admin widens the boundary first, or the workflow fails with
  access denied.

Ex:
| Change                         | Infra boundary  | Role boundary                |
| ------------------------------ | --------------- | ---------------------------- |
| **Add an S3 bucket**           | —               | —                            |
| **Add a service or worker**    | —               | —                            |
| **Move Redis to ElastiCache**  | Add ElastiCache | — (reached over the network) |
| **Send email with Amazon SES** | — ([Email](email.md)) | —                      |
