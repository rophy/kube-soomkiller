# kube-soomkiller

A Kubernetes controller that provides graceful pod termination under memory pressure, as an alternative to the kernel's immediate SIGKILL via cgroup OOM killer.

**Name origin:**
- **s**oft **oom** **killer** - graceful termination instead of immediate SIGKILL
- **s**wap **oom** **killer** - swap-aware memory pressure management

## Getting Started

### Prerequisites

- Kubernetes cluster with swap enabled on nodes (`NodeSwap` feature gate)
- NBD-based swap with tc rate limiting configured on target nodes
- Nodes labeled with `swap=enabled`

### Installation

```bash
# Deploy the controller
kubectl apply -f deploy/namespace.yaml
kubectl apply -f deploy/serviceaccount.yaml
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/daemonset.yaml

# Verify it's running
kubectl get pods -n kube-soomkiller
```

### Configuration

Edit `deploy/daemonset.yaml` to adjust parameters:

| Flag | Default | Description |
|------|---------|-------------|
| `--tc-queue-threshold` | 100 | tc queue depth (packets) to trigger action |
| `--psi-threshold` | 50 | PSI full avg10 threshold for pod selection |
| `--poll-interval` | 5s | How often to check metrics |
| `--dry-run` | true | Log actions without executing |
| `--tc-device` | lo | Network device for tc stats |

### Building from Source

```bash
# Build binary
make build

# Build container image
make image

# Run tests
make test
```

## Problem Statement

When a pod exceeds its memory limit, the Linux kernel's OOM killer sends SIGKILL - an immediate, uninterruptible termination. This causes:

- Data loss (uncommitted transactions, unflushed buffers)
- Corruption risk (incomplete writes)
- Long recovery times (crash recovery, WAL replay)
- No opportunity for graceful shutdown

**Goal:** Give pods configurable grace time to shut down cleanly before being killed.

## Solution Overview

Use NBD-based swap with traffic control (tc) rate limiting to create a "pressure buffer" that slows down pods under memory pressure instead of killing them immediately. A controller monitors this buffer and proactively terminates pods before the system deadlocks.

```
┌──────────────────────────────────────────────────────────────┐
│                         Architecture                          │
├──────────────────────────────────────────────────────────────┤
│                                                              │
│   Pod Memory Pressure                                        │
│          │                                                   │
│          ▼                                                   │
│   ┌─────────────┐     ┌─────────────┐     ┌─────────────┐   │
│   │  NBD Swap   │────▶│  tc Queue   │────▶│  NBD Server │   │
│   │  (write)    │     │  (buffer)   │     │  (drain)    │   │
│   └─────────────┘     └─────────────┘     └─────────────┘   │
│                              │                               │
│                              │ monitor                       │
│                              ▼                               │
│                    ┌─────────────────┐                       │
│                    │   Controller    │                       │
│                    │   (DaemonSet)   │                       │
│                    └────────┬────────┘                       │
│                             │                                │
│                             │ kubectl delete pod             │
│                             ▼                                │
│                    ┌─────────────────┐                       │
│                    │ Graceful Stop   │                       │
│                    │ (SIGTERM + wait)│                       │
│                    └─────────────────┘                       │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

## Components

### 1. NBD Swap with tc Rate Limiting

Network Block Device (NBD) provides swap over a loopback connection. Traffic control (tc) rate limits this connection, creating an artificial bottleneck.

**How it works:**
- Swap writes go through NBD over loopback
- tc HTB qdisc limits bandwidth (e.g., 50 Mbit/s)
- When write rate exceeds limit, packets queue up
- Queued packets = time buffer before deadlock

**Configuration:**
```bash
# Create NBD swap file
dd if=/dev/zero of=/swapfile bs=1M count=6144
nbd-server 10809 /swapfile

# Rate limit loopback
tc qdisc add dev lo root handle 1: htb default 10
tc class add dev lo parent 1: classid 1:10 htb rate 50mbit

# Connect and enable
nbd-client localhost 10809 /dev/nbd0
mkswap /dev/nbd0
swapon /dev/nbd0
```

### 2. Controller DaemonSet

Runs on each swap-enabled node, monitoring pressure and taking action.

**Monitors:**
| Metric | Source | Purpose |
|--------|--------|---------|
| tc queue depth | `tc -s qdisc show` | System-level pressure indicator |
| PSI full avg10 | cgroup `memory.pressure` | Per-pod thrashing detection |
| Swap usage | cgroup `memory.swap.current` | Identify pods using swap |

**Trigger Condition:**
```
tc_queue_depth > threshold
```

When tc queue fills beyond threshold, the system is at risk of deadlock (packets dropped, NBD requests lost, processes stuck in D state).

**Pod Selection:**
```
candidate_pods = pods where (swap_usage > 0)
victim = max(candidate_pods, key=psi_full_avg10)
```

Select the pod with:
1. Non-zero swap usage (actively using swap)
2. Highest PSI `full` value (most severe memory stalls)

**Action:**
```bash
kubectl delete pod <victim> --grace-period=<configured>
```

Using `kubectl delete` instead of direct SIGTERM because:
- Kubernetes handles SIGTERM → grace period → SIGKILL
- Proper cleanup (endpoint removal, finalizers)
- Respects pod's `terminationGracePeriodSeconds`
- Controller only needs K8s API access

### 3. Kubernetes Configuration

**Kubelet swap settings:**
```yaml
featureGates:
  NodeSwap: true
