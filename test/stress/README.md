# Stress Test

Sysbench-based stress test for evaluating swap behavior under memory pressure.

## Requirements

### Kubernetes Components

The following must be deployed in the `kube-soomkiller` namespace:

- **MariaDB StatefulSet** - Target workload (`deploy/mariadb-*.yaml`)
- **Sysbench Deployment** - Load generator (`deploy/sysbench-deployment.yaml`)
- **Prometheus** - Metrics collection (`deploy/prometheus-*.yaml`)
- **node-exporter DaemonSet** - Swap I/O metrics (`deploy/node-exporter-daemonset.yaml`)

### Prometheus Metrics

The script queries these metrics:

| Metric | Source | Purpose |
|--------|--------|---------|
| `container_memory_working_set_bytes` | cadvisor | Pod memory usage |
| `container_memory_swap` | cadvisor | Pod swap usage |
| `node_vmstat_pswpout` | node-exporter | Pages swapped out/sec |
| `node_vmstat_pswpin` | node-exporter | Pages swapped in/sec |
| `container_cpu_usage_seconds_total` | cadvisor | CPU usage |

### Tools

- `kubectl` with access to the cluster
- `jq` for JSON processing

## Usage

```bash
./run-test.sh <threads> [duration]
```

### Arguments

| Argument | Description | Default |
|----------|-------------|---------|
| `threads` | Number of sysbench threads | (required) |
| `duration` | Test duration in seconds | 60 |

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `NAMESPACE` | Kubernetes namespace | `kube-soomkiller` |
| `KUBE_CONTEXT` | kubectl context | `k3s` |
| `MARIADB_POD` | Target MariaDB pod | `mariadb-0` |
| `MYSQL_HOST` | MariaDB hostname | `$MARIADB_POD.mariadb` |

## Examples

### Basic test (50 threads, 60 seconds)

```bash
./run-test.sh 50
```

### Extended test (150 threads, 2 minutes)

```bash
./run-test.sh 150 120
```

### Test on different MariaDB replica

```bash
MARIADB_POD=mariadb-1 MYSQL_HOST=mariadb-1.mariadb ./run-test.sh 150 120
```

## Output

The script outputs JSON with test results and metrics:

```json
{
  "test": {
    "threads": 150,
    "duration": 120,
    "target": "mariadb-0.mariadb",
    "pod": "mariadb-0",
    "node": "k3s-worker2",
    "workload": "oltp_read_write",
    "table_size": 500000,
    "tables": 4
  },
  "timing": {
    "start": "2026-01-29T15:16:03Z",
    "end": "2026-01-29T15:16:36Z",
    "start_epoch": 1769699763,
    "end_epoch": 1769699796,
    "actual_duration": 33
  },
  "sysbench": {
    "tps": 184.51,
    "qps": 3691.84,
    "avg_latency_ms": 803.01,
    "p95_latency_ms": 2009.23
  },
  "outcome": {
    "status": "Survived",
    "pod_restarts": 0,
    "restarts_during_test": 0,
    "last_termination_reason": ""
  },
  "metrics": {
    "memory": {
      "avg_bytes": 493261970,
      "peak_bytes": 536342528,
      "avg_mb": 470,
      "peak_mb": 511
    },
    "swap": {
      "avg_bytes": 14736822,
      "peak_bytes": 20631552,
      "avg_mb": 14,
      "peak_mb": 19
    },
    "swap_io": {
      "peak_pswpout_per_sec": 503,
      "peak_pswpin_per_sec": 0
    },
    "cpu": {
      "avg_percent": 104.2,
      "peak_percent": 179.6
    },
    "time_series": {
      "pswpout": [[1769699763, "0"], [1769699768, "503.7"], ...],
      "pswpin": [[1769699763, "0"], [1769699768, "0.6"], ...],
      "memory_bytes": [[1769699763, "390610944"], ...],
      "swap_bytes": [[1769699763, "0"], ...],
      "cpu_rate": [[1769699763, "0.40"], ...]
    }
  }
}
```

## Test Phases

1. **Restart MariaDB pod** - Ensures clean baseline (0 swap usage)
2. **Configure MariaDB** - Sets `max_connections=500`, adjusts buffer pool
3. **Prepare sysbench tables** - Creates 4 tables with 500k rows each
4. **Run sysbench test** - Executes OLTP read/write workload
5. **Collect metrics** - Queries Prometheus for memory, swap, CPU metrics

## Tuning Thread Count

| Threads | Expected Behavior |
|---------|-------------------|
| < 130 | Memory stays under limit, no swap |
| 130-140 | Edge case, may or may not swap |
| 150+ | Triggers swap usage |
| 200+ | Heavy swap pressure, may cause OOMKill |

The sweet spot depends on MariaDB's memory limit (default 512Mi).

## Notes

- Each test restarts the MariaDB pod to ensure consistent baseline
- Progress is logged to stderr, JSON output goes to stdout
- Use `| jq .` to pretty-print output
- Use `2>/dev/null` to suppress progress logs
