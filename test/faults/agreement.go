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

package faults

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// ConditionState is one Condition of the CubridCluster as one read returned it.
type ConditionState struct {
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// MemberStatus is one entry of status.instances[].
type MemberStatus struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// LastObserved is the member's lastObservedTime; zero when it is not set.
	LastObserved time.Time `json:"lastObservedTime"`
}

// ConditionSample is one read of the CubridCluster's status.
type ConditionSample struct {
	ReadStart      time.Time                 `json:"readStart"`
	ReadEnd        time.Time                 `json:"readEnd"`
	Conditions     map[string]ConditionState `json:"conditions"`
	CurrentPrimary string                    `json:"currentPrimary"`
	// LastObserved is status.lastObservedTime; zero when it is not set.
	LastObserved time.Time      `json:"lastObservedTime"`
	Instances    []MemberStatus `json:"instances"`
	// RWAvailable is the availableReplicas of the read-write Broker
	// Deployment, read right after the status; nil when it was not read.
	RWAvailable *int `json:"rwAvailable,omitempty"`
}

// RoleSweep is one round of asking CUBRID on every member for its role,
// with cubrid heartbeat status. A member that did not answer, or answered
// with neither an active master nor a standby slave, has the role "".
type RoleSweep struct {
	Start time.Time         `json:"start"`
	End   time.Time         `json:"end"`
	Roles map[string]string `json:"roles"`
	// NodeStates is the state heartbeat gives each member's own node
	// ("current <member>, state <state>"), whatever its server's state; ""
	// when the member did not answer.
	NodeStates map[string]string `json:"nodeStates,omitempty"`
}

// Outcomes of an SQL operation, as the workload client records them.
const (
	SQLAcknowledged = "acknowledged"
	SQLFailed       = "failed"
	SQLUnknown      = "unknown"
)

// SQLEvent is the outcome of one client operation.
type SQLEvent struct {
	T time.Time `json:"t"`
	// Write is true for an operation that commits a change.
	Write   bool   `json:"write"`
	Outcome string `json:"outcome"`
}

// RowCounts is one count of the workload's ledger rows on some members, each
// read on the member itself; a member whose count could not be read is
// missing.
type RowCounts struct {
	Start time.Time      `json:"start"`
	End   time.Time      `json:"end"`
	Rows  map[string]int `json:"rows"`
	// Errors says, for each member whose count could not be read, why.
	Errors map[string]string `json:"errors,omitempty"`
}

// Observations are everything a run recorded that the Conditions are
// compared with.
type Observations struct {
	Samples []ConditionSample
	Sweeps  []RoleSweep
	Rows    []RowCounts
	SQL     []SQLEvent
	Steps   []Step
}

// RuleKind says how a rule is judged.
type RuleKind string

const (
	// Never is a combination of Conditions that one status read never
	// holds, because the Operator sets them together.
	Never RuleKind = "never"
	// Tolerance compares the Conditions with another source, and counts a
	// difference only when it lasted longer than the rule's tolerance.
	Tolerance RuleKind = "tolerance"
	// Report is recorded for a reader and never fails a run.
	Report RuleKind = "report"
)

// Rule is one comparison of the Conditions with what was observed.
type Rule struct {
	ID        string
	Kind      RuleKind
	Tolerance time.Duration
	// Says is the rule in words, for agreement.txt.
	Says  string
	check func(Observations) finding
	// optional, when set, says why the rule's evidence is not required: a
	// rule that could not be judged leaves a note instead of blocking the
	// scenario, while a disagreement it finds still fails it.
	optional string
}

// finding is what one rule found.
type finding struct {
	disagreements, notes []string
	// evaluated holds the start of every status read the rule could judge.
	evaluated []time.Time
	// considered is how many status reads the rule needs half of for a
	// verdict; zero means all of them.
	considered int
	// afterOutcome marks a rule that judges only the reads after the outcome,
	// so it does not need a judged read between the fault and the outcome.
	afterOutcome bool
	// inWait marks a rule that judges only the reads of the wait for the
	// outcome, so it does not need a judged read after the outcome.
	inWait bool
	// notJudged says why the rule could not be judged at all: the evidence
	// it needs is missing.
	notJudged string
}

// Agreement is what CheckAgreement found. Each line starts with the ID of
// the rule that found it; a disagreement starts with the rule's kind before
// that, "never: " or "tolerance: ".
type Agreement struct {
	Disagreements []string
	Notes         []string
	// NotJudged lists the rules that could not be judged at all, with why.
	// The comparison then has no verdict: the scenario is blocked, because
	// evidence it requires is missing (docs/testing/scenario-contract.md,
	// "Results").
	NotJudged []string
	// Evaluated is how many status reads each Never or Tolerance rule could
	// judge.
	Evaluated map[string]int
}

// Reading the HA status. staleAfter is the reader rule of
// docs/observability.md ("Freshness"): a status whose lastObservedTime is more
// than 60 seconds old is stale. The times are written in whole seconds, so one
// second more is allowed. roleAnswerAge is roleObservationTTL in
// internal/controller/haresolve.go: the role answers an observation rests on
// may be that much older than the observation. samplingSlack covers the
// two-second sampling and the time one round of reads takes.
const (
	staleAfter    = 60*time.Second + time.Second
	roleAnswerAge = 15 * time.Second
	samplingSlack = 5 * time.Second
)

// AgreementTolerance is how long a Condition may lag behind another source
// before the difference is a disagreement: the age of a status that is not
// stale, the age of the role answers behind it, and the sampling.
const AgreementTolerance = staleAfter - time.Second + roleAnswerAge + samplingSlack

// brokerTolerance is how long the Broker Conditions may lag behind the
// read-write Broker Deployment. The status staleness above does not apply to
// them: the Operator watches the Deployments it owns, so a change of their
// available Pods starts a reconcile at once. That reconcile asks the members
// for their roles first, which takes up to 5 seconds, and a conflicting write
// of a Broker object keeps the Broker Conditions as they were for up to 30
// seconds (conflictPatience in internal/controller/cubridcluster_controller.go)
// before it is reported. The sampling adds samplingSlack.
const brokerTolerance = 5*time.Second + 30*time.Second + samplingSlack

// readingGap is the longest time between two readings of the Deployment that
// still counts as having watched it: three sampling periods of 2 seconds. In a
// longer gap the Deployment may have had no available Pod without any reading
// showing it.
const readingGap = 6 * time.Second

// The least evidence a Never or Tolerance rule needs for a verdict. It must
// have judged at least minEvaluatedReads status reads, and at least half of
// all the reads, so that one read without the evidence in a run of many
// leaves a note while a rule that could judge few of them is not judged.
// When the timeline marks the fault (fault_issued) and the outcome
// (outcome_reached), at least one judged read must fall between the two and
// one after the outcome, in the stable period: a rule that judged only reads
// before the fault, or none while the fault acted, did not test it.
const minEvaluatedReads = 3

// maxLines is how many findings of one rule are written out; the rest are
// counted.
const maxLines = 5

// Rule IDs. They name the rule in agreement.txt and in summary.json.
const (
	ruleRoutingNeedsWriteEndpoint      = "routing-ready-needs-write-endpoint"
	ruleRoutingWithoutPrimary          = "routing-ready-without-resolved-primary"
	ruleHAReadyNeedsPrimary            = "ha-ready-needs-resolved-primary"
	ruleBrokersNeedEndpoints           = "broker-ready-needs-both-endpoints"
	rulePrimaryNeedsEveryAnswer        = "resolved-primary-needs-every-member-answer"
	ruleReplicationNamesSlaves         = "replication-unhealthy-names-slaves"
	rulePrimaryIsConfirmedMaster       = "resolved-primary-is-heartbeat-master"
	ruleWriteEndpointFollowsDeployment = "write-endpoint-follows-rw-deployment"
	ruleWriteEndpointAllBrokersLost    = "write-endpoint-after-all-rw-brokers-lost"
	ruleWritesWithoutRouting           = "writes-acknowledged-without-routing"
	ruleRoutingWhileWritesFail         = "routing-ready-while-writes-fail"
	ruleS03PrimaryWasMaster            = "resolved-primary-was-a-heartbeat-master"
	ruleS03PrimaryAfterOutcome         = "resolved-primary-is-the-new-master-after-the-outcome"
	ruleS03RoutingNeedsPrimary         = "routing-ready-needs-resolved-primary"
	ruleHAReadyFollowsReplication      = "ha-ready-not-true-while-replication-unhealthy"
	ruleStallMatchesRows               = "stall-reported-while-the-slave-lacks-rows"
	ruleStallSpareNotNamed             = "healthy-slave-never-named-unhealthy"
	ruleStallRecoveryLag               = "replication-healthy-after-the-rows-match"
	ruleS06PrimaryAfterReturn          = "resolved-primary-is-the-master-after-the-operator-returned"
	ruleS06WritesWhileStale            = "writes-while-the-status-was-stale"
)

