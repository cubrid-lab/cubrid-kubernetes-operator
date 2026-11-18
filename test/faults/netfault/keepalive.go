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

package netfault

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// KeepaliveContainer is the ephemeral container that sets TCP keepalive in
// a member Pod's network namespace. It is privileged: the container runtime
// mounts /proc/sys read-only in any other container, so the faults' own
// container, with NET_ADMIN only, cannot write net.ipv4.tcp_keepalive_*
// (#982). It runs the faults' image and only sleeps, for no longer than the
// step that uses it (keepaliveLifetime at most), so the privileged process
// does not live as long as the Pod. The stopped container stays listed in
// the Pod's spec, which cannot drop an ephemeral container; the values it set
// go away with the Pod's network namespace.
const KeepaliveContainer = "netsysctl"

// keepaliveLifetime bounds how long the keepalive container sleeps: the
// rest of the step's context, plus keepaliveMargin, and this when the
// context has no deadline.
var keepaliveLifetime = 15 * time.Minute

const keepaliveMargin = time.Minute

// minKeepaliveKernel is the first Linux release whose tcp_keepalive_* are per
// network namespace (4.13); on an older kernel a write from one Pod would
// change the node's value.
var minKeepaliveKernel = [2]int{4, 13}

// reconnectedNote says what the reconnected connection rests on.
const reconnectedNote = "the reconnected connection is identified by its addresses only: an established connection " +
	"from the cut-off member to the master's port with a local address that was not listed before the reset; the " +
	"process that owns it is not read"

// The kernel's bounds on the keepalive values (MAX_TCP_KEEPIDLE,
// MAX_TCP_KEEPINTVL, MAX_TCP_KEEPCNT), and its default keepalive time, which
// a shortened time must be under.
const (
	maxKeepaliveInterval    = 32767 * time.Second
	maxKeepaliveProbes      = 127
	defaultKeepaliveTime    = 2 * time.Hour
	keepaliveSysctlDir      = "/proc/sys/net/ipv4"
	keepaliveSysctlTime     = keepaliveSysctlDir + "/tcp_keepalive_time"
	keepaliveSysctlInterval = keepaliveSysctlDir + "/tcp_keepalive_intvl"
	keepaliveSysctlProbes   = keepaliveSysctlDir + "/tcp_keepalive_probes"
)

// Keepalive is the TCP keepalive a namespace's new connections get: idle
// Time before the first probe, Interval between probes, and Probes
// unanswered before the kernel ends the connection.
type Keepalive struct {
	Time, Interval time.Duration
	Probes         int
}

// Validate refuses values the kernel would refuse, values that are not
// whole seconds, and a time that is not shorter than the kernel's default.
func (k Keepalive) Validate() error {
	switch {
	case k.Time < time.Second || k.Time%time.Second != 0 || k.Time >= defaultKeepaliveTime:
		return fmt.Errorf("the keepalive time %s must be whole seconds, at least 1s and under the kernel's "+
			"default of %s", k.Time, defaultKeepaliveTime)
	case k.Interval < time.Second || k.Interval%time.Second != 0 || k.Interval > maxKeepaliveInterval:
		return fmt.Errorf("the keepalive interval %s must be whole seconds, from 1s to %s", k.Interval,
			maxKeepaliveInterval)
	case k.Probes < 1 || k.Probes > maxKeepaliveProbes:
		return fmt.Errorf("the keepalive probes %d must be from 1 to %d", k.Probes, maxKeepaliveProbes)
	}
	return nil
}

func (k Keepalive) values() KeepaliveValues {
	return KeepaliveValues{Time: int(k.Time / time.Second), Interval: int(k.Interval / time.Second),
		Probes: k.Probes}
}

// KeepaliveValues are a namespace's net.ipv4.tcp_keepalive_time,
// tcp_keepalive_intvl and tcp_keepalive_probes.
type KeepaliveValues struct {
	Time     int `json:"tcpKeepaliveTimeSeconds"`
	Interval int `json:"tcpKeepaliveIntervalSeconds"`
	Probes   int `json:"tcpKeepaliveProbes"`
}

