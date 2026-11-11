# Pull-based Agents replace push

From v0.2 every Host runs an Agent that dials out to the Control Plane and runs Compose itself; the Control Plane never connects to a Host (no SSH, no Docker over TLS). We chose this over keeping push next to agents because Hosts behind NAT and firewalls must work, secrets must stay on the Host (which needs Compose to run there, see ADR 0002), and one model means one code path to build, test and secure. It is how Argo's own agent works.

## Consequences

- Every Host needs an Agent installed (container or systemd); `import` reads stacks through the Agent, there is no SSH path.
- The Control Plane's own machine is a Host like any other: an Agent installed beside it and enrolled, replacing the
  `local` Host. (Amended 2026-11-11, [Q104](../v0.2/decisions.md#no-built-in-agent-2026-11-11): first a built-in Agent
  in the server; dropped so the Control Plane holds no engine socket.)
- v0.2 breaks with v0.1: no migration, v0.1 continues on the `release-0.1` branch for fixes.
- Agents must work without the Control Plane: they keep, restore and self-heal their apps offline (no data loss).

## Considered Options

- **Keep push, add agents as a second Host type**: two code paths forever; rejected.
- **Agent as a thin proxy for the Docker API**: push tunnelled over a socket; secrets would still be decrypted centrally; rejected.
