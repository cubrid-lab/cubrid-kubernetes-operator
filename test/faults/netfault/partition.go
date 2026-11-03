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
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// Direction says which traffic between the target and a peer a cut drops.
type Direction string

const (
	// In drops what the peer sends to the target.
	In Direction = "in"
	// Out drops what the target sends to the peer.
	Out Direction = "out"
	// Both drops both.
	Both Direction = "both"
)

// Cut is one peer the target is cut from, in a direction.
type Cut struct {
	Peer      Endpoint
	Direction Direction
}

// Path is the traffic from one endpoint to another.
type Path struct{ From, To Endpoint }

func (p Path) String() string { return p.From.Name + " -> " + p.To.Name }

// What a probe of a path saw.
const (
	SeenOpen      = "open"
	SeenCut       = "cut"
	SeenUndecided = "undecided"
)

// UndecidedError is what Confirm returns when every path it could decide is
// as expected and some could not be decided yet, such as one from a sender
// that has sent nothing. It is never read as a cut.
type UndecidedError struct{ Paths []string }

func (e *UndecidedError) Error() string { return "not decided yet: " + strings.Join(e.Paths, ", ") }

// Phases of a partition, as its observations name them.
const (
	PhaseBefore  = "before"
	PhaseFault   = "fault"
	PhaseRemoved = "removed"
)

// Methods of a probe.
const (
	// MethodCounted: TCP connects from the sender, and the packets from it
	// counted in the receiver's namespace after the drop rules.
	MethodCounted = "counted_at_receiver"
	// MethodConnect: TCP connects from the sender to a receiver that is not
	// entered. It cannot tell the two directions apart.
	MethodConnect = "tcp_connect"
	// MethodPassive: the receiver's counters for a sender that is not
	// entered, over a window: passed, or dropped.
	MethodPassive = "receiver_counters"
)

// Observation is one probe of a path.
type Observation struct {
	T        time.Time `json:"t"`
	Phase    string    `json:"phase"`
	Path     string    `json:"path"`
	Expected string    `json:"expected"`
	Seen     string    `json:"seen"`
	Method   string    `json:"method"`
	Detail   string    `json:"detail"`
}

// Record is a partition's evidence.
type Record struct {
	Fault  string `json:"fault"`
	Target string `json:"target"`
	// Rules are the drop rules read back from each endpoint's namespace
	// after the injection.
	Rules        map[string][]string `json:"rules,omitempty"`
	Observations []Observation       `json:"observations"`
}

// Partition drops traffic between Target and each peer of Cuts, and keeps the
// paths of Keep open. A one-way cut also keeps the other direction open, and
// checks that it is.
//
// Every drop is made where the packets arrive, in the receiver's INPUT, so
// that a sender sees what a real partition shows it: nothing. A drop in a
// sender's OUTPUT would fail its own sendto() with EPERM, which a UDP sender
// such as CUBRID's heartbeat would notice. Only the target's traffic to an
// endpoint that is not entered (the Operator's Pod) is dropped in the
// target's OUTPUT; that traffic is TCP.
//
// Prepare must succeed before Inject: it adds the ephemeral container to
// every entered endpoint, installs the counting rules and sees every path
// open. Remove takes away the drop rules, sees every path open again, and
// then takes away the counting rules.
type Partition struct {
	Env
	Target Endpoint
	Cuts   []Cut
	Keep   []Path

	mu       sync.Mutex
	prepared bool
	since    map[string]rules // each entered endpoint's counters at the start of the phase
	window   map[string]rules // a passive path's counters at its last decided reading
	rec      Record
}

// Name says what the partition drops.
func (p *Partition) Name() string {
	parts := make([]string, 0, len(p.Cuts))
	for _, c := range p.Cuts {
		parts = append(parts, fmt.Sprintf("%s (%s)", c.Peer.Name, c.Direction))
	}
	return fmt.Sprintf("drop traffic between %s and %s", p.Target.Name, strings.Join(parts, ", "))
}

