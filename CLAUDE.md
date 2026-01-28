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

Follow the instructions in README.md for how to deploy and perform stress test.
