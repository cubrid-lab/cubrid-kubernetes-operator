#!/usr/bin/env bash
# Write-quarantine checks on an isolated Kind cluster, with real CUBRID Pods.
#
# It checks two behaviors of the write quarantine of ADR-0005, section 8, that
# the unit and envtest suites cannot show with real Pods, Services and SQL
# sessions:
#
#   child-write-denied (#949): while the Operator's NetworkPolicy writes are
#     refused by an admission policy on every pass, a member that outranks
#     the only active master stops answering the Operator. The Operator must
#     still observe the members, start the WriteQuarantinedMemberUnverified
#     quarantine and stop the read-write Brokers, and keep reporting the
#     refused write. An SQL session opened through the read-write Service
#     before the quarantine is recorded throughout.
#
#   node-read-forbidden (#948): the Operator runs namespace-scoped, bound by
#     a RoleBinding only, so every Node read is Forbidden. The same silent
#     higher-ranked member must still start the quarantine: a refused Node
#     read is never a fence.
#
# A member is made silent by dropping, inside its own Pod's network namespace,
# the packets it sends to the Operator's Pod: the replies of its Instance
# Manager API. Its CUBRID heartbeat and replication with the other members
# are untouched. The mechanism is the one of test/faults/netfault (an
# ephemeral container named netfault, image netfault.Image, NET_ADMIN and
# NET_RAW, iptables OUTPUT), cut to the Operator only.
#
# Kind is one node with all members on it: this shows the Operator's
# decisions with real Pods and SQL, not CUBRID's behavior under a VM fault or
# a real network partition.
#
# Usage (see docs/testing/kind-quarantine-checks.md):
#   KUBECONFIG=<kubeconfig of a Kind cluster> \
#   MANAGER_IMG=<loaded manager image> IM_IMG=<loaded Instance Manager image> \
#   EVIDENCE_DIR=<empty directory> test/kindquarantine/run.sh
#
# The script refuses a kubectl context that is not a Kind cluster's. It does
# not create or delete the Kind cluster.

set -uo pipefail

: "${KUBECONFIG:?set KUBECONFIG to the kubeconfig of the Kind cluster}"
: "${MANAGER_IMG:?set MANAGER_IMG to the manager image loaded on Kind}"
: "${IM_IMG:?set IM_IMG to the Instance Manager image loaded on Kind}"
: "${EVIDENCE_DIR:?set EVIDENCE_DIR to an empty directory for the evidence}"
export KUBECONFIG

CHECKS=${CHECKS:-"child-write-denied node-read-forbidden"}
NS=${NS:-qc-ha}
STORE_NS=${STORE_NS:-qc-store}
CLUSTER=qc
DATABASE=appdb
OP_NS=cubrid-kubernetes-operator-system
OP_DEPLOY=cubrid-kubernetes-operator-controller-manager
OP_SA=cubrid-kubernetes-operator-controller-manager
OP_USER="system:serviceaccount:${OP_NS}:${OP_SA}"
# The same image and digest as test/faults/netfault (netfault.Image).
NETFAULT_IMAGE="docker.io/nicolaka/netshoot@sha256:7f08c4aff13ff61a35d30e30c5c1ea8396eac6ab4ce19fd02d5a4b3b5d0d09a2"
SILENT=${CLUSTER}-0
QUARANTINE_LIMIT=${QUARANTINE_LIMIT:-180}
RELEASE_LIMIT=${RELEASE_LIMIT:-600}
FORMATION_LIMIT=${FORMATION_LIMIT:-1500}

REPO=$(cd "$(dirname "$0")/../.." && pwd)
mkdir -p "$EVIDENCE_DIR"
if [ -n "$(ls -A "$EVIDENCE_DIR")" ]; then
	echo "EVIDENCE_DIR $EVIDENCE_DIR is not empty" >&2
	exit 2
fi
LOG="$EVIDENCE_DIR/run.log"
RESULTS="$EVIDENCE_DIR/results.txt"

utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }
now() { date -u +%s; }
note() { echo "$(utc) $*" | tee -a "$LOG"; }
result() { echo "$(utc) $*" | tee -a "$RESULTS" "$LOG"; }
k() { kubectl --request-timeout=30s "$@"; }

# field prints a JSONPath of the CubridCluster, empty when it cannot be read.
field() { k -n "$NS" get cubridcluster "$CLUSTER" -o "jsonpath=$1" 2>/dev/null; }
condition() { field "{.status.conditions[?(@.type==\"$1\")].$2}"; }
rw_replicas() { k -n "$NS" get deployment "${CLUSTER}-broker-rw" -o 'jsonpath={.spec.replicas}/{.status.replicas}' 2>/dev/null; }
rw_endpoints() {
	k -n "$NS" get endpointslices -l "kubernetes.io/service-name=${CLUSTER}-rw" \
		-o 'jsonpath={range .items[*].endpoints[*]}{.addresses[0]}{" "}{end}' 2>/dev/null
}

