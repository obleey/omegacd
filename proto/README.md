# Protobuf schemas

- `omegacd/agent/v1`: Agent ↔ Control Plane messages, one `Envelope` per WebSocket frame.
- `omegacd/source/v1`: the Source Server's gRPC service, used by the Control Plane.

Generated Go lives in `internal/proto/` and is committed. After changing a `.proto`, run `make proto` (buf lints and
generates; tool versions are pinned in the Makefile and `buf.gen.yaml`) and commit the result; CI fails if the
generated code is out of date.

## Versioning

Agents one minor version behind must keep working (Q41), so within `v1`:

- **Add** fields and messages; never change a field's type or meaning.
- **Never reuse** a field number or name: when removing a field, `reserved` both.
- Old readers ignore new fields; a new reader sees zero values from an old writer, so zero must mean "not sent".
- A change that can't follow these rules is a new package (`v2`) and a new `protocol_version`.
