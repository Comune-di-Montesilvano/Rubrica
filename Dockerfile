# Multi-stage build for Rubrica
FROM golang:1.25-alpine AS builder

# Install build dependencies for CGO (required for SQLite)
RUN apk add --no-cache gcc musl-dev sqlite-dev

WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build binary with version injection
ARG VERSION=dev
RUN CGO_ENABLED=1 GOOS=linux go build \
    -ldflags="-X main.AppVersion=${VERSION} -s -w" \
    -o rubrica \
    ./cmd/server

# Runtime stage
FROM alpine:3.19

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata

# Create non-root user
RUN addgroup -g 1001 rubrica && \
    adduser -D -u 1001 -G rubrica rubrica

# Create data directory
RUN mkdir -p /data && chown rubrica:rubrica /data

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/rubrica .

# Copy web assets
COPY web/ ./web/

# Change ownership
RUN chown -R rubrica:rubrica /app

# Switch to non-root user
USER rubrica

# Volume for persistent data
VOLUME ["/data"]

# Expose port
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

# Run
CMD ["./rubrica"]
