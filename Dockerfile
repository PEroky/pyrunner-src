# Build stage
FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY main.go index.html ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o pyrunner .

# Runtime stage
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    tini && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /build/pyrunner /app/pyrunner
RUN mkdir -p /data/scripts
ENV PYRUNNER_PORT=8000 \
    PYRUNNER_USER= \
    PYRUNNER_PASS= \
    PYRUNNER_DATA=/data
EXPOSE 8000
VOLUME /data
ENTRYPOINT ["tini", "--"]
CMD ["/app/pyrunner"]
