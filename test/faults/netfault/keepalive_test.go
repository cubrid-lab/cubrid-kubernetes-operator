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
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The tests in this file cover how the network partition lane shortens TCP
// keepalive in the members' network namespaces before the cut (#982): the
// values are written through a privileged ephemeral container and read back,
// and the cut-off member's connection to the master's port is reset so that
// CUBRID makes it again under the new values. A socket keeps the keepalive
// schedule it was armed with, so a value set after a connection was made
// would not reach it.

// fakeSocket is one established TCP connection in a fake namespace.
type fakeSocket struct {
	local, peer string
	// keepalive is what ss prints inside timer:(keepalive,...), empty for a
	// socket without SO_KEEPALIVE.
	keepalive string
}

// fakeSysctlNS is the keepalive state of one Pod's network namespace.
type fakeSysctlNS struct {
	ip                    string
	time, intvl, probes   int
	sockets               []fakeSocket
	nextPort              int
	reconnectIn           int // reads of the sockets until a reset connection is made again; -1 never
	pendingReconnect      []string
	pendingSinceReset     int
	keepaliveOnReconnect  bool
	readOnly, ignoreWrite bool
	// ignoreDestroy: ss -K prints nothing and exits 0, and the sockets stay,
	// as on a kernel that answers SOCK_DESTROY with EOPNOTSUPP. rearmOld
	// shows every socket with a short keepalive timer, as an old socket
	// whose first expiry came after the write.
	ignoreDestroy, rearmOld bool
	kernel                  string
}

// fakeSysctl answers the keepalive container's commands.
type fakeSysctl struct {
	mu      sync.Mutex
	podIP   map[string]string
	execIn  map[string]string
	ns      map[string]*fakeSysctlNS // by IP
	scripts []string
}

func newFakeSysctl() *fakeSysctl {
	s := &fakeSysctl{podIP: testPods, execIn: map[string]string{}, ns: map[string]*fakeSysctlNS{}}
	for _, ip := range testPods {
		s.ns[ip] = &fakeSysctlNS{ip: ip, time: 7200, intvl: 75, probes: 9, nextPort: 50000,
			keepaliveOnReconnect: true, reconnectIn: 1, kernel: "5.15.0-203-generic"}
	}
	// The cut-off member copies the master's log over a connection made
	// before the run changed anything, armed with the 2-hour default; and
	// another member's log over a second one. The master copies the cut-off
	// member's log over a connection made as early (#1111).
	s.ns[testPods[target]].sockets = []fakeSocket{
		{local: testPods[target] + ":48794", peer: testPods[slave1] + ":1523", keepalive: "105min"},
		{local: testPods[target] + ":48800", peer: testPods[slave2] + ":1523", keepalive: "104min"},
	}
	s.ns[testPods[slave1]].sockets = []fakeSocket{
		{local: testPods[slave1] + ":47232", peer: testPods[target] + ":1523", keepalive: "114min"},
	}
	return s
}

var (
	keepaliveWriteArg = regexp.MustCompile(`^: netfault keepalive-write (\d+) (\d+) (\d+)`)
	socketsResetArg   = regexp.MustCompile(`^: netfault sockets-reset (\S+) (\d+)`)
)

