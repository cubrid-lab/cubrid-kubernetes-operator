# Derivative CUBRID image with the Instance Manager (ADR-0003).
# Stage 1 builds the manager binary; stage 2 layers it onto the official
# CUBRID image so the CUBRID CLIs, HA config templates, and helper scripts are
# all present (validated in docs/poc/RESULTS.md).

ARG GO_IMAGE=golang:1.26
ARG CUBRID_IMAGE=cubrid/cubrid:11.4

# The official CUBRID image exists for linux/amd64 only. The manager is built
# on the build host's own platform and cross-compiled, so an arm64 host does
# not put an arm64 binary on the amd64 image, and does not emulate the compiler.
FROM --platform=${BUILDPLATFORM} ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY api/ api/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/instance-manager ./cmd/instance-manager

FROM --platform=linux/amd64 ${CUBRID_IMAGE}
COPY --from=build /out/instance-manager /usr/local/bin/instance-manager
COPY build/instance-manager/entrypoint.sh /usr/local/bin/im-entrypoint.sh
COPY build/instance-manager/broker-entrypoint.sh /usr/local/bin/broker-entrypoint.sh
# nss-myhostname must not answer for the node's own short name: it returns
# loopback, which breaks HA (docs/poc/RESULTS.md). A non-root entrypoint cannot
# edit this file, so it is fixed here.
RUN sed -i 's/^hosts:.*/hosts:      files dns/' /etc/nsswitch.conf \
    && mkdir -p /var/lib/cubrid \
    && chown cubrid:cubrid /var/lib/cubrid
# Run as the image's cubrid user (uid 1000), the user the operator's Pod
# securityContext requires. Numeric, so runAsNonRoot can be verified.
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/im-entrypoint.sh"]
