# SPDX-License-Identifier: AGPL-3.0-only
# Parental: pure Go (CGO off), static binary, no shell, non-root. No private modules and
# no local files: everything lives in Postgres, migrations are embedded in the binary.
ARG GO_IMAGE=docker.io/library/golang:1.26-bookworm
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS TARGETARCH
ENV GOFLAGS=-buildvcs=false
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/parental ./cmd/parental

FROM scratch
COPY --from=build /out/parental /parental
USER 10001:10001
ENTRYPOINT ["/parental"]