// Record returns the partition's evidence so far.
func (p *Partition) Record() Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.rec
	r.Fault, r.Target = p.Name(), p.Target.Name
	r.Rules = maps.Clone(r.Rules)
	r.Observations = slices.Clone(r.Observations)
	return r
}

// cutPaths are the paths the partition must cut.
func (p *Partition) cutPaths() []Path {
	var paths []Path
	for _, c := range p.Cuts {
		if c.Direction == Out || c.Direction == Both {
			paths = append(paths, Path{p.Target, c.Peer})
		}
		if c.Direction == In || c.Direction == Both {
			paths = append(paths, Path{c.Peer, p.Target})
		}
	}
	return paths
}

// keptPaths are the paths that must stay open: Keep and the other direction
// of each one-way cut.
func (p *Partition) keptPaths() []Path {
	paths := slices.Clone(p.Keep)
	for _, c := range p.Cuts {
		switch c.Direction {
		case Out:
			paths = append(paths, Path{c.Peer, p.Target})
		case In:
			paths = append(paths, Path{p.Target, c.Peer})
		}
	}
	return paths
}

func (p *Partition) paths() []Path { return append(p.cutPaths(), p.keptPaths()...) }

// isCut reports whether the fault drops the path.
func (p *Partition) isCut(path Path) bool {
	return slices.ContainsFunc(p.cutPaths(), func(c Path) bool { return c.String() == path.String() })
}

// all returns every endpoint the partition names, as often as it names it.
func (p *Partition) all() []Endpoint {
	all := make([]Endpoint, 0, 1+len(p.Cuts)+2*len(p.Keep))
	all = append(all, p.Target)
	for _, c := range p.Cuts {
		all = append(all, c.Peer)
	}
	for _, k := range p.Keep {
		all = append(all, k.From, k.To)
	}
	return all
}

// endpoints returns every endpoint of the partition once.
func (p *Partition) endpoints() []Endpoint {
	var unique []Endpoint
	for _, e := range p.all() {
		if !slices.ContainsFunc(unique, func(u Endpoint) bool { return u.Name == e.Name }) {
			unique = append(unique, e)
		}
	}
	return unique
}

func (p *Partition) entered() []Endpoint {
	var entered []Endpoint
	for _, e := range p.endpoints() {
		if e.Entered() {
			entered = append(entered, e)
		}
	}
	return entered
}

// Validate fails for a partition whose cuts could not be confirmed.
func (p *Partition) Validate() error {
	if !p.Target.Entered() {
		return errors.New("the target must be a Pod the faults can enter")
	}
	if len(p.Cuts) == 0 {
		return errors.New("a partition needs at least one cut")
	}
	var errs []error
	byName := map[string]Endpoint{}
	for _, e := range p.all() {
		if was, ok := byName[e.Name]; ok && was != e {
			errs = append(errs, fmt.Errorf("two different endpoints are named %s", e.Name))
		}
		byName[e.Name] = e
	}
	byIP := map[string]string{}
	for _, e := range p.endpoints() {
		switch {
		case e.Name == "" || e.IP == "":
			errs = append(errs, fmt.Errorf("endpoint %q has no name or no IP", e.Name))
		case byIP[e.IP] != "" && byIP[e.IP] != e.Name:
			errs = append(errs, fmt.Errorf("%s and %s have the same IP %s", byIP[e.IP], e.Name, e.IP))
		case e.Pod != "" && (e.Namespace == "" || e.UID == ""):
			errs = append(errs, fmt.Errorf("%s: an endpoint with a Pod needs its namespace and Pod UID", e.Name))
		}
		byIP[e.IP] = e.Name
	}
	peers := map[string]bool{}
	for _, c := range p.Cuts {
		switch {
		case c.Peer.Name == p.Target.Name:
			errs = append(errs, fmt.Errorf("%s is cut from itself", c.Peer.Name))
		case peers[c.Peer.Name]:
			errs = append(errs, fmt.Errorf("%s is cut twice", c.Peer.Name))
		case c.Direction != In && c.Direction != Out && c.Direction != Both:
			errs = append(errs, fmt.Errorf("%s: unknown direction %q", c.Peer.Name, c.Direction))
		case c.Direction != Both && !c.Peer.Entered():
			// A TCP connect fails when either direction is dropped, so
			// only counting at the receiver tells one from the other.
			errs = append(errs, fmt.Errorf("%s: a one-way cut needs a peer the faults can enter", c.Peer.Name))
		}
		peers[c.Peer.Name] = true
	}
	cut := p.cutPaths()
	for _, path := range p.keptPaths() {
		if slices.ContainsFunc(cut, func(c Path) bool { return c.String() == path.String() }) {
			errs = append(errs, fmt.Errorf("%s is to be cut and kept open", path))
		}
	}
	for _, path := range p.paths() {
		switch {
		case !path.From.Entered() && !path.To.Entered():
			errs = append(errs, fmt.Errorf("%s: neither end can be entered, so it cannot be probed", path))
		case path.From.Entered() && path.To.Port <= 0:
			errs = append(errs, fmt.Errorf("%s: %s has no port to connect to", path, path.To.Name))
		}
	}
	return errors.Join(errs...)
}

