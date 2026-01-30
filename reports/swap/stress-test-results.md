# Swap Stress Test Results

**Date:** 2026-01-30
**Cluster:** K3s v1.31.6 on multipass VMs
**Target:** MariaDB with 512Mi memory limit

## Test Environment

### Node

| Node | Swap Type | Size |
|------|-----------|------|
| k3s-worker1 | dm-crypt | 6GB |

### Kernel Parameters

```
vm.swappiness: 60 (default)
vm.min_free_kbytes: 45056
vm.watermark_scale_factor: 10
```

### Workload

- **Tool:** sysbench oltp_read_write
- **Tables:** 4 × 500,000 rows
- **MariaDB limit:** 512Mi memory

## Thread Calibration Results

### Goal

Find the minimum thread count that reliably triggers swap usage.

### Results Summary

| Threads | Duration | Runs | Swap Triggered | Swap Rate |
|---------|----------|------|----------------|-----------|
| 130 | 5 min | 10 | 1/10 (10%) | 0-3 pages/sec |
| 135 | 2 min | 5 | 1/5 (20%) | 0-15 pages/sec |
| 140 | 2 min | 5 | 4/5 (80%) | 72-202 pages/sec |

### Detailed Results

#### 130 Threads (5 min × 10 runs)

| Run | Peak Memory | Peak Swap | pswpout | TPS |
|-----|-------------|-----------|---------|-----|
| 1 | 508MB | 0MB | 0/sec | 181.17 |
| 2 | (failed) | - | - | - |
| 3 | 511MB | 0MB | 0/sec | 184.61 |
| 4 | 509MB | 0MB | 0/sec | 189.99 |
| 5 | 505MB | 0MB | 0/sec | 191.18 |
| 6 | 510MB | 0MB | 0/sec | 188.52 |
| 7 | 506MB | 0MB | 0/sec | 190.65 |
| 8 | 510MB | 0MB | 0/sec | 185.09 |
| 9 | 509MB | 0MB | 0/sec | 123.26 |
| 10 | 506MB | 0MB | 3/sec | 120.88 |

**Conclusion:** Memory peaks at 505-511MB, just under the 512Mi limit. Swap rarely triggered.

#### 135 Threads (2 min × 5 runs)

| Run | Peak Memory | Peak Swap | pswpout | TPS |
|-----|-------------|-----------|---------|-----|
| 1 | 506MB | 0MB | 0/sec | 104.67 |
| 2 | 506MB | 0MB | 0/sec | 106.64 |
| 3 | 510MB | 0MB | 0/sec | 119.58 |
| 4 | 499MB | 0MB | 0/sec | 100.54 |
| 5 | 511MB | 0MB | 15/sec | 112.91 |

**Conclusion:** Still borderline. Swap activity only in 1 run.

#### 140 Threads (2 min × 5 runs)

| Run | Peak Memory | Peak Swap | pswpout | TPS |
|-----|-------------|-----------|---------|-----|
| 1 | 511MB | 7MB | 107/sec | 116.18 |
| 2 | 511MB | 7MB | 198/sec | 112.25 |
| 3 | 511MB | 10MB | 202/sec | 112.08 |
| 4 | 507MB | 0MB | 0/sec | 111.26 |
| 5 | 511MB | 2MB | 72/sec | 113.02 |

**Conclusion:** Swap reliably triggered (80% of runs). Peak swap 2-10MB, pswpout 72-202 pages/sec.

## Swap I/O Pattern Analysis

### Observation

When swap is triggered, the pattern is:

1. **Initial burst:** pswpout spikes (100-500+ pages/sec) for ~5-10 seconds
2. **Stabilization:** Swap usage stays constant, pswpout drops to near zero
3. **Occasional pswpin:** Low rate (1-10 pages/sec) as pages fault back in

### Implication for soomkiller

The current swap I/O detection approach sees:
- Brief spike at memory pressure onset
- Then quiet period (pages sitting in swap)
- Not sustained high I/O unless continuous memory churn

## Key Findings

1. **Thread threshold:** 140+ threads needed to reliably trigger swap with 512Mi MariaDB limit

2. **Swap pattern:** Brief burst then stable - not sustained high I/O

3. **Kernel parameters:** Default swappiness=60 means swap only at memory limit edge

4. **Node vs Pod metrics:**
   - Node-level PSI available via node-exporter
   - Per-pod PSI requires K8s 1.34+ (KubeletPSI feature gate)

## Recommendations

### For soomkiller testing

- Use **150+ threads** for reliable swap triggering
- Monitor both **pswpout** and **pswpin** (combined I/O)
- Consider **PSI metrics** (memory.pressure) when K8s 1.34 available

### For production

- Per-pod PSI metrics (K8s 1.34+) would be ideal detection signal
- Swap I/O rate may not sustain long enough for threshold-based detection
- Consider memory pressure duration, not just I/O rate
