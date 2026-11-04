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
	"maps"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// at is t0 plus the given number of seconds.
func at(seconds float64) time.Time { return t0.Add(time.Duration(seconds * float64(time.Second))) }

// healthy is a status read at the given second of a cluster with master m-0,
// every Condition True, an observation 5 seconds old in which every member
// answered, and two available read-write Broker Pods.
func healthy(second float64) ConditionSample {
	observed := at(second - 5)
	conditions := map[string]ConditionState{}
	for _, c := range []string{"PrimaryResolved", "HAReady", "RoutingReady", "ReplicationHealthy",
		"BrokerReady", "WriteEndpointReady", "ReadEndpointReady"} {
		conditions[c] = ConditionState{Status: "True", LastTransitionTime: t0.Add(-time.Hour)}
	}
	return ConditionSample{
		ReadStart: at(second), ReadEnd: at(second + 0.2),
		Conditions: conditions, CurrentPrimary: m0, LastObserved: observed,
		Instances: []MemberStatus{
			{Name: m0, Role: roleMaster, LastObserved: observed},
			{Name: m1, Role: roleSlave, LastObserved: observed},
			{Name: m2, Role: roleSlave, LastObserved: observed},
		},
		RWAvailable: new(2),
	}
}

// with returns s with the Condition c set to status and reason.
func with(s ConditionSample, c, status, reason string) ConditionSample {
	conditions := make(map[string]ConditionState, len(s.Conditions))
	maps.Copy(conditions, s.Conditions)
	conditions[c] = ConditionState{Status: status, Reason: reason, LastTransitionTime: s.ReadStart.Add(-time.Second)}
	s.Conditions = conditions
	return s
}

func sweep(second float64, master string) RoleSweep {
	roles := map[string]string{m0: roleSlave, m1: roleSlave, m2: roleSlave}
	if master != "" {
		roles[master] = roleMaster
	}
	return RoleSweep{Start: at(second), End: at(second + 1), Roles: roles}
}

// series is a healthy sample and a sweep that confirms m-0 every two seconds
// from second 0 to last.
func series(last float64) Observations {
	var obs Observations
	for s := 0.0; s <= last; s += 2 {
		obs.Samples = append(obs.Samples, healthy(s))
		obs.Sweeps = append(obs.Sweeps, sweep(s, m0))
	}
	return obs
}

// replace sets the samples read at or after the given second through f.
func replace(obs Observations, from float64, f func(ConditionSample) ConditionSample) Observations {
	samples := make([]ConditionSample, len(obs.Samples))
	for i, s := range obs.Samples {
		if !s.ReadStart.Before(at(from)) {
			s = f(s)
		}
		samples[i] = s
	}
	obs.Samples = samples
	return obs
}

func one(s ConditionSample) Observations { return Observations{Samples: []ConditionSample{s}} }

// mentions reports whether one of lines contains every one of words.
func mentions(lines []string, words ...string) bool {
	for _, l := range lines {
		all := true
		for _, w := range words {
			all = all && strings.Contains(l, w)
		}
		if all {
			return true
		}
	}
	return false
}

type agreementCase struct {
	obs Observations
	// disagree is the rule ID expected among the disagreements; "" expects
	// none.
	disagree string
	// note is a word expected in a note; "" expects nothing of the notes.
	note string
	// notJudged is the rule ID expected among the rules that could not be
	// judged at all; "" expects none.
	notJudged string
}

func checkCases(t *testing.T, rules []Rule, cases map[string]agreementCase) {
	t.Helper()
	for name, c := range cases {
		got := CheckAgreement(c.obs, rules)
		switch {
		case c.disagree == "" && len(got.Disagreements) > 0:
			t.Errorf("%s: unexpected disagreements %v", name, got.Disagreements)
		case c.disagree != "" && !mentions(got.Disagreements, c.disagree):
			t.Errorf("%s: no disagreement of rule %s among %v", name, c.disagree, got.Disagreements)
		}
		if c.note != "" && !mentions(got.Notes, c.note) {
			t.Errorf("%s: no note mentions %q among %v", name, c.note, got.Notes)
		}
		switch {
		case c.notJudged == "" && len(got.NotJudged) > 0:
			t.Errorf("%s: unexpected rules not judged %v", name, got.NotJudged)
		case c.notJudged != "" && !mentions(got.NotJudged, c.notJudged):
			t.Errorf("%s: rule %s is not among the rules not judged %v", name, c.notJudged, got.NotJudged)
		}
	}
}

