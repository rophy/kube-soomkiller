#!/bin/bash
#
# Sysbench test runner with metrics collection for kube-soomkiller
#
# Each test run:
#   1. Restarts MariaDB pod (clean baseline)
#   2. Configures MariaDB (max_connections, buffer pool)
#   3. Prepares sysbench tables
#   4. Runs sysbench test
#   5. Collects metrics from Prometheus
#
# Usage: ./run-test.sh <threads> [duration]
#
# Arguments:
#   threads       Number of sysbench threads
#   duration      Test duration in seconds (default: 60)
#
# Environment variables:
#   NAMESPACE     Kubernetes namespace (default: kube-soomkiller)
#   KUBE_CONTEXT  kubectl context (default: k3s)
#   MARIADB_POD   MariaDB pod name (default: mariadb-0)
#   MYSQL_HOST    MariaDB hostname (default: $MARIADB_POD.mariadb)
#
# Workload config (memory-intensive):
#   - table-size: 500000 rows per table (~500MB dataset)
#   - tables: 4
#   - workload: oltp_read_write
#
# Examples:
#   ./run-test.sh 50
#   ./run-test.sh 100 120
#   MARIADB_POD=mariadb-1 ./run-test.sh 200
#

set -e

usage() {
  echo "Usage: $0 <threads> [duration]"
  echo ""
  echo "Arguments:"
  echo "  threads       Number of sysbench threads"
  echo "  duration      Test duration in seconds (default: 60)"
  echo ""
  echo "Environment variables:"
  echo "  NAMESPACE     Kubernetes namespace (default: kube-soomkiller)"
  echo "  KUBE_CONTEXT  kubectl context (default: k3s)"
  echo "  MARIADB_POD   MariaDB pod name (default: mariadb-0)"
  echo "  MYSQL_HOST    MariaDB hostname (default: \$MARIADB_POD.mariadb)"
  echo ""
  echo "Examples:"
  echo "  $0 50"
  echo "  $0 100 120"
  echo "  MARIADB_POD=mariadb-1 $0 200"
  exit 1
}

if [ -z "$1" ]; then
  usage
fi

THREADS="$1"
DURATION="${2:-60}"

# Configuration (can be overridden via environment)
NAMESPACE="${NAMESPACE:-kube-soomkiller}"
KUBE_CONTEXT="${KUBE_CONTEXT:-k3s}"
MARIADB_POD="${MARIADB_POD:-mariadb-0}"
# Use pod's stable DNS name (pod.service) to avoid headless service load balancing
# which could route to different replicas for cleanup vs prepare vs run
MYSQL_HOST="${MYSQL_HOST:-${MARIADB_POD}.mariadb}"

# kubectl wrapper with context and namespace
kctl() {
  kubectl --context "$KUBE_CONTEXT" -n "$NAMESPACE" "$@"
}

# Memory-intensive workload config
# Large tables + read-write workload = more buffer pool + transaction log pressure
TABLE_SIZE=500000
TABLES=4
WORKLOAD="oltp_read_write"

echo "=== Phase 1: Restart MariaDB pod ===" >&2
kctl delete pod "$MARIADB_POD" --wait=true >&2
echo "Waiting for pod to be ready..." >&2
kctl wait --for=condition=Ready pod/"$MARIADB_POD" --timeout=120s >&2

# Wait a bit more for MariaDB to fully initialize
sleep 5

echo "=== Phase 2: Configure MariaDB ===" >&2
# Retry loop for MariaDB configuration (may need time to accept connections)
for i in {1..10}; do
  if kctl exec "$MARIADB_POD" -- mariadb -uroot -ptestpass -e "
    SET GLOBAL max_connections = 2000;
    SET GLOBAL max_prepared_stmt_count = 100000;
  " >&2 2>/dev/null; then
    echo "MariaDB configured successfully" >&2
    break
  fi
  echo "Waiting for MariaDB to accept connections... ($i/10)" >&2
  sleep 2
done

echo "=== Phase 3: Prepare sysbench tables ===" >&2
SYSBENCH_POD=$(kctl get pod -l app=sysbench -o jsonpath='{.items[0].metadata.name}')

