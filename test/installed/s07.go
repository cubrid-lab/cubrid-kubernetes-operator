/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package installed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	// FaultPartitionMasterIsolated and FaultPartitionSlaveIsolated select
	// S07 after LAB01: a member is cut off, both ways, from the other members
	// and from the Operator's Pod by the network faults of
	// test/faults/netfault. The first cuts off the master, which ADR-0005
	// section 8 expects to quarantine the read-write path once another member
	// is an active master; the second cuts off a slave that ranks below the
	// master and is not the last resolved primary, which must not.
	// FaultPartitionHigherRankIsolated cuts off, the same way, a slave that
	// ranks above the master (a lower ordinal, earlier in ha_node_list),
	// which ADR-0005 section 8 expects to quarantine while the master stays
	// the one active master the Operator can reach. When the master is the
	// first member no slave outranks it, so the run first moves the master
	// off it by deleting its Pod and letting CUBRID fail over.
	FaultPartitionMasterIsolated     = "partition-master-isolated"
	FaultPartitionSlaveIsolated      = "partition-slave-isolated"
	FaultPartitionHigherRankIsolated = "partition-higher-rank-isolated"
	s07ID                            = "S07"
	s07LanePrefix                    = "lab-installed-ha-s07-"
	// The contract's variants the faults are recorded under.
	s07MasterIsolated     = "master-isolated"
	s07SlaveIsolated      = "slave-isolated"
	s07HigherRankIsolated = "higher-rank-isolated"
	// quarantineLimitName and partitionDurationName are the contract's
	// limits of S07.
	quarantineLimitName   = "quarantine_limit"
	partitionDurationName = "partition_duration"
	// s07OpenClient holds one application connection to the read-write
	// Service from before the fault and stops at its first error;
	// s07ProbePrefix starts the client ID of each write through a new
	// connection.
	s07OpenClient  = "s07open"
	s07ProbePrefix = "s07n"
	// The roles a sweep records for a member.
	s07ActiveMaster = "active master"
	s07Slave        = "slave"
	s07OtherRole    = "other"
	s07Unread       = "unread"
	// The triggers of ADR-0005 section 8, as the run reads them from the
	// members' own answers and the members the Operator cannot reach.
	s07TriggerMultipleMasters = "two or more active masters the Operator can reach"
	s07TriggerUnverified      = "one active master and an unreachable member that outranks it or is the last " +
		"resolved primary"
	// The read-write Brokers' access mode label value.
	s07ReadWrite = "rw"
	// readyCondition is the cluster's overall condition; haReadyCondition and
	// replicationHealthyCondition are the ones that cover replication.
	readyCondition              = "Ready"
	haReadyCondition            = "HAReady"
	replicationHealthyCondition = "ReplicationHealthy"
	routingReadyCondition       = "RoutingReady"
	// s07NoAnswer is the outcome of a new-connection write with no line in
	// the client's history.
	s07NoAnswer = "no answer"
	// s07UnknownPresent and s07UnknownAbsent resolve a write whose outcome
	// the client could not know by the rows the data check read: its row is
	// on some member, or on none.
	s07UnknownPresent = "unknown-present"
	s07UnknownAbsent  = "unknown-absent"
	// s07MemberPort is the member port the faults probe (cub_master);
	// s07OperatorPort the manager's health port.
	s07MemberPort   = 1523
	s07OperatorPort = 8081
)

// The run's own bounds on the steps of S07; they are not the contract's
// limits, which judge the result. Prepare adds the ephemeral container to
// each member, which may pull its image. Tests shorten them.
var (
	s07PrepareBound = 4 * time.Minute
	s07ConfirmSlack = 2 * time.Minute
	s07CleanupBound = 2 * time.Minute
	s07SettleBound  = 5 * time.Minute
	// s07EndRemoveBound bounds the removal of the drop rules when the run's
	// context ends: the lab launcher sends SIGKILL 2 seconds after the
	// SIGTERM of a lost Lease.
	s07EndRemoveBound = 10 * time.Second
	// s07ProbeSlack is how long after the run saw every read-write Broker
	// Pod gone and the -rw EndpointSlices empty a new connection may still
	// have been answered by what kube-proxy had not yet changed.
	s07ProbeSlack = 2 * time.Second
	// s07HealthWindow is how long, after the data check, the run reads
	// whether the cluster reports Ready with no quarantine.
	s07HealthWindow = 30 * time.Second
	// s07MoveBound bounds the move of the master off the first member before
	// higher-rank-isolated: the graceful stop of the deleted Pod, CUBRID's
	// failover, the new Pod's start and its return as a slave.
	s07MoveBound = 10 * time.Minute
)

// errS07MasterOnFirstMember is why higher-rank-isolated cannot pick a member
// to cut off yet: the master is the first member, so no slave outranks it.
var errS07MasterOnFirstMember = errors.New("no slave outranks the master")

// partitionFault reports whether the run's fault is an S07 partition.
func (c HAConfig) partitionFault() bool {
	return c.Fault == FaultPartitionMasterIsolated || c.Fault == FaultPartitionSlaveIsolated ||
		c.Fault == FaultPartitionHigherRankIsolated
}

// s07Variant is the contract's variant of an S07 fault.
func s07Variant(fault string) string {
	switch fault {
	case FaultPartitionSlaveIsolated:
		return s07SlaveIsolated
	case FaultPartitionHigherRankIsolated:
		return s07HigherRankIsolated
	}
	return s07MasterIsolated
}

// validateS07 refuses an S07 partition outside the VM lab, with a negative
// limit, without a hold, or with a hold shorter than partition_duration,
// which would fail by construction. A limit of zero is unset.
func (c HAConfig) validateS07() error {
	if !c.partitionFault() {
		return nil
	}
	if c.Level != evidence.LevelVMLab {
		return errors.New("S07 cuts a member off the network: it runs on the vm-lab level only")
	}
	if c.QuarantineLimit < 0 || c.PartitionDuration < 0 {
		return errors.New("S07 needs quarantine and partition limits that are not negative (0 is unset)")
	}
	if c.PartitionHold <= 0 || c.PartitionHold < c.PartitionDuration {
		return errors.New("S07 needs a positive partition hold, at least partition_duration")
	}
	if least := c.s07StepBounds(); c.Fault == FaultPartitionHigherRankIsolated && c.Timeout <= least {
		return fmt.Errorf("S07 higher-rank-isolated needs a -timeout longer than %s, the bounds of its own steps "+
			"(moving the master, preparing, holding, removing and settling the cut, and the data check), "+
			"plus the time LAB01 takes", least)
	}
	return nil
}

// s07StepBounds is the longest S07 higher-rank-isolated may take after LAB01
// by the bounds of its own steps.
func (c HAConfig) s07StepBounds() time.Duration {
	return s07MoveBound + s07PrepareBound + c.PartitionHold + s07ConfirmSlack + s07CleanupBound + s07SettleBound +
		c.DataCheckWithin
}

// s07Sweep is one reading of every member's role through kubectl exec,
// which reaches a member through its kubelet, not the Pod network, so the
// cut-off member is read too.
type s07Sweep struct {
	T     time.Time         `json:"t"`
	Roles map[string]string `json:"roles"`
	// Unreachable are the members cut off from the Operator when the sweep
	// was made.
	Unreachable []string `json:"unreachableFromOperator,omitempty"`
	// Trigger is the quarantine trigger the sweep shows; Complete is set when
	// every member the Operator can reach answered with a role.
	Trigger  string `json:"trigger,omitempty"`
	Complete bool   `json:"complete"`
}

// s07Revision is a reason or revision of a quarantine as the run first saw
// it in status.writeQuarantine.
type s07Revision struct {
	SeenAt       time.Time `json:"seenAt"`
	GenerationID string    `json:"generationID"`
	Reason       string    `json:"reason"`
	Revision     int64     `json:"revision"`
	Members      []string  `json:"members,omitempty"`
	MemberNames  []string  `json:"memberNames,omitempty"`
	Master       string    `json:"observedMaster,omitempty"`
	// WhileCut is set when the run first saw this revision while the member
	// was cut off.
	WhileCut bool `json:"seenWhileCut"`
}

// s07Condition is a condition of the cluster when the run saw it change.
type s07Condition struct {
	T      time.Time `json:"t"`
	Type   string    `json:"type"`
	Status string    `json:"status"`
	Reason string    `json:"reason,omitempty"`
}

// s07Path is the read-write path when the run saw it change.
type s07Path struct {
	T time.Time `json:"t"`
	// Desired is spec.replicas of the read-write Broker Deployment, -1 when
	// it could not be read.
	Desired int32 `json:"desiredReplicas"`
	// Pods counts the read-write Broker Pods, terminating ones included;
	// Endpoints the endpoints of the -rw EndpointSlices, ReadyEndpoints the
	// ready ones.
	Pods           int `json:"pods"`
	Endpoints      int `json:"endpoints"`
	ReadyEndpoints int `json:"readyEndpoints"`
}

// s07Probe is one write through a new connection to the read-write Service.
// Its times are on the run's clock, converted from the client's.
type s07Probe struct {
	Client     string    `json:"client"`
	StartedAt  time.Time `json:"startedAt"`
	AnsweredAt time.Time `json:"answeredAt"`
	Exit       int       `json:"exit"`
	Outcome    string    `json:"outcome"`
	Error      string    `json:"error,omitempty"`
	// Resolved is, for an unknown outcome, s07UnknownPresent or
	// s07UnknownAbsent once the data check read every member.
	Resolved string `json:"resolved,omitempty"`
}

// s07Session is what happened to the one connection s07OpenClient held from
// before the fault. It is reported, not judged: the contract sets no bound
// on it. Times are on the client's clock.
type s07Session struct {
	Acknowledged            int        `json:"acknowledged"`
	AcknowledgedAfterFault  int        `json:"acknowledgedAfterFault"`
	AcknowledgedAfterClosed int        `json:"acknowledgedAfterPathSeenClosed"`
	Failed                  int        `json:"failed"`
	Unknown                 int        `json:"unknown"`
	LastAcknowledgedAt      *time.Time `json:"lastAcknowledgedAt,omitempty"`
	EndedAt                 *time.Time `json:"endedAt,omitempty"`
	EndedBy                 string     `json:"endedBy,omitempty"`
	OpenAtEnd               bool       `json:"openAtEnd"`
	ReadError               string     `json:"readError,omitempty"`
	Diagnostics             string     `json:"diagnosticsFile,omitempty"`
}

// s07Data is the post-recovery data check on every member, read directly.
type s07Data struct {
	Read  bool   `json:"read"`
	Error string `json:"error,omitempty"`
	OK    bool   `json:"ok"`
	// AcknowledgedMissing counts acknowledged writes some member lacks;
	// MissingByMember counts them on each member, and Diverged the rows
	// that differ between members.
	AcknowledgedMissing int            `json:"acknowledgedMissing"`
	MissingByMember     map[string]int `json:"acknowledgedMissingByMember,omitempty"`
	Diverged            int            `json:"diverged"`
	// LaggingByMember counts, on each slave, the rows the final master holds
	// that the slave lacks: a slave behind or stalled, which the cluster
	// reports through HAReady and ReplicationHealthy. ConflictingByMember
	// counts, on each slave, the rows absent from the final master or held
	// with other values: histories that diverged. MasterMissingAcknowledged
	// counts the acknowledged writes the final master lacks.
	LaggingByMember           map[string]int `json:"laggingByMember,omitempty"`
	ConflictingByMember       map[string]int `json:"conflictingByMember,omitempty"`
	MasterMissingAcknowledged int            `json:"masterMissingAcknowledged"`
	File                      string         `json:"file,omitempty"`
}

