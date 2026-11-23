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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The tests in this file cover the soak after the base HA run (LAB01): the
// background clients keep writing and reading for the soak's length with no
// fault, and the existing data rules are applied to every member and both
// Services at every interval and once more after the clients stopped. A
// broken rule ends the soak as failed at the check that found it; a check that
// could not read the rows is recorded, and the soak goes on to the next one.

func testSoakConfig(t *testing.T) HAConfig {
	t.Helper()
	cfg := testHAConfig(t)
	cfg.Timeout = time.Hour
	cfg.SoakDuration = 3 * time.Minute
	cfg.SoakCheckEvery = time.Minute
	return cfg
}

// fakeSoakClock is a clock whose sleep returns at once, moving the time on;
// onSleep is called with the number of the sleep after it.
type fakeSoakClock struct {
	mu      sync.Mutex
	now     time.Time
	slept   []time.Duration
	onSleep func(n int)
}

func (c *fakeSoakClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeSoakClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	n, onSleep := len(c.slept), c.onSleep
	c.mu.Unlock()
	if onSleep != nil {
		onSleep(n)
	}
	return nil
}

func newSoakFixture(t *testing.T, cfg HAConfig) (*haFixture, *fakeSoakClock) {
	t.Helper()
	f := newHAFixture(t, cfg)
	clock := &fakeSoakClock{now: time.Date(2026, 12, 8, 0, 0, 0, 0, time.UTC)}
	f.runner.soakClock = clock
	liveSoakClients(f)
	return f, clock
}

// liveSoakClients plays clients that keep running: each read of a client's
// history adds an acknowledged insert, a rolled-back one and a read of the
// insert, all answered. It also plays an insert that commits between two reads
// of the history while the run reads the rows: its row is there at once, and
// its lines appear only at the next read. Once the clients were stopped, no
// such insert is started.
func liveSoakClients(f *haFixture) {
	var mu sync.Mutex
	seq, lines, pending := map[string]int{}, map[string][]string{}, map[string][]string{}
	f.liveHistory = func(id string) string {
		mu.Lock()
		defer mu.Unlock()
		at := time.Now().UTC()
		add := func(e workload.Event) {
			e.T, e.Client, e.Endpoint = at, id, "rw"
			data, _ := json.Marshal(e)
			lines[id] = append(lines[id], string(data))
		}
		lines[id] = append(lines[id], pending[id]...)
		pending[id] = nil
		insert := func() workload.Row {
			seq[id]++
			n := seq[id]
			row := workload.Row{OpID: fmt.Sprintf("%s-%06d", id, n), Client: id, Seq: n, Amount: n*37%1000 + 1,
				Note: fmt.Sprintf("%s-%d", id, n)}
			f.mu.Lock()
			f.rows = append(f.rows, row)
			f.mu.Unlock()
			return row
		}
		row := insert()
		add(workload.Event{Seq: row.Seq, Op: workload.OpInsert, OpID: row.OpID, Event: workload.Attempted,
			Amount: row.Amount, Note: row.Note})
		add(workload.Event{Seq: row.Seq, Op: workload.OpInsert, OpID: row.OpID, Event: workload.Acknowledged})
		seq[id]++
		n := seq[id]
		rollback := fmt.Sprintf("%s-%06d", id, n)
		add(workload.Event{Seq: n, Op: workload.OpRollback, OpID: rollback, Event: workload.Attempted,
			Amount: n*37%1000 + 1, Note: workload.RolledBackNote})
		add(workload.Event{Seq: n, Op: workload.OpRollback, OpID: rollback, Event: workload.Acknowledged})
		seq[id]++
		n = seq[id]
		read := fmt.Sprintf("%s-%06d", id, n)
		add(workload.Event{Seq: n, Op: workload.OpRead, OpID: read, Event: workload.Attempted, Amount: row.Amount,
			Note: row.Note, Target: row.OpID})
		add(workload.Event{Seq: n, Op: workload.OpRead, OpID: read, Event: workload.Acknowledged})
		f.mu.Lock()
		stopped := f.kills > 0
		f.mu.Unlock()
		if !stopped {
			later := insert()
			for _, e := range []workload.Event{{Seq: later.Seq, Op: workload.OpInsert, OpID: later.OpID,
				Event: workload.Attempted, Amount: later.Amount, Note: later.Note},
				{Seq: later.Seq, Op: workload.OpInsert, OpID: later.OpID, Event: workload.Acknowledged}} {
				e.T, e.Client, e.Endpoint = at, id, "rw"
				data, _ := json.Marshal(e)
				pending[id] = append(pending[id], string(data))
			}
		}
		return strings.Join(lines[id], "\n") + "\n"
	}
}

