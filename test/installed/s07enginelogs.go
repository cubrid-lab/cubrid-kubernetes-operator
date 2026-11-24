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
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// The bounds of the engine log capture made when the cluster did not settle.
// They are variables so that tests can make them small.
var (
	// s07EngineLogBound bounds the capture up to the second applyinfo
	// readings, so that the run namespace is still deleted within the run's
	// deadline; each member gets an equal share of what is left of each of
	// its two passes, as in the rows snapshot.
	s07EngineLogBound = 2 * time.Minute
	// s07EngineLogLines bounds the lines kept of one log file.
	s07EngineLogLines = 2000
	// s07EngineLogBytes bounds what is kept of one command or file, as the
	// diagnostic log bundle's SourceBytes does.
	s07EngineLogBytes int64 = 1 << 20
	// s07EngineLogTotal bounds what the capture reads in all, as the bundle's
	// BundleBytes does.
	s07EngineLogTotal int64 = 16 << 20
	// s07ApplyInfoInterval is the least time between the two applyinfo
	// readings of one copied log, so that the delayed page count and the
	// applied position can be compared: an applier still progressing moves
	// them, a stopped one does not (POC-66 read them every 15 s; one minute
	// leaves room for a slow apply to show). It is at most half of
	// s07EngineLogBound, so the wait for it overlaps the reading of the logs
	// and adds no time to the capture's bound.
	s07ApplyInfoInterval = time.Minute
	// s07ApplyInfoBound bounds the second applyinfo readings of all
	// members, from the time the last of them is due; each member gets an
	// equal share of what is left of it. It is short, as the run's default
	// -timeout of 30 minutes still has to cover master-isolated with
	// -partition-manual-release.
	s07ApplyInfoBound = 20 * time.Second
)

// s07EngineLogCaptureBound is the longest the whole capture takes: the two
// passes over the members, during which the interval before the second
// applyinfo readings passes, and those readings.
func s07EngineLogCaptureBound() time.Duration {
	return s07EngineLogBound + s07ApplyInfoBound
}

// s07EngineLogFiles is the most log files read on one member, newest first,
// as the diagnostic log bundle's EngineFiles.
const s07EngineLogFiles = 8

// The origins of the sources of the capture, as the record names them.
const (
	s07OriginHeartbeat = "cubrid heartbeat status -v"
	s07OriginHAMode    = "cubrid changemode " + databaseName + "@localhost"
	s07OriginListing   = "listing of $CUBRID/log"
	// s07OriginApplyInfo is the applyinfo source of a member whose verbose
	// heartbeat status named no copied log; each copied log's readings are
	// named by s07ApplyInfoOrigin.
	s07OriginApplyInfo = "cubrid applyinfo"
	s07OriginSockets   = "ss -tnpio"
)

// s07ApplyInfoOrigin names one applyinfo reading of the copied log in dir.
func s07ApplyInfoOrigin(dir, reading string) string {
	return s07OriginApplyInfo + " -L " + dir + " -a " + databaseName + " (" + reading + " reading)"
}

// s07CopiedLog is a copied log the member's copylogdb keeps for one peer,
// as its verbose heartbeat status names it.
type s07CopiedLog struct {
	Peer, Dir string
}

// s07CopiedLogLine matches a Copylogdb line of cubrid heartbeat status -v,
// such as "Copylogdb appdb@ha-1:/var/lib/cubrid/databases/appdb_ha-1 (pid
// 326, state registered)": the database, the peer and the copied log's
// directory. Only plain host names and absolute paths are taken, as each is
// passed on to a shell command as an argument.
var s07CopiedLogLine = regexp.MustCompile(`(?m)^\s*Copylogdb\s+([^@\s]+)@([A-Za-z0-9.-]+):(/[A-Za-z0-9._/-]+)\s+\(`)

// s07CopiedLogs lists, once each and in order, the copied logs of the run's
// database that the verbose heartbeat status out names.
func s07CopiedLogs(out string) []s07CopiedLog {
	var logs []s07CopiedLog
	for _, m := range s07CopiedLogLine.FindAllStringSubmatch(out, -1) {
		l := s07CopiedLog{Peer: m[2], Dir: m[3]}
		if m[1] == databaseName && !slices.Contains(logs, l) {
			logs = append(logs, l)
		}
	}
	return logs
}

