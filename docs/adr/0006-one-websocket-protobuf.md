# One WebSocket per Agent with protobuf messages

Everything between an Agent and the Control Plane (signals, payload files, status, logs) travels over one WebSocket the Agent opens, as protobuf messages in binary frames; payloads are content-addressed and only files the Agent lacks are sent. We chose one connection over a signal channel plus HTTPS downloads because it means one authentication, one port and one reconnect logic, passes any reverse proxy, and payloads are small; Argo's agent does the same. Protobuf keeps a later move to gRPC a transport swap, with the WebSocket kept as the fallback for proxies without HTTP/2.

## Consequences

- Large payloads are chunked so status and logs are never stuck behind them; transfers resume by hash.
- Authentication is a signed challenge inside the connection, so it works behind TLS-terminating proxies (no client certificates).
- Message schemas are versioned (add fields, never reuse numbers) and Agents one minor version behind are supported.
