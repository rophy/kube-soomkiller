#!/bin/bash
# Run all e2e tests
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
cd "$SCRIPT_DIR"

# Default context
export KUBE_CONTEXT="${KUBE_CONTEXT:-k3s}"

echo "Running e2e tests with context: $KUBE_CONTEXT"
echo ""

# Check prerequisites
if ! command -v bats &> /dev/null; then
    echo "Error: bats is not installed"
    echo "Install with: apt install bats"
    exit 1
fi

if ! kubectl --context "$KUBE_CONTEXT" get nodes &> /dev/null; then
    echo "Error: Cannot connect to Kubernetes cluster with context '$KUBE_CONTEXT'"
    exit 1
fi

# Setup: Deploy with skaffold and prepare sysbench
echo "Setting up test environment..."
(cd "$PROJECT_ROOT" && skaffold run --kube-context "$KUBE_CONTEXT") || {
    echo "Error: skaffold deploy failed"
    exit 1
}

kubectl --context "$KUBE_CONTEXT" rollout status daemonset/kube-soomkiller \
    -n kube-soomkiller --timeout=120s

echo "Waiting for MariaDB to be ready..."
kubectl --context "$KUBE_CONTEXT" wait --for=condition=Ready pod/mariadb-0 \
    -n kube-soomkiller --timeout=120s

# Wait for MariaDB to accept connections
echo "Waiting for MariaDB to accept connections..."
for i in {1..30}; do
    if kubectl --context "$KUBE_CONTEXT" exec -n kube-soomkiller mariadb-0 -- \
        mariadb -uroot -ptestpass -e "SELECT 1" &>/dev/null; then
        break
    fi
    sleep 2
done

echo "Preparing sysbench database..."
kubectl --context "$KUBE_CONTEXT" exec -n kube-soomkiller mariadb-0 -- \
    mariadb -uroot -ptestpass -e "CREATE DATABASE IF NOT EXISTS sbtest;"

kubectl --context "$KUBE_CONTEXT" exec -n kube-soomkiller deploy/sysbench -- \
    sysbench /usr/share/sysbench/oltp_read_write.lua \
    --mysql-host=mariadb --mysql-port=3306 \
    --mysql-user=root --mysql-password=testpass \
    --mysql-db=sbtest --tables=10 --table-size=100000 prepare 2>&1 || echo "Tables may already exist"

echo ""
echo "Running tests..."

# Run tests
bats --timing *.bats "$@"