// s07Record is the evidence of an S07 run besides the fault timeline.
type s07Record struct {
	Variant string `json:"variant"`
	// Control is set for the variant that must not quarantine.
	Control             bool          `json:"control"`
	Master              string        `json:"master"`
	LastResolvedPrimary string        `json:"lastResolvedPrimary"`
	Target              imageIdentity `json:"target"`
	TargetIP            string        `json:"targetIP"`
	// Operator is the manager Pod the target is cut off from, which the
	// faults never enter.
	Operator      string          `json:"operatorPod"`
	OperatorUID   string          `json:"operatorPodUID"`
	OperatorIP    string          `json:"operatorPodIP"`
	MembersBefore []imageIdentity `json:"membersBefore"`
	// MasterMove is the move of the master off the first member that
	// higher-rank-isolated made before it picked the member to cut off.
	MasterMove *s07MasterMove `json:"masterMove,omitempty"`
	// OperatorChangedAt is when the run first saw the manager Pod replaced,
	// gone or with another IP, and OperatorChange how; from then on no
	// trigger is counted.
	OperatorChangedAt *time.Time `json:"operatorChangedAt,omitempty"`
	OperatorChange    string     `json:"operatorChange,omitempty"`
	// ClusterReadsAfterGrace counts the readings of the cluster, and
	// OneMasterSweepsAfterGrace the complete sweeps with exactly one
	// reachable active master, made while the member was cut off and the
	// grace had passed: the control's evidence that it was observed.
	ClusterReadsAfterGrace    int `json:"clusterReadsAfterGrace"`
	OneMasterSweepsAfterGrace int `json:"oneMasterSweepsAfterGrace"`
	// StillRunning names the steps of the fault flow that did not return.
	StillRunning []string `json:"stepsStillRunning,omitempty"`
	// ForeignTargets are endpoints the faults would have entered that are
	// not the run's own members; any fails the result and nothing is cut.
	ForeignTargets []string           `json:"foreignTargetsRefused,omitempty"`
	ClockOffsets   map[string]float64 `json:"clockOffsetsSeconds,omitempty"`
	// The fault's times, on the run's clock: issued, confirmed (the first
	// observation after the confirmation), the end of the hold, and seen
	// removed. CleanupError says why it was not seen removed.
	FaultIssuedAt    *time.Time `json:"faultIssuedAt,omitempty"`
	FaultConfirmedAt *time.Time `json:"faultConfirmedAt,omitempty"`
	HoldEndedAt      *time.Time `json:"holdEndedAt,omitempty"`
	FaultRemovedAt   *time.Time `json:"faultRemovedAt,omitempty"`
	CleanupSucceeded bool       `json:"cleanupSucceeded"`
	CleanupError     string     `json:"cleanupError,omitempty"`
	NotHeld          string     `json:"notHeld,omitempty"`
	// RemovedOnEnd is set when the run's end took the drop rules away before
	// the fault's own removal; RemoveOnEndError says what was left.
	RemovedOnEnd     bool       `json:"dropRulesRemovedOnRunEnd,omitempty"`
	RemoveOnEndError string     `json:"dropRulesRemovalOnRunEndError,omitempty"`
	Sweeps           []s07Sweep `json:"sweeps"`
	// TriggerObservedAt is the first of two consecutive sweeps that showed a
	// trigger, TriggerWindowStart the sweep before it (or the fault's issue):
	// the trigger arose after it. IncompleteSweeps counts the sweeps while
	// the member was cut off that could not read every reachable member.
	TriggerObservedAt  *time.Time `json:"triggerObservedAt,omitempty"`
	TriggerWindowStart *time.Time `json:"triggerWindowStart,omitempty"`
	Trigger            string     `json:"trigger,omitempty"`
	IncompleteSweeps   int        `json:"incompleteSweeps"`
	// TargetClaimedAt is when the cut-off member first answered as an
	// active master, and TwoClaimsAt when two members first did: what CUBRID
	// did, which the Operator could see only in part.
	TargetClaimedAt *time.Time `json:"targetClaimedActiveMasterAt,omitempty"`
	TwoClaimsAt     *time.Time `json:"twoActiveMasterClaimsAt,omitempty"`
	// The quarantine as the run saw it: first seen, its start and enforced
	// times from status (the Operator's clock), and every reason and
	// revision.
	QuarantineSeenAt  *time.Time     `json:"quarantineSeenAt,omitempty"`
	QuarantineStart   *time.Time     `json:"quarantineStartTime,omitempty"`
	EnforcedTime      *time.Time     `json:"enforcedTime,omitempty"`
	EnforcedSeenAt    *time.Time     `json:"enforcedSeenAt,omitempty"`
	Generations       []string       `json:"generations,omitempty"`
	Revisions         []s07Revision  `json:"revisions,omitempty"`
	QuarantineInForce string         `json:"quarantineInForce,omitempty"`
	QuarantineAtEnd   string         `json:"quarantineAtEnd,omitempty"`
	ReleaseSeenAt     *time.Time     `json:"releaseSeenAt,omitempty"`
	ReleaseTime       *time.Time     `json:"releaseTime,omitempty"`
	ReleaseMethod     string         `json:"releaseMethod,omitempty"`
	ReleaseReason     string         `json:"releaseReason,omitempty"`
	ReleaseGeneration string         `json:"releaseGeneration,omitempty"`
	Conditions        []s07Condition `json:"conditions"`
	// The read-write path: each change, and when the run first saw the
	// Deployment at zero, no Broker Pod, no endpoint, and endpoints again.
	Path               []s07Path  `json:"readWritePath"`
	RWScaledDownAt     *time.Time `json:"rwScaledDownAt,omitempty"`
	RWPodsGoneAt       *time.Time `json:"rwPodsGoneAt,omitempty"`
	RWEndpointsEmptyAt *time.Time `json:"rwEndpointsEmptyAt,omitempty"`
	RWReopenedAt       *time.Time `json:"rwReopenedAt,omitempty"`
	// ResolvedWhileUncertain and ReopenedWhileQuarantined are never-events
	// seen while the run observed.
	ResolvedWhileUncertain   []string    `json:"resolvedWhileUncertain,omitempty"`
	ReopenedWhileQuarantined []string    `json:"reopenedWhileQuarantined,omitempty"`
	Probes                   []s07Probe  `json:"newConnectionProbes"`
	NewConnectionsStoppedAt  *time.Time  `json:"newConnectionsStoppedAt,omitempty"`
	Session                  *s07Session `json:"establishedSession,omitempty"`
	// SettledAt is when, after the removal, every member answered with one
	// master and two slaves and any quarantine left was one only a person
	// can release; SettleUnmet what did not hold at the settle bound.
	SettledAt    *time.Time      `json:"settledAt,omitempty"`
	SettleUnmet  string          `json:"settleUnmet,omitempty"`
	FinalMaster  string          `json:"finalMaster,omitempty"`
	FinalSlaves  []string        `json:"finalSlaves,omitempty"`
	MembersAfter []imageIdentity `json:"membersAfter"`
	Data         *s07Data        `json:"dataCheck,omitempty"`
	// HealthReadings counts the readings of the cluster in the health window
	// after the data check, and ReadyWithoutQuarantineAt is when one first
	// reported Ready with no quarantine; HealthyAtEnd is set then.
	HealthReadings           int        `json:"healthReadings"`
	ReadyWithoutQuarantineAt *time.Time `json:"readyWithoutQuarantineAt,omitempty"`
	// ReportedHealthyAt is when a reading in the health window first
	// reported HAReady and ReplicationHealthy True with no quarantine: the
	// cluster reported healthy, including its replication, which Ready does
	// not cover. HealthyAtEnd is set then.
	ReportedHealthyAt *time.Time `json:"reportedHealthyAt,omitempty"`
	HealthyAtEnd      bool       `json:"healthyAtEnd"`
	// StallReportedAt is when, after the cut was removed, the Operator first
	// reported replication stalled (HAReady or ReplicationHealthy False with
	// LogCopyStalled, ReplicationStalled or ApplyFailures), and StallReason
	// what it reported.
	StallReportedAt *time.Time         `json:"stallReportedAt,omitempty"`
	StallReason     string             `json:"stallReason,omitempty"`
	Observed        []faults.Step      `json:"observed"`
	ClientsStopped  bool               `json:"clientsStopped"`
	ClientsRead     bool               `json:"clientsRead"`
	ClientHistories []s04ClientHistory `json:"clientHistories,omitempty"`
	Partition       *netfault.Record   `json:"partition,omitempty"`
	// mu serializes the observations and the record's writes, which the run's
	// end can make while a step of the fault is still running; cut is the
	// state in cut.json.
	mu  sync.Mutex
	cut *s07CutFile
}

// s07MasterMove records how the run moved the master off the first member:
// the master and its Pod UID before, when the Pod was deleted, the new master
// and when every check held; Unmet is the last check that did not hold.
// Members are the member Pods once it settled; LAB01's stay in the record's
// membersBefore. Quarantine is the first write quarantine seen during the
// move, and LaterGenerations the generations of any seen after it.
type s07MasterMove struct {
	From             string             `json:"from"`
	FromUID          string             `json:"fromPodUID"`
	DeletedAt        *time.Time         `json:"podDeletedAt,omitempty"`
	To               string             `json:"to,omitempty"`
	SettledAt        *time.Time         `json:"settledAt,omitempty"`
	Unmet            string             `json:"unmet,omitempty"`
	Members          []imageIdentity    `json:"membersAfterMove,omitempty"`
	Quarantine       *s07MoveQuarantine `json:"quarantine,omitempty"`
	LaterGenerations []string           `json:"laterQuarantineGenerations,omitempty"`
}

// s07MoveQuarantine is a write quarantine seen while the master was moved:
// its reason and generation, when the run first saw it, and when the run
// first saw no quarantine after it, on the run's clock.
type s07MoveQuarantine struct {
	Reason       string     `json:"reason"`
	GenerationID string     `json:"generationID"`
	SeenAt       time.Time  `json:"firstSeenAt"`
	ReleasedAt   *time.Time `json:"releasedSeenAt,omitempty"`
}

func (rec *s07Record) note(kind, detail string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.Observed = append(rec.Observed, faults.Step{T: time.Now().UTC(), Kind: kind, Detail: detail})
}

func s07File(variant, name string) string { return s07ID + "/" + variant + "/" + name }

// memberOrdinal is the ordinal of a StatefulSet member's name, -1 when it
// has none. A lower ordinal comes first in ha_node_list and outranks a
// higher one (ADR-0005 section 8).
func memberOrdinal(member string) int {
	i := strings.LastIndex(member, "-")
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(member[i+1:])
	if err != nil {
		return -1
	}
	return n
}

// s07Trigger reads a sweep as ADR-0005 section 8 reads the Operator's
// observations: only the members the Operator can reach count as active
// masters, and a member it cannot reach is unverified. It returns the
// trigger, or "" for none, and whether every reachable member answered with
// a role. Two reachable active masters are a trigger whatever else was read;
// otherwise a sweep that is not complete decides nothing.
func s07Trigger(roles map[string]string, members, unreachable []string, lastResolved string) (string, bool) {
	var masters []string
	complete := true
	for _, m := range members {
		if slices.Contains(unreachable, m) {
			continue
		}
		switch roles[m] {
		case s07ActiveMaster:
			masters = append(masters, m)
		case s07Slave:
		default:
			complete = false
		}
	}
	if len(masters) >= 2 {
		return s07TriggerMultipleMasters, complete
	}
	if !complete || len(masters) != 1 {
		return "", complete
	}
	master := masters[0]
	for _, u := range unreachable {
		if u == master {
			continue
		}
		if u == lastResolved || memberOrdinal(u) >= 0 && memberOrdinal(u) < memberOrdinal(master) {
			return s07TriggerUnverified, true
		}
	}
	return "", true
}

// s07PickTarget picks the member to cut off: the master for
// master-isolated, which must be the last resolved primary; for
// slave-isolated a slave that ranks below the master and is not the last
// resolved primary, the intentional non-trigger; for higher-rank-isolated
// the highest-ranked slave that outranks the master, which must be the last
// resolved primary so that the rank alone makes the trigger. An error says
// why the variant cannot run on this cluster; errS07MasterOnFirstMember says
// that higher-rank-isolated can once the master has moved.
func s07PickTarget(variant, master string, slaves []string, lastResolved string) (string, error) {
	switch variant {
	case s07MasterIsolated:
		if lastResolved != master {
			return "", fmt.Errorf("status.lastResolvedPrimary is %q, not the master %s: the cut would not be of the "+
				"last resolved primary", lastResolved, master)
		}
		return master, nil
	case s07HigherRankIsolated:
		if lastResolved != master {
			return "", fmt.Errorf("status.lastResolvedPrimary is %q, not the master %s: a trigger could come from "+
				"the last resolved primary and not from the rank alone", lastResolved, master)
		}
		for _, s := range slices.SortedFunc(slices.Values(slaves), func(a, b string) int {
			return memberOrdinal(a) - memberOrdinal(b)
		}) {
			if memberOrdinal(s) >= 0 && memberOrdinal(s) < memberOrdinal(master) {
				return s, nil
			}
		}
		return "", fmt.Errorf("%w %s among %v", errS07MasterOnFirstMember, master, slaves)
	}
	for _, s := range slices.Sorted(slices.Values(slaves)) {
		if s != lastResolved && memberOrdinal(s) > memberOrdinal(master) && memberOrdinal(master) >= 0 {
			return s, nil
		}
	}
	return "", fmt.Errorf("no slave of %v ranks below the master %s and is not the last resolved primary %q: the "+
		"control case cannot run on this cluster", slaves, master, lastResolved)
}

