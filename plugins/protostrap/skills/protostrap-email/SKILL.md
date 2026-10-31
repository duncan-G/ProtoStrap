---
name: protostrap-email
description: Send email from an application deployed with ProtoStrap, with Amazon SES - sign-up and password-reset mail, notifications, one-time codes, Keycloak's mail. Covers the spec's email section and `uses: [email]` (or AddProtoStrapEmail in an Aspire AppHost), sending with the SES v2 API and the component's own role from .NET, Node.js or Python, Keycloak and other software that only speaks SMTP, sending locally, each account's email_domains and email_dmarc, and SES production access. Use whenever code needs to send mail, a component starts or stops sending, a From domain changes, or mail isn't arriving.
---

# Email

ProtoStrap sends mail with Amazon SES. The spec lists the domains the application sends
from. Each component that sends has `email` in its `uses`, and sends with the **SES v2 API
and its own role's credentials**. There is no SMTP password anywhere: no IAM user, no access
key, no key in the secret.

The design is `docs/email.md` at the spec's `protostrap:` version: `.protostrap/docs/email.md`
or `../ProtoStrap/docs/email.md` if checked out at `v<version>`, otherwise
`https://github.com/duncan-G/protostrap/blob/v<version>/docs/email.md`. Where it and this
skill disagree, it wins.

## Rules for you

- **Never create SMTP credentials.** Don't add an IAM user, an access key, an SES SMTP
  password or an `Smtp__Password` key, and don't ask an Org Admin for one. ProtoStrap's
  boundaries forbid IAM users, and the component's role is already allowed to send.
- **Never handle a recipient list or a real address in code you commit,** test fixtures
  included, and never log a recipient. Log what was sent and that it succeeded.
- **The From address must be in an email domain.** The role may send only from
  `*@<domain>`, so a From address elsewhere fails with `AccessDenied`. Read it from
  configuration, not a constant, since another deployed environment may send from another
  domain.
- **Nothing acts on an account.** The infra workflow creates the identities and records.
  Production access is an Org Admin's request to AWS. Write the steps; don't run them.

## Turning it on

1. **The spec.** With a hand-written `protostrap.yaml`:

   ```yaml
   email:
     domains: [shop.example.com]

   components:
     api:
       uses: [orders, email]
   ```

   With an Aspire AppHost, the spec is generated, so change the AppHost instead
   (protostrap-aspire skill):

   ```csharp
   var email = builder.AddProtoStrapEmail("email", "shop.example.com");
   builder.AddProject<Projects.Shop_Api>("api").WithReference(email);
   ```

   - Each domain must be in the application's Cloudflare zone, the platform root's
     `cloudflare_zone`, or the infra plan fails.
   - List a subdomain that sends on its own, and never a wildcard.
   - Keep one email section, with every domain. No queue or bucket may be called `email`.
2. **Each account.** Two accounts can't share a domain: both would write its `bounce.`
   records. If a second deployed environment shares prod's spec, its platform root sets a
   domain of its own, in a zone it manages:

   ```hcl
   email_domains = ["staging.shop.example.com"]
   ```

   Set `email_dmarc` only where nothing else publishes DMARC for the domain. Check first with
   `dig +short TXT _dmarc.<domain>`. Start at `"v=DMARC1; p=none; rua=mailto:<address>"`.
3. **Production access.** A new account's SES only sends to verified addresses, 200 a day.
   For prod, write the Org Admin the request: the SES console's "Request production access",
   or `aws sesv2 put-account-details --production-access-enabled --mail-type TRANSACTIONAL
   --website-url <url> --use-case-description "<what the mail is and who asked for it>"`.
   AWS reviews it, often within a day. Another deployed environment can stay in the sandbox,
   with test addresses verified in SES.
4. **The code** sends with SES (below), and its settings hold the From address.

The pull request says it **runs the infra workflow**. It adds, for each domain, an SES
identity, three DKIM CNAMEs, and MX and SPF records on `bounce.<domain>`, plus the send
permission for each component that uses email. That changes the roles, not the replicas.
The domain verifies once DKIM resolves, usually within minutes; sends fail until it does.

