# Project Instructions for Claude

## kubectl Context

Use context: `k3s` (when K3s cluster is running via multipass)

## VM Management

This machine is shared. Before launching any VM (multipass, etc.), ALWAYS:

1. Check host memory capacity:
   ```bash
   free -h
   ```

2. Calculate total memory needed:
   - K3s server: 2GB minimum
   - K3s worker: 2GB minimum each
   - Host system overhead: keep at least 4GB free

3. If available memory is insufficient, **STOP and ask the user** before proceeding.
   - Do NOT automatically delete VMs - other users may be using them
   - Inform the user of current memory status and what is needed
   - Let the user decide how to proceed

## E2E Testing

### Prerequisites
- BATS installed (`apt install bats`)
- stern installed (`go install github.com/stern/stern@latest`)
- K3s cluster running with swap enabled
- kubectl context set to `k3s`

### Running Tests

Run all tests (with suite setup):
```bash
bats test/e2e/
```

Run a specific test file (skips suite setup):
```bash
bats test/e2e/core_functionality.bats
```

Run tests matching a pattern:
```bash
bats --filter "threshold" test/e2e/
```

### Test Setup

The `setup_suite.bash` file handles one-time setup automatically when running `bats test/e2e/`:
- Deploys kube-soomkiller via skaffold
- Waits for daemonset rollout
- Prepares sysbench database

### Manual Stress Testing

Follow the instructions in README.md for how to deploy and perform stress test.