// The Conditions the rules read.
const (
	condPrimaryResolved    = "PrimaryResolved"
	condHAReady            = "HAReady"
	condRoutingReady       = "RoutingReady"
	condReplicationHealthy = "ReplicationHealthy"
	condBrokerReady        = "BrokerReady"
	condWriteEndpointReady = "WriteEndpointReady"
	condReadEndpointReady  = "ReadEndpointReady"
)

const condTrue, condFalse = "True", "False"

// Roles as status.instances and RoleSweep name them.
const roleMaster, roleSlave = "master", "slave"

// CheckAgreement applies the rules to the observations. What a Report rule
// finds is a note, never a disagreement, and a Report rule is never counted
// as not judged.
func CheckAgreement(obs Observations, rules []Rule) Agreement {
	var a Agreement
	for _, r := range rules {
		f := r.check(obs)
		if r.Kind == Report {
			f.notes = append(f.disagreements, f.notes...)
			f.disagreements = nil
			if f.notJudged != "" {
				f.notes = append(f.notes, "not judged: "+f.notJudged)
				f.notJudged = ""
			}
		}
		if r.Kind != Report {
			if a.Evaluated == nil {
				a.Evaluated = map[string]int{}
			}
			a.Evaluated[r.ID] = len(f.evaluated)
			if f.notJudged == "" {
				considered := len(obs.Samples)
				if f.considered > 0 {
					considered = f.considered
				}
				f.notJudged = insufficient(f.evaluated, considered, obs.Steps, f.afterOutcome, f.inWait)
			}
			if f.notJudged != "" && r.optional != "" {
				f.notes = append(f.notes, "evidence not required ("+r.optional+"): "+f.notJudged)
				f.notJudged = ""
			}
		}
		a.Disagreements = append(a.Disagreements, prefixed(string(r.Kind)+": "+r.ID, f.disagreements)...)
		a.Notes = append(a.Notes, prefixed(r.ID, f.notes)...)
		if f.notJudged != "" {
			a.NotJudged = append(a.NotJudged, r.ID+": "+f.notJudged)
		}
	}
	return a
}

// insufficient says why the judged reads are too few for a verdict, or "".
func insufficient(evaluated []time.Time, reads int, steps []Step, afterOutcome, inWait bool) string {
	if len(evaluated) < minEvaluatedReads || 2*len(evaluated) < reads {
		return fmt.Sprintf("only %d of %d status reads could be judged; at least %d and at least half are needed",
			len(evaluated), reads, minEvaluatedReads)
	}
	var faultAt, outcomeAt time.Time
	for _, st := range steps {
		switch st.Kind {
		case "fault_issued":
			faultAt = st.T
		case "outcome_reached":
			outcomeAt = st.T
		}
	}
	in := func(from, to time.Time) bool {
		return slices.ContainsFunc(evaluated, func(t time.Time) bool {
			return !t.Before(from) && (to.IsZero() || t.Before(to))
		})
	}
	switch {
	case !afterOutcome && !faultAt.IsZero() && !in(faultAt, outcomeAt):
		return "no status read between the fault and the outcome could be judged"
	case !inWait && !outcomeAt.IsZero() && !in(outcomeAt, time.Time{}):
		return "no status read in the stable period after the outcome could be judged"
	}
	return ""
}

// prefixed puts the rule's ID before each line, and keeps maxLines of them.
func prefixed(id string, lines []string) []string {
	out := make([]string, 0, min(len(lines), maxLines+1))
	for i, l := range lines {
		if i == maxLines {
			out = append(out, fmt.Sprintf("%s: and %d more", id, len(lines)-maxLines))
			break
		}
		out = append(out, id+": "+l)
	}
	return out
}

// HAReportRules are reported for every status read of an HA cluster and
// never fail a run.
//
// RoutingReady=True is not required to come with PrimaryResolved=True: after a
// conflicting write of a Broker object the Broker Conditions are kept as they
// were while the primary is resolved again (brokerTier in
// internal/controller/broker.go), so for one read they may disagree.
func HAReportRules() []Rule {
	return []Rule{
		{
			ID: ruleRoutingWithoutPrimary, Kind: Report,
			Says: "RoutingReady=True while PrimaryResolved is not True; possible for one read after a conflicting " +
				"write of a Broker object, which leaves the Broker Conditions as they were",
			check: perRead(nil, func(s ConditionSample) (string, string) {
				c, ok := s.Conditions[condPrimaryResolved]
				if !isTrue(s, condRoutingReady) || !ok || c.Status == condTrue {
					return "", ""
				}
				return fmt.Sprintf("read at %s: RoutingReady=True, PrimaryResolved=%s (%s)",
					stamp(s.ReadStart), c.Status, c.Reason), ""
			}),
		},
	}
}

// NeverRules are combinations of Conditions that one status read of an HA
// cluster never holds, because the Operator sets each pair in the same step.
// HAReady=False is allowed with PrimaryResolved=True: an unhealthy
// replication may make HAReady False on its own.
func NeverRules() []Rule {
	return []Rule{
		{
			ID: ruleRoutingNeedsWriteEndpoint, Kind: Never,
			Says: "RoutingReady=True comes with WriteEndpointReady=True in the same read: routing is ready only " +
				"when a read-write Broker Pod is available, and both are set from the same count",
			check: perRead([]string{condRoutingReady, condWriteEndpointReady}, func(s ConditionSample) (string, string) {
				if !isTrue(s, condRoutingReady) {
					return "", ""
				}
				return needTrue(s, condWriteEndpointReady)
			}),
		},
		{
			ID: ruleHAReadyFollowsReplication, Kind: Never,
			Says: "HAReady is not True while ReplicationHealthy=False in the same read: HAReady takes " +
				"ReplicationHealthy's False in the same pass, and holds an earlier value only when it is not False",
			check: perRead([]string{condHAReady, condReplicationHealthy}, func(s ConditionSample) (string, string) {
				if c := s.Conditions[condReplicationHealthy]; c.Status != condFalse || !isTrue(s, condHAReady) {
					return "", ""
				}
				return fmt.Sprintf("read at %s: HAReady=True while ReplicationHealthy=False (%s)",
					stamp(s.ReadStart), s.Conditions[condReplicationHealthy].Reason), ""
			}),
		},
		{
			ID: ruleHAReadyNeedsPrimary, Kind: Never,
			Says: "HAReady=True comes with PrimaryResolved=True in the same read; HAReady=False with " +
				"PrimaryResolved=True is allowed",
			check: perRead([]string{condHAReady, condPrimaryResolved}, func(s ConditionSample) (string, string) {
				if !isTrue(s, condHAReady) {
					return "", ""
				}
				return needTrue(s, condPrimaryResolved)
			}),
		},
		{
			ID: ruleBrokersNeedEndpoints, Kind: Never,
			Says: "BrokerReady=True comes with WriteEndpointReady=True and ReadEndpointReady=True in the same read",
			check: perRead([]string{condBrokerReady, condWriteEndpointReady, condReadEndpointReady},
				func(s ConditionSample) (string, string) {
					if !isTrue(s, condBrokerReady) {
						return "", ""
					}
					if bad, skip := needTrue(s, condWriteEndpointReady); bad != "" || skip != "" {
						return bad, skip
					}
					return needTrue(s, condReadEndpointReady)
				}),
		},
		{
			ID: rulePrimaryNeedsEveryAnswer, Kind: Never,
			Says: "PrimaryResolved=True comes with every member's lastObservedTime equal to the cluster's: a " +
				"primary is resolved only from an observation in which every member answered",
			check: perRead([]string{condPrimaryResolved}, func(s ConditionSample) (string, string) {
				if !isTrue(s, condPrimaryResolved) {
					return "", ""
				}
				if s.LastObserved.IsZero() || len(s.Instances) == 0 {
					return "", "the read has no status.lastObservedTime or no instances"
				}
				var older []string
				for _, in := range s.Instances {
					if in.LastObserved.Before(s.LastObserved) {
						older = append(older, fmt.Sprintf("%s (%s)", in.Name, stampOrUnset(in.LastObserved)))
					}
				}
				if len(older) == 0 {
					return "", ""
				}
				return fmt.Sprintf("read at %s: PrimaryResolved=True, observation at %s, but members answered "+
					"earlier: %s", stamp(s.ReadStart), stamp(s.LastObserved), strings.Join(older, ", ")), ""
			}),
		},
		{
			ID: ruleReplicationNamesSlaves, Kind: Never,
			Says: "every member ReplicationHealthy=False names is a slave in status.instances of the same read",
			check: perRead([]string{condReplicationHealthy}, func(s ConditionSample) (string, string) {
				c, ok := s.Conditions[condReplicationHealthy]
				if !ok || c.Status != condFalse {
					return "", ""
				}
				names, ok := namedMembers(c.Message)
				if !ok {
					return "", "the message of ReplicationHealthy=False names no member"
				}
				roles := make(map[string]string, len(s.Instances))
				for _, in := range s.Instances {
					roles[in.Name] = in.Role
				}
				var others []string
				for _, name := range names {
					if role := roles[name]; role != roleSlave {
						others = append(others, fmt.Sprintf("%s (%s)", name, orMissing(role, role != "")))
					}
				}
				if len(others) == 0 {
					return "", ""
				}
				return fmt.Sprintf("read at %s: ReplicationHealthy=False (%s) names members that are not slaves: %s",
					stamp(s.ReadStart), c.Reason, strings.Join(others, ", ")), ""
			}),
		},
	}
}

