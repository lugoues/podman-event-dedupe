# Cross-compiles on the build host, so multi-arch builds don't need emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /podman-event-dedupe ./cmd/podman-event-dedupe

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /podman-event-dedupe /podman-event-dedupe
ENTRYPOINT ["/podman-event-dedupe"]