// s07Triggered finds, in the sweeps, the first of two consecutive sweeps
// that showed a trigger, and the sweep before it, or issued when there is
// none: the trigger arose after that.
func s07Triggered(sweeps []s07Sweep, issued time.Time) (observed, windowStart *time.Time, trigger string) {
	for i := 0; i+1 < len(sweeps); i++ {
		if sweeps[i].Trigger == "" || sweeps[i+1].Trigger == "" {
			continue
		}
		at, from := sweeps[i].T, issued
		if i > 0 {
			from = sweeps[i-1].T
		}
		return &at, &from, sweeps[i].Trigger
	}
	return nil, nil, ""
}

// s07NewConnectionsStopped returns when writes through new connections
// stopped succeeding from from on: the start of the first write that was not
// acknowledged after which none started before until was acknowledged; nil
// when the last such write was acknowledged or none was made. A zero until
// is no bound.
func s07NewConnectionsStopped(probes []s07Probe, from, until time.Time) *time.Time {
	sorted := slices.Clone(probes)
	slices.SortFunc(sorted, func(a, b s07Probe) int { return a.StartedAt.Compare(b.StartedAt) })
	var stopped *time.Time
	for _, p := range sorted {
		if p.StartedAt.Before(from) || !until.IsZero() && !p.StartedAt.Before(until) {
			continue
		}
		if s07TookEffect(p) || p.Outcome == workload.Unknown && p.Resolved == "" {
			// An unknown outcome not resolved may have taken effect: it does
			// not show new connections stopped.
			stopped = nil
			continue
		}
		if stopped == nil {
			at := p.StartedAt
			stopped = &at
		}
	}
	return stopped
}

// s07TookEffect reports whether a new-connection write is known to have
// taken effect: acknowledged, or unknown with its row on some member.
func s07TookEffect(p s07Probe) bool {
	return p.Outcome == workload.Acknowledged || p.Resolved == s07UnknownPresent
}

// s07Limits are the contract's limits S07 is judged by; zero is unset.
type s07Limits struct{ Quarantine, Partition time.Duration }

func (h *haRun) s07Limits() s07Limits {
	return s07Limits{Quarantine: h.cfg.QuarantineLimit, Partition: h.cfg.PartitionDuration}
}

// s07Verdict is what judgeS07 makes of a record.
type s07Verdict struct {
	Result       evidence.Result
	Reason       string
	NeverEvents  []string
	Limits       map[string]string
	Measurements map[string]any
}

// manualOnly reports whether ADR-0005 lets only a person release a
// quarantine of this reason.
func manualOnly(reason string) bool {
	return reason == string(databasev1alpha1.WriteQuarantinedMultipleMasters) ||
		reason == string(databasev1alpha1.WriteQuarantinedMasterChanged)
}

// s07NeverEvents are the contract's must-never-happen outcomes of S07 and
// the lane's own.
func s07NeverEvents(rec *s07Record) []string {
	never := []string{}
	never = append(never, rec.ResolvedWhileUncertain...)
	never = append(never, rec.ReopenedWhileQuarantined...)
	if rec.ReleaseSeenAt != nil && manualOnly(rec.ReleaseReason) &&
		rec.ReleaseMethod != string(databasev1alpha1.WriteQuarantineReleasedManually) {
		never = append(never, fmt.Sprintf("a %s quarantine, which only a person may release, was released %s",
			rec.ReleaseReason, rec.ReleaseMethod))
	}
	if d := rec.Data; d != nil && d.Read && rec.HealthyAtEnd &&
		(s07Sum(d.ConflictingByMember) > 0 || d.MasterMissingAcknowledged > 0) {
		never = append(never, fmt.Sprintf("the cluster reported HAReady and ReplicationHealthy True with no "+
			"quarantine after the partition ended while %d conflicting row(s) were on members other than the final "+
			"master %s and the final master lacks %d acknowledged write(s)", s07Sum(d.ConflictingByMember),
			rec.FinalMaster, d.MasterMissingAcknowledged))
	}
	if len(rec.ForeignTargets) > 0 {
		never = append(never, "the run was about to cut Pods that are not its own: "+
			strings.Join(rec.ForeignTargets, "; "))
	}
	return never
}

// s07Broken names the first expectation the record breaks, or "".
func s07Broken(rec *s07Record) string {
	switch {
	case rec.FaultIssuedAt != nil && !rec.CleanupSucceeded:
		return "the network fault was not seen removed from every member: " + cmpOr(rec.CleanupError, "unknown")
	case rec.TriggerObservedAt != nil && rec.QuarantineSeenAt == nil:
		return fmt.Sprintf("no write quarantine was recorded in status.writeQuarantine, although two consecutive "+
			"sweeps from %s showed %s", rec.TriggerObservedAt.Format(time.RFC3339), rec.Trigger)
	case rec.TriggerObservedAt != nil && (rec.RWPodsGoneAt == nil || rec.RWEndpointsEmptyAt == nil):
		return "a write quarantine was recorded, but the run never saw every read-write Broker Pod gone and the " +
			"-rw EndpointSlices empty; RoutingReady=False or a desired replica count alone is no quarantine"
	case rec.Control && rec.TriggerObservedAt == nil && rec.QuarantineSeenAt != nil && s07ControlUncounted(rec) == "":
		return fmt.Sprintf("a write quarantine (%s) naming only %s started in the control case while it was cut "+
			"off, where every sweep read one reachable active master and only a lower-ranked member that is not "+
			"the last resolved primary cut off", firstReason(rec), rec.Target.Pod)
	}
	return ""
}

// s07ControlUncounted says why a quarantine in the control case is not
// counted as a violation, or "" when it is: only a
// WriteQuarantinedMemberUnverified quarantine first seen while the member was
// cut off, naming that member alone, with every sweep complete, the manager
// Pod unchanged, and no member having answered as an active master beside
// the master before it was seen, is one. Another may be legitimate: two
// masters after the heal (rule 1), or an Operator that could not verify a
// reachable member.
func s07ControlUncounted(rec *s07Record) string {
	if len(rec.Revisions) == 0 {
		return "its reason was not read"
	}
	r := rec.Revisions[0]
	claimedBefore := func(t *time.Time) bool { return t != nil && !t.After(r.SeenAt) }
	switch {
	case r.Reason != string(databasev1alpha1.WriteQuarantinedMemberUnverified):
		return "its reason is " + r.Reason
	case !r.WhileCut:
		return "it was first seen after the cut was removed"
	case !slices.Equal(r.MemberNames, []string{rec.Target.Pod}):
		return fmt.Sprintf("it names %s, not %s alone", strings.Join(r.MemberNames, ", "), rec.Target.Pod)
	case rec.IncompleteSweeps > 0:
		return fmt.Sprintf("%d sweep(s) could not read every reachable member's role", rec.IncompleteSweeps)
	case rec.OperatorChangedAt != nil:
		return "the manager Pod changed: " + rec.OperatorChange
	case claimedBefore(rec.TargetClaimedAt) || claimedBefore(rec.TwoClaimsAt):
		return "a member other than the master had answered as an active master before it was seen"
	}
	return ""
}

func firstReason(rec *s07Record) string {
	if len(rec.Revisions) == 0 {
		return ""
	}
	return rec.Revisions[0].Reason
}

// s07Blocked names the first observation that keeps S07 from being judged,
// or "".
func s07Blocked(rec *s07Record) string {
	switch {
	case len(rec.StillRunning) > 0:
		return "steps of the fault flow were still running after their limit: " +
			strings.Join(rec.StillRunning, ", ")
	case rec.FaultConfirmedAt == nil:
		return "the partition was not confirmed: " + cmpOr(rec.NotHeld, "the fault did not take effect")
	case rec.HoldEndedAt == nil:
		return "the partition was not held for the run's hold: " + cmpOr(rec.NotHeld, "unknown")
	case !rec.Control && rec.TriggerObservedAt == nil:
		return "no sweep pair showed a quarantine trigger while the partition held: the run cannot say a " +
			"quarantine was required (whether one started is recorded)"
	case rec.OperatorChangedAt != nil:
		return fmt.Sprintf("the manager Pod changed at %s (%s): the run stopped counting triggers",
			rec.OperatorChangedAt.Format(time.RFC3339), rec.OperatorChange)
	case rec.Control && rec.TriggerObservedAt == nil && rec.QuarantineSeenAt != nil:
		return fmt.Sprintf("a %s quarantine started in the control case and is recorded, not counted as a "+
			"violation, because %s", cmpOr(firstReason(rec), "reason unknown"), s07ControlUncounted(rec))
	case rec.Control && (rec.ClusterReadsAfterGrace < 2 || rec.OneMasterSweepsAfterGrace < 2):
		return fmt.Sprintf("the control was not observed: after the grace the run read the cluster %d time(s) and "+
			"saw one reachable active master in %d complete sweep(s), and needs two of each",
			rec.ClusterReadsAfterGrace, rec.OneMasterSweepsAfterGrace)
	case rec.Session != nil && rec.Session.AcknowledgedAfterClosed > 0:
		return fmt.Sprintf("the already-open connection had %d write(s) acknowledged after the run saw every "+
			"read-write Broker Pod gone and no -rw endpoint: an anomaly the run cannot explain",
			rec.Session.AcknowledgedAfterClosed)
	case rec.SettledAt == nil:
		return "the cluster did not settle after the partition was removed: " + cmpOr(rec.SettleUnmet, "unknown")
	case rec.Data == nil || !rec.Data.Read:
		reason := "not made"
		if rec.Data != nil {
			reason = rec.Data.Error
		}
		return "the post-recovery data check on every member could not be made: " + reason
	case rec.HealthReadings == 0:
		return "the cluster was not read in the health window after the data check"
	}
	return ""
}

// judgeS07 judges what an S07 run recorded once its steps ended. A
// never-event or a broken expectation fails it, the first named in the
// reason; an observation the run could not make blocks it; otherwise its
// limits judge it through faults.Judge, so an unset limit is a baseline,
// never a pass.
func judgeS07(rec *s07Record, limits s07Limits) s07Verdict {
	v := s07Verdict{NeverEvents: s07NeverEvents(rec), Measurements: s07Measurements(rec)}
	quarantine := faults.Bound{Limit: faults.Limit{Name: quarantineLimitName, Value: limits.Quarantine}}
	partition := faults.Bound{Limit: faults.Limit{Name: partitionDurationName, Value: limits.Partition},
		AtLeast: true}
	v.Limits = map[string]string{quarantineLimitName: quarantine.Limit.String(),
		partitionDurationName: partition.Limit.String()}
	if len(v.NeverEvents) > 0 {
		v.Result, v.Reason = evidence.Fail, strings.Join(v.NeverEvents, "; ")
		return v
	}
	if broken := s07Broken(rec); broken != "" {
		v.Result, v.Reason = evidence.Fail, broken
		return v
	}
	if blocked := s07Blocked(rec); blocked != "" {
		v.Result, v.Reason = evidence.Blocked, blocked
		return v
	}
	partition.Measured = rec.HoldEndedAt.Sub(*rec.FaultConfirmedAt)
	bounds := []faults.Bound{partition}
	if rec.TriggerObservedAt != nil {
		if rec.NewConnectionsStoppedAt == nil {
			quarantine.Unmeasured = true
		} else {
			quarantine.Measured = rec.NewConnectionsStoppedAt.Sub(*rec.TriggerWindowStart)
		}
		bounds = append([]faults.Bound{quarantine}, bounds...)
	}
	judged := faults.Judge(bounds...)
	v.Result, v.Reason = judged.Result, judged.Reason
	if v.Result != evidence.Pass {
		return v
	}
	if rec.Control {
		v.Reason = fmt.Sprintf("with %s cut off, no quarantine started, the partition was removed and the cluster "+
			"settled on master %s", rec.Target.Pod, rec.FinalMaster)
	} else {
		v.Reason = fmt.Sprintf("with %s cut off, %s led to a %s quarantine, the read-write Broker Pods and -rw "+
			"endpoints were seen gone and new connections stopped, the partition was removed and the cluster settled "+
			"on master %s with %s", rec.Target.Pod, rec.Trigger, cmpOr(firstReason(rec), "recorded"), rec.FinalMaster,
			cmpOr(rec.QuarantineAtEnd, "no quarantine left"))
	}
	v.Reason += s07DataNote(rec)
	return v
}

