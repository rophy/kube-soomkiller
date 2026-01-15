#!/bin/bash
#
# Setup 3-node K3s cluster with Multipass and encrypted swap
#
# Creates:
#   - k3s-server: Control plane
#   - k3s-worker1: Worker node with 6GB encrypted swap
#   - k3s-worker2: Worker node with 6GB encrypted swap
#
# Usage:
#   ./setup-k3s-multipass.sh [up|down|status|kubeconfig|ssh]
#

set -e

# Configuration
SERVER_NAME="k3s-server"
WORKER_NAMES=("k3s-worker1" "k3s-worker2")
ALL_VMS=("$SERVER_NAME" "${WORKER_NAMES[@]}")

# VM specs
SERVER_CPUS=2
SERVER_MEMORY="2G"
SERVER_DISK="10G"

WORKER_CPUS=2
WORKER_MEMORY="3G"
WORKER_DISK="20G"

# Swap configuration
SWAP_SIZE_MB=6144

# K3s version (use specific K8s version)
K3S_VERSION="v1.31.6+k3s1"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

check_multipass() {
  if ! command -v multipass &> /dev/null; then
    log_error "multipass not installed. Install with: sudo snap install multipass"
    exit 1
  fi
}

wait_for_vm() {
  local vm_name="$1"
  local max_wait=60
  local waited=0
  while [ $waited -lt $max_wait ]; do
    if multipass info "$vm_name" 2>/dev/null | grep -q "State:.*Running"; then
      return 0
    fi
    sleep 2
    waited=$((waited + 2))
  done
  log_error "Timeout waiting for VM $vm_name"
  return 1
}

get_vm_ip() {
  local vm_name="$1"
  multipass info "$vm_name" --format json | jq -r ".info.\"$vm_name\".ipv4[0]"
}

# Install K3s server
install_k3s_server() {
  log_info "Installing K3s server on $SERVER_NAME (K3s $K3S_VERSION)..."
  multipass exec "$SERVER_NAME" -- bash -c "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='$K3S_VERSION' INSTALL_K3S_EXEC='--disable traefik' sh -"

  log_info "Waiting for K3s server to be ready..."
  multipass exec "$SERVER_NAME" -- bash -c '
    for i in {1..60}; do
      if sudo kubectl get nodes &>/dev/null; then
        echo "K3s server ready"
        exit 0
      fi
      sleep 2
    done
    echo "Timeout waiting for K3s"
    exit 1
  '
}

# Install K3s agent with swap support
install_k3s_agent() {
  local worker_name="$1"
  local server_ip="$2"
  local token="$3"
  local with_swap="${4:-false}"

  log_info "Installing K3s agent on $worker_name (swap=$with_swap)..."

  if [ "$with_swap" = "true" ]; then
    # K3s < 1.32 doesn't auto-read kubelet drop-in configs, so we use --kubelet-arg
    # to pass the config file directly
    multipass exec "$worker_name" -- bash -c "
      curl -sfL https://get.k3s.io | \
        INSTALL_K3S_VERSION='$K3S_VERSION' \
        K3S_URL=https://${server_ip}:6443 \
        K3S_TOKEN=${token} \
        INSTALL_K3S_EXEC='--kubelet-arg=config=/etc/rancher/k3s/kubelet-swap.yaml' \
        sh -
    "
  else
    multipass exec "$worker_name" -- bash -c "curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION='$K3S_VERSION' K3S_URL=https://${server_ip}:6443 K3S_TOKEN=${token} sh -"
  fi
}

# Configure encrypted swap on a node
configure_swap() {
  local vm_name="$1"
  local swap_size_mb="$2"

  log_info "Configuring encrypted swap on $vm_name (${swap_size_mb}MB)..."

  multipass exec "$vm_name" -- sudo bash -c "
    set -e

    # Create swap directory
    mkdir -p /var/swap

    # Create swap backing file
    echo 'Creating ${swap_size_mb}MB swap file...'
    dd if=/dev/zero of=/var/swap/swapfile bs=1M count=${swap_size_mb} status=progress
    chmod 600 /var/swap/swapfile

    # Configure encrypted swap with random key (AES-256)
    echo 'swap_crypt /var/swap/swapfile /dev/urandom swap,cipher=aes-xts-plain64,size=512' >> /etc/crypttab
    echo '/dev/mapper/swap_crypt none swap sw 0 0' >> /etc/fstab

    # Enable encrypted swap now
    cryptsetup open --type plain --cipher aes-xts-plain64 --key-size 512 --key-file /dev/urandom /var/swap/swapfile swap_crypt
    mkswap /dev/mapper/swap_crypt
    swapon /dev/mapper/swap_crypt

    # Verify swap
    echo 'Swap configured:'
    swapon --show
    free -h
  "
}

# Create kubelet config file for swap support (called BEFORE installing K3s agent)
prepare_kubelet_swap_config() {
  local vm_name="$1"

  log_info "Creating kubelet swap config on $vm_name..."

  multipass exec "$vm_name" -- sudo bash -c '
    set -e

    # Create config directory
    mkdir -p /etc/rancher/k3s

    # Create kubelet config file for swap support
    # Note: K3s < 1.32 does not auto-read drop-in configs, so we create a full config
    # and pass it via --kubelet-arg=config=/etc/rancher/k3s/kubelet-swap.yaml
    cat > /etc/rancher/k3s/kubelet-swap.yaml << CONFIG
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
failSwapOn: false
memorySwap:
  swapBehavior: LimitedSwap
CONFIG

    echo "Kubelet swap config created"
    cat /etc/rancher/k3s/kubelet-swap.yaml
  '
}

