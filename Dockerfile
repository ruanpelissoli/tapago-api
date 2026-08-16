# Build stage: compile a static binary so the runtime image needs no toolchain.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Dependencies are copied first so `go mod download` stays cached across
# source-only changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 keeps the binary static: no libc to match in the final image.
# -trimpath strips local build paths out of the binary.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# Runtime stage: alpine rather than distroless/scratch because compose's
# healthcheck needs a shell and wget to call /health, and ca-certificates are
# required to reach Google, Apple and Mercado Pago over TLS.
FROM alpine:3.22

RUN apk add --no-cache ca-certificates \
    && adduser -D -u 10001 api

COPY --from=build /out/api /usr/local/bin/api

USER api
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/api"]
