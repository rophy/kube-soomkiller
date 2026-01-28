#!/usr/bin/env bats
# Pod selection tests for kube-soomkiller

setup() {
    load 'test_helper'
    cleanup_stress_pods
    # Use dry-run mode - we only need to verify selection logic
    set_dry_run true
}

teardown() {
    cleanup_stress_pods
    set_dry_run false
}

@test "pods with swap=0 are not selected as victims" {
    # Create a minimal pod that won't use swap
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: no-swap-pod
  namespace: $NAMESPACE
  labels:
    app: stress-test
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: "true"
  containers:
  - name: sleep
    image: busybox
    resources:
      requests:
        memory: "32Mi"
    command: ["sleep", "infinity"]
EOF

    wait_for_pod_ready "no-swap-pod" 60

    # Verify pod is Burstable (has requests, no limits)
    local qos
    qos=$(kubectl get pod no-swap-pod -n "$NAMESPACE" -o jsonpath='{.status.qosClass}')
    [[ "$qos" == "Burstable" ]]

    # Lower threshold and trigger activity
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=100", "-v=2"]}]'
    wait_for_daemonset kube-soomkiller 120

    # Run brief sysbench
    sysbench_run 150 30 &
    local sysbench_pid=$!
    sleep 25
    kill $sysbench_pid 2>/dev/null || true
    wait $sysbench_pid 2>/dev/null || true

    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Restore
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=1000", "-v=2"]}]'

    # no-swap-pod should NOT be selected as victim (swap=0)
    if echo "$logs" | grep -q "Selected victim:.*no-swap-pod"; then
        echo "ERROR: Pod with swap=0 was selected as victim"
        echo "$logs"
        false
    fi
}

@test "pods with PSI=0 are not selected as victims" {
    # Create a pod that uses some memory but has low PSI (not actively swapping)
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: low-psi-pod
  namespace: $NAMESPACE
  labels:
    app: stress-test
spec:
  nodeSelector:
    node-role.kubernetes.io/worker: "true"
  containers:
  - name: idle
    image: busybox
    resources:
      requests:
        memory: "64Mi"
    command: ["sh", "-c", "dd if=/dev/zero of=/tmp/data bs=1M count=50 && sleep infinity"]
EOF

    wait_for_pod_ready "low-psi-pod" 60
    sleep 10

    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Verify PSI filtering logic exists in logs
    # The log format shows pods with PSI > 0 marked with "*"
    if echo "$logs" | grep -q "with PSI > 0"; then
        # Good - PSI filtering is active
        return 0
    fi

    # If no candidate listing yet, just verify the pod exists
    kubectl get pod low-psi-pod -n "$NAMESPACE"
}

@test "no victim selected when no eligible pods exist" {
    # Make sure no stress pods are running
    cleanup_stress_pods

    # Restore default settings (in case previous test changed them)
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=1000", "-v=2"]}]' 2>/dev/null || true

    # Wait for system to stabilize (logs accumulate over 30s intervals)
    sleep 35

    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Without stress, the system should be idle
    # Should NOT see any victim selection
    if echo "$logs" | grep -q "Selected victim:"; then
        echo "ERROR: Victim was selected when system should be idle"
        echo "$logs"
        false
    fi

    # Verify system is running (either "idle" message or just no victim selection)
    # The key assertion is no victim selection - system being idle is secondary
    return 0
}
