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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// The tests in this file cover CUBRID's own state and HA logs, which the
// network partition lane keeps per member when the cluster does not settle
// after the cut is removed. On the VM lab a member stayed heartbeat master
// with its server registered_and_to_be_active for more than 25 minutes after
// the heal, and the engine's logs that could say why were deleted with the
// run namespace (#716, #115).

// s07EngineSecret is a value the tests register with the redactor, so that a
// log that prints it shows whether what is stored was redacted.
const s07EngineSecret = "engine-log-secret-value"

// s07EngineFake answers the commands of the engine log capture for each
// member: files maps a member to the files of its $CUBRID/log, in the order
// ls -1t lists them, and their content; failing names a member whose exec
// does not reach it, and hanging one whose exec waits for its context.
// stuck names a member whose second applyinfo reading waits for its context,
// and readings counts the applyinfo readings of each member's copied log, so
// that the second reading prints fewer delayed pages than the first.
type s07EngineFake struct {
	files    map[string][][2]string
	failing  string
	hanging  string
	stuck    string
	readings map[string]int
}

func (f s07EngineFake) command(ctx context.Context, args ...string) (string, error) {
	pod := args[3]
	if err := ctx.Err(); err != nil {
		return "", err
	}
	switch pod {
	case f.failing:
		return "error: unable to upgrade connection: pod does not exist", errors.New("exit status 1")
	case f.hanging:
		<-ctx.Done()
		return "", ctx.Err()
	}
	dash := slices.Index(args, "--")
	if dash < 0 || len(args) < dash+4 {
		return "", fmt.Errorf("not a command of the capture: %q", args)
	}
	script, params := args[dash+3], args[min(len(args), dash+5):]
	switch {
	case strings.Contains(script, "heartbeat status -v"):
		return heartbeatOutput(pod, roleMaster, testToBeActive) + "password=" + s07EngineSecret + "\n", nil
	case strings.Contains(script, "changemode"):
		return "The server '" + databaseName + "@localhost''s current HA running mode is to-be-active.\n", nil
	case strings.Contains(script, "applyinfo"):
		key := pod + " " + params[2]
		f.readings[key]++
		if pod == f.stuck && f.readings[key] > 1 {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return fmt.Sprintf(" *** Applied Info. *** \nDelayed log page count         : %d\ncopied log %s, db %s\n",
			100/f.readings[key], params[2], params[1]), nil
	case strings.Contains(script, "ss -tnpio"):
		return "ESTAB 0 0 10.0.0.1:41234 10.0.0.2:1523 users:((\"cub_admin\",pid=326,fd=6))\n" +
			"\t bytes_received:7985092 password=" + s07EngineSecret + "\n", nil
	case strings.Contains(script, "ls -1t"):
		var b strings.Builder
		for _, file := range f.files[pod] {
			b.WriteString(file[0] + "\n")
		}
		return b.String(), nil
	case strings.Contains(script, "tail -n"):
		lines, _ := strconv.Atoi(params[0])
		limit, _ := strconv.Atoi(params[1])
		for _, file := range f.files[pod] {
			if file[0] == params[2] {
				text := file[1]
				all := strings.SplitAfter(text, "\n")
				if all[len(all)-1] == "" {
					all = all[:len(all)-1]
				}
				text = strings.Join(all[max(0, len(all)-lines):], "")
				return text[max(0, len(text)-limit):], nil
			}
		}
		return "tail: cannot open '" + params[2] + "'", errors.New("exit status 1")
	}
	return "", fmt.Errorf("not a command of the capture: %q", script)
}

// s07EngineRun is a run of the three members whose redactor knows
// s07EngineSecret, with a short interval between the two applyinfo readings.
func s07EngineRun(t *testing.T, fake s07EngineFake) *haRun {
	s07ShortApplyInfoInterval(t)
	if fake.readings == nil {
		fake.readings = map[string]int{}
	}
	h := s07SnapshotRun(t)
	h.redactor = evidence.NewRedactor(s07EngineSecret)
	h.Command = fake.command
	return h
}

// The four kinds of HA log of a member with two peers, as the Instance
// Manager's entrypoint and CUBRID name them in $CUBRID/log.
func s07AllHALogs(pod string) [][2]string {
	return [][2]string{
		{"server/" + databaseName + ".err", "server line password=" + s07EngineSecret + "\n"},
		{pod + "_master.err", "master line\n"},
		{databaseName + "@ha-1_copylogdb.err", "copylogdb line\n"},
		{databaseName + "@ha-2_copylogdb.err", "copylogdb line\n"},
		{databaseName + "@localhost_applylogdb_ha-1.err", "applylogdb line\n"},
		{databaseName + "@localhost_applylogdb_ha-2.err", "applylogdb line\n"},
	}
}

// s07ShortApplyInfoInterval makes the interval between the two applyinfo
// readings short for one test.
func s07ShortApplyInfoInterval(t *testing.T) {
	old := s07ApplyInfoInterval
	s07ApplyInfoInterval = 50 * time.Millisecond
	t.Cleanup(func() { s07ApplyInfoInterval = old })
}

func s07Members(c *s07EngineLogs) map[string]*s07MemberEngineLogs {
	if c == nil {
		return nil
	}
	return c.Members
}

func s07Source(m *s07MemberEngineLogs, origin string) *s07EngineLogSource {
	if m == nil {
		return nil
	}
	for i := range m.Sources {
		if m.Sources[i].Origin == origin {
			return &m.Sources[i]
		}
	}
	return nil
}

// When the cluster did not settle, the run keeps each member's verbose
// heartbeat status, its server's HA mode and the HA logs its $CUBRID/log
// lists, redacted, under the variant's engine-logs directory, and names in
// the record every file it stored. A kind of HA log the listing does not show
// is recorded as missing; a member the run could not reach is unreadable,
// with no file and no missing kind claimed.
func TestCaptureS07EngineLogsWhenTheClusterDidNotSettle(t *testing.T) {
	h := s07EngineRun(t, s07EngineFake{files: map[string][][2]string{
		m0: s07AllHALogs(m0),
		m1: {{"server/" + databaseName + ".err", "server line\n"}},
	}, failing: m2})
	rec := &s07Record{Variant: s07HigherRankIsolated}
	h.captureS07EngineLogs(context.Background(), rec)
	c := rec.UnsettledEngineLogs
	if c == nil || c.At.IsZero() {
		t.Fatalf("no capture: %+v", c)
	}
	a := c.Members[m0]
	for _, origin := range []string{s07OriginHeartbeat, s07OriginHAMode, s07OriginListing,
		"$CUBRID/log/server/" + databaseName + ".err", "$CUBRID/log/" + m0 + "_master.err",
		"$CUBRID/log/" + databaseName + "@ha-1_copylogdb.err",
		"$CUBRID/log/" + databaseName + "@localhost_applylogdb_ha-2.err"} {
		s := s07Source(a, origin)
		if s == nil || s.Status != evidence.LogCollected || s.File == "" {
			t.Errorf("%s %s: %+v, want collected into a file", m0, origin, s)
			continue
		}
		if !strings.HasPrefix(s.File, "S07/higher-rank-isolated/engine-logs/"+m0+"/") {
			t.Errorf("%s %s is stored at %s, outside the variant's engine-logs directory", m0, origin, s.File)
		}
		if !slices.Contains(h.files, s.File) {
			t.Errorf("%s is not listed among the run's evidence files", s.File)
		}
		data, err := os.ReadFile(filepath.Join(h.cfg.EvidenceDir, s.File))
		if err != nil || len(data) == 0 || strings.Contains(string(data), s07EngineSecret) {
			t.Errorf("%s: %v, %q, want the stored, redacted output", s.File, err, data)
		}
	}
	if len(a.Missing) != 0 {
		t.Errorf("%s misses %v, want none", m0, a.Missing)
	}
	beat, _ := os.ReadFile(filepath.Join(h.cfg.EvidenceDir, s07Source(a, s07OriginHeartbeat).File))
	if !strings.Contains(string(beat), testToBeActive) {
		t.Errorf("the verbose heartbeat status %q lost the server's state", beat)
	}
	b := c.Members[m1]
	if want := []string{s07KindMaster, s07KindCopylogdb, s07KindApplylogdb}; !slices.Equal(b.Missing, want) {
		t.Errorf("%s misses %v, want %v", m1, b.Missing, want)
	}
	d := c.Members[m2]
	if d == nil || len(d.Sources) == 0 || d.Missing != nil {
		t.Fatalf("%s: %+v, want unreadable sources and no missing kind claimed", m2, d)
	}
	for _, s := range d.Sources {
		if s.Status != evidence.LogUnavailable || s.File != "" || s.Reason == "" {
			t.Errorf("%s %s: %+v, want unreadable with its reason and no file", m2, s.Origin, s)
		}
	}
}

// A member stuck in to-be-active may have an applier that still applies the
// copied log or one that has stopped (POC-66, #716). To tell them apart, the
// run reads cubrid applyinfo for each copied log the member's verbose
// heartbeat status names twice, s07ApplyInfoInterval apart, and keeps the
// member's TCP sockets with the processes that own them, redacted. A member
// whose heartbeat status could not be read names no copied log, so its
// applyinfo is unreadable and says why.
func TestCaptureS07EngineLogsReadsEachApplierTwice(t *testing.T) {
	h := s07EngineRun(t, s07EngineFake{files: map[string][][2]string{m0: s07AllHALogs(m0)}, failing: m2})
	rec := &s07Record{Variant: s07HigherRankIsolated}
	h.captureS07EngineLogs(context.Background(), rec)
	a := s07Members(rec.UnsettledEngineLogs)[m0]
	dir := "/var/lib/cubrid/databases/" + databaseName + "_ha-1"
	var texts [2]string
	var at [2]time.Time
	for i, reading := range []string{"first", "second"} {
		origin := "cubrid applyinfo -L " + dir + " -a " + databaseName + " (" + reading + " reading)"
		s := s07Source(a, origin)
		if s == nil || s.Status != evidence.LogCollected || s.File == "" || s.At == nil {
			t.Fatalf("%s %s: %+v, want collected into a file with its time", m0, origin, s)
		}
		data, err := os.ReadFile(filepath.Join(h.cfg.EvidenceDir, s.File))
		if err != nil || !strings.Contains(string(data), "copied log "+dir+", db "+databaseName) {
			t.Fatalf("%s: %v, %q, want applyinfo of %s on %s", s.File, err, data, dir, databaseName)
		}
		texts[i], at[i] = string(data), *s.At
	}
	if texts[0] == texts[1] {
		t.Errorf("both readings stored %q, want each reading's own output", texts[0])
	}
	if gap := at[1].Sub(at[0]); gap < s07ApplyInfoInterval {
		t.Errorf("the readings are %s apart, want at least %s", gap, s07ApplyInfoInterval)
	}
	s := s07Source(a, s07OriginSockets)
	if s == nil || s.Status != evidence.LogCollected || s.File == "" {
		t.Fatalf("%s sockets: %+v, want collected into a file", m0, s)
	}
	data, _ := os.ReadFile(filepath.Join(h.cfg.EvidenceDir, s.File))
	if !strings.Contains(string(data), "cub_admin") || strings.Contains(string(data), s07EngineSecret) {
		t.Errorf("the sockets %q lost the owning process or were not redacted", data)
	}
	d := s07Members(rec.UnsettledEngineLogs)[m2]
	if s := s07Source(d, s07OriginApplyInfo); s == nil || s.Status != evidence.LogUnavailable || s.File != "" ||
		!strings.Contains(s.Reason, "heartbeat status") {
		t.Errorf("%s, whose heartbeat status could not be read: applyinfo %+v, want unreadable with the reason", m2, s)
	}
}

// The copied logs to read are taken only from the Copylogdb lines of the
// verbose heartbeat status for the run's database, and a line whose peer or
// directory is not a plain host name or absolute path is left out, so that
// nothing the engine prints reaches the shell as anything but an argument.
func TestS07CopiedLogsFromTheVerboseHeartbeatStatus(t *testing.T) {
	out := " HA-Process Info (master 63, state master)\n" +
		"   Copylogdb appdb@ha-1:/var/lib/cubrid/databases/appdb_ha-1 (pid 326, state registered)\n" +
		"   Copylogdb appdb@ha-2:/var/lib/cubrid/databases/appdb_ha-2 (pid 327, state registered)\n" +
		"   Applylogdb appdb@localhost:/var/lib/cubrid/databases/appdb_ha-1 (pid 328, state registered)\n" +
		"   Copylogdb otherdb@ha-1:/var/lib/cubrid/databases/otherdb_ha-1 (pid 329, state registered)\n" +
		"   Copylogdb appdb@ha-3:/tmp/$(reboot) (pid 330, state registered)\n" +
		"   Copylogdb appdb@ha-1:/var/lib/cubrid/databases/appdb_ha-1 (pid 326, state registered)\n"
	got := s07CopiedLogs(out)
	want := []s07CopiedLog{{Peer: "ha-1", Dir: "/var/lib/cubrid/databases/appdb_ha-1"},
		{Peer: "ha-2", Dir: "/var/lib/cubrid/databases/appdb_ha-2"}}
	if !slices.Equal(got, want) {
		t.Errorf("copied logs %+v, want %+v", got, want)
	}
}

// The second readings have a time bound of their own: a member whose
// applyinfo hangs on its second reading uses only its share of it, and the
// members after it are still read twice.
func TestCaptureS07EngineLogsBoundsTheSecondApplyInfoReadings(t *testing.T) {
	old := s07ApplyInfoBound
	s07ApplyInfoBound = 300 * time.Millisecond
	t.Cleanup(func() { s07ApplyInfoBound = old })
	h := s07EngineRun(t, s07EngineFake{files: map[string][][2]string{}, stuck: m0})
	rec := &s07Record{Variant: s07HigherRankIsolated}
	start := time.Now()
	h.captureS07EngineLogs(context.Background(), rec)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the capture took %s with a second-reading bound of %s", took, s07ApplyInfoBound)
	}
	second := "cubrid applyinfo -L /var/lib/cubrid/databases/" + databaseName + "_ha-1 -a " + databaseName +
		" (second reading)"
	c := rec.UnsettledEngineLogs
	if s := s07Source(c.Members[m0], second); s == nil || s.Status != evidence.LogUnavailable || s.File != "" {
		t.Errorf("%s hung on its second reading: %+v, want unreadable", m0, s)
	}
	for _, pod := range []string{m1, m2} {
		if s := s07Source(c.Members[pod], second); s == nil || s.Status != evidence.LogCollected {
			t.Errorf("%s after a hung %s: %+v, want collected", pod, m0, s)
		}
	}
}