func (s *fakeSysctl) kubectl(ctx context.Context, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(args) != 10 || args[2] != "exec" || args[5] != KeepaliveContainer {
		return "", fmt.Errorf("unexpected kubectl call %q", args)
	}
	pod, script := args[3], args[9]
	first, _, _ := strings.Cut(script, "\n")
	op := strings.Fields(first)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scripts = append(s.scripts, pod+": "+first)
	ip := s.podIP[pod]
	if in, ok := s.execIn[pod]; ok {
		ip = in
	}
	ns := s.ns[ip]
	switch op[2] {
	case "addr":
		return fmt.Sprintf("2: eth0    inet %s/24 brd 10.42.1.255 scope global eth0\n", ns.ip), nil
	case "ss-version":
		return "ss utility, iproute2-6.15.0\n", nil
	case "kernel":
		return ns.kernel + "\n", nil
	case "keepalive-read":
		return fmt.Sprintf("%d\n%d\n%d\n", ns.time, ns.intvl, ns.probes), nil
	case "keepalive-write":
		if ns.readOnly {
			return "sh: can't create /proc/sys/net/ipv4/tcp_keepalive_time: Read-only file system",
				errors.New("command terminated with exit code 1")
		}
		m := keepaliveWriteArg.FindStringSubmatch(first)
		if !ns.ignoreWrite {
			ns.time, _ = strconv.Atoi(m[1])
			ns.intvl, _ = strconv.Atoi(m[2])
			ns.probes, _ = strconv.Atoi(m[3])
		}
		return "", nil
	case "sockets":
		if len(ns.pendingReconnect) > 0 && ns.reconnectIn >= 0 {
			ns.pendingSinceReset++
			if ns.pendingSinceReset >= ns.reconnectIn {
				for _, peer := range ns.pendingReconnect {
					ns.nextPort++
					sock := fakeSocket{local: ns.ip + ":" + strconv.Itoa(ns.nextPort), peer: peer}
					if ns.keepaliveOnReconnect {
						sock.keepalive = strconv.Itoa(ns.time-1) + "sec"
					}
					ns.sockets = append(ns.sockets, sock)
				}
				ns.pendingReconnect = nil
			}
		}
		if ns.rearmOld {
			for i := range ns.sockets {
				ns.sockets[i].keepalive = "25sec"
			}
		}
		return ns.ss(""), nil
	case "sockets-reset":
		m := socketsResetArg.FindStringSubmatch(first)
		peer := m[1] + ":" + m[2]
		if ns.ignoreDestroy {
			return "", nil
		}
		out := ns.ss(peer)
		ns.sockets = slices.DeleteFunc(ns.sockets, func(sk fakeSocket) bool {
			if sk.peer == peer {
				ns.pendingReconnect, ns.pendingSinceReset = append(ns.pendingReconnect, peer), 0
				return true
			}
			return false
		})
		return out, nil
	}
	return "", fmt.Errorf("unexpected script %q", script)
}

