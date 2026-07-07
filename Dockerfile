# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/amd64/renovate-server ./cmd/renovate-server && \
    GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/arm64/renovate-server ./cmd/renovate-server

FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
COPY --from=build /out/${TARGETARCH}/renovate-server /renovate-server
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/renovate-server"]
CMD ["-config", "/etc/renovate-server/config.yaml"]
