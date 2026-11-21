#!/bin/bash
# On the runner: creates the namespace csql-read-check and a three-member HA
# cluster in it, as test/installed/ha.go does (restricted Pod Security, the
# managed-by label the lab's object store admits, a copy of the seed Secret's
# two keys, Required member spread, 640Mi/150m requests, 2Gi local-path data).
# The Instance Manager image is the Operator's default, as for cubrid-test/ha.
# Usage: setup.sh <run id>    (the seed prefix is lab-runs/<run id>)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
k() { "$here/k" "$@"; }
run=$1 ns=csql-read-check
uid=$(k get ns kube-system -o jsonpath='{.metadata.uid}')
[ "$uid" = ddc0876e-c844-47ab-82c5-0f11d3985707 ] || { echo "not the VM lab: kube-system UID $uid" >&2; exit 1; }
k apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $ns
  labels:
    app.kubernetes.io/managed-by: cubrid-installed-smoke
    app.kubernetes.io/instance: $run
    pod-security.kubernetes.io/enforce: restricted
EOF
# The two keys are piped from one kubectl to the other and never printed.
k -n cubrid-test get secret object-storage -o json |
  python3 -c 'import json,sys; s=json.load(sys.stdin); print(json.dumps({"apiVersion":"v1","kind":"Secret","type":"Opaque","metadata":{"name":"object-storage","namespace":"'$ns'"},"data":{k:s["data"][k] for k in ("accessKey","secretKey")}}))' |
  k apply -f - > /dev/null
k apply -f - <<EOF
apiVersion: database.cubrid.io/v1alpha1
kind: CubridCluster
metadata:
  name: csql-read-check
  namespace: $ns
spec:
  version: "11.4"
  databases:
  - name: appdb
  topology:
    promotableMembers: 3
    readReplicas: 0
  highAvailability:
    enabled: true
  objectStorage:
    endpoint: seaweedfs.object-store.svc:8333
    insecure: true
    bucket: cubrid-seed
    prefix: lab-runs/$run
    credentialsSecretRef:
      name: object-storage
  resources:
    requests:
      memory: 640Mi
      cpu: 150m
  placement:
    memberSpread: Required
  storage:
    data:
      size: 2Gi
      storageClassName: local-path
EOF
k get ns $ns -o jsonpath='{.metadata.uid}{"\n"}'