// since returns s with the Condition c set to status and reason since the
// given second.
func since(s ConditionSample, c, status, reason string, second float64) ConditionSample {
	s = with(s, c, status, reason)
	state := s.Conditions[c]
	state.LastTransitionTime = at(second)
	s.Conditions[c] = state
	return s
}

func TestNeverRules(t *testing.T) {
	older := healthy(10)
	older.Instances = append([]MemberStatus(nil), older.Instances...)
	older.Instances[2].LastObserved = older.LastObserved.Add(-20 * time.Second)
	unobserved := older
	unobserved.Instances = append([]MemberStatus(nil), older.Instances...)
	unobserved.Instances[2].LastObserved = time.Time{}
	noClusterTime := healthy(10)
	noClusterTime.LastObserved = time.Time{}
	noWriteEndpoint := healthy(10)
	noWriteEndpoint.Conditions = map[string]ConditionState{"RoutingReady": {Status: "True"}}
	replicationOn := func(names string) ConditionSample {
		s := with(healthy(10), "ReplicationHealthy", "False", "ReplicationStalled")
		c := s.Conditions["ReplicationHealthy"]
		c.Message = "log pages have waited for 1m0s or longer with nothing applied on: " + names
		s.Conditions["ReplicationHealthy"] = c
		return s
	}
	unparsed := with(healthy(10), "ReplicationHealthy", "False", "SomethingElse")

	checkCases(t, NeverRules(), map[string]agreementCase{
		"every Condition True agrees": {obs: series(30)},
		"routing ready although the write endpoint is not": {
			obs:      one(with(healthy(10), "WriteEndpointReady", "False", "NoBrokerAvailable")),
			disagree: ruleRoutingNeedsWriteEndpoint},
		"routing ready although the write endpoint is unknown": {
			obs:      one(with(healthy(10), "WriteEndpointReady", "Unknown", "BrokerReconcileFailed")),
			disagree: ruleRoutingNeedsWriteEndpoint},
		"routing unknown and the write endpoint not ready make no claim": {
			obs: one(with(with(with(healthy(10), "RoutingReady", "Unknown", "RolesNotObserved"),
				"WriteEndpointReady", "False", "NoBrokerAvailable"), "BrokerReady", "False", "BrokersNotAvailable"))},
		"a read without the write endpoint Condition is not judged by that rule": {
			obs: one(noWriteEndpoint), note: ruleRoutingNeedsWriteEndpoint},
		"HA ready although the primary is not resolved": {
			obs:      one(with(healthy(10), "PrimaryResolved", "False", "PrimaryObservationIncomplete")),
			disagree: ruleHAReadyNeedsPrimary},
		// A replication stall may make HAReady False while the primary stays
		// resolved, so the two are not required to be equal.
		"HA not ready while the primary is resolved agrees": {
			obs: one(with(healthy(10), "HAReady", "False", "ReplicationUnhealthy"))},
		"Brokers ready although the read endpoint is not": {
			obs:      one(with(healthy(10), "ReadEndpointReady", "False", "NoBrokerAvailable")),
			disagree: ruleBrokersNeedEndpoints},
		"primary resolved on an observation one member did not answer": {
			obs: one(older), disagree: rulePrimaryNeedsEveryAnswer},
		"primary resolved with a member that has no answer time": {
			obs: one(unobserved), disagree: rulePrimaryNeedsEveryAnswer},
		"an older member answer while the primary is not resolved agrees": {
			obs: one(with(with(with(older, "PrimaryResolved", "False", "PrimaryObservationIncomplete"),
				"HAReady", "False", "PrimaryObservationIncomplete"), "RoutingReady", "False", "PrimaryObservationIncomplete"))},
		"a read without the cluster's observation time is not judged by that rule": {
			obs: one(noClusterTime), note: rulePrimaryNeedsEveryAnswer},
		"replication unhealthy on a slave agrees": {obs: one(replicationOn(m1 + ", " + m2))},
		"replication unhealthy on the master": {
			obs: one(replicationOn(m1 + ", " + m0)), disagree: ruleReplicationNamesSlaves},
		"replication unhealthy on a member the read does not list": {
			obs: one(replicationOn("m-7")), disagree: ruleReplicationNamesSlaves},
		"replication unhealthy with a message that names no member is not judged": {
			obs: one(unparsed), note: ruleReplicationNamesSlaves},
		"without a single status read no rule can be judged": {
			obs: Observations{}, notJudged: ruleHAReadyNeedsPrimary},
	})
	// The Broker Conditions are kept as they were after a conflicting write
	// of a Broker object, while the primary is resolved afresh, so the code
	// does not guarantee this pair; it is reported only.
	checkCases(t, HAReportRules(), map[string]agreementCase{
		"routing ready while the primary is not resolved is reported, not failed": {
			obs: one(with(with(healthy(10), "PrimaryResolved", "False", "PrimaryObservationIncomplete"),
				"HAReady", "False", "PrimaryObservationIncomplete")),
			note: ruleRoutingWithoutPrimary},
	})
	for _, r := range NeverRules() {
		if r.Kind != Never {
			t.Errorf("rule %s of kind %s is among the Never rules", r.ID, r.Kind)
		}
	}
}

