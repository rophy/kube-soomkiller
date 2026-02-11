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
| `--memory-threshold-percent` | 99 | Kill pods with memory.current > this % of memory.max |
| `--swap-threshold-percent` | 0 | Kill pods with swap.current > this % of memory.max |
| `--file-cache-threshold-percent` | 1 | Kill pods with file cache < this % of memory.max |
| `--poll-interval` | 1s | How often to scan cgroups (minimum 1s) |
| `--dry-run` | true | Log actions without executing (also via `DRY_RUN` env var) |
| `--cgroup-root` | /sys/fs/cgroup | Path to cgroup v2 root |
| `--metrics-addr` | :8080 | Address to serve Prometheus metrics |
| `--protected-namespaces` | kube-system | Comma-separated list of namespaces to never kill pods from |

**How it works:** Every poll interval, the controller scans all burstable pod cgroups on the node. A pod is terminated when ALL three conditions are true:
- `memory.current / memory.max > memory-threshold-percent` (memory nearly full)
- `swap.current / memory.max > swap-threshold-percent` (swap in use)
- `file_cache / memory.max < file-cache-threshold-percent` (file cache exhausted)

All thresholds use `memory.max` as denominator for consistency. The file cache condition prevents false kills when the kernel is swapping anonymous pages while file cache is still reclaimable.

### Prometheus Metrics

The controller exposes metrics on `:8080/metrics`:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `soomkiller_node_swap_in_pages_total` | Counter | node | Total pages swapped in (from /proc/vmstat) |
| `soomkiller_node_swap_out_pages_total` | Counter | node | Total pages swapped out (from /proc/vmstat) |
| `soomkiller_pods_killed_total` | Counter | node | Total pods killed |
| `soomkiller_last_kill_timestamp_seconds` | Gauge | node | Unix timestamp of last pod kill |
| `soomkiller_container_swap_bytes` | Gauge | node, namespace, pod, container | Swap usage in bytes |
| `soomkiller_container_swap_max_bytes` | Gauge | node, namespace, pod, container | Swap limit in bytes |
| `soomkiller_container_memory_current_bytes` | Gauge | node, namespace, pod, container | Memory usage in bytes |
| `soomkiller_container_memory_max_bytes` | Gauge | node, namespace, pod, container | Memory limit in bytes |
| `soomkiller_container_file_cache_bytes` | Gauge | node, namespace, pod, container | File cache (page cache) in bytes |
| `soomkiller_config_memory_threshold_percent` | Gauge | node | Configured memory threshold % |
| `soomkiller_config_swap_threshold_percent` | Gauge | node | Configured swap threshold % |
| `soomkiller_config_file_cache_threshold_percent` | Gauge | node | Configured file cache threshold % |
| `soomkiller_config_dry_run` | Gauge | node | 1 if dry-run mode, 0 otherwise |

**Note:** Container metrics are only emitted for burstable pods on the node. You can calculate swap percentage in PromQL:
```promql
soomkiller_container_swap_bytes / soomkiller_container_memory_max_bytes * 100
```

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

When MariaDB's swap usage exceeds the threshold (default 1% of memory limit), soomkiller will terminate the pod gracefully.

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

Proactively terminate pods under memory pressure before the system becomes unresponsive. Swap provides a natural "buffer" - pods under pressure are stalled on swap I/O, giving the controller time to detect and act.

```
┌─────────────────────────────────────────────────────────────┐
│                       Architecture                          │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│   Every poll-interval (1s default)                          │
│          │                                                  │
│          ▼                                                  │
│   ┌─────────────────┐      ┌─────────────────────────────┐  │
│   │   Controller    │      │  Per-pod metrics (cgroup)   │  │
│   │   (DaemonSet)   │─────▶│  - memory.current / .max    │  │
│   └────────┬────────┘      │  - swap.current             │  │
│            │               │  - memory.stat (file cache)  │  │
│            │               └─────────────────────────────┘  │
│            │                                                │
│            │ Kill when ALL true:                            │
│            │   memory > A% AND swap > B% AND cache < C%    │
│            ▼                                                │
│   ┌─────────────────┐                                       │
│   │ kubectl delete  │──▶ SIGTERM ──▶ Grace Period ──▶ Clean │
│   │ (graceful)      │                                       │
│   └─────────────────┘                                       │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## How It Works

### 1. Periodic Cgroup Scanning

Every poll interval (default 1s), the controller scans all pod cgroups on the node. This is a lightweight filesystem operation with no Kubernetes API calls. The scan reads:

- `memory.current` - current memory usage in bytes (includes anon + file cache)
- `memory.max` - memory limit in bytes
- `memory.swap.current` - current swap usage in bytes
- `memory.stat` - detailed memory stats (parsed for `file` field = file cache bytes)

Only burstable pods are scanned, since guaranteed pods don't use swap and besteffort pods have no memory limits.

### 2. Threshold Check

For each container, the controller calculates three percentages (all relative to `memory.max`):
```
memory_percent    = memory.current / memory.max * 100
swap_percent      = swap.current   / memory.max * 100
file_cache_percent = file_cache    / memory.max * 100
```

A container is a kill candidate when ALL three conditions are true:
- `memory_percent > memory-threshold-percent` (default 99%)
- `swap_percent > swap-threshold-percent` (default 0%)
- `file_cache_percent < file-cache-threshold-percent` (default 1%)

### 3. Pod Selection and Termination

Kill all pods where:
1. At least one container meets all three conditions above
2. Pod is not in a protected namespace
3. Pod is not already terminating

**Key insight:** The file cache condition prevents false kills. With `vm.swappiness > 0`, the kernel may swap anonymous pages while file cache is still reclaimable. Checking that file cache is nearly zero ensures the pod is truly under memory pressure, not just experiencing normal swap balancing.

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

### Tuning the Thresholds

The three thresholds work together as an AND gate. All must be satisfied to trigger a kill:

| Flag | Default | Raise to... | Effect |
|------|---------|-------------|--------|
| `--memory-threshold-percent` | 99 | 95 | Kill earlier, before memory is completely full |
| `--swap-threshold-percent` | 0 | 5 | Tolerate small amounts of swap before killing |
| `--file-cache-threshold-percent` | 1 | 5 | Require more file cache to be evicted before killing |

Start with the defaults and adjust based on your workload. The defaults kill when memory is nearly full, any swap is in use, and file cache is nearly zero - indicating genuine memory pressure rather than normal kernel swap balancing.

## Limitations

### Per-Pod Swap I/O Attribution

cgroup v2 does not expose per-cgroup `pswpin`/`pswpout` counters. We use per-pod `memory.swap.current` / `memory.max` for threshold-based termination.

This means we detect swap usage (bytes allocated), not swap I/O rate (pages/sec). A pod with allocated swap that isn't actively thrashing will still be terminated if above threshold.

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

## References

- [Kubernetes NodeSwap Feature](https://kubernetes.io/docs/concepts/architecture/nodes/#swap-memory)
- [cgroups v2 Memory Controller](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory)
- [PSI - Pressure Stall Information](https://docs.kernel.org/accounting/psi.html)
- [Kubernetes Issue #40157 - Make OOM not be SIGKILL](https://github.com/kubernetes/kubernetes/issues/40157)
