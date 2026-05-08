# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS builder
WORKDIR /src

# Cache module layer separately so source-only changes don't re-download deps.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/kuu-api ./cmd/server
RUN go build -trimpath -ldflags="-s -w" -o /out/kuu-migrate ./cmd/migrate

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/kuu-api /usr/local/bin/kuu-api
COPY --from=builder /out/kuu-migrate /usr/local/bin/kuu-migrate

USER nonroot:nonroot
EXPOSE 8081
ENTRYPOINT ["/usr/local/bin/kuu-api"]