# Cleanup existing tables first (in case of previous run with PVC)
echo "Cleaning up existing sysbench tables (if any)..." >&2
kctl exec "$SYSBENCH_POD" -- sysbench "$WORKLOAD" \
  --mysql-host="$MYSQL_HOST" \
  --mysql-user=root \
  --mysql-password=testpass \
  --mysql-db=testdb \
  --tables="$TABLES" \
  cleanup >&2 2>/dev/null || true

# Retry loop for sysbench prepare (network may need time to be ready)
for i in {1..10}; do
  if kctl exec "$SYSBENCH_POD" -- sysbench "$WORKLOAD" \
    --mysql-host="$MYSQL_HOST" \
    --mysql-user=root \
    --mysql-password=testpass \
    --mysql-db=testdb \
    --table-size="$TABLE_SIZE" \
    --tables="$TABLES" \
    prepare >&2 2>/dev/null; then
    echo "Sysbench tables prepared successfully" >&2
    break
  fi
  if [ $i -eq 10 ]; then
    echo "Failed to prepare sysbench tables after 10 attempts" >&2
    exit 1
  fi
  echo "Waiting for MariaDB connection from sysbench... ($i/10)" >&2
  sleep 3
done

echo "=== Phase 4: Run sysbench test ===" >&2

# Get node info and container ID (for filtering metrics after pod recreation)
NODE=$(kctl get pod "$MARIADB_POD" -o jsonpath='{.spec.nodeName}')
CONTAINER_ID=$(kctl get pod "$MARIADB_POD" -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's|containerd://||')

# Record initial restart count to detect OOMKill during test
INITIAL_RESTARTS=$(kctl get pod "$MARIADB_POD" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo "0")

# Record start time
START_TIME=$(date +%s)
START_TIME_ISO=$(date -u +%Y-%m-%dT%H:%M:%SZ)

echo "Starting test: $THREADS threads, $DURATION seconds" >&2
echo "Target: $MYSQL_HOST ($MARIADB_POD) on node $NODE" >&2
echo "Start time: $START_TIME_ISO" >&2

# Run sysbench (don't exit on failure - pod may OOMKill)
set +e
SYSBENCH_OUTPUT=$(kctl exec "$SYSBENCH_POD" -- sysbench "$WORKLOAD" \
  --mysql-host="$MYSQL_HOST" \
  --mysql-user=root \
  --mysql-password=testpass \
  --mysql-db=testdb \
  --table-size="$TABLE_SIZE" \
  --tables="$TABLES" \
  --threads="$THREADS" \
  --time="$DURATION" \
  --thread-init-timeout=300 \
  run 2>&1)
SYSBENCH_EXIT_CODE=$?
set -e

# Record end time
END_TIME=$(date +%s)
END_TIME_ISO=$(date -u +%Y-%m-%dT%H:%M:%SZ)

echo "End time: $END_TIME_ISO" >&2

if [ $SYSBENCH_EXIT_CODE -ne 0 ]; then
  echo "Sysbench exited with code $SYSBENCH_EXIT_CODE (possible OOMKill)" >&2
fi

echo "=== Phase 5: Collect metrics ===" >&2

# Parse sysbench output
TPS=$(echo "$SYSBENCH_OUTPUT" | grep -oP 'transactions:\s+\d+\s+\(\K[0-9.]+' || echo "0")
QPS=$(echo "$SYSBENCH_OUTPUT" | grep -oP 'queries:\s+\d+\s+\(\K[0-9.]+' || echo "0")
AVG_LATENCY=$(echo "$SYSBENCH_OUTPUT" | grep -oP 'avg:\s+\K[0-9.]+' || echo "0")
P95_LATENCY=$(echo "$SYSBENCH_OUTPUT" | grep -oP '95th percentile:\s+\K[0-9.]+' || echo "0")
TOTAL_TIME=$(echo "$SYSBENCH_OUTPUT" | grep -oP 'total time:\s+\K[0-9.]+' || echo "")

# Fall back to calculated duration if sysbench didn't report total time (e.g., OOMKill)
if [ -z "$TOTAL_TIME" ] || [ "$TOTAL_TIME" = "0" ]; then
  TOTAL_TIME=$((END_TIME - START_TIME))
