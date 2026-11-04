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

// Observations are everything a run recorded that the Conditions are
// compared with.
type Observations struct {
	Samples []ConditionSample
	Sweeps  []RoleSweep
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
	check func(Observations) (disagreements, notes []string)
}

// Agreement is what CheckAgreement found. Each line starts with the ID of
// the rule that found it.
type Agreement struct {
	Disagreements []string
	Notes         []string
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
// finds is a note, never a disagreement.
func CheckAgreement(obs Observations, rules []Rule) Agreement {
	var a Agreement
	for _, r := range rules {
		disagreements, notes := r.check(obs)
		if r.Kind == Report {
			disagreements, notes = nil, append(disagreements, notes...)
		}
		a.Disagreements = append(a.Disagreements, prefixed(r.ID, disagreements)...)
		a.Notes = append(a.Notes, prefixed(r.ID, notes)...)
	}
	return a
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

// NeverRules are combinations of Conditions that one status read of an HA
// cluster never holds, because the Operator sets each pair in the same step.
//
// RoutingReady=True is not required to come with PrimaryResolved=True: after a
// conflicting write of a Broker object the Broker Conditions are kept as they
// were while the primary is resolved again (brokerTier in
// internal/controller/broker.go), so for one read they may disagree. That
// pair is reported only. HAReady=False is allowed with PrimaryResolved=True:
// an unhealthy replication may make HAReady False on its own.
func NeverRules() []Rule {
	return []Rule{
		{
			ID: ruleRoutingNeedsWriteEndpoint, Kind: Never,
			Says: "RoutingReady=True comes with WriteEndpointReady=True in the same read: routing is ready only " +
				"when a read-write Broker Pod is available, and both are set from the same count",
			check: perRead(func(s ConditionSample) (string, string) {
				if !isTrue(s, condRoutingReady) {
					return "", ""
				}
				return needTrue(s, condWriteEndpointReady)
			}),
		},
		{
			ID: ruleRoutingWithoutPrimary, Kind: Report,
			Says: "RoutingReady=True while PrimaryResolved is not True; possible for one read after a conflicting " +
				"write of a Broker object, which leaves the Broker Conditions as they were",
			check: perRead(func(s ConditionSample) (string, string) {
				c, ok := s.Conditions[condPrimaryResolved]
				if !isTrue(s, condRoutingReady) || !ok || c.Status == condTrue {
					return "", ""
				}
				return fmt.Sprintf("read at %s: RoutingReady=True, PrimaryResolved=%s (%s)",
					stamp(s.ReadStart), c.Status, c.Reason), ""
			}),
		},
		{
			ID: ruleHAReadyNeedsPrimary, Kind: Never,
			Says: "HAReady=True comes with PrimaryResolved=True in the same read; HAReady=False with " +
				"PrimaryResolved=True is allowed",
			check: perRead(func(s ConditionSample) (string, string) {
				if !isTrue(s, condHAReady) {
					return "", ""
				}
				return needTrue(s, condPrimaryResolved)
			}),
		},
		{
			ID: ruleBrokersNeedEndpoints, Kind: Never,
			Says: "BrokerReady=True comes with WriteEndpointReady=True and ReadEndpointReady=True in the same read",
			check: perRead(func(s ConditionSample) (string, string) {
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
			check: perRead(func(s ConditionSample) (string, string) {
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
			check: perRead(func(s ConditionSample) (string, string) {
				c, ok := s.Conditions[condReplicationHealthy]
				if !ok || c.Status != condFalse {
					return "", ""
				}
				cut := strings.LastIndex(c.Message, "on: ")
				if cut < 0 {
					return "", "the message of ReplicationHealthy=False names no member"
				}
				roles := make(map[string]string, len(s.Instances))
				for _, in := range s.Instances {
					roles[in.Name] = in.Role
				}
				var others []string
				for name := range strings.SplitSeq(c.Message[cut+len("on: "):], ", ") {
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
		check: func(obs Observations) ([]string, []string) {
			return checkPrimaryIsConfirmedMaster(obs, startingMaster)
		},
	}}
	if brokerLeft {
		rules = append(rules, Rule{
			ID: ruleWriteEndpointFollowsDeployment, Kind: Tolerance, Tolerance: AgreementTolerance,
			Says: "a status that is not stale never says WriteEndpointReady=False (NoBrokerAvailable) when " +
				"every reading of the read-write Broker Deployment within the tolerance before it showed an " +
				"available Pod",
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
	return append(rules,
		Rule{
			ID: ruleWritesWithoutRouting, Kind: Report,
			Says: "writes acknowledged while the last status read before them had RoutingReady or " +
				"PrimaryResolved not True; the Conditions are no guarantee about SQL (docs/observability.md)",
			check: func(obs Observations) ([]string, []string) {
				return reportWrites(obs, func(e SQLEvent, s ConditionSample) bool {
					return e.Outcome == SQLAcknowledged && (!isTrue(s, condRoutingReady) || !isTrue(s, condPrimaryResolved))
				}, "acknowledged while RoutingReady or PrimaryResolved was not True")
			},
		},
		Rule{
			ID: ruleRoutingWhileWritesFail, Kind: Report,
			Says: "writes that failed or ended without a known outcome while the last status read before them " +
				"had RoutingReady=True",
			check: func(obs Observations) ([]string, []string) {
				return reportWrites(obs, func(e SQLEvent, s ConditionSample) bool {
					return (e.Outcome == SQLFailed || e.Outcome == SQLUnknown) && isTrue(s, condRoutingReady)
				}, "failed or ended without a known outcome while RoutingReady was True")
			},
		},
	)
}

func checkPrimaryIsConfirmedMaster(obs Observations, master string) ([]string, []string) {
	if len(obs.Sweeps) == 0 {
		return nil, []string{"not judged: no heartbeat sweep was recorded"}
	}
	var confirmed []time.Time
	for _, sw := range obs.Sweeps {
		for member, role := range sw.Roles {
			if role == roleMaster && member != master {
				return nil, []string{fmt.Sprintf("not judged: the heartbeat sweep at %s reports %s as a master, "+
					"so the starting master was not the only master throughout", stamp(sw.Start), member)}
			}
		}
		if sw.Roles[master] == roleMaster {
			confirmed = append(confirmed, sw.Start)
		}
	}
	if len(confirmed) == 0 {
		return nil, []string{"not judged: no heartbeat sweep confirmed the starting master " + master}
	}
	fresh, notes := freshSamples(obs.Samples)
	var bad []string
	unconfirmed := 0
	for _, s := range fresh {
		if !isTrue(s, condPrimaryResolved) || s.CurrentPrimary == master {
			continue
		}
		if !slices.ContainsFunc(confirmed, func(t time.Time) bool {
			return !t.Before(s.ReadStart.Add(-AgreementTolerance)) && !t.After(s.ReadEnd)
		}) {
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
	return bad, notes
}

func checkWriteEndpointFollowsDeployment(obs Observations) ([]string, []string) {
	type reading struct {
		t         time.Time
		available int
	}
	var readings []reading
	var empty []time.Time
	for _, s := range obs.Samples {
		if s.RWAvailable != nil {
			readings = append(readings, reading{s.ReadEnd, *s.RWAvailable})
			if *s.RWAvailable == 0 {
				empty = append(empty, s.ReadEnd)
			}
		}
	}
	if len(readings) == 0 {
		return nil, []string{"not judged: the read-write Broker Deployment was not read"}
	}
	var notes []string
	if len(empty) > 0 {
		notes = append(notes, fmt.Sprintf("the read-write Broker Deployment had none of its Pods available in "+
			"%d readings, first at %s; a status after them may say so within the tolerance", len(empty), stamp(empty[0])))
	}
	fresh, staleNotes := freshSamples(obs.Samples)
	notes = append(notes, staleNotes...)
	var bad []string
	uncovered := 0
	for _, s := range fresh {
		c, ok := s.Conditions[condWriteEndpointReady]
		if !ok || c.Status != condFalse || c.Reason != "NoBrokerAvailable" {
			continue
		}
		from := s.ReadStart.Add(-AgreementTolerance)
		if readings[0].t.After(from) {
			uncovered++
			continue
		}
		all, seen := true, 0
		for _, r := range readings {
			if r.t.Before(from) || r.t.After(s.ReadEnd) {
				continue
			}
			seen++
			all = all && r.available > 0
		}
		if seen > 0 && all {
			bad = append(bad, fmt.Sprintf("read at %s: WriteEndpointReady=False (NoBrokerAvailable), while all %d "+
				"readings of the Deployment since %s showed an available Pod", stamp(s.ReadStart), seen, stamp(from)))
		}
	}
	if uncovered > 0 {
		notes = append(notes, fmt.Sprintf("not judged in %d reads that say no Broker is available: the "+
			"Deployment was not read for the whole tolerance before them", uncovered))
	}
	return bad, notes
}

func reportAllBrokersLost(obs Observations) ([]string, []string) {
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
	return nil, []string{span("WriteEndpointReady was not True", notReady) + "; " +
		span("the read-write Broker Deployment had no available Pod", empty)}
}

// reportWrites counts the writes that, compared with the last status read that
// ended before them and was not stale, match.
func reportWrites(obs Observations, match func(SQLEvent, ConditionSample) bool, what string) ([]string, []string) {
	fresh, _ := freshSamples(obs.Samples)
	count := 0
	var first time.Time
	for _, e := range obs.SQL {
		if !e.Write {
			continue
		}
		last := -1
		for i, s := range fresh {
			if s.ReadEnd.After(e.T) {
				break
			}
			last = i
		}
		if last < 0 || !match(e, fresh[last]) {
			continue
		}
		if count == 0 {
			first = e.T
		}
		count++
	}
	if count == 0 {
		return nil, nil
	}
	return nil, []string{fmt.Sprintf("%d writes %s, the first at %s%s", count, what, stamp(first),
		sinceFault(obs.Steps, first))}
}

// perRead applies a check to every status read. check returns a finding, or
// why the read could not be judged; reads that could not be judged are
// counted in one note per reason.
func perRead(check func(ConditionSample) (bad, skip string)) func(Observations) ([]string, []string) {
	return func(obs Observations) ([]string, []string) {
		var bad []string
		skipped := map[string]int{}
		for _, s := range obs.Samples {
			b, skip := check(s)
			if b != "" {
				bad = append(bad, b)
			}
			if skip != "" {
				skipped[skip]++
			}
		}
		notes := make([]string, 0, len(skipped))
		for _, why := range slices.Sorted(maps.Keys(skipped)) {
			notes = append(notes, fmt.Sprintf("not judged in %d reads: %s", skipped[why], why))
		}
		return bad, notes
	}
}

// freshSamples returns the reads, in the order of their start, whose status
// is not stale, and a note on the ones that were.
func freshSamples(samples []ConditionSample) ([]ConditionSample, []string) {
	var fresh []ConditionSample
	stale := 0
	for _, s := range samples {
		if s.LastObserved.IsZero() || s.ReadStart.Sub(s.LastObserved) > staleAfter {
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
