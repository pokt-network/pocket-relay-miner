# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.24.3-alpine AS builder

# Install build dependencies
RUN apk add --no-cache \
    git \
    make \
    ca-certificates

WORKDIR /build

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build arguments for version information
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Build the binary with version information
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w \
    -X 'main.Version=${VERSION}' \
    -X 'main.Commit=${COMMIT}' \
    -X 'main.BuildDate=${BUILD_DATE}'" \
    -trimpath \
    -o pocket-relay-miner .

# Runtime stage
FROM alpine:latest

# TARGETARCH is automatically set by buildx (amd64, arm64, etc.)
ARG TARGETARCH

# Install runtime tools for debugging and testing
# Use --no-scripts to avoid QEMU emulation issues with package triggers
RUN apk add --no-cache --no-scripts \
    ca-certificates \
    curl \
    jq \
    yq \
    tini \
    && rm -rf /var/cache/apk/*

# Install grpcurl (not available in alpine repos)
# Map Docker TARGETARCH to grpcurl architecture naming
RUN GRPCURL_VERSION=1.9.1 && \
    case "${TARGETARCH}" in \
        amd64) GRPCURL_ARCH=x86_64 ;; \
        arm64) GRPCURL_ARCH=arm64 ;; \
        *) echo "Unsupported architecture: ${TARGETARCH}" && exit 1 ;; \
    esac && \
    wget -qO- "https://github.com/fullstorydev/grpcurl/releases/download/v${GRPCURL_VERSION}/grpcurl_${GRPCURL_VERSION}_linux_${GRPCURL_ARCH}.tar.gz" | \
    tar -xz -C /usr/local/bin grpcurl && \
    chmod +x /usr/local/bin/grpcurl

# Install websocat (websocket testing tool)
# Map Docker TARGETARCH to Rust target triple
RUN WEBSOCAT_VERSION=1.14.0 && \
    case "${TARGETARCH}" in \
        amd64) WEBSOCAT_ARCH=x86_64-unknown-linux-musl ;; \
        arm64) WEBSOCAT_ARCH=aarch64-unknown-linux-musl ;; \
        *) echo "Unsupported architecture: ${TARGETARCH}" && exit 1 ;; \
    esac && \
    wget -qO /usr/local/bin/websocat "https://github.com/vi/websocat/releases/download/v${WEBSOCAT_VERSION}/websocat.${WEBSOCAT_ARCH}" && \
    chmod +x /usr/local/bin/websocat

# Copy the binary from builder
COPY --from=builder /build/pocket-relay-miner /usr/local/bin/pocket-relay-miner

# Create non-root user
RUN addgroup -g 1000 pocket && \
    adduser -D -u 1000 -G pocket pocket

# Create directories for keys and cache
RUN mkdir -p /home/pocket/.pocket-relay-miner/keys \
             /home/pocket/.pocket-relay-miner/cache && \
    chown -R pocket:pocket /home/pocket/.pocket-relay-miner

USER pocket
WORKDIR /home/pocket

# Expose default ports
# 8080: HTTP relay endpoint
# 9090: gRPC relay endpoint
# 2112: Prometheus metrics
EXPOSE 8080 9090 2112

# Use tini as init system for proper signal handling
ENTRYPOINT ["/sbin/tini", "--", "pocket-relay-miner"]
CMD ["--help"]