# snapshot keeps what a reader needs to judge one moment of a check.
snapshot() {
	local dir="$EVIDENCE_DIR/$1"
	mkdir -p "$dir"
	k -n "$NS" get cubridcluster "$CLUSTER" -o yaml >"$dir/cubridcluster.yaml" 2>&1
	k -n "$NS" get pods,deployments,statefulsets,services,endpointslices,networkpolicies -o wide >"$dir/objects.txt" 2>&1
	k -n "$NS" get events --sort-by=.lastTimestamp >"$dir/events.txt" 2>&1
	for m in 0 1 2; do
		k -n "$NS" exec "${CLUSTER}-$m" -c cubrid -- bash -c 'PATH="${CUBRID}/bin:${PATH}" cubrid heartbeat status' \
			>"$dir/heartbeat-${CLUSTER}-$m.txt" 2>&1
	done
	k -n "$OP_NS" logs "deployment/$OP_DEPLOY" -c manager --tail=-1 >"$dir/manager.log" 2>&1
	echo "$(utc)" >"$dir/time.txt"
}

# wait_for polls a shell condition every 3 s up to a limit in seconds.
wait_for() {
	local what=$1 limit=$2 cond=$3 start
	start=$(now)
	while ! eval "$cond"; do
		if [ $(($(now) - start)) -ge "$limit" ]; then
			note "TIMEOUT after ${limit}s: $what"
			return 1
		fi
		sleep 3
	done
	note "reached after $(($(now) - start))s: $what"
}

preflight() {
	local ctx
	ctx=$(kubectl config current-context)
	case "$ctx" in
	kind-*) ;;
	*)
		echo "refusing context $ctx: not a Kind cluster" >&2
		exit 2
		;;
	esac
	# The context name alone can be renamed: every node must also be a Kind node.
	local providers
	providers=$(k get nodes -o 'jsonpath={range .items[*]}{.spec.providerID}{"\n"}{end}')
	if [ -z "$providers" ] || grep -qv '^kind://' <<<"$providers"; then
		echo "refusing context $ctx: a node's providerID does not start with kind://" >&2
		exit 2
	fi
	note "context $ctx, manager $MANAGER_IMG, Instance Manager $IM_IMG, source $(git -C "$REPO" rev-parse HEAD)"
	note "script sha256 $(sha256sum "$0" | cut -d' ' -f1); uncommitted changes: $(git -C "$REPO" status --porcelain | tr '\n' ' ')"
	k get nodes -o wide >"$EVIDENCE_DIR/nodes.txt"
}

# Set while a fault the script made is in place, for cleanup.
SILENCED=""
DENIED=""
SCOPED=""
WATCHER=""

stop_watcher() {
	[ -n "$WATCHER" ] && kill "$WATCHER" 2>/dev/null
	WATCHER=""
}

# cleanup runs on every exit: it removes what is still in place, so that a
# stopped or failed run does not leave the cluster silenced, refusing the
# Operator's writes or with a namespace-scoped manager.
cleanup() {
	trap - EXIT INT TERM
	stop_watcher
	[ -n "$SILENCED" ] && heal "$SILENCED"
	[ -n "$DENIED" ] && allow_networkpolicy_writes
	[ -n "$SCOPED" ] && unscope_operator
}

deploy_operator() {
	note "installing CRDs and the cluster-wide manager"
	(cd "$REPO" && make install && make deploy IMG="$MANAGER_IMG") >>"$LOG" 2>&1 || return 1
	k -n "$OP_NS" rollout status "deployment/$OP_DEPLOY" --timeout=5m >>"$LOG" 2>&1
}

create_cluster() {
	note "creating the object store, the namespaces and a three-member HA cluster"
	k create ns "$STORE_NS" >>"$LOG" 2>&1
	# As the E2E HA bootstrap does (test/e2e/ha_bootstrap_test.go).
	k apply -f - >>"$LOG" 2>&1 <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: s3mock, namespace: $STORE_NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: s3mock}}
  template:
    metadata: {labels: {app: s3mock}}
    spec:
      containers:
        - name: s3mock
          image: docker.io/adobe/s3mock:5.2.3
          imagePullPolicy: Never
          env: [{name: COM_ADOBE_TESTING_S3MOCK_STORE_INITIAL_BUCKETS, value: seed}]
          ports: [{containerPort: 9090}]
          readinessProbe: {tcpSocket: {port: 9090}, periodSeconds: 3}