func seenRule(ip string) string { return "-A " + chainSeen + " -s " + ip + "/32 -j RETURN" }
func dropInRule(ip string) string {
	return "-A " + chainIn + " -s " + ip + "/32 -j DROP"
}
func dropOutRule(ip string) string {
	return "-A " + chainOut + " -d " + ip + "/32 -j DROP"
}

const (
	jumpIn   = "-A INPUT -j " + chainIn
	jumpOut  = "-A OUTPUT -j " + chainOut
	jumpSeen = "-A INPUT -j " + chainSeen
)

// removedMarker is the file Remove leaves in an ephemeral container. An
// injection that starts after it refuses to install anything.
const removedMarker = "/tmp/netfault-removed"

// observed returns the addresses an entered endpoint counts packets from:
// those of every other endpoint.
func (p *Partition) observed(e Endpoint) []string {
	var ips []string
	for _, o := range p.endpoints() {
		if o.Name != e.Name {
			ips = append(ips, o.IP)
		}
	}
	return ips
}

// dropRules returns, by endpoint name, the drop rules the partition installs
// there.
func (p *Partition) dropRules() map[string][]string {
	by := map[string][]string{}
	for _, c := range p.Cuts {
		if c.Direction == In || c.Direction == Both {
			by[p.Target.Name] = append(by[p.Target.Name], dropInRule(c.Peer.IP))
		}
		if c.Direction == Out || c.Direction == Both {
			if c.Peer.Entered() {
				by[c.Peer.Name] = append(by[c.Peer.Name], dropInRule(p.Target.IP))
			} else {
				by[p.Target.Name] = append(by[p.Target.Name], dropOutRule(c.Peer.IP))
			}
		}
	}
	return by
}

// expected returns the rules and jumps of the drop chains an endpoint must
// hold while the fault is on, nothing more.
func expected(lines []string) []string {
	var in, out bool
	for _, l := range lines {
		in = in || strings.HasPrefix(l, "-A "+chainIn+" ")
		out = out || strings.HasPrefix(l, "-A "+chainOut+" ")
	}
	all := slices.Clone(lines)
	if in {
		all = append(all, jumpIn)
	}
	if out {
		all = append(all, jumpOut)
	}
	slices.Sort(all)
	return all
}

// ruled returns the entered endpoints that get drop rules.
func (p *Partition) ruled() []Endpoint {
	by := p.dropRules()
	var ruled []Endpoint
	for _, e := range p.entered() {
		if len(by[e.Name]) > 0 {
			ruled = append(ruled, e)
		}
	}
	return ruled
}

// beforeReserve is ctx with its deadline moved Reserve earlier, so that what
// must follow a wait still fits in ctx's own limit.
func (p *Partition) beforeReserve(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(ctx, deadline.Add(-p.reserve()))
	}
	return context.WithCancel(ctx)
}

