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
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	// soakLane and soakID name the soak's lane and result. Both are the
	// installed harness's own: the scenario registry holds neither, because
	// what a soak must show to accept a release is not defined yet (#126).
	soakLane       = "lab-installed-ha-soak"
	soakID         = "SOAK"
	soakRecordFile = "SOAK/record.json"
	// soakCheckFile is the data report of check n.
	soakCheckFile = "SOAK/check-%03d.json"
	// soakClientInterval is the pause of each soak client between two
	// operations, and soakReadEvery makes every tenth operation read back the
	// client's last acknowledged insert through the read-write Service.
	soakClientInterval = time.Second
	soakReadEvery      = 10
	// soakStopBound bounds the stop of the clients (stopBackgroundClients).
	soakStopBound = time.Minute
)

// soakClients write and read through the read-write Service during the soak.
var soakClients = []string{"soak1", "soak2"}

// The verdicts of one soak check: the data rules held, a rule was broken, the
// rows could not be read within the data check window, or the run ended
// during the check.
const (
	soakHeld     = "held"
	soakViolated = "violated"
	soakUnread   = "unread"
	soakEnded    = "ended"
)

// soakClock is the clock the soak schedules its checks by.
type soakClock interface {
	Now() time.Time
	// Sleep returns after d, or with ctx's error once ctx ends.
	Sleep(ctx context.Context, d time.Duration) error
}