// KeepaliveMember is one member's namespace: the values read before the
// write and after it, when they were seen set, and why not when they were
// not.
type KeepaliveMember struct {
	Name   string           `json:"member"`
	Pod    string           `json:"pod"`
	UID    string           `json:"podUID"`
	Kernel string           `json:"kernelRelease,omitempty"`
	Before *KeepaliveValues `json:"before,omitempty"`
	After  *KeepaliveValues `json:"after,omitempty"`
	SetAt  *time.Time       `json:"setAt,omitempty"`
	Error  string           `json:"error,omitempty"`
}

// Socket is one established TCP connection as ss printed it: its local and
// peer address, and the keepalive timer's time left, when it shows one.
type Socket struct {
	Local                string  `json:"local"`
	Peer                 string  `json:"peer"`
	Keepalive            string  `json:"keepaliveTimer,omitempty"`
	KeepaliveLeftSeconds float64 `json:"keepaliveLeftSeconds,omitempty"`
	KeepaliveUnread      bool    `json:"keepaliveTimerUnread,omitempty"`
}

// KeepaliveRecord is what ShortenKeepalive did: the values asked for, each
// member's namespace, and the cut-off member's connections to the members'
// port before the reset, those reset, and the connection to the master
// made again under the new values.
type KeepaliveRecord struct {
	Requested     KeepaliveValues   `json:"requested"`
	Container     string            `json:"container"`
	Members       []KeepaliveMember `json:"members"`
	Target        string            `json:"target"`
	Master        string            `json:"master"`
	SocketsBefore []Socket          `json:"targetSocketsBeforeReset"`
	Reset         []Socket          `json:"targetSocketsReset"`
	ResetAt       *time.Time        `json:"resetAt,omitempty"`
	Reconnected   *Socket           `json:"targetReconnectedSocket,omitempty"`
	ReconnectedAt *time.Time        `json:"reconnectedSeenAt,omitempty"`
	Unmet         string            `json:"unmet,omitempty"`
	Note          string            `json:"note"`
	SSVersion     string            `json:"ssVersion,omitempty"`
}