// Prepare adds the ephemeral container to every entered endpoint, checks
// that each is the Pod it names, installs the counting rules, and waits until
// every path is seen open. It refuses an endpoint that has rules or chains of
// the faults, left by an earlier fault. On failure it takes away the counting
// rules it installed, within ctx's limit.
func (p *Partition) Prepare(ctx context.Context) (err error) {
	if err := p.Validate(); err != nil {
		return err
	}
	var installed []Endpoint
	defer func() {
		if err != nil && len(installed) > 0 {
			// Also when ctx was cancelled, as by a SIGTERM: the counting
			// rules must not be left behind.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.reserve())
			defer cancel()
			err = errors.Join(err, p.removeObservers(cleanup, installed))
		}
	}()
	for _, e := range p.endpoints() {
		if e.Pod != "" && !e.Entered() {
			if _, err := currentPod(ctx, p.Env, e); err != nil {
				return err
			}
		}
	}
	for _, e := range p.entered() {
		if err := Attach(ctx, p.Env, e); err != nil {
			return err
		}
		if err := checkNamespace(ctx, p.Env, e); err != nil {
			return err
		}
		r, err := readRules(ctx, p.Env, e)
		if err != nil {
			return err
		}
		if len(r.lines) > 0 || len(r.chains) > 0 {
			return fmt.Errorf("%s: rules of an earlier fault are installed: %v %v", e.Name, r.chains, r.lines)
		}
		var script strings.Builder
		script.WriteString(": netfault observe\nset -e\nrm -f " + removedMarker + "\n")
		script.WriteString("iptables -w 5 -N " + chainSeen + "\niptables -w 5 -A INPUT -j " + chainSeen)
		for _, ip := range p.observed(e) {
			script.WriteString("\niptables -w 5 -A " + chainSeen + " -s " + ip + "/32 -j RETURN")
		}
		installed = append(installed, e)
		if _, err := shell(ctx, p.Env, e, script.String()); err != nil {
			return fmt.Errorf("install the counting rules: %w", err)
		}
		if r, err = readRules(ctx, p.Env, e); err != nil {
			return err
		}
		for _, ip := range p.observed(e) {
			if !r.has(seenRule(ip)) {
				return fmt.Errorf("%s: the counting rule for %s is not installed: %v", e.Name, ip, r.lines)
			}
		}
	}
	wait, cancel := p.beforeReserve(ctx)
	defer cancel()
	if err := p.snapshot(wait); err != nil {
		return err
	}
	if err := p.waitOpen(wait, PhaseBefore, p.paths()); err != nil {
		return err
	}
	p.mu.Lock()
	p.prepared = true
	p.mu.Unlock()
	return nil
}

// Inject checks, for every endpoint that gets drop rules, that it is still
// the Pod it names and that the ephemeral container is in its network
// namespace; installs the rules; and reads them back from each namespace.
func (p *Partition) Inject(ctx context.Context) error {
	p.mu.Lock()
	prepared := p.prepared
	p.mu.Unlock()
	if !prepared {
		return errors.New("the partition was not prepared")
	}
	ruled := p.ruled()
	for _, e := range ruled {
		if err := checkNamespace(ctx, p.Env, e); err != nil {
			return err
		}
	}
	by := p.dropRules()
	for _, e := range ruled {
		var script strings.Builder
		script.WriteString(": netfault inject\nset -e\n")
		script.WriteString("[ ! -e " + removedMarker + " ] || { echo 'the fault was removed'; exit 3; }\n")
		lines := by[e.Name]
		chains := map[string]bool{}
		for _, l := range lines {
			chains[strings.Fields(l)[1]] = true
		}
		for _, c := range []string{chainIn, chainOut} {
			if chains[c] {
				script.WriteString("iptables -w 5 -N " + c + "\n")
			}
		}
		for _, l := range lines {
			script.WriteString("iptables -w 5 " + l + "\n")
		}
		if chains[chainIn] {
			script.WriteString("iptables -w 5 -I INPUT 1 -j " + chainIn + "\n")
		}
		if chains[chainOut] {
			script.WriteString("iptables -w 5 -I OUTPUT 1 -j " + chainOut + "\n")
		}
		if _, err := shell(ctx, p.Env, e, script.String()); err != nil {
			return fmt.Errorf("install the drop rules: %w", err)
		}
	}
	read := map[string][]string{}
	for _, e := range ruled {
		r, err := p.checkDropRules(ctx, e)
		if err != nil {
			return err
		}
		read[e.Name] = dropLines(r)
	}
	p.mu.Lock()
	p.rec.Rules = read
	p.mu.Unlock()
	return p.snapshot(ctx)
}