// ss prints the sockets as `ss -tnoH state established` does, only those to
// peer when it is set.
func (ns *fakeSysctlNS) ss(peer string) string {
	var b strings.Builder
	for _, sk := range ns.sockets {
		if peer != "" && sk.peer != peer {
			continue
		}
		fmt.Fprintf(&b, "0      0      %s %s", sk.local, sk.peer)
		if sk.keepalive != "" {
			fmt.Fprintf(&b, " timer:(keepalive,%s,0)", sk.keepalive)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// stillOpen is the reason a connection listed before its reset is still
// listed after it.
const stillOpen = "still open after the reset"

var testKeepalive = Keepalive{Time: 30 * time.Second, Interval: 10 * time.Second, Probes: 3}

// keepaliveEnv is an Env whose ephemeral containers start at once, and
// whose kubectl answers the keepalive container's commands.
func keepaliveEnv(t *testing.T, s *fakeSysctl) Env {
	t.Helper()
	env := testEnv(t, newFakeNet(testPods))
	env.Kubectl = s.kubectl
	return env
}

func shorten(t *testing.T, s *fakeSysctl, wait time.Duration) (KeepaliveRecord, error) {
	t.Helper()
	env := keepaliveEnv(t, s)
	members := []Endpoint{member(target), member(slave1), member(slave2)}
	return ShortenKeepalive(context.Background(), env, testKeepalive, members, member(target), member(slave1),
		wait)
}

// requireKeepaliveSet checks a member read with the kernel's defaults before
// the write and want after it, on a kernel with per-namespace keepalive.
func requireKeepaliveSet(t *testing.T, m KeepaliveMember, want KeepaliveValues) {
	t.Helper()
	if m.Before == nil || *m.Before != (KeepaliveValues{Time: 7200, Interval: 75, Probes: 9}) ||
		m.After == nil || *m.After != want || m.SetAt == nil || m.Error != "" || m.UID != "uid-"+m.Name ||
		m.Kernel != "5.15.0-203-generic" {
		t.Errorf("member %+v", m)
	}
}

// Every member's namespace gets the new values, read back afterwards; the
// cut-off member's connection to the master is reset, and the connection
// CUBRID makes again carries a keepalive timer within the new time. So do the
// other connections the cut crosses that were on the old schedule: the
// target's to the other member and the master's to the target; the other
// member, with no connection to the target, is recorded with none.
func TestShortenKeepaliveSetsEveryMemberAndRemakesTheConnectionToTheMaster(t *testing.T) {
	s := newFakeSysctl()
	rec, err := shorten(t, s, time.Second)
	if err != nil {
		t.Fatalf("ShortenKeepalive: %v", err)
	}
	want := KeepaliveValues{Time: 30, Interval: 10, Probes: 3}
	if rec.Requested != want || rec.Container != KeepaliveContainer {
		t.Errorf("requested %+v in %q", rec.Requested, rec.Container)
	}
	if len(rec.Members) != 3 {
		t.Fatalf("members %+v", rec.Members)
	}
	for _, m := range rec.Members {
		requireKeepaliveSet(t, m, want)
	}
	if rec.Target != target || rec.Master != slave1 {
		t.Errorf("target %q, master %q", rec.Target, rec.Master)
	}
	if len(rec.SocketsBefore) != 2 || len(rec.Reset) != 1 || rec.Reset[0].Local != testPods[target]+":48794" ||
		rec.Reset[0].KeepaliveLeftSeconds != 105*60 || rec.ResetAt == nil {
		t.Errorf("sockets before %+v, reset %+v at %v", rec.SocketsBefore, rec.Reset, rec.ResetAt)
	}
	got := rec.Reconnected
	if got == nil || got.Peer != testPods[slave1]+":1523" || got.Local == rec.Reset[0].Local ||
		got.KeepaliveLeftSeconds <= 0 || got.KeepaliveLeftSeconds > 30 || rec.ReconnectedAt == nil || rec.Unmet != "" {
		t.Errorf("reconnected %+v at %v, unmet %q", got, rec.ReconnectedAt, rec.Unmet)
	}
	// The connection is told apart by its addresses only: the record says so.
	if rec.SSVersion != "ss utility, iproute2-6.15.0" {
		t.Errorf("ss version %q", rec.SSVersion)
	}
	if !strings.Contains(rec.Note, "addresses") || !strings.Contains(rec.Note, "process") {
		t.Errorf("note %q", rec.Note)
	}
	requireCrossingRemade(t, rec)
}

// requireCrossingRemade checks the crossing pairs of newFakeSysctl: the
// master's connection to the target and the target's to the other member,
// both on the old schedule, reset and made again; the other member, with no
// connection to the target, recorded with none.
func requireCrossingRemade(t *testing.T, rec KeepaliveRecord) {
	t.Helper()
	type pair struct{ member, peer string }
	crossing := map[pair]KeepaliveCross{}
	for _, c := range rec.Crossing {
		crossing[pair{c.Member, c.Peer}] = c
	}
	if len(rec.Crossing) != 3 || len(crossing) != 3 {
		t.Fatalf("crossing %+v", rec.Crossing)
	}
	for _, p := range []struct {
		pair
		old, addr string
	}{
		{pair{slave1, target}, testPods[slave1] + ":47232", testPods[target] + ":1523"},
		{pair{target, slave2}, testPods[target] + ":48800", testPods[slave2] + ":1523"},
	} {
		c := crossing[p.pair]
		if len(c.Before) != 1 || len(c.Reset) != 1 || c.Reset[0].Local != p.old || c.ResetAt == nil {
			t.Errorf("%s to %s: before %+v, reset %+v at %v", p.member, p.peer, c.Before, c.Reset, c.ResetAt)
		}
		if r := c.Reconnected; r == nil || r.Peer != p.addr || r.Local == p.old || r.KeepaliveLeftSeconds > 30 ||
			c.ReconnectedAt == nil || c.Unmet != "" {
			t.Errorf("%s to %s: reconnected %+v, unmet %q", p.member, p.peer, r, c.Unmet)
		}
	}
	if c := crossing[pair{slave2, target}]; len(c.Before) != 0 || len(c.Reset) != 0 || c.ResetAt != nil ||
		c.Reconnected != nil || c.Unmet != "" {
		t.Errorf("a member with no connection to the target: %+v", c)
	}
}

// A crossing connection already on the new schedule, such as one made after
// the write, is not reset.
func TestShortenKeepaliveLeavesACrossingConnectionOnTheNewSchedule(t *testing.T) {
	s := newFakeSysctl()
	s.ns[testPods[slave2]].sockets = []fakeSocket{
		{local: testPods[slave2] + ":59296", peer: testPods[target] + ":1523", keepalive: "27sec"},
	}
	rec, err := shorten(t, s, time.Second)
	if err != nil {
		t.Fatalf("ShortenKeepalive: %v", err)
	}
	i := slices.IndexFunc(rec.Crossing, func(c KeepaliveCross) bool { return c.Member == slave2 && c.Peer == target })
	if i < 0 || len(rec.Crossing[i].Before) != 1 || len(rec.Crossing[i].Reset) != 0 || rec.Crossing[i].ResetAt != nil {
		t.Fatalf("crossing %+v", rec.Crossing)
	}
	if slices.ContainsFunc(s.scripts, func(sc string) bool {
		return strings.HasPrefix(sc, slave2+": ") && strings.Contains(sc, "sockets-reset")
	}) {
		t.Errorf("%s's connection was reset: %v", slave2, s.scripts)
	}
	if !slices.ContainsFunc(s.ns[testPods[slave2]].sockets, func(sk fakeSocket) bool {
		return sk.local == testPods[slave2]+":59296"
	}) {
		t.Errorf("the connection on the new schedule was reset: %+v", s.ns[testPods[slave2]].sockets)
	}
}

// A crossing connection that the kernel does not reset, or that CUBRID does
// not make again within the wait, ends the step with an error naming the
// pair, after the connection to the master was made again.
func TestShortenKeepaliveCrossingFailures(t *testing.T) {
	cases := []struct {
		name string
		set  func(*fakeSysctlNS)
		want string
	}{
		{"the kernel does not reset it", func(ns *fakeSysctlNS) { ns.ignoreDestroy = true },
			stillOpen},
		{"CUBRID does not connect again", func(ns *fakeSysctlNS) { ns.reconnectIn = -1 },
			"no new connection with a keepalive timer"},
		{"the new connection has no keepalive timer", func(ns *fakeSysctlNS) { ns.keepaliveOnReconnect = false },
			"no new connection with a keepalive timer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeSysctl()
			tc.set(s.ns[testPods[slave1]])
			rec, err := shorten(t, s, 50*time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), slave1) {
				t.Fatalf("error %v, want %q naming %s", err, tc.want, slave1)
			}
			if rec.Reconnected == nil || !strings.Contains(rec.Unmet, tc.want) {
				t.Errorf("reconnected %+v, unmet %q", rec.Reconnected, rec.Unmet)
			}
			i := slices.IndexFunc(rec.Crossing, func(c KeepaliveCross) bool {
				return c.Member == slave1 && c.Peer == target
			})
			if i < 0 || rec.Crossing[i].Unmet == "" || rec.Crossing[i].Reconnected != nil {
				t.Errorf("crossing %+v", rec.Crossing)
			}
		})
	}
}

// Pairs reset together are waited for together: one made again at once is
// kept while another is made again only a few listings later, and a pair
// that never is names what was seen and leaves the others recorded.
func TestShortenKeepaliveCrossingWaitsForEachPair(t *testing.T) {
	s := newFakeSysctl()
	s.ns[testPods[slave1]].reconnectIn = 4
	rec, err := shorten(t, s, time.Second)
	if err != nil {
		t.Fatalf("ShortenKeepalive: %v", err)
	}
	for _, c := range rec.Crossing {
		if c.ResetAt != nil && (c.Reconnected == nil || c.Unmet != "") {
			t.Errorf("pair %s to %s: %+v", c.Member, c.Peer, c)
		}
	}

	s = newFakeSysctl()
	s.ns[testPods[slave1]].keepaliveOnReconnect = false
	rec, err = shorten(t, s, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "has no keepalive timer") {
		t.Fatalf("error %v", err)
	}
	for _, c := range rec.Crossing {
		switch {
		case c.Member == slave1 && c.Peer == target:
			if !strings.Contains(c.Unmet, "has no keepalive timer") {
				t.Errorf("unmet %q", c.Unmet)
			}
		case c.ResetAt != nil && c.Reconnected == nil:
			t.Errorf("pair %s to %s was not made again and says nothing: %+v", c.Member, c.Peer, c)
		}
	}
}

// The pairs are each other member to the target, and the target to each
// other member but the master, whose connection the step remakes first.
func TestCrossingPairs(t *testing.T) {
	members := []Endpoint{member(target), member(slave1), member(slave2)}
	got := make([]string, 0, 3)
	for _, p := range crossingPairs(members, member(target), member(slave1)) {
		got = append(got, p[0].Name+">"+p[1].Name)
	}
	want := []string{slave1 + ">" + target, slave2 + ">" + target, target + ">" + slave2}
	if !slices.Equal(got, want) {
		t.Errorf("pairs %v, want %v", got, want)
	}
}

// The values are written through a privileged container of its own: the
// faults' container, with NET_ADMIN only, sees /proc/sys read-only, and the
// container runtime mounts it writable only for a privileged container. It
// is added to each member's Pod once.
func TestKeepaliveContainerIsPrivileged(t *testing.T) {
	s := newFakeSysctl()
	env := keepaliveEnv(t, s)
	members := []Endpoint{member(target), member(slave1), member(slave2)}
	for range 2 {
		if _, err := ShortenKeepalive(context.Background(), env, testKeepalive, members, member(target),
			member(slave1), time.Second); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{target, slave1, slave2} {
		pod := &corev1.Pod{}
		if err := env.Client.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name},
			pod); err != nil {
			t.Fatal(err)
		}
		var found []corev1.EphemeralContainer
		for _, c := range pod.Spec.EphemeralContainers {
			if c.Name == KeepaliveContainer {
				found = append(found, c)
			}
		}
		if len(found) != 1 || found[0].SecurityContext == nil || found[0].SecurityContext.Privileged == nil ||
			!*found[0].SecurityContext.Privileged || found[0].Image != Image {
			t.Fatalf("%s: keepalive containers %+v", name, found)
		}
		// The member Pods run as non-root at Pod level; a container that
		// runs as root must say runAsNonRoot false, or the kubelet refuses to
		// start it (CreateContainerConfigError), as POC-39 found for the
		// faults' container.
		if sc := found[0].SecurityContext; sc.RunAsNonRoot == nil || *sc.RunAsNonRoot || sc.RunAsUser == nil ||
			*sc.RunAsUser != 0 {
			t.Errorf("%s: the keepalive container's security context %+v does not run as root", name, sc)
		}
		// The privileged process ends by itself instead of living as long as
		// the Pod.
		if cmd := found[0].Command; len(cmd) != 2 || cmd[0] != "sleep" {
			t.Errorf("%s: the keepalive container runs %q, want a bounded sleep", name, cmd)
		} else if secs, err := strconv.Atoi(cmd[1]); err != nil || secs <= 0 ||
			secs > int(keepaliveLifetime.Seconds()) {
			t.Errorf("%s: the keepalive container sleeps %q, want at most %s", name, cmd[1], keepaliveLifetime)
		}
	}
}