// ShortenKeepalive sets k in each member's network namespace and reads the
// values back, then makes the target's connections to the master's port
// start again under them, within wait.
//
// Linux reads the namespace's keepalive values when a socket's keepalive
// timer is armed and each time it fires; a connection's timer is first
// armed with the values of when it was made, so a connection made before
// the write keeps its old schedule, up to the 2-hour default, until that
// first expiry. The target's connections to the master's port, which
// CUBRID's log copy uses, were made before the run could write anything:
// when its Pod started. So the run resets them (ss -K, which needs only
// NET_ADMIN and a kernel with socket destroy support) and waits until a new
// connection to the master shows a keepalive timer no longer than k.Time.
// A target with no connection to the master's port before the reset stops
// the step there, so a connection made later cannot count as the new one.
// ss -K prints nothing and exits 0 when the kernel cannot destroy sockets,
// so the reset is judged by the listing: a connection to the master listed
// before it must be gone, and no connection listed before it ever counts as
// the new one. The new one is told apart by its addresses only, not by the
// process that owns it. The connections to the other members are left as
// they are. Each member's kernel release is read and recorded, and one older
// than 4.13, whose keepalive values are not per namespace, is refused.
//
// Any step that does not hold returns an error that says why, and the
// record keeps what was seen.
func ShortenKeepalive(ctx context.Context, env Env, k Keepalive, members []Endpoint, target, master Endpoint,
	wait time.Duration) (KeepaliveRecord, error) {
	rec := KeepaliveRecord{Requested: k.values(), Container: KeepaliveContainer, Members: []KeepaliveMember{},
		Target: target.Name, Master: master.Name, SocketsBefore: []Socket{}, Reset: []Socket{}, Note: reconnectedNote}
	fail := func(err error) (KeepaliveRecord, error) {
		rec.Unmet = err.Error()
		return rec, err
	}
	if err := k.Validate(); err != nil {
		return fail(err)
	}
	lifetime := keepaliveLifetime
	if deadline, ok := ctx.Deadline(); ok {
		lifetime = min(time.Until(deadline)+keepaliveMargin, keepaliveLifetime)
	}
	for _, e := range members {
		m, err := setKeepalive(ctx, env, e, k, lifetime)
		rec.Members = append(rec.Members, m)
		if err != nil {
			return fail(err)
		}
	}
	memberIPs := make([]string, 0, len(members))
	for _, e := range members {
		memberIPs = append(memberIPs, normalAddr(e.IP))
	}
	port := strconv.Itoa(master.Port)
	toMaster := normalAddr(master.IP) + ":" + port
	version, err := shellIn(ctx, env, target, KeepaliveContainer, ": netfault ss-version\nss -V")
	if err != nil {
		return fail(fmt.Errorf("read the ss version: %w", err))
	}
	rec.SSVersion = strings.TrimSpace(version)
	sockets, err := readSockets(ctx, env, target, master.Port)
	if err != nil {
		return fail(err)
	}
	var before, expected []string
	rec.SocketsBefore, before, expected = toMembers(sockets, memberIPs, toMaster)
	// Without a connection to the master listed before the reset, nothing
	// would be reset, and a connection made later, for a reason the run did
	// not cause, would look like the one CUBRID made again. So the step stops
	// before the reset instead of recording a reset that did not happen.
	if len(expected) == 0 {
		return fail(fmt.Errorf("%s had no connection to %s's port %s before the reset, so there is no "+
			"connection to make again under the new keepalive", target.Name, master.Name, port))
	}
	// An IPv4 peer may be listed as IPv4-mapped on a dual-stack socket.
	out, err := shellIn(ctx, env, target, KeepaliveContainer, fmt.Sprintf(": netfault sockets-reset %[1]s %[2]s\n"+
		"ss -K -tnoH state established '( ( dst %[1]s or dst [::ffff:%[1]s] ) and dport = :%[2]s )'",
		master.IP, port))
	if err != nil {
		return fail(fmt.Errorf("reset %s's connections to %s's port %s: %w", target.Name, master.Name, port, err))
	}
	now := time.Now().UTC()
	rec.ResetAt = &now
	if rec.Reset, err = parseSockets(out); err != nil {
		return fail(fmt.Errorf("%s: %w", target.Name, err))
	}
	deadline := time.Now().Add(wait)
	var seen []Socket
	for {
		start := time.Now()
		sockets, err := readSockets(ctx, env, target, master.Port)
		if err != nil {
			return fail(err)
		}
		seen = seen[:0]
		for _, s := range sockets {
			switch {
			case s.Peer != toMaster:
			case slices.Contains(expected, s.Local):
				// ss -K prints nothing and exits 0 when the kernel answers
				// SOCK_DESTROY with EOPNOTSUPP, so the listing decides.
				printed := fmt.Sprintf("ss -K reported %d reset", len(rec.Reset))
				if len(rec.Reset) == 0 {
					printed = "ss -K reported none reset"
				}
				return fail(fmt.Errorf("%s's connection %s to %s is still open after the reset (%s): the kernel "+
					"may not support destroying sockets (CONFIG_INET_DIAG_DESTROY)", target.Name, s.Local, toMaster,
					printed))
			case slices.Contains(before, s.Local):
				// A connection listed before the reset is never the new one,
				// whatever its timer shows.
			case s.Keepalive != "" && !s.KeepaliveUnread && s.KeepaliveLeftSeconds <= k.Time.Seconds():
				at := time.Now().UTC()
				rec.Reconnected, rec.ReconnectedAt = &s, &at
				return rec, nil
			default:
				seen = append(seen, s)
			}
		}
		if time.Now().After(deadline) {
			break
		}
		if err := env.pause(ctx, time.Since(start)); errors.Is(err, errNoTimeLeft) {
			break
		} else if err != nil {
			return fail(err)
		}
	}
	if len(seen) == 0 {
		return fail(fmt.Errorf("no new connection from %s to %s's port %s within %s after the reset", target.Name,
			master.Name, port, wait))
	}
	var timers []string
	for _, s := range seen {
		switch {
		case s.Keepalive == "":
			timers = append(timers, s.Local+" has no keepalive timer")
		case s.KeepaliveUnread:
			timers = append(timers, fmt.Sprintf("%s has a keepalive timer %q that could not be read", s.Local,
				s.Keepalive))
		default:
			timers = append(timers, fmt.Sprintf("%s has a keepalive timer of %s, longer than %s", s.Local,
				s.Keepalive, k.Time))
		}
	}
	return fail(fmt.Errorf("%s's new connections to %s's port %s do not use the new keepalive: %s", target.Name,
		master.Name, port, strings.Join(timers, "; ")))
}

