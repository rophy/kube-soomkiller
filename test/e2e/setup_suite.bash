#!/bin/bash
# Suite-level setup - runs once before all tests

setup_suite() {
    # Get project root (two levels up from test/e2e/)
    local project_root
    project_root="$(cd "$(dirname "$BATS_TEST_FILENAME")/../.." && pwd)"

    # Deploy the application (allow failure for mariadb scheduling issues)
    echo "# Deploying kube-soomkiller with skaffold..."
    (cd "$project_root" && skaffold run --kube-context "${KUBE_CONTEXT:-k3s}" --status-check=false)

    # Wait for daemonset
    kubectl --context "${KUBE_CONTEXT:-k3s}" rollout status daemonset/kube-soomkiller \
        -n kube-soomkiller --timeout=120s

    # Wait for at least one mariadb pod to be ready
    echo "# Waiting for mariadb pods..."
    kubectl --context "${KUBE_CONTEXT:-k3s}" -n kube-soomkiller wait --for=condition=ready \
        pod/mariadb-0 --timeout=120s

    # Prepare sysbench (idempotent)
    echo "# Preparing sysbench tables..."
    source "$(dirname "$BATS_TEST_FILENAME")/test_helper.bash"
    sysbench_prepare mariadb-0
}

teardown_suite() {
    : # nothing to cleanup
}
