# API Optimization for Soomkiller Scanning

## Current Implementation

The current scanning flow makes a Kubernetes API call on every scan:

```
reconcile() [every 1s]
  ├── GetSwapIOStats()          # Read /proc/vmstat (kernel memory)
  ├── if swapIORate == 0: return  # Skip scan if no swap activity
  └── findAndKillOverThreshold()
        └── findCandidates()
              ├── K8sClient.Pods().List()  # API CALL: list pods on this node
              ├── FindPodCgroups()          # Walk /sys/fs/cgroup (kernel memory)
              └── Match container IDs
```

**Problem**: The API call happens on every scan, even when no pods are over threshold (99% of cases).

**Current workaround**: Only scan when `swapIORate > 0`. But this causes the bug where pods using swap are missed if the initial burst is missed.

## Proposed Optimization

Restructure to scan cgroups first, only call API when needed:

```
reconcile() [every 1s]
  ├── GetSwapIOStats()          # Read /proc/vmstat (kernel memory)
  └── scanAndKillOverThreshold()
        ├── scanCgroupsForSwap()      # NO API CALL
        │     ├── FindPodCgroups()     # Walk /sys/fs/cgroup
        │     ├── Filter by QoS path   # Only burstable pods
        │     ├── Read swap metrics    # memory.swap.current
        │     └── Extract pod UIDs     # From cgroup path
        │
        ├── if no candidates: return   # Zero API calls in normal case
        │
        └── killOverThreshold(candidates)
              ├── K8sClient.Pods().List()  # API CALL: only when killing
              ├── Match UID to namespace/name
              └── Delete pods
```

## Cgroup Path Structure

The cgroup path contains QoS class and pod UID:

```
# Burstable pod
/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod<UID>.slice/cri-containerd-<CID>.scope

# BestEffort pod
/sys/fs/cgroup/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod<UID>.slice/cri-containerd-<CID>.scope

# Guaranteed pod
/sys/fs/cgroup/kubepods.slice/kubepods-pod<UID>.slice/cri-containerd-<CID>.scope
```

From the path we can extract:
- **QoS class**: `burstable`, `besteffort`, or guaranteed (no prefix)
- **Pod UID**: e.g., `b47ed05b_d1f1_4318_a7ea_f4c6015264b6` (replace `_` with `-`)
- **Container ID**: e.g., `1041b0e149965894e3458949fefc53179dfefef634657ff6e39827b3bdf46ef1`

## API Call Comparison

| Scenario | Current | Proposed |
|----------|---------|----------|
| No swap activity | 0 calls | 0 calls |
| Swap activity, no pods over threshold | 1 call/scan | 0 calls |
| Swap activity, pods over threshold | 1 call/scan | 1 call/scan |
| No swap activity, pods over threshold | 0 calls (BUG!) | 0 calls, then 1 call when killing |

## Implementation Steps

1. Add `extractPodUIDFromCgroup(path string) string` function
2. Add `extractQoSFromCgroup(path string) string` function
3. Modify `FindPodCgroups()` to return QoS and UID info
4. Create new `scanCgroupsForSwap()` that doesn't call API
5. Modify `findAndKillOverThreshold()` to only call API when candidates exist
6. Remove `swapIORate == 0` early return (no longer needed for performance)

## Protected Namespace Handling

Currently we filter by protected namespaces using the API response. Without API:

**Option A**: Call API only when killing, filter then
- Pro: Simple, no change to protected namespace logic
- Con: Might log "over threshold" for protected pods before filtering

**Option B**: Parse namespace from pod name convention (not reliable)
- Con: Pod names don't encode namespace

**Option C**: Accept that protected namespace filtering happens at kill time
- This is actually fine - we just won't kill protected pods

**Recommendation**: Option A - keep current protected namespace logic, just move it to kill phase.

## Metrics Consideration

Current code updates per-pod Prometheus metrics during scan. Without API call:
- We have pod UID but not namespace/name
- Metrics would need to use UID as label, or skip per-pod metrics during scan

**Recommendation**: Update per-pod metrics only when we call the API (when candidates exist). This is acceptable since metrics are most useful when pods are actually using significant swap.