// s07Reading is a first applyinfo reading of one member's copied log, to
// be read again once s07ApplyInfoInterval has passed.
type s07Reading struct {
	s07CopiedLog
	At time.Time
}

// The kinds of HA log a member is expected to hold in $CUBRID/log, which the
// Instance Manager's entrypoint mounts from the member's data volume
// (cubrid-log, #856). A kind the listing does not show is recorded missing.
const (
	s07KindMaster     = "cub_master log"
	s07KindServer     = "server error log"
	s07KindCopylogdb  = "copylogdb log"
	s07KindApplylogdb = "applylogdb log"
)

// s07NotRead is a source the capture did not read because one of its bounds
// was reached first; Reason says which.
const s07NotRead = evidence.LogStatus("not read")

// s07EngineLogArg0 is the name the capture's shell commands run under.
const s07EngineLogArg0 = "s07-engine-log"

// The commands of the capture. Each only reads, and each is bounded by
// timeout or by the size it prints:
//   - the verbose heartbeat status runs once the cut is removed, as the
//     diagnostic log bundle's heartbeat status does (it pings the hosts of
//     ha_ping_hosts, POC-52);
//   - cubrid changemode with no mode only asks the server for its HA mode, as
//     the identity check of test/sqlcheck and the member state of S12 do;
//   - the tail reads one file the listing named and AllowedEngineLog allows.
const (
	s07HeartbeatVerbose = `PATH="${CUBRID}/bin:${PATH}" timeout 15 cubrid heartbeat status -v 2>&1 | head -c "$1"`
	s07HAMode           = `PATH="${CUBRID}/bin:${PATH}" timeout 15 cubrid changemode "$2@localhost" 2>&1 | head -c "$1"`
	s07TailEngineLog    = `cd "$CUBRID/log" && tail -n "$1" -- "$3" | tail -c "$2"`
	// s07ApplyInfo reads the applier's position in the copied log in $3, in
	// the form POC-66 ran on the lab's image
	// (docs/poc/poc66/applyinfo-sampler.sh): cubrid applyinfo -L <copied
	// log> -a <database>.
	s07ApplyInfo = `PATH="${CUBRID}/bin:${PATH}" timeout 15 cubrid applyinfo -L "$3" -a "$2" 2>&1 | head -c "$1"`
	// s07Sockets lists the member's TCP sockets with their owners and
	// internal state, as POC-66 did in the member as its CUBRID user
	// (docs/poc/poc66/trial.sh); the member runs as that user, so -p names
	// CUBRID's own processes: copylogdb and applylogdb run as cub_admin
	// (docs/poc/poc66/sockets.py). /usr/sbin and /sbin are added in case the
	// container's PATH lacks them.
	s07Sockets = `PATH="${PATH}:/usr/sbin:/sbin" timeout 15 ss -tnpio 2>&1 | head -c "$1"`
)

// s07EngineLogs is what CUBRID reported and logged on each member when the
// cluster did not settle after the cut was removed, kept before the run
// namespace is deleted. It is reported and never judged.
type s07EngineLogs struct {
	At      time.Time                       `json:"at"`
	Members map[string]*s07MemberEngineLogs `json:"members"`
}

// s07MemberEngineLogs is one member's sources, in the order they were read,
// and the kinds of HA log its listing did not show. Missing is left out when
// the listing could not be read: nothing is claimed missing unread.
type s07MemberEngineLogs struct {
	Sources []s07EngineLogSource `json:"sources"`
	Missing []string             `json:"missing,omitempty"`
}

// s07EngineLogSource is one command or file of the capture: what became of
// it, why when it is not whole, and the evidence file that holds it.
type s07EngineLogSource struct {
	Origin string             `json:"origin"`
	Status evidence.LogStatus `json:"status"`
	Reason string             `json:"reason,omitempty"`
	File   string             `json:"file,omitempty"`
	// At is when an applyinfo reading was started, so that the two
	// readings of one copied log can be set apart in time.
	At *time.Time `json:"at,omitempty"`
}

