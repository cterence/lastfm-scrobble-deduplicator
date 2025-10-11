FROM golang:1.24-bookworm@sha256:4ed690d6649d63c312b99a6120025ec79ce3b542968a37da53d6236c7c61a848 AS deps

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

FROM golang:1.24-bookworm@sha256:4ed690d6649d63c312b99a6120025ec79ce3b542968a37da53d6236c7c61a848 AS builder

WORKDIR /app
COPY --from=deps /go/pkg /go/pkg
COPY . .
ENV CGO_ENABLED=0
RUN go build -ldflags="-w -s" -o main .

FROM golang:1.24-bookworm@sha256:4ed690d6649d63c312b99a6120025ec79ce3b542968a37da53d6236c7c61a848 AS development

WORKDIR /app
RUN go install github.com/air-verse/air@latest

COPY go.mod go.sum ./
RUN go mod download

CMD ["air", "-c", ".air.toml"]

FROM debian:bookworm-slim@sha256:7e490910eea2861b9664577a96b54ce68ea3e02ce7f51d89cb0103a6f9c386e0 AS production

WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
  rm -rf /var/lib/apt/lists/* && \
  groupadd -r appuser && \
  useradd -r -g appuser appuser

COPY --from=builder /app/main .
RUN chown appuser:appuser /app/main

USER appuser

ENTRYPOINT ["/app/main"]
