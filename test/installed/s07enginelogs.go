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
	"strconv"
	"strings"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// The bounds of the engine log capture made when the cluster did not settle.
// They are variables so that tests can make them small.
var (
	// s07EngineLogBound bounds the whole capture, so that the run namespace
	// is still deleted within the run's deadline; each member gets an equal
	// share of what is left of it, as in the rows snapshot.
	s07EngineLogBound = 2 * time.Minute
	// s07EngineLogLines bounds the lines kept of one log file.
	s07EngineLogLines = 2000
	// s07EngineLogBytes bounds what is kept of one command or file, as the
	// diagnostic log bundle's SourceBytes does.
	s07EngineLogBytes int64 = 1 << 20
	// s07EngineLogTotal bounds what the capture reads in all, as the bundle's
	// BundleBytes does.
	s07EngineLogTotal int64 = 16 << 20
)

// s07EngineLogFiles is the most log files read on one member, newest first,
// as the diagnostic log bundle's EngineFiles.
const s07EngineLogFiles = 8

// The origins of the sources of the capture, as the record names them.
const (
	s07OriginHeartbeat = "cubrid heartbeat status -v"
	s07OriginHAMode    = "cubrid changemode " + databaseName + "@localhost"
	s07OriginListing   = "listing of $CUBRID/log"
)

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

// captureS07EngineLogs keeps, within s07EngineLogBound, each member's
// verbose heartbeat status, its server's HA mode and the tail of its HA logs
// under the variant's engine-logs directory, redacted, and records them. It
// is made when the cluster did not settle, so that why a member stayed
// to-be-active is not lost with the namespace. A source that could not be
// read is recorded unreadable; nothing here blocks cleanup or changes the
// verdict.
//
// The members are read one after another, each within an equal share of what
// is left of the bound, so a member that hangs cannot take the time of the
// members read after it.
func (h *haRun) captureS07EngineLogs(ctx context.Context, rec *s07Record) {
	ctx, cancel := context.WithTimeout(ctx, s07EngineLogBound)
	defer cancel()
	c := &s07EngineLogs{At: time.Now().UTC(), Members: map[string]*s07MemberEngineLogs{}}
	read := int64(0)
	deadline, _ := ctx.Deadline()
	for i, pod := range h.members {
		memberCtx, cancelMember := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(h.members)-i))
		c.Members[pod] = h.captureS07Member(memberCtx, rec.Variant, pod, &read)
		cancelMember()
	}
	rec.UnsettledEngineLogs = c
}

// captureS07Member reads one member's sources; read counts the bytes the
// capture has read on all members.
func (h *haRun) captureS07Member(ctx context.Context, variant, pod string, read *int64) *s07MemberEngineLogs {
	m := &s07MemberEngineLogs{Sources: []s07EngineLogSource{}}
	dir := "engine-logs/" + pod + "/"
	keep := func(origin, name string, lines int, head bool, command func(room int64) []string) string {
		counted := read
		if origin == s07OriginListing {
			// The listing is needed to know which logs exist; like the
			// diagnostic log bundle's, it is bounded on its own.
			counted = new(int64)
		}
		s, out := h.s07EngineSource(ctx, pod, s07File(variant, dir+name), lines, head, counted, command)
		s.Origin = origin
		m.Sources = append(m.Sources, s)
		return out
	}
	keep(s07OriginHeartbeat, "heartbeat-status-verbose.txt", 0, true, func(room int64) []string {
		return []string{"sh", "-c", s07HeartbeatVerbose, s07EngineLogArg0, strconv.FormatInt(room+1, 10)}
	})
	keep(s07OriginHAMode, "changemode.txt", 0, true, func(room int64) []string {
		return []string{"sh", "-c", s07HAMode, s07EngineLogArg0, strconv.FormatInt(room+1, 10), databaseName}
	})
	listed := keep(s07OriginListing, "cubrid-log-listing.txt", 0, true, func(int64) []string {
		return evidence.ListEngineLogs
	})
	if last := m.Sources[len(m.Sources)-1]; last.Status == evidence.LogUnavailable || last.Status == s07NotRead {
		return m
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
	return m
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
