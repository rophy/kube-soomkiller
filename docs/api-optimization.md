# API Optimization for Soomkiller Scanning

## Problem Statement

The original scanning flow made a Kubernetes API call on every scan (every 1s), even when no pods were over threshold (99% of cases).

## Solution: Node-Scoped Pod Informer

Instead of calling the API on every scan, we use a Kubernetes SharedIndexInformer that:
1. Watches only pods on this node (using `spec.nodeName` field selector)
2. Maintains a local cache updated via watch events
3. Provides O(1) lookup by pod UID using a custom indexer

### Scanning Flow

```
startup()
  └── Start PodInformer with node-scoped field selector
        └── Watch: spec.nodeName=<this-node>

reconcile() [every 1s]
  ├── GetSwapIOStats()              # Read /proc/vmstat
  └── findAndKillOverThreshold()
        ├── scanCgroupsForSwap()    # NO API CALL
        │     ├── FindPodCgroups()   # Walk /sys/fs/cgroup
        │     ├── Filter by QoS path # Only burstable pods
        │     ├── Read swap metrics  # memory.swap.current, memory.max
        │     ├── Calculate swap %   # Per container, take max
        │     └── Extract pod UIDs   # From cgroup path
        │
        ├── if no candidates: return # Zero API calls
        │
        └── Resolve and kill
              ├── PodInformer.GetPodByUID()  # Local cache lookup (O(1))
              ├── Skip if DeletionTimestamp set
              ├── Skip if protected namespace
              └── Delete pods over threshold
```

## Cgroup Path Structure

Container cgroups encode QoS class and pod UID:

```
# Burstable pod
/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<UID>.slice/cri-containerd-<CID>.scope

# BestEffort pod
/sys/fs/cgroup/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod<UID>.slice/cri-containerd-<CID>.scope

# Guaranteed pod (no QoS prefix - has memory limits, no swap)
/sys/fs/cgroup/kubepods.slice/kubepods-pod<UID>.slice/cri-containerd-<CID>.scope
```

We only scan burstable pods since:
- Guaranteed pods have memory limits and don't use swap
- BestEffort pods have no limits, so swap % is undefined

## Swap Percentage Calculation

Swap percentage is calculated per-container:
```
swapPercent = (memory.swap.current / memory.max) * 100
```

For pods with multiple containers, we take the **maximum** swap percentage across all containers. If any container exceeds the threshold, the pod is killed.

Note: Pause containers have `memory.max=max` (unlimited) and don't use swap, so they're effectively ignored.

## Informer Design

The PodInformer (`internal/controller/informer.go`) provides:

```go
type PodInformer struct {
    informer cache.SharedIndexInformer
    indexer  cache.Indexer
}

// Node-scoped: only watches pods on this node
func NewPodInformer(client kubernetes.Interface, nodeName string, resyncPeriod time.Duration) *PodInformer

// O(1) lookup by UID (used to match cgroup paths to pods)
func (p *PodInformer) GetPodByUID(uid string) *corev1.Pod
```

Key features:
- **Field selector**: `spec.nodeName=<node>` reduces watch scope
- **UID index**: Custom indexer for O(1) pod lookup from cgroup UID
- **Keeps terminating pods**: Controller checks `DeletionTimestamp` to skip

## API Call Comparison

| Scenario | Before | After |
|----------|--------|-------|
| No swap activity | 0 calls | 0 calls |
| Swap activity, no pods over threshold | 1 call/scan | 0 calls |
| Swap activity, pods over threshold | 1 call/scan | 0 calls (informer cache) |
| Initial startup | N/A | 1 List call (informer sync) |

## Log Levels

| Level | Messages |
|-------|----------|
| Info | Startup, pod kills, significant events |
| Warning | Errors, abnormalities, pod deletions |
| V(1) | Swap I/O detection |
| V(3) | Steady state ("no pods using swap", "skipping terminating pod") |

## Files Changed

- `internal/controller/informer.go` - New PodInformer implementation
- `internal/controller/controller.go` - Use informer instead of API calls
- `cmd/kube-soomkiller/main.go` - Initialize and start informer
- `deploy/soomkiller/rbac.yaml` - Add `watch` verb for pods
- `internal/metrics/collector.go` - Renamed to ContainerMetrics
- `internal/metrics/prometheus.go` - Removed SwapBytes/MemoryMax metrics
