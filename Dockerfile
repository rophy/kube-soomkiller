# Build stage
FROM golang:1.22-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum* ./
RUN go mod download

# Copy source code
COPY . .

# Build binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o kube-soomkiller ./cmd/kube-soomkiller

# Runtime stage
FROM alpine:3.19

RUN apk add --no-cache iproute2

COPY --from=builder /app/kube-soomkiller /usr/local/bin/kube-soomkiller

ENTRYPOINT ["/usr/local/bin/kube-soomkiller"]
