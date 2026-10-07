# Test-only image: the Instance Manager API over a scripted CUBRID CLI
# (test/fakeim). It contains no CUBRID and is never a release image.

ARG GO_IMAGE=golang:1.26
ARG BASE_IMAGE=debian:12-slim

FROM --platform=${BUILDPLATFORM} ${GO_IMAGE} AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY internal/ internal/
COPY test/fakeim/ test/fakeim/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -o /out/fakeim ./test/fakeim

# Not scratch: the DB Pod's preStop hook runs /bin/sh, and the init container
# of an HA member runs bash, getent and timeout.
FROM ${BASE_IMAGE}
COPY --from=build /out/fakeim /usr/local/bin/fakeim
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/fakeim"]