func readSoakRecord(t *testing.T, dir string) soakRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, soakRecordFile))
	if err != nil {
		t.Fatal(err)
	}
	var rec soakRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func soakScenarios(t *testing.T, dir string) (evidence.Summary, evidence.Scenario, evidence.Scenario) {
	t.Helper()
	summary := readSummary(t, dir)
	if summary.Lane != soakLane || len(summary.Scenarios) != 2 || summary.Scenarios[1].ID != soakID {
		t.Fatalf("summary = %+v, want LAB01 and %s in lane %s", summary, soakID, soakLane)
	}
	return summary, summary.Scenarios[0], summary.Scenarios[1]
}

// requireCheckFiles requires each check's data report to be kept and listed as
// the soak's evidence.
func requireCheckFiles(t *testing.T, dir string, soak evidence.Scenario, rec soakRecord) {
	t.Helper()
	for _, check := range rec.Checks {
		if check.Report == "" {
			t.Errorf("check %d keeps no data report: %+v", check.N, check)
			continue
		}
		if !slices.Contains(soak.Evidence, check.Report) {
			t.Errorf("soak evidence = %v, want %s", soak.Evidence, check.Report)
		}
		if _, err := os.Stat(filepath.Join(dir, check.Report)); err != nil {
			t.Errorf("check %d: %v", check.N, err)
		}
	}
	if !slices.Contains(soak.Evidence, soakRecordFile) {
		t.Errorf("soak evidence = %v, want %s", soak.Evidence, soakRecordFile)
	}
}

func TestSoakScheduleChecksEveryIntervalAndAtTheEnd(t *testing.T) {
	start := time.Date(2026, 12, 8, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		duration, every time.Duration
		want            []time.Duration
	}{
		{3 * time.Minute, time.Minute, []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute}},
		{70 * time.Minute, 30 * time.Minute, []time.Duration{30 * time.Minute, time.Hour, 70 * time.Minute}},
		{time.Hour, time.Hour, []time.Duration{time.Hour}},
	} {
		got := soakSchedule(start, tc.duration, tc.every)
		want := make([]time.Time, 0, len(tc.want))
		for _, d := range tc.want {
			want = append(want, start.Add(d))
		}
		if !slices.Equal(got, want) {
			t.Errorf("soakSchedule(%s, %s) = %v, want %v", tc.duration, tc.every, got, want)
		}
	}
	day := soakSchedule(start, 24*time.Hour, 30*time.Minute)
	if len(day) != 48 || !day[47].Equal(start.Add(24*time.Hour)) {
		t.Errorf("a 24-hour soak checked every 30 minutes has the checks %v, want 48 ending at 24h", day)
	}
}