---
apiVersion: v1
kind: Service
metadata: {name: s3mock, namespace: $STORE_NS}
spec:
  selector: {app: s3mock}
  ports: [{port: 9090, targetPort: 9090}]
EOF
	k -n "$STORE_NS" rollout status deployment/s3mock --timeout=5m >>"$LOG" 2>&1 || return 1
	# Privileged enforcement for the ephemeral netfault container, restricted
	# warn and audit, as the installed harness does for network faults.
	k create ns "$NS" >>"$LOG" 2>&1
	k label --overwrite ns "$NS" pod-security.kubernetes.io/enforce=privileged \
		pod-security.kubernetes.io/warn=restricted pod-security.kubernetes.io/audit=restricted >>"$LOG" 2>&1
	k -n "$NS" create secret generic object-storage --from-literal=accessKey=kind \
		--from-literal=secretKey=kind-quarantine-check >>"$LOG" 2>&1
	k apply -f - >>"$LOG" 2>&1 <<EOF
apiVersion: database.cubrid.io/v1alpha1
kind: CubridCluster
metadata: {name: $CLUSTER, namespace: $NS}
spec:
  version: "11.4"
  image: {repository: ${IM_IMG%:*}, tag: ${IM_IMG##*:}}
  databases: [{name: $DATABASE}]
  topology: {promotableMembers: 3, readReplicas: 0}
  highAvailability: {enabled: true}
  objectStorage:
    endpoint: s3mock.$STORE_NS.svc:9090
    insecure: true
    bucket: seed
    credentialsSecretRef: {name: object-storage}
  placement: {memberSpread: Preferred}
  storage: {data: {size: 2Gi}}
  networkPolicy:
    clients:
      - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: $STORE_NS}}
EOF
}

# settled: one resolved primary, the cluster Ready, no quarantine in force,
# and the read-write Brokers serving.
settled() {
	[ "$(condition Ready status)" = True ] && [ "$(condition PrimaryResolved status)" = True ] &&
		[ -z "$(field '{.status.writeQuarantine.reason}')" ] && [ -n "$(rw_endpoints)" ]
}

move_master() {
	local primary uid
	primary=$(field '{.status.currentPrimary}')
	if [ "$primary" != "$SILENT" ]; then
		note "the master is $primary, already off $SILENT"
		return 0
	fi
	uid=$(k -n "$NS" get pod "$SILENT" -o 'jsonpath={.metadata.uid}')
	note "moving the master off $SILENT: deleting its Pod $uid gracefully (preStop stops heartbeat; CUBRID promotes)"
	k -n "$NS" delete pod "$SILENT" --wait=false >>"$LOG" 2>&1
	wait_for "the master moved off $SILENT, which rejoined in a new Pod, and the cluster settled" 600 \
		"p=\$(field '{.status.currentPrimary}'); [ -n \"\$p\" ] && [ \"\$p\" != $SILENT ] && \
		 [ \"\$(k -n $NS get pod $SILENT -o 'jsonpath={.metadata.uid}' 2>/dev/null)\" != $uid ] && \
		 [ \"\$(k -n $NS get pod $SILENT -o 'jsonpath={.status.containerStatuses[?(@.name==\"cubrid\")].ready}' 2>/dev/null)\" = true ] && settled"
}

# operator_pod is the manager Pod that holds the leader-election Lease
# (LeaderElectionID in cmd/main.go): during a rollout the old Pod is still
# Running, and silencing it would leave the acting manager's answers intact.
operator_pod() {
	local holder
	holder=$(k -n "$OP_NS" get lease 91d4fe35.cubrid.io -o 'jsonpath={.spec.holderIdentity}')
	echo "${holder%%_*}"
}

# wait_for_new_leader waits until one manager Pod remains, it holds the Lease
# and has reconciled the cluster to a settled state; a quarantine the new
# manager starts on its first passes, before it has two answers from every
# member, is recorded and must end first.
wait_for_new_leader() {
	wait_for "one manager Pod left after the rollout" 300 \
		"[ \"\$(k -n $OP_NS get pods -l control-plane=controller-manager -o name | wc -l)\" = 1 ]" || return 1
	wait_for "the new manager holds the Lease" 300 \
		"[ \"\$(operator_pod)\" = \"\$(k -n $OP_NS get pods -l control-plane=controller-manager -o 'jsonpath={.items[0].metadata.name}')\" ]" ||
		return 1
	sleep 30
	wait_for "the new manager settled the cluster" 300 settled || return 1
	k -n "$OP_NS" logs "deployment/$OP_DEPLOY" -c manager --tail=-1 2>/dev/null |
		grep -E 'Quarantined the read-write path|Released the write quarantine' >"$EVIDENCE_DIR/$1-manager-start-quarantine.txt"
	result "$1: write-quarantine starts and releases logged by the new manager before the silence: $(grep -c . "$EVIDENCE_DIR/$1-manager-start-quarantine.txt")"
}

