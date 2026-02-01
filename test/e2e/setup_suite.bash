#!/bin/bash
# Suite-level setup - runs once before all tests

setup_suite() {
    # Get project root (two levels up from test/e2e/)
    local project_root
    project_root="$(cd "$(dirname "$BATS_TEST_FILENAME")/../.." && pwd)"

    # Deploy soomkiller + e2e fixtures using skaffold e2e profile
    echo "# Deploying kube-soomkiller with skaffold (e2e profile)..."
    (cd "$project_root" && skaffold run --kube-context "${KUBE_CONTEXT:-k3s}" --profile e2e)

    # Wait for daemonset
    echo "# Waiting for daemonset rollout..."
    kubectl --context "${KUBE_CONTEXT:-k3s}" rollout status daemonset/kube-soomkiller \
        -n kube-soomkiller --timeout=120s

    echo "# Setup complete"
}

teardown_suite() {
    # Cleanup e2e test pods
    kubectl --context "${KUBE_CONTEXT:-k3s}" delete pod memory-hog \
        -n kube-soomkiller --ignore-not-found=true 2>/dev/null || true
}
