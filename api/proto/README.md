# gRPC schema

The wire protocol between `creek` CLI (TypeScript, in main monorepo) and `creekd` (Go, this repo) lives here as `.proto` files. Both repos generate code from these protos — single source of truth.

## Files (to be added in M5)

- `supervisor.proto` — deploy, undeploy, list, restart, status
- `logs.proto` — log streaming (server-streaming RPC)
- `health.proto` — supervisor health, individual app health

## Code generation

```bash
# Go side (this repo)
protoc --go_out=. --go-grpc_out=. api/proto/*.proto

# TypeScript side (monorepo)
# See packages/cli for ts-proto-codegen invocation
```

## Versioning

Proto files are versioned via package namespace:
- `creek.v1` — stable
- `creek.v2` — breaking changes (parallel to v1 during migration)

Never break `creek.v1` after Phase 1 launch. New features are additive (new methods, new optional fields).
