# Appliance security model

## Network zones

The recommended deployment uses three logical paths:

1. **Client ingress** allows internal agents and applications to reach the
   OpenAI-compatible listener.
2. **Management ingress** limits `/console/` and administrative API routes to
   an operator VLAN, VPN or reverse proxy.
3. **Provider egress** allows only the appliance identity to reach approved AI
   providers. Client networks are denied direct AI-provider egress by the
   surrounding firewall.

TLS interception and provider-domain classification belong to the firewall and
are not implemented by the gateway.

## Credentials

Provider credentials are stored in an AES-256-GCM encrypted document. The
encrypted vault and its key are separate files. The key is created with mode
`0600`; appliance builds should supply it from a TPM-backed filesystem or
platform secret store. Neither list nor create responses return stored secrets.

Back up the vault and key independently. Losing the key makes the vault
deliberately unrecoverable. Copying both files together removes most of the
benefit of encryption at rest.

## Administrative access

The console exchanges an administrator password for a short-lived signed,
HttpOnly, `SameSite=Strict` session cookie. Unsafe requests require a random
per-session CSRF token. Login attempts are rate limited by source address.

Set `console.admin_password_sha256` to a SHA-256 password digest and enable
`console.secure_cookies` behind HTTPS. If the digest is omitted, the master key
acts as a bootstrap password. That fallback is intended only for initial setup.

The bearer master key remains available for local automation. It must never be
given to agents or applications; create a scoped virtual identity for each
runtime principal instead.

## Fail-closed controls

- Authentication, governance snapshots, provider/model ACLs and budgets fail
  closed.
- The appliance example enables PII, secret and injection scanners with
  `guardrail.fail_open: false`.
- Full prompt logging is disabled in the appliance example.
- Missing environment-variable placeholders fail configuration loading.
- Readiness fails when no model is installed or the governance store cannot be
  read.

## Data handling

Spend records contain identity, provider, model, token count, latency, status
and calculated cost. Security events intentionally do not store detected PII
or secret values. Full request-body logging is optional and should remain off
unless an explicit retention and access policy exists.
