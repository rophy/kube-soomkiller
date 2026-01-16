#!/bin/bash
set -e

# Image name
IMAGE_NAME="rophy/kube-soomkiller"

# Get tag from git describe
IMAGE_TAG=$(git describe --tags)

# Detect container runtime (prefer podman, fallback to docker)
if command -v podman &> /dev/null; then
    CONTAINER_RUNTIME="podman"
else
    CONTAINER_RUNTIME="docker"
fi

echo "Building ${IMAGE_NAME}:${IMAGE_TAG} using ${CONTAINER_RUNTIME}"

${CONTAINER_RUNTIME} build -t "${IMAGE_NAME}:${IMAGE_TAG}" .
