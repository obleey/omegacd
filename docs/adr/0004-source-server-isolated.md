# A separate Source Server owns Git and rendering

Fetching Git, validating and rendering an Application's Desired State is done by the Source Server, a separate component that alone holds Git credentials and has no database, no keys and no Docker access; only the Control Plane may call it (gRPC with mutual TLS), and Agents get payloads through the Control Plane. We chose isolation because a breach of the part that talks to the outside world (Git hosts, rendering of repository content, later templating) should not expose Hosts or secrets, and a breach of the Control Plane should not expose Git credentials. It renders with secrets still encrypted; the Agent does the final render.

## Consequences

- It runs in-process by default and standalone for production (one setting); the isolation only exists standalone.
- Repository credentials are passed through the Control Plane and stored sealed by the Source Server.
- The Control Plane keeps every payload it delivered, so a Source Server outage only delays new Revisions.

## Considered Options

- **Git and rendering inside the Control Plane** (v0.1): simplest, but one breach exposes everything.
- **Agents fetch Git themselves** (Argo's agent clusters run their own repo-server): spreads Git credentials to every Host; rejected.