func TestSoakConfigurationIsRefusedWhenItCannotRun(t *testing.T) {
	const soakEveryFlag = "-soak-check-every"
	for _, tc := range []struct {
		name   string
		change func(*HAConfig)
		reason string
	}{
		{"negative duration", func(c *HAConfig) { c.SoakDuration = -time.Minute }, "-soak-duration"},
		{"zero interval", func(c *HAConfig) { c.SoakCheckEvery = 0 }, soakEveryFlag},
		{"negative interval", func(c *HAConfig) { c.SoakCheckEvery = -time.Minute }, soakEveryFlag},
		{"interval over the duration", func(c *HAConfig) { c.SoakCheckEvery = 4 * time.Minute }, soakEveryFlag},
		{"a timeout shorter than the soak", func(c *HAConfig) { c.Timeout = 2 * time.Minute }, "-timeout"},
		{"a timeout without room for the soak's own steps", func(c *HAConfig) { c.Timeout = 4 * time.Minute },
			"-timeout"},
		{"a fault", func(c *HAConfig) {
			c.Fault = FaultOperatorRestart
			c.StablePeriod = time.Second
			c.ResyncLimit = time.Second
			c.LeaderLease = testLeaderLease
		}, "no fault"},
		{"a backup", func(c *HAConfig) {
			c.BackupBucket, c.ObjectStoreService, c.BackupLimit = "soak-backups", "o/s:1", time.Hour
		}, "no fault"},
	} {
		cfg := testSoakConfig(t)
		cfg.NamespaceMode = FreshNamespace
		tc.change(&cfg)
		if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: validate() = %v, want a refusal naming %s", tc.name, err, tc.reason)
		}
	}
	cfg := testSoakConfig(t)
	cfg.NamespaceMode = FreshNamespace
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want a three-minute soak within an hour accepted", err)
	}
	if least := cfg.soakStepBounds(); least <= cfg.SoakDuration {
		t.Fatalf("soakStepBounds() = %s, want more than the soak itself", least)
	}
}

func TestRunSoakChecksTheDataAtEveryIntervalAndPasses(t *testing.T) {
	cfg := testSoakConfig(t)
	f, clock := newSoakFixture(t, cfg)
	if err := f.runner.RunHA(t.Context(), cfg); err != nil {
		t.Fatalf("RunHA() error = %v", err)
	}
	summary, lab, soak := soakScenarios(t, cfg.EvidenceDir)
	if !summary.Passed || lab.Result != evidence.Pass || soak.Result != evidence.Pass {
		t.Fatalf("summary = %+v, want LAB01 and the soak passed", summary)
	}
	if !slices.Equal(clock.slept, []time.Duration{time.Minute, time.Minute, time.Minute}) {
		t.Fatalf("the soak waited %v, want a minute before each of its three checks", clock.slept)
	}
	rec := readSoakRecord(t, cfg.EvidenceDir)
	if len(rec.Checks) != 3 {
		t.Fatalf("soak checks = %+v, want three", rec.Checks)
	}
	previous := 0
	for i, check := range rec.Checks {
		if check.N != i+1 || check.Verdict != soakHeld || check.Final != (i == 2) ||
			!check.ScheduledAt.Equal(clock.now.Add(time.Duration(i-2)*time.Minute)) {
			t.Errorf("check %d = %+v, want check %d held at its minute, the last final", i, check, i+1)
		}
		if check.Roles == nil || check.Roles.Master != testHAPrimary || len(check.Conditions) == 0 {
			t.Errorf("check %d records roles %+v and conditions %+v, want both", i+1, check.Roles, check.Conditions)
		}
		if check.Operations[workload.Attempted] <= previous || check.Rows[testHAPrimary] == 0 ||
			check.Rows["ro"] == 0 {
			t.Errorf("check %d operations %v rows %v, want more operations than before and rows read on every "+
				"member and Service", i+1, check.Operations, check.Rows)
		}
		previous = check.Operations[workload.Attempted]
	}
	if !rec.ClientsStopped || soak.Operations == nil || soak.Operations.Acknowledged == 0 {
		t.Fatalf("record = %+v, operations = %+v, want the clients stopped and their operations counted",
			rec, soak.Operations)
	}
	if soak.Measurements["checksHeld"] != 3.0 || soak.Measurements["checksUnread"] != 0.0 {
		t.Fatalf("soak measurements = %v", soak.Measurements)
	}
	requireCheckFiles(t, cfg.EvidenceDir, soak, rec)
	requireOwnEvidence(t, lab, haScenarioID)
	requireOwnEvidence(t, soak, soakID)
	requireNamespaceGone(t, f)
}