// What is kept of one log is bounded by lines and by bytes: a log longer
// than either keeps its newest whole lines and says it was cut. The whole
// capture is bounded in bytes too, and a file past that bound is not read
// and says why.
func TestCaptureS07EngineLogsIsBoundedInSize(t *testing.T) {
	for name, tc := range map[string]struct {
		lines int
		bytes int64
		text  string
	}{
		"past the line bound": {lines: 3, bytes: 1 << 20, text: "1\n2\n3\n4\n5\n"},
		"past the byte bound": {lines: 2000, bytes: 10, text: "first line\nsecond\nthird\n"},
	} {
		t.Run(name, func(t *testing.T) {
			oldLines, oldBytes := s07EngineLogLines, s07EngineLogBytes
			s07EngineLogLines, s07EngineLogBytes = tc.lines, tc.bytes
			t.Cleanup(func() { s07EngineLogLines, s07EngineLogBytes = oldLines, oldBytes })
			rel := "server/" + databaseName + ".err"
			h := s07EngineRun(t, s07EngineFake{files: map[string][][2]string{m0: {{rel, tc.text}}}})
			rec := &s07Record{Variant: s07HigherRankIsolated}
			h.captureS07EngineLogs(context.Background(), rec)
			s := s07Source(s07Members(rec.UnsettledEngineLogs)[m0], "$CUBRID/log/"+rel)
			if s == nil || s.Status != evidence.LogTruncated || s.File == "" || s.Reason == "" {
				t.Fatalf("%+v, want a truncated file with its reason", s)
			}
			data, _ := os.ReadFile(filepath.Join(h.cfg.EvidenceDir, s.File))
			if int64(len(data)) > tc.bytes || strings.Count(string(data), "\n") > tc.lines ||
				!strings.HasSuffix(tc.text, string(data)) || !strings.HasSuffix(string(data), "\n") ||
				(len(data) < len(tc.text) && tc.text[len(tc.text)-len(data)-1] != '\n') {
				t.Errorf("kept %q of %q, want its newest whole lines within the bounds", data, tc.text)
			}
		})
	}
	oldTotal := s07EngineLogTotal
	s07EngineLogTotal = 1
	t.Cleanup(func() { s07EngineLogTotal = oldTotal })
	h := s07EngineRun(t, s07EngineFake{files: map[string][][2]string{m0: s07AllHALogs(m0)}})
	rec := &s07Record{Variant: s07HigherRankIsolated}
	h.captureS07EngineLogs(context.Background(), rec)
	s := s07Source(s07Members(rec.UnsettledEngineLogs)[m0], "$CUBRID/log/"+m0+"_master.err")
	if s == nil || s.Status != s07NotRead || s.File != "" || !strings.Contains(s.Reason, "size bound") {
		t.Errorf("past the capture's size bound: %+v, want not read with the reason", s)
	}
}

