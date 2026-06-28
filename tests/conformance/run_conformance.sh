#!/usr/bin/env bash
# Run s3-test-suite conformance tests against a Nexus instance.
set -euo pipefail

NEXUS_HOST="${NEXUS_HOST:-localhost}"
NEXUS_PORT="${NEXUS_PORT:-9000}"
NEXUS_ACCESS_KEY="${NEXUS_ACCESS_KEY:-nexus-test}"
NEXUS_SECRET_KEY="${NEXUS_SECRET_KEY:-nexus-test-secret}"
ENDPOINT="http://${NEXUS_HOST}:${NEXUS_PORT}"

echo "=== Nexus S3 Conformance Test Suite ==="
echo "Endpoint: ${ENDPOINT}"
echo ""

# Check if Nexus is reachable
if ! curl -sf "${ENDPOINT}/health" > /dev/null 2>&1; then
    echo "ERROR: Cannot reach Nexus at ${ENDPOINT}"
    echo "Make sure Nexus is running before executing conformance tests."
    exit 1
fi
echo "Nexus is reachable."

# Run Go conformance tests
echo ""
echo "--- Running Go conformance tests ---"
cd "$(dirname "$0")"
NEXUS_TEST_ENDPOINT="${ENDPOINT}" \
NEXUS_TEST_ACCESS_KEY="${NEXUS_ACCESS_KEY}" \
NEXUS_TEST_SECRET_KEY="${NEXUS_SECRET_KEY}" \
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
host = ${NEXUS_HOST}
port = ${NEXUS_PORT}
is_secure = False
[fixtures]
bucket prefix = conformance-{random}-
[s3 main]
access_key = ${NEXUS_ACCESS_KEY}
secret_key = ${NEXUS_SECRET_KEY}
display_name = Conformance Test User
email = conformance@test.nexus
EOF
    else
        echo ""
        echo "s3-tests directory not found at ${S3TESTS_DIR}, skipping."
        echo "Set S3TESTS_DIR to the path of ceph/s3-tests to include them."
    fi
fi

echo ""
echo "=== Conformance tests complete ==="