# attach_netfault adds the netfault ephemeral container to the silent member,
# as netfault.Attach does; it stays until the Pod is gone.
attach_netfault() {
	local have
	have=$(k -n "$NS" get pod "$SILENT" -o 'jsonpath={.spec.ephemeralContainers[?(@.name=="netfault")].name}')
	if [ -z "$have" ]; then
		k -n "$NS" patch pod "$SILENT" --subresource=ephemeralcontainers --type=strategic -p "$(
			cat <<EOF
{"spec":{"ephemeralContainers":[{"name":"netfault","image":"$NETFAULT_IMAGE","imagePullPolicy":"IfNotPresent",
"command":["tail","-f","/dev/null"],"securityContext":{"runAsUser":0,"runAsNonRoot":false,
"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"],"add":["NET_ADMIN","NET_RAW"]}}}]}}
EOF
		)" >>"$LOG" 2>&1 || return 1
	fi
	wait_for "the netfault container runs in $SILENT" 180 \
		"[ -n \"\$(k -n $NS get pod $SILENT -o 'jsonpath={.status.ephemeralContainerStatuses[?(@.name==\"netfault\")].state.running.startedAt}')\" ]"
}

# silence drops what the member sends to the Operator's Pod.
silence() {
	local op ip
	op=$(operator_pod)
	ip=$(k -n "$OP_NS" get pod "$op" -o 'jsonpath={.status.podIP}')
	attach_netfault || return 1
	note "silencing $SILENT towards the Operator Pod $op ($ip)"
	SILENCED=$1
	k -n "$NS" exec "$SILENT" -c netfault -- sh -c \
		"iptables -w 5 -N QC_SILENT 2>/dev/null; iptables -w 5 -F QC_SILENT && \
		 iptables -w 5 -A QC_SILENT -d $ip/32 -j DROP && \
		 { iptables -w 5 -C OUTPUT -j QC_SILENT 2>/dev/null || iptables -w 5 -I OUTPUT 1 -j QC_SILENT; } && \
		 iptables-save -c -t filter" >"$EVIDENCE_DIR/$1-silence-rules.txt" 2>&1
}

# heal removes the drop. It first saves the rules with their counters, and
# sets DROPPED to the packets the drop discarded: 0 means the member was never
# silent towards the acting manager, so its check did not take place.
heal() {
	note "removing the drop in $SILENT"
	k -n "$NS" exec "$SILENT" -c netfault -- sh -c \
		"iptables-save -c -t filter; while iptables -w 5 -D OUTPUT -j QC_SILENT 2>/dev/null; do :; done; \
		 iptables -w 5 -F QC_SILENT 2>/dev/null; iptables -w 5 -X QC_SILENT 2>/dev/null; true" \
		>"$EVIDENCE_DIR/$1-heal-rules.txt" 2>&1
	SILENCED=""
	DROPPED=$(sed -n 's/^\[\([0-9]*\):[0-9]*\] -A QC_SILENT -d .* -j DROP$/\1/p' "$EVIDENCE_DIR/$1-heal-rules.txt" | head -1)
	DROPPED=${DROPPED:-0}
	result "$1: packets dropped towards the manager while silenced: $DROPPED"
}

# quarantine_state prints one line for the record of each poll.
quarantine_state() {
	echo "$(utc) reason=$(field '{.status.writeQuarantine.reason}') master=$(field '{.status.writeQuarantine.observedMaster}')" \
		"members=$(field '{.status.writeQuarantine.members[*].name}') ready=$(condition Ready reason)" \
		"routing=$(condition RoutingReady reason) rw=$(rw_replicas) endpoints=[$(rw_endpoints)]"
}

# watch_quarantine records quarantine_state every 2 s in the background.
watch_quarantine() {
	(while :; do quarantine_state; sleep 2; done) >"$EVIDENCE_DIR/$1-timeline.txt" 2>&1 &
	WATCHER=$!
}

