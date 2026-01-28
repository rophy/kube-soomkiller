#!/bin/bash
# Common test helpers for kube-soomkiller e2e tests

# Kubernetes context to use
KUBE_CONTEXT="${KUBE_CONTEXT:-k3s}"
NAMESPACE="kube-soomkiller"

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
    stern -n "$NAMESPACE" -l app=kube-soomkiller -s "$since" --no-follow
    # kubectl logs -n "$NAMESPACE" daemonset/kube-soomkiller --all-containers=true --since="$since"
}

# Run sysbench prepare (idempotent)
# Usage: sysbench_prepare [pod] - defaults to mariadb-0 mariadb-1
sysbench_prepare() {
    local pods="${*:-mariadb-0 mariadb-1}"
    for pod in $pods; do
        kubectl exec -n "$NAMESPACE" "$pod" -- \
            mariadb -uroot -ptestpass -e "CREATE DATABASE IF NOT EXISTS sbtest;" 2>/dev/null || true

        kubectl exec -n "$NAMESPACE" deploy/sysbench -- \
            sysbench /usr/share/sysbench/oltp_read_write.lua \
            --mysql-host="$pod.mariadb" --mysql-port=3306 \
            --mysql-user=root --mysql-password=testpass \
            --mysql-db=sbtest --tables=10 --table-size=100000 prepare 2>/dev/null || true
    done
}

# Run sysbench stress test with timeout
# Usage: sysbench_run [threads] [duration] [host]
sysbench_run() {
    local threads="${1:-150}"
    local duration="${2:-60}"
    local host="${3:-mariadb-0.mariadb}"

    timeout "$duration" kubectl exec -n "$NAMESPACE" deploy/sysbench -- \
        sysbench /usr/share/sysbench/oltp_read_write.lua \
        --mysql-host="$host" --mysql-port=3306 \
        --mysql-user=root --mysql-password=testpass \
        --mysql-db=sbtest --tables=10 --table-size=100000 \
        --threads="$threads" --time="$duration" --report-interval=10 run || true
}

# Set dry-run mode via env var
# Usage: set_dry_run true|false
set_dry_run() {
    local enabled="${1:-true}"
    kubectl set env daemonset/kube-soomkiller -n "$NAMESPACE" DRY_RUN="$enabled"
    wait_for_daemonset kube-soomkiller 120
}