// Each way the setting can fail ends with an error that says why, never a
// run that goes on as if the values were set.
func TestShortenKeepaliveFailures(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*fakeSysctl)
		want  string
		reset bool // the target's connection was reset before the failure
	}{
		{"/proc/sys is read-only", func(s *fakeSysctl) { s.ns[testPods[slave2]].readOnly = true },
			"Read-only file system", false},
		{"the write does not hold", func(s *fakeSysctl) { s.ns[testPods[slave1]].ignoreWrite = true },
			"read back", false},
		{"the container is in another Pod's namespace", func(s *fakeSysctl) { s.execIn[slave1] = testPods[slave2] },
			"wrong target", false},
		{"the kernel does not reset the connection", func(s *fakeSysctl) {
			s.ns[testPods[target]].ignoreDestroy = true
		}, stillOpen, false},
		// ss -K prints nothing and exits 0 when the kernel answers
		// SOCK_DESTROY with EOPNOTSUPP: the old socket is still listed.
		{"ss -K prints nothing and the old socket stays", func(s *fakeSysctl) {
			s.ns[testPods[target]].ignoreDestroy = true
		}, "ss -K reported none reset", false},
		// The old socket's first expiry came after the write, so it shows a
		// short timer; it is still not a new connection.
		{"the old socket re-arms with a short timer", func(s *fakeSysctl) {
			s.ns[testPods[target]].ignoreDestroy, s.ns[testPods[target]].rearmOld = true, true
		}, stillOpen, false},
		{"a kernel without per-namespace keepalive", func(s *fakeSysctl) {
			s.ns[testPods[slave1]].kernel = "4.9.0-11-amd64"
		}, "4.13", false},
		// The target has no connection to the master before the reset, and
		// one appears later: nothing was reset, so the later one must not
		// count as the connection made again under the new keepalive.
		{"the target has no connection to the master before the reset", func(s *fakeSysctl) {
			ns := s.ns[testPods[target]]
			ns.sockets = slices.DeleteFunc(ns.sockets, func(sk fakeSocket) bool {
				return sk.peer == testPods[slave1]+":1523"
			})
			// It appears on the second listing, after the one taken before the
			// reset.
			ns.pendingReconnect, ns.reconnectIn = []string{testPods[slave1] + ":1523"}, 2
		}, "no connection to", false},
		{"CUBRID does not connect again", func(s *fakeSysctl) { s.ns[testPods[target]].reconnectIn = -1 },
			"no new connection", true},
		{"the new connection has no keepalive timer", func(s *fakeSysctl) {
			s.ns[testPods[target]].keepaliveOnReconnect = false
		}, "no keepalive timer", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeSysctl()
			tc.set(s)
			rec, err := shorten(t, s, 50*time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if rec.Unmet == "" || !strings.Contains(rec.Unmet, tc.want) {
				t.Errorf("unmet %q, want %q", rec.Unmet, tc.want)
			}
			destroy := strings.Contains(tc.name, "reset the connection") || strings.Contains(tc.name, "ss -K") ||
				strings.Contains(tc.name, "re-arms")
			if (rec.ResetAt != nil) != (tc.reset || destroy) {
				t.Errorf("reset at %v", rec.ResetAt)
			}
			if rec.Reconnected != nil {
				t.Errorf("reconnected %+v after a failure", rec.Reconnected)
			}
		})
	}
	// A member whose write failed is recorded with what it had and why.
	s := newFakeSysctl()
	s.ns[testPods[slave2]].readOnly = true
	rec, _ := shorten(t, s, 50*time.Millisecond)
	i := slices.IndexFunc(rec.Members, func(m KeepaliveMember) bool { return m.Name == slave2 })
	if i < 0 || rec.Members[i].Before == nil || rec.Members[i].After != nil || rec.Members[i].SetAt != nil ||
		!strings.Contains(rec.Members[i].Error, "Read-only") {
		t.Errorf("members %+v", rec.Members)
	}
	if slices.ContainsFunc(s.scripts, func(sc string) bool { return strings.Contains(sc, "sockets-reset") }) {
		t.Errorf("a connection was reset although a member's values were not set: %v", s.scripts)
	}
}

