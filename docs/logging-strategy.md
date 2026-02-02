# Logging Strategy

This document outlines the logging strategy for kube-soomkiller, aligned with [Kubernetes logging guidelines](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md).

## Current State

The codebase uses unstructured klog functions (`klog.Infof`, `klog.Warningf`, `klog.Errorf`).

## Target State

Migrate to structured logging with `klog.InfoS` and `klog.ErrorS` for better observability and JSON output support.

## Verbosity Levels

| Level | Purpose | Examples |
|-------|---------|----------|
| **Info (V0)** | Always visible to operators | Startup config, pod deleted, controller stopped |
| **V(1)** | Reasonable default | Not currently used |
| **V(2)** | Recommended default for systems | Steady state info, system changes (found N pods over threshold) |
| **V(3)** | Extended information | Skip reasons, candidate details, filtering decisions |
| **V(4)** | Debug level | Per-cgroup scan details, per-scan status (swap I/O detected, no pods using swap) |
| **V(5)** | Trace level | Not currently used |

## Message Categories

### Startup/Shutdown (Info)
- Controller started
- Configuration values
- Pod informer started/synced

### Pod Lifecycle Events (Info)
- Pod deleted (past tense, with reason)

### Steady State Changes (V2)
- Found N pods over threshold (only when action will be taken)

### Filtering Decisions (V3)
- Skipped pod: already terminating
- Skipped pod: protected namespace
- Pod UID not found in cache
- Found N pods using swap, none over threshold

### Debug Details (V4)
- Per-cgroup scan: skipped cgroup (QoS not burstable)
- Per-scan status: swap I/O detected, no pods using swap
- Candidate details before threshold filtering

### Warnings
- Abnormalities that may need attention but aren't errors
- Could not extract pod UID from cgroup path
- Failed to get metrics for cgroup

### Errors
- Actionable problems requiring admin intervention
- Failed to delete pod

## Message Style Guidelines

Per Kubernetes conventions:

1. **Start with capital letter**: "Deleted pod" not "deleted pod"
2. **No ending punctuation**: "Deleted pod" not "Deleted pod."
3. **Use past tense**: "Deleted pod" not "Deleting pod"
4. **Specify object type**: "Deleted pod" not "Deleted"
5. **Active voice**: "Controller started" not "Controller was started"

## Structured Logging Format

### Before (unstructured)
```go
klog.Infof("Successfully deleted pod %s/%s", namespace, name)
```

### After (structured)
```go
klog.InfoS("Deleted pod", "pod", klog.KRef(namespace, name), "reason", "swap threshold exceeded")
```

### Key naming conventions
- Use camelCase for keys
- Use `klog.KRef()` for namespace/name pairs
- Use `klog.KObj()` for Kubernetes objects
- Common keys: `pod`, `namespace`, `node`, `err`, `reason`

## Output Formats

### Text (default)
```
I0202 12:34:56.789012   12345 controller.go:100] "Deleted pod" pod="default/my-pod" reason="swap threshold exceeded"
```

### JSON (--logging-format=json)
```json
{"ts":1706875696.789,"v":0,"msg":"Deleted pod","pod":{"namespace":"default","name":"my-pod"},"reason":"swap threshold exceeded"}
```

## Migration Plan

### Phase 1: Adjust verbosity levels (current)
- [x] V(4) for per-cgroup scan info
- [x] V(4) for per-scan status (swap I/O detected, no pods using swap)
- [x] V(3) for filtering decisions
- [x] V(2) for steady state changes (pods over threshold)
- [x] Warning for abnormalities

### Phase 2: Migrate to structured logging
- [ ] Replace `klog.Infof` with `klog.InfoS`
- [ ] Replace `klog.Errorf` with `klog.ErrorS`
- [ ] Replace `klog.Warningf` with `klog.InfoS` at appropriate level or keep as warning
- [ ] Use `klog.KRef()` and `klog.KObj()` for Kubernetes objects

### Phase 3: Message style cleanup
- [ ] Fix message capitalization
- [ ] Change to past tense
- [ ] Remove ending punctuation
- [ ] Ensure object types are specified

## Error Handling

Per Kubernetes guidelines:
- Don't log an error before returning an error
- Return wrapped errors using `fmt.Errorf()` with `%w`
- For debug error logging, use `klog.V(4).InfoS()` with `"err"` key

### Before
```go
if err != nil {
    klog.Errorf("Failed to delete pod: %v", err)
    return err
}
```

### After
```go
if err != nil {
    return fmt.Errorf("failed to delete pod %s/%s: %w", namespace, name, err)
}
```

Only log at the top level where the error is handled, not at each layer.
