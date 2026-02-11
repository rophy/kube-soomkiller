#!/usr/bin/env bats
# Core functionality tests for kube-soomkiller v2

setup_file() {
    load 'test_helper'

    # Default timeout for each test (in seconds)
    export BATS_TEST_TIMEOUT="${BATS_TEST_TIMEOUT:-120}"

    # Deploy soomkiller using skaffold (default profile)
    # skaffold run waits for rollout by default
    echo "# Deploying kube-soomkiller with skaffold..."
    (cd "$(get_project_root)" && skaffold run --kube-context "${KUBE_CONTEXT:-k3s}")

    echo "# Setup complete"
}

teardown_file() {
    load 'test_helper'

    # Cleanup e2e test jobs
    kubectl delete job memory-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true
    kubectl delete job file-cache-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true
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

    echo "# Controller pods are running"
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

    echo "$metrics" | grep -q "soomkiller_node_swap_in_pages_total" || missing="$missing node_swap_in_pages_total"
    echo "$metrics" | grep -q "soomkiller_node_swap_out_pages_total" || missing="$missing node_swap_out_pages_total"
    echo "$metrics" | grep -q "soomkiller_pods_killed_total" || missing="$missing pods_killed_total"
    echo "$metrics" | grep -q "soomkiller_config_memory_threshold_percent" || missing="$missing config_memory_threshold_percent"
    echo "$metrics" | grep -q "soomkiller_config_swap_threshold_percent" || missing="$missing config_swap_threshold_percent"
    echo "$metrics" | grep -q "soomkiller_config_file_cache_threshold_percent" || missing="$missing config_file_cache_threshold_percent"
    echo "$metrics" | grep -q "soomkiller_config_dry_run" || missing="$missing config_dry_run"
    # Note: soomkiller_container_* metrics only appear when containers are using swap

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
    # Capture test start time for log filtering (RFC3339 format)
    local test_start
    test_start=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

    # Delete any existing memory-hog job and wait for cleanup
    kubectl delete job memory-hog -n "$NAMESPACE" --ignore-not-found=true --wait=true 2>/dev/null || true

    # Create memory-hog job (runs stress command automatically)
    kubectl apply -f "$(get_project_root)/deploy/e2e/memory-hog.yaml"

    # Get the pod name (must capture before pod is deleted)
    local pod_name=""
    local attempts=0
    while [[ -z "$pod_name" && $attempts -lt 10 ]]; do
        sleep 0.5
        attempts=$((attempts + 1))
        pod_name=$(kubectl get pods -n "$NAMESPACE" -l job-name=memory-hog -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    done
    echo "# Memory-hog pod: $pod_name"

    # Wait for Soomkilled event for this specific pod (poll for up to 15 seconds)
    local event_found=false
    attempts=0
    local max_attempts=15

    while [[ $attempts -lt $max_attempts ]]; do
        sleep 1
        attempts=$((attempts + 1))

        # Check for Soomkilled event for this specific pod
        if kubectl get events -n "$NAMESPACE" --field-selector reason=Soomkilled 2>/dev/null | grep -q "$pod_name"; then
            event_found=true
            echo "# Soomkilled event detected after $attempts seconds"
            break
        fi
    done

    # Parse node name from event message (format: "Pod <name> deleted by kube-soomkiller on node <node>: ...")
    local node=""
    local event_message=""
    if $event_found; then
        event_message=$(kubectl get events -n "$NAMESPACE" --field-selector reason=Soomkilled -o jsonpath='{.items[?(@.involvedObject.name=="'"$pod_name"'")].message}' 2>/dev/null || true)
        node=$(echo "$event_message" | sed -n 's/.*on node \([^:]*\):.*/\1/p' || true)
        echo "# Node (from event): $node"
    fi

    # Get soomkiller logs from the specific node
    local logs=""
    if [[ -n "$node" ]]; then
        local soomkiller_pod
        soomkiller_pod=$(kubectl get pods -n "$NAMESPACE" -l app=kube-soomkiller \
            --field-selector spec.nodeName="$node" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        if [[ -n "$soomkiller_pod" ]]; then
            echo "# Soomkiller pod on node $node: $soomkiller_pod"
            logs=$(kubectl logs -n "$NAMESPACE" "$soomkiller_pod" --since-time="$test_start" 2>/dev/null || true)
        fi
    fi

    # Check logs for swap detection (found pods over threshold or deleted)
    local detected_swap=false
    if echo "$logs" | grep -qE "(pods over threshold|Deleted pod)"; then
        detected_swap=true
    fi

    # Show job status (persists even after pod deletion)
    echo "# Job status:"
    kubectl get job memory-hog -n "$NAMESPACE" -o wide 2>/dev/null || true

    # Show results
    echo "# Results: detected_swap=$detected_swap event_found=$event_found"

    # Show relevant logs
    if [[ -n "$logs" ]]; then
        echo "# Relevant logs:"
        echo "$logs" | grep -E "(over threshold|memory-hog|Deleted pod)" | tail -10 || true
    fi

    # Show event if found
    if $event_found; then
        echo "# Soomkilled event:"
        kubectl get events -n "$NAMESPACE" --field-selector reason=Soomkilled | grep "$pod_name" | tail -3
    fi

    # Cleanup
    kubectl delete job memory-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true

    # Fail if swap was not detected
    if ! $detected_swap; then
        echo "ERROR: Pod swap detection failed (no pods over threshold or deleted)"
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

# Validates the file cache condition prevents false kills
# Pod has swap usage BUT file cache > 1% of memory.max → should NOT be killed
# bats test_tags=slow
@test "pod with file cache and swap is not soomkilled" {
    # This test needs more time: pod startup + condition verification + survival wait
    export BATS_TEST_TIMEOUT=180
    # Delete any existing file-cache-hog job
    kubectl delete job file-cache-hog -n "$NAMESPACE" --ignore-not-found=true --wait=true 2>/dev/null || true

    # Ensure soomkiller daemonset is fully rolled out and stable.
    # Skaffold doesn't wait for DaemonSet rollouts, and during rolling updates
    # the OLD soomkiller pod may still be running in its grace period.
    # We must wait for it to fully terminate before deploying test workloads.
    kubectl rollout status ds/kube-soomkiller -n "$NAMESPACE" --timeout=60s
    # Wait for all soomkiller pods to be Running and Ready (no Terminating pods)
    attempts=0
    while [[ $attempts -lt 30 ]]; do
        local total_pods ready_pods
        total_pods=$(kubectl get pods -n "$NAMESPACE" -l app=kube-soomkiller --no-headers 2>/dev/null | wc -l)
        ready_pods=$(kubectl get pods -n "$NAMESPACE" -l app=kube-soomkiller --no-headers 2>/dev/null | grep -c "Running" || true)
        if [[ "$total_pods" -gt 0 && "$total_pods" -eq "$ready_pods" ]]; then
            break
        fi
        sleep 2
        attempts=$((attempts + 1))
    done
    # Extra settling time for old pods in termination grace period
    sleep 5

    # Deploy file-cache-hog job (creates file cache + anon memory pressure)
    kubectl apply -f "$(get_project_root)/deploy/e2e/file-cache-hog.yaml"

    # Wait for pod to start running
    local pod_name=""
    local attempts=0
    while [[ -z "$pod_name" && $attempts -lt 20 ]]; do
        sleep 1
        attempts=$((attempts + 1))
        pod_name=$(kubectl get pods -n "$NAMESPACE" -l job-name=file-cache-hog \
            --field-selector status.phase=Running \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    done

    if [[ -z "$pod_name" ]]; then
        echo "ERROR: file-cache-hog pod never reached Running state"
        kubectl get pods -n "$NAMESPACE" -l job-name=file-cache-hog 2>/dev/null || true
        kubectl delete job file-cache-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true
        false
    fi
    echo "# Pod running: $pod_name"

    # Find which node the pod is on and get soomkiller pod IP on that node
    local node
    node=$(kubectl get pod "$pod_name" -n "$NAMESPACE" -o jsonpath='{.spec.nodeName}')
    echo "# Pod scheduled on node: $node"

    # Wait for soomkiller pod on the same node to be ready (may restart after previous test)
    local soomkiller_ip=""
    attempts=0
    while [[ -z "$soomkiller_ip" && $attempts -lt 30 ]]; do
        soomkiller_ip=$(kubectl get pod -n "$NAMESPACE" -l app=kube-soomkiller \
            --field-selector spec.nodeName="$node" \
            -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)
        if [[ -z "$soomkiller_ip" ]]; then
            sleep 2
            attempts=$((attempts + 1))
        fi
    done

    if [[ -z "$soomkiller_ip" ]]; then
        echo "ERROR: Could not find soomkiller pod on node $node after 60s"
        kubectl get pods -n "$NAMESPACE" -l app=kube-soomkiller -o wide 2>/dev/null || true
        kubectl delete job file-cache-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true
        false
    fi

    # Poll metrics until we see both swap > 0 and file cache > 0
    local swap_detected=false
    local file_cache_detected=false
    local swap_bytes=""
    local cache_bytes=""
    attempts=0

    while [[ $attempts -lt 30 ]]; do
        sleep 2
        attempts=$((attempts + 1))

        # Check pod is still running (not already killed)
        local phase
        phase=$(kubectl get pod "$pod_name" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        if [[ "$phase" != "Running" ]]; then
            echo "ERROR: Pod stopped running (phase=$phase) before conditions were verified"
            echo "# All events for pod:"
            kubectl get events -n "$NAMESPACE" --field-selector involvedObject.name="$pod_name" 2>/dev/null || true
            echo "# Pod describe:"
            kubectl describe pod "$pod_name" -n "$NAMESPACE" 2>/dev/null | tail -20 || true
            echo "# Soomkiller logs:"
            local sk_pod_diag
            sk_pod_diag=$(kubectl get pod -n "$NAMESPACE" -l app=kube-soomkiller \
                --field-selector spec.nodeName="$node" \
                -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
            if [[ -n "$sk_pod_diag" ]]; then
                kubectl logs -n "$NAMESPACE" "$sk_pod_diag" --tail=20 2>/dev/null | grep -E "(threshold|Deleted|kill)" || echo "(no kill logs)"
            fi
            kubectl delete job file-cache-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true
            false
        fi

        local metrics
        metrics=$(kubectl exec -n "$NAMESPACE" deploy/curl -- \
            curl -s --max-time 5 "http://${soomkiller_ip}:8080/metrics" 2>/dev/null || true)

        # Extract swap and file cache bytes for our pod
        swap_bytes=$(echo "$metrics" | grep "soomkiller_container_swap_bytes" | \
            grep "$pod_name" | awk '{print $2}' | head -1)

        cache_bytes=$(echo "$metrics" | grep "soomkiller_container_file_cache_bytes" | \
            grep "$pod_name" | awk '{print $2}' | head -1)

        if [[ -n "$swap_bytes" ]] && awk "BEGIN{exit !($swap_bytes > 0)}"; then
            swap_detected=true
        fi

        if [[ -n "$cache_bytes" ]] && awk "BEGIN{exit !($cache_bytes > 0)}"; then
            file_cache_detected=true
        fi

        if $swap_detected && $file_cache_detected; then
            echo "# Conditions verified after $((attempts * 2))s: swap=${swap_bytes} file_cache=${cache_bytes}"
            break
        fi
    done

    if ! $swap_detected || ! $file_cache_detected; then
        echo "# SKIP: Could not achieve both swap and file cache simultaneously"
        echo "# swap_detected=$swap_detected (bytes=$swap_bytes)"
        echo "# file_cache_detected=$file_cache_detected (bytes=$cache_bytes)"
        echo "# This may happen with vm.swappiness=0 or insufficient memory pressure"
        kubectl delete job file-cache-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true
        skip "Could not achieve test conditions (swap + file cache)"
    fi

    # Both conditions confirmed - poll for 15s to verify pod survives
    echo "# Monitoring pod for 15s to verify it is not soomkilled..."
    local survived=true
    local final_phase="Running"
    local check=0
    while [[ $check -lt 8 ]]; do
        sleep 2
        check=$((check + 1))
        final_phase=$(kubectl get pod "$pod_name" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        if [[ "$final_phase" != "Running" ]]; then
            echo "# Pod stopped at check $check (${check}x2s): phase=$final_phase"
            echo "# Events for pod:"
            kubectl get events -n "$NAMESPACE" --field-selector involvedObject.name="$pod_name" 2>/dev/null || true
            survived=false
            break
        fi
    done
    echo "# Pod phase after monitoring: $final_phase"

    # Check for Soomkilled event
    local soomkill_event
    soomkill_event=$(kubectl get events -n "$NAMESPACE" --field-selector reason=Soomkilled 2>/dev/null | grep "$pod_name" || true)

    # Diagnostic: if pod is gone, check why
    if [[ -z "$final_phase" || "$final_phase" == "Failed" ]]; then
        echo "# Diagnostic: pod status"
        kubectl get pod "$pod_name" -n "$NAMESPACE" -o jsonpath='{.status.containerStatuses[0].state}' 2>/dev/null || true
        kubectl get pod "$pod_name" -n "$NAMESPACE" -o jsonpath='{.status.containerStatuses[0].lastState}' 2>/dev/null || true
        echo ""
        echo "# Diagnostic: job status"
        kubectl get job file-cache-hog -n "$NAMESPACE" -o wide 2>/dev/null || true
        echo "# Diagnostic: soomkiller logs (last 30 lines)"
        local sk_pod
        sk_pod=$(kubectl get pod -n "$NAMESPACE" -l app=kube-soomkiller \
            --field-selector spec.nodeName="$node" \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        if [[ -n "$sk_pod" ]]; then
            kubectl logs -n "$NAMESPACE" "$sk_pod" --tail=30 2>/dev/null || true
        fi
    fi

    # Cleanup
    kubectl delete job file-cache-hog -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true

    if [[ -n "$soomkill_event" ]]; then
        echo "ERROR: Pod was falsely soomkilled despite file cache!"
        echo "$soomkill_event"
        false
    fi

    if ! $survived; then
        echo "ERROR: Pod did not survive the monitoring period (phase=$final_phase)"
        false
    fi

    echo "# SUCCESS: Pod with file cache survived soomkiller (swap=${swap_bytes}, cache=${cache_bytes})"
}
