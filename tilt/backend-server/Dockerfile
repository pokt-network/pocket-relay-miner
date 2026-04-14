# syntax=docker/dockerfile:1

# Build stage
FROM golang:1.26.2-alpine AS builder

WORKDIR /build

# Install protoc and protoc-gen-go
RUN apk add --no-cache protobuf-dev git && \
    go install google.golang.org/protobuf/cmd/protoc-gen-go@latest && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# Copy go module files first
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source
COPY . .

# Generate protobuf code
RUN protoc --go_out=. --go_opt=paths=source_relative \
    --go-grpc_out=. --go-grpc_opt=paths=source_relative \
    pb/demo.proto

# Tidy dependencies after protobuf generation
RUN go mod tidy

# Build binary
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o demo-backend .

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app

COPY --from=builder /build/demo-backend .
COPY config.yaml .

EXPOSE 8545 50051 9095

ENTRYPOINT ["./demo-backend"]
