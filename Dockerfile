# syntax=docker/dockerfile:1.7
FROM golang:1.26.3-bookworm AS build

WORKDIR /src
ARG GOPROXY=https://proxy.golang.org,direct
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod GOPROXY="$GOPROXY" go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/judging-server ./cmd

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=build --chown=65532:65532 /out/judging-server /app/judging-server
COPY --chown=65532:65532 configs/config.yaml /app/configs/config.yaml
USER 65532:65532
ENTRYPOINT ["/app/judging-server"]
