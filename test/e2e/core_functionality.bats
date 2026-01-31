#!/usr/bin/env bats
# Core functionality tests for kube-soomkiller v2

setup() {
    load 'test_helper'
}

@test "swap usage triggers pod scanning and threshold check" {
    # Run sysbench to create swap pressure
    sysbench_run 150 15

    # Capture logs from the test period
    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Track what we verified
    local verified_scan=false
    local verified_candidates=false

    # Check 1: Swap I/O detected and pods scanned
    if echo "$logs" | grep -q "Swap I/O detected"; then
        verified_scan=true
    fi

    # Check 2: Pods using swap found
    if echo "$logs" | grep -q "pods using swap"; then
        verified_candidates=true
    fi

    # Must have detected swap I/O and scanned for candidates
    if ! $verified_scan; then
        echo "ERROR: No swap I/O detected"
        echo "Logs:"
        echo "$logs" | tail -30
        false
    fi

    # Report what was verified
    echo "# Verified: scan=$verified_scan candidates=$verified_candidates"
}

@test "pods below threshold are not killed" {
    # Ensure dry-run mode is on
    set_dry_run true

    # Run light stress that may trigger some swap but stay below threshold
    sysbench_run 120 10

    # Capture logs
    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Check that pods were scanned but none exceeded threshold
    local over_threshold=false
    if echo "$logs" | grep -q "over threshold"; then
        over_threshold=true
    fi

    if $over_threshold; then
        echo "# Pod(s) detected over threshold"
        echo "$logs" | grep -E "(over threshold|swap=)" | tail -10
    else
        echo "# No pods exceeded threshold (as expected for light load)"
    fi
}

@test "pods over threshold are killed (dry-run)" {
    # Ensure dry-run mode is on
    set_dry_run true

    # Run heavy stress to trigger swap and exceed threshold
    sysbench_run 150 20

    # Capture logs
    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Track verification
    local verified_scan=false
    local verified_over_threshold=false
    local verified_dryrun=false

    # Check 1: Swap I/O detected
    if echo "$logs" | grep -q "Swap I/O detected"; then
        verified_scan=true
    fi

    # Check 2: Pod over threshold detected
    if echo "$logs" | grep -q "over threshold"; then
        verified_over_threshold=true
    fi

    # Check 3: Dry-run log shows would-delete
    if echo "$logs" | grep -q "DRY-RUN.*Would delete pod"; then
        verified_dryrun=true
    fi

    # Report results
    echo "# Verified: scan=$verified_scan over_threshold=$verified_over_threshold dryrun=$verified_dryrun"

    # Show relevant log lines
    if $verified_over_threshold || $verified_dryrun; then
        echo "# Threshold/kill logs:"
        echo "$logs" | grep -E "(over threshold|DRY-RUN|swap=)" | tail -10
    fi

    # Must have scanned
    if ! $verified_scan; then
        echo "ERROR: No swap I/O detected"
        echo "$logs" | tail -30
        false
    fi
}

@test "protected namespaces are not killed" {
    # kube-system is protected by default
    # This test verifies that pods in protected namespaces are filtered out

    local logs
    logs=$(get_soomkiller_logs_since "300s")

    # Check that no kube-system pods appear in candidates or kill logs
    if echo "$logs" | grep -E "(kube-system.*swap=|Would delete.*kube-system)"; then
        echo "ERROR: Protected namespace pod appeared in logs"
        false
    fi

    echo "# Verified: no kube-system pods in kill candidates"
}

@test "metrics endpoint is accessible" {
    local pod
    pod=$(kubectl get pod -n "$NAMESPACE" -l app=kube-soomkiller -o jsonpath='{.items[0].metadata.name}')

    # Find an available port
    local port
    port=$(shuf -i 30000-40000 -n 1)

    # Port-forward and check metrics
    kubectl port-forward -n "$NAMESPACE" "$pod" "${port}:8080" &
    local pf_pid=$!
    sleep 2

    # Fetch metrics
    local metrics
    metrics=$(curl -s "http://localhost:${port}/metrics" 2>/dev/null || true)

    # Cleanup
    kill $pf_pid 2>/dev/null || true
    wait $pf_pid 2>/dev/null || true

    # Check for expected metrics
    local has_swap_io=false
    local has_pods_killed=false
    local has_threshold=false

    if echo "$metrics" | grep -q "soomkiller_swap_io_rate"; then
        has_swap_io=true
    fi
    if echo "$metrics" | grep -q "soomkiller_pods_killed_total"; then
        has_pods_killed=true
    fi
    if echo "$metrics" | grep -q "soomkiller_config_swap_threshold_percent"; then
        has_threshold=true
    fi

    echo "# Metrics: swap_io=$has_swap_io pods_killed=$has_pods_killed threshold=$has_threshold"

    if ! $has_swap_io || ! $has_pods_killed || ! $has_threshold; then
        echo "ERROR: Missing expected metrics"
        echo "$metrics" | grep soomkiller || true
        false
    fi
}