func TestRunSoakFailsAtTheCheckThatFindsAViolation(t *testing.T) {
	cfg := testSoakConfig(t)
	f, clock := newSoakFixture(t, cfg)
	clock.onSleep = func(n int) {
		if n == 2 {
			// From the second check on, the master lacks LAB01's first
			// acknowledged row: no lag explains that.
			f.mu.Lock()
			f.missing[testHAPrimary] = 0
			f.mu.Unlock()
		}
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed with an acknowledged row missing on the master")
	}
	summary, lab, soak := soakScenarios(t, cfg.EvidenceDir)
	if summary.Passed || lab.Result != evidence.Pass || soak.Result != evidence.Fail ||
		!strings.Contains(soak.Reason, "check 2") || !strings.Contains(soak.Reason, workload.RuleMissing) {
		t.Fatalf("soak = %+v, want failed at check 2 for the missing row", soak)
	}
	rec := readSoakRecord(t, cfg.EvidenceDir)
	if len(rec.Checks) != 2 || rec.Checks[0].Verdict != soakHeld || rec.Checks[1].Verdict != soakViolated ||
		rec.Checks[1].Error == "" {
		t.Fatalf("soak checks = %+v, want the first held and the second violated, and no third", rec.Checks)
	}
	if len(clock.slept) != 2 || !rec.ClientsStopped || f.kills < len(soakClients) {
		t.Fatalf("slept %v, record %+v, kills %d: want the soak ended at check 2 with its clients stopped",
			clock.slept, rec, f.kills)
	}
	requireCheckFiles(t, cfg.EvidenceDir, soak, rec)
	requireNamespaceGone(t, f)
}

func TestRunSoakRecordsAnUnreadCheckRetriesItAndGoesOn(t *testing.T) {
	cfg := testSoakConfig(t)
	f, clock := newSoakFixture(t, cfg)
	var mu sync.Mutex
	unreadable, attempts := false, 0
	f.csqlHook = func(_, sql string) (string, error, bool) {
		mu.Lock()
		defer mu.Unlock()
		if !unreadable || !strings.Contains(sql, "FROM ledger") {
			return "", nil, false
		}
		attempts++
		return "", errors.New(testUpgradeRefused), true
	}
	clock.onSleep = func(n int) {
		mu.Lock()
		defer mu.Unlock()
		unreadable = n == 1
	}
	if err := f.runner.RunHA(t.Context(), cfg); err == nil {
		t.Fatal("RunHA() passed although a check could not read the rows")
	}
	_, lab, soak := soakScenarios(t, cfg.EvidenceDir)
	if lab.Result != evidence.Pass || soak.Result != evidence.Blocked || !strings.Contains(soak.Reason, "check 1") {
		t.Fatalf("soak = %+v, want blocked naming the unread check 1, not failed", soak)
	}
	rec := readSoakRecord(t, cfg.EvidenceDir)
	verdicts := make([]string, 0, len(rec.Checks))
	for _, check := range rec.Checks {
		verdicts = append(verdicts, check.Verdict)
	}
	if !slices.Equal(verdicts, []string{soakUnread, soakHeld, soakHeld}) || rec.Checks[0].Error == "" {
		t.Fatalf("soak checks = %+v, want check 1 unread with its error, then checks 2 and 3 held", rec.Checks)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts < 2 {
		t.Fatalf("check 1 read the rows %d times, want it retried within the data check window", attempts)
	}
	if soak.Measurements["checksUnread"] != 1.0 || soak.Measurements["checksHeld"] != 2.0 {
		t.Fatalf("soak measurements = %v", soak.Measurements)
	}
	requireCheckFiles(t, cfg.EvidenceDir, soak, rec)
}

// A row of an operation the clients attempted after the check's cutoff is left
// to a later check; a row no operation explains is kept, and breaks the rules.
func TestWithoutLaterOpsKeepsOnlyRowsTheCutoffExplains(t *testing.T) {
	const known, later, unexplained = "a-000001", "a-000002", "x-000001"
	cutoff := workload.History{Operations: map[string]workload.Operation{known: {OpID: known}}}
	rows := map[string][]workload.Row{
		"m0": {{OpID: known}, {OpID: later}, {OpID: unexplained}},
		"ro": {{OpID: later}},
	}
	withoutLaterOps(rows, cutoff, []workload.Event{{OpID: known}, {OpID: later}})
	if ids := func(rows []workload.Row) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.OpID)
		}
		return out
	}; !slices.Equal(ids(rows["m0"]), []string{known, unexplained}) || len(rows["ro"]) != 0 {
		t.Fatalf("rows = %+v, want the later operation's rows left out and the others kept", rows)
	}
}
