# kube-soomkiller

A Kubernetes controller that provides graceful pod termination under memory pressure, as an alternative to the kernel's immediate SIGKILL via cgroup OOM killer.

**Name origin:**
- **s**oft **oom** **killer** - graceful termination instead of immediate SIGKILL
- **s**wap **oom** **killer** - swap-aware memory pressure management

## Getting Started

### Prerequisites

- Kubernetes cluster with swap enabled on nodes (`NodeSwap` feature gate)
- Swap configured on target nodes (dedicated swap disk recommended)
- Nodes labeled with `swap=enabled`

### Installation

**Using skaffold (recommended for development):**

```bash
skaffold run
```

**Manual deployment:**

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
| `--swap-threshold-percent` | 1 | Kill pods with swap usage > this % of memory limit |
| `--poll-interval` | 1s | How often to sample /proc/vmstat (minimum 1s) |
| `--dry-run` | true | Log actions without executing (also via `DRY_RUN` env var) |
| `--cgroup-root` | /sys/fs/cgroup | Path to cgroup v2 root |
| `--metrics-addr` | :8080 | Address to serve Prometheus metrics |
| `--protected-namespaces` | kube-system | Comma-separated list of namespaces to never kill pods from |

**How it works:** When any swap I/O is detected, the controller scans all pods on the node. Pods with `memory.swap.current / memory.max > swap-threshold-percent` are terminated. The threshold is expressed as a percentage of the pod's memory limit.

### Prometheus Metrics

The controller exposes metrics on `:8080/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `soomkiller_swap_io_rate_pages_per_second` | Gauge | Current swap I/O rate |
| `soomkiller_pods_killed_total` | Counter | Total pods killed |
| `soomkiller_last_kill_timestamp_seconds` | Gauge | Unix timestamp of last pod kill |
| `soomkiller_candidate_pods_count` | Gauge | Pods currently using swap |
| `soomkiller_pod_swap_bytes{namespace,pod}` | Gauge | Swap usage per pod |
| `soomkiller_pod_swap_percent{namespace,pod}` | Gauge | Swap usage as % of memory limit per pod |
| `soomkiller_pod_memory_max_bytes{namespace,pod}` | Gauge | Memory limit per pod |
| `soomkiller_config_swap_threshold_percent` | Gauge | Configured swap threshold % |
| `soomkiller_config_dry_run` | Gauge | 1 if dry-run mode, 0 otherwise |

**Health endpoint:** `/healthz` returns `ok` when healthy.

**Prometheus scraping:** The daemonset includes annotations for auto-discovery:
```yaml
annotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "8080"
  prometheus.io/path: "/metrics"
```

### Building from Source

```bash
# Build container image
make image

# Run linter and unit tests
make test-unit

# Run e2e tests (requires K3s cluster)
make test-e2e
```

### Testing with K3s (Multipass)

A complete test environment is provided using K3s on Multipass VMs with encrypted swap:

```bash
# Prerequisites: Install Multipass
# Ubuntu: sudo snap install multipass
# macOS: brew install multipass

# Create K3s cluster with 3 nodes (1 server + 2 workers with 6GB swap each)
./scripts/setup-k3s-multipass.sh up

# Export kubeconfig
./scripts/setup-k3s-multipass.sh kubeconfig

# Check cluster status
./scripts/setup-k3s-multipass.sh status

# Deploy kube-soomkiller and test workloads
export KUBECONFIG=~/.kube/k3s-multipass.yaml
skaffold run

# Verify swap is configured on workers
multipass exec k3s-worker1 -- free -h
multipass exec k3s-worker1 -- swapon --show

# Clean up
./scripts/setup-k3s-multipass.sh down
```

**Manual kubeconfig setup (if not using the script):**

```bash
# Get kubeconfig from K3s server
multipass exec k3s-server -- sudo cat /etc/rancher/k3s/k3s.yaml > ~/.kube/k3s-multipass.yaml