// The capture is bounded in time so that cleanup still happens within the
// run's deadline: a member that hangs, as one stuck in to-be-master may, uses
// only its share of the bound, and the members after it are still read.
func TestCaptureS07EngineLogsIsBoundedInTime(t *testing.T) {
	old := s07EngineLogBound
	s07EngineLogBound = 300 * time.Millisecond
	t.Cleanup(func() { s07EngineLogBound = old })
	h := s07EngineRun(t, s07EngineFake{files: map[string][][2]string{
		m1: s07AllHALogs(m1), m2: s07AllHALogs(m2)}, hanging: m0})
	rec := &s07Record{Variant: s07HigherRankIsolated}
	start := time.Now()
	h.captureS07EngineLogs(context.Background(), rec)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the capture took %s with a bound of %s", took, s07EngineLogBound)
	}
	c := rec.UnsettledEngineLogs
	if s := s07Source(c.Members[m0], s07OriginHeartbeat); s == nil || s.Status != evidence.LogUnavailable {
		t.Errorf("%s hung: %+v, want unreadable", m0, s)
	}
	for _, pod := range []string{m1, m2} {
		if s := s07Source(c.Members[pod], "$CUBRID/log/"+pod+"_master.err"); s == nil ||
			s.Status != evidence.LogCollected {
			t.Errorf("%s after a hung %s: %+v, want collected", pod, m0, s)
		}
	}
}