# Create cluster
cmd_up() {
  check_multipass
  log_info "Creating K3s cluster..."

  # Create server
  log_info "Creating server VM: $SERVER_NAME"
  if ! multipass info "$SERVER_NAME" &>/dev/null; then
    multipass launch --name "$SERVER_NAME" --cpus "$SERVER_CPUS" --memory "$SERVER_MEMORY" --disk "$SERVER_DISK" 22.04
  else
    log_warn "VM $SERVER_NAME already exists"
  fi
  wait_for_vm "$SERVER_NAME"

  # Create workers
  for worker in "${WORKER_NAMES[@]}"; do
    log_info "Creating worker VM: $worker"
    if ! multipass info "$worker" &>/dev/null; then
      multipass launch --name "$worker" --cpus "$WORKER_CPUS" --memory "$WORKER_MEMORY" --disk "$WORKER_DISK" 22.04
    else
      log_warn "VM $worker already exists"
    fi
    wait_for_vm "$worker"
  done

  # Install K3s server
  install_k3s_server

  # Get server IP and token
  SERVER_IP=$(get_vm_ip "$SERVER_NAME")
  TOKEN=$(multipass exec "$SERVER_NAME" -- sudo cat /var/lib/rancher/k3s/server/node-token)
  log_info "Server IP: $SERVER_IP"

  # Configure swap on workers BEFORE installing K3s agent
  # This ensures kubelet starts with proper swap configuration
  for worker in "${WORKER_NAMES[@]}"; do
    configure_swap "$worker" "$SWAP_SIZE_MB"
    prepare_kubelet_swap_config "$worker"
  done

  # Install K3s agents with swap support
  for worker in "${WORKER_NAMES[@]}"; do
    install_k3s_agent "$worker" "$SERVER_IP" "$TOKEN" "true"
  done

  # Wait for all nodes
  log_info "Waiting for all nodes to be ready..."
  multipass exec "$SERVER_NAME" -- bash -c '
    for i in {1..60}; do
      ready=$(sudo kubectl get nodes --no-headers 2>/dev/null | grep -c " Ready " || echo 0)
      if [ "$ready" -eq 3 ]; then
        echo "All nodes ready"
        exit 0
      fi
      echo "Waiting... ($ready/3 ready)"
      sleep 5
    done
    exit 1
  '

  # Label nodes
  log_info "Labeling nodes..."
  multipass exec "$SERVER_NAME" -- sudo kubectl label node "$SERVER_NAME" node-role.kubernetes.io/control-plane=true --overwrite
  for worker in "${WORKER_NAMES[@]}"; do
    multipass exec "$SERVER_NAME" -- sudo kubectl label node "$worker" node-role.kubernetes.io/worker=true --overwrite
  done

  log_info "Cluster ready with swap enabled on all workers!"
  echo ""
  cmd_status
  echo ""
  log_info "Get kubeconfig: $0 kubeconfig > ~/.kube/k3s-config"
  log_info "SSH to node:    $0 ssh <node-name>"
}

# Delete cluster
cmd_down() {
  check_multipass
  log_info "Deleting cluster..."
  for vm in "${ALL_VMS[@]}"; do
    if multipass info "$vm" &>/dev/null; then
      log_info "Deleting VM: $vm"
      multipass delete "$vm" --purge
    fi
  done
  log_info "Cluster deleted"
}

# Show status
cmd_status() {
  check_multipass

  echo "=== VMs ==="
  multipass list | grep -E "^(Name|k3s-)" || echo "No VMs"

  echo ""
  echo "=== Kubernetes Nodes ==="
  if multipass info "$SERVER_NAME" &>/dev/null; then
    multipass exec "$SERVER_NAME" -- sudo kubectl get nodes -o wide 2>/dev/null || echo "Cluster not ready"
  else
    echo "Server not running"
  fi
}

# Output kubeconfig
cmd_kubeconfig() {
  check_multipass
  if ! multipass info "$SERVER_NAME" &>/dev/null; then
    log_error "Server VM not running"
    exit 1
  fi
  SERVER_IP=$(get_vm_ip "$SERVER_NAME")
  multipass exec "$SERVER_NAME" -- sudo cat /etc/rancher/k3s/k3s.yaml | sed "s/127.0.0.1/${SERVER_IP}/g"
}

# SSH to VM
cmd_ssh() {
  local vm_name="${1:-$SERVER_NAME}"
  check_multipass
  if ! multipass info "$vm_name" &>/dev/null; then
    log_error "VM $vm_name not found"
    echo "Available VMs: ${ALL_VMS[*]}"
    exit 1
  fi
  multipass shell "$vm_name"
}

# Main
case "${1:-up}" in
  up)      cmd_up ;;
  down)    cmd_down ;;
  status)  cmd_status ;;
  kubeconfig) cmd_kubeconfig ;;
  ssh)     cmd_ssh "$2" ;;
  *)
    echo "Usage: $0 [up|down|status|kubeconfig|ssh <vm>]"
    exit 1
    ;;
esac