memorySwap:
  swapBehavior: LimitedSwap
```

**Node labeling:**
```bash
kubectl label node <node> swap=enabled
```

**DaemonSet node selector:**
```yaml
nodeSelector:
  swap: enabled
```

## Configuration Options

| Parameter | Description | Default |
|-----------|-------------|---------|
| `tcQueueThreshold` | Queue depth triggering action | Configurable per node |
| `pollInterval` | How often to check metrics | 5s |
| `cooldownPeriod` | Wait time after killing a pod | 30s |
| `minPsiThreshold` | Minimum PSI to consider pod as candidate | 10% |
| `dryRun` | Log actions without executing | false |

## Metrics Explained

### tc Queue Depth

```bash
$ tc -s qdisc show dev lo
qdisc htb 1: root ... direct_packets_stat 0
 Sent 1234567 bytes 8901 pkt (dropped 0, overlimits 5678 requeues 0)
 backlog 12345b 89p requeues 0
         ^^^^^^ ^^^
         bytes  packets in queue
```

When `backlog` grows and `dropped` increases, the queue is overflowing.

### PSI (Pressure Stall Information)

```bash
$ cat /sys/fs/cgroup/.../memory.pressure
some avg10=17.42 avg60=3.24 avg300=0.68 total=2649745
full avg10=13.37 avg60=2.41 avg300=0.50 total=2098080
```

- `some`: % of time at least one task stalled on memory
- `full`: % of time ALL tasks stalled on memory
- `avg10`: 10-second moving average
- `total`: cumulative stall time in microseconds

High `full` indicates thrashing - the pod is struggling but may not be growing swap (same pages swapped in/out repeatedly).

### Swap Usage

```bash
$ cat /sys/fs/cgroup/.../memory.swap.current
20971520  # bytes
```

Pods with swap > 0 are candidates for termination under pressure.

## Why This Works

### Traditional OOM Kill
```
Memory limit hit → SIGKILL → Immediate death
```

### Soft OOM Kill
```
Memory limit hit → Swap to NBD → tc throttles → Queue fills → Controller detects → kubectl delete → SIGTERM → Grace period → Clean shutdown
```

The tc queue acts as a time buffer. Instead of instant death, the pod slows down while the controller has time to:
1. Detect the pressure
2. Select the appropriate victim
3. Initiate graceful termination

## Limitations

### tc Queue Overflow
If sustained swap I/O exceeds the rate limit for too long, the tc queue overflows, packets drop, and NBD deadlocks. The controller must act before this happens.

**Sizing consideration:**
```
Queue can buffer: queue_size_bytes / (swap_rate - drain_rate)
```

If incoming swap rate sustains above drain rate, no queue size is sufficient. The controller must terminate pods before this becomes critical.

### Per-Pod Swap I/O Attribution
cgroup v2 does not expose per-cgroup `pswpin`/`pswpout` counters. We use PSI as a proxy for thrashing detection instead of direct swap I/O measurement.

### Single Point of Failure
The controller DaemonSet must be highly available. If it fails, the system falls back to kernel OOM kill behavior (after potential deadlock).

## Comparison with Alternatives

| Approach | Signal | Grace Period | Scope |
|----------|--------|--------------|-------|
| Kernel OOM Kill | SIGKILL | None | Per-container |
| Memory QoS (cgroups v2) | Throttle | N/A | Per-container |
| Kubelet Node Eviction | SIGTERM | Yes | Node-wide |
| **Soft OOM Killer** | SIGTERM | Yes | Per-pod, swap-aware |

## Future Enhancements

1. **eBPF-based swap I/O tracking** - More accurate per-pod swap I/O attribution
2. **Prometheus metrics export** - Integrate with existing monitoring
3. **PodDisruptionBudget awareness** - Optionally respect PDB (may need override for emergencies)
4. **Predictive termination** - Use swap growth rate to predict overflow before it happens

## References

- [Kubernetes NodeSwap Feature](https://kubernetes.io/docs/concepts/architecture/nodes/#swap-memory)
- [cgroups v2 Memory Controller](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory)
- [PSI - Pressure Stall Information](https://docs.kernel.org/accounting/psi.html)
- [NBD - Network Block Device](https://nbd.sourceforge.io/)
- [tc - Traffic Control](https://man7.org/linux/man-pages/man8/tc.8.html)
- [Kubernetes Issue #40157 - Make OOM not be SIGKILL](https://github.com/kubernetes/kubernetes/issues/40157)
