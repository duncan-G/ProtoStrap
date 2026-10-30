# Email

An application sends mail with Amazon SES. It lists the domains it sends from, and each
component that sends has `email` in its `uses`. That component sends with the SES API and its
own role's credentials, like any other AWS call, so no SMTP password exists to set, leak or
rotate.

| To                                        | See                                                     |
| ----------------------------------------- | ------------------------------------------------------- |
| Turn email on                             | [The spec](#the-spec)                                   |
| See what ProtoStrap creates               | [What ProtoStrap creates](#what-protostrap-creates)     |
| Set an account's domains, DMARC or limits | [Each account](#each-account)                           |
| Send from a service, worker or task       | [Sending](#sending)                                     |
| Send from software that only speaks SMTP  | [Software that only speaks SMTP](#software-that-only-speaks-smtp) |
| Send mail on a laptop                     | [Locally](#locally)                                     |


## The spec

```yaml
email:
  domains: [shop.example.com]

components:
  api:
    uses: [orders, email]
```

- **`domains`** are the domains the application sends from, such as `shop.example.com` for
  `orders@shop.example.com`. Each must be in the application's Cloudflare zone, where its
  DNS records go. There's no wildcard: a subdomain that sends is listed on its own.
- **`email` in `uses`** lets the component send from any address in the domains, and from
  nowhere else. A component without it can't send at all.
- **One `email` section** holds every domain. `email` names it in `uses`, so no queue or
  bucket can be called `email`.

With [ProtoStrap.Hosting](protostrap-aspire.md), `AddProtoStrapEmail("email", "shop.example.com")`
writes the section, and `.WithReference(email)` adds `email` to a component's `uses`.

`protostrap check` refuses a domain that isn't a domain, a domain listed twice, an empty
`domains`, and `email` in `uses` without an `email` section.


## What ProtoStrap creates

The [infra workflow](protostrap.md#infra-workflow) creates, for each domain:

- **An SES identity**, signing with Easy DKIM, 2048-bit.
- **A MAIL FROM domain, `bounce.<domain>`.** Bounces and complaints come back there, under
  the application's own domain, so the envelope sender passes SPF and lines up with the
  From address for DMARC. If its MX record is missing, SES refuses the send rather than fall
  back to `amazonses.com`, which DMARC would fail.
- **Its DNS records** in the Cloudflare zone, unproxied:

  | Name                               | Type  | Content                                    |
  | ---------------------------------- | ----- | ------------------------------------------ |
  | `<token>._domainkey.<domain>`, × 3 | CNAME | `<token>.dkim.amazonses.com`               |
  | `bounce.<domain>`                  | MX 10 | `feedback-smtp.<region>.amazonses.com`     |
  | `bounce.<domain>`                  | TXT   | `"v=spf1 include:amazonses.com ~all"`      |
  | `_dmarc.<domain>`                  | TXT   | `email_dmarc`, only if the account sets it |

For each component with `email` in its `uses`, its [role](identity.md#component-roles) may
call `ses:SendEmail` and `ses:SendRawEmail` on those identities, with a From address in the
domains (`ses:FromAddress`). Both [boundaries](identity.md#identity-boundaries) allow SES: Infra to
manage identities, and the role boundary to send.

SES verifies a domain once its DKIM records resolve, usually within minutes of the apply and
at most 72 hours. Until then, a send fails.


## Each account

Set these in the account's platform root ([infra/README.md](../infra/README.md)):

- **`email_domains`** replaces the spec's domains in this account. Two accounts can't send
  from one domain, since both would write its `bounce.` records, so dev sends from one of
  its own, such as `dev.shop.example.com`, and lists it in its own Cloudflare zone.
- **`email_dmarc`** is the DMARC record to publish at `_dmarc.<domain>`, such as
  `v=DMARC1; p=none; rua=mailto:dmarc@shop.example.com`. A domain has one DMARC record, and
  it may have one already from another mail service, so ProtoStrap publishes none unless
  this is set. Start at `p=none`, read the reports, then tighten it to `quarantine` or
  `reject`.
- **Production access.** A new account's SES is in the sandbox: it sends only to verified
  addresses, and at most 200 messages a day. An Org Admin asks AWS to lift that, once per
  account, in the SES console or with
  `aws sesv2 put-account-details --production-access-enabled --mail-type TRANSACTIONAL ...`.
  AWS reviews the request, which can take a day. Dev can stay in the sandbox and send to
  verified test addresses.

SES's account-level suppression list is on by default, so an address that bounced or
complained isn't sent to again. ProtoStrap doesn't yet route bounce or complaint events
anywhere: watch the rates on SES's reputation dashboard, since AWS pauses sending when they
climb.


## Sending

A replica sends with the SES v2 API's `SendEmail`. The AWS SDK finds its credentials and
region itself, from the agent (`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`) and `AWS_REGION`
([Application contract](protostrap.md#application-contract)). The From address is the
application's own setting, in any of the domains.

.NET, with `AWSSDK.SimpleEmailV2`:

```csharp
await ses.SendEmailAsync(new SendEmailRequest
{
    FromEmailAddress = "Shop <orders@shop.example.com>",
    Destination = new Destination { ToAddresses = [to] },
    Content = new EmailContent { Simple = new Message
    {
        Subject = new Content { Data = subject, Charset = "UTF-8" },
        Body = new Body { Text = new Content { Data = text, Charset = "UTF-8" },
                          Html = new Content { Data = html, Charset = "UTF-8" } },
    }},
}, ct);
```

Node.js, with `@aws-sdk/client-sesv2`:

```js
await ses.send(new SendEmailCommand({
  FromEmailAddress: "Shop <orders@shop.example.com>",
  Destination: { ToAddresses: [to] },
  Content: { Simple: { Subject: { Data: subject }, Body: { Text: { Data: text }, Html: { Data: html } } } },
}));
```

Python, with boto3:

```python
boto3.client("sesv2").send_email(
    FromEmailAddress="Shop <orders@shop.example.com>",
    Destination={"ToAddresses": [to]},
    Content={"Simple": {"Subject": {"Data": subject}, "Body": {"Text": {"Data": text}, "Html": {"Data": html}}}},
)
```

- **Send from a job, not a request,** when the mail can wait: SES can throttle, and a retry
  on the queue is safer than a slow request.
- **A send that fails throws.** The SDK retries throttling itself. A message to a
  suppressed address is accepted and then dropped, so a send that succeeds isn't proof of
  delivery.
- **Never log the recipient's address.** Log what was sent and that it succeeded.


## Software that only speaks SMTP

SES's SMTP interface needs a password derived from an IAM user's access key, and ProtoStrap
never creates IAM users or keys. Software that only speaks SMTP needs a way to send through
the API instead:

- **Keycloak** has an email sender SPI. A provider in the image's `providers/` directory
  that calls SES, selected with `KC_SPI_EMAIL_SENDER__PROVIDER=<its id>` before
  `kc.sh build`, sends every message Keycloak sends: verification, password resets and
  one-time codes. The realm's `smtpServer` still supplies the From address and display name.
  The provider must bundle the AWS SDK for Java, since Keycloak loads only what's in
  `providers/`.
- **Anything else** gets a sender of its own in the same way, or the application asks for
  an SMTP relay in ProtoStrap, which doesn't exist yet.


## Locally

`aspire run` has no SES. `AddProtoStrapEmail` does nothing in run mode, and a reference to it
sets no variable. A component that sends picks its sender from its own settings: an SMTP
host, such as smtp4dev's, when one is set, which only the AppHost sets in run mode, and SES
otherwise. Keycloak keeps its built-in SMTP sender locally, since only the deployed image
selects the SES provider.