// s07LogKind names the kind of HA log of a file of $CUBRID/log, or "" for a
// file of another kind, such as a Broker's.
func s07LogKind(rel string) string {
	switch {
	case strings.HasPrefix(rel, "server/"):
		return s07KindServer
	case strings.HasSuffix(rel, "_master.err"):
		return s07KindMaster
	case strings.HasSuffix(rel, "_copylogdb.err"):
		return s07KindCopylogdb
	case strings.Contains(rel, "applylogdb"):
		return s07KindApplylogdb
	}
	return ""
}

// captureS07EngineLogs keeps, within s07EngineLogCaptureBound, each member's
// verbose heartbeat status, two readings of the applier of each of its
// copied logs, its server's HA mode, its TCP sockets and the tail of its HA
// logs under the variant's engine-logs directory, redacted, and records
// them. It is made when the cluster did not settle, so that why a member
// stayed to-be-active is not lost with the namespace. A source that could
// not be read is recorded unreadable; nothing here blocks cleanup or changes
// the verdict.
//
// The members are read one after another, each within an equal share of what
// is left of a pass's bound, so a member that hangs cannot take the time of
// the members read after it. The first pass, within the first half of
// s07EngineLogBound, reads the heartbeat status and the first applyinfo
// readings; the second, within the rest of it, the other sources. The
// interval before the second applyinfo readings so overlaps the reading of
// the logs, and as it is at most half of s07EngineLogBound, the second
// readings are due by the end of the second pass and take at most
// s07ApplyInfoBound after it.
func (h *haRun) captureS07EngineLogs(ctx context.Context, rec *s07Record) {
	ctx, cancel := context.WithTimeout(ctx, s07EngineLogCaptureBound())
	defer cancel()
	c := &s07EngineLogs{At: time.Now().UTC(), Members: map[string]*s07MemberEngineLogs{}}
	read := int64(0)
	start := time.Now()
	readings := map[string][]s07Reading{}
	share := func(end time.Time, i int) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, time.Until(end)/time.Duration(len(h.members)-i))
	}
	for i, pod := range h.members {
		memberCtx, cancelMember := share(start.Add(s07EngineLogBound/2), i)
		c.Members[pod] = &s07MemberEngineLogs{Sources: []s07EngineLogSource{}}
		readings[pod] = h.readS07Appliers(memberCtx, rec.dir(), pod, c.Members[pod], &read)
		cancelMember()
	}
	for i, pod := range h.members {
		memberCtx, cancelMember := share(start.Add(s07EngineLogBound), i)
		h.captureS07Member(memberCtx, rec.dir(), pod, c.Members[pod], &read)
		cancelMember()
	}
	h.readS07AppliersAgain(ctx, rec.dir(), c, readings, &read)
	rec.UnsettledEngineLogs = c
}

// s07Keep runs one command of the capture on pod, stores what it printed as
// name in the member's engine-logs directory and adds the source to m. It
// returns the output as read.
func (h *haRun) s07Keep(ctx context.Context, variant, pod string, m *s07MemberEngineLogs, read *int64,
	origin, name string, lines int, head bool, command func(room int64) []string) string {
	if origin == s07OriginListing {
		// The listing is needed to know which logs exist; like the
		// diagnostic log bundle's, it is bounded on its own.
		read = new(int64)
	}
	s, out := h.s07EngineSource(ctx, pod, s07File(variant, "engine-logs/"+pod+"/"+name), lines, head, read,
		command)
	s.Origin = origin
	m.Sources = append(m.Sources, s)
	return out
}

// readS07Appliers keeps the member's verbose heartbeat status and the first
// applyinfo reading of each copied log it names, and returns the readings to
// take again. A member whose heartbeat status names no copied log has one
// unreadable applyinfo source that says why.
func (h *haRun) readS07Appliers(ctx context.Context, variant, pod string, m *s07MemberEngineLogs,
	read *int64) []s07Reading {
	beat := h.s07Keep(ctx, variant, pod, m, read, s07OriginHeartbeat, "heartbeat-status-verbose.txt", 0, true,
		func(room int64) []string {
			return []string{"sh", "-c", s07HeartbeatVerbose, s07EngineLogArg0, strconv.FormatInt(room+1, 10)}
		})
	copied := s07CopiedLogs(beat)
	if len(copied) == 0 {
		reason := "the verbose heartbeat status names no copylogdb process of " + databaseName
		if beat == "" {
			reason = "the verbose heartbeat status, which names the copied logs, could not be read"
		}
		m.Sources = append(m.Sources, s07EngineLogSource{Origin: s07OriginApplyInfo, Status: evidence.LogUnavailable,
			Reason: reason})
	}
	var readings []s07Reading
	for _, l := range copied {
		s := h.s07ApplyInfoReading(ctx, variant, pod, l, "first", read)
		m.Sources = append(m.Sources, s)
		if s.Status != s07NotRead {
			readings = append(readings, s07Reading{s07CopiedLog: l, At: *s.At})
		}
	}
	return readings
}