# expect_quarantine waits for the rule-2 quarantine after the silence and
# checks what enforces it.
expect_quarantine() {
	local check=$1 silenced=$2 ok=0 started
	# Only a quarantine that started at or after the silence counts: one from
	# before it, such as a restarted manager's first passes, shows nothing.
	if wait_for "$check: WriteQuarantinedMemberUnverified in status, started after the silence" "$QUARANTINE_LIMIT" \
		"[ \"\$(field '{.status.writeQuarantine.reason}')\" = WriteQuarantinedMemberUnverified ] && \
		 [ \"\$(date -u -d \"\$(field '{.status.writeQuarantine.startTime}')\" +%s)\" -ge $silenced ]"; then
		started=$(field '{.status.writeQuarantine.startTime}')
		result "$check: quarantine started at $started, $(($(date -u -d "$started" +%s) - silenced))s after the silence"
	else
		result "$check: FAIL no WriteQuarantinedMemberUnverified quarantine within ${QUARANTINE_LIMIT}s"
		return 1
	fi
	wait_for "$check: read-write Brokers at zero and no -rw endpoint" 120 \
		"case \"\$(rw_replicas)\" in 0/0 | 0/) [ -z \"\$(rw_endpoints)\" ] ;; *) false ;; esac" || ok=1
	local members master
	members=$(field '{.status.writeQuarantine.members[*].name}')
	master=$(field '{.status.writeQuarantine.observedMaster}')
	result "$check: status.writeQuarantine observedMaster=$master members=[$members] rw=$(rw_replicas) endpoints=[$(rw_endpoints)]"
	case " $members " in *" $SILENT "*) ;; *) ok=1 ;; esac
	[ "$ok" = 0 ] && result "$check: PASS quarantine enforced" || result "$check: FAIL quarantine not fully enforced"
	return "$ok"
}

expect_release() {
	local check=$1 extra=${2:-}
	wait_for "$check: quarantine released automatically after the heal" "$RELEASE_LIMIT" \
		"[ -z \"\$(field '{.status.writeQuarantine.reason}')\" ]" ||
		{
			result "$check: FAIL quarantine not released within ${RELEASE_LIMIT}s after the heal"
			return 1
		}
	result "$check: released, last release $(field '{.status.lastWriteQuarantineRelease.method}') at $(field '{.status.lastWriteQuarantineRelease.releaseTime}')"
	[ -n "$extra" ] && eval "$extra"
	return 0
}

# start_sql_client opens one session through -rw and writes a row a second
# on it; it also tries a fresh connection every 5 s once that session failed.
start_sql_client() {
	k apply -f - >>"$LOG" 2>&1 <<EOF
apiVersion: v1
kind: Pod
metadata: {name: sql-session, namespace: $STORE_NS}
spec:
  restartPolicy: Never
  containers:
    - name: client
      image: docker.io/library/python:3.12-slim
      imagePullPolicy: Never
      env:
        - {name: RW_HOST, value: "$CLUSTER-rw.$NS.svc"}
        - {name: DATABASE, value: "$DATABASE"}
        - {name: PIP_DISABLE_PIP_VERSION_CHECK, value: "1"}
      command: ["sh", "-c"]
      args:
        - |
          set -e
          pip install --quiet --no-cache-dir pycubrid==1.9.0
          exec python -u - <<'PY'
          import datetime, json, os, socket, time
          import pycubrid
          socket.setdefaulttimeout(10)
          # A table of this run's own, with a primary key so that it replicates.
          TABLE = "qc_session_%d" % int(time.time())
          def log(**kw):
              kw["t"] = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="milliseconds")
              print(json.dumps(kw), flush=True)
          def connect():
              return pycubrid.connect(host=os.environ["RW_HOST"], port=33000,
                                      database=os.environ["DATABASE"], user="dba", password="")
          c = connect(); cur = c.cursor()
          try:
              cur.execute("CREATE TABLE " + TABLE + " (id INT PRIMARY KEY, via VARCHAR(16), written VARCHAR(40))")
              c.commit()
          except Exception as exc:
              log(ev="create_table", err=repr(exc))
          c.close()
          session = connect()
          log(ev="session_open", table=TABLE)
          i, fresh, failed = 0, 1000000, False
          while not os.path.exists("/tmp/stop"):
              i += 1
              try:
                  cur = session.cursor()
                  cur.execute("INSERT INTO " + TABLE + " VALUES (%d, 'session', '%s')" % (i, time.time()))
                  session.commit()
                  log(ev="session_write", id=i, ok=True)
              except Exception as exc:
                  failed = True
                  log(ev="session_write", id=i, ok=False, err=repr(exc))
              if failed and i % 5 == 0:
                  fresh += 1
                  try:
                      f = connect(); fc = f.cursor()
                      fc.execute("INSERT INTO " + TABLE + " VALUES (%d, 'fresh', '%s')" % (fresh, time.time()))
                      f.commit(); f.close()
                      log(ev="fresh_write", id=fresh, ok=True)
                  except Exception as exc:
                      log(ev="fresh_write", id=fresh, ok=False, err=repr(exc))
              time.sleep(1)
          log(ev="stopped")
          PY
