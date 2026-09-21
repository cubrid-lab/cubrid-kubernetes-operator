# Derivative CUBRID image with the Instance Manager (ADR-0003).
# Stage 1 builds the manager binary; stage 2 layers it onto the official
# CUBRID image so the CUBRID CLIs, HA config templates, and helper scripts are
# all present (validated in docs/poc/RESULTS.md).

ARG GO_IMAGE=golang:1.26
ARG CUBRID_IMAGE=cubrid/cubrid:11.4

FROM ${GO_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY api/ api/
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/instance-manager ./cmd/instance-manager

FROM ${CUBRID_IMAGE}
COPY --from=build /out/instance-manager /usr/local/bin/instance-manager
COPY build/instance-manager/entrypoint.sh /usr/local/bin/im-entrypoint.sh
# The official image runs as root to set ulimits/chown then drops to `cubrid`
# via gosu; the entrypoint keeps that model and starts the manager alongside.
ENTRYPOINT ["/usr/local/bin/im-entrypoint.sh"]