// checkDropRules fails unless the endpoint's drop chains hold exactly its
// drop rules and the jumps to them, and INPUT drops before it counts.
func (p *Partition) checkDropRules(ctx context.Context, e Endpoint) (rules, error) {
	r, err := readRules(ctx, p.Env, e)
	if err != nil {
		return r, err
	}
	found := dropLines(r)
	slices.Sort(found)
	if want := expected(p.dropRules()[e.Name]); !slices.Equal(found, want) {
		return r, fmt.Errorf("%s: the drop rules in its network namespace are %v, not %v", e.Name, found, want)
	}
	if slices.Contains(found, jumpIn) {
		if seen := slices.Index(r.lines, jumpSeen); seen < 0 || slices.Index(r.lines, jumpIn) > seen {
			return r, fmt.Errorf("%s: INPUT does not drop packets before it counts them: %v", e.Name, r.lines)
		}
	}
	return r, nil
}

// Confirm reports whether every cut path is seen cut and every kept path
// open, while every endpoint with a Pod is still the Pod it names. A cut path
// seen open, or a kept path seen cut, is an error: the fault did not take
// effect, or took effect elsewhere. When only some paths cannot be decided
// yet, the error is an *UndecidedError.
func (p *Partition) Confirm(ctx context.Context) (bool, error) {
	for _, e := range p.endpoints() {
		if e.Pod == "" {
			continue
		}
		if _, err := currentPod(ctx, p.Env, e); err != nil {
			return false, err
		}
	}
	for _, e := range p.ruled() {
		if _, err := p.checkDropRules(ctx, e); err != nil {
			return false, err
		}
	}
	var problems, undecided []string
	for _, path := range p.cutPaths() {
		seen, err := p.observe(ctx, PhaseFault, path, SeenCut)
		switch {
		case err != nil:
			return false, err
		case seen == SeenOpen:
			problems = append(problems, path.String()+" is still open")
		case seen == SeenUndecided:
			undecided = append(undecided, path.String())
		}
	}
	for _, path := range p.keptPaths() {
		seen, err := p.observe(ctx, PhaseFault, path, SeenOpen)
		switch {
		case err != nil:
			return false, err
		case seen == SeenCut:
			problems = append(problems, path.String()+" must stay open and is cut")
		case seen == SeenUndecided:
			undecided = append(undecided, path.String())
		}
	}
	switch {
	case len(problems) > 0 && len(undecided) > 0:
		return false, fmt.Errorf("%s; also %s", strings.Join(problems, "; "), (&UndecidedError{undecided}).Error())
	case len(problems) > 0:
		return false, errors.New(strings.Join(problems, "; "))
	case len(undecided) > 0:
		return false, &UndecidedError{Paths: undecided}
	}
	return true, nil
}

// removeScript stops an injection that is still running, keeps any later
// one from installing, and deletes the drop chains.
const removeScript = ": netfault remove\n" +
	"touch " + removedMarker + "\n" +
	"pkill -f 'netfault [i]nject' 2>/dev/null\n" +
	"while iptables -w 5 -D INPUT -j " + chainIn + " 2>/dev/null; do :; done\n" +
	"while iptables -w 5 -D OUTPUT -j " + chainOut + " 2>/dev/null; do :; done\n" +
	"for c in " + chainIn + " " + chainOut + "; do\n" +
	"  iptables -w 5 -F $c 2>/dev/null; iptables -w 5 -X $c 2>/dev/null\n" +
	"done\n" +
	"true"

