# Security policy

Torana is pre-release. Until the first stable release, only the current `main`
branch receives security fixes.

## Report a vulnerability privately

Do not open a public issue for a suspected vulnerability. Use this repository's
**Security** tab and choose **Report a vulnerability**. GitHub's private
vulnerability reporting keeps the report and follow-up discussion confidential.

Please include:

- the affected component and exact commit or version;
- a minimal reproduction or proof of concept;
- the security impact and required attacker access;
- whether secrets, user traffic, or third-party systems may have been exposed;
- any known workaround.

Do not include real provider credentials or customer data. Use synthetic values
in reproductions. If a live secret may have been exposed, rotate or revoke it
immediately rather than waiting for triage.

We will acknowledge the report, validate it against the supported branch, and
coordinate a fix and disclosure. Public disclosure should wait until a fix is
available or a timeline has been agreed with the maintainers.
