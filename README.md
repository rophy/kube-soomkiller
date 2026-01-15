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
| `--swap-io-threshold` | 1000 | Swap I/O rate (pages/sec) to trigger action |
| `--sustained-duration` | 10s | How long threshold must be exceeded |
| `--psi-threshold` | 50 | Minimum PSI full avg10 for pod selection |
| `--poll-interval` | 1s | How often to sample /proc/vmstat |
| `--cooldown-period` | 30s | Wait time after killing a pod |
| `--dry-run` | true | Log actions without executing |

**Note:** With 1s poll interval and 10s sustained duration, the controller requires 10 consecutive samples above threshold before acting. This filters out short spikes while remaining responsive to real pressure.

### Building from Source

```bash
# Build binary
make build

# Build container image
make image

# Run tests
make test
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

**Running the stress test:**

```bash
# Create sysbench database
kubectl exec -n kube-soomkiller mariadb-0 -- \
  mariadb -uroot -ptestpass -e "CREATE DATABASE IF NOT EXISTS sbtest;"

# Prepare tables
kubectl exec -n kube-soomkiller deploy/sysbench -- \
  sysbench /usr/share/sysbench/oltp_read_write.lua \
  --mysql-host=mariadb --mysql-port=3306 \
  --mysql-user=root --mysql-password=testpass \
  --mysql-db=sbtest --tables=10 --table-size=100000 prepare

# Run stress test (150 threads to trigger swap pressure)
kubectl exec -n kube-soomkiller deploy/sysbench -- \
  sysbench /usr/share/sysbench/oltp_read_write.lua \
  --mysql-host=mariadb --mysql-port=3306 \
  --mysql-user=root --mysql-password=testpass \
  --mysql-db=sbtest --tables=10 --table-size=100000 \
  --threads=150 --time=120 --report-interval=10 run

# Monitor soomkiller logs
kubectl logs -n kube-soomkiller daemonset/kube-soomkiller -f
```

When swap I/O exceeds 1000 pages/sec for 10 seconds, soomkiller will terminate the MariaDB pod gracefully.

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
│          │ swap_io_rate > threshold                         │
│          │ for sustained_duration                           │
│          ▼                                                  │
│   ┌─────────────────┐      ┌─────────────────────────────┐  │
│   │   Controller    │      │  Per-pod metrics (cgroup)   │  │
│   │   (DaemonSet)   │◀────▶│  - memory.swap.current      │  │
│   └────────┬────────┘      │  - memory.pressure (PSI)    │  │
│            │               └─────────────────────────────┘  │
│            │                                                │
│            │ Select victim:                                 │
│            │   where swap_usage > 0                         │
│            │   max by psi_full_avg10                        │
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
swap_io_rate > swap_io_threshold
  for duration > sustained_duration
```

When swap I/O exceeds the threshold for a sustained period, the node is under memory pressure and action is needed.

### 3. Pod Selection

```
candidate_pods = pods where (swap_usage > 0)
victim = max(candidate_pods, key=psi_full_avg10)
```

Select the pod with:
1. Non-zero swap usage (actively using swap)
2. Highest PSI `full` value (most severe memory stalls)

### 4. Graceful Termination

```bash
kubectl delete pod <victim>
```

Using `kubectl delete` because:
- Kubernetes handles SIGTERM → grace period → SIGKILL
- Proper cleanup (endpoint removal, finalizers)
- Respects pod's `terminationGracePeriodSeconds`
- Controller only needs K8s API access

### 5. Cooldown

After killing a pod, the controller waits for `cooldown-period` before taking further action. This:
- Gives the system time to stabilize
- Allows the killed pod's memory to be reclaimed
- Prevents cascading failures from killing too many pods

## Why This Works

### Traditional OOM Kill
```
Memory limit hit → SIGKILL → Immediate death
```

### Soft OOM Kill
```
Memory limit hit → Swap thrashing → Controller detects → kubectl delete → SIGTERM → Grace period → Clean shutdown
```

**Key insight:** Thrashing itself provides the buffer time. When a pod is swapping heavily, it's stalled on I/O - not progressing. This gives the controller time to detect the pressure and act before the system becomes unresponsive.

## Metrics Explained

### Swap I/O Rate

```bash
$ cat /proc/vmstat | grep -E '^psw'
pswpin 12345
pswpout 67890
```

- `pswpin`: Pages read from swap (cumulative)
- `pswpout`: Pages written to swap (cumulative)

Sample every second, calculate delta. High sustained rates indicate memory pressure.

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

### Tuning Parameters

| Scenario | swap-io-threshold | sustained-duration |
|----------|------------------|-------------------|
| Conservative | 500 pages/sec | 15s |
| Balanced | 1000 pages/sec | 10s |
| Aggressive | 2000 pages/sec | 5s |

Start conservative and tune based on your workload characteristics.

## Limitations

### Per-Pod Swap I/O Attribution

cgroup v2 does not expose per-cgroup `pswpin`/`pswpout` counters. We use:
- Node-level swap I/O for trigger
- Per-pod PSI + swap usage for victim selection

This means we detect node pressure, then infer the worst offender from cgroup metrics.

### PSI vs Swap I/O

PSI measures memory pressure broadly, not just swap I/O. A pod may have high PSI from:
- Page cache reclaim
- Direct reclaim
- Memory compaction

We mitigate this by requiring `swap_usage > 0` for victim selection.

### Single Point of Failure

The controller DaemonSet must be running. If it fails:
- System falls back to kernel OOM kill behavior
- No graceful termination

Recommendation: Set high priority class and resource requests to ensure controller survives pressure.

## Comparison with Alternatives

| Approach | Signal | Grace Period | Scope |
|----------|--------|--------------|-------|
| Kernel OOM Kill | SIGKILL | None | Per-container |
| Memory QoS (cgroups v2) | Throttle | N/A | Per-container |
| Kubelet Node Eviction | SIGTERM | Yes | Node-wide threshold |
| **Soft OOM Killer** | SIGTERM | Yes | Per-pod, swap-aware |

## Future Enhancements

1. **eBPF-based swap I/O tracking** - Per-pod swap I/O attribution
2. **Prometheus metrics export** - Integrate with existing monitoring
3. **PodDisruptionBudget awareness** - Optionally respect PDB
4. **Predictive termination** - Use swap growth rate to predict pressure

## References

- [Kubernetes NodeSwap Feature](https://kubernetes.io/docs/concepts/architecture/nodes/#swap-memory)
- [cgroups v2 Memory Controller](https://docs.kernel.org/admin-guide/cgroup-v2.html#memory)
- [PSI - Pressure Stall Information](https://docs.kernel.org/accounting/psi.html)
- [Kubernetes Issue #40157 - Make OOM not be SIGKILL](https://github.com/kubernetes/kubernetes/issues/40157)
