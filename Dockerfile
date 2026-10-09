# The base images come from Google's mirror of Docker Hub: Docker Hub's auth
# service failing or its pull limit refused builds that had changed nothing.
# No `# syntax=` line: the frontend BuildKit carries builds this file, and
# fetching another one is a Docker Hub pull of its own.

# Build stage
FROM mirror.gcr.io/library/golang:1.26.5-alpine AS builder

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
FROM mirror.gcr.io/library/alpine:latest

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
    ws \
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

# Copy the binary from builder
COPY --from=builder /build/pocket-relay-miner /usr/local/bin/pocket-relay-miner

# Create non-root user
RUN addgroup -g 1000 pocket && \
    adduser -D -u 1000 -G pocket pocket

# Create directories for keys, cache and the standalone mode's embedded store.
# A volume mounted on data/ starts owned by the image's user: one mounted on a
# path the image does not have would be root's, and the store could not write.
RUN mkdir -p /home/pocket/.pocket-relay-miner/keys \
             /home/pocket/.pocket-relay-miner/cache \
             /home/pocket/.pocket-relay-miner/data && \
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