// S05Rules are the rules of a Broker failure (S05), which never moves the
// master: startingMaster is the master before the fault. brokerLeft is true in
// the variants that leave a read-write Broker Pod available (one Pod deleted,
// or one Broker process killed).
func S05Rules(startingMaster string, brokerLeft bool) []Rule {
	rules := []Rule{{
		ID: rulePrimaryIsConfirmedMaster, Kind: Tolerance, Tolerance: AgreementTolerance,
		Says: "while CUBRID's heartbeat reports the starting master " + startingMaster + " as the only master " +
			"throughout the run, a status that is not stale never resolves another member as the primary; " +
			"a read counts when a heartbeat sweep within the tolerance before it confirmed that master",
		check: func(obs Observations) finding { return checkPrimaryIsConfirmedMaster(obs, startingMaster) },
	}}
	if brokerLeft {
		rules = append(rules, Rule{
			ID: ruleWriteEndpointFollowsDeployment, Kind: Tolerance, Tolerance: brokerTolerance,
			Says: "the status never says WriteEndpointReady=False (NoBrokerAvailable) for longer than the " +
				"tolerance while every reading of the read-write Broker Deployment in the tolerance before it, " +
				"none more than " + readingGap.String() + " apart, showed an available Pod; a read whose " +
				"Condition changed within the tolerance, or whose readings leave a gap, is not judged",
			check: checkWriteEndpointFollowsDeployment,
		})
	} else {
		rules = append(rules, Rule{
			ID: ruleWriteEndpointAllBrokersLost, Kind: Report,
			Says: "when every read-write Broker Pod is deleted, when WriteEndpointReady was not True and when " +
				"the Deployment had no available Pod",
			check: reportAllBrokersLost,
		})
	}
	return append(rules, sqlReportRules()...)
}

// sqlReportRules compare the clients' writes with the last status read before
// them. They are reported only: the Conditions are no guarantee about SQL.
func sqlReportRules() []Rule {
	return []Rule{
		{
			ID: ruleWritesWithoutRouting, Kind: Report,
			Says: "writes acknowledged while the last status read before them had RoutingReady or " +
				"PrimaryResolved not True; the Conditions are no guarantee about SQL (docs/observability.md)",
			check: func(obs Observations) finding {
				return reportWrites(obs, func(e SQLEvent, s ConditionSample) bool {
					return e.Outcome == SQLAcknowledged && (!isTrue(s, condRoutingReady) || !isTrue(s, condPrimaryResolved))
				}, "acknowledged while RoutingReady or PrimaryResolved was not True")
			},
		},
		{
			ID: ruleRoutingWhileWritesFail, Kind: Report,
			Says: "writes that failed or ended without a known outcome while the last status read before them " +
				"had RoutingReady=True",
			check: func(obs Observations) finding {
				return reportWrites(obs, func(e SQLEvent, s ConditionSample) bool {
					return (e.Outcome == SQLFailed || e.Outcome == SQLUnknown) && isTrue(s, condRoutingReady)
				}, "failed or ended without a known outcome while RoutingReady was True")
			},
		},
	}
}

func checkPrimaryIsConfirmedMaster(obs Observations, master string) finding {
	if len(obs.Sweeps) == 0 {
		return finding{notJudged: "no heartbeat sweep was recorded"}
	}
	var confirmed []time.Time
	for _, sw := range obs.Sweeps {
		for member, role := range sw.Roles {
			if role == roleMaster && member != master {
				return finding{notJudged: fmt.Sprintf("the heartbeat sweep at %s reports %s as a master, "+
					"so the starting master was not the only master throughout", stamp(sw.Start), member)}
			}
		}
		if sw.Roles[master] == roleMaster {
			confirmed = append(confirmed, sw.Start)
		}
	}
	if len(confirmed) == 0 {
		return finding{notJudged: "no heartbeat answer was recognized as the starting master " + master +
			" being the active master"}
	}
	fresh, notes := freshSamples(obs.Samples)
	var bad []string
	unconfirmed := 0
	var evaluated []time.Time
	for _, s := range fresh {
		confirmedNear := slices.ContainsFunc(confirmed, func(t time.Time) bool {
			return !t.Before(s.ReadStart.Add(-AgreementTolerance)) && !t.After(s.ReadEnd)
		})
		if _, ok := s.Conditions[condPrimaryResolved]; ok && confirmedNear {
			evaluated = append(evaluated, s.ReadStart)
		}
		if !isTrue(s, condPrimaryResolved) || s.CurrentPrimary == master {
			continue
		}
		if !confirmedNear {
			unconfirmed++
			continue
		}
		bad = append(bad, fmt.Sprintf("read at %s: PrimaryResolved=True names %s, while heartbeat confirms %s",
			stamp(s.ReadStart), orMissing(s.CurrentPrimary, s.CurrentPrimary != ""), master))
	}
	if unconfirmed > 0 {
		notes = append(notes, fmt.Sprintf("not judged in %d reads that name another primary: no heartbeat sweep "+
			"within the tolerance before them confirmed the starting master", unconfirmed))
	}
	return finding{disagreements: bad, notes: notes, evaluated: evaluated}
}

// checkWriteEndpointFollowsDeployment compares every read that says no
// read-write Broker is available with the readings of the Deployment. Such a
// read is a disagreement only when the Condition has said so for longer than
// brokerTolerance and every reading over that time, none further than
// readingGap from the next, showed an available Pod. A Condition that changed
// more recently may rest on a moment without an available Pod that no reading
// caught, and is not judged.
//
// The reads that report no Broker since the same lastTransitionTime are one
// period. A period in which that lasted longer than brokerTolerance needs a
// verdict: at least one of its reads must have a complete window of
// Deployment readings, or the rule is not judged. Other reads of the period
// whose window has a gap are only noted. A read counts as judged when it
// carries WriteEndpointReady and a reading of the Deployment.
func checkWriteEndpointFollowsDeployment(obs Observations) finding {
	var readings []reading
	var empty []time.Time
	for _, s := range obs.Samples {
		if s.RWAvailable != nil {
			readings = append(readings, reading{s.ReadEnd, *s.RWAvailable > 0})
			if *s.RWAvailable == 0 {
				empty = append(empty, s.ReadEnd)
			}
		}
	}
	if len(readings) == 0 {
		return finding{notJudged: "the read-write Broker Deployment was not read"}
	}
	slices.SortStableFunc(readings, func(a, b reading) int { return a.t.Compare(b.t) })
	var notes []string
	if len(empty) > 0 {
		notes = append(notes, fmt.Sprintf("the read-write Broker Deployment had none of its Pods available in "+
			"%d readings, first at %s", len(empty), stamp(empty[0])))
	}
	var bad []string
	var evaluated []time.Time
	// undecided holds, by the time a period began, whether none of its reads
	// that need a verdict could be decided yet.
	undecided := map[time.Time]bool{}
	uncovered, gaps := 0, 0
	for _, s := range obs.Samples {
		c, ok := s.Conditions[condWriteEndpointReady]
		if ok && s.RWAvailable != nil {
			evaluated = append(evaluated, s.ReadStart)
		}
		if !ok || c.Status != condFalse || c.Reason != "NoBrokerAvailable" {
			continue
		}
		if !c.LastTransitionTime.IsZero() && s.ReadStart.Sub(c.LastTransitionTime) <= brokerTolerance {
			continue // within the tolerance of the change
		}
		period := c.LastTransitionTime
		if _, seen := undecided[period]; !seen {
			undecided[period] = true
		}
		if period.IsZero() {
			continue // without a transition time the period cannot be judged
		}
		w := windowBefore(readings, s.ReadStart.Add(-brokerTolerance), s.ReadEnd, readingGap)
		switch {
		case !w.covered:
			uncovered++
			continue
		case w.gap:
			gaps++
			continue
		}
		undecided[period] = false
		if w.allHold {
			bad = append(bad, fmt.Sprintf("read at %s: WriteEndpointReady=False (NoBrokerAvailable) since %s, "+
				"while all %d readings of the Deployment since %s showed an available Pod",
				stamp(s.ReadStart), stamp(c.LastTransitionTime), w.seen, stamp(w.from)))
		}
	}
	var open []string
	for _, period := range slices.SortedFunc(maps.Keys(undecided), time.Time.Compare) {
		if undecided[period] {
			open = append(open, stampOrUnset(period))
		}
	}
	for _, n := range []struct {
		count int
		why   string
	}{
		{uncovered, "the Deployment was not read for the whole tolerance before them"},
		{gaps, fmt.Sprintf("the readings of the Deployment before them leave a gap of more than %s", readingGap)},
	} {
		if n.count > 0 {
			notes = append(notes, fmt.Sprintf("not judged in %d reads that say no Broker is available: %s",
				n.count, n.why))
		}
	}
	f := finding{disagreements: bad, notes: notes, evaluated: evaluated}
	if len(open) > 0 {
		f.notJudged = fmt.Sprintf("no read of the periods that reported no Broker for longer than the tolerance, "+
			"since %s, has a complete window of Deployment readings", strings.Join(open, ", "))
	}
	return f
}