// toMembers returns the sockets to a member's address, their local
// addresses (before) and the local addresses of those to the master
// (expected), which the reset must take away.
func toMembers(sockets []Socket, memberIPs []string, toMaster string) (toMember []Socket, before,
	expected []string) {
	toMember = []Socket{}
	for _, s := range sockets {
		if host, _, _ := strings.Cut(s.Peer, ":"); slices.Contains(memberIPs, host) {
			toMember = append(toMember, s)
			before = append(before, s.Local)
			if s.Peer == toMaster {
				expected = append(expected, s.Local)
			}
		}
	}
	return toMember, before, expected
}

// setKeepalive adds the keepalive container to the member's Pod, checks that
// it is in the Pod's network namespace, and writes k there, reading the
// values before and after.
func setKeepalive(ctx context.Context, env Env, e Endpoint, k Keepalive, lifetime time.Duration) (KeepaliveMember,
	error) {
	m := KeepaliveMember{Name: e.Name, Pod: e.Pod, UID: e.UID}
	fail := func(err error) (KeepaliveMember, error) {
		m.Error = err.Error()
		return m, err
	}
	if err := attach(ctx, env, e, keepaliveContainer(env.image(), lifetime)); err != nil {
		return fail(err)
	}
	if err := checkNamespaceIn(ctx, env, e, KeepaliveContainer); err != nil {
		return fail(err)
	}
	out, err := shellIn(ctx, env, e, KeepaliveContainer, ": netfault kernel\nuname -r")
	if err != nil {
		return fail(err)
	}
	m.Kernel = strings.TrimSpace(out)
	if err := checkKernel(m.Kernel); err != nil {
		return fail(fmt.Errorf("%s: %w", e.Name, err))
	}
	before, err := readKeepalive(ctx, env, e)
	if err != nil {
		return fail(err)
	}
	m.Before = &before
	want := k.values()
	out, err = shellIn(ctx, env, e, KeepaliveContainer, fmt.Sprintf(
		": netfault keepalive-write %d %d %d\n"+
			"{ echo %[1]d > %[4]s && echo %[2]d > %[5]s && echo %[3]d > %[6]s; } 2>&1",
		want.Time, want.Interval, want.Probes, keepaliveSysctlTime, keepaliveSysctlInterval, keepaliveSysctlProbes))
	if err != nil {
		return fail(fmt.Errorf("write the keepalive values: %w: %s", err, strings.TrimSpace(out)))
	}
	after, err := readKeepalive(ctx, env, e)
	if err != nil {
		return fail(err)
	}
	m.After = &after
	if after != want {
		return fail(fmt.Errorf("%s: read back %+v after writing %+v", e.Name, after, want))
	}
	now := time.Now().UTC()
	m.SetAt = &now
	return m, nil
}

var kernelRelease = regexp.MustCompile(`^(\d+)\.(\d+)`)

// checkKernel refuses a kernel release older than minKeepaliveKernel, or one
// it cannot read.
func checkKernel(release string) error {
	m := kernelRelease.FindStringSubmatch(release)
	if m == nil {
		return fmt.Errorf("unexpected kernel release %q: per-namespace TCP keepalive cannot be confirmed", release)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < minKeepaliveKernel[0] || (major == minKeepaliveKernel[0] && minor < minKeepaliveKernel[1]) {
		return fmt.Errorf("kernel %s is older than %d.%d, the first with per-namespace TCP keepalive: a write "+
			"would change the node's value", release, minKeepaliveKernel[0], minKeepaliveKernel[1])
	}
	return nil
}

func keepaliveContainer(image string, lifetime time.Duration) corev1.EphemeralContainer {
	return corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
		Name: KeepaliveContainer, Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{"sleep", strconv.Itoa(max(int(lifetime/time.Second), 1))},
		// The member Pods set runAsNonRoot at Pod level, which a container
		// inherits: one that runs as root must say false, or the kubelet
		// refuses to start it with CreateContainerConfigError (POC-39).
		SecurityContext: &corev1.SecurityContext{Privileged: new(true), RunAsUser: new(int64(0)),
			RunAsNonRoot: new(false)},
	}}
}