func TestDisagreementsNameTheirKind(t *testing.T) {
	never := CheckAgreement(one(with(healthy(10), "PrimaryResolved", "False", "PrimaryObservationIncomplete")),
		NeverRules())
	if len(never.Disagreements) == 0 || !strings.HasPrefix(never.Disagreements[0], "never: ") {
		t.Errorf("a Never finding does not say its kind: %v", never.Disagreements)
	}
	tolerance := CheckAgreement(replace(series(30), 10, func(s ConditionSample) ConditionSample {
		s.CurrentPrimary = m1
		return s
	}), S05Rules(m0, true))
	if !mentions(tolerance.Disagreements, "tolerance: "+rulePrimaryIsConfirmedMaster) {
		t.Errorf("a tolerance finding does not say its kind: %v", tolerance.Disagreements)
	}
}

func TestS05PrimaryAgreesWithHeartbeat(t *testing.T) {
	otherPrimary := func(s ConditionSample) ConditionSample { s.CurrentPrimary = m1; return s }
	stale := func(age time.Duration) func(ConditionSample) ConditionSample {
		return func(s ConditionSample) ConditionSample {
			s = otherPrimary(s)
			s.LastObserved = s.ReadStart.Add(-age)
			return s
		}
	}
	unset := func(s ConditionSample) ConditionSample {
		s = otherPrimary(s)
		s.LastObserved = time.Time{}
		return s
	}
	handover := series(30)
	handover.Sweeps = append(handover.Sweeps, sweep(31, m1))
	noSweeps := replace(series(30), 10, otherPrimary)
	noSweeps.Sweeps = nil
	// Every sweep ran, but no answer was recognized as a master or a slave.
	unrecognized := replace(series(30), 10, otherPrimary)
	for i := range unrecognized.Sweeps {
		unrecognized.Sweeps[i].Roles = map[string]string{m0: "", m1: "", m2: ""}
	}

	checkCases(t, S05Rules(m0, true), map[string]agreementCase{
		"the status names the master heartbeat confirms": {obs: series(30)},
		"the status names another primary while heartbeat confirms the master": {
			obs: replace(series(30), 10, otherPrimary), disagree: rulePrimaryIsConfirmedMaster},
		"heartbeat names another master once, so the rule's precondition does not hold": {
			obs: replace(handover, 10, otherPrimary), notJudged: rulePrimaryIsConfirmedMaster},
		"without a heartbeat sweep the rule is not judged": {
			obs: noSweeps, notJudged: rulePrimaryIsConfirmedMaster},
		"heartbeat answers that never name a master leave the rule not judged": {
			obs: unrecognized, notJudged: rulePrimaryIsConfirmedMaster},
		// The reader rule of docs/observability.md: a status observed more
		// than 60 seconds before it was read is stale; whole-second clocks
		// get one second more.
		"a status observed 61 seconds before the read is still compared": {
			obs: replace(series(30), 10, stale(61*time.Second)), disagree: rulePrimaryIsConfirmedMaster},
		"a status observed 62 seconds before the read is stale and not compared": {
			obs: replace(series(30), 10, stale(62*time.Second)), note: "stale"},
		"a status without an observation time is not compared": {
			obs: replace(series(30), 10, unset), note: "stale"},
	})
}

