# Build stage
FROM docker.io/golang:1.24-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum* ./

# Enable toolchain auto-download for newer Go versions
ENV GOTOOLCHAIN=auto
RUN go mod download

# Copy source code
COPY cmd/ cmd/
COPY internal/ internal/

# Build binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o kube-soomkiller ./cmd/kube-soomkiller

# Runtime stage
FROM docker.io/alpine:3.21

COPY --from=builder /app/kube-soomkiller /usr/local/bin/kube-soomkiller

ENTRYPOINT ["/usr/local/bin/kube-soomkiller"]
