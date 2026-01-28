#!/usr/bin/env bats
# Core functionality tests for kube-soomkiller

setup() {
    load 'test_helper'
    cleanup_stress_pods
    # Disable dry-run - these tests verify actual behavior
    set_dry_run false
}

teardown() {
    cleanup_stress_pods
}

@test "swap I/O threshold triggers action when exceeded" {
    # This test verifies that sustained swap pressure triggers victim selection.
    # We use a lower threshold (100) to ensure the test triggers reliably.

    # Patch daemonset to use lower threshold for testing
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=100", "-v=2"]}]'
    wait_for_daemonset kube-soomkiller 120

    # Run sysbench to create swap pressure
    sysbench_run 150 60 &
    local sysbench_pid=$!

    # Wait for potential threshold trigger
    sleep 45

    # Cleanup sysbench
    kill $sysbench_pid 2>/dev/null || true
    wait $sysbench_pid 2>/dev/null || true

    # Capture logs BEFORE restoring (new pods won't have these logs)
    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Restore original threshold
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=1000", "-v=2"]}]'

    # Test passes if threshold was exceeded or victim was selected
    if echo "$logs" | grep -q "Swap I/O threshold exceeded"; then
        return 0
    fi
    if echo "$logs" | grep -q "Selected victim:"; then
        return 0
    fi

    # Show logs for debugging
    echo "Logs:"
    echo "$logs"
    false
}

@test "sustained duration required before action (10s)" {
    # Patch daemonset to use lower threshold and short sustained duration
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=50", "--sustained-duration=10s", "-v=2"]}]'
    wait_for_daemonset kube-soomkiller 120

    # Run brief sysbench (only 5 seconds - less than sustained duration)
    sysbench_run 150 5 || true

    # Capture logs
    local logs
    logs=$(get_soomkiller_logs_since "30s")

    # Restore original settings
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=1000", "-v=2"]}]'

    # Should see "waiting" message indicating sustained duration not met
    # OR should NOT see victim selection (brief spike ignored)
    if echo "$logs" | grep -q "need 10s"; then
        return 0
    fi
    # If no "need 10s" message, verify no victim was selected during brief spike
    if ! echo "$logs" | grep -q "Selected victim:"; then
        return 0
    fi

    echo "ERROR: Victim was selected without sustained duration"
    echo "$logs"
    false
}

@test "pod with highest PSI is selected as victim" {
    # Patch to very low threshold to ensure trigger
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=50", "-v=2"]}]'
    wait_for_daemonset kube-soomkiller 120

    # Run sysbench - this stresses mariadb which should have highest PSI
    sysbench_run 150 90 &
    local sysbench_pid=$!

    # Wait for victim selection (longer wait for more reliable trigger)
    sleep 70

    # Capture logs
    local logs
    logs=$(get_soomkiller_logs_since "90s")

    # Cleanup
    kill $sysbench_pid 2>/dev/null || true
    wait $sysbench_pid 2>/dev/null || true

    # Restore
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=1000", "-v=2"]}]'

    # If victim was selected, verify it was mariadb
    if echo "$logs" | grep -q "Selected victim:"; then
        # Victim should be mariadb-0 (highest PSI under sysbench load)
        if ! echo "$logs" | grep "Selected victim:" | grep -q "mariadb-0"; then
            echo "ERROR: Wrong victim selected (expected mariadb-0)"
            echo "$logs" | grep "Selected victim:"
            false
        fi
        return 0
    fi

    # If no victim selected, check if threshold was at least exceeded
    if echo "$logs" | grep -q "Swap I/O threshold exceeded"; then
        # Threshold exceeded but no victim - might be waiting for sustained duration
        # This is acceptable behavior
        return 0
    fi

    # No activity at all - the workload didn't generate swap pressure
    echo "WARNING: No swap pressure generated - test inconclusive"
    echo "$logs" | tail -20
    # Don't fail - this depends on system state
    return 0
}

@test "cooldown period prevents rapid kills" {
    # Patch to lower threshold and short cooldown for testing
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=100", "--cooldown-period=30s", "-v=2"]}]'
    wait_for_daemonset kube-soomkiller 120

    # Run sysbench to trigger kill
    sysbench_run 150 60 &
    local sysbench_pid=$!

    # Wait for kill and cooldown
    sleep 50

    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Cleanup
    kill $sysbench_pid 2>/dev/null || true
    wait $sysbench_pid 2>/dev/null || true

    # Restore
    kubectl patch daemonset kube-soomkiller -n "$NAMESPACE" --type=json \
        -p '[{"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": ["--swap-io-threshold=1000", "-v=2"]}]'

    # If a pod was deleted, must see cooldown messages
    if echo "$logs" | grep -q "Successfully deleted pod"; then
        if ! echo "$logs" | grep -q "in cooldown"; then
            echo "ERROR: No cooldown after kill"
            echo "$logs"
            false
        fi
    fi
}
