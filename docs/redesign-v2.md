# Soomkiller v2 Redesign

## Motivation

The current design detects **node-level swap thrashing** (sustained high swap I/O). This has a fundamental limitation: pods can be OOMKilled before thrashing is detected.

The new design is based on a key insight: **any swap usage means the pod exceeded its memory limit and would have been OOMKilled without swap**.

## Design Philosophy

| Aspect | v1 (Current) | v2 (New) |
|--------|--------------|----------|
| Goal | Detect thrashing, kill offending pod | Convert SIGKILL to SIGTERM |
| Trigger | Node swap I/O > threshold for duration | Any pod swap > threshold |
| Assumption | Thrashing is bad, brief swap is OK | Any swap = over budget |

## New Algorithm

```
Every poll-interval (1s):
  1. Scan pod cgroups on this node
  2. For each burstable pod:
     - Read memory.swap.current
     - Read memory.max
     - Calculate: swap_ratio = swap.current / memory.max
  3. Kill all pods where swap_ratio > swap-threshold-percent
```

**Note:** Earlier designs considered using `/proc/vmstat` swap I/O rate as a trigger
(only scan when swap I/O > 0). This was removed because:
- Swap usage (`memory.swap.current > 0`) is the actual signal we care about
- Checking swap I/O rate adds complexity without benefit
- Pods can have swap allocated without active I/O (pages already swapped)
- The cgroup scan is lightweight (filesystem reads only, no API calls)

## Parameters

### Removed

| Parameter | Reason |
|-----------|--------|
| `--swap-io-threshold` | Per-pod swap usage is the trigger, not node-level I/O rate |
| `--sustained-duration` | No longer waiting for sustained pressure |
| `--cooldown-period` | Each pod evaluated independently |

### Added

| Parameter | Default | Description |
|-----------|---------|-------------|
| `--swap-threshold-percent` | 1 | Kill pods with swap.current/memory.max > this % |

### Unchanged

| Parameter | Default | Description |
|-----------|---------|-------------|
| `--poll-interval` | 1s | How often to check /proc/vmstat |
| `--protected-namespaces` | kube-system | Namespaces to never kill pods from |
| `--dry-run` | true | Log actions without executing |
| `--node-name` | $NODE_NAME | Node to monitor |
| `--cgroup-root` | /sys/fs/cgroup | Cgroup v2 root path |
| `--metrics-addr` | :8080 | Prometheus metrics address |

## Implementation Guide

### 1. Update main.go

```go
// Remove these flags:
// - swap-io-threshold
// - sustained-duration
// - cooldown-period

// Add new flag:
flag.Float64Var(&swapThresholdPercent, "swap-threshold-percent", 1.0,
    "Kill pods with swap usage > this % of memory limit")

// Update Config struct passed to controller
```

### 2. Update controller/controller.go

#### 2.1 Update Config struct

```go
type Config struct {
    NodeName              string
    PollInterval          time.Duration
    SwapThresholdPercent  float64       // NEW: % of memory.max
    DryRun                bool
    ProtectedNamespaces   []string
    K8sClient             kubernetes.Interface
    Metrics               *metrics.Collector
}
```

#### 2.2 Simplify Controller struct

```go
type Controller struct {
    config Config

    // No state tracking needed - each scan is independent
    // Remove:
    // - lastSwapIO, lastSampleTime (no swap I/O rate tracking)
    // - thresholdExceeded time.Time
    // - lastKillTime time.Time
    // - lastRateAbove bool
    // - etc.

    protectedNamespaces map[string]bool
}
```

#### 2.3 Update PodCandidate struct

```go
type PodCandidate struct {
    Namespace     string
    Name          string
    CgroupPath    string
    SwapBytes     int64
    MemoryMax     int64   // NEW
    SwapPercent   float64 // NEW: SwapBytes / MemoryMax * 100
}
```

#### 2.4 Simplify reconcile()

```go
func (c *Controller) reconcile(ctx context.Context) error {
    // Scan cgroups and kill pods over threshold
    // No swap I/O rate check - we scan every interval
    // (cgroup scan is lightweight: filesystem reads only, no API calls)
    return c.findAndKillOverThreshold(ctx)
}
```

#### 2.5 New findAndKillOverThreshold()

```go
func (c *Controller) findAndKillOverThreshold(ctx context.Context) error {
    candidates, err := c.findCandidates(ctx)
    if err != nil {
        return err
    }

    var killed int
    for _, cand := range candidates {
        if cand.SwapPercent > c.config.SwapThresholdPercent {
            klog.Warningf("Pod %s/%s over threshold: swap=%.1f%% (%.1fMB / %.1fMB limit)",
                cand.Namespace, cand.Name, cand.SwapPercent,
                float64(cand.SwapBytes)/1024/1024,
                float64(cand.MemoryMax)/1024/1024)

            if err := c.terminatePod(ctx, cand); err != nil {
                klog.Errorf("Failed to kill pod %s/%s: %v",
                    cand.Namespace, cand.Name, err)
                continue
            }
            killed++
        }
    }

    if killed > 0 {
        klog.Infof("Killed %d pods over swap threshold", killed)
        metrics.PodsKilledTotal.Add(float64(killed))
    }

    return nil
}
```

#### 2.6 Update findCandidates()

