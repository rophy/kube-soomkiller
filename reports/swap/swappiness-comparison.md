# Swappiness Comparison: 0 vs 1 vs 60

**Date:** 2026-01-30
**Cluster:** K3s v1.31.6 on multipass VMs
**Target:** MariaDB with 512Mi memory limit

## Test Environment

- **Swap:** dm-crypt, 6GB
- **Workload:** sysbench oltp_read_write
- **Duration:** 2 minutes per run

## Thread Calibration

### swappiness=0 (worker-1)

| Threads | Runs | Swap Triggered | OOMKilled |
|---------|------|----------------|-----------|
| 133 | 10 | 0% (0/10) | 0% (0/10) |
| 134 | 10 | 0% (0/10) | 30% (3/10) |
| 140 | 5 | 0% (0/5) | 100% (5/5) |

### swappiness=1 (worker-1)

| Threads | Runs | Swap Triggered | OOMKilled |
|---------|------|----------------|-----------|
| 130 | 5 | 0% (0/5) | 0% (0/5) |
| 133 | 5 | 0% (0/5) | 0% (0/5) |
| 134 | 10 | 90% (9/10) | 0% (0/10) |
| 135 | 5 | 80% (4/5) | 0% (0/5) |
| 136 | 5 | 100% (5/5) | 0% (0/5) |
| 137 | 5 | 100% (5/5) | 0% (0/5) |

### swappiness=60 (worker-2)

| Threads | Runs | Swap Triggered | OOMKilled |
|---------|------|----------------|-----------|
| 133 | 5 | 0% (0/5) | 0% (0/5) |
| 137 | 4 | 100% (4/4) | 0% (0/4) |

## Detailed Results

### swappiness=0 (140 threads)

| Run | Peak Memory | Peak Swap | pswpout | Status |
|-----|-------------|-----------|---------|--------|
| 1 | 373MB | 0MB | 0/sec | OOMKilled |
| 2 | 510MB | 0MB | 0/sec | OOMKilled |
| 3 | 511MB | 0MB | 0/sec | OOMKilled |
| 4 | 509MB | 0MB | 0/sec | OOMKilled |
| 5 | 509MB | 0MB | 0/sec | OOMKilled |

**Result:** 0% swap, 100% OOMKilled

### swappiness=1 (137 threads)

| Run | Peak Memory | Peak Swap | pswpout | TPS | Status |
|-----|-------------|-----------|---------|-----|--------|
| 1 | 511MB | 4MB | 61/sec | 172.08 | Survived |
| 2 | 511MB | 35MB | 897/sec | 162.00 | Survived |
| 3 | 511MB | 9MB | 236/sec | 158.01 | Survived |
| 4 | 511MB | 27MB | 350/sec | 161.85 | Survived |
| 5 | 511MB | 7MB | 169/sec | 164.00 | Survived |

**Result:** 100% swap, 0% OOMKilled, Avg TPS: 163.59

### swappiness=60 (137 threads)

| Run | Peak Memory | Peak Swap | pswpout | TPS | Status |
|-----|-------------|-----------|---------|-----|--------|
| 1 | (failed) | - | - | - | - |
| 2 | 511MB | 15MB | 402/sec | 161.44 | Survived |
| 3 | 511MB | 11MB | 179/sec | 172.86 | Survived |
| 4 | 511MB | 34MB | 460/sec | 152.17 | Survived |
| 5 | 511MB | 5MB | 137/sec | 171.82 | Survived |

**Result:** 100% swap, 0% OOMKilled, Avg TPS: 164.57

## Summary

| Swappiness | Threads | Swap Triggered | OOMKilled | Avg TPS |
|------------|---------|----------------|-----------|---------|
| 0 | 134 | 0% | 30% | 163.64 |
| 0 | 140 | 0% | 100% | N/A |
| 1 | 134 | 90% | 0% | 162.22 |
| 1 | 137 | 100% | 0% | 163.59 |
| 60 | 137 | 100% | 0% | 164.57 |