// s07Sum adds the counts of a map.
func s07Sum(counts map[string]int) int {
	n := 0
	for _, c := range counts {
		n += c
	}
	return n
}

// s07DataNote reports, for a passing run, what the data check found that the
// run does not fail on: slaves behind the final master, with whether the
// Operator reported the stall, and conflicting rows or acknowledged writes
// the final master lacks while the cluster did not report itself healthy.
func s07DataNote(rec *s07Record) string {
	d := rec.Data
	var notes []string
	for _, member := range slices.Sorted(maps.Keys(d.LaggingByMember)) {
		reported := "the Operator did not report a stalled replication"
		if rec.StallReportedAt != nil {
			reported = "the Operator reported " + rec.StallReason
			if rec.FaultRemovedAt != nil {
				reported += fmt.Sprintf(" %s after the heal", rec.StallReportedAt.Sub(*rec.FaultRemovedAt).Round(
					time.Second))
			}
		}
		notes = append(notes, fmt.Sprintf("slave %s lacks %d row(s) the master holds (%s)", member,
			d.LaggingByMember[member], reported))
	}
	if n := s07Sum(d.ConflictingByMember); n > 0 || d.MasterMissingAcknowledged > 0 {
		notes = append(notes, fmt.Sprintf("%d conflicting row(s) on other members and %d acknowledged write(s) "+
			"the final master lacks, reported while the cluster did not report its replication healthy",
			n, d.MasterMissingAcknowledged))
	}
	if len(notes) == 0 {
		return ""
	}
	return "; " + strings.Join(notes, "; ")
}

// s07Measurements are the times S07 records, as durations from the fault's
// issue on the run's clock, the quarantine's own times as status gives
// them on the Operator's clock, and "unknown" where a value was not seen.
func s07Measurements(rec *s07Record) map[string]any {
	m := map[string]any{}
	since := func(name string, from, to *time.Time) {
		m[name] = evidence.Unknown
		if from != nil && to != nil {
			m[name] = to.Sub(*from).Round(time.Millisecond).String()
		}
	}
	stamp := func(name string, t *time.Time) {
		m[name] = evidence.Unknown
		if t != nil {
			m[name] = t.UTC().Format(time.RFC3339)
		}
	}
	issued := rec.FaultIssuedAt
	since("faultConfirmedWithin", issued, rec.FaultConfirmedAt)
	since("partitionHeldFor", rec.FaultConfirmedAt, rec.HoldEndedAt)
	since("triggerObservedWithin", issued, rec.TriggerObservedAt)
	since("targetClaimedActiveMasterWithin", issued, rec.TargetClaimedAt)
	since("quarantineSeenWithin", issued, rec.QuarantineSeenAt)
	since("rwScaledDownWithin", issued, rec.RWScaledDownAt)
	since("rwPodsGoneWithin", issued, rec.RWPodsGoneAt)
	since("rwEndpointsEmptyWithin", issued, rec.RWEndpointsEmptyAt)
	since("newConnectionsStoppedWithin", issued, rec.NewConnectionsStoppedAt)
	since("quarantineWithin", rec.TriggerWindowStart, rec.NewConnectionsStoppedAt)
	since("releaseSeenWithin", issued, rec.ReleaseSeenAt)
	since("rwReopenedWithin", issued, rec.RWReopenedAt)
	stamp("quarantineStartTime", rec.QuarantineStart)
	stamp("enforcedTime", rec.EnforcedTime)
	stamp("releaseTime", rec.ReleaseTime)
	m["quarantineReason"] = cmpOr(firstReason(rec), evidence.Unknown)
	m["releaseMethod"] = cmpOr(rec.ReleaseMethod, evidence.Unknown)
	m["quarantineAtEnd"] = cmpOr(rec.QuarantineAtEnd, evidence.Unknown)
	m["finalMaster"] = cmpOr(rec.FinalMaster, evidence.Unknown)
	classes := map[string]int{}
	for _, p := range rec.Probes {
		classes[cmpOr(p.Resolved, p.Outcome)]++
	}
	m["newConnectionWrites"] = classes
	m["operatorChange"] = cmpOr(rec.OperatorChange, "none seen")
	m["establishedSession"] = evidence.Unknown
	if s := rec.Session; s != nil && s.ReadError == "" {
		m["establishedSession"] = "open at the end"
		if !s.OpenAtEnd {
			m["establishedSession"] = "ended by " + s.EndedBy
		}
		m["establishedAcknowledgedAfterFault"] = s.AcknowledgedAfterFault
		m["establishedAcknowledgedAfterPathClosed"] = s.AcknowledgedAfterClosed
	}
	since("stallReportedAfterHeal", rec.FaultRemovedAt, rec.StallReportedAt)
	m["stallReason"] = cmpOr(rec.StallReason, evidence.Unknown)
	m["acknowledgedMissing"], m["membersDiverged"] = evidence.Unknown, evidence.Unknown
	if d := rec.Data; d != nil && d.Read {
		m["acknowledgedMissing"], m["membersDiverged"] = d.AcknowledgedMissing, d.Diverged
		if len(d.MissingByMember) > 0 {
			m["acknowledgedMissingByMember"] = d.MissingByMember
		}
		m["laggingByMember"], m["conflictingByMember"] = d.LaggingByMember, d.ConflictingByMember
		m["masterMissingAcknowledged"] = d.MasterMissingAcknowledged
	}
	return m
}

// s07Foreign names each endpoint the faults would enter that is not one of
// the run's own members in the run namespace.
func (h *haRun) s07Foreign(p *netfault.Partition) []string {
	var foreign []string
	endpoints := make([]netfault.Endpoint, 0, 1+len(p.Cuts)+2*len(p.Keep))
	endpoints = append(endpoints, p.Target)
	for _, c := range p.Cuts {
		endpoints = append(endpoints, c.Peer)
	}
	for _, k := range p.Keep {
		endpoints = append(endpoints, k.From, k.To)
	}
	for _, e := range endpoints {
		if !e.Entered() {
			continue
		}
		if e.Namespace != h.cfg.Namespace || !slices.Contains(h.members, e.Pod) {
			foreign = append(foreign, e.Namespace+"/"+e.Pod)
		}
	}
	slices.Sort(foreign)
	return slices.Compact(foreign)
}

// s07Endpoint is a member as the faults name it, read afresh.
func (h *haRun) s07Endpoint(ctx context.Context, name string) (netfault.Endpoint, error) {
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, pod); err != nil {
		return netfault.Endpoint{}, fmt.Errorf("read member Pod %s: %w", name, err)
	}
	if pod.Status.PodIP == "" || pod.Status.Phase != corev1.PodRunning {
		return netfault.Endpoint{}, fmt.Errorf("member Pod %s is not running with an IP", name)
	}
	return netfault.Endpoint{Name: name, Namespace: h.cfg.Namespace, Pod: name, UID: string(pod.UID),
		IP: pod.Status.PodIP, Port: s07MemberPort}, nil
}

// s07Operator is the one manager Pod LAB01 recorded, as an address only: the
// faults never enter it.
func (h *haRun) s07Operator(ctx context.Context) (netfault.Endpoint, error) {
	if len(h.rec.OperatorAfter.Pods) != 1 {
		return netfault.Endpoint{}, fmt.Errorf("want one manager Pod recorded at LAB01's end, found %d",
			len(h.rec.OperatorAfter.Pods))
	}
	manager := h.rec.OperatorAfter.Pods[0]
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: manager.Pod},
		pod); err != nil {
		return netfault.Endpoint{}, fmt.Errorf("read the manager Pod %s: %w", manager.Pod, err)
	}
	if string(pod.UID) != manager.UID || pod.Status.PodIP == "" || pod.DeletionTimestamp != nil {
		return netfault.Endpoint{}, fmt.Errorf("the manager Pod %s is not the running Pod %s LAB01 recorded",
			manager.Pod, manager.UID)
	}
	return netfault.Endpoint{Name: "operator", Namespace: h.cfg.OperatorNamespace, Pod: pod.Name,
		UID: string(pod.UID), IP: pod.Status.PodIP, Port: s07OperatorPort, NoEnter: true}, nil
}

// prepareS07 picks the member to cut off and builds the partition: that
// member cut both ways from every other member and from the manager Pod,
// the other members still reaching each other. Nothing is cut yet; for
// higher-rank-isolated with the master on the first member, the master is
// moved first.
func (h *haRun) prepareS07(ctx context.Context, rec *s07Record) (*netfault.Partition, error) {
	slaves, identities := h.rec.StartingState.Slaves, rec.MembersBefore
	if _, err := s07PickTarget(rec.Variant, rec.Master, slaves, rec.Master); errors.Is(err,
		errS07MasterOnFirstMember) && memberOrdinal(rec.Master) == 0 {
		if slaves, err = h.moveS07Master(ctx, rec); err != nil {
			return nil, err
		}
		identities = rec.MasterMove.Members
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return nil, err
	}
	rec.LastResolvedPrimary = cluster.Status.LastResolvedPrimary
	target, err := s07PickTarget(rec.Variant, rec.Master, slaves, rec.LastResolvedPrimary)
	if err != nil {
		return nil, err
	}
	for _, m := range identities {
		if m.Pod == target {
			rec.Target = m
		}
	}
	env := netfault.Env{Client: h.Client, Kubectl: h.Command, Poll: h.cfg.Poll,
		Reserve: min(netfault.DefaultReserve, h.cfg.CleanupTimeout/4)}
	p := &netfault.Partition{Env: env}
	if p.Target, err = h.s07Endpoint(ctx, target); err != nil {
		return nil, err
	}
	rec.TargetIP = p.Target.IP
	var others []netfault.Endpoint
	for _, m := range h.members {
		if m == target {
			continue
		}
		e, err := h.s07Endpoint(ctx, m)
		if err != nil {
			return nil, err
		}
		others = append(others, e)
		p.Cuts = append(p.Cuts, netfault.Cut{Peer: e, Direction: netfault.Both})
	}
	for _, a := range others {
		for _, b := range others {
			if a.Name != b.Name {
				p.Keep = append(p.Keep, netfault.Path{From: a, To: b})
			}
		}
	}
	op, err := h.s07Operator(ctx)
	if err != nil {
		return nil, err
	}
	rec.Operator, rec.OperatorUID, rec.OperatorIP = op.Pod, op.UID, op.IP
	p.Cuts = append(p.Cuts, netfault.Cut{Peer: op, Direction: netfault.Both})
	if rec.ForeignTargets = h.s07Foreign(p); len(rec.ForeignTargets) > 0 {
		return nil, fmt.Errorf("the partition would enter Pods that are not the run's own: %v", rec.ForeignTargets)
	}
	return p, p.Validate()
}

