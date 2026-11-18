#!/bin/bash
# login-shell-check.sh > out/config/login-shell-check.txt
# Local Docker, not the lab: shows which CUBRID_DATABASES a plain shell and a login shell see in
# the Instance Manager image when the Pod's value is set, the image's ~/.cubrid.sh, and the
# volumes the image declares.
I=ghcr.io/cubrid-lab/cubrid-kubernetes-operator/instance-manager@sha256:7a0e659dca4edfb2387d85bdb7171108c7f2ccc3f0bb3249c8849df0219d46fb
echo "# $(date -u +%Y-%m-%dT%H:%M:%SZ), local Docker $(docker version --format '{{.Server.Version}}' 2>/dev/null), image $I"
echo '$ docker run --rm -e CUBRID_DATABASES=/var/lib/cubrid/databases --entrypoint bash <image> -c '"'"'echo "plain: $CUBRID_DATABASES"; bash -lc "echo login: \$CUBRID_DATABASES"; cat ~/.cubrid.sh'"'"
docker run --rm -e CUBRID_DATABASES=/var/lib/cubrid/databases --entrypoint bash "$I" \
  -c 'echo "plain: $CUBRID_DATABASES"; bash -lc "echo login: \$CUBRID_DATABASES"; echo "--- ~/.cubrid.sh"; cat ~/.cubrid.sh' 2>&1
echo '$ docker image inspect <image> --format "{{json .Config.Volumes}}"'
docker image inspect "$I" --format '{{json .Config.Volumes}}' 2>&1