func TestS05WriteEndpointAgreesWithDeployment(t *testing.T) {
	// noBroker reports no read-write Broker since the given second.
	noBroker := func(from float64) func(ConditionSample) ConditionSample {
		return func(s ConditionSample) ConditionSample {
			s = since(s, "WriteEndpointReady", "False", "NoBrokerAvailable", from)
			s = since(s, "RoutingReady", "False", "NoBrokerAvailable", from)
			return since(s, "BrokerReady", "False", "BrokersNotAvailable", from)
		}
	}
	pods := func(n int) func(ConditionSample) ConditionSample {
		return func(s ConditionSample) ConditionSample { s.RWAvailable = new(n); return s }
	}
	unread := func(s ConditionSample) ConditionSample { s.RWAvailable = nil; return s }

	// The fault is issued right after the first read. One Pod stays
	// available, and the status says from second 6 on that none is.
	aftermath := replace(replace(series(70), 4, pods(1)), 6, noBroker(6))
	// The same, but the status changed only 30 seconds before the last read.
	recent := replace(replace(series(70), 4, pods(1)), 40, noBroker(40))
	// The Deployment had no available Pod at second 40.
	dipped := replace(replace(replace(series(70), 4, pods(1)), 40, pods(0)), 42, pods(1))
	dipped = replace(dipped, 6, noBroker(6))
	// Ten seconds without a reading of the Deployment, from second 32 to 44:
	// a drop to no available Pod in between would not have been seen.
	coarse := replace(replace(replace(series(70), 4, pods(1)), 6, noBroker(6)), 32, unread)
	coarse = replace(coarse, 44, pods(1))
	// The status said so since before the reads began.
	early := replace(replace(series(30), 0, pods(1)), 0, noBroker(-30))

	checkCases(t, S05Rules(m0, true), map[string]agreementCase{
		"no Broker reported for longer than the tolerance after the fault while one Pod was available": {
			obs: aftermath, disagree: ruleWriteEndpointFollowsDeployment},
		"no Broker reported for less than the tolerance agrees":       {obs: recent},
		"a reading with no available Pod within the tolerance agrees": {obs: dipped, note: "none"},
		"a gap between the readings of the Deployment is not judged": {
			obs: coarse, note: "gap"},
		"no Broker reported since before the readings cover the tolerance is not judged": {
			obs: early, note: ruleWriteEndpointFollowsDeployment},
		"without a reading of the Deployment the rule is not judged": {
			obs: replace(aftermath, 0, unread), notJudged: ruleWriteEndpointFollowsDeployment},
	})
	checkCases(t, S05Rules(m0, false), map[string]agreementCase{
		"with every read-write Broker Pod deleted, the write endpoint is reported only": {
			obs: aftermath, note: ruleWriteEndpointAllBrokersLost},
	})
}

