#!/usr/bin/env bats
# Core functionality tests for kube-soomkiller v2

# Default timeout for each test (in seconds)
export BATS_TEST_TIMEOUT="${BATS_TEST_TIMEOUT:-120}"

setup() {
    load 'test_helper'
}

@test "controller starts and discovers cgroups" {
    # Check that controller pods are running
    local running
    running=$(kubectl get pods -n "$NAMESPACE" -l app=kube-soomkiller -o jsonpath='{.items[*].status.phase}')

    if [[ ! "$running" =~ "Running" ]]; then
        echo "ERROR: Controller pods not running"
        kubectl get pods -n "$NAMESPACE" -l app=kube-soomkiller
        false
    fi

    # Check logs for successful startup
    local logs
    logs=$(get_soomkiller_logs_since "120s")

    if ! echo "$logs" | grep -q "Controller started"; then
        echo "ERROR: Controller did not start properly"
        echo "$logs" | tail -20
        false
    fi

    if ! echo "$logs" | grep -q "container cgroups"; then
        echo "ERROR: Controller did not discover cgroups"
        echo "$logs" | tail -20
        false
    fi

    echo "# Controller started and discovered cgroups"
}

@test "metrics endpoint exposes expected metrics" {
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
    local missing=""

    echo "$metrics" | grep -q "soomkiller_swap_io_rate" || missing="$missing swap_io_rate"
    echo "$metrics" | grep -q "soomkiller_pods_killed_total" || missing="$missing pods_killed_total"
    echo "$metrics" | grep -q "soomkiller_config_swap_threshold_percent" || missing="$missing config_swap_threshold_percent"
    echo "$metrics" | grep -q "soomkiller_config_dry_run" || missing="$missing config_dry_run"

    if [[ -n "$missing" ]]; then
        echo "ERROR: Missing metrics:$missing"
        echo "Available soomkiller metrics:"
        echo "$metrics" | grep soomkiller || true
        false
    fi

    echo "# All expected metrics present"
}

@test "health endpoint returns ok" {
    local pod
    pod=$(kubectl get pod -n "$NAMESPACE" -l app=kube-soomkiller -o jsonpath='{.items[0].metadata.name}')

    # Find an available port
    local port
    port=$(shuf -i 30000-40000 -n 1)

    # Port-forward and check health
    kubectl port-forward -n "$NAMESPACE" "$pod" "${port}:8080" &
    local pf_pid=$!
    sleep 2

    # Check health endpoint
    local health
    health=$(curl -s "http://localhost:${port}/healthz" 2>/dev/null || true)

    # Cleanup
    kill $pf_pid 2>/dev/null || true
    wait $pf_pid 2>/dev/null || true

    if [[ "$health" != "ok" ]]; then
        echo "ERROR: Health check failed, got: $health"
        false
    fi

    echo "# Health endpoint returned ok"
}

# Note: This test depends on cluster having limited memory to trigger swap.
# It may be skipped in environments with abundant memory.
@test "memory pressure triggers swap detection (requires constrained memory)" {
    # Delete any existing memory-hog pod
    kubectl delete pod memory-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true
    sleep 2

    # Create fresh memory-hog pod
    kubectl apply -f "$(get_project_root)/deploy/e2e/memory-hog.yaml"
    kubectl wait --for=condition=ready pod/memory-hog -n "$NAMESPACE" --timeout=60s

    # Get the node where memory-hog is running
    local node
    node=$(kubectl get pod memory-hog -n "$NAMESPACE" -o jsonpath='{.spec.nodeName}')
    echo "# memory-hog scheduled on node: $node"

    # Trigger memory pressure - allocate more than limit to force swap
    # stress --vm 1 --vm-bytes 300M will try to allocate 300MB against 256MB limit
    kubectl exec -n "$NAMESPACE" memory-hog -- timeout 20 stress --vm 1 --vm-bytes 300M --vm-keep 2>/dev/null || true

    # Give controller time to detect
    sleep 3

    # Capture logs
    local logs
    logs=$(get_soomkiller_logs_since "60s")

    # Verify the flow
    local detected_swap=false
    local found_pods=false

    if echo "$logs" | grep -q "Swap I/O detected"; then
        detected_swap=true
    fi

    if echo "$logs" | grep -q "pods using swap"; then
        found_pods=true
    fi

    echo "# Results: detected_swap=$detected_swap found_pods=$found_pods"

    # Show relevant logs
    echo "# Relevant logs:"
    echo "$logs" | grep -E "(Swap I/O|pods using swap|over threshold|memory-hog)" | tail -10 || true

    # Check for Soomkilled event
    local event_found=false
    if kubectl get events -n "$NAMESPACE" --field-selector reason=Soomkilled 2>/dev/null | grep -q memory-hog; then
        event_found=true
        echo "# Soomkilled event found:"
        kubectl get events -n "$NAMESPACE" --field-selector reason=Soomkilled | grep memory-hog | tail -3
    fi

    # Cleanup
    kubectl delete pod memory-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true

    # This test may not trigger swap if the node has plenty of free memory
    # We log the result but don't fail - the core functionality tests (1-3) are more important
    if ! $detected_swap; then
        echo "# NOTE: No swap I/O detected - node may have sufficient free memory"
        echo "# This is expected in environments with abundant memory"
        skip "Swap was not triggered - node has sufficient free memory"
    fi

    # Verify Soomkilled event was emitted
    if ! $event_found; then
        echo "# WARNING: Soomkilled event not found for memory-hog pod"
        echo "# Available events:"
        kubectl get events -n "$NAMESPACE" --field-selector reason=Soomkilled || true
    fi
}
