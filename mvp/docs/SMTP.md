# Transactional email through your own SMTP server

The live worker uses blackmail.heraldic.cloud:587 with STARTTLS, sending from
Mise En Abyme <mea@blackmail.heraldic.cloud>. MEA authentication and required
email verification are unchanged. SES sandbox limits do not apply to this mail.

The worker supports authenticated STARTTLS (normally port 587) and implicit TLS
(normally port 465), validates the server certificate and requires TLS 1.2 or
later. Plaintext connections and invalid certificates fail closed. The server
must offer AUTH PLAIN over TLS. Each attempt is limited to ten seconds, and
failed outbox entries retry through the existing watchdog, up to five attempts.
An SMTP acceptance response confirms queueing by the server, not inbox delivery.

## Configure

Use a dedicated SMTP account allowed to send from the configured address. The
server must sign that sender domain with DKIM, and its outbound IP should be
included in the domain's SPF record. Using an existing address on the mail
server's domain avoids changing miseenaby.me DNS until that domain is configured
on the mail server. Check the receiving mailbox's SPF/DKIM/DMARC results during
the real delivery test.

Copy docs/smtp.secret.example.json to ignored mvp/.env.smtp.local. Fill in the
actual host, port, tlsMode, username, password and bare from address. Keep the
password out of chat, shell arguments, committed files and Terraform variables.
Then run from mvp:

    ./scripts/configure-smtp.ps1 -CredentialsFile .env.smtp.local
    ./scripts/deploy.ps1 -PlanOnly
    ./scripts/deploy.ps1

The configuration script stores the JSON in Secrets Manager as mea-staging/smtp
and writes only its ARN to ignored infra/staging.auto.tfvars. The secret uses
the default Secrets Manager encryption key. An existing secret can also be used
by setting smtp_secret_arn directly; it must be in deployment account
681773521032 and region us-west-2. A customer-managed KMS key needs a separately
scoped kms:Decrypt grant for the worker role.

Only the worker gets GetSecretValue permission for that exact ARN. The API,
database Lambdas, desktops and Guacamole do not receive SMTP credentials. The
worker reads the current secret for every delivery attempt, allowing password
rotation without redeployment. Credentials and message tokens are not logged.
No fallback to SES occurs after SMTP is selected. Leave smtp_secret_arn empty
to select SES explicitly.

Register a new test account using an inbox you control, wait for the watchdog
(up to a minute), follow the delivered verification link, and test password
reset. Confirm the received message's authentication results and check the
mail-server queue/log if it was accepted but did not arrive. Delete test
accounts after validation. Remove the local credentials file once stored and
tested; the runtime continues reading Secrets Manager.

## Verified on 25 October 2026

blackmail.herladic.cloud did not resolve. blackmail.heraldic.cloud resolved to
35.165.243.3 and port 587 completed a TLS 1.3 STARTTLS handshake with a valid
certificate for blackmail.heraldic.cloud (expires 16 December 2026). It offered
AUTH PLAIN and LOGIN. Port 465 timed out. The owner confirmed the corrected
hostname and supplied mea@blackmail.heraldic.cloud and a private PASSWORD env
file. SMTP and IMAP authentication succeeded. The password was imported directly
into Secrets Manager in memory; it was not printed, committed or put in Terraform.

The deployed worker delivered a verification and a reset email to a unique
plus alias of that mailbox. The public API accepted each delivered link,
rejected replay, revoked the old session after reset, rejected the old password,
and accepted the new password. Both messages had DKIM signatures. The temporary
account was deactivated. Gmail/Outlook inbox placement was not tested.
Saved evidence: verification/smtp-live.json.

Repeat with an owned mailbox supporting plus aliases:

    python scripts/test-email.py --password-file ../../.env --mailbox mea@blackmail.heraldic.cloud --server blackmail.heraldic.cloud

This script creates a temporary account and sends two real messages. It reads
only mail addressed to its unique test alias, keeps credentials and link tokens
in memory, and deactivates the account in a finally block. It does not provision
EC2 desktops or use the deployment verification helper. Output goes to ignored
test-results/smtp-live.json. The source env file remains unchanged; the deployed
service reads Secrets Manager rather than that file.

The SMTP secret is externally managed, so Terraform teardown does not delete
it. Remove it separately if it is dedicated to this deployment and no longer
needed.
