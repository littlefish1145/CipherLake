#!/usr/bin/env bash
# Run s3-test-suite conformance tests against a CipherLake instance.
set -euo pipefail

CIPHERLAKE_HOST="${CIPHERLAKE_HOST:-localhost}"
CIPHERLAKE_PORT="${CIPHERLAKE_PORT:-9000}"
CIPHERLAKE_ACCESS_KEY="${CIPHERLAKE_ACCESS_KEY:-cipherlake-test}"
CIPHERLAKE_SECRET_KEY="${CIPHERLAKE_SECRET_KEY:-cipherlake-test-secret}"
ENDPOINT="http://${CIPHERLAKE_HOST}:${CIPHERLAKE_PORT}"

echo "=== CipherLake S3 Conformance Test Suite ==="
echo "Endpoint: ${ENDPOINT}"
echo ""

# Check if CipherLake is reachable
if ! curl -sf "${ENDPOINT}/health" > /dev/null 2>&1; then
    echo "ERROR: Cannot reach CipherLake at ${ENDPOINT}"
    echo "Make sure CipherLake is running before executing conformance tests."
    exit 1
fi
echo "CipherLake is reachable."

# Run Go conformance tests
echo ""
echo "--- Running Go conformance tests ---"
cd "$(dirname "$0")"
CIPHERLAKE_TEST_ENDPOINT="${ENDPOINT}" \
CIPHERLAKE_TEST_ACCESS_KEY="${CIPHERLAKE_ACCESS_KEY}" \
CIPHERLAKE_TEST_SECRET_KEY="${CIPHERLAKE_SECRET_KEY}" \
go test -v -timeout 300s ./...

# Run s3-tests (if available)
if command -v venv &> /dev/null || command -v python3 &> /dev/null; then
    S3TESTS_DIR="${S3TESTS_DIR:-/opt/s3-tests}"
    if [ -d "${S3TESTS_DIR}" ]; then
        echo ""
        echo "--- Running Ceph s3-tests ---"
        cd "${S3TESTS_DIR}"
        S3TEST_CONF=/dev/stdin nosetests -v 2>&1 <<EOF
[DEFAULT]
host = ${CIPHERLAKE_HOST}
port = ${CIPHERLAKE_PORT}
is_secure = False
[fixtures]
bucket prefix = conformance-{random}-
[s3 main]
access_key = ${CIPHERLAKE_ACCESS_KEY}
secret_key = ${CIPHERLAKE_SECRET_KEY}
display_name = Conformance Test User
email = conformance@test.cipherlake
EOF
    else
        echo ""
        echo "s3-tests directory not found at ${S3TESTS_DIR}, skipping."
        echo "Set S3TESTS_DIR to the path of ceph/s3-tests to include them."
    fi
fi

echo ""
echo "=== Conformance tests complete ==="