// reading is one reading another source gave: of the read-write Broker
// Deployment, holds is true when it showed an available Pod; of the row
// counts, when the stalled slave held at least the master's rows.
type reading struct {
	t     time.Time
	holds bool
}

// window is what the readings over a tolerance window show.
type window struct {
	// covered is false when no reading was taken at or before its start.
	covered bool
	// gap is true when two readings, or the last one and the window's end,
	// are more than readingGap apart.
	gap bool
	// allHold is true when every reading in it holds.
	allHold bool
	seen    int
	// from is the time of the first reading used.
	from time.Time
}

// windowBefore looks at the sorted readings from the last one at or before
// from to the last one at or before end. Two readings, or the last one and
// end, further apart than maxGap are a gap.
func windowBefore(readings []reading, from, end time.Time, maxGap time.Duration) window {
	first := -1
	for i, r := range readings {
		if !r.t.After(from) {
			first = i
		}
	}
	if first < 0 {
		return window{}
	}
	w := window{covered: true, allHold: true, from: readings[first].t}
	last := readings[first].t
	for i := first; i < len(readings) && !readings[i].t.After(end); i++ {
		if i > first && readings[i].t.Sub(readings[i-1].t) > maxGap {
			w.gap = true
		}
		w.seen++
		w.allHold = w.allHold && readings[i].holds
		last = readings[i].t
	}
	w.gap = w.gap || end.Sub(last) > maxGap
	return w
}

func reportAllBrokersLost(obs Observations) finding {
	var notReady, empty []time.Time
	for _, s := range obs.Samples {
		if c, ok := s.Conditions[condWriteEndpointReady]; ok && c.Status != condTrue {
			notReady = append(notReady, s.ReadStart)
		}
		if s.RWAvailable != nil && *s.RWAvailable == 0 {
			empty = append(empty, s.ReadEnd)
		}
	}
	span := func(what string, ts []time.Time) string {
		if len(ts) == 0 {
			return what + " in no read"
		}
		return fmt.Sprintf("%s in %d reads, from %s to %s%s", what, len(ts), stamp(ts[0]), stamp(ts[len(ts)-1]),
			sinceFault(obs.Steps, ts[0]))
	}
	return finding{notes: []string{span("WriteEndpointReady was not True", notReady) + "; " +
		span("the read-write Broker Deployment had no available Pod", empty)}}
}

// reportWrites counts the writes that, compared with the last status read that
// ended before them, match. A write whose last read was stale is not
// compared: that status says nothing of the time of the write.
func reportWrites(obs Observations, match func(SQLEvent, ConditionSample) bool, what string) finding {
	samples := byReadStart(obs.Samples)
	count := 0
	var first time.Time
	for _, e := range obs.SQL {
		if !e.Write {
			continue
		}
		last := lastReadBefore(samples, e.T)
		if last < 0 || isStale(samples[last]) || !match(e, samples[last]) {
			continue
		}
		// The events of several clients may be joined one after the other.
		if count == 0 || e.T.Before(first) {
			first = e.T
		}
		count++
	}
	if count == 0 {
		return finding{}
	}
	return finding{notes: []string{fmt.Sprintf("%d writes %s, the first at %s%s", count, what, stamp(first),
		sinceFault(obs.Steps, first))}}
}

// perRead applies a check to every status read. check returns a finding, or
// why the read could not be judged; reads that could not be judged are
// counted in one note per reason. Without any read the rule is not judged.
//
// A read counts as judged when it carries every Condition in needs and check
// gave no reason to skip it.
func perRead(needs []string, check func(ConditionSample) (bad, skip string)) func(Observations) finding {
	return func(obs Observations) finding {
		if len(obs.Samples) == 0 {
			return finding{notJudged: "no status read was recorded"}
		}
		var bad []string
		var evaluated []time.Time
		skipped := map[string]int{}
		for _, s := range obs.Samples {
			b, skip := check(s)
			if b != "" {
				bad = append(bad, b)
			}
			if skip == "" && hasAll(s, needs) {
				evaluated = append(evaluated, s.ReadStart)
			}
			if skip != "" {
				skipped[skip]++
			}
		}
		notes := make([]string, 0, len(skipped))
		for _, why := range slices.Sorted(maps.Keys(skipped)) {
			notes = append(notes, fmt.Sprintf("not judged in %d reads: %s", skipped[why], why))
		}
		return finding{disagreements: bad, notes: notes, evaluated: evaluated}
	}
}

func hasAll(s ConditionSample, conditions []string) bool {
	for _, c := range conditions {
		if _, ok := s.Conditions[c]; !ok {
			return false
		}
	}
	return true
}

// isStale reports whether the read's status is stale: its
// status.lastObservedTime is unset or more than staleAfter old.
func isStale(s ConditionSample) bool {
	return s.LastObserved.IsZero() || s.ReadStart.Sub(s.LastObserved) > staleAfter
}

// byReadStart returns the reads in the order of their start.
func byReadStart(samples []ConditionSample) []ConditionSample {
	sorted := slices.Clone(samples)
	slices.SortStableFunc(sorted, func(a, b ConditionSample) int { return a.ReadStart.Compare(b.ReadStart) })
	return sorted
}

// lastReadBefore returns the index of the last of the sorted reads that ended
// before t, or -1.
func lastReadBefore(sorted []ConditionSample, t time.Time) int {
	last := -1
	for i, s := range sorted {
		if s.ReadEnd.After(t) {
			break
		}
		last = i
	}
	return last
}

// freshSamples returns the reads, in the order of their start, whose status
// is not stale, and a note on the ones that were.
func freshSamples(samples []ConditionSample) ([]ConditionSample, []string) {
	var fresh []ConditionSample
	stale := 0
	for _, s := range samples {
		if isStale(s) {
			stale++
			continue
		}
		fresh = append(fresh, s)
	}
	slices.SortStableFunc(fresh, func(a, b ConditionSample) int { return a.ReadStart.Compare(b.ReadStart) })
	if stale == 0 {
		return fresh, nil
	}
	return fresh, []string{fmt.Sprintf("%d reads were stale (status.lastObservedTime unset or more than 60 "+
		"seconds old) and were not compared", stale)}
}

func isTrue(s ConditionSample, condition string) bool {
	return s.Conditions[condition].Status == condTrue
}

// needTrue returns a finding when the condition is in the read and not True,
// and a reason to skip the read when it is not in it.
func needTrue(s ConditionSample, condition string) (bad, skip string) {
	c, ok := s.Conditions[condition]
	switch {
	case !ok:
		return "", "the read has no " + condition
	case c.Status != condTrue:
		return fmt.Sprintf("read at %s: %s=%s (%s)", stamp(s.ReadStart), condition, c.Status, c.Reason), ""
	}
	return "", ""
}