EOF
	wait_for "the SQL session wrote through -rw" 300 \
		"[ \"\$(k -n $STORE_NS logs sql-session 2>/dev/null | grep -c '\"ok\": true')\" -ge 5 ]"
}

# deny_networkpolicy_writes refuses every NetworkPolicy write of the
# Operator's ServiceAccount in the cluster's namespace, and changes the
# cluster's Broker clients so that every pass tries one.
deny_networkpolicy_writes() {
	note "denying the Operator's NetworkPolicy writes with an admission policy"
	k apply -f - >>"$LOG" 2>&1 <<EOF
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata: {name: qc-deny-operator-networkpolicy-writes}
spec:
  failurePolicy: Fail
  matchConstraints:
    resourceRules:
      - apiGroups: [networking.k8s.io]
        apiVersions: [v1]
        operations: [CREATE, UPDATE, DELETE]
        resources: [networkpolicies]
  matchConditions:
    - name: operator-only
      expression: request.userInfo.username == "$OP_USER"
  validations:
    - expression: "false"
      message: kind quarantine check denies the Operator's NetworkPolicy writes on purpose
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata: {name: qc-deny-operator-networkpolicy-writes}
spec:
  policyName: qc-deny-operator-networkpolicy-writes
  validationActions: [Deny]
  matchResources:
    namespaceSelector:
      matchLabels: {kubernetes.io/metadata.name: $NS}
EOF
	sleep 5
	DENIED=1
	k -n "$NS" patch cubridcluster "$CLUSTER" --type=json -p \
		'[{"op":"add","path":"/spec/networkPolicy/clients/-","value":{"namespaceSelector":{"matchLabels":{"qc-extra-client":"true"}}}}]' \
		>>"$LOG" 2>&1
}

allow_networkpolicy_writes() {
	note "removing the admission policy and the extra Broker client"
	k delete validatingadmissionpolicybinding,validatingadmissionpolicy qc-deny-operator-networkpolicy-writes \
		--ignore-not-found >>"$LOG" 2>&1
	if [ -n "$DENIED" ]; then
		k -n "$NS" patch cubridcluster "$CLUSTER" --type=json -p '[{"op":"remove","path":"/spec/networkPolicy/clients/1"}]' \
			>>"$LOG" 2>&1
	fi
	DENIED=""
}

# denials counts the refused NetworkPolicy writes. Each one is logged twice,
# as "Failed to write an object" and as controller-runtime's "Reconciler
# error"; only the first is counted, so the count is one per refused pass.
denials() {
	k -n "$OP_NS" logs "deployment/$OP_DEPLOY" -c manager --tail=-1 2>/dev/null |
		grep 'qc-deny-operator-networkpolicy-writes' | grep -c 'Failed to write an object'
}

# stop_child_write_denied undoes what child-write-denied set up when it stops
# early, so that the next check starts without its faults.
stop_child_write_denied() {
	stop_watcher
	[ -n "$SILENCED" ] && heal child-write-denied
	allow_networkpolicy_writes
	k -n "$STORE_NS" logs sql-session >"$EVIDENCE_DIR/child-write-denied/sql-session.jsonl" 2>&1
	k -n "$STORE_NS" delete pod sql-session --ignore-not-found --wait=false >>"$LOG" 2>&1
	wait_for "child-write-denied: cluster settled after stopping early" 600 settled
}

