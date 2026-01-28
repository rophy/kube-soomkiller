#!/usr/bin/env bats
# Dry-run mode tests for kube-soomkiller

setup() {
    load 'test_helper'
    cleanup_stress_pods
}

teardown() {
    cleanup_stress_pods
    # Restore normal mode
    skaffold run --kube-context "$KUBE_CONTEXT" >/dev/null 2>&1 || true
}

@test "dry-run mode logs but does not kill pods" {
    # Patch daemonset to enable dry-run with low threshold
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--node-name=$(NODE_NAME)", "--swap-io-threshold=100", "--dry-run=true"]}]'
    wait_for_daemonset kube-soomkiller 120

    # Run sysbench to create pressure
    sysbench_run 150 60 &
    local sysbench_pid=$!

    # Wait for potential kill
    sleep 45

    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Cleanup
    kill $sysbench_pid 2>/dev/null || true
    wait $sysbench_pid 2>/dev/null || true

    # Restore original settings (no dry-run, normal threshold)
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--node-name=$(NODE_NAME)", "--swap-io-threshold=1000"]}]'

    # In dry-run mode with a victim selected:
    # - Should see "DRY-RUN" in logs
    # - Should NOT see "Successfully deleted pod"
    if echo "$logs" | grep -q "Selected victim:"; then
        # Should NOT see actual deletion
        if echo "$logs" | grep -q "Successfully deleted pod"; then
            echo "ERROR: Pod was deleted in dry-run mode!"
            false
        fi
        # Should see dry-run indication
        echo "$logs" | grep -q -i "dry.run\|DRY-RUN"
    fi
}