// removeAttempts bounds how often the drop rules of one endpoint are removed
// before the rules left are reported.
const removeAttempts = 3

// removeDrops removes the endpoint's drop chains, reads the namespace back and
// tries again while any is left. It returns what is left.
func (p *Partition) removeDrops(ctx context.Context, e Endpoint) ([]string, error) {
	var left []string
	var errs []error
	for range removeAttempts {
		if _, err := shell(ctx, p.Env, e, removeScript); err != nil {
			errs = append(errs, fmt.Errorf("remove the drop rules: %w", err))
		}
		r, err := readRules(ctx, p.Env, e)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if left = dropLeft(r); len(left) == 0 {
			return nil, nil
		}
	}
	return left, errors.Join(errs...)
}

// Remove takes away the drop rules, checks that none is left, waits until
// every path is seen open again, reads the rules once more, and then takes
// away the counting rules of every entered endpoint and checks that none is
// left. The wait ends Reserve before ctx's limit, so that the last steps fit
// in it. Every failure is returned.
func (p *Partition) Remove(ctx context.Context) error {
	var errs []error
	for _, e := range p.ruled() {
		left, err := p.removeDrops(ctx, e)
		if err != nil {
			errs = append(errs, err)
		}
		if len(left) > 0 {
			errs = append(errs, fmt.Errorf("%s: drop rules left after the removal: %v", e.Name, left))
		}
	}
	restored := false
	if len(errs) == 0 {
		wait, cancel := p.beforeReserve(ctx)
		if err := p.snapshot(wait); err != nil {
			errs = append(errs, err)
		} else if err := p.waitOpen(wait, PhaseRemoved, p.paths()); err != nil {
			errs = append(errs, fmt.Errorf("the network was not seen restored: %w", err))
		} else {
			restored = true
		}
		cancel()
	}
	// An injection that was not stopped in time may have installed its rules
	// after they were removed.
	for _, e := range p.ruled() {
		r, err := readRules(ctx, p.Env, e)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if late := dropLeft(r); len(late) > 0 {
			when := "after the network was seen restored"
			if !restored {
				when = "again after they were removed"
			}
			errs = append(errs, fmt.Errorf("%s: drop rules found %s: %v", e.Name, when, late))
			if left, err := p.removeDrops(ctx, e); err != nil || len(left) > 0 {
				errs = append(errs, fmt.Errorf("%s: drop rules left: %v %w", e.Name, left, err))
			}
		}
	}
	return errors.Join(append(errs, p.removeObservers(ctx, p.entered()))...)
}

// dropLines returns the rules of the drop chains and the jumps to them.
func dropLines(r rules) []string {
	var lines []string
	for _, l := range r.lines {
		if strings.Contains(l, chainIn) || strings.Contains(l, chainOut) {
			lines = append(lines, l)
		}
	}
	return lines
}

// dropLeft returns the drop chains and rules among the rules.
func dropLeft(r rules) []string {
	var left []string
	for _, line := range append(slices.Clone(r.chains), r.lines...) {
		if strings.Contains(line, chainIn) || strings.Contains(line, chainOut) {
			left = append(left, line)
		}
	}
	return left
}