// beforeS07 verifies the state the cut starts from: the starting conditions,
// the same master, no quarantine in force, and the members as LAB01 left
// them; then it prepares the partition, which refuses a member that still
// has rules of an earlier fault.
func (h *haRun) beforeS07(ctx context.Context, rec *s07Record, p *netfault.Partition) error {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return err
	}
	if unmet, err := clusterReady(cluster); unmet != "" || err != nil {
		return errors.Join(err, errors.New(unmet))
	}
	if q := cluster.Status.WriteQuarantine; q != nil {
		return fmt.Errorf("a write quarantine (%s) is in force before the cut", q.Reason)
	}
	roles, unmet, err := h.roles(ctx)
	if err != nil || unmet != "" {
		return errors.Join(err, errors.New(unmet))
	}
	if roles.Master != rec.Master || cluster.Status.CurrentPrimary != rec.Master ||
		cluster.Status.LastResolvedPrimary != rec.LastResolvedPrimary {
		return fmt.Errorf("the master is %s, status.currentPrimary %q and lastResolvedPrimary %q; %s and %q when "+
			"the target was picked", roles.Master, cluster.Status.CurrentPrimary, cluster.Status.LastResolvedPrimary,
			rec.Master, rec.LastResolvedPrimary)
	}
	return p.Prepare(ctx)
}

// sweepS07 reads every member's role and records the sweep, the trigger it
// shows, and what CUBRID did that the Operator could see only in part. cut
// is set while the target is cut off from the Operator.
func (h *haRun) sweepS07(ctx context.Context, rec *s07Record, now time.Time, cut, afterGrace bool) {
	h.checkS07Operator(ctx, rec, now)
	sweep := s07Sweep{T: now, Roles: map[string]string{}}
	var claims []string
	for _, pod := range h.members {
		out, err := h.member(ctx, pod, "bash", "-c", heartbeatStatus)
		switch {
		case err != nil && !commandRan.MatchString(out):
			sweep.Roles[pod] = s07Unread
		case heartbeatRole(pod, out) == roleMaster:
			sweep.Roles[pod] = s07ActiveMaster
			claims = append(claims, pod)
		case heartbeatRole(pod, out) == roleSlave:
			sweep.Roles[pod] = s07Slave
		default:
			sweep.Roles[pod] = s07OtherRole
		}
	}
	if cut {
		sweep.Unreachable = []string{rec.Target.Pod}
	}
	sweep.Trigger, sweep.Complete = s07Trigger(sweep.Roles, h.members, sweep.Unreachable, rec.LastResolvedPrimary)
	if cut && !sweep.Complete {
		rec.IncompleteSweeps++
	}
	if cut && afterGrace && sweep.Complete && len(slices.DeleteFunc(slices.Clone(claims), func(m string) bool {
		return slices.Contains(sweep.Unreachable, m)
	})) == 1 {
		rec.OneMasterSweepsAfterGrace++
	}
	if cut && slices.Contains(claims, rec.Target.Pod) && rec.TargetClaimedAt == nil {
		rec.TargetClaimedAt = &now
		rec.note("target_claimed_active_master", rec.Target.Pod)
	}
	if len(claims) >= 2 && rec.TwoClaimsAt == nil {
		rec.TwoClaimsAt = &now
		rec.note("two_active_master_claims", strings.Join(claims, " and "))
	}
	rec.Sweeps = append(rec.Sweeps, sweep)
	if rec.TriggerObservedAt == nil && rec.FaultIssuedAt != nil && rec.OperatorChangedAt == nil {
		if at, from, trigger := s07Triggered(rec.Sweeps, *rec.FaultIssuedAt); at != nil {
			rec.TriggerObservedAt, rec.TriggerWindowStart, rec.Trigger = at, from, trigger
			rec.note("trigger_observed", trigger)
		}
	}
}

// observeS07Path reads the quarantine, the release, the conditions and the
// read-write path, and records what changed. cut is set while the target is
// cut off from the Operator; afterGrace while PrimaryResolved and
// RoutingReady must no longer be True for it.
func (h *haRun) observeS07Path(ctx context.Context, rec *s07Record, now time.Time, cut, afterGrace bool) {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		rec.note("cluster_unread", err.Error())
		return
	}
	if cut && afterGrace {
		rec.ClusterReadsAfterGrace++
	}
	h.observeS07Quarantine(rec, cluster, now, cut)
	if !cut {
		noteS07Stall(rec, cluster, now)
	}
	observeS07Conditions(rec, cluster, now, cut && afterGrace)
	path := h.readS07Path(ctx)
	if n := len(rec.Path); n == 0 || rec.Path[n-1].Desired != path.Desired || rec.Path[n-1].Pods != path.Pods ||
		rec.Path[n-1].Endpoints != path.Endpoints || rec.Path[n-1].ReadyEndpoints != path.ReadyEndpoints {
		path.T = now
		rec.Path = append(rec.Path, path)
	}
	if path.Desired == 0 && rec.RWScaledDownAt == nil {
		rec.RWScaledDownAt = &now
		rec.note("rw_scaled_down", "")
	}
	if path.Pods == 0 && rec.RWPodsGoneAt == nil {
		rec.RWPodsGoneAt = &now
		rec.note("rw_pods_gone", "")
	}
	if path.Endpoints == 0 && rec.RWEndpointsEmptyAt == nil {
		rec.RWEndpointsEmptyAt = &now
		rec.note("rw_endpoints_empty", "")
	}
	if rec.RWEndpointsEmptyAt != nil && path.Endpoints > 0 {
		if rec.QuarantineInForce != "" {
			never := fmt.Sprintf("the -rw EndpointSlices had %d endpoint(s) again at %s while the %s quarantine "+
				"was still in force", path.Endpoints, now.Format(time.RFC3339), rec.QuarantineInForce)
			if len(rec.ReopenedWhileQuarantined) == 0 {
				rec.ReopenedWhileQuarantined = append(rec.ReopenedWhileQuarantined, never)
			}
		} else if rec.RWReopenedAt == nil {
			rec.RWReopenedAt = &now
			rec.note("rw_reopened", fmt.Sprintf("%d endpoint(s)", path.Endpoints))
		}
	}
}

// observeS07Conditions records each change of the conditions S07 reads, and
// PrimaryResolved or RoutingReady True while mustNotResolve, as a never-event.
func observeS07Conditions(rec *s07Record, cluster *databasev1alpha1.CubridCluster, now time.Time,
	mustNotResolve bool) {
	for _, t := range []string{readyCondition, primaryResolvedCondition, routingReadyCondition, haReadyCondition,
		replicationHealthyCondition} {
		c := meta.FindStatusCondition(cluster.Status.Conditions, t)
		status, reason := "Unknown", ""
		if c != nil {
			status, reason = string(c.Status), c.Reason
		}
		last := ""
		for _, seen := range rec.Conditions {
			if seen.Type == t {
				last = seen.Status + "/" + seen.Reason
			}
		}
		if last != status+"/"+reason {
			rec.Conditions = append(rec.Conditions, s07Condition{T: now, Type: t, Status: status, Reason: reason})
		}
		resolving := t == primaryResolvedCondition || t == routingReadyCondition
		if mustNotResolve && resolving && status == string(metav1.ConditionTrue) {
			never := fmt.Sprintf("the Operator reported %s True at %s while %s was cut off from it", t,
				now.Format(time.RFC3339), rec.Target.Pod)
			if !slices.ContainsFunc(rec.ResolvedWhileUncertain, func(s string) bool {
				return strings.HasPrefix(s, "the Operator reported "+t+" True")
			}) {
				rec.ResolvedWhileUncertain = append(rec.ResolvedWhileUncertain, never)
			}
		}
	}
}

// observeS07Quarantine records the quarantine in force, each reason and
// revision, its enforcement, and the release of a quarantine the run saw.
func (h *haRun) observeS07Quarantine(rec *s07Record, cluster *databasev1alpha1.CubridCluster, now time.Time,
	cut bool) {
	q := cluster.Status.WriteQuarantine
	rec.QuarantineInForce = ""
	if q != nil {
		rec.QuarantineInForce = string(q.Reason)
		if rec.QuarantineSeenAt == nil {
			start := q.StartTime.UTC()
			rec.QuarantineSeenAt, rec.QuarantineStart = &now, &start
			rec.note("quarantine_seen", string(q.Reason))
		}
		if !slices.Contains(rec.Generations, q.GenerationID) {
			rec.Generations = append(rec.Generations, q.GenerationID)
		}
		if n := len(rec.Revisions); n == 0 || rec.Revisions[n-1].GenerationID != q.GenerationID ||
			rec.Revisions[n-1].Revision != q.Revision || rec.Revisions[n-1].Reason != string(q.Reason) {
			r := s07Revision{SeenAt: now, GenerationID: q.GenerationID, Reason: string(q.Reason), Revision: q.Revision,
				Master: q.ObservedMaster, WhileCut: cut}
			for _, m := range q.Members {
				r.Members = append(r.Members, m.Name+" ("+m.ObservedRole+")")
				r.MemberNames = append(r.MemberNames, m.Name)
			}
			rec.Revisions = append(rec.Revisions, r)
		}
		if q.EnforcedTime != nil && rec.EnforcedTime == nil {
			enforced := q.EnforcedTime.UTC()
			rec.EnforcedTime, rec.EnforcedSeenAt = &enforced, &now
			rec.note("quarantine_enforced", enforced.Format(time.RFC3339))
		}
	}
	if rel := cluster.Status.LastWriteQuarantineRelease; rel != nil && rec.ReleaseSeenAt == nil &&
		slices.Contains(rec.Generations, rel.GenerationID) {
		released := rel.ReleaseTime.UTC()
		rec.ReleaseSeenAt, rec.ReleaseTime = &now, &released
		rec.ReleaseMethod, rec.ReleaseReason, rec.ReleaseGeneration = string(rel.Method), string(rel.Reason),
			rel.GenerationID
		rec.note("quarantine_released", string(rel.Method))
	}
}

// readS07Path reads the read-write Broker Deployment's desired replicas, its
// Pods and the -rw EndpointSlices. A value that could not be read is -1.
func (h *haRun) readS07Path(ctx context.Context) s07Path {
	path := s07Path{Desired: -1, Pods: -1, Endpoints: -1, ReadyEndpoints: -1}
	deployment := &appsv1.Deployment{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace,
		Name: haClusterName + "-broker-" + s07ReadWrite}, deployment); err == nil {
		path.Desired = 1
		if deployment.Spec.Replicas != nil {
			path.Desired = *deployment.Spec.Replicas
		}
	}
	pods := &corev1.PodList{}
	if err := h.Client.List(ctx, pods, client.InNamespace(h.cfg.Namespace),
		client.MatchingLabels{instanceLabel: haClusterName, componentLabel: brokerComponent,
			"database.cubrid.io/broker-access-mode": s07ReadWrite}); err == nil {
		path.Pods = len(pods.Items)
	}
	endpointSlices := &discoveryv1.EndpointSliceList{}
	if err := h.Client.List(ctx, endpointSlices, client.InNamespace(h.cfg.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: haClusterName + "-" + s07ReadWrite}); err == nil {
		path.Endpoints, path.ReadyEndpoints = 0, 0
		for _, s := range endpointSlices.Items {
			for _, e := range s.Endpoints {
				path.Endpoints++
				if e.Conditions.Ready == nil || *e.Conditions.Ready {
					path.ReadyEndpoints++
				}
			}
		}
	}
	return path
}

// s07Probes is the script that writes, in the client, one operation
// through a new connection to the read-write Service after another until a
// stop file appears, each under a client ID of its own, and logs each with
// its start and end on the client's clock and its exit code.
const s07Probes = `i=0; while [ ! -e /tmp/s07n.stop ]; do i=$((i+1)); s=$(date +%s.%N); ` +
	`CLIENT_ID=s07n$i OPS=1 ROLLBACK_EVERY=0 workload run >> /tmp/s07n.jsonl 2>>/tmp/s07n.err; x=$?; ` +
	`echo "s07n$i $s $(date +%s.%N) $x" >> /tmp/s07n.log; sleep 1; done`

// startS07Probes starts the new-connection writes in the background.
func (h *haRun) startS07Probes(ctx context.Context) error {
	_, err := h.clientRun(ctx, "rm -f /tmp/s07n.stop; nohup sh -c '"+s07Probes+"' >/dev/null 2>&1 & "+
		"echo $! > /tmp/s07n.pid")
	return err
}

