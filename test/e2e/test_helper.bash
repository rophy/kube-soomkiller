#!/bin/bash
# Common test helpers for kube-soomkiller e2e tests

# Kubernetes context to use
KUBE_CONTEXT="${KUBE_CONTEXT:-k3s}"
NAMESPACE="kube-soomkiller"

# kubectl wrapper with context
kubectl() {
    command kubectl --context "$KUBE_CONTEXT" "$@"
}

# Wait for a pod to be ready
wait_for_pod_ready() {
    local pod="$1"
    local timeout="${2:-60}"
    kubectl wait --for=condition=Ready "pod/$pod" -n "$NAMESPACE" --timeout="${timeout}s"
}

# Wait for daemonset rollout
wait_for_daemonset() {
    local ds="$1"
    local timeout="${2:-120}"
    kubectl rollout status "daemonset/$ds" -n "$NAMESPACE" --timeout="${timeout}s"
}

# Get soomkiller logs from all pods
get_soomkiller_logs() {
    kubectl logs -n "$NAMESPACE" daemonset/kube-soomkiller --all-containers=true
}

# Get soomkiller logs since a timestamp
get_soomkiller_logs_since() {
    local since="$1"
    kubectl logs -n "$NAMESPACE" daemonset/kube-soomkiller --all-containers=true --since="$since"
}

# Create a stress pod with given QoS class
# Usage: create_stress_pod <name> <qos> [node]
# qos: burstable, guaranteed, besteffort
create_stress_pod() {
    local name="$1"
    local qos="$2"
    local node="${3:-}"

    local node_selector=""
    if [[ -n "$node" ]]; then
        node_selector="nodeName: $node"
    else
        node_selector='nodeSelector:
    node-role.kubernetes.io/worker: "true"'
    fi

    case "$qos" in
        burstable)
            # requests but no limits = Burstable
            kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NAMESPACE
  labels:
    app: stress-test
spec:
  $node_selector
  containers:
  - name: stress
    image: polinux/stress
    resources:
      requests:
        memory: "256Mi"
    command: ["stress"]
    args: ["--vm", "1", "--vm-bytes", "800M", "--vm-hang", "0"]
EOF
            ;;
        guaranteed)
            # requests = limits = Guaranteed
            kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NAMESPACE
  labels:
    app: stress-test
spec:
  $node_selector
  containers:
  - name: stress
    image: polinux/stress
    resources:
      requests:
        memory: "512Mi"
        cpu: "100m"
      limits:
        memory: "512Mi"
        cpu: "100m"
    command: ["stress"]
    args: ["--vm", "1", "--vm-bytes", "400M", "--vm-hang", "0"]
EOF
            ;;
        besteffort)
            # no requests, no limits = BestEffort
            kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NAMESPACE
  labels:
    app: stress-test
spec:
  $node_selector
  containers:
  - name: stress
    image: polinux/stress
    command: ["stress"]
    args: ["--vm", "1", "--vm-bytes", "200M", "--vm-hang", "0"]
EOF
            ;;
    esac
}

# Delete all stress test pods
cleanup_stress_pods() {
    kubectl delete pods -n "$NAMESPACE" -l app=stress-test --force --grace-period=0 2>/dev/null || true
}

# Check if a pod was killed by soomkiller (appears in logs)
pod_was_killed() {
    local pod="$1"
    get_soomkiller_logs | grep -q "Deleting pod $NAMESPACE/$pod"
}

# Check if a pod was selected as victim (appears in logs)
pod_was_selected_victim() {
    local pod="$1"
    get_soomkiller_logs | grep -q "Selected victim: $NAMESPACE/$pod"
}

# Get the last selected victim from logs
get_last_victim() {
    get_soomkiller_logs | grep "Selected victim:" | tail -1 | sed 's/.*Selected victim: //' | cut -d' ' -f1
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

# Deploy with custom soomkiller args
# Usage: deploy_soomkiller [extra_args...]
deploy_soomkiller() {
    local extra_args="$*"

    # Use skaffold with default config
    skaffold run --kube-context "$KUBE_CONTEXT" 2>/dev/null

    # If extra args provided, patch the daemonset
    if [[ -n "$extra_args" ]]; then
        kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
            -p "[{\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/args\", \"value\": [\"--node-name=\$(NODE_NAME)\", $extra_args]}]"
        wait_for_daemonset kube-soomkiller
    fi
}

# Redeploy soomkiller with specific threshold
deploy_with_threshold() {
    local threshold="$1"
    kubectl set env daemonset/kube-soomkiller -n "$NAMESPACE" --containers=kube-soomkiller \
        SWAP_IO_THRESHOLD="$threshold" 2>/dev/null || \
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p "[{\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/args\", \"value\": [\"--node-name=\$(NODE_NAME)\", \"--swap-io-threshold=$threshold\"]}]"
    wait_for_daemonset kube-soomkiller
}

# Set dry-run mode via env var
# Usage: set_dry_run true|false
set_dry_run() {
    local enabled="${1:-true}"
    kubectl set env daemonset/kube-soomkiller -n "$NAMESPACE" DRY_RUN="$enabled"
    wait_for_daemonset kube-soomkiller 120
}

# Get worker node name
get_worker_node() {
    kubectl get nodes -l node-role.kubernetes.io/worker=true -o jsonpath='{.items[0].metadata.name}'
}
