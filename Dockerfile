# syntax=docker/dockerfile:1

# --- Build Stage ---
FROM golang:1.25-alpine AS builder

# Install system dependencies needed for building
RUN apk add --no-cache git ca-certificates

# Set the working directory inside the container
WORKDIR /app

# Copy dependency files first to leverage Docker layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the Go app as a highly optimized static binary
# -ldflags="-s -w" strips debug symbols for a smaller, faster binary
# CGO_ENABLED=0 ensures the binary is fully static and runs in minimal environments
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o qdrant-mcp-server main.go

# --- Runtime Stage ---
FROM alpine:3.19

# Add CA certificates for secure connections (essential for HTTPS calls to Qdrant Cloud or external APIs)
RUN apk add --no-cache ca-certificates tzdata

# Set the working directory
WORKDIR /app

# Copy the pre-compiled binary from the builder stage
COPY --from=builder /app/qdrant-mcp-server /app/qdrant-mcp-server

# Define the entrypoint to run the server
ENTRYPOINT ["/app/qdrant-mcp-server"]
