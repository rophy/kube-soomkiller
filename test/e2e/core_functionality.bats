#!/usr/bin/env bats
# Core functionality tests for kube-soomkiller

setup() {
    load 'test_helper'
}

@test "swap pressure triggers detection, victim selection, and cooldown" {
    # Run sysbench to create swap pressure (12s to trigger soomkill)
    sysbench_run 150 12

    # Capture logs from the test period
    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Track what we verified
    local verified_threshold=false
    local verified_victim=false
    local verified_cooldown=false

    # Check 1: Threshold exceeded was detected
    if echo "$logs" | grep -q "Swap I/O threshold exceeded"; then
        verified_threshold=true
    fi

    # Check 2: Victim selection - if selected, must be mariadb (highest PSI)
    if echo "$logs" | grep -q "Selected victim:"; then
        verified_victim=true
        if ! echo "$logs" | grep "Selected victim:" | grep -q "mariadb"; then
            echo "ERROR: Wrong victim selected (expected mariadb)"
            echo "$logs" | grep "Selected victim:"
            false
        fi
    fi

    # Check 3: Cooldown - run stress against mariadb-1 to trigger cooldown message
    sysbench_run 150 20 mariadb-1.mariadb

    # Capture logs again to check for cooldown
    logs=$(get_soomkiller_logs_since "60s")
    if echo "$logs" | grep -q "in cooldown"; then
        verified_cooldown=true
    else
        echo "ERROR: No cooldown message after second stress test"
        echo "$logs" | grep -E "(cooldown|threshold|victim)" || true
        false
    fi

    # Must have at least detected threshold or selected victim
    if ! $verified_threshold && ! $verified_victim; then
        echo "ERROR: No swap pressure activity detected"
        echo "Logs:"
        echo "$logs" | tail -30
        false
    fi

    # Report what was verified
    echo "# Verified: threshold=$verified_threshold victim=$verified_victim cooldown=$verified_cooldown"
}
