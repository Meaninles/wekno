# Security Policy

## Reporting a Vulnerability

The WeKnora team takes security vulnerabilities seriously.  
We appreciate your efforts to responsibly disclose any security issues you discover.

⚠️ **Please do NOT report security vulnerabilities through public GitHub issues.**

### Preferred reporting method

We recommend reporting security vulnerabilities using GitHub’s private vulnerability reporting feature:

1. Go to the **Security** tab of this repository
2. Click **“Report a vulnerability”**
3. Fill in the details and submit the report

This allows us to discuss, investigate, and fix the issue privately.

### Alternative contact

If you are unable to use GitHub’s Security Advisory feature, you may contact the maintainers through the repository owners.

> Please avoid sharing sensitive information publicly.

### What to include in your report

To help us understand and resolve the issue quickly, please include:

- A clear description of the vulnerability
- Steps to reproduce (proof-of-concept if available)
- The affected version(s)
- Potential impact and severity
- Any suggested mitigations or fixes (if known)

### Response timeline

We aim to:
- Acknowledge receipt of your report within **48 hours**
- Provide a status update as the investigation progresses

### Coordinated disclosure

We kindly ask reporters to follow responsible disclosure practices and allow us reasonable time to address the issue before any public disclosure.

Thank you for helping keep **WeKnora** and its users secure.

## Current Deployment Security Boundaries

The current enterprise fork adds multi-replica document and Agent services.
Security reviews should include the following project-specific boundaries:

- **Tenant authorization remains in Go.** Python Agent services do not connect
  directly to the WeKnora database, business databases, MCP servers, or
  object-store credentials. Every tool callback and artifact download is
  re-authorized by tenant/session/run.
- **Durable objects are private.** Development uses MinIO and production uses
  OBS. Knowledge objects, Agent artifacts, and temporary Agent inputs use
  separate deployment/namespace-scoped prefixes. Enterprise files must never
  use `public-read`.
- **Temporary inputs are bounded.** Object keys do not contain usernames or
  source filenames; objects are deleted on success/failure/cancellation/panic,
  with a prefix-only lifecycle of at most 24 hours as a hard-kill fallback.
- **Internal Agent APIs require a dedicated key.** Do not reuse JWT, model
  credentials, or OBS keys. Rotate through Kubernetes Secret management and
  never log the value.
- **Uploads are validated by the backend.** Raising Nginx/Ingress to 2304 MiB
  only permits transport of a 2048 MiB knowledge source; it does not bypass
  file type, archive/XML complexity, expansion-ratio, SSRF, or parser limits.
- **Document takeover fails closed.** Heartbeat/lease expiry is not proof that
  an old process stopped. The SystemAdmin termination-attestation endpoint must
  only be used after the exact boot is terminated or its node/runtime is
  fenced.
- **Local scratch is disposable.** App, DocReader, and Agent Pods use isolated
  RWO workspaces. Do not mount OBS/S3 as a POSIX workspace or introduce an RWX
  share containing cross-tenant intermediate files.
- **Secrets are not documentation.** Production values reference existing
  Secrets; rendered Secret values, API keys, SSO credentials, database
  passwords, and object-store keys must not be committed or pasted into issue
  reports.

Operational details are in
[`docs/custom/当前版本生产更新部署执行手册.md`](./docs/custom/当前版本生产更新部署执行手册.md)
and the architecture boundary is documented in
[`docs/custom/当前实现架构与文档索引.md`](./docs/custom/当前实现架构与文档索引.md).
