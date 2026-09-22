# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /killswitch ./cmd/killswitch
FROM gcr.io/distroless/static
COPY --from=build /killswitch /killswitch
EXPOSE 8080
ENTRYPOINT ["/killswitch"]