// stopS07Probes asks the new-connection writes to stop after the one under
// way, and waits for them within its own bound, also when ctx has ended.
func (h *haRun) stopS07Probes(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	out, err := h.clientRun(ctx, `touch /tmp/s07n.stop; p=$(cat /tmp/s07n.pid 2>/dev/null); `+
		`[ -n "$p" ] || { echo stopped; exit 0; }; i=0; while [ $i -lt 75 ]; do `+
		`s=$(cut -d' ' -f3 "/proc/$p/stat" 2>/dev/null); `+
		`if [ -z "$s" ] || [ "$s" = Z ]; then echo stopped; exit 0; fi; i=$((i+1)); sleep 1; done; `+
		`kill "$p" 2>/dev/null; echo running`)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "stopped" {
		return errors.New("the new-connection writes did not stop")
	}
	return nil
}

// readS07Probes reads the new-connection writes, adds their lines to the
// run's history so the data rules account for their rows, and returns each
// write with its times on the run's clock.
func (h *haRun) readS07Probes(ctx context.Context, offset time.Duration) ([]s07Probe, error) {
	log, err := h.clientRun(ctx, "cat /tmp/s07n.log 2>/dev/null; true")
	if err != nil {
		return nil, err
	}
	text, err := h.clientRun(ctx, "cat /tmp/s07n.jsonl 2>/dev/null; true")
	if err != nil {
		return nil, err
	}
	text = text[:strings.LastIndex(text, "\n")+1]
	h.history.WriteString(text)
	if err := h.write(haHistoryFile, []byte(h.history.String())); err != nil {
		return nil, err
	}
	history, err := workload.ReadHistory(strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	return parseS07Probes(log, history, offset), nil
}

// parseS07Probes reads the probe log, one line per write: client, start
// and end on the client's clock and exit code, and takes each write's
// outcome from the history. offset is the client's clock minus the run's.
func parseS07Probes(log string, history workload.History, offset time.Duration) []s07Probe {
	probes := []s07Probe{}
	epoch := func(s string) (time.Time, bool) {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(0, int64(f*1e9)).UTC().Add(-offset), true
	}
	for line := range strings.Lines(log) {
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}
		start, ok1 := epoch(fields[1])
		end, ok2 := epoch(fields[2])
		exit, err := strconv.Atoi(fields[3])
		if !ok1 || !ok2 || err != nil {
			continue
		}
		p := s07Probe{Client: fields[0], StartedAt: start, AnsweredAt: end, Exit: exit, Outcome: s07NoAnswer}
		if op, ok := history.Operations[fmt.Sprintf("%s-%06d", p.Client, 1)]; ok && op.Outcome != "" {
			p.Outcome, p.Error = op.Outcome, op.Error
		}
		probes = append(probes, p)
	}
	return probes
}

// readS07Session reads the history of the client that held one connection
// open, adds it to the run's history, and records what happened to that
// connection. faultAt and closedAt are on the client's clock; closedAt is
// zero when the run never saw the path closed.
func (h *haRun) readS07Session(ctx context.Context, rec *s07Record, faultAt, closedAt time.Time) {
	s := &s07Session{}
	rec.Session = s
	text, err := h.clientRun(ctx, "cat /tmp/"+s07OpenClient+".jsonl")
	if err != nil {
		s.ReadError = err.Error()
		return
	}
	text = text[:strings.LastIndex(text, "\n")+1]
	h.history.WriteString(text)
	if err := h.write(haHistoryFile, []byte(h.history.String())); err != nil {
		s.ReadError = err.Error()
	}
	events, err := workload.ReadEvents(strings.NewReader(text))
	if err != nil {
		s.ReadError = err.Error()
		return
	}
	s.OpenAtEnd = true
	for _, e := range events {
		t := e.T.UTC()
		switch e.Event {
		case workload.Acknowledged:
			s.Acknowledged++
			s.LastAcknowledgedAt = &t
			if t.After(faultAt) {
				s.AcknowledgedAfterFault++
			}
			if !closedAt.IsZero() && t.After(closedAt) {
				s.AcknowledgedAfterClosed++
			}
		case workload.Failed:
			s.Failed++
		case workload.Unknown:
			s.Unknown++
		}
		if s.EndedAt == nil && (e.Event == workload.Failed || e.Event == workload.Unknown) {
			s.EndedAt, s.EndedBy, s.OpenAtEnd = &t, e.Event+" "+e.OpID+": "+e.Error, false
		}
	}
	if diagnostics, err := h.clientRun(ctx, "cat /tmp/"+s07OpenClient+".diag.jsonl"); err == nil {
		file := s07File(rec.Variant, "open-connection.jsonl")
		if h.write(file, []byte(diagnostics)) == nil {
			s.Diagnostics = file
		}
	}
}

// removeOnEnd takes the drop rules away as soon as ctx ends, as on a signal,
// the run's timeout or a lost Lease, without waiting for a step of the fault
// that is still running; the fault's own removal follows. The returned func
// stops the watch when the scenario ends and returns once the watch has; it
// may be called more than once.
func (h *haRun) removeOnEnd(ctx context.Context, rec *s07Record, p *netfault.Partition) func() {
	return p.RemoveOnEnd(ctx, s07EndRemoveBound, func(err error) {
		rec.mu.Lock()
		rec.RemovedOnEnd = err == nil
		if err != nil {
			rec.RemoveOnEndError = err.Error()
		}
		rec.mu.Unlock()
	})
}

// whileCut observes the cluster while the target is cut off and holds once
// the partition was held for PartitionHold and is confirmed again.
func (h *haRun) whileCut(ctx context.Context, rec *s07Record, p *netfault.Partition) (bool, error) {
	now := time.Now().UTC()
	if rec.FaultConfirmedAt == nil {
		rec.FaultConfirmedAt = &now
	}
	h.sweepS07(ctx, rec, now, true, now.Sub(*rec.FaultConfirmedAt) >= primaryResolvedGrace)
	h.observeS07Path(ctx, rec, now, true, now.Sub(*rec.FaultConfirmedAt) >= primaryResolvedGrace)
	if time.Since(*rec.FaultConfirmedAt) < h.cfg.PartitionHold {
		return false, nil
	}
	held, err := p.Confirm(ctx)
	if undecided := (*netfault.UndecidedError)(nil); errors.As(err, &undecided) {
		rec.NotHeld = err.Error()
		return false, nil
	}
	if err != nil {
		rec.NotHeld = err.Error()
		return false, err
	}
	if held {
		at := time.Now().UTC()
		rec.HoldEndedAt = &at
		rec.note("hold_ended", "")
	}
	return held, nil
}

// settleS07 observes the cluster after the partition was removed until
// every member answers with one master and two slaves and any quarantine
// left is one only a person may release, within s07SettleBound.
func (h *haRun) settleS07(ctx context.Context, rec *s07Record) {
	deadline := time.Now().Add(s07SettleBound)
	for {
		now := time.Now().UTC()
		h.sweepS07(ctx, rec, now, false, false)
		h.observeS07Path(ctx, rec, now, false, false)
		roles := rec.Sweeps[len(rec.Sweeps)-1].Roles
		var masters, slaves []string
		for _, m := range h.members {
			switch roles[m] {
			case s07ActiveMaster:
				masters = append(masters, m)
			case s07Slave:
				slaves = append(slaves, m)
			}
		}
		switch {
		case len(masters) != 1 || len(slaves) != len(h.members)-1:
			rec.SettleUnmet = fmt.Sprintf("CUBRID reports the roles %v", roles)
		case rec.QuarantineInForce != "" && !manualOnly(rec.QuarantineInForce):
			rec.SettleUnmet = "the " + rec.QuarantineInForce + " quarantine is still in force and may yet be " +
				"released automatically"
		default:
			rec.SettledAt, rec.SettleUnmet = &now, ""
			rec.FinalMaster, rec.FinalSlaves, rec.QuarantineAtEnd = masters[0], slaves, rec.QuarantineInForce
			rec.note("settled", "master "+masters[0])
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.cfg.Poll):
		}
	}
}

// runS07 runs S07 on the cluster LAB01 left in its checked state: one
// client holds a connection to the read-write Service from before the fault
// and another writes through a new connection again and again, while a
// member is cut off both ways from the other members and from the manager
// Pod for PartitionHold. The run reads every member's role on each poll
// through its kubelet and decides from what the Operator can reach whether
// ADR-0005 section 8 requires a quarantine; it records the quarantine, its
// enforcement as the read-write Broker Pods and -rw endpoints the run sees,
// the SQL of both clients and the release, removes the fault however the run
// ends, waits for the cluster to settle and checks every member's rows. A
// run that ends before it is judged is blocked, not failed.
func (h *haRun) runS07(ctx context.Context) (result evidence.Scenario) {
	variant := s07Variant(h.cfg.Fault)
	defer h.inScenario(s07ID+"/"+variant, &result)()
	result = evidence.Scenario{ID: s07ID, Variant: variant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := &s07Record{Variant: variant, Control: variant == s07SlaveIsolated, Master: h.rec.StartingState.Master,
		MembersBefore: h.rec.MembersAfter, MembersAfter: []imageIdentity{}, Sweeps: []s07Sweep{},
		Conditions: []s07Condition{}, Path: []s07Path{}, Probes: []s07Probe{}, Observed: []faults.Step{}}
	clientsStarted := false
	var p *netfault.Partition
	defer func() {
		if clientsStarted && !rec.ClientsStopped {
			err := errors.Join(h.stopS07Probes(ctx), h.stopBackgroundClients(ctx, []string{s07OpenClient}))
			rec.ClientsStopped = err == nil
			if err != nil && result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, "stop the clients: "+err.Error()
			}
		}
		if clientsStarted && !rec.ClientsRead {
			rec.ClientHistories = h.keepClientHistories(ctx, []string{s07OpenClient, "s07n"},
				s07File(variant, "clients/"))
		}
		if p != nil {
			r := p.Record()
			rec.Partition = &r
		}
		if result.Measurements == nil || result.Limits == nil {
			v := judgeS07(rec, h.s07Limits())
			result.Measurements, result.Limits = v.Measurements, v.Limits
		}
		rec.mu.Lock()
		err := h.writeJSON(s07File(variant, "record.json"), rec)
		if rec.cut != nil {
			rec.cut.RemovedOnEnd = rec.RemovedOnEnd
			err = errors.Join(err, h.writeJSON(s07File(variant, "cut.json"), rec.cut))
		}
		rec.mu.Unlock()
		if err != nil {
			result.Result, result.Reason = evidence.Fail, "write S07 evidence: "+err.Error()
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, s07ID+"/"+variant+"/")
		})
	}()
	end := func(r evidence.Result, format string, args ...any) evidence.Scenario {
		result.Result, result.Reason = r, fmt.Sprintf(format, args...)
		if ctx.Err() != nil && r == evidence.Fail && len(result.NeverEvents) == 0 {
			result.Result, result.Reason = evidence.Blocked, "the run ended before S07 was judged: "+result.Reason
		}
		return result
	}
	var err error
	if p, err = h.prepareS07(ctx, rec); err != nil {
		if len(rec.ForeignTargets) > 0 {
			result.NeverEvents = s07NeverEvents(rec)
			return end(evidence.Fail, "%v", err)
		}
		return end(evidence.Blocked, "%v", err)
	}
	offsets, err := h.clockOffsets(ctx)
	rec.ClockOffsets = offsets
	if err != nil {
		return end(evidence.Blocked, "%v", err)
	}
	if h.rec.Client == nil {
		return end(evidence.Blocked, "the workload client's node is not known: its clock offset cannot be read")
	}
	if _, ok := offsets[h.rec.Client.Node]; !ok {
		return end(evidence.Blocked, "the clock offset of the workload client's node %s was not read",
			h.rec.Client.Node)
	}
	if _, err := h.clientRun(ctx, "rm -f /tmp/s07n.log /tmp/s07n.jsonl /tmp/s07n.err /tmp/s07n.stop /tmp/s07n.pid "+
		"/tmp/"+s07OpenClient+".jsonl /tmp/"+s07OpenClient+".diag.jsonl"); err != nil {
		return end(evidence.Blocked, "clear the clients' files: %v", err)
	}
	clientsStarted = true
	if err := h.startBackgroundClientWith(ctx, s07OpenClient, "APP_RECONNECT_POLICY=stop-after-error "+
		"APP_CONNECTION_DIAGNOSTICS=/tmp/"+s07OpenClient+".diag.jsonl "); err != nil {
		return end(evidence.Blocked, "start the established-session client: %v", err)
	}
	if err := h.startS07Probes(ctx); err != nil {
		return end(evidence.Blocked, "start the new-connection writes: %v", err)
	}
	stopWatch := h.removeOnEnd(ctx, rec, p)
	defer stopWatch()
	outcome := faults.Run(ctx, faults.Scenario{
		Fault: &s07Cut{Partition: p, h: h, rec: rec},
		Limits: faults.Limits{Confirm: s07PrepareBound, Outcome: h.cfg.PartitionHold + s07ConfirmSlack,
			Cleanup: s07CleanupBound, Poll: h.cfg.Poll},
		Before: func(ctx context.Context) error { return h.beforeS07(ctx, rec, p) },
		Check: func(ctx context.Context) (bool, error) {
			done, err := h.whileCut(ctx, rec, p)
			if ctx.Err() != nil {
				return false, nil
			}
			return done, err
		},
	})
	stopWatch()
	h.recordS07Outcome(rec, outcome)
	if err := h.writeTimeline(s07File(variant, "timeline.jsonl"), outcome.Timeline); err != nil {
		return end(evidence.Fail, "write the timeline: %v", err)
	}
	result.FaultConfirmed = &outcome.FaultConfirmed
	if !outcome.FaultIssuedAt.IsZero() {
		result.CleanupSucceeded = &outcome.CleanupSucceeded
	}
	if outcome.FaultIssuedAt.IsZero() {
		return end(evidence.Blocked, "%s", outcome.Reason)
	}
	if len(outcome.StillRunning) > 0 {
		// A step that did not return may still write the record: nothing more
		// is observed or read next to it.
		rec.mu.Lock()
		rec.StillRunning = outcome.StillRunning
		rec.mu.Unlock()
		return end(evidence.Blocked, "steps of the fault flow were still running after their limit: %s",
			strings.Join(outcome.StillRunning, ", "))
	}
	if outcome.CleanupSucceeded {
		h.settleS07(ctx, rec)
	}
	return h.finishS07(ctx, rec, &result, end)
}

