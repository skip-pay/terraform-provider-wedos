# Security Policy

## Reporting a vulnerability

Do not open a public issue. Report the vulnerability privately through
[GitHub private vulnerability reporting](https://github.com/skip-pay/terraform-provider-wedos/security/advisories/new).

Include the provider version, what an attacker can achieve, and the steps to reproduce.

## Supported versions

Security fixes are released for the latest minor version only.

## Credentials

The provider does not log credentials. When sharing debug output (`TF_LOG=DEBUG`), check it for
WEDOS client secrets and bearer tokens before posting it anywhere.
