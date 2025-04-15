# Build stage
FROM golang:1.23.8-alpine AS builder

WORKDIR /build

# Install necessary build tools
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 go build -o wsterm ./cmd/wsterm

# Final stage
FROM alpine:3.19

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/wsterm /app/

# Create non-root user
RUN adduser -D -H -h /app wsterm && \
    chown -R wsterm:wsterm /app

USER wsterm

ENTRYPOINT ["/app/wsterm"]