// removeObservers takes away the counting rules of the endpoints and checks
// that no rule or chain of the faults is left in them.
func (p *Partition) removeObservers(ctx context.Context, endpoints []Endpoint) error {
	script := ": netfault unobserve\n" +
		"while iptables -w 5 -D INPUT -j " + chainSeen + " 2>/dev/null; do :; done\n" +
		"iptables -w 5 -F " + chainSeen + " 2>/dev/null; iptables -w 5 -X " + chainSeen + " 2>/dev/null\ntrue"
	var errs []error
	for _, e := range endpoints {
		if _, err := shell(ctx, p.Env, e, script); err != nil {
			errs = append(errs, fmt.Errorf("remove the counting rules: %w", err))
			continue
		}
		r, err := readRules(ctx, p.Env, e)
		switch {
		case err != nil:
			errs = append(errs, err)
		case len(r.lines) > 0 || len(r.chains) > 0:
			errs = append(errs, fmt.Errorf("%s: rules left after the removal: %v %v", e.Name, r.chains, r.lines))
		}
	}
	return errors.Join(errs...)
}

// snapshot keeps every entered endpoint's counters, the start of a phase for
// the probes that count passively.
func (p *Partition) snapshot(ctx context.Context) error {
	since := map[string]rules{}
	for _, e := range p.entered() {
		r, err := readRules(ctx, p.Env, e)
		if err != nil {
			return err
		}
		since[e.Name] = r
	}
	p.mu.Lock()
	p.since, p.window = since, map[string]rules{}
	p.mu.Unlock()
	return nil
}

// waitOpen probes the paths until every one is seen open, or ctx ends.
func (p *Partition) waitOpen(ctx context.Context, phase string, paths []Path) error {
	open := map[string]bool{}
	// last is what the last observation of each path that the wait's own
	// deadline did not cut short found, as it is reported. One that the
	// deadline cut short does not replace it: that the time ran out is not
	// what the path showed (#630).
	last := map[string]string{}
	for {
		start := time.Now()
		var notYet []string
		var lastErr error
		for _, path := range paths {
			if open[path.String()] {
				continue
			}
			seen, err := p.observe(ctx, phase, path, SeenOpen)
			switch {
			case err != nil && ctx.Err() != nil && last[path.String()] != "":
				notYet = append(notYet, path.String()+last[path.String()])
			case err != nil:
				lastErr = err
				last[path.String()] = ": " + err.Error()
				notYet = append(notYet, path.String()+last[path.String()])
			case seen == SeenOpen:
				open[path.String()] = true
			default:
				last[path.String()] = " " + seen
				notYet = append(notYet, path.String()+last[path.String()])
			}
		}
		if len(notYet) == 0 {
			return nil
		}
		if err := p.pause(ctx, time.Since(start)); err != nil {
			return fmt.Errorf("not seen open: %s: %w", strings.Join(notYet, "; "), errors.Join(err, lastErr))
		}
	}
}

// observe probes a path once and records what it saw.
func (p *Partition) observe(ctx context.Context, phase string, path Path, expected string) (string, error) {
	o := Observation{Phase: phase, Path: path.String(), Expected: expected}
	var err error
	switch {
	case path.From.Entered() && path.To.Entered():
		o.Method = MethodCounted
		o.Seen, o.Detail, err = p.counted(ctx, path)
	case path.From.Entered():
		o.Method = MethodConnect
		o.Seen, o.Detail, err = p.connected(ctx, path)
	default:
		o.Method = MethodPassive
		o.Seen, o.Detail, err = p.passive(ctx, path)
	}
	if err != nil {
		o.Seen, o.Detail = SeenUndecided, err.Error()
	}
	o.T = time.Now().UTC()
	p.mu.Lock()
	p.rec.Observations = append(p.rec.Observations, o)
	p.mu.Unlock()
	return o.Seen, err
}

// decide turns what a probe saw into open, cut or undecided. A path the
// fault drops is cut only when nothing of it arrived, no connect got an
// answer, and its drop rule dropped packets meanwhile: a sender that sent
// nothing, for any reason, is not taken for a cut. A path the fault leaves
// open is seen cut without a drop count, which fails the confirmation.
func (p *Partition) decide(path Path, arrived bool, attempts []attempt, dropped uint64) string {
	switch {
	case arrived:
		return SeenOpen
	case allDropped(attempts) && (dropped > 0 || !p.isCut(path)):
		return SeenCut
	}
	return SeenUndecided
}