// A keepalive container the kubelet cannot start ends the step with the
// kubelet's reason and message: at once when it will not start without a
// change (CreateContainerConfigError), and at the step's bound when it only
// waits (ImagePullBackOff), instead of a bare "no time is left".
func TestKeepaliveContainerThatDoesNotStart(t *testing.T) {
	cases := []struct {
		name  string
		state corev1.ContainerState
		limit time.Duration
		want  []string
		fast  bool
	}{
		{"its configuration is refused", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "CreateContainerConfigError", Message: "container's runAsUser breaks non-root policy"}},
			time.Minute, []string{"CreateContainerConfigError", "breaks non-root policy"}, true},
		{"its image is still being pulled", corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason: "ImagePullBackOff", Message: "Back-off pulling image"}},
			200 * time.Millisecond, []string{"did not start", "ImagePullBackOff", "Back-off pulling image"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeSysctl()
			n := newFakeNet(testPods)
			n.ephemeralState = map[string]corev1.ContainerState{KeepaliveContainer: tc.state}
			env := testEnv(t, n)
			env.Kubectl = s.kubectl
			ctx, cancel := context.WithTimeout(context.Background(), tc.limit)
			defer cancel()
			start := time.Now()
			rec, err := ShortenKeepalive(ctx, env, testKeepalive, []Endpoint{member(target)}, member(target),
				member(slave1), time.Second)
			if err == nil {
				t.Fatal("a container that did not start was accepted")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) || !strings.Contains(rec.Unmet, w) {
					t.Errorf("error %q, unmet %q, want %q", err, rec.Unmet, w)
				}
			}
			if tc.fast && time.Since(start) > 5*time.Second {
				t.Errorf("waited %s for a container that cannot start", time.Since(start))
			}
		})
	}
}

