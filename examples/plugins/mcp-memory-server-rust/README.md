# mcp-memory-server (Rust example plugin)

A **standalone Rust project** demonstrating a Nexus plugin with:

- `wasm_init` / `on_request` / `on_hook` entry points
- Host imports: `request.read`, `request.body`, `route.response-write`, `vector.search`, `state.get`, `state.put`
- MCP (Model Context Protocol) SSE + JSON-RPC routes
- A `/memory/search` route that exercises `vector:search`

This crate lives outside the main Nexus Go module at `examples/plugins/mcp-memory-server-rust/`, so it can be built, versioned, and packaged independently.

## Build

Requires [Rust](https://rustup.rs/) with the `wasm32-wasip1` target:

```bash
rustup target add wasm32-wasip1
cargo build --target wasm32-wasip1 --release
# Or use the convenience script:
./build.sh
```

The artifact is copied to `plugin.wasm` next to `manifest.json`.

## Install into a running Loader

Phase 1 installs plugins through the admin API (P3-1). For local testing you
can use a Go test helper that calls `Loader.InstallPlugin` with `manifest.json`
and `plugin.wasm`.

## Routes

| Route | Behavior |
|---|---|
| `/mcp/sse` | Returns an SSE `endpoint` event pointing at `/mcp/messages` |
| `/mcp/messages` | Handles MCP JSON-RPC: `initialize`, `tools/list`, `tools/call` |
| `/memory/search` | Demonstrates `vector.search` host import |

## End-to-end smoke test

With the Nexus gateway listening on `:8080` and the plugin installed as
`mcp-memory-server`:

```bash
curl -N http://localhost:8080/_plugins/mcp-memory-server/mcp/sse
curl -X POST http://localhost:8080/_plugins/mcp-memory-server/mcp/messages \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"initialize","id":1}'
```