check_child_write_denied() {
	local c=child-write-denied silenced before after
	note "=== $c (#949)"
	mkdir -p "$EVIDENCE_DIR/$c"
	start_sql_client || {
		result "$c: BLOCKED the SQL session did not write before the fault"
		stop_child_write_denied
		return 1
	}
	deny_networkpolicy_writes
	wait_for "$c: NetworkPolicy writes refused in at least three passes" 180 \
		"[ \"\$(denials)\" -ge 3 ] && [ \"\$(condition Ready reason)\" = NetworkPolicyReconcileFailed ]" || {
		result "$c: BLOCKED the NetworkPolicy writes were not refused repeatedly"
		stop_child_write_denied
		return 1
	}
	result "$c: refused NetworkPolicy writes before the silence: $(denials); Ready reason $(condition Ready reason)"
	if [ -n "$(field '{.status.writeQuarantine.reason}')" ]; then
		result "$c: BLOCKED a quarantine was already in force before the silence"
		stop_child_write_denied
		return 1
	fi
	snapshot "$c/1-before-silence"
	watch_quarantine "$c"
	silence "$c" || {
		result "$c: BLOCKED could not silence $SILENT"
		stop_child_write_denied
		return 1
	}
	silenced=$(now)
	before=$(denials)
	local ok=0
	expect_quarantine "$c" "$silenced" || ok=1
	sleep 30
	after=$(denials)
	result "$c: refused NetworkPolicy writes: $before at the silence, $after 30s after the quarantine; Ready reason $(condition Ready reason); RoutingReady reason $(condition RoutingReady reason)"
	[ "$after" -gt "$before" ] && [ "$(condition Ready reason)" = NetworkPolicyReconcileFailed ] ||
		{
			result "$c: FAIL the refused write is no longer reported while the quarantine is in force"
			ok=1
		}
	sleep 30
	snapshot "$c/2-quarantined"
	k -n "$STORE_NS" logs sql-session >"$EVIDENCE_DIR/$c/sql-session-at-quarantine.jsonl" 2>&1
	heal "$c"
	expect_release "$c" \
		"result \"$c: read-write Brokers while the NetworkPolicy writes are still refused: \$(rw_replicas)\"" || ok=1
	snapshot "$c/3-released"
	allow_networkpolicy_writes
	wait_for "$c: cluster settled after the writes are allowed again" 600 settled || ok=1
	result "$c: after allowing the writes: Ready $(condition Ready reason), rw $(rw_replicas), endpoints [$(rw_endpoints)]"
	sleep 15
	k -n "$STORE_NS" exec sql-session -- touch /tmp/stop >>"$LOG" 2>&1
	sleep 5
	k -n "$STORE_NS" logs sql-session >"$EVIDENCE_DIR/$c/sql-session.jsonl" 2>&1
	stop_watcher
	snapshot "$c/4-settled"
	summarize_sql "$c"
	verdict "$c" "$ok"
}

# verdict prints a check's outcome. A drop that discarded no packet means the
# member never went silent towards the acting manager: the check did not take
# place, whatever the status showed.
verdict() {
	if [ "${DROPPED:-0}" = 0 ]; then
		result "$1: BLOCKED the drop discarded no packet, so $SILENT was never silent towards the acting manager"
		return 1
	fi
	[ "$2" = 0 ] && result "$1: PASS" || result "$1: FAIL"
	return "$2"
}

summarize_sql() {
	local f="$EVIDENCE_DIR/$1/sql-session.jsonl"
	result "$1: SQL session: last acknowledged session write $(grep '"session_write"' "$f" | grep '"ok": true' | tail -1 | sed 's/.*"t": "\([^"]*\)".*/\1/')," \
		"first failed session write: $(grep '"session_write"' "$f" | grep '"ok": false' | head -1)"
	result "$1: SQL session: session writes ok=$(grep '"session_write"' "$f" | grep -c '"ok": true')" \
		"failed=$(grep '"session_write"' "$f" | grep -c '"ok": false'); acknowledged after the first failure:" \
		"$(awk '/"session_write"/ && /"ok": false/ {f=1} f && /"session_write"/ && /"ok": true/ {n++} END {print n+0}' "$f")"
	result "$1: SQL session: fresh connections ok=$(grep '"fresh_write"' "$f" | grep -c '"ok": true')" \
		"failed=$(grep '"fresh_write"' "$f" | grep -c '"ok": false'); first fresh success: $(grep '"fresh_write"' "$f" | grep '"ok": true' | head -1)"
}

# scope_operator replaces the manager's ClusterRoleBinding with a RoleBinding
# in the cluster's namespace, and limits its watch to that namespace, as
# docs/operations/watch-namespace.md describes. Nodes are then unreadable.
scope_operator() {
	note "binding the manager to the cluster's namespace only"
	SCOPED=1
	k apply -f - >>"$LOG" 2>&1 <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: qc-manager, namespace: $NS}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cubrid-kubernetes-operator-manager-role}
subjects: [{kind: ServiceAccount, name: $OP_SA, namespace: $OP_NS}]
EOF
	k delete clusterrolebinding cubrid-kubernetes-operator-manager-rolebinding >>"$LOG" 2>&1
	k -n "$OP_NS" patch deployment "$OP_DEPLOY" --type=json -p \
		"[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--watch-namespace=$NS\"}]" >>"$LOG" 2>&1
	k -n "$OP_NS" rollout status "deployment/$OP_DEPLOY" --timeout=5m >>"$LOG" 2>&1
	{
		echo "get nodes (cluster): $(k auth can-i get nodes --as="$OP_USER" 2>&1)"
		echo "get pods -n $NS: $(k auth can-i get pods -n "$NS" --as="$OP_USER" 2>&1)"
		echo "update deployments -n $NS: $(k auth can-i update deployments -n "$NS" --as="$OP_USER" 2>&1)"
		echo "list cubridclusters (all namespaces): $(k auth can-i list cubridclusters.database.cubrid.io -A --as="$OP_USER" 2>&1)"
		k get clusterrolebindings -o wide | grep -i cubrid-kubernetes-operator
		k -n "$OP_NS" get deployment "$OP_DEPLOY" -o 'jsonpath={.spec.template.spec.containers[0].args}'
		echo
	} >"$EVIDENCE_DIR/node-read-forbidden-authorization.txt" 2>&1
	note "authorization: $(head -1 "$EVIDENCE_DIR/node-read-forbidden-authorization.txt")"
}

