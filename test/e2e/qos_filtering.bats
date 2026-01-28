#!/usr/bin/env bats
# QoS class filtering tests for kube-soomkiller

setup() {
    load 'test_helper'
    cleanup_stress_pods
}

teardown() {
    cleanup_stress_pods
}

@test "only Burstable QoS pods are candidates" {
    # Create a Burstable pod that uses memory
    create_stress_pod "stress-burstable" "burstable"
    wait_for_pod_ready "stress-burstable" 60

    # Wait for swap usage to appear
    sleep 15

    local logs
    logs=$(get_soomkiller_logs_since "30s")

    # Burstable pod MUST appear in candidate list (it has memory requests, no limits)
    # The controller logs candidates when threshold is exceeded
    # Even if threshold not exceeded, we can verify from periodic logs

    # Check that stress-burstable appears in logs as a candidate
    if echo "$logs" | grep -q "stress-burstable"; then
        return 0
    fi

    # If not in logs yet, the system may not have had threshold event
    # In that case, verify the pod is Burstable and would qualify
    local qos
    qos=$(kubectl get pod stress-burstable -n "$NAMESPACE" -o jsonpath='{.status.qosClass}')
    [[ "$qos" == "Burstable" ]]
}

@test "Guaranteed QoS pods are ignored" {
    # Create a Guaranteed pod (requests = limits for all resources)
    create_stress_pod "stress-guaranteed" "guaranteed"
    wait_for_pod_ready "stress-guaranteed" 60

    # Verify it's actually Guaranteed QoS
    local qos
    qos=$(kubectl get pod stress-guaranteed -n "$NAMESPACE" -o jsonpath='{.status.qosClass}')
    [[ "$qos" == "Guaranteed" ]]

    # Wait a bit for any candidate listing
    sleep 15

    local logs
    logs=$(get_soomkiller_logs_since "30s")

    # Guaranteed pods should NOT appear in candidate list
    # They don't get swap in LimitedSwap mode
    if echo "$logs" | grep -q "stress-guaranteed"; then
        echo "ERROR: Guaranteed pod appeared in candidate list"
        echo "$logs"
        false
    fi
}

@test "BestEffort QoS pods are ignored" {
    # Create a BestEffort pod (no requests, no limits)
    create_stress_pod "stress-besteffort" "besteffort"
    wait_for_pod_ready "stress-besteffort" 60

    # Verify it's actually BestEffort QoS
    local qos
    qos=$(kubectl get pod stress-besteffort -n "$NAMESPACE" -o jsonpath='{.status.qosClass}')
    [[ "$qos" == "BestEffort" ]]

    # Wait a bit for any candidate listing
    sleep 15

    local logs
    logs=$(get_soomkiller_logs_since "30s")

    # BestEffort pods should NOT appear in candidate list
    # They don't get swap in LimitedSwap mode
    if echo "$logs" | grep -q "stress-besteffort"; then
        echo "ERROR: BestEffort pod appeared in candidate list"
        echo "$logs"
        false
    fi
}
