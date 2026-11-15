# Security Policy

## Supported versions

Only the `main` branch is supported. lambe is in early development with no
stable releases yet; security fixes land on `main` and are cut into the next
release.

## Reporting a vulnerability

Report privately via a GitHub security advisory
(Security → Report a vulnerability). For non-sensitive bugs, open a regular
GitHub issue. Please do not open public issues with exploit details, tokens,
or paths into your trackers.

## Trust model notes

lambe is an agent-spawning supervisor. Keep these boundaries in mind:

- **MCP trust boundary.** Anything lambe exposes over MCP (or any future
  surface) is a control plane for processes that execute code. Treat MCP
  endpoints as privileged: bind to loopback by default, and never expose them
  to untrusted networks.
- **Depth limits.** lambe supervises agents; it does not spawn supervisors of
  supervisors. Recursive agent trees are out of scope and blocked by depth
  limits rather than assumed harmless.
- **Loopback defaults.** Network listeners default to loopback interfaces.
  Changing that is an explicit operator decision.
- **Agents run with your credentials.** A supervised agent inherits the
  environment, filesystem access, and credentials of the `lambe` process.
  Scope what you hand it accordingly.
