# gh-workflow-exporter container image.
#
# ARG VERSION/COMMIT/DATE mirror the Makefile's -ldflags stamping (see
# internal/version) so a container-built binary reports the same version info
# `make build` would.
ARG VERSION=docker
ARG COMMIT=unknown
ARG DATE=

FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG VERSION
ARG COMMIT
ARG DATE
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X github.com/jedwards1230/gh-workflow-exporter/internal/version.Version=${VERSION} -X github.com/jedwards1230/gh-workflow-exporter/internal/version.Commit=${COMMIT} -X github.com/jedwards1230/gh-workflow-exporter/internal/version.Date=${DATE}" \
    -o /out/gh-workflow-exporter .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/gh-workflow-exporter /usr/local/bin/gh-workflow-exporter
EXPOSE 9877
USER nonroot
ENTRYPOINT ["/usr/local/bin/gh-workflow-exporter"]
