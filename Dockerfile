# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
      -o /out/gateway .

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="api-gateway" \
      org.opencontainers.image.description="Config-driven Go API gateway" \
      org.opencontainers.image.source="https://github.com/Mohdcode/api-Gateway" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /out/gateway /gateway

USER nonroot:nonroot

EXPOSE 8080 6060

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/gateway", "-healthcheck"]

ENTRYPOINT ["/gateway"]