fi

# Wait for pod state to stabilize after potential OOMKill
# The pod may be restarting, give it time to update status
echo "Waiting for pod state to stabilize..." >&2
sleep 5

# Check pod status with retry (pod may still be initializing after OOMKill)
for i in {1..5}; do
  RESTARTS=$(kctl get pod "$MARIADB_POD" -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo "0")
  POD_STATUS=$(kctl get pod "$MARIADB_POD" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
  LAST_STATE=$(kctl get pod "$MARIADB_POD" -o jsonpath='{.status.containerStatuses[0].lastState.terminated.reason}' 2>/dev/null || echo "")

  # If pod is running and we got valid data, break
  if [ "$POD_STATUS" = "Running" ] && [ -n "$RESTARTS" ]; then
    break
  fi
  echo "Pod not ready yet, retrying... ($i/5)" >&2
  sleep 2
done

# Determine outcome by comparing restart counts
# If restarts increased during test, pod was OOMKilled
RESTART_DIFF=$((RESTARTS - INITIAL_RESTARTS))
if [ "$RESTART_DIFF" -gt 0 ] || [ "$LAST_STATE" = "OOMKilled" ] || [ "$POD_STATUS" != "Running" ]; then
  OUTCOME="OOMKilled"
  echo "Pod was OOMKilled (restarts: $INITIAL_RESTARTS -> $RESTARTS, lastState: $LAST_STATE)" >&2
else
  OUTCOME="Survived"
fi

# Query Prometheus for metrics during test period
# Prometheus is in the same namespace (kube-soomkiller)
PROM_POD="prometheus-0"

# Swap I/O rates (pswpout/pswpin)
PSWPOUT_DATA=$(kctl exec "$PROM_POD" -- sh -c \
  "wget -qO- 'http://localhost:9090/api/v1/query_range?query=rate(node_vmstat_pswpout%5B15s%5D)&start=$START_TIME&end=$END_TIME&step=5'" 2>/dev/null)

PSWPIN_DATA=$(kctl exec "$PROM_POD" -- sh -c \
  "wget -qO- 'http://localhost:9090/api/v1/query_range?query=rate(node_vmstat_pswpin%5B15s%5D)&start=$START_TIME&end=$END_TIME&step=5'" 2>/dev/null)

# Container memory usage (filter by container ID to avoid stale metrics from previous pod)
MEMORY_DATA=$(kctl exec "$PROM_POD" -- sh -c \
  "wget -qO- 'http://localhost:9090/api/v1/query_range?query=container_memory_usage_bytes%7Bname%3D%22${CONTAINER_ID}%22%7D&start=$START_TIME&end=$END_TIME&step=5'" 2>/dev/null)

# Container swap usage (filter by container ID)
SWAP_DATA=$(kctl exec "$PROM_POD" -- sh -c \
  "wget -qO- 'http://localhost:9090/api/v1/query_range?query=container_memory_swap%7Bname%3D%22${CONTAINER_ID}%22%7D&start=$START_TIME&end=$END_TIME&step=5'" 2>/dev/null)

# Container CPU usage (rate of cpu seconds, filter by container ID)
CPU_DATA=$(kctl exec "$PROM_POD" -- sh -c \
  "wget -qO- 'http://localhost:9090/api/v1/query_range?query=rate(container_cpu_usage_seconds_total%7Bname%3D%22${CONTAINER_ID}%22%7D%5B30s%5D)&start=$START_TIME&end=$END_TIME&step=5'" 2>/dev/null)

# Extract time series from Prometheus responses
extract_values() {
  local data="$1"
  local node_filter="$2"
  echo "$data" | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
    for r in d.get('data', {}).get('result', []):
        if '$node_filter' and r.get('metric', {}).get('node') != '$node_filter':
            continue
        print(json.dumps(r.get('values', [])))
        break
except:
    print('[]')
" 2>/dev/null || echo "[]"
}

PSWPOUT_VALUES=$(extract_values "$PSWPOUT_DATA" "$NODE")
PSWPIN_VALUES=$(extract_values "$PSWPIN_DATA" "$NODE")
MEMORY_VALUES=$(extract_values "$MEMORY_DATA" "")
SWAP_VALUES=$(extract_values "$SWAP_DATA" "")
CPU_VALUES=$(extract_values "$CPU_DATA" "")

# Calculate peak values
calc_peak() {
  echo "$1" | python3 -c "
import sys, json
try:
    vals = json.load(sys.stdin)
    if vals:
        print(int(max(float(v[1]) for v in vals)))
    else:
        print(0)
except:
    print(0)
" 2>/dev/null || echo "0"
}

calc_avg() {
  echo "$1" | python3 -c "
import sys, json
try:
    vals = json.load(sys.stdin)
    if vals:
        print(int(sum(float(v[1]) for v in vals) / len(vals)))
    else:
        print(0)
except:
    print(0)
" 2>/dev/null || echo "0"
}

PEAK_PSWPOUT=$(calc_peak "$PSWPOUT_VALUES")
PEAK_PSWPIN=$(calc_peak "$PSWPIN_VALUES")
AVG_MEMORY=$(calc_avg "$MEMORY_VALUES")
AVG_SWAP=$(calc_avg "$SWAP_VALUES")
PEAK_MEMORY=$(calc_peak "$MEMORY_VALUES")
PEAK_SWAP=$(calc_peak "$SWAP_VALUES")

# CPU is a rate (0-N cores), calculate as percentage of 1 core
AVG_CPU=$(echo "$CPU_VALUES" | python3 -c "
import sys, json
try:
    vals = json.load(sys.stdin)
    if vals:
        print(round(sum(float(v[1]) for v in vals) / len(vals) * 100, 1))
    else:
        print(0)
except:
    print(0)
" 2>/dev/null || echo "0")

PEAK_CPU=$(echo "$CPU_VALUES" | python3 -c "
import sys, json
try:
    vals = json.load(sys.stdin)
    if vals:
        print(round(max(float(v[1]) for v in vals) * 100, 1))
    else:
        print(0)
except:
    print(0)
" 2>/dev/null || echo "0")

# Output JSON result
cat << EOF
{
  "test": {
    "threads": $THREADS,
    "duration": $DURATION,
    "target": "$MYSQL_HOST",
    "pod": "$MARIADB_POD",
    "node": "$NODE",
    "workload": "$WORKLOAD",
    "table_size": $TABLE_SIZE,
    "tables": $TABLES
  },
  "timing": {
    "start": "$START_TIME_ISO",
    "end": "$END_TIME_ISO",
    "start_epoch": $START_TIME,
    "end_epoch": $END_TIME,
    "actual_duration": $TOTAL_TIME
  },
  "sysbench": {
    "tps": $TPS,
    "qps": $QPS,
    "avg_latency_ms": $AVG_LATENCY,
    "p95_latency_ms": $P95_LATENCY
  },
  "outcome": {
    "status": "$OUTCOME",
    "pod_restarts": $RESTARTS,
    "restarts_during_test": $RESTART_DIFF,
    "last_termination_reason": "$LAST_STATE"
  },
  "metrics": {
    "memory": {
      "avg_bytes": $AVG_MEMORY,
      "peak_bytes": $PEAK_MEMORY,
      "avg_mb": $((AVG_MEMORY / 1024 / 1024)),
      "peak_mb": $((PEAK_MEMORY / 1024 / 1024))
    },
    "swap": {
      "avg_bytes": $AVG_SWAP,
      "peak_bytes": $PEAK_SWAP,
      "avg_mb": $((AVG_SWAP / 1024 / 1024)),
      "peak_mb": $((PEAK_SWAP / 1024 / 1024))
    },
    "swap_io": {
      "peak_pswpout_per_sec": $PEAK_PSWPOUT,
      "peak_pswpin_per_sec": $PEAK_PSWPIN
    },
    "cpu": {
      "avg_percent": $AVG_CPU,
      "peak_percent": $PEAK_CPU
    },
    "time_series": {
      "pswpout": $PSWPOUT_VALUES,
      "pswpin": $PSWPIN_VALUES,
      "memory_bytes": $MEMORY_VALUES,
      "swap_bytes": $SWAP_VALUES,
      "cpu_rate": $CPU_VALUES
    }
  }
}
EOF
