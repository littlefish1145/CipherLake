#!/usr/bin/env bash
set -euo pipefail

# Build the Rust plugin for wasm32-wasip1 and copy the artifact next to the manifest.
cargo build --target wasm32-wasip1 --release
cp target/wasm32-wasip1/release/mcp-memory-server.wasm ./plugin.wasm

echo "Built plugin.wasm"