```go
func (c *Controller) findCandidates(ctx context.Context) ([]PodCandidate, error) {
    // ... existing pod listing and cgroup discovery logic ...

    for _, cgroupPath := range cgroupsResult.Cgroups {
        // ... existing container ID extraction ...

        podMetrics, err := c.config.Metrics.GetPodMetrics(cgroupPath)
        if err != nil {
            klog.V(2).Infof("Failed to get metrics for %s: %v", cgroupPath, err)
            continue
        }

        // Skip if not using swap
        if podMetrics.SwapCurrent == 0 {
            continue
        }

        // Calculate swap percentage
        var swapPercent float64
        if podMetrics.MemoryMax > 0 {
            swapPercent = float64(podMetrics.SwapCurrent) / float64(podMetrics.MemoryMax) * 100
        }

        podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
        if existing, ok := processedPods[podKey]; ok {
            existing.SwapBytes += podMetrics.SwapCurrent
            // Recalculate percent with aggregated swap
            if existing.MemoryMax > 0 {
                existing.SwapPercent = float64(existing.SwapBytes) / float64(existing.MemoryMax) * 100
            }
        } else {
            processedPods[podKey] = &PodCandidate{
                Namespace:   pod.Namespace,
                Name:        pod.Name,
                CgroupPath:  cgroupPath,
                SwapBytes:   podMetrics.SwapCurrent,
                MemoryMax:   podMetrics.MemoryMax,
                SwapPercent: swapPercent,
            }
        }
    }

    // ... convert map to slice ...
}
```

#### 2.7 Remove selectVictim()

No longer needed - we kill all pods over threshold, not just one victim.

### 3. Update metrics/collector.go

#### 3.1 Update PodMetrics struct

```go
type PodMetrics struct {
    SwapCurrent int64
    MemoryMax   int64  // NEW
    PSI         PSIStats
}
```

#### 3.2 Update GetPodMetrics()

```go
func (c *Collector) GetPodMetrics(cgroupPath string) (*PodMetrics, error) {
    fullPath := filepath.Join(c.cgroupRoot, cgroupPath)

    // Read memory.swap.current
    swapCurrent, err := c.readCgroupInt64(filepath.Join(fullPath, "memory.swap.current"))
    if err != nil {
        return nil, fmt.Errorf("failed to read memory.swap.current: %w", err)
    }

    // Read memory.max (NEW)
    memoryMax, err := c.readCgroupInt64(filepath.Join(fullPath, "memory.max"))
    if err != nil {
        // memory.max might be "max" (unlimited), treat as MaxInt64
        memoryMax = math.MaxInt64
    }

    // Read PSI (optional, for observability)
    psi, _ := c.readPSI(filepath.Join(fullPath, "memory.pressure"))

    return &PodMetrics{
        SwapCurrent: swapCurrent,
        MemoryMax:   memoryMax,
        PSI:         psi,
    }, nil
}
```

### 4. Update Prometheus Metrics

#### 4.1 Remove

```go
// ConfigSwapIOThreshold
// ConfigSustainedDuration
// ConfigCooldownPeriod
// SwapIOThresholdExceeded
// SwapIOThresholdExceededDuration
// CooldownRemaining
```

#### 4.2 Add

```go
var (
    ConfigSwapThresholdPercent = promauto.NewGauge(prometheus.GaugeOpts{
        Name: "soomkiller_config_swap_threshold_percent",
        Help: "Configured swap threshold as % of memory limit",
    })

    PodSwapPercent = promauto.NewGaugeVec(prometheus.GaugeOpts{
        Name: "soomkiller_pod_swap_percent",
        Help: "Pod swap usage as % of memory limit",
    }, []string{"namespace", "pod"})

    PodMemoryMax = promauto.NewGaugeVec(prometheus.GaugeOpts{
        Name: "soomkiller_pod_memory_max_bytes",
        Help: "Pod memory.max (limit) in bytes",
    }, []string{"namespace", "pod"})
)
```

### 5. Update Tests

#### 5.1 Update e2e tests

- Remove tests for sustained-duration behavior
- Add tests for swap-threshold-percent
- Test that pods are killed when swap > threshold
- Test that pods survive when swap < threshold

#### 5.2 Test scenarios

| Scenario | Expected |
|----------|----------|
| Pod swap = 0 | No kill |
| Pod swap = 0.5% (threshold = 1%) | No kill |
| Pod swap = 2% (threshold = 1%) | Kill |
| Multiple pods over threshold | Kill all |
| Protected namespace pod over threshold | No kill |

### 6. Documentation Updates

- Update README.md with new parameters
- Update helm chart values.yaml
- Add migration guide for v1 → v2

## Migration Guide

### Breaking Changes

Users upgrading from v1 must update their configuration:

```yaml
# v1 (old)
args:
  - --swap-io-threshold=1000
  - --sustained-duration=10s
  - --cooldown-period=30s

# v2 (new)
args:
  - --swap-threshold-percent=1
```

### Behavioral Changes

| v1 | v2 |
|----|-----|
| Waits for sustained thrashing | Acts immediately on swap usage |
| Kills one pod at a time | Kills all pods over threshold |
| Requires PSI > 0 | Ignores PSI |
| May miss fast OOMKills | Catches any swap usage |
