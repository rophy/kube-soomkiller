#!/bin/bash
# Common test helpers for kube-soomkiller e2e tests

# Kubernetes context to use
KUBE_CONTEXT="${KUBE_CONTEXT:-k3s}"
NAMESPACE="kube-soomkiller"

# Get project root directory
get_project_root() {
    cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd
}

# kubectl wrapper with context
kubectl() {
    command kubectl --context "$KUBE_CONTEXT" "$@"
}

# Wait for daemonset rollout
wait_for_daemonset() {
    local ds="$1"
    local timeout="${2:-120}"
    kubectl rollout status "daemonset/$ds" -n "$NAMESPACE" --timeout="${timeout}s"
}

# Get soomkiller logs since a timestamp
get_soomkiller_logs_since() {
    local since="$1"
    stern -n "$NAMESPACE" -l app=kube-soomkiller -s "$since" --no-follow 2>/dev/null || \
        kubectl logs -n "$NAMESPACE" daemonset/kube-soomkiller --all-containers=true --since="$since" 2>/dev/null || true
}