// readKeepalive reads the namespace's three keepalive values.
func readKeepalive(ctx context.Context, env Env, e Endpoint) (KeepaliveValues, error) {
	out, err := shellIn(ctx, env, e, KeepaliveContainer, fmt.Sprintf(": netfault keepalive-read\ncat %s %s %s",
		keepaliveSysctlTime, keepaliveSysctlInterval, keepaliveSysctlProbes))
	if err != nil {
		return KeepaliveValues{}, err
	}
	fields := strings.Fields(out)
	if len(fields) != 3 {
		return KeepaliveValues{}, fmt.Errorf("%s: unexpected keepalive values %q", e.Name, out)
	}
	var n [3]int
	for i, f := range fields {
		if n[i], err = strconv.Atoi(f); err != nil {
			return KeepaliveValues{}, fmt.Errorf("%s: keepalive values %q: %w", e.Name, out, err)
		}
	}
	return KeepaliveValues{Time: n[0], Interval: n[1], Probes: n[2]}, nil
}

// readSockets reads the endpoint's established TCP connections to port.
func readSockets(ctx context.Context, env Env, e Endpoint, port int) ([]Socket, error) {
	out, err := shellIn(ctx, env, e, KeepaliveContainer, fmt.Sprintf(": netfault sockets %[1]d\n"+
		"ss -tnoH state established '( dport = :%[1]d )'", port))
	if err != nil {
		return nil, err
	}
	sockets, err := parseSockets(out)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", e.Name, err)
	}
	return sockets, nil
}

var (
	keepaliveTimer = regexp.MustCompile(`timer:\(keepalive,([^,]*),\d+\)`)
	// ss prints a timer's time left as [<m>min][<s>sec | <s>.<ms>sec | <ms>ms]
	// since iproute2 6.11, which the pinned image has (6.15.0); before 6.11
	// it printed <s>.<ms>ms for <s>.<ms>sec (Debian #1054044). Both mean
	// seconds and milliseconds.
	ssTime = regexp.MustCompile(`^(?:(\d+)min)?(?:(\d+)sec|(\d+)\.(\d{3})(?:sec|ms)|(\d+)ms)?$`)
)

// parseSockets reads `ss -tnoH state established`: receive and send queue,
// local and peer address, then the timer when there is one.
func parseSockets(out string) ([]Socket, error) {
	sockets := []Socket{}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 4 {
			return nil, fmt.Errorf("unexpected ss line %q", line)
		}
		_, err1 := strconv.Atoi(fields[0])
		_, err2 := strconv.Atoi(fields[1])
		if errors.Join(err1, err2) != nil {
			return nil, fmt.Errorf("unexpected ss line %q", line)
		}
		s := Socket{Local: normalAddrPort(fields[2]), Peer: normalAddrPort(fields[3])}
		if m := keepaliveTimer.FindStringSubmatch(line); m != nil {
			// A timer that cannot be read is kept as printed and marked; it
			// never counts as the new keepalive.
			left, err := ssSeconds(m[1])
			s.Keepalive, s.KeepaliveLeftSeconds, s.KeepaliveUnread = m[1], left, err != nil
		}
		sockets = append(sockets, s)
	}
	return sockets, nil
}

// normalAddrPort writes an address and port the same way whichever form ss
// printed: an IPv4-mapped IPv6 address as its IPv4 address. One that does
// not parse is kept as it is.
func normalAddrPort(text string) string {
	ap, err := netip.ParseAddrPort(text)
	if err != nil {
		return text
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()).String()
}

// normalAddr is normalAddrPort for an address alone.
func normalAddr(text string) string {
	a, err := netip.ParseAddr(text)
	if err != nil {
		return text
	}
	return a.Unmap().String()
}

// ssSeconds reads a timer's time left; an overdue timer, which ss prints
// with a leading minus, has none.
func ssSeconds(text string) (float64, error) {
	if strings.HasPrefix(text, "-") {
		if _, err := ssSeconds(text[1:]); err != nil {
			return 0, err
		}
		return 0, nil
	}
	m := ssTime.FindStringSubmatch(text)
	if m == nil {
		return 0, fmt.Errorf("unexpected timer %q", text)
	}
	n := func(s string) float64 {
		v, _ := strconv.Atoi(s)
		return float64(v)
	}
	return n(m[1])*60 + n(m[2]) + n(m[3]) + n(m[4])/1000 + n(m[5])/1000, nil
}