# Replace localhost with server IP
SERVER_IP=$(multipass info k3s-server --format json | jq -r '.info["k3s-server"].ipv4[0]')
sed -i "s/127.0.0.1/$SERVER_IP/g" ~/.kube/k3s-multipass.yaml

# Set permissions
chmod 600 ~/.kube/k3s-multipass.yaml

# Use it
export KUBECONFIG=~/.kube/k3s-multipass.yaml
kubectl get nodes
```

**Manual encrypted swap setup (for any Linux node):**

```bash
# Create swap file (6GB)
sudo mkdir -p /var/swap
sudo dd if=/dev/zero of=/var/swap/swapfile bs=1M count=6144
sudo chmod 600 /var/swap/swapfile

# Configure encrypted swap with ephemeral key
echo 'swap_crypt /var/swap/swapfile /dev/urandom swap,cipher=aes-xts-plain64,size=512' | sudo tee -a /etc/crypttab

# Enable the encrypted swap
sudo cryptdisks_start swap_crypt
sudo mkswap /dev/mapper/swap_crypt
sudo swapon /dev/mapper/swap_crypt

# Make persistent
echo '/dev/mapper/swap_crypt none swap sw 0 0' | sudo tee -a /etc/fstab

# Verify
swapon --show
sudo dmsetup status  # Should show "swap_crypt: ... crypt"
```

**Kubelet swap configuration (K3s < 1.32):**

K3s versions before 1.32 don't auto-read kubelet drop-in configs. Pass the config explicitly:

```bash
# Create kubelet config
sudo mkdir -p /etc/rancher/k3s
cat <<EOF | sudo tee /etc/rancher/k3s/kubelet-swap.yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
failSwapOn: false
memorySwap:
  swapBehavior: LimitedSwap
EOF

# Install K3s agent with swap support
curl -sfL https://get.k3s.io | \
  K3S_URL=https://<SERVER_IP>:6443 \
  K3S_TOKEN=<TOKEN> \
  INSTALL_K3S_EXEC='--kubelet-arg=config=/etc/rancher/k3s/kubelet-swap.yaml' \
  sh -
```

**Verifying swap is working with kubelet:**

```bash
# 1. Check kubelet is using the swap config
cat /etc/systemd/system/k3s-agent.service | grep kubelet-arg
# Should show: --kubelet-arg=config=/etc/rancher/k3s/kubelet-swap.yaml

# 2. Check swap is active on the node
free -h
# Should show non-zero Swap total

# 3. Verify kubelet allows swap (check node conditions)
kubectl describe node <node-name> | grep -i swap
# Should NOT show "NodeHasInsufficientSwap" condition

# 4. Deploy a test pod and verify it can use swap
kubectl run test-swap --image=alpine --restart=Never -- sleep infinity
kubectl exec test-swap -- cat /proc/self/cgroup
# Note the cgroup path, then check swap limit:
# On the node: cat /sys/fs/cgroup/<cgroup-path>/memory.swap.max
# Should show "max" (unlimited) for Burstable QoS pods with LimitedSwap

# 5. For pods using swap, check current swap usage:
# On the node: cat /sys/fs/cgroup/<cgroup-path>/memory.swap.current
```

**Running the stress test:**

```bash
# Run stress test with 50 threads for 60 seconds (outputs JSON with metrics)
./test/stress/run-test.sh 50 60

# Higher thread count to trigger swap pressure
./test/stress/run-test.sh 150 120

# Monitor soomkiller logs in another terminal
kubectl logs -n kube-soomkiller daemonset/kube-soomkiller -f
```

The script handles MariaDB restart, table preparation, sysbench execution, and Prometheus metrics collection. Output includes TPS, latency, memory/swap usage, and swap I/O time series.

When swap I/O exceeds 1000 pages/sec for 10 seconds, soomkiller will terminate the MariaDB pod gracefully.

**Running automated e2e tests:**

```bash
# Prerequisites: bats (apt install bats)
# Requires: K3s cluster running with context 'k3s'

