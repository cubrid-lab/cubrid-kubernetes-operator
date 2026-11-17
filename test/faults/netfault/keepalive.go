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
// (#982). It runs the faults' image and only sleeps; like the faults'
// container, it cannot be removed, and the values it sets go away with the
// Pod's network namespace.
const KeepaliveContainer = "netsysctl"

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
// The connections to the other members are left as they are.
//
// Any step that does not hold returns an error that says why, and the
// record keeps what was seen.
func ShortenKeepalive(ctx context.Context, env Env, k Keepalive, members []Endpoint, target, master Endpoint,
	wait time.Duration) (KeepaliveRecord, error) {
	rec := KeepaliveRecord{Requested: k.values(), Container: KeepaliveContainer, Members: []KeepaliveMember{},
		Target: target.Name, Master: master.Name, SocketsBefore: []Socket{}, Reset: []Socket{}}
	fail := func(err error) (KeepaliveRecord, error) {
		rec.Unmet = err.Error()
		return rec, err
	}
	if err := k.Validate(); err != nil {
		return fail(err)
	}
	for _, e := range members {
		m, err := setKeepalive(ctx, env, e, k)
		rec.Members = append(rec.Members, m)
		if err != nil {
			return fail(err)
		}
	}
	memberIPs := make([]string, 0, len(members))
	for _, e := range members {
		memberIPs = append(memberIPs, e.IP)
	}
	port := strconv.Itoa(master.Port)
	toMaster := master.IP + ":" + port
	sockets, err := readSockets(ctx, env, target, master.Port)
	if err != nil {
		return fail(err)
	}
	for _, s := range sockets {
		if host, _, _ := strings.Cut(s.Peer, ":"); slices.Contains(memberIPs, host) {
			rec.SocketsBefore = append(rec.SocketsBefore, s)
		}
	}
	out, err := shellIn(ctx, env, target, KeepaliveContainer, fmt.Sprintf(": netfault sockets-reset %[1]s %[2]s\n"+
		"ss -K -tnoH state established '( dst %[1]s and dport = :%[2]s )'", master.IP, port))
	if err != nil {
		return fail(fmt.Errorf("reset %s's connections to %s's port %s: %w", target.Name, master.Name, port, err))
	}
	now := time.Now().UTC()
	rec.ResetAt = &now
	if rec.Reset, err = parseSockets(out); err != nil {
		return fail(fmt.Errorf("%s: %w", target.Name, err))
	}
	var reset []string
	for _, s := range rec.Reset {
		reset = append(reset, s.Local)
	}
	deadline := time.Now().Add(wait)
	var seen []Socket
	for checked := false; ; checked = true {
		start := time.Now()
		sockets, err := readSockets(ctx, env, target, master.Port)
		if err != nil {
			return fail(err)
		}
		seen = seen[:0]
		for _, s := range sockets {
			switch {
			case s.Peer != toMaster:
			case slices.Contains(reset, s.Local) && !checked:
				return fail(fmt.Errorf("%s's connection %s to %s is still open after the reset: the kernel may "+
					"not support destroying sockets (CONFIG_INET_DIAG_DESTROY)", target.Name, s.Local, toMaster))
			case slices.Contains(reset, s.Local):
			case s.Keepalive != "" && s.KeepaliveLeftSeconds <= k.Time.Seconds():
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
		if s.Keepalive == "" {
			timers = append(timers, s.Local+" has no keepalive timer")
		} else {
			timers = append(timers, fmt.Sprintf("%s has a keepalive timer of %s, longer than %s", s.Local,
				s.Keepalive, k.Time))
		}
	}
	return fail(fmt.Errorf("%s's new connections to %s's port %s do not use the new keepalive: %s", target.Name,
		master.Name, port, strings.Join(timers, "; ")))
}

// setKeepalive adds the keepalive container to the member's Pod, checks that
// it is in the Pod's network namespace, and writes k there, reading the
// values before and after.
func setKeepalive(ctx context.Context, env Env, e Endpoint, k Keepalive) (KeepaliveMember, error) {
	m := KeepaliveMember{Name: e.Name, Pod: e.Pod, UID: e.UID}
	fail := func(err error) (KeepaliveMember, error) {
		m.Error = err.Error()
		return m, err
	}
	if err := attach(ctx, env, e, keepaliveContainer(env.image())); err != nil {
		return fail(err)
	}
	if err := checkNamespaceIn(ctx, env, e, KeepaliveContainer); err != nil {
		return fail(err)
	}
	before, err := readKeepalive(ctx, env, e)
	if err != nil {
		return fail(err)
	}
	m.Before = &before
	want := k.values()
	out, err := shellIn(ctx, env, e, KeepaliveContainer, fmt.Sprintf(
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

func keepaliveContainer(image string) corev1.EphemeralContainer {
	return corev1.EphemeralContainer{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
		Name: KeepaliveContainer, Image: image, ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"tail", "-f", "/dev/null"},
		SecurityContext: &corev1.SecurityContext{Privileged: new(true), RunAsUser: new(int64(0))},
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
	// ss prints a timer's time left as [<m>min][<s>sec | <s>.<ms>ms | <ms>ms].
	ssTime = regexp.MustCompile(`^(?:(\d+)min)?(?:(\d+)sec|(\d+)\.(\d{3})ms|(\d+)ms)?$`)
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
		s := Socket{Local: fields[2], Peer: fields[3]}
		if m := keepaliveTimer.FindStringSubmatch(line); m != nil {
			left, err := ssSeconds(m[1])
			if err != nil {
				return nil, fmt.Errorf("ss line %q: %w", line, err)
			}
			s.Keepalive, s.KeepaliveLeftSeconds = m[1], left
		}
		sockets = append(sockets, s)
	}
	return sockets, nil
}

func ssSeconds(text string) (float64, error) {
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