// counted connects from the sender and reads the receiver's count of the
// sender's packets around it, and the receiver's drop count for the sender:
// a cut between two entered endpoints is always made in the receiver.
func (p *Partition) counted(ctx context.Context, path Path) (string, string, error) {
	before, err := readRules(ctx, p.Env, path.To)
	if err != nil {
		return "", "", err
	}
	if _, ok := before.seen[path.From.IP]; !ok {
		return "", "", fmt.Errorf("%s counts no packets from %s", path.To.Name, path.From.Name)
	}
	attempts, err := connect(ctx, p.Env, path.From, path.To.IP, path.To.Port, probeAttempts, 2*time.Second)
	if err != nil {
		return "", "", err
	}
	after, err := readRules(ctx, p.Env, path.To)
	if err != nil {
		return "", "", err
	}
	counted := grown(before.seen[path.From.IP], after.seen[path.From.IP])
	dropped := grown(before.dropIn[path.From.IP], after.dropIn[path.From.IP])
	detail := fmt.Sprintf("packets from %s counted at %s: %d -> %d; dropped there: %d; connects to port %d: %s",
		path.From.Name, path.To.Name, before.seen[path.From.IP], after.seen[path.From.IP], dropped, path.To.Port,
		describe(attempts))
	return p.decide(path, counted > 0, attempts, dropped), detail, nil
}

func allDropped(attempts []attempt) bool {
	return !slices.ContainsFunc(attempts, func(a attempt) bool { return !a.dropped() })
}

// connected connects from the sender to a receiver that is not entered, and
// reads the sender's own drop count for it.
func (p *Partition) connected(ctx context.Context, path Path) (string, string, error) {
	before, err := readRules(ctx, p.Env, path.From)
	if err != nil {
		return "", "", err
	}
	attempts, err := connect(ctx, p.Env, path.From, path.To.IP, path.To.Port, probeAttempts, 2*time.Second)
	if err != nil {
		return "", "", err
	}
	after, err := readRules(ctx, p.Env, path.From)
	if err != nil {
		return "", "", err
	}
	dropped := grown(before.dropOut[path.To.IP], after.dropOut[path.To.IP])
	detail := fmt.Sprintf("connects to %s port %d: %s; dropped by %s's rule: %d", path.To.Name, path.To.Port,
		describe(attempts), path.From.Name, dropped)
	answered := slices.ContainsFunc(attempts, func(a attempt) bool { return a.connected() || a.refused() })
	return p.decide(path, answered, attempts, dropped), detail, nil
}

// passive reads the receiver's counters for a sender that is not entered,
// over a window: from the start of the phase, then from the path's last
// decided reading, so that each decision rests on packets of its own. It is
// open when packets from the sender passed, cut when none passed and the
// drop rule dropped some, and undecided while nothing came from it.
func (p *Partition) passive(ctx context.Context, path Path) (string, string, error) {
	now, err := readRules(ctx, p.Env, path.To)
	if err != nil {
		return "", "", err
	}
	p.mu.Lock()
	base, ok := p.window[path.String()]
	if !ok {
		base = p.since[path.To.Name]
	}
	p.mu.Unlock()
	seen := grown(base.seen[path.From.IP], now.seen[path.From.IP])
	dropped := grown(base.dropIn[path.From.IP], now.dropIn[path.From.IP])
	detail := fmt.Sprintf("in the window, %s let %d packets from %s through and dropped %d",
		path.To.Name, seen, path.From.Name, dropped)
	result := SeenUndecided
	switch {
	case seen > 0:
		result = SeenOpen
	case dropped > 0:
		result = SeenCut
	}
	if result != SeenUndecided {
		p.mu.Lock()
		p.window[path.String()] = now
		p.mu.Unlock()
	}
	return result, detail, nil
}

// grown is how much a counter grew; a counter that went back was reset, and
// all of it is new.
func grown(was, now uint64) uint64 {
	if now < was {
		return now
	}
	return now - was
}