// readS07AppliersAgain takes the second applyinfo reading of each copied log
// read once, no earlier than s07ApplyInfoInterval after its first. They end
// within s07ApplyInfoBound of the time the last of them is due, each member
// within an equal share of what is left of it.
func (h *haRun) readS07AppliersAgain(ctx context.Context, variant string, c *s07EngineLogs,
	readings map[string][]s07Reading, read *int64) {
	var pods []string
	due := time.Now()
	for _, pod := range h.members {
		for _, r := range readings[pod] {
			if at := r.At.Add(s07ApplyInfoInterval); at.After(due) {
				due = at
			}
		}
		if len(readings[pod]) > 0 {
			pods = append(pods, pod)
		}
	}
	end := due.Add(s07ApplyInfoBound)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(end) {
		end = deadline
	}
	wait := func(ctx context.Context, until time.Time) {
		select {
		case <-ctx.Done():
		case <-time.After(time.Until(until)):
		}
	}
	for i, pod := range pods {
		wait(ctx, readings[pod][0].At.Add(s07ApplyInfoInterval))
		memberCtx, cancelMember := context.WithTimeout(ctx, time.Until(end)/time.Duration(len(pods)-i))
		m := c.Members[pod]
		for _, r := range readings[pod] {
			wait(memberCtx, r.At.Add(s07ApplyInfoInterval))
			m.Sources = append(m.Sources, h.s07ApplyInfoReading(memberCtx, variant, pod, r.s07CopiedLog, "second",
				read))
		}
		cancelMember()
	}
}

// s07ApplyInfoReading reads cubrid applyinfo for one copied log of pod and
// records when the reading was started.
func (h *haRun) s07ApplyInfoReading(ctx context.Context, variant, pod string, l s07CopiedLog, reading string,
	read *int64) s07EngineLogSource {
	at := time.Now().UTC()
	file := s07File(variant, "engine-logs/"+pod+"/applyinfo-"+l.Peer+"-"+reading+".txt")
	s, _ := h.s07EngineSource(ctx, pod, file, 0, true, read, func(room int64) []string {
		return []string{"sh", "-c", s07ApplyInfo, s07EngineLogArg0, strconv.FormatInt(room+1, 10), databaseName, l.Dir}
	})
	s.Origin, s.At = s07ApplyInfoOrigin(l.Dir, reading), &at
	return s
}

