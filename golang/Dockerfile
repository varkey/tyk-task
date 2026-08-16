# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.buildVersion=${VERSION}" \
    -o /out/tyk-sre-assignment .

# Distroless, non-root, no shell - minimal attack surface for a service that
# holds cluster-scoped RBAC.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/tyk-sre-assignment /usr/local/bin/tyk-sre-assignment

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/tyk-sre-assignment"]