node_read_refusals() {
	k -n "$OP_NS" logs "deployment/$OP_DEPLOY" -c manager --tail=-1 2>/dev/null |
		grep 'Fenced no member by an out-of-service taint' | grep -ci forbidden
}

# unscope_operator restores the cluster-wide manager that deploy_operator
# installed: make deploy brings back the ClusterRoleBinding and the
# Deployment's arguments without --watch-namespace.
unscope_operator() {
	note "restoring the cluster-wide manager"
	k -n "$NS" delete rolebinding qc-manager --ignore-not-found >>"$LOG" 2>&1
	(cd "$REPO" && make deploy IMG="$MANAGER_IMG") >>"$LOG" 2>&1
	k -n "$OP_NS" rollout status "deployment/$OP_DEPLOY" --timeout=5m >>"$LOG" 2>&1
	SCOPED=""
}

check_node_read_forbidden() {
	local c=node-read-forbidden silenced ok=0
	note "=== $c (#948)"
	scope_operator
	if [ "$(k auth can-i get nodes --as="$OP_USER" 2>/dev/null)" != no ]; then
		result "$c: BLOCKED the manager can still read Nodes"
		return 1
	fi
	mkdir -p "$EVIDENCE_DIR/$c"
	wait_for_new_leader "$c" || {
		result "$c: BLOCKED the cluster did not settle under the namespace-scoped manager"
		return 1
	}
	snapshot "$c/1-before-silence"
	watch_quarantine "$c"
	silence "$c" || {
		stop_watcher
		result "$c: BLOCKED could not silence $SILENT"
		return 1
	}
	silenced=$(now)
	expect_quarantine "$c" "$silenced" || ok=1
	wait_for "$c: a Forbidden Node read logged" 60 "[ \"\$(node_read_refusals)\" -ge 1 ]" || ok=1
	k -n "$OP_NS" logs "deployment/$OP_DEPLOY" -c manager --tail=-1 2>/dev/null |
		grep 'Fenced no member by an out-of-service taint' | head -3 >"$EVIDENCE_DIR/$c/node-read-refusals.txt"
	result "$c: Forbidden Node reads logged: $(node_read_refusals); first: $(head -c 400 "$EVIDENCE_DIR/$c/node-read-refusals.txt")"
	sleep 30
	result "$c: 30s later: $(quarantine_state)"
	[ "$(field '{.status.writeQuarantine.reason}')" = WriteQuarantinedMemberUnverified ] ||
		{
			result "$c: FAIL the quarantine did not stay in force while $SILENT is silent"
			ok=1
		}
	snapshot "$c/2-quarantined"
	heal "$c"
	expect_release "$c" || ok=1
	wait_for "$c: cluster settled after the release" 600 settled || ok=1
	stop_watcher
	snapshot "$c/3-settled"
	verdict "$c" "$ok"
}

main() {
	local rc=0
	preflight
	trap cleanup EXIT
	trap 'exit 130' INT TERM
	deploy_operator || {
		result "BLOCKED the manager could not be deployed"
		exit 1
	}
	create_cluster || {
		result "BLOCKED the cluster could not be created"
		exit 1
	}
	local formed
	formed=$(now)
	wait_for "HA cluster formed and settled" "$FORMATION_LIMIT" settled || {
		snapshot formation-failed
		result "BLOCKED the HA cluster did not settle"
		exit 1
	}
	result "HA cluster settled $(($(now) - formed))s after creation, primary $(field '{.status.currentPrimary}')"
	move_master || {
		snapshot master-move-failed
		result "BLOCKED the master did not move off $SILENT"
		exit 1
	}
	result "master moved: primary $(field '{.status.currentPrimary}'), $SILENT outranks it"
	snapshot 0-starting-state
	for c in $CHECKS; do
		case "$c" in
		child-write-denied) check_child_write_denied || rc=1 ;;
		node-read-forbidden) check_node_read_forbidden || rc=1 ;;
		*)
			result "unknown check $c"
			rc=1
			;;
		esac
	done
	note "done, results in $RESULTS"
	exit "$rc"
}

main