// Values the kernel would refuse or that are not shorter than its default
// are refused before anything is written.
func TestKeepaliveValidate(t *testing.T) {
	for _, k := range []Keepalive{
		{}, {Time: 1500 * time.Millisecond, Interval: time.Second, Probes: 1},
		{Time: 2 * time.Hour, Interval: time.Second, Probes: 1}, {Time: time.Minute, Interval: 0, Probes: 1},
		{Time: time.Minute, Interval: time.Second, Probes: 0},
	} {
		if err := k.Validate(); err == nil {
			t.Errorf("%+v was accepted", k)
		}
	}
	if err := testKeepalive.Validate(); err != nil {
		t.Errorf("%+v: %v", testKeepalive, err)
	}
}

// The pinned netshoot image has iproute2 6.15.0 (`ss -V`: "ss utility,
// iproute2-6.15.0"; apk iproute2-ss-6.15.0-r0). Since 6.11 it prints a timer
// under ten seconds with milliseconds as <s>.<ms>sec; before 6.11 the same
// value was printed as <s>.<ms>ms (Debian #1054044), which still means
// seconds. The lines below are its real output, read locally with Docker on
// 2026-11-18 UTC from sockets with keepalive times of 1, 5, 15, 65 and 7200
// seconds; the lab printed 119min and 27sec in the same format.
func TestParseSocketsFromThePinnedImage(t *testing.T) {
	out := "0      0      127.0.0.1:44130 127.0.0.1:1523 timer:(keepalive,880ms,0)\n" +
		"0      0      127.0.0.1:44212 127.0.0.1:1523 timer:(keepalive,4.304sec,0)\n" +
		"0      0      127.0.0.1:35330 127.0.0.1:1523 timer:(keepalive,14sec,0)   \n" +
		"0      0      127.0.0.1:35332 127.0.0.1:1523 timer:(keepalive,1min4sec,0)\n" +
		"0      0      127.0.0.1:40742 127.0.0.1:1523 timer:(keepalive,119min,0)\n"
	sockets, err := parseSockets(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{0.88, 4.304, 14, 64, 119 * 60}
	if len(sockets) != len(want) {
		t.Fatalf("sockets %+v", sockets)
	}
	for i, sk := range sockets {
		if sk.KeepaliveUnread || sk.KeepaliveLeftSeconds != want[i] {
			t.Errorf("socket %d: %+v, want %v seconds left", i, sk, want[i])
		}
	}
}

// ss prints a timer's time left in minutes, seconds and milliseconds; the
// legacy <s>.<ms>ms form of iproute2 before 6.11 means seconds.
func TestParseSockets(t *testing.T) {
	out := "0      0      10.42.3.189:48794 10.42.2.52:1523 timer:(keepalive,105min,0)\n" +
		"0      0      10.42.3.189:48795 10.42.2.52:1523 timer:(keepalive,1min29sec,0)\n" +
		"0      0      10.42.3.189:48796 10.42.2.52:1523 timer:(keepalive,5.123ms,0)\n" +
		"0      0      10.42.3.189:48797 10.42.2.52:1523 timer:(keepalive,123ms,1)\n" +
		"0      0      10.42.3.189:48798 10.42.2.52:1523 timer:(on,200ms,0)\n" +
		"0      0      10.42.3.189:48799 10.42.2.52:1523\n" +
		"0      0      [::ffff:10.42.3.189]:48800 [::ffff:10.42.2.52]:1523 timer:(keepalive,-1sec,0)\n" +
		"0      0      10.42.3.189:48801 10.42.2.52:1523 timer:(keepalive,soon,0)\n"
	sockets, err := parseSockets(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{105 * 60, 89, 5.123, 0.123, 0, 0, 0, 0}
	if len(sockets) != len(want) {
		t.Fatalf("sockets %+v", sockets)
	}
	for i, sk := range sockets {
		if sk.KeepaliveLeftSeconds != want[i] || sk.Peer != "10.42.2.52:1523" {
			t.Errorf("socket %d: %+v, want %v seconds left", i, sk, want[i])
		}
	}
	if sockets[4].Keepalive != "" || sockets[5].Keepalive != "" || sockets[0].Keepalive != "105min" {
		t.Errorf("timers %+v", sockets)
	}
	// An IPv4-mapped address compares as the IPv4 one; an overdue timer has
	// no time left; one that cannot be read is kept as printed and marked.
	if sockets[6].Local != "10.42.3.189:48800" || sockets[6].Keepalive != "-1sec" || sockets[6].KeepaliveUnread {
		t.Errorf("mapped or overdue socket %+v", sockets[6])
	}
	if sockets[7].Keepalive != "soon" || !sockets[7].KeepaliveUnread {
		t.Errorf("socket with an unreadable timer %+v", sockets[7])
	}
	if _, err := parseSockets("garbage\n"); err == nil {
		t.Error("a line that is not a socket was accepted")
	}
}