// After a heal that did not settle, the run keeps the engine logs before it
// returns, so they are in the record when the namespace is deleted, and
// the verdict and its reason are those the run reaches without them.
func TestFinishS07KeepsTheEngineLogsOfAClusterThatDidNotSettle(t *testing.T) {
	finish := func(capture bool) (*s07Record, evidence.Scenario) {
		h := s07SnapshotRun(t)
		h.rec.Client = &imageIdentity{Pod: clientPodName, Node: testClientNode}
		open := s07OpenHistory(s06Acknowledged)
		rows := s07OpenRows(s06Acknowledged)
		ledger := s07LedgerAnswers(map[string][]workload.Row{m0: rows, m1: rows}, m2, "")
		s07ShortApplyInfoInterval(t)
		engine := s07EngineFake{files: map[string][][2]string{m0: s07AllHALogs(m0)}, failing: m2,
			readings: map[string]int{}}
		h.Command = func(ctx context.Context, args ...string) (string, error) {
			script := args[len(args)-1]
			switch {
			case args[3] == clientPodName && strings.Contains(script, "echo stopped"):
				return testStopped, nil
			case args[3] == clientPodName && strings.Contains(script, "cat /tmp/"+s07OpenClient+".jsonl"):
				return open, nil
			case args[3] == clientPodName:
				return "", nil
			case slices.ContainsFunc(args, func(a string) bool {
				return strings.Contains(a, "heartbeat status -v") || strings.Contains(a, "changemode") ||
					strings.Contains(a, "ls -1t") || strings.Contains(a, "tail -n") ||
					strings.Contains(a, "applyinfo") || strings.Contains(a, "ss -tnpio")
			}):
				if !capture {
					return "", errors.New("exit status 1")
				}
				return engine.command(ctx, args...)
			}
			return ledger(ctx, args...)
		}
		issued := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
		rec := s07Base(false)
		rec.SettledAt, rec.SettleUnmet, rec.Data, rec.HealthReadings = nil, "CUBRID reports "+m0+": to-be-master", nil, 0
		rec.FaultIssuedAt, rec.ClockOffsets = &issued, map[string]float64{testClientNode: 0}
		result := &evidence.Scenario{}
		got := h.finishS07(context.Background(), rec, result, func(r evidence.Result, format string,
			args ...any) evidence.Scenario {
			result.Result, result.Reason = r, fmt.Sprintf(format, args...)
			return *result
		})
		return rec, got
	}
	rec, got := finish(true)
	c := rec.UnsettledEngineLogs
	if c == nil || s07Source(c.Members[m0], "$CUBRID/log/"+m0+"_master.err") == nil {
		t.Fatalf("capture %+v, want %s's logs kept", c, m0)
	}
	_, unread := finish(false)
	if got.Result != evidence.Blocked || got.Result != unread.Result || got.Reason != unread.Reason {
		t.Errorf("finished %s %q with the logs and %s %q without, want the same blocked verdict",
			got.Result, got.Reason, unread.Result, unread.Reason)
	}
}

