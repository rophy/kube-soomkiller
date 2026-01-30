# Swappiness Comparison: 0 vs 1 vs 60

**Date:** 2026-01-30
**Cluster:** K3s v1.31.6 on multipass VMs
**Target:** MariaDB with 512Mi memory limit

## Test Environment

- **Swap:** dm-crypt, 6GB
- **Workload:** sysbench oltp_read_write
- **Duration:** 2 minutes per run
- **Runs:** 5 per configuration

## Results

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

**Result:** 100% swap, 0% OOMKilled

### swappiness=60 (137 threads)

| Run | Peak Memory | Peak Swap | pswpout | TPS | Status |
|-----|-------------|-----------|---------|-----|--------|
| 1 | (failed) | - | - | - | - |
| 2 | 511MB | 15MB | 402/sec | 161.44 | Survived |
| 3 | 511MB | 11MB | 179/sec | 172.86 | Survived |
| 4 | 511MB | 34MB | 460/sec | 152.17 | Survived |
| 5 | 511MB | 5MB | 137/sec | 171.82 | Survived |

**Result:** 100% swap, 0% OOMKilled

## Summary

| Swappiness | Swap Triggered | OOMKilled | Avg TPS |
|------------|----------------|-----------|---------|
| 0 | 0% | 100% | N/A |
| 1 | 100% | 0% | 163.59 |
| 60 | 100% | 0% | 164.57 |

## Conclusion

- **swappiness=0** prevents swap entirely, resulting in OOMKill
- **swappiness=1 and swappiness=60** show similar behavior at this workload - both trigger swap and prevent OOMKill