# Run all e2e tests (includes suite setup)
bats test/e2e/

# Run specific test file (skips suite setup)
bats test/e2e/core_functionality.bats

# Run with custom context
KUBE_CONTEXT=minikube bats test/e2e/
```

## Problem Statement

When a pod exceeds its memory limit, the Linux kernel's OOM killer sends SIGKILL - an immediate, uninterruptible termination. This causes:

- Data loss (uncommitted transactions, unflushed buffers)
- Corruption risk (incomplete writes)
- Long recovery times (crash recovery, WAL replay)
- No opportunity for graceful shutdown

**Goal:** Give pods configurable grace time to shut down cleanly before being killed.

## Solution Overview

Monitor node-level swap I/O and proactively terminate pods under memory pressure before the system becomes unresponsive. Swap provides a natural "buffer" - pods under pressure are stalled on swap I/O, giving the controller time to detect and act.

```
┌─────────────────────────────────────────────────────────────┐
│                       Architecture                          │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│   /proc/vmstat (node-level)                                 │
│   ├── pswpin:  pages swapped in                             │
│   └── pswpout: pages swapped out                            │
│          │                                                  │
│          │ swap_io_rate > 0 (any swap activity)             │
│          ▼                                                  │
│   ┌─────────────────┐      ┌─────────────────────────────┐  │
│   │   Controller    │      │  Per-pod metrics (cgroup)   │  │
│   │   (DaemonSet)   │◀────▶│  - memory.swap.current      │  │
│   └────────┬────────┘      │  - memory.max               │  │
│            │               └─────────────────────────────┘  │
│            │                                                │
│            │ Kill all pods where:                           │
│            │   swap.current / memory.max > threshold        │
│            ▼                                                │
│   ┌─────────────────┐                                       │
│   │ kubectl delete  │──▶ SIGTERM ──▶ Grace Period ──▶ Clean │
│   │ (graceful)      │                                       │
│   └─────────────────┘                                       │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## How It Works

### 1. Node-Level Swap I/O Monitoring

The controller monitors `/proc/vmstat` for swap activity. This file shows system-wide kernel statistics and is not namespaced, so it can be read from within a container without special volume mounts:

```bash
$ cat /proc/vmstat | grep -E '^psw'
pswpin 12345    # cumulative pages swapped in
pswpout 67890   # cumulative pages swapped out
```

By sampling periodically, it calculates the swap I/O rate:
```
swap_io_rate = (pswpin_delta + pswpout_delta) / interval
```

### 2. Trigger Condition

```
swap_io_rate > 0
```

Any swap activity indicates memory pressure. The controller then scans all pods to find those exceeding the threshold.

### 3. Pod Selection and Termination

```
for each pod:
  swap_percent = memory.swap.current / memory.max * 100
  if swap_percent > swap_threshold_percent:
    delete pod
```

Kill all pods where:
1. Swap usage exceeds the configured threshold (% of memory limit)
2. Pod is not in a protected namespace

**Key insight:** Any swap usage means the pod exceeded its memory limit and would have been OOMKilled without swap. The threshold provides a buffer for edge cases (e.g., 1 byte swap).

### 4. Graceful Termination

```bash
kubectl delete pod <victim>
```

Using `kubectl delete` because:
- Kubernetes handles SIGTERM → grace period → SIGKILL
- Proper cleanup (endpoint removal, finalizers)
- Respects pod's `terminationGracePeriodSeconds`
- Controller only needs K8s API access

## Why This Works

### Traditional OOM Kill (without swap)
```
Memory limit hit → SIGKILL → Immediate death
```

### Soft OOM Kill (with swap + soomkiller)
```
Memory limit hit → Swap used → Controller detects → kubectl delete → SIGTERM → Grace period → Clean shutdown
```

**Key insight:** Any swap usage means the pod exceeded its memory limit. Without swap, this would have been an immediate OOMKill. With swap, the controller can detect this and terminate the pod gracefully.

## Metrics Explained