// wallClock is the soakClock of a real run.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// soakCheck is what one check of the soak recorded: when it was due and ran,
// the client operations it judged, the rows each member and Service held, the
// roles CUBRID reported and the cluster's Conditions, and its verdict. Roles
// and Conditions are recorded, not judged.
type soakCheck struct {
	N           int       `json:"n"`
	Final       bool      `json:"final,omitempty"`
	ScheduledAt time.Time `json:"scheduledAt"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	Verdict     string    `json:"verdict"`
	Error       string    `json:"error,omitempty"`
	// Report is the check's data report, the last reading of the rows.
	Report string `json:"report"`
	// Operations counts, by outcome, the operations of the history the check
	// judged: LAB01's and the soak clients' up to the check's cutoff.
	Operations          map[string]int `json:"operations,omitempty"`
	Rows                map[string]int `json:"rows,omitempty"`
	AcknowledgedMissing *int           `json:"acknowledgedMissing,omitempty"`
	// HeldAfterSeconds is the time from the cutoff until the rules held.
	HeldAfterSeconds *float64           `json:"heldAfterSeconds,omitempty"`
	Roles            *startingState     `json:"roles,omitempty"`
	RolesNotHeld     string             `json:"rolesNotHeld,omitempty"`
	RolesError       string             `json:"rolesError,omitempty"`
	CurrentPrimary   string             `json:"currentPrimary,omitempty"`
	Conditions       []metav1.Condition `json:"conditions,omitempty"`
	ConditionsError  string             `json:"conditionsError,omitempty"`
}

// soakRecord is the soak's record: its settings, the clients, and every check.
type soakRecord struct {
	Duration         string      `json:"duration"`
	CheckEvery       string      `json:"checkEvery"`
	DataCheckWithin  string      `json:"dataCheckWithin"`
	Clients          []string    `json:"clients"`
	ClientInterval   string      `json:"clientInterval"`
	ClientReadEvery  int         `json:"clientReadEvery"`
	ClientOperations int         `json:"clientOperations"`
	StartedAt        *time.Time  `json:"startedAt,omitempty"`
	EndedAt          *time.Time  `json:"endedAt,omitempty"`
	Checks           []soakCheck `json:"checks"`
	ClientsStopped   bool        `json:"clientsStopped"`
}

// soakStepBounds is the longest the soak may take after LAB01 by the bounds of
// its own steps: each client's start, the soak itself, a check that began just
// before the end, the clients' stop and the final check.
func (c HAConfig) soakStepBounds() time.Duration {
	return time.Duration(len(soakClients))*s06ClientStart + c.SoakDuration + soakStopBound +
		2*(c.DataCheckWithin+readSlack)
}

func (c HAConfig) validateSoak() error {
	if c.SoakDuration < 0 {
		return errors.New("-soak-duration must be positive")
	}
	if c.SoakDuration == 0 {
		return nil
	}
	if c.Fault != "" || c.BackupBucket != "" {
		return errors.New("the soak runs after LAB01 with no fault: not with -fault or -backup-bucket")
	}
	if c.SoakCheckEvery <= 0 || c.SoakCheckEvery > c.SoakDuration {
		return errors.New("-soak-check-every must be positive and at most -soak-duration")
	}
	if least := c.soakStepBounds(); c.Timeout <= least {
		return fmt.Errorf("the soak needs a -timeout longer than %s, the soak and the bounds of its own steps "+
			"(starting and stopping the clients, a check running at the end and the final check), plus the time "+
			"LAB01 takes", least)
	}
	return nil
}

// recordSoak records the soak when it ran, or settles it as blocked when it
// was selected and LAB01 did not pass.
func (h *haRun) recordSoak(summary *evidence.Summary) {
	switch {
	case h.soak != nil:
		summary.Record(*h.soak)
	case h.cfg.SoakDuration > 0:
		summary.Settle(evidence.Requirement{ID: soakID}, evidence.Blocked,
			"the soak starts from LAB01's checked state, and LAB01 did not pass")
	}
}

// soakSchedule returns when the soak's checks are due: every `every` from
// start while before start+duration, and at start+duration the final one.
func soakSchedule(start time.Time, duration, every time.Duration) []time.Time {
	end := start.Add(duration)
	var at []time.Time
	for t := start.Add(every); t.Before(end); t = t.Add(every) {
		at = append(at, t)
	}
	return append(at, end)
}

// withoutLaterOps leaves out of rows those of the operations the clients
// attempted after the check's cutoff: later holds the clients' events read
// after the rows. The clients keep writing while a check reads, so such a row
// may be on one member and not yet on another; a later check judges it. A row
// no operation explains stays, and breaks the rules.
func withoutLaterOps(rows map[string][]workload.Row, cutoff workload.History, later []workload.Event) {
	after := map[string]bool{}
	for _, e := range later {
		if _, known := cutoff.Operations[e.OpID]; !known {
			after[e.OpID] = true
		}
	}
	for name, held := range rows {
		rows[name] = slices.DeleteFunc(held, func(r workload.Row) bool { return after[r.OpID] })
	}
}

// soakClientHistories reads what the soak clients recorded so far. A client
// writing a line leaves it unfinished; it is left out.
func (h *haRun) soakClientHistories(ctx context.Context) (string, error) {
	var b strings.Builder
	for _, id := range soakClients {
		text, err := h.clientRun(ctx, fmt.Sprintf("cat /tmp/%s.jsonl", id))
		if err != nil {
			return "", fmt.Errorf("read the history of client %s: %w", id, err)
		}
		b.WriteString(text[:strings.LastIndex(text, "\n")+1])
	}
	return b.String(), nil
}

// observeSoak records the roles CUBRID reports on every member and the
// cluster's status Conditions. Nothing is judged on them.
func (h *haRun) observeSoak(ctx context.Context, check *soakCheck) {
	roles, unmet, err := h.roles(ctx)
	check.Roles, check.RolesNotHeld = roles, unmet
	if err != nil {
		check.RolesError = err.Error()
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: haClusterName},
		cluster); err != nil {
		check.ConditionsError = err.Error()
		return
	}
	check.CurrentPrimary, check.Conditions = cluster.Status.CurrentPrimary, cluster.Status.Conditions
}

// soakIntervalCheck applies the data rules while the clients keep writing.
// The history up to the check's cutoff is read first: each operation it holds
// as acknowledged must be on every member and Service within DataCheckWithin
// of the cutoff. The rows of operations attempted after it are left out
// (withoutLaterOps).
func (h *haRun) soakIntervalCheck(ctx context.Context, check *soakCheck) (*workload.Report, time.Duration,
	error) {
	clients, err := h.soakClientHistories(ctx)
	if err != nil {
		return nil, 0, err
	}
	text := h.history.String() + clients
	cutoff, err := workload.ReadHistory(strings.NewReader(text))
	if err != nil {
		return nil, 0, err
	}
	check.Operations = cutoff.Counts
	filter := func(ctx context.Context, rows map[string][]workload.Row) error {
		later, err := h.soakClientHistories(ctx)
		if err != nil {
			return err
		}
		events, err := workload.ReadEvents(strings.NewReader(later))
		if err != nil {
			return err
		}
		withoutLaterOps(rows, cutoff, events)
		return nil
	}
	return h.checkFilteredDataOf(ctx, text, h.sourceTarget(), time.Now(), check.Report, filter)
}

// soakFinalCheck stops the clients, adds what they recorded to the run's
// history and applies the data rules to all of it.
func (h *haRun) soakFinalCheck(ctx context.Context, check *soakCheck, rec *soakRecord,
	result *evidence.Scenario) (*workload.Report, time.Duration, error) {
	if err := h.stopBackgroundClients(ctx, soakClients); err != nil {
		return nil, 0, fmt.Errorf("stop the clients: %w", err)
	}
	rec.ClientsStopped = true
	if err := h.collectSoakClients(ctx, result); err != nil {
		return nil, 0, err
	}
	check.Operations = h.rec.Operations
	return h.checkDataOf(ctx, h.history.String(), h.sourceTarget(), time.Now(), check.Report)
}

// collectSoakClients adds what the stopped clients recorded to the run's
// history and counts their operations as the soak's.
func (h *haRun) collectSoakClients(ctx context.Context, result *evidence.Scenario) error {
	answers, err := h.collectBackgroundClients(ctx, soakClients, time.Time{})
	if err != nil {
		return fmt.Errorf("collect the clients: %w", err)
	}
	result.Operations = &answers.operations
	return nil
}

// soakVerdict classifies a check's error.
func soakVerdict(ctx context.Context, err error) string {
	switch {
	case err == nil:
		return soakHeld
	case errors.As(err, new(dataViolation)):
		return soakViolated
	case ctx.Err() != nil:
		return soakEnded
	default:
		return soakUnread
	}
}

// runSoak keeps two clients writing and reading through the read-write Service
// for SoakDuration on the cluster LAB01 left in its checked state, with no
// fault, and applies the data rules to every member and both Services every
// SoakCheckEvery, and once more after the clients stopped. A broken rule fails
// the soak at the check that found it. A check that could not read the rows
// within the data check window is recorded, and the soak goes on; it leaves
// the soak blocked, not passed. A run that ends before the final check leaves
// it blocked. Roles and Conditions are recorded at each check, not judged.
func (h *haRun) runSoak(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(soakID, &result)()
	result = evidence.Scenario{ID: soakID, Result: evidence.Fail, NeverEvents: []string{}}
	clock := h.soakClock
	if clock == nil {
		clock = wallClock{}
	}
	ops := int((h.cfg.SoakDuration + time.Hour) / soakClientInterval)
	rec := soakRecord{Duration: h.cfg.SoakDuration.String(), CheckEvery: h.cfg.SoakCheckEvery.String(),
		DataCheckWithin: h.cfg.DataCheckWithin.String(), Clients: soakClients,
		ClientInterval: soakClientInterval.String(), ClientReadEvery: soakReadEvery, ClientOperations: ops,
		Checks: []soakCheck{}}
	clientsStarted := false
	defer func() {
		if clientsStarted && !rec.ClientsStopped {
			// Clients left writing would change the rows after the verdict.
			err := h.stopBackgroundClients(ctx, soakClients)
			rec.ClientsStopped = err == nil
			if err != nil && result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, "stop the clients: "+err.Error()
			}
			if err == nil {
				// What the clients recorded is kept in the run's history.
				collect, cancel := context.WithTimeout(context.WithoutCancel(ctx), soakStopBound)
				_ = h.collectSoakClients(collect, &result)
				cancel()
			}
		}
		if err := h.writeJSON(soakRecordFile, rec); err != nil {
			result.Result, result.Reason = evidence.Fail, "write the soak record: "+err.Error()
		}
		held, unread := 0, 0
		for _, check := range rec.Checks {
			switch check.Verdict {
			case soakHeld:
				held++
			case soakUnread:
				unread++
			}
		}
		result.Measurements = map[string]any{"checks": len(rec.Checks), "checksHeld": held,
			"checksUnread": unread, "soakSeconds": evidence.Unknown}
		if rec.StartedAt != nil && rec.EndedAt != nil {
			result.Measurements["soakSeconds"] = rec.EndedAt.Sub(*rec.StartedAt).Seconds()
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, soakID+"/")
		})
	}()
	end := func(r evidence.Result, format string, args ...any) evidence.Scenario {
		result.Result, result.Reason = r, fmt.Sprintf(format, args...)
		return result
	}
	clientsStarted = true
	env := fmt.Sprintf("OPS=%d INTERVAL_MS=%d READ_EVERY=%d ", ops, soakClientInterval.Milliseconds(), soakReadEvery)
	for _, id := range soakClients {
		if err := h.startBackgroundClientWith(ctx, id, env); err != nil {
			return end(evidence.Blocked, "start the clients: %v", err)
		}
	}
	start := clock.Now().UTC()
	rec.StartedAt = &start
	schedule := soakSchedule(start, h.cfg.SoakDuration, h.cfg.SoakCheckEvery)
	var unread []string
	for i, due := range schedule {
		if wait := due.Sub(clock.Now()); wait > 0 {
			if err := clock.Sleep(ctx, wait); err != nil {
				return end(evidence.Blocked, "the run ended before check %d of %d of the soak: %v", i+1,
					len(schedule), err)
			}
		}
		check := soakCheck{N: i + 1, Final: i == len(schedule)-1, ScheduledAt: due, StartedAt: clock.Now().UTC(),
			Report: fmt.Sprintf(soakCheckFile, i+1)}
		h.observeSoak(ctx, &check)
		var report *workload.Report
		var held time.Duration
		var err error
		if check.Final {
			report, held, err = h.soakFinalCheck(ctx, &check, &rec, &result)
		} else {
			report, held, err = h.soakIntervalCheck(ctx, &check)
		}
		check.FinishedAt, check.Verdict = clock.Now().UTC(), soakVerdict(ctx, err)
		if err != nil {
			check.Error = err.Error()
		}
		if report != nil {
			check.Rows = map[string]int{}
			for name, member := range report.Members {
				check.Rows[name] = member.Rows
			}
			check.AcknowledgedMissing = &report.AcknowledgedMissing
		} else if werr := h.writeJSON(check.Report, map[string]string{"error": check.Error}); werr != nil {
			return end(evidence.Fail, "write the report of soak check %d: %v", check.N, werr)
		}
		if err == nil {
			seconds := held.Seconds()
			check.HeldAfterSeconds = &seconds
		}
		rec.Checks = append(rec.Checks, check)
		if werr := h.writeJSON(soakRecordFile, rec); werr != nil {
			return end(evidence.Fail, "write the soak record: %v", werr)
		}
		switch check.Verdict {
		case soakViolated:
			return end(evidence.Fail, "check %d of %d, due %s: %v", check.N, len(schedule),
				due.Format(time.RFC3339), err)
		case soakEnded:
			return end(evidence.Blocked, "the run ended during check %d of %d of the soak: %v", check.N,
				len(schedule), err)
		case soakUnread:
			unread = append(unread, fmt.Sprintf("check %d (%v)", check.N, err))
		}
	}
	ended := clock.Now().UTC()
	rec.EndedAt = &ended
	if len(unread) > 0 {
		return end(evidence.Blocked, "the data rules held at every check that read the rows, but the rows could "+
			"not be read at %s", strings.Join(unread, "; "))
	}
	result.Result = evidence.Pass
	result.Reason = fmt.Sprintf("the data rules held on every member and both Services at each of the %d checks "+
		"over %s of continuous writes and reads with no fault, the last after the clients stopped", len(schedule),
		h.cfg.SoakDuration)
	return result
}
