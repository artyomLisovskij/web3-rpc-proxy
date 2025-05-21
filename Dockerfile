FROM golang:1.21-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy and download dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o web3-rpc-proxy ./cmd/main.go

# Final stage
FROM alpine:latest

WORKDIR /app

# Create non-root user
RUN addgroup -g 1000 appuser && \
    adduser -u 1000 -G appuser -s /bin/sh -D appuser

# Create logs directory with proper permissions
RUN mkdir -p /app/logs && \
    chown -R appuser:appuser /app && \
    chmod -R 755 /app

# Copy binary from builder
COPY --from=builder /app/web3-rpc-proxy .

# Switch to non-root user
USER appuser

# Command to run the executable
CMD ["./web3-rpc-proxy"]