### Swap I/O Rate

```bash
$ cat /proc/vmstat | grep -E '^psw'
pswpin 12345
pswpout 67890
```

- `pswpin`: Pages read from swap (cumulative)
- `pswpout`: Pages written to swap (cumulative)

Sampled every second, delta calculated. Any rate > 0 triggers pod scanning.

### PSI (Pressure Stall Information)

```bash
$ cat /sys/fs/cgroup/.../memory.pressure
some avg10=17.42 avg60=3.24 avg300=0.68 total=2649745
full avg10=13.37 avg60=2.41 avg300=0.50 total=2098080
```

- `some`: % of time at least one task stalled on memory
- `full`: % of time ALL tasks stalled on memory
- `avg10`: 10-second moving average

High `full` indicates severe thrashing.

**Note:** PSI measures memory pressure broadly, not just swap I/O. A pod can have high PSI from page cache churn without using swap. This is why we filter by `swap_usage > 0`.

### Swap Usage

```bash
$ cat /sys/fs/cgroup/.../memory.swap.current
20971520  # bytes
```

Pods with swap > 0 are candidates for termination under pressure.

## Kubernetes Configuration

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

**Required volume mounts:**
- `/sys/fs/cgroup` (read-only) - for per-pod memory metrics and PSI

Note: `/proc/vmstat` is system-wide and accessible without special mounts. No privileged mode required.

## Deployment Recommendations

### Dedicated Swap Disk

For production, use a dedicated disk or partition for swap:

```bash
# Separate disk for swap
mkswap /dev/sdb
swapon /dev/sdb
```

This isolates swap I/O from the root filesystem, preventing swap activity from starving kubelet, etcd, and other control plane components.

### Tuning the Threshold

| Scenario | swap-threshold-percent | Description |
|----------|------------------------|-------------|
| Aggressive | 0.1% | Kill pods at first sign of swap |
| Balanced | 1% (default) | Allow minor swap before killing |
| Conservative | 5% | Allow more swap headroom |

Start with the default (1%) and adjust based on your workload characteristics. Lower values are more aggressive but may kill pods prematurely for brief memory spikes.

## Limitations

### Per-Pod Swap I/O Attribution

cgroup v2 does not expose per-cgroup `pswpin`/`pswpout` counters. We use:
- Node-level swap I/O as trigger (any swap activity)
- Per-pod `memory.swap.current` / `memory.max` for threshold-based kill

This means we detect node-level swap activity, then check each pod's swap usage as a percentage of its memory limit.

### Single Point of Failure

The controller DaemonSet must be running. If it fails:
- System falls back to kernel OOM kill behavior
- No graceful termination

**Important:** The controller must use Guaranteed QoS (set `requests = limits` for memory) to prevent itself from being swapped or selected as a victim. Without this, under memory pressure the controller's memory could be swapped, making it unresponsive when it's needed most.

## Comparison with Alternatives

| Approach | Signal | Grace Period | Scope |
|----------|--------|--------------|-------|
| Kernel OOM Kill | SIGKILL | None | Per-container |
| Memory QoS (cgroups v2) | Throttle | N/A | Per-container |
| Kubelet Node Eviction | SIGTERM | Yes | Node-wide threshold |
| **Soft OOM Killer** | SIGTERM | Yes | Per-pod, swap-aware |

## Future Enhancements

1. **eBPF-based swap I/O tracking** - Per-pod swap I/O attribution
2. **PodDisruptionBudget awareness** - Optionally respect PDB
3. **Predictive termination** - Use swap growth rate to predict pressure

## References

- [Kubernetes NodeSwap Feature](https://kubernetes.io/docs/concepts/architecture/nodes/#swap-memory)
- [cgroups v2 Memory Controller](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory)
- [PSI - Pressure Stall Information](https://docs.kernel.org/accounting/psi.html)
- [Kubernetes Issue #40157 - Make OOM not be SIGKILL](https://github.com/kubernetes/kubernetes/issues/40157)