// captureS07Member reads the member's other sources into m: its server's HA
// mode, its TCP sockets, the listing of its $CUBRID/log and the HA logs the
// listing names; read counts the bytes the capture has read on all members.
func (h *haRun) captureS07Member(ctx context.Context, variant, pod string, m *s07MemberEngineLogs, read *int64) {
	keep := func(origin, name string, lines int, head bool, command func(room int64) []string) string {
		return h.s07Keep(ctx, variant, pod, m, read, origin, name, lines, head, command)
	}
	keep(s07OriginHAMode, "changemode.txt", 0, true, func(room int64) []string {
		return []string{"sh", "-c", s07HAMode, s07EngineLogArg0, strconv.FormatInt(room+1, 10), databaseName}
	})
	keep(s07OriginSockets, "sockets.txt", 0, true, func(room int64) []string {
		return []string{"sh", "-c", s07Sockets, s07EngineLogArg0, strconv.FormatInt(room+1, 10)}
	})
	listed := keep(s07OriginListing, "cubrid-log-listing.txt", 0, true, func(int64) []string {
		return evidence.ListEngineLogs
	})
	if last := m.Sources[len(m.Sources)-1]; last.Status == evidence.LogUnavailable || last.Status == s07NotRead {
		return
	}
	found := map[string]bool{}
	var files []string
	for line := range strings.Lines(listed) {
		rel := strings.TrimSpace(line)
		kind := s07LogKind(rel)
		if kind == "" || !evidence.AllowedEngineLog(rel) {
			continue
		}
		found[kind] = true
		files = append(files, rel)
	}
	for _, kind := range []string{s07KindMaster, s07KindServer, s07KindCopylogdb, s07KindApplylogdb} {
		if !found[kind] {
			m.Missing = append(m.Missing, kind)
		}
	}
	for i, rel := range files {
		origin := "$CUBRID/log/" + rel
		if i >= s07EngineLogFiles {
			m.Sources = append(m.Sources, s07EngineLogSource{Origin: origin, Status: s07NotRead,
				Reason: fmt.Sprintf("older than the newest %d files, the most read on one member", s07EngineLogFiles)})
			continue
		}
		keep(origin, "cubrid-log/"+rel, s07EngineLogLines, false, func(room int64) []string {
			return []string{"sh", "-c", s07TailEngineLog, s07EngineLogArg0, strconv.Itoa(s07EngineLogLines + 1),
				strconv.FormatInt(room+1, 10), rel}
		})
	}
}

// s07EngineSource runs one command of the capture on pod and stores what it
// printed in file, within the capture's bounds: at most lines lines when
// lines is set, and at most the bytes left, whole lines only. head keeps the
// start of a command's output, else the end of a log is kept. It returns the
// source and the output as read.
func (h *haRun) s07EngineSource(ctx context.Context, pod, file string, lines int, head bool, read *int64,
	command func(room int64) []string) (s07EngineLogSource, string) {
	room := min(s07EngineLogBytes, s07EngineLogTotal-*read)
	if room <= 0 {
		return s07EngineLogSource{Status: s07NotRead, Reason: fmt.Sprintf(
			"the capture's size bound of %d bytes was reached", s07EngineLogTotal)}, ""
	}
	if err := ctx.Err(); err != nil {
		return s07EngineLogSource{Status: evidence.LogUnavailable, Reason: fmt.Sprintf(
			"the member's share of the capture's time bound of %s ended: %v", s07EngineLogBound, err)}, ""
	}
	out, err := h.member(ctx, pod, command(room)...)
	if err != nil {
		reason := strings.Join(strings.Fields(cmpOr(out, "no output")), " ") + ": " + err.Error()
		return s07EngineLogSource{Status: evidence.LogUnavailable,
			Reason: s07Short(string(h.redactor.Redact([]byte(reason))))}, ""
	}
	*read += min(int64(len(out)), room)
	if out == "" {
		return s07EngineLogSource{Status: evidence.LogEmpty, Reason: "the member answered and printed nothing"}, ""
	}
	s := s07EngineLogSource{Status: evidence.LogCollected}
	text := out
	all := strings.SplitAfter(text, "\n")
	if all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	if lines > 0 && len(all) > lines {
		text = strings.Join(all[len(all)-lines:], "")
		s.Status, s.Reason = evidence.LogTruncated, fmt.Sprintf("the newest %d lines are kept: older lines exist", lines)
	}
	if int64(len(text)) > room {
		if head {
			text = text[:room]
			text = text[:strings.LastIndexByte(text, '\n')+1]
		} else {
			cut := int64(len(text)) - room
			midLine := text[cut-1] != '\n'
			text = text[cut:]
			if midLine {
				text = text[strings.IndexByte(text, '\n')+1:]
			}
		}
		s.Status, s.Reason = evidence.LogTruncated, fmt.Sprintf("the %s whole lines within %d bytes are kept",
			map[bool]string{true: "first", false: "newest"}[head], room)
	}
	if text == "" {
		s.Reason += ": no whole line fits"
		return s, out
	}
	if err := h.write(file, []byte(text)); err != nil {
		s.Status, s.Reason = evidence.LogUnavailable, s07Short(string(h.redactor.Redact([]byte("write "+file+": "+
			err.Error()))))
		return s, out
	}
	s.File = file
	return s, out
}