// The -timeout check counts the capture with the rows read when the cluster
// does not settle, in place of -data-check-within when they are longer: the
// two passes over the members, during which the interval between the two
// applyinfo readings passes, and the second readings.
func TestS07StepBoundsCountTheEngineLogCapture(t *testing.T) {
	cfg := testHAConfig(t)
	cfg.PartitionHold, cfg.DataCheckWithin = 2*time.Minute, time.Second
	withoutCapture := s07PrepareBound + cfg.PartitionHold + s07ConfirmSlack + s07CleanupBound + cfg.s07Settle() +
		s07SnapshotBound
	if 2*s07ApplyInfoInterval > s07EngineLogBound {
		t.Errorf("the applyinfo interval %s is longer than half the capture's bound %s, so its wait adds time",
			s07ApplyInfoInterval, s07EngineLogBound)
	}
	capture := s07EngineLogBound + s07ApplyInfoBound
	if got, want := cfg.s07StepBounds(), withoutCapture+capture; got != want {
		t.Errorf("the steps' bounds are %s, want %s with the engine log capture", got, want)
	}
}

// The commands run here with a stand-in $CUBRID: the tail keeps the newest
// lines within both bounds, and the listing names the HA logs the capture
// may copy, newest first.
func TestS07EngineLogScripts(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	t.Setenv("CUBRID", home)
	if err := os.MkdirAll(filepath.Join(home, "log", "server"), 0o750); err != nil {
		t.Fatal(err)
	}
	rel := "server/" + databaseName + ".err"
	if err := os.WriteFile(filepath.Join(home, "log", rel), []byte("1\n2\n3\n4\n5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		out, err := exec.Command(sh, append([]string{"-c"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%q: %v\n%s", args, err, out)
		}
		return string(out)
	}
	if got := run(s07TailEngineLog, s07EngineLogArg0, "2", "100", rel); got != "4\n5\n" {
		t.Errorf("the line-bounded tail printed %q", got)
	}
	if got := run(s07TailEngineLog, s07EngineLogArg0, "10", "3", rel); got != "\n5\n" {
		t.Errorf("the byte-bounded tail printed %q", got)
	}
	if got := run(evidence.ListEngineLogs[2]); got != rel+"\n" {
		t.Errorf("the listing printed %q", got)
	}
	// Stand-in cubrid and ss print the arguments they were given, so the
	// command forms are those POC-66 ran on the lab's image.
	stubs := t.TempDir()
	for path, name := range map[string]string{filepath.Join(home, "bin", "cubrid"): "cubrid",
		filepath.Join(stubs, "ss"): "ss"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+name+" \"$@\"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := "/var/lib/cubrid/databases/" + databaseName + "_ha-1"
	if got, want := run(s07ApplyInfo, s07EngineLogArg0, "100", databaseName, dir),
		"cubrid applyinfo -L "+dir+" -a "+databaseName+"\n"; got != want {
		t.Errorf("applyinfo ran %q, want %q", got, want)
	}
	if got := run(s07Sockets, s07EngineLogArg0, "100"); got != "ss -tnpio\n" {
		t.Errorf("the socket listing ran %q", got)
	}
	if got := run(s07Sockets, s07EngineLogArg0, "4"); got != "ss -" {
		t.Errorf("the socket listing is not bounded in bytes: %q", got)
	}
}