// recordS07 records the S07 partition selected to follow LAB01, or settles
// it as blocked when it did not run.
func (h *haRun) recordS07(summary *evidence.Summary) {
	switch {
	case h.s07 != nil:
		summary.Record(*h.s07)
	case h.cfg.partitionFault():
		summary.Settle(evidence.Requirement{ID: s07ID, Variant: s07Variant(h.cfg.Fault)}, evidence.Blocked,
			"S07 starts from LAB01's checked state, and LAB01 did not pass")
	}
}

// recordS07Outcome takes the fault's times and cleanup from the outcome.
func (h *haRun) recordS07Outcome(rec *s07Record, o faults.Outcome) {
	if !o.FaultIssuedAt.IsZero() {
		issued := o.FaultIssuedAt
		rec.FaultIssuedAt = &issued
	}
	rec.CleanupSucceeded = o.CleanupSucceeded
	for _, step := range o.Timeline {
		switch step.Kind {
		case "fault_removed":
			t := step.T
			rec.FaultRemovedAt = &t
		case "cleanup_failed":
			rec.CleanupError = step.Detail
		}
	}
	if !o.FaultConfirmed || o.Result != evidence.Pass {
		rec.NotHeld = strings.TrimSpace(cmpOr(rec.NotHeld, o.Reason))
		if !o.FaultConfirmed {
			rec.FaultConfirmedAt = nil
		}
	}
}

// finishS07 stops and reads the clients, checks every member's rows, and
// judges S07.
func (h *haRun) finishS07(ctx context.Context, rec *s07Record, result *evidence.Scenario,
	end func(evidence.Result, string, ...any) evidence.Scenario) evidence.Scenario {
	if err := errors.Join(h.stopS07Probes(ctx), h.stopBackgroundClients(ctx, []string{s07OpenClient})); err != nil {
		return end(evidence.Fail, "stop the clients: %v", err)
	}
	rec.ClientsStopped = true
	offset := time.Duration(rec.ClockOffsets[h.rec.Client.Node] * float64(time.Second))
	probes, err := h.readS07Probes(ctx, offset)
	if err != nil {
		return end(evidence.Blocked, "read the new-connection writes: %v", err)
	}
	rec.Probes = probes
	closed := s07PathClosedAt(rec)
	var closedOnClient time.Time
	if closed != nil {
		closedOnClient = closed.Add(offset)
	}
	h.readS07Session(ctx, rec, rec.FaultIssuedAt.Add(offset), closedOnClient)
	rec.ClientsRead = true
	if all, err := workload.ReadHistory(strings.NewReader(h.history.String())); err == nil {
		h.rec.Operations = all.Counts
		result.Operations = &evidence.Operations{Attempted: all.Counts[workload.Attempted],
			Acknowledged: all.Counts[workload.Acknowledged], Failed: all.Counts[workload.Failed],
			Unknown: all.Counts[workload.Unknown]}
	}
	if rec.SettledAt != nil {
		h.checkS07Data(ctx, rec)
		h.observeS07Health(ctx, rec)
	}
	h.judgeS07Probes(rec, closed)
	v := judgeS07(rec, h.s07Limits())
	result.Measurements, result.Limits, result.NeverEvents = v.Measurements, v.Limits, v.NeverEvents
	return end(v.Result, "%s", v.Reason)
}

// judgeS07Probes records, from the new-connection writes with their unknown
// outcomes resolved, when new connections stopped succeeding and whether one
// took effect after the run saw the path closed while the quarantine was in
// force.
func (h *haRun) judgeS07Probes(rec *s07Record, closed *time.Time) {
	until := time.Time{}
	if rec.ReleaseSeenAt != nil {
		until = *rec.ReleaseSeenAt
	}
	if closed != nil {
		for _, p := range rec.Probes {
			if s07TookEffect(p) && p.StartedAt.After(*closed) && (until.IsZero() || p.StartedAt.Before(until)) {
				rec.ReopenedWhileQuarantined = append(rec.ReopenedWhileQuarantined, fmt.Sprintf(
					"the write %s through a new connection to %s-rw, started at %s after the run saw every "+
						"read-write Broker Pod gone and no -rw endpoint, took effect (%s) while the quarantine was "+
						"in force", p.Client, haClusterName, p.StartedAt.Format(time.RFC3339),
					cmpOr(p.Resolved, p.Outcome)))
				break
			}
		}
	}
	if rec.TriggerWindowStart != nil {
		rec.NewConnectionsStoppedAt = s07NewConnectionsStopped(rec.Probes, *rec.TriggerWindowStart, until)
	}
}

// s07PathClosedAt is when the run had seen both every read-write Broker Pod
// gone and no -rw endpoint, with the slack kube-proxy may take, or nil.
func s07PathClosedAt(rec *s07Record) *time.Time {
	if rec.RWPodsGoneAt == nil || rec.RWEndpointsEmptyAt == nil {
		return nil
	}
	at := *rec.RWPodsGoneAt
	if rec.RWEndpointsEmptyAt.After(at) {
		at = *rec.RWEndpointsEmptyAt
	}
	at = at.Add(s07ProbeSlack)
	return &at
}

// checkS07Data applies the data rules of the run's whole history to every
// member, read directly and not through a Broker, records what is missing
// and what differs, and resolves the new-connection writes whose outcome
// was unknown by whether their row is on some member.
func (h *haRun) checkS07Data(ctx context.Context, rec *s07Record) {
	file := s07File(rec.Variant, "data-check.json")
	d := &s07Data{File: file, MissingByMember: map[string]int{}}
	rec.Data = d
	target := dataTarget{members: h.members, slaves: rec.FinalSlaves}
	report, _, err := h.checkDataOf(ctx, h.history.String(), target, time.Now(), file)
	if report == nil {
		d.Error = "read the ledger rows: " + errString(err)
		return
	}
	if err != nil {
		if _, violated := errors.AsType[dataViolation](err); !violated {
			d.Error = err.Error()
			return
		}
	}
	d.Read, d.OK = true, report.OK
	d.AcknowledgedMissing, d.Diverged = report.AcknowledgedMissing, len(report.Diverged)
	for member, check := range report.Members {
		for _, v := range check.Violations {
			if v.Rule == workload.RuleMissing {
				d.MissingByMember[member]++
			}
		}
	}
	d.MasterMissingAcknowledged = d.MissingByMember[rec.FinalMaster]
	rows, err := h.readRowsOf(ctx, dataTarget{members: h.members})
	if err != nil {
		d.Read, d.Error = false, "read the rows to compare with the final master: "+err.Error()
		rec.note("unknown_outcomes_unresolved", err.Error())
	} else {
		d.LaggingByMember, d.ConflictingByMember = classifyS07Rows(rec.FinalMaster, rows)
		present := map[string]bool{}
		for _, member := range rows {
			for _, row := range member {
				present[row.OpID] = true
			}
		}
		resolveS07Unknowns(rec.Probes, present)
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err == nil {
		if members, unmet, err := h.memberIdentities(ctx, cluster); err == nil && unmet == "" {
			rec.MembersAfter = members
		}
	}
}

// resolveS07Unknowns resolves each new-connection write with an unknown
// outcome by whether its row is present on some member.
func resolveS07Unknowns(probes []s07Probe, present map[string]bool) {
	for i := range probes {
		if probes[i].Outcome != workload.Unknown {
			continue
		}
		probes[i].Resolved = s07UnknownAbsent
		if present[fmt.Sprintf("%s-%06d", probes[i].Client, 1)] {
			probes[i].Resolved = s07UnknownPresent
		}
	}
}

// s07ReportsHealthy reports whether the cluster says it is healthy,
// replication included: HAReady and ReplicationHealthy True and no
// quarantine. Ready alone does not cover replication.
func s07ReportsHealthy(cluster *databasev1alpha1.CubridCluster) bool {
	return cluster.Status.WriteQuarantine == nil &&
		meta.IsStatusConditionTrue(cluster.Status.Conditions, haReadyCondition) &&
		meta.IsStatusConditionTrue(cluster.Status.Conditions, replicationHealthyCondition)
}

// s07StallReport returns what the cluster reports of a stalled replication,
// or "": HAReady or ReplicationHealthy False with LogCopyStalled,
// ReplicationStalled or ApplyFailures.
func s07StallReport(conditions []metav1.Condition) string {
	var reported []string
	for _, t := range []string{haReadyCondition, replicationHealthyCondition} {
		c := meta.FindStatusCondition(conditions, t)
		if c != nil && c.Status == metav1.ConditionFalse && slices.Contains(s07StallReasons, c.Reason) {
			reported = append(reported, t+"=False/"+c.Reason)
		}
	}
	return strings.Join(reported, "; ")
}

// s07StallReasons are the reasons of HAReady and ReplicationHealthy for a
// slave that does not apply the master's log (docs/observability.md).
var s07StallReasons = []string{"LogCopyStalled", "ReplicationStalled", "ApplyFailures"}

// noteS07Stall records the first report of a stalled replication after the
// cut was removed.
func noteS07Stall(rec *s07Record, cluster *databasev1alpha1.CubridCluster, now time.Time) {
	if rec.StallReportedAt != nil {
		return
	}
	if stall := s07StallReport(cluster.Status.Conditions); stall != "" {
		rec.StallReportedAt, rec.StallReason = &now, stall
		rec.note("stall_reported", stall)
	}
}

// classifyS07Rows compares each other member's rows with the final
// master's: rows the master holds that the member lacks (lagging), and rows
// the member holds that the master lacks or holds with other values
// (conflicting).
func classifyS07Rows(master string, rows map[string][]workload.Row) (lagging, conflicting map[string]int) {
	lagging, conflicting = map[string]int{}, map[string]int{}
	held := map[string]workload.Row{}
	for _, r := range rows[master] {
		held[r.OpID] = r
	}
	for member, memberRows := range rows {
		if member == master {
			continue
		}
		seen := map[string]bool{}
		for _, r := range memberRows {
			seen[r.OpID] = true
			if want, ok := held[r.OpID]; !ok || want != r {
				conflicting[member]++
			}
		}
		for id := range held {
			if !seen[id] {
				lagging[member]++
			}
		}
	}
	maps.DeleteFunc(lagging, func(_ string, n int) bool { return n == 0 })
	maps.DeleteFunc(conflicting, func(_ string, n int) bool { return n == 0 })
	return lagging, conflicting
}

// observeS07Health reads the cluster for s07HealthWindow and records
// whether any reading reported it healthy (s07ReportsHealthy), whether one
// reported Ready with no quarantine, and the first report of a stall.
func (h *haRun) observeS07Health(ctx context.Context, rec *s07Record) {
	deadline := time.Now().Add(s07HealthWindow)
	for {
		cluster := &databasev1alpha1.CubridCluster{}
		err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster)
		if err == nil {
			rec.HealthReadings++
			now := time.Now().UTC()
			noteS07Stall(rec, cluster, now)
			if cluster.Status.WriteQuarantine == nil && rec.ReadyWithoutQuarantineAt == nil &&
				meta.IsStatusConditionTrue(cluster.Status.Conditions, readyCondition) {
				rec.ReadyWithoutQuarantineAt = &now
				rec.note("ready_without_quarantine", "")
			}
			if rec.ReportedHealthyAt == nil && s07ReportsHealthy(cluster) {
				rec.ReportedHealthyAt, rec.HealthyAtEnd = &now, true
				rec.note("reported_healthy", "HAReady and ReplicationHealthy True with no quarantine")
			}
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.cfg.Poll):
		}
	}
}