func TestS05SQLIsReportedOnly(t *testing.T) {
	notRouted := replace(series(30), 10, func(s ConditionSample) ConditionSample {
		return with(s, "RoutingReady", "False", "NoBrokerAvailable")
	})
	notRouted.SQL = []SQLEvent{{T: at(12.5), Write: true, Outcome: SQLAcknowledged}}
	failing := series(30)
	failing.SQL = []SQLEvent{
		{T: at(12.5), Write: true, Outcome: SQLFailed},
		{T: at(13), Write: true, Outcome: SQLUnknown},
	}
	reads := series(30)
	reads.SQL = []SQLEvent{{T: at(12.5), Outcome: SQLFailed}}
	before := series(30)
	before.SQL = []SQLEvent{{T: at(-1), Write: true, Outcome: SQLFailed}}

	checkCases(t, S05Rules(m0, true), map[string]agreementCase{
		"a write acknowledged while routing was not ready is reported": {
			obs: notRouted, note: ruleWritesWithoutRouting},
		"a write that failed while routing was ready is reported": {
			obs: failing, note: ruleRoutingWhileWritesFail},
	})
	for name, obs := range map[string]Observations{"a failed read": reads, "a write before the first read": before} {
		got := CheckAgreement(obs, S05Rules(m0, true))
		if len(got.Disagreements) > 0 || mentions(got.Notes, ruleRoutingWhileWritesFail, "1 ") {
			t.Errorf("%s: counted as a failed write while routing was ready: %+v", name, got)
		}
	}
}

func TestRulesAreDescribed(t *testing.T) {
	rules := append(append(NeverRules(), HAReportRules()...), S05Rules(m0, true)...)
	rules = append(rules, S05Rules(m0, false)...)
	if len(rules) == 0 {
		t.Fatal("no rules")
	}
	for _, r := range rules {
		if r.ID == "" || r.Says == "" || r.check == nil {
			t.Errorf("rule %+v has no ID, description or check", r)
		}
		if (r.Kind == Tolerance) != (r.Tolerance > 0) {
			t.Errorf("rule %s of kind %s has tolerance %s", r.ID, r.Kind, r.Tolerance)
		}
	}
}

func TestBrokerTolerance(t *testing.T) {
	// The Operator watches the Broker Deployments, so a change of their
	// available Pods starts a reconcile at once. It waits up to 5 seconds
	// for the role answers, keeps the Broker Conditions for up to 30 seconds
	// of conflicting Broker writes, and the sampling adds a few seconds.
	if brokerTolerance < 35*time.Second || brokerTolerance > 45*time.Second {
		t.Errorf("brokerTolerance = %s, want 35s plus a few seconds of sampling", brokerTolerance)
	}
	for _, r := range S05Rules(m0, true) {
		if r.ID == ruleWriteEndpointFollowsDeployment && r.Tolerance != brokerTolerance {
			t.Errorf("rule %s has tolerance %s, want %s", r.ID, r.Tolerance, brokerTolerance)
		}
	}
}

func TestTolerance(t *testing.T) {
	// The status may be 60 seconds old (docs/observability.md), the role
	// answer it rests on 15 more (roleObservationTTL), plus the sampling.
	if AgreementTolerance < 75*time.Second || AgreementTolerance > 90*time.Second {
		t.Errorf("AgreementTolerance = %s, want 75s plus a few seconds of sampling", AgreementTolerance)
	}
}

func TestReportRulesNeverDisagree(t *testing.T) {
	for _, r := range append(S05Rules(m0, true), S05Rules(m0, false)...) {
		if r.Kind != Report {
			continue
		}
		r.check = func(Observations) finding {
			return finding{disagreements: []string{"a difference"}, notJudged: "no evidence"}
		}
		if got := CheckAgreement(series(4), []Rule{r}); len(got.Disagreements) > 0 || len(got.NotJudged) > 0 ||
			!mentions(got.Notes, "a difference") {
			t.Errorf("report rule %s: %+v, want its finding among the notes", r.ID, got)
		}
	}
}
