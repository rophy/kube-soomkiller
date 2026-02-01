#!/usr/bin/env bats
# Core functionality tests for kube-soomkiller v2

setup_file() {
    load 'test_helper'

    # Default timeout for each test (in seconds)
    export BATS_TEST_TIMEOUT="${BATS_TEST_TIMEOUT:-120}"

    # Deploy soomkiller + e2e fixtures using skaffold e2e profile
    # skaffold run waits for rollout by default
    echo "# Deploying kube-soomkiller with skaffold (e2e profile)..."
    (cd "$(get_project_root)" && skaffold run --kube-context "${KUBE_CONTEXT:-k3s}" --profile e2e)

    echo "# Setup complete"
}

teardown_file() {
    load 'test_helper'

    # Cleanup e2e test jobs
    kubectl delete job memory-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true
}

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
    # Get soomkiller pod IP
    local pod_ip
    pod_ip=$(kubectl get pod -n "$NAMESPACE" -l app=kube-soomkiller -o jsonpath='{.items[0].status.podIP}')

    # Fetch metrics using the curl pod deployed by e2e profile
    local metrics
    metrics=$(kubectl exec -n "$NAMESPACE" deploy/curl -- curl -s "http://${pod_ip}:8080/metrics" 2>/dev/null || true)

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
    # Get soomkiller pod IP
    local pod_ip
    pod_ip=$(kubectl get pod -n "$NAMESPACE" -l app=kube-soomkiller -o jsonpath='{.items[0].status.podIP}')

    # Check health endpoint using the curl pod deployed by e2e profile
    local health
    health=$(kubectl exec -n "$NAMESPACE" deploy/curl -- curl -s "http://${pod_ip}:8080/healthz" 2>/dev/null || true)

    if [[ "$health" != "ok" ]]; then
        echo "ERROR: Health check failed, got: $health"
        false
    fi

    echo "# Health endpoint returned ok"
}

# Critical test: verifies soomkiller detects swap usage and emits Soomkilled event
@test "memory pressure triggers swap detection and Soomkilled event" {
    # Delete any existing memory-hog job and wait for cleanup
    kubectl delete job memory-hog -n "$NAMESPACE" --ignore-not-found=true --wait=true 2>/dev/null || true

    # Create memory-hog job (runs stress command automatically)
    kubectl apply -f "$(get_project_root)/deploy/e2e/memory-hog.yaml"

    # Get the pod name created by the job
    local pod_name=""
    local attempts=0
    while [[ -z "$pod_name" && $attempts -lt 10 ]]; do
        sleep 0.5
        attempts=$((attempts + 1))
        pod_name=$(kubectl get pods -n "$NAMESPACE" -l job-name=memory-hog -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    done
    echo "# Pod name: $pod_name"

    # Wait for soomkiller to detect and kill the pod (poll for up to 15 seconds)
    local detected_swap=false
    local event_found=false
    attempts=0
    local max_attempts=15
    local logs=""

    while [[ $attempts -lt $max_attempts ]]; do
        sleep 1
        attempts=$((attempts + 1))

        # Check logs for swap detection
        logs=$(get_soomkiller_logs_since "30s")
        if echo "$logs" | grep -q "Swap I/O detected"; then
            detected_swap=true
        fi

        # Check for Soomkilled event for this specific pod
        if [[ -n "$pod_name" ]] && kubectl get events -n "$NAMESPACE" --field-selector reason=Soomkilled 2>/dev/null | grep -q "$pod_name"; then
            event_found=true
            echo "# Soomkilled event detected after $attempts seconds"
            break
        fi
    done

    # Show job status (persists even after pod deletion)
    echo "# Job status:"
    kubectl get job memory-hog -n "$NAMESPACE" -o wide 2>/dev/null || true

    # Show results
    echo "# Results: detected_swap=$detected_swap event_found=$event_found"

    # Show relevant logs
    echo "# Relevant logs:"
    echo "$logs" | grep -E "(Swap I/O|pods using swap|over threshold|memory-hog)" | tail -10 || true

    # Show event if found
    if $event_found; then
        echo "# Soomkilled event:"
        kubectl get events -n "$NAMESPACE" --field-selector reason=Soomkilled | grep "$pod_name" | tail -3
    fi

    # Cleanup
    kubectl delete job memory-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true

    # Fail if swap was not detected
    if ! $detected_swap; then
        echo "ERROR: Swap I/O was not detected by soomkiller"
        echo "# Check vm.swappiness on worker node (must be > 0):"
        kubectl get nodes -o name | head -1 | xargs -I{} kubectl debug {} -it --image=busybox -- cat /proc/sys/vm/swappiness 2>/dev/null || true
        false
    fi

    # Fail if Soomkilled event was not emitted
    if ! $event_found; then
        echo "ERROR: Soomkilled event not found for pod $pod_name"
        echo "# Available Soomkilled events:"
        kubectl get events -n "$NAMESPACE" --field-selector reason=Soomkilled || true
        false
    fi
}