// sinceFault says how long after the fault was issued t was, or nothing when
// the timeline has no fault.
func sinceFault(steps []Step, t time.Time) string {
	for _, st := range steps {
		if st.Kind == "fault_issued" {
			return fmt.Sprintf(" (%s after the fault was issued)", t.Sub(st.T).Round(time.Millisecond))
		}
	}
	return ""
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func stampOrUnset(t time.Time) string {
	if t.IsZero() {
		return "not set"
	}
	return stamp(t)
}

// namedMembers returns the members a ReplicationHealthy message names after
// its last "on: ", and false when it names none.
func namedMembers(message string) ([]string, bool) {
	cut := strings.LastIndex(message, "on: ")
	if cut < 0 {
		return nil, false
	}
	return strings.Split(message[cut+len("on: "):], ", "), true
}

// stalledNaming reports whether the read says ReplicationHealthy=False with
// reason ReplicationStalled and names member.
func stalledNaming(s ConditionSample, member string) bool {
	c := s.Conditions[condReplicationHealthy]
	if c.Status != condFalse || c.Reason != "ReplicationStalled" {
		return false
	}
	names, _ := namedMembers(c.Message)
	return slices.Contains(names, member)
}

// rowReadingGap is the longest time between two row counts that still counts
// as having watched the rows: a few rounds of the sampling, each of which
// runs csql on two members.
const rowReadingGap = 10 * time.Second

// StallRules are the rules of the replication-stall step, in which the slave
// victim applies nothing while the master writes, and healthySlave is the
// other slave. Each compares the reads with the counts of the ledger rows on
// victim and master that the same sampling took.
// Stall describes one run of the replication-stall step.
type Stall struct {
	Victim, Master, HealthySlave string
	// WritesAcknowledged is when the writes after the stall were
	// acknowledged; zero when not known.
	WritesAcknowledged time.Time
	// RepairIssued is when the stalled slave's Pod was deleted; zero when
	// it was not.
	RepairIssued time.Time
}

func StallRules(st Stall) []Rule {
	victim, master, healthySlave := st.Victim, st.Master, st.HealthySlave
	return []Rule{
		{
			// The healthy slave is the former master's replacement Pod. While
			// it rejoins it may report apply failures, and after the victim's
			// repair a stalled copy of the log, so only the claim that it
			// stalled the way the victim did, in the same read, is a
			// disagreement: it applies the writes the victim does not.
			ID: ruleStallSpareNotNamed, Kind: Never,
			Says: "the healthy slave " + healthySlave + " (the former master's replacement Pod) is never named " +
				"with reason ReplicationStalled in a read that names " + victim + " as stalled",
			check: perRead([]string{condReplicationHealthy}, func(s ConditionSample) (string, string) {
				if !stalledNaming(s, victim) || !stalledNaming(s, healthySlave) {
					return "", ""
				}
				return fmt.Sprintf("read at %s: ReplicationStalled names the healthy slave %s with %s",
					stamp(s.ReadStart), healthySlave, victim), ""
			}),
		},
		{
			ID: ruleStallMatchesRows, Kind: Tolerance, Tolerance: AgreementTolerance,
			Says: "a status that is not stale, read after the writes were acknowledged, never names " + victim +
				" as stalled (ReplicationStalled) when every row count in the tolerance before it, none more than " +
				rowReadingGap.String() + " apart, showed it holding at least the rows of the master " + master +
				"; at least one such stalled read must have a row count of the same pass, and the evidence " +
				"limits count only the reads from the writes to the repair of " + victim,
			check: func(obs Observations) finding { return checkStallMatchesRows(obs, st) },
		},
		{
			ID: ruleStallRecoveryLag, Kind: Report,
			Says: "how long after the row counts of " + victim + " matched the master's again ReplicationHealthy " +
				"was True",
			check: func(obs Observations) finding { return reportStallRecoveryLag(obs, victim, master) },
		},
	}
}

// rowReadings are the counts that read both victim and master, in time order;
// a reading holds when victim had at least master's rows.
func rowReadings(obs Observations, victim, master string) []reading {
	var readings []reading
	for _, c := range obs.Rows {
		v, okV := c.Rows[victim]
		m, okM := c.Rows[master]
		if okV && okM {
			readings = append(readings, reading{c.Start, v >= m})
		}
	}
	slices.SortStableFunc(readings, func(a, b reading) int { return a.t.Compare(b.t) })
	return readings
}

// samePass returns the row reading taken within samplingSlack of the end of
// the status read s, and false when there is none.
func samePass(readings []reading, s ConditionSample) (reading, bool) {
	for _, r := range readings {
		if d := r.t.Sub(s.ReadEnd); d >= -samplingSlack && d <= samplingSlack {
			return r, true
		}
	}
	return reading{}, false
}

// checkStallMatchesRows compares the stalled reads with the row counts. The
// Operator judges a stall by log pages, not rows: the failover writes log of
// its own, so the stall can be reported before the step's writes reach the
// master, while both hold the same rows. A stalled read before the writes were
// acknowledged is therefore not compared. While the victim's Pod is replaced
// its rows cannot be counted, so the evidence limits count only the reads from
// the writes to the repair.
func checkStallMatchesRows(obs Observations, st Stall) finding {
	victim, master := st.Victim, st.Master
	failures := rowCountFailures(obs.Rows)
	readings := rowReadings(obs, victim, master)
	if len(readings) == 0 {
		return finding{notJudged: "no row count read both " + victim + " and " + master, notes: failures}
	}
	fresh, notes := freshSamples(obs.Samples)
	notes = append(notes, failures...)
	inComparison := func(t time.Time) bool {
		return !t.Before(st.WritesAcknowledged) && (st.RepairIssued.IsZero() || t.Before(st.RepairIssued))
	}
	var bad []string
	var evaluated []time.Time
	considered, compared, behind, early, soon, uncovered, gaps := 0, 0, 0, 0, 0, 0, 0
	for _, s := range fresh {
		pass, inPass := samePass(readings, s)
		if inComparison(s.ReadStart) {
			considered++
			if _, ok := s.Conditions[condReplicationHealthy]; ok && inPass {
				evaluated = append(evaluated, s.ReadStart)
			}
		}
		if !stalledNaming(s, victim) {
			continue
		}
		if s.ReadStart.Before(st.WritesAcknowledged) {
			early++
			continue
		}
		if inPass {
			compared++
			if !pass.holds {
				behind++
			}
		}
		// Rows counted before the writes say nothing of a stall the writes
		// would show: the window is judged only once it lies after them.
		from := s.ReadStart.Add(-AgreementTolerance)
		if from.Before(st.WritesAcknowledged) {
			soon++
			continue
		}
		w := windowBefore(readings, from, s.ReadEnd, rowReadingGap)
		switch {
		case !w.covered:
			uncovered++
		case w.gap:
			gaps++
		case w.allHold:
			bad = append(bad, fmt.Sprintf("read at %s: ReplicationStalled names %s, while all %d row counts "+
				"since %s showed it holding at least the rows of %s", stamp(s.ReadStart), victim, w.seen,
				stamp(w.from), master))
		}
	}
	if early > 0 {
		notes = append(notes, fmt.Sprintf("%d reads naming %s as stalled before the writes were acknowledged at %s "+
			"were not compared with the rows", early, victim, stamp(st.WritesAcknowledged)))
	}
	if compared == 0 {
		return finding{notJudged: "no read naming " + victim + " as stalled after the writes had a row count of " +
			"the same pass", notes: notes, evaluated: evaluated, considered: considered}
	}
	notes = append(notes, fmt.Sprintf("%d reads naming %s as stalled had a row count of the same pass; in %d of "+
		"them it held fewer rows than %s", compared, victim, behind, master))
	for _, n := range []struct {
		count int
		why   string
	}{
		{soon, "they came less than the tolerance after the writes were acknowledged"},
		{uncovered, "the rows were not counted for the whole tolerance before them"},
		{gaps, fmt.Sprintf("the row counts before them leave a gap of more than %s", rowReadingGap)},
	} {
		if n.count > 0 {
			notes = append(notes, fmt.Sprintf("not compared over the tolerance in %d stalled reads: %s",
				n.count, n.why))
		}
	}
	return finding{disagreements: bad, notes: notes, evaluated: evaluated, considered: considered}
}

// rowCountFailures says, per member, in how many rounds its rows could not be
// counted, and why the first time.
func rowCountFailures(counts []RowCounts) []string {
	failed := map[string]int{}
	first := map[string]string{}
	for _, c := range counts {
		for member, why := range c.Errors {
			if failed[member] == 0 {
				first[member] = fmt.Sprintf("%s at %s", why, stamp(c.Start))
			}
			failed[member]++
		}
	}
	notes := make([]string, 0, len(failed))
	for _, member := range slices.Sorted(maps.Keys(failed)) {
		notes = append(notes, fmt.Sprintf("the rows of %s could not be counted in %d rounds; the first: %s",
			member, failed[member], first[member]))
	}
	return notes
}

// reportStallRecoveryLag finds the first row count, after the first read
// naming victim as stalled, in which victim held the master's rows, and the
// first read from then on with ReplicationHealthy=True.
func reportStallRecoveryLag(obs Observations, victim, master string) finding {
	fresh, _ := freshSamples(obs.Samples)
	first := slices.IndexFunc(fresh, func(s ConditionSample) bool { return stalledNaming(s, victim) })
	if first < 0 {
		return finding{notes: []string{"no read named " + victim + " as stalled"}}
	}
	var caughtUp time.Time
	for _, r := range rowReadings(obs, victim, master) {
		if r.holds && r.t.After(fresh[first].ReadStart) {
			caughtUp = r.t
			break
		}
	}
	if caughtUp.IsZero() {
		return finding{notes: []string{"no row count after the stall showed " + victim + " holding the rows of " + master}}
	}
	for _, s := range fresh[first:] {
		if !s.ReadStart.Before(caughtUp) && isTrue(s, condReplicationHealthy) {
			return finding{notes: []string{fmt.Sprintf("ReplicationHealthy was True %s after the row counts matched "+
				"(rows matched at %s, ReplicationHealthy=True read at %s)",
				s.ReadStart.Sub(caughtUp).Round(time.Second), stamp(caughtUp), stamp(s.ReadStart))}}
		}
	}
	return finding{notes: []string{fmt.Sprintf("the row counts matched at %s, and no later read had "+
		"ReplicationHealthy=True", stamp(caughtUp))}}
}

// S03Rules are the rules of a primary Pod termination (S03), in which CUBRID
// makes newMaster the master. Which member a client's write reached is not
// recorded, so currentPrimary cannot be compared with it; CUBRID's heartbeat
// stands in for it.
func S03Rules(newMaster string) []Rule {
	return append([]Rule{
		{
			// The check S03 made of every status read it took while it
			// waited for the outcome (ADR-0005, section 6): the write
			// endpoint is never offered while the primary is not resolved.
			// It is a Never rule in that wait only, from the confirmed fault
			// to the end of the wait, as that check was. Before and after the
			// wait the pair is reported only, as HAReportRules reports it: a
			// conflicting write of a Broker object can keep the Broker
			// Conditions as they were for one read.
			ID: ruleS03RoutingNeedsPrimary, Kind: Never,
			Says: "RoutingReady=True comes with PrimaryResolved=True in the same read, in every read from the " +
				"confirmed fault to the end of the wait for the outcome; outside the wait it is reported only, " +
				"since a conflicting write of a Broker object can keep the Broker Conditions as they were",
			check: checkRoutingNeedsPrimaryInWait,
		},
		{
			ID: ruleS03PrimaryWasMaster, Kind: Tolerance, Tolerance: AgreementTolerance,
			Says: "a status that is not stale resolves only a member whose own node heartbeat showed as master " +
				"(the state the Instance Manager reports as the role master, whether or not its server is " +
				"active yet) in a sweep within the tolerance before the read",
			check: checkPrimaryWasMaster,
		},
		{
			ID: ruleS03PrimaryAfterOutcome, Kind: Never,
			Says: "from the outcome to the end of the stable period, a read that resolves a primary names " +
				newMaster,
			check: func(obs Observations) finding { return checkPrimaryAfterOutcome(obs, newMaster) },
		},
	}, sqlReportRules()...)
}

// nodeMaster reports whether the sweep shows member's own node as master.
func nodeMaster(sw RoleSweep, member string) bool {
	if sw.NodeStates != nil {
		return sw.NodeStates[member] == roleMaster
	}
	return sw.Roles[member] == roleMaster
}

func checkPrimaryWasMaster(obs Observations) finding {
	if len(obs.Sweeps) == 0 {
		return finding{notJudged: "no heartbeat sweep was recorded"}
	}
	fresh, notes := freshSamples(obs.Samples)
	var bad []string
	var evaluated []time.Time
	unswept := 0
	for _, s := range fresh {
		if _, ok := s.Conditions[condPrimaryResolved]; !ok {
			continue
		}
		from := s.ReadStart.Add(-AgreementTolerance)
		var window []RoleSweep
		for _, sw := range obs.Sweeps {
			if !sw.End.Before(from) && !sw.Start.After(s.ReadEnd) {
				window = append(window, sw)
			}
		}
		if len(window) == 0 {
			unswept++
			continue
		}
		evaluated = append(evaluated, s.ReadStart)
		if !isTrue(s, condPrimaryResolved) {
			continue
		}
		if !slices.ContainsFunc(window, func(sw RoleSweep) bool { return nodeMaster(sw, s.CurrentPrimary) }) {
			bad = append(bad, fmt.Sprintf("read at %s: PrimaryResolved=True names %s, which no heartbeat sweep "+
				"since %s showed as master", stamp(s.ReadStart), orMissing(s.CurrentPrimary, s.CurrentPrimary != ""),
				stamp(from)))
		}
	}
	if unswept > 0 {
		notes = append(notes, fmt.Sprintf("not judged in %d reads: no heartbeat sweep within the tolerance "+
			"before them", unswept))
	}
	return finding{disagreements: bad, notes: notes, evaluated: evaluated}
}

// outcomeTime is when the timeline marks the outcome as reached, or the zero
// time when it does not.
func outcomeTime(steps []Step) time.Time {
	var t time.Time
	for _, st := range steps {
		if st.Kind == "outcome_reached" {
			t = st.T
		}
	}
	return t
}

func checkPrimaryAfterOutcome(obs Observations, newMaster string) finding {
	outcomeAt := outcomeTime(obs.Steps)
	if outcomeAt.IsZero() {
		return finding{notJudged: "the timeline has no outcome"}
	}
	var bad []string
	var evaluated []time.Time
	considered := 0
	for _, s := range obs.Samples {
		if s.ReadStart.Before(outcomeAt) {
			continue
		}
		considered++
		if _, ok := s.Conditions[condPrimaryResolved]; !ok {
			continue
		}
		evaluated = append(evaluated, s.ReadStart)
		if isTrue(s, condPrimaryResolved) && s.CurrentPrimary != newMaster {
			bad = append(bad, fmt.Sprintf("read at %s, after the outcome: PrimaryResolved=True names %s, not %s",
				stamp(s.ReadStart), orMissing(s.CurrentPrimary, s.CurrentPrimary != ""), newMaster))
		}
	}
	f := finding{disagreements: bad, evaluated: evaluated, considered: considered, afterOutcome: true}
	if considered == 0 {
		f.notJudged = "no status read after the outcome was recorded"
	}
	return f
}

// checkRoutingNeedsPrimaryInWait judges the reads that started in the wait for
// the outcome: from the step fault_confirmed to the step that ends the wait,
// which is the next one in the timeline, whether the outcome was reached or
// not. A read outside the wait with RoutingReady=True and PrimaryResolved not
// True is a note.
func checkRoutingNeedsPrimaryInWait(obs Observations) finding {
	var from, to time.Time
	for i, st := range obs.Steps {
		if st.Kind == "fault_confirmed" && i+1 < len(obs.Steps) {
			from, to = st.T, obs.Steps[i+1].T
		}
	}
	if from.IsZero() {
		return finding{notJudged: "the timeline has no confirmed fault and end of the wait for the outcome"}
	}
	var bad, outside []string
	var evaluated []time.Time
	considered := 0
	for _, s := range obs.Samples {
		inWait := !s.ReadStart.Before(from) && s.ReadStart.Before(to)
		if inWait {
			considered++
			if hasAll(s, []string{condRoutingReady, condPrimaryResolved}) {
				evaluated = append(evaluated, s.ReadStart)
			}
		}
		if !isTrue(s, condRoutingReady) || isTrue(s, condPrimaryResolved) {
			continue
		}
		line := fmt.Sprintf("read at %s: RoutingReady=True while PrimaryResolved=%s (%s)", stamp(s.ReadStart),
			s.Conditions[condPrimaryResolved].Status, s.Conditions[condPrimaryResolved].Reason)
		if inWait {
			bad = append(bad, line)
		} else {
			outside = append(outside, line+", outside the wait for the outcome, reported only")
		}
	}
	f := finding{disagreements: bad, notes: outside, evaluated: evaluated, considered: considered, inWait: true}
	if considered == 0 {
		f.notJudged = "no status read was recorded in the wait for the outcome"
	}
	return f
}

// S06Rules are the rules of an Operator failure (S06). While no Operator Pod
// runs nobody updates the status, so what it says then is the status the
// Operator left: it is compared only while it is not stale, and a stale read
// is neither judged as current nor counted against the evidence limits of a
// rule that compares the status with heartbeat. master is the master the
// status must name once the Operator is back: the unchanged master after a
// restart, or the member CUBRID elected while the Operator was absent; ""
// when the outcome was not reached. returnedAt is the earliest time a status
// the returned Operator wrote can carry as status.lastObservedTime: when the
// old Pod was confirmed gone, or when the Deployment was scaled back.
// strictlyAfter is set for a restart, where the killed process can outlive its
// Pod: only a status.lastObservedTime in a later second than returnedAt's then
// shows that the replacement wrote it.
func S06Rules(master string, returnedAt time.Time, strictlyAfter bool) []Rule {
	return append([]Rule{
		{
			ID: ruleS03PrimaryWasMaster, Kind: Tolerance, Tolerance: AgreementTolerance,
			Says: "a status that is not stale resolves only a member whose own node heartbeat showed as master " +
				"in a sweep within the tolerance before the read; a stale read, as the status the Operator left " +
				"becomes while it is absent, is not compared, and the evidence limits count only the reads that " +
				"were not stale",
			check: checkPrimaryWasMasterWhileFresh,
		},
		{
			ID: ruleS06PrimaryAfterReturn, Kind: Never,
			Says: "from the later of the outcome and the Operator's return (" + stampOrUnset(returnedAt) + ") to the " +
				"end of the stable period, a read that resolves a primary names " + orMissing(master, master != "") +
				", in every read that is not stale and whose status.lastObservedTime is not before the return" +
				strictSecond(strictlyAfter) + ", so that the returned Operator wrote it; other reads are counted in a " +
				"note and not judged",
			check: func(obs Observations) finding {
				return checkPrimaryAfterReturn(obs, master, returnedAt, strictlyAfter)
			},
		},
		{
			ID: ruleS06WritesWhileStale, Kind: Report,
			Says: "the writes acknowledged, failed or ended without a known outcome while the last status read " +
				"before them was stale, as it is while no Operator runs; they are not compared with that status",
			check: reportWritesWhileStale,
		},
	}, sqlReportRules()...)
}

// checkPrimaryWasMasterWhileFresh is checkPrimaryWasMaster with the reads
// that were not stale as the reads it needs half of.
func checkPrimaryWasMasterWhileFresh(obs Observations) finding {
	f := checkPrimaryWasMaster(obs)
	fresh, _ := freshSamples(obs.Samples)
	f.considered = len(fresh)
	if f.notJudged == "" && len(fresh) == 0 {
		f.notJudged = "every status read was stale"
	}
	return f
}

func strictSecond(strictlyAfter bool) string {
	if strictlyAfter {
		return " (for a restart, in a later second than the return, since the killed process can outlive its Pod)"
	}
	return ""
}

func checkPrimaryAfterReturn(obs Observations, master string, returnedAt time.Time, strictlyAfter bool) finding {
	outcomeAt := outcomeTime(obs.Steps)
	switch {
	case outcomeAt.IsZero():
		return finding{notJudged: "the timeline has no outcome"}
	case master == "":
		return finding{notJudged: "the master the returned Operator must name is not known"}
	}
	from := outcomeAt
	if returnedAt.After(from) {
		from = returnedAt
	}
	// The API server stores lastObservedTime to the second. A status of the
	// second of the return is accepted unless the killed process may have
	// written it: it can outlive its Pod, so a restart needs a later second.
	written := returnedAt.Truncate(time.Second)
	if strictlyAfter {
		written = written.Add(time.Second)
	}
	var bad, notes []string
	var evaluated []time.Time
	considered, stale, older := 0, 0, 0
	for _, s := range obs.Samples {
		switch {
		case s.ReadStart.Before(from):
			continue
		case isStale(s):
			stale++
			continue
		case s.LastObserved.Before(written):
			older++
			continue
		}
		considered++
		if _, ok := s.Conditions[condPrimaryResolved]; !ok {
			continue
		}
		evaluated = append(evaluated, s.ReadStart)
		if isTrue(s, condPrimaryResolved) && s.CurrentPrimary != master {
			bad = append(bad, fmt.Sprintf("read at %s, after the Operator returned: PrimaryResolved=True names %s, "+
				"not %s", stamp(s.ReadStart), orMissing(s.CurrentPrimary, s.CurrentPrimary != ""), master))
		}
	}
	if stale > 0 {
		notes = append(notes, fmt.Sprintf("not judged in %d reads after the return: the status was stale", stale))
	}
	if older > 0 {
		notes = append(notes, fmt.Sprintf("not judged in %d reads after the return: status.lastObservedTime was "+
			"before the return, so the returned Operator had not written it", older))
	}
	f := finding{disagreements: bad, notes: notes, evaluated: evaluated, considered: considered, afterOutcome: true}
	if considered == 0 {
		f.notJudged = "no status read after the outcome and the Operator's return was written by the returned " +
			"Operator and not stale"
	}
	return f
}

// reportWritesWhileStale counts, by outcome, the writes whose last status read
// that ended before them was stale.
func reportWritesWhileStale(obs Observations) finding {
	samples := byReadStart(obs.Samples)
	var stale []time.Time
	for _, s := range samples {
		if isStale(s) {
			stale = append(stale, s.ReadStart)
		}
	}
	if len(stale) == 0 {
		return finding{notes: []string{"no status read was stale"}}
	}
	notes := []string{fmt.Sprintf("the status was stale in %d reads, from %s to %s%s", len(stale), stamp(stale[0]),
		stamp(stale[len(stale)-1]), sinceFault(obs.Steps, stale[0]))}
	counts := map[string]int{}
	var first time.Time
	for _, e := range obs.SQL {
		if !e.Write {
			continue
		}
		if last := lastReadBefore(samples, e.T); last < 0 || !isStale(samples[last]) {
			continue
		}
		if first.IsZero() || e.T.Before(first) {
			first = e.T
		}
		counts[e.Outcome]++
	}
	if first.IsZero() {
		return finding{notes: append(notes, "no write came while the last status read before it was stale")}
	}
	return finding{notes: append(notes, fmt.Sprintf("while the last status read before them was stale: %d "+
		"acknowledged, %d failed, %d without a known outcome, the first at %s%s", counts[SQLAcknowledged],
		counts[SQLFailed], counts[SQLUnknown], stamp(first), sinceFault(obs.Steps, first)))}
}

// Rule IDs of the install and smoke test (S00).
const (
	ruleS00NoPrimary          = "standalone-cluster-resolves-no-primary"
	ruleS00ReadyFollowedBySQL = "ready-true-is-followed-by-sql-in-the-pod"
	ruleS00SQLBeforeCondition = "sql-succeeded-before-the-condition-was-true"
	ruleS00SQLFailedWhileTrue = "sql-failed-while-the-condition-was-true"
)

const condReady = "Ready"

// ReadyTolerance is how long Ready may stay True after SQL in the member's Pod
// failed. Ready follows the Pod's readiness, which the kubelet takes from
// /readyz every 5 seconds, each probe given 5 seconds, and turns off after 3
// failed probes: up to 20 seconds after the server stopped answering
// (readinessProbe in internal/controller/cubridcluster_controller.go). The
// StatefulSet's status and the Operator's reconcile that follows add a few
// seconds, and the sampling samplingSlack.
const ReadyTolerance = 3*5*time.Second + 5*time.Second + 5*time.Second + samplingSlack

// SmokeSQL is one SQL operation of the install and smoke test (S00), with
// when it started and when its outcome was known.
type SmokeSQL struct {
	Start, End time.Time
	// Broker is true for a client through the Broker Services, false for
	// csql in the member's Pod.
	Broker bool
	// Write is true for an operation that commits a change.
	Write bool
	OK    bool
	// Inconclusive, when set, says why the operation's outcome is no evidence
	// that the statement failed or succeeded in the member's Pod: the row was
	// read but did not match, csql could not be run there, or csql printed
	// the row read with -199 and -111 (POC-44). Such an
	// operation is left out of the rules that compare the Conditions with the
	// SQL, and the reason is noted.
	Inconclusive string
	// What names the operation in agreement.txt.
	What string
}

// S00Run describes one run of the install and smoke test (S00).
type S00Run struct {
	// Member is the one member of the standalone cluster.
	Member string
	// PodReady is when the member's Pod was first seen ready; zero when it
	// was not.
	PodReady time.Time
	// SmokeEnd is when the smoke checks ended; zero when they did not.
	SmokeEnd time.Time
	SQL      []SmokeSQL
}

// S00Rules are the rules of the install and smoke test (S00), whose cluster
// is standalone: from its creation through the member's formation, Ready and
// the smoke SQL there is no fault. A standalone member runs no heartbeat and
// the Operator resolves no primary for it, so the Conditions are compared
// with the SQL of the smoke checks, and heartbeat only confirms that no member
// answers as an HA master. Its status has no status.lastObservedTime, so no
// rule here waits for it to be fresh.
//
// The shared Never rules are applied to every read. Four of them read
// Conditions a standalone cluster never carries (PrimaryResolved,
// ReplicationHealthy, and HAReady only as False): they still fail a read that
// breaks them, but their lack of judged reads does not block the run, since
// standalone-cluster-resolves-no-primary judges every read for the claims
// they start from.
func S00Rules(run S00Run) []Rule {
	optional := map[string]string{
		ruleHAReadyFollowsReplication: "HAReady=True, which " + ruleS00NoPrimary + " judges in every read",
		ruleHAReadyNeedsPrimary:       "HAReady=True, which " + ruleS00NoPrimary + " judges in every read",
		rulePrimaryNeedsEveryAnswer:   "PrimaryResolved=True, which " + ruleS00NoPrimary + " judges in every read",
		ruleReplicationNamesSlaves: "ReplicationHealthy=False, which a standalone cluster never carries, " +
			"having no replication",
	}
	var rules []Rule
	for _, r := range NeverRules() {
		if premise, ok := optional[r.ID]; ok {
			r.optional = "on a standalone cluster this rule starts from " + premise
			r.Says += "; on a standalone cluster its evidence is not required, since it starts from " + premise
		}
		rules = append(rules, r)
	}
	return append(rules,
		Rule{
			ID: ruleS00NoPrimary, Kind: Never,
			Says: "a standalone cluster claims no primary and no HA: in every read PrimaryResolved and HAReady are " +
				"not True, status.currentPrimary is empty and no member of status.instances has the role master, " +
				"since its member runs no heartbeat and the Operator resolves no primary for it; the heartbeat " +
				"sweeps are noted. At least one judged read must come before the member's Pod was ready and one " +
				"from then to the end of the smoke checks",
			check: func(obs Observations) finding { return checkS00NoPrimary(obs, run) },
		},
		Rule{
			ID: ruleS00ReadyFollowedBySQL, Kind: Tolerance, Tolerance: ReadyTolerance,
			Says: "Ready=True is not read while SQL fails (ADR-0003): SQL with csql in the member's Pod that failed " +
				"is a disagreement when the last read before it and every read for " + ReadyTolerance.String() +
				" after it, none more than " + readingGap.String() + " apart, said Ready=True. The reads that " +
				"started before the last of those operations ended are the reads it judges, and at least one of " +
				"them must say Ready=True",
			check: func(obs Observations) finding { return checkS00ReadyFollowedBySQL(obs, run) },
		},
		Rule{
			ID: ruleS00SQLBeforeCondition, Kind: Report,
			Says: "SQL that succeeded while the last read before it had Ready (csql in the member's Pod) or " +
				"RoutingReady (a client through the Broker Services) not True; the Conditions are no guarantee " +
				"about SQL (docs/observability.md)",
			check: func(obs Observations) finding { return reportS00SQL(obs, run, true) },
		},
		Rule{
			ID: ruleS00SQLFailedWhileTrue, Kind: Report,
			Says: "SQL that failed while the last read before it had Ready (csql in the member's Pod) or " +
				"RoutingReady (a client through the Broker Services) True; a failure of the client through the " +
				"Brokers is reported only, since the Broker Conditions say that Pods are available, not that a " +
				"client can run SQL",
			check: func(obs Observations) finding { return reportS00SQL(obs, run, false) },
		},
	)
}

func checkS00NoPrimary(obs Observations, run S00Run) finding {
	f := perRead(nil, func(s ConditionSample) (string, string) {
		var claims []string
		for _, c := range []string{condPrimaryResolved, condHAReady} {
			if isTrue(s, c) {
				claims = append(claims, c+"=True")
			}
		}
		if s.CurrentPrimary != "" {
			claims = append(claims, "status.currentPrimary "+s.CurrentPrimary)
		}
		for _, in := range s.Instances {
			if in.Role == roleMaster {
				claims = append(claims, in.Name+" with the role master")
			}
		}
		if len(claims) == 0 {
			return "", ""
		}
		return fmt.Sprintf("read at %s: a standalone cluster's status claims %s", stamp(s.ReadStart),
			strings.Join(claims, ", ")), ""
	})(obs)

	masters := map[string]int{}
	for _, sw := range obs.Sweeps {
		for member := range sw.Roles {
			if sw.Roles[member] == roleMaster || nodeMaster(sw, member) {
				masters[member]++
			}
		}
	}
	switch {
	case len(obs.Sweeps) == 0:
		f.notes = append(f.notes, "no heartbeat sweep was recorded")
	case len(masters) == 0:
		f.notes = append(f.notes, fmt.Sprintf("in all %d heartbeat sweeps no member answered as an HA master, "+
			"as for a standalone member that runs no heartbeat; its cubrid heartbeat status fails with \"The server was not "+
			"configured for HA.\" (docs/poc/RESULTS.md, POC-26), which the sweeps record as no answer, so they cannot "+
			"tell it from an exec that did not run", len(obs.Sweeps)))
	default:
		for _, member := range slices.Sorted(maps.Keys(masters)) {
			f.notes = append(f.notes, fmt.Sprintf("heartbeat showed %s as master in %d of %d sweeps, on a "+
				"standalone cluster", member, masters[member], len(obs.Sweeps)))
		}
	}

	if f.notJudged != "" {
		return f
	}
	in := func(from, to time.Time) bool {
		return slices.ContainsFunc(f.evaluated, func(t time.Time) bool {
			return !t.Before(from) && (to.IsZero() || t.Before(to))
		})
	}
	switch {
	case run.PodReady.IsZero():
		f.notJudged = "the member's Pod was never seen ready, so no read after the formation could be judged"
	case !in(time.Time{}, run.PodReady):
		f.notJudged = "no status read before the member's Pod was ready, at " + stamp(run.PodReady) + ", was recorded"
	case !in(run.PodReady, run.SmokeEnd):
		f.notJudged = "no status read from the member's Pod being ready to the end of the smoke checks was recorded"
	}
	return f
}

func checkS00ReadyFollowedBySQL(obs Observations, run S00Run) finding {
	var inPod []SmokeSQL
	var lastEnd time.Time
	var notes []string
	for _, op := range run.SQL {
		if !op.Broker && op.Inconclusive != "" {
			notes = append(notes, fmt.Sprintf("%s, from %s to %s, is not compared with Ready: %s", op.What,
				stamp(op.Start), stamp(op.End), op.Inconclusive))
		} else if !op.Broker {
			inPod = append(inPod, op)
			if op.End.After(lastEnd) {
				lastEnd = op.End
			}
		}
	}
	if len(inPod) == 0 {
		return finding{notJudged: "no SQL operation with csql in the member's Pod was recorded", notes: notes}
	}
	samples := byReadStart(obs.Samples)
	var readings []reading
	var evaluated []time.Time
	considered, ready := 0, 0
	for _, s := range samples {
		_, ok := s.Conditions[condReady]
		if ok {
			readings = append(readings, reading{s.ReadStart, isTrue(s, condReady)})
		}
		if !s.ReadStart.Before(lastEnd) {
			continue
		}
		considered++
		if ok {
			evaluated = append(evaluated, s.ReadStart)
			if isTrue(s, condReady) {
				ready++
			}
		}
	}
	var bad []string
	uncovered := 0
	for _, op := range inPod {
		if op.OK {
			continue
		}
		w := windowBefore(readings, op.Start, op.End.Add(ReadyTolerance), readingGap)
		switch {
		case !w.covered || w.gap:
			uncovered++
		case w.allHold:
			bad = append(bad, fmt.Sprintf("%s failed from %s to %s, while every one of the %d reads from %s to %s "+
				"after the failure said Ready=True", op.What, stamp(op.Start), stamp(op.End), w.seen, stamp(w.from),
				ReadyTolerance))
		}
	}
	if uncovered > 0 {
		notes = append(notes, fmt.Sprintf("not judged for %d failed operations: the reads do not cover the time "+
			"from the last read before each to %s after it, none more than %s apart", uncovered, ReadyTolerance,
			readingGap))
	}
	f := finding{disagreements: bad, notes: notes, evaluated: evaluated, considered: considered}
	switch {
	case considered == 0:
		f.notJudged = "no status read started before an SQL operation in the member's Pod ended"
	case ready == 0:
		f.notJudged = "no read with Ready=True came before an SQL operation in the member's Pod, so no claim " +
			"of Ready was followed by SQL"
	}
	return f
}

// reportS00SQL compares each smoke SQL operation with the last read that
// ended before it started: csql in the member's Pod with Ready, a client
// through the Brokers with RoutingReady. succeeded picks the operations that
// succeeded while the Condition was not True, otherwise those that failed
// while it was.
func reportS00SQL(obs Observations, run S00Run, succeeded bool) finding {
	samples := byReadStart(obs.Samples)
	var notes []string
	unread := 0
	for _, op := range run.SQL {
		condition := condReady
		if op.Broker {
			condition = condRoutingReady
		}
		if op.Inconclusive != "" {
			continue
		}
		last := lastReadBefore(samples, op.Start)
		if last < 0 {
			unread++
			continue
		}
		s := samples[last]
		status := orMissing(s.Conditions[condition].Status, s.Conditions[condition].Status != "")
		switch {
		case succeeded && op.OK && !isTrue(s, condition):
			notes = append(notes, fmt.Sprintf("%s succeeded from %s to %s, while the last read before it, at %s, "+
				"had %s=%s", op.What, stamp(op.Start), stamp(op.End), stamp(s.ReadStart), condition, status))
		case !succeeded && !op.OK && isTrue(s, condition):
			notes = append(notes, fmt.Sprintf("%s failed from %s to %s, while the last read before it, at %s, "+
				"had %s=True", op.What, stamp(op.Start), stamp(op.End), stamp(s.ReadStart), condition))
		}
	}
	if unread > 0 && succeeded {
		notes = append(notes, fmt.Sprintf("%d SQL operations came before the first status read and were not "+
			"compared", unread))
	}
	return finding{notes: notes}
}