## Sending

Use the SES **v2** API's `SendEmail` with the SDK's default credentials and region. The agent
provides both (`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`, `AWS_REGION`), so pass no keys and
no endpoint.

- **.NET:** `AWSSDK.SimpleEmailV2`. Register `IAmazonSimpleEmailServiceV2` with
  `AddAWSService<IAmazonSimpleEmailServiceV2>()` (`AWSSDK.Extensions.NETCore.Setup`) and call
  `SendEmailAsync` with `Content.Simple` holding the subject and a `Body` with `Text` and,
  optionally, `Html`.
- **Node.js:** `@aws-sdk/client-sesv2`, `new SESv2Client({})` and `SendEmailCommand`.
- **Python:** `boto3.client("sesv2").send_email(...)`.

Worked examples of all three are in `docs/email.md#sending`.

- **Hide the sender behind an interface** the code already calls, such as an `IEmailSender`,
  with an SES implementation and an SMTP one for local runs (below).
- **Mail that can wait goes through a job** (protostrap-contract skill): SES can throttle,
  and a queued retry is safer than a slow request.
- **A success isn't a delivery.** SES accepts mail to an address on its suppression list and
  drops it. Don't build a flow that assumes the mail arrived.

## Software that only speaks SMTP

Software that only speaks SMTP can't use the role. Give it a sender that calls the SES API:

- **Keycloak:** an email sender provider, a Java class implementing
  `org.keycloak.email.EmailSenderProvider`. It implements `send(config, address, subject,
  textBody, htmlBody)` and `validate(config)`, plus a factory with an id such as `ses`. Put
  it in the image's `providers/`, bundled with the AWS SDK for Java's `sesv2` and
  `url-connection-client`. The image's Dockerfile selects it before building:

  ```dockerfile
  ENV KC_SPI_EMAIL_SENDER__PROVIDER=ses
  RUN /opt/keycloak/bin/kc.sh build
  ```

  - Build the SES client lazily, on the first send, so Keycloak starts where there's no AWS
    region (with the sender selected at build time, only its factory is loaded).
  - The selection is build time: `KC_SPI_EMAIL_SENDER__PROVIDER` (two underscores; one is
    ignored). Under `aspire run`, `start-dev` rebuilds at start, so setting it to `default` there
    sends with Keycloak's SMTP sender; `start --optimized` refuses a different value.
  - The realm's `smtpServer` still supplies `from` and `fromDisplayName`.
  - The Keycloak component has `email` in its `uses` like any other.
  - The SPI must match the server's version, so check the interface on each Keycloak upgrade.
- **Anything else:** look for a pluggable sender or an HTTP API mode first. If there's none,
  say so to the user. ProtoStrap has no SMTP relay, and SMTP credentials aren't an option.

## Locally

`aspire run` has no SES, and `AddProtoStrapEmail` does nothing there. The AppHost runs a mail
catcher, such as smtp4dev, in run mode only, and passes its host to the components that send.
The code picks its sender from configuration:

- **SMTP** when an SMTP host is set, which only the AppHost does, locally.
- **SES** otherwise, which is every deployed environment.

For Keycloak, the deployed image selects the SES provider and the local container doesn't,
so it keeps its built-in SMTP sender against the mail catcher.

## When mail doesn't arrive

Check these in order, read-only (protostrap-operate skill for the logs):

1. **`AccessDenied` in the component's log:** the component lacks `email` in its `uses`, or
   its From address isn't in an email domain, or the infra workflow hasn't run since the
   spec changed.
2. **`MailFromDomainNotVerified` or "identity not verified":** DKIM or the `bounce.` MX
   hasn't resolved yet, or the domain's records were changed by hand in Cloudflare.
3. **"Email address is not verified" while sending to an outside address:** the account is
   still in the sandbox, so it needs production access (step 3).
4. **Accepted but never arrives:** the address is on SES's suppression list after an earlier
   bounce or complaint, or the receiving side filtered it. Check DMARC alignment: the From
   domain must be an email domain, and DKIM signs only those.
