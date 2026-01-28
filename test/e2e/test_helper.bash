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
    kubectl logs -n "$NAMESPACE" daemonset/kube-soomkiller --all-containers=true --since="$since"
}

# Run sysbench prepare (idempotent)
sysbench_prepare() {
    kubectl exec -n "$NAMESPACE" mariadb-0 -- \
        mariadb -uroot -ptestpass -e "CREATE DATABASE IF NOT EXISTS sbtest;" 2>/dev/null || true

    kubectl exec -n "$NAMESPACE" deploy/sysbench -- \
        sysbench /usr/share/sysbench/oltp_read_write.lua \
        --mysql-host=mariadb --mysql-port=3306 \
        --mysql-user=root --mysql-password=testpass \
        --mysql-db=sbtest --tables=10 --table-size=100000 prepare 2>/dev/null || true
}

# Run sysbench stress test
# Usage: sysbench_run [threads] [time]
sysbench_run() {
    local threads="${1:-150}"
    local time="${2:-60}"

    kubectl exec -n "$NAMESPACE" deploy/sysbench -- \
        sysbench /usr/share/sysbench/oltp_read_write.lua \
        --mysql-host=mariadb --mysql-port=3306 \
        --mysql-user=root --mysql-password=testpass \
        --mysql-db=sbtest --tables=10 --table-size=100000 \
        --threads="$threads" --time="$time" --report-interval=10 run
}

# Set dry-run mode via env var
# Usage: set_dry_run true|false
set_dry_run() {
    local enabled="${1:-true}"
    kubectl set env daemonset/kube-soomkiller -n "$NAMESPACE" DRY_RUN="$enabled"
    wait_for_daemonset kube-soomkiller 120
}