// checkS07Operator reads the manager Pod and records the first time it is
// gone, replaced or has another IP.
func (h *haRun) checkS07Operator(ctx context.Context, rec *s07Record, now time.Time) {
	if rec.OperatorChangedAt != nil || rec.Operator == "" {
		return
	}
	pod := &corev1.Pod{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.OperatorNamespace, Name: rec.Operator}, pod)
	change := ""
	switch {
	case apierrors.IsNotFound(err):
		change = "the manager Pod " + rec.Operator + " is gone"
	case err != nil:
		rec.note("operator_unread", err.Error())
		return
	case string(pod.UID) != rec.OperatorUID:
		change = fmt.Sprintf("the manager Pod %s has UID %s, was %s", rec.Operator, pod.UID, rec.OperatorUID)
	case pod.Status.PodIP != rec.OperatorIP:
		change = fmt.Sprintf("the manager Pod %s has IP %s, was %s", rec.Operator, pod.Status.PodIP, rec.OperatorIP)
	case pod.DeletionTimestamp != nil:
		change = "the manager Pod " + rec.Operator + " is being deleted"
	default:
		return
	}
	rec.OperatorChangedAt, rec.OperatorChange = &now, change
	rec.note("operator_changed", change)
}

// s07CutFile is the state of the cut alone, written before it is injected
// and when it is removed, so that what was injected is on disk before a
// SIGKILL.
type s07CutFile struct {
	Namespace         string     `json:"namespace"`
	Target            string     `json:"target"`
	TargetIP          string     `json:"targetIP"`
	Peers             []string   `json:"peers"`
	OperatorPod       string     `json:"operatorPod"`
	OperatorIP        string     `json:"operatorIP"`
	InjectRequestedAt *time.Time `json:"injectRequestedAt,omitempty"`
	RemovedAt         *time.Time `json:"removedAt,omitempty"`
	RemoveError       string     `json:"removeError,omitempty"`
	RemovedOnEnd      bool       `json:"dropRulesRemovedOnRunEnd,omitempty"`
}

// s07Cut is the partition with the cut state written around its injection
// and removal.
type s07Cut struct {
	*netfault.Partition
	h   *haRun
	rec *s07Record
}

// Inject writes the cut's state, then injects it.
func (c *s07Cut) Inject(ctx context.Context) error {
	now := time.Now().UTC()
	c.writeState(func(f *s07CutFile) { f.InjectRequestedAt = &now })
	return c.Partition.Inject(ctx)
}

// Remove removes the cut and writes how that went.
func (c *s07Cut) Remove(ctx context.Context) error {
	err := c.Partition.Remove(ctx)
	now := time.Now().UTC()
	c.writeState(func(f *s07CutFile) {
		if err != nil {
			f.RemoveError = err.Error()
			return
		}
		f.RemovedAt, f.RemoveError = &now, ""
	})
	return err
}

// writeState changes the cut's state and writes cut.json at once, under the
// record's lock. The file is listed with the record at the scenario's end.
func (c *s07Cut) writeState(change func(*s07CutFile)) {
	c.rec.mu.Lock()
	defer c.rec.mu.Unlock()
	if c.rec.cut == nil {
		f := &s07CutFile{Namespace: c.h.cfg.Namespace, Target: c.Target.Pod, TargetIP: c.Target.IP}
		for _, cut := range c.Cuts {
			if cut.Peer.NoEnter {
				f.OperatorPod, f.OperatorIP = cut.Peer.Pod, cut.Peer.IP
				continue
			}
			f.Peers = append(f.Peers, cut.Peer.Pod)
		}
		c.rec.cut = f
	}
	change(c.rec.cut)
	c.rec.cut.RemovedOnEnd = c.rec.RemovedOnEnd
	if data, err := json.MarshalIndent(c.rec.cut, "", "  "); err == nil {
		_ = c.h.redactor.WriteFile(c.h.cfg.EvidenceDir, s07File(c.rec.Variant, "cut.json"), append(data, '\n'))
	}
}

func errString(err error) string {
	if err == nil {
		return "no rows"
	}
	return err.Error()
}

var _ faults.Fault = (*netfault.Partition)(nil)

// moveS07Master moves the master off the first member before
// higher-rank-isolated, as POC-51 did on Kind: it deletes the master's Pod
// gracefully, with its UID as a precondition, so that its preStop stops
// heartbeat and CUBRID, not the run, promotes another member. Within
// s07MoveBound it waits until another member is the active master, the
// deleted member is back in a new Pod as a slave, every member Pod is Ready,
// the starting conditions hold and status names the new master as current
// and last resolved primary. It returns the slaves then; otherwise an error
// that names what did not hold, which blocks S07 before anything is cut.
//
// It deletes nothing unless the master is the first member, the run's
// namespace guard passes, and the Pod is controlled by the run's HA
// StatefulSet. A write quarantine seen during the move is recorded; one that
// only a person may release blocks at once.
func (h *haRun) moveS07Master(ctx context.Context, rec *s07Record) ([]string, error) {
	move := &s07MasterMove{From: rec.Master}
	rec.MasterMove = move
	if memberOrdinal(move.From) != 0 {
		return nil, fmt.Errorf("the master %s is not the first member: there is no master to move", move.From)
	}
	if err := h.guard(ctx); err != nil {
		return nil, fmt.Errorf("check the run namespace before deleting the master Pod %s: %w", move.From, err)
	}
	pod, err := h.s07RunMember(ctx, move.From)
	if err != nil {
		return nil, err
	}
	uid := pod.UID
	move.FromUID = string(uid)
	if err := h.Client.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil {
		return nil, fmt.Errorf("delete the master Pod %s to move the master off it: %w", move.From, err)
	}
	deleted := time.Now().UTC()
	move.DeletedAt = &deleted
	rec.note("master_pod_deleted", move.From)
	deadline := time.Now().Add(s07MoveBound)
	for {
		state, members, unmet, err := h.s07MoveUnmet(ctx, rec)
		if err != nil {
			return nil, err
		}
		if unmet == "" {
			settled := time.Now().UTC()
			move.To, move.SettledAt, move.Unmet, move.Members = state.Master, &settled, "", members
			rec.Master = state.Master
			rec.note("master_moved", move.From+" -> "+state.Master)
			return state.Slaves, nil
		}
		move.Unmet = unmet
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the master did not move off %s within %s: %s", move.From, s07MoveBound, unmet)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(h.cfg.Poll):
		}
	}
}

// s07RunMember reads a member Pod and refuses one that the run's HA
// StatefulSet, itself controlled by the run's HA cluster, does not control.
func (h *haRun) s07RunMember(ctx context.Context, name string) (*corev1.Pod, error) {
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		return nil, fmt.Errorf("read the HA cluster: %w", err)
	}
	sts := &appsv1.StatefulSet{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), sts); err != nil {
		return nil, fmt.Errorf("read the HA StatefulSet: %w", err)
	}
	if !controllerOwner(sts, "CubridCluster", cluster.UID) {
		return nil, errors.New("the database StatefulSet does not belong to the HA cluster")
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, pod); err != nil {
		return nil, fmt.Errorf("read the master Pod %s: %w", name, err)
	}
	if !controllerOwner(pod, "StatefulSet", sts.UID) {
		return nil, fmt.Errorf("pod %s does not belong to the HA StatefulSet: it is not deleted", name)
	}
	return pod, nil
}

// noteS07MoveQuarantine records the first write quarantine seen during the
// move of the master, and its release, so that one released before the move
// settles is kept; a later one of another generation is noted. One that only
// a person may release is returned as an error: the move cannot settle.
func noteS07MoveQuarantine(rec *s07Record, q *databasev1alpha1.WriteQuarantine, now time.Time) error {
	move := rec.MasterMove
	switch {
	case q != nil && move.Quarantine == nil:
		move.Quarantine = &s07MoveQuarantine{Reason: string(q.Reason), GenerationID: q.GenerationID, SeenAt: now}
		rec.note("master_move_quarantine_seen", string(q.Reason)+" "+q.GenerationID)
	case q != nil && q.GenerationID != move.Quarantine.GenerationID &&
		!slices.Contains(move.LaterGenerations, q.GenerationID):
		move.LaterGenerations = append(move.LaterGenerations, q.GenerationID)
		rec.note("master_move_quarantine_seen", string(q.Reason)+" "+q.GenerationID)
	case q == nil && move.Quarantine != nil && move.Quarantine.ReleasedAt == nil:
		move.Quarantine.ReleasedAt = &now
		rec.note("master_move_quarantine_released", move.Quarantine.GenerationID)
	}
	if q != nil && manualOnly(string(q.Reason)) {
		return fmt.Errorf("moving the master off %s started a %s quarantine, which only a person may release",
			move.From, q.Reason)
	}
	return nil
}

// s07MoveUnmet reads whether the master has moved off the first member: what
// does not hold yet, or the roles and the members' identities once all does.
func (h *haRun) s07MoveUnmet(ctx context.Context, rec *s07Record) (*startingState, []imageIdentity, string,
	error) {
	move := rec.MasterMove
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName}, cluster); err != nil {
		if retryable(err) {
			return nil, nil, "the HA cluster" + unread + err.Error(), nil
		}
		return nil, nil, "", err
	}
	if err := noteS07MoveQuarantine(rec, cluster.Status.WriteQuarantine, time.Now().UTC()); err != nil {
		return nil, nil, "", err
	}
	pod := &corev1.Pod{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: move.From}, pod)
	switch {
	case apierrors.IsNotFound(err):
		return nil, nil, move.From + " has no Pod yet", nil
	case err != nil && retryable(err):
		return nil, nil, move.From + unread + err.Error(), nil
	case err != nil:
		return nil, nil, "", err
	case string(pod.UID) == move.FromUID:
		return nil, nil, move.From + " still has the Pod that was deleted", nil
	}
	members, unmet, err := h.memberIdentities(ctx, cluster)
	if err != nil || unmet != "" {
		return nil, nil, unmet, err
	}
	state, unmet, err := h.roles(ctx)
	switch {
	case err != nil || unmet != "":
		return nil, nil, unmet, err
	case state.Master == move.From:
		return nil, nil, "CUBRID reports " + move.From + " as the master again", nil
	}
	if unmet, err := clusterReady(cluster); err != nil || unmet != "" {
		return nil, nil, unmet, err
	}
	if q := cluster.Status.WriteQuarantine; q != nil {
		return nil, nil, fmt.Sprintf("a write quarantine (%s) is in force", q.Reason), nil
	}
	if s := cluster.Status; s.CurrentPrimary != state.Master || s.LastResolvedPrimary != state.Master {
		return nil, nil, fmt.Sprintf("CUBRID reports the master %s, status.currentPrimary %q and "+
			"lastResolvedPrimary %q", state.Master, s.CurrentPrimary, s.LastResolvedPrimary), nil
	}
	return state, members, "", nil
}
