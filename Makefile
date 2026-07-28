.PHONY: test test-unit test-bench test-soak bench bench-tier2 conformance conformance-force build build-plugins rust-plugin

# Default: run all unit tests
test: test-unit

# Run all unit tests (short mode, no integration/soak)
test-unit:
	go test ./... -short -count=1

# Run benchmarks (quick, 1 iteration per bench)
test-bench:
	go test ./internal/pluginloader/ -bench=BenchmarkState -benchtime=1x -count=1

# Run the soak/stability test (default 30s; set SOAK_DURATION=24h for full run)
test-soak:
	go test ./internal/pluginloader/ -run TestSoak_PluginLoaderStability -count=1 -timeout=30m

# Run all benchmarks with reasonable benchtime
bench:
	go test ./internal/pluginloader/ -bench=. -benchtime=100x -count=1

# Run Tier-2-only host import benchmarks
bench-tier2:
	go test ./internal/pluginloader/ -bench=BenchmarkIAMPolicyEvaluate -benchtime=100x
	go test ./internal/pluginloader/ -bench=BenchmarkKMSDataKeyGenerate -benchtime=100x
	go test ./internal/pluginloader/ -bench=BenchmarkCryptoAuditSign -benchtime=100x
	go test ./internal/pluginloader/ -bench=BenchmarkGatewayHookRegister -benchtime=100x

# Run S3 conformance tests (requires running CipherLake gateway on localhost:9000)
conformance:
	./tests/conformance/run_conformance.sh

# Force conformance tests even if gateway is unreachable
conformance-force:
	CIPHERLAKE_FORCE_CONFORMANCE=1 go test ./tests/conformance/... -v -timeout=300s

# Build all Go binaries
build:
	go build ./...

# Build the Rust mcp-memory-server example plugin
build-plugins: rust-plugin

# Build the Rust mcp-memory-server WASM plugin
rust-plugin:
	cd examples/plugins/mcp-memory-server-rust && cargo build --target wasm32-wasip1 --release
	cp examples/plugins/mcp-memory-server-rust/target/wasm32-wasip1/release/mcp-memory-server.wasm examples/plugins/mcp-memory-server-rust/plugin.wasm

# Full soak test (24 hours)
soak-24h:
	SOAK_DURATION=24h go test ./internal/pluginloader/ -run TestSoak_PluginLoaderStability -count=1 -timeout=25h -v
