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

// Command netfault exercises the network faults of test/faults/netfault on
// the members of an existing HA cluster, one variant after another, each
// through faults.Run: prepared and seen open, injected, confirmed, held,
// removed and seen restored. It judges the faults, not the cluster: it
// observes nothing of CUBRID's or the Operator's behavior
// (docs/testing/network-faults.md).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/installed"
)

// Variants of the command.
const (
	variantIsolate = "isolate" // the target cut both ways from the other members and the Operator
	variantOneWay  = "one-way" // the target's packets to the second member dropped
	variantDelay   = "delay"   // the target's packets to the second member delayed
)

type config struct {
	kubeconfig, context, namespace, operatorNamespace, evidenceDir string
	members, variants                                              []string
	memberPort, operatorPort                                       int
	delay, hold, confirm, cleanup, poll                            time.Duration
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	// The API server's warnings, such as Pod Security's on the ephemeral
	// container, are logged through it.
	ctrllog.SetLogger(zap.New())
	if err := run(ctx, os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) (runErr error) {
	flags := flag.NewFlagSet("netfault", flag.ContinueOnError)
	var cfg config
	var members, variants string
	flags.StringVar(&cfg.kubeconfig, "kubeconfig", "", "explicit target kubeconfig")
	flags.StringVar(&cfg.context, "context", "", "explicit context")
	flags.StringVar(&cfg.namespace, "namespace", "", "namespace of the cluster's members (cubrid-lab-*)")
	flags.StringVar(&members, "members", "", "member Pods, the target first, then at least one peer")
	flags.StringVar(&cfg.operatorNamespace, "operator-namespace", "", "namespace of the Operator's manager Pod")
	flags.IntVar(&cfg.memberPort, "member-port", 1523, "TCP port probed on a member (cub_master)")
	flags.IntVar(&cfg.operatorPort, "operator-port", 8081, "TCP port probed on the manager Pod (health)")
	flags.StringVar(&variants, "variants", strings.Join([]string{variantIsolate, variantOneWay, variantDelay}, ","),
		"variants to run, in order")
	flags.DurationVar(&cfg.delay, "delay", 500*time.Millisecond, "delay of the delay variant")
	flags.DurationVar(&cfg.hold, "hold", 15*time.Second, "how long a confirmed fault must keep holding")
	flags.DurationVar(&cfg.confirm, "confirm-limit", 2*time.Minute, "bound of the preparation, and of the confirmation")
	flags.DurationVar(&cfg.cleanup, "cleanup-limit", 2*time.Minute, "bound of the removal and its checks")
	flags.DurationVar(&cfg.poll, "poll", 2*time.Second, "pause between two observations")
	flags.StringVar(&cfg.evidenceDir, "evidence-dir", "", "empty directory for one JSON file per variant")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	cfg.members, cfg.variants = splitList(members), splitList(variants)
	if err := cfg.validate(); err != nil {
		return err
	}
	target, err := installed.OpenTarget(cfg.kubeconfig, cfg.context)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, target.Close()) }()
	runner, err := target.Runner()
	if err != nil {
		return err
	}
	env := netfault.Env{Client: runner.Client, Kubectl: runner.Command, Poll: cfg.poll}
	var failed []string
	for _, variant := range cfg.variants {
		o, err := runVariant(ctx, cfg, env, variant)
		if err != nil {
			return fmt.Errorf("%s: %w", variant, err)
		}
		fmt.Printf("%s: %s %s, fault confirmed %v, cleanup succeeded %v\n", variant, o.Result, o.Reason,
			o.FaultConfirmed, o.CleanupSucceeded)
		if o.Result != evidence.Pass || !o.CleanupSucceeded {
			failed = append(failed, variant)
		}
		if !o.FaultIssuedAt.IsZero() && !o.CleanupSucceeded {
			// The network may still be cut: no later variant is started on it.
			return fmt.Errorf("%s: the fault was not seen removed; the variants after it were not run", variant)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("variants that did not pass with a confirmed cleanup: %v", failed)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for v := range strings.SplitSeq(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (c config) validate() error {
	var errs []error
	if c.context == "" || !strings.HasPrefix(c.namespace, "cubrid-lab-") {
		errs = append(errs, errors.New("name the context and a cubrid-lab-* namespace explicitly"))
	}
	if len(c.members) < 2 {
		errs = append(errs, errors.New("-members needs the target and at least one peer"))
	}
	if entries, err := os.ReadDir(c.evidenceDir); err != nil || len(entries) > 0 {
		errs = append(errs, errors.New("-evidence-dir must name an existing empty directory"))
	}
	for _, v := range c.variants {
		switch v {
		case variantOneWay, variantDelay:
		case variantIsolate:
			if c.operatorNamespace == "" {
				errs = append(errs, errors.New("the isolate variant needs -operator-namespace"))
			}
		default:
			errs = append(errs, fmt.Errorf("unknown variant %q", v))
		}
	}
	if c.hold <= 0 || c.confirm <= 0 || c.cleanup <= 0 || c.poll <= 0 {
		errs = append(errs, errors.New("every time limit must be set"))
	}
	return errors.Join(errs...)
}

// variantRecord is the evidence file of one variant.
type variantRecord struct {
	Variant          string            `json:"variant"`
	Fault            string            `json:"fault"`
	Result           evidence.Result   `json:"result"`
	Reason           string            `json:"reason,omitempty"`
	FaultConfirmed   bool              `json:"faultConfirmed"`
	CleanupSucceeded *bool             `json:"cleanupSucceeded,omitempty"`
	Limits           map[string]string `json:"limits"`
	Timeline         []faults.Step     `json:"timeline"`
	Partition        *netfault.Record  `json:"partition,omitempty"`
	Measurements     map[string]any    `json:"measurements,omitempty"`
}

// runVariant reads the endpoints afresh, runs the variant and writes its
// evidence file.
func runVariant(ctx context.Context, cfg config, env netfault.Env, variant string) (faults.Outcome, error) {
	endpoints := make([]netfault.Endpoint, 0, len(cfg.members))
	for _, name := range cfg.members {
		e, err := memberEndpoint(ctx, env.Client, cfg, name)
		if err != nil {
			return faults.Outcome{}, err
		}
		endpoints = append(endpoints, e)
	}
	target, peers := endpoints[0], endpoints[1:]
	var fault interface {
		faults.Fault
		Prepare(context.Context) error
	}
	var partition *netfault.Partition
	var delay *netfault.Delay
	switch variant {
	case variantIsolate:
		op, err := operatorEndpoint(ctx, env.Client, cfg)
		if err != nil {
			return faults.Outcome{}, err
		}
		partition = &netfault.Partition{Env: env, Target: target}
		for _, p := range peers {
			partition.Cuts = append(partition.Cuts, netfault.Cut{Peer: p, Direction: netfault.Both})
			for _, q := range peers {
				if p.Name != q.Name {
					partition.Keep = append(partition.Keep, netfault.Path{From: p, To: q})
				}
			}
		}
		partition.Cuts = append(partition.Cuts, netfault.Cut{Peer: op, Direction: netfault.Both})
		fault = partition
	case variantOneWay:
		partition = &netfault.Partition{Env: env, Target: target,
			Cuts: []netfault.Cut{{Peer: peers[0], Direction: netfault.Out}}}
		for _, p := range peers[1:] {
			partition.Keep = append(partition.Keep, netfault.Path{From: target, To: p}, netfault.Path{From: p, To: target})
		}
		fault = partition
	case variantDelay:
		delay = &netfault.Delay{Env: env, Target: target, Peer: peers[0], Delay: cfg.delay}
		if len(peers) > 1 {
			// Its traffic must not be delayed.
			delay.Control = peers[1]
		}
		fault = delay
	}
	limits := faults.Limits{Confirm: cfg.confirm, Outcome: cfg.hold + cfg.confirm, Cleanup: cfg.cleanup, Poll: cfg.poll}
	// The outcome is that the confirmed fault still holds cfg.hold later. A
	// confirmation that cannot decide yet is asked again; one that sees the
	// fault gone or too wide breaks the expectation.
	var confirmedAt time.Time
	var undecided error
	o := faults.Run(ctx, faults.Scenario{Fault: fault, Limits: limits, Before: fault.Prepare,
		Check: func(ctx context.Context) (bool, error) {
			if confirmedAt.IsZero() {
				confirmedAt = time.Now()
			}
			if time.Since(confirmedAt) < cfg.hold {
				return false, nil
			}
			held, err := fault.Confirm(ctx)
			if notYet := (*netfault.UndecidedError)(nil); errors.As(err, &notYet) {
				undecided = err
				return false, nil
			}
			return held, err
		}})
	if o.Result == evidence.Fail && strings.HasPrefix(o.Reason, "time_limit_exceeded") && undecided != nil {
		// Not observed is not a failure.
		o.Result, o.Reason = evidence.Blocked, "hold_not_observed: "+undecided.Error()
	}
	rec := variantRecord{Variant: variant, Fault: fault.Name(), Result: o.Result, Reason: o.Reason,
		FaultConfirmed: o.FaultConfirmed, Timeline: o.Timeline,
		Limits: map[string]string{"confirm": limits.Confirm.String(), "hold": cfg.hold.String(),
			"cleanup": limits.Cleanup.String()}}
	if !o.FaultIssuedAt.IsZero() {
		rec.CleanupSucceeded = &o.CleanupSucceeded
	}
	if partition != nil {
		r := partition.Record()
		rec.Partition = &r
	}
	if delay != nil {
		rec.Measurements = delay.Measurements()
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return o, err
	}
	return o, os.WriteFile(filepath.Join(cfg.evidenceDir, variant+".json"), append(data, '\n'), 0o600)
}

func memberEndpoint(ctx context.Context, c client.Client, cfg config, name string) (netfault.Endpoint, error) {
	pod := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: cfg.namespace, Name: name}, pod); err != nil {
		return netfault.Endpoint{}, fmt.Errorf("read member Pod %s: %w", name, err)
	}
	if pod.Status.PodIP == "" || pod.Status.Phase != corev1.PodRunning {
		return netfault.Endpoint{}, fmt.Errorf("member Pod %s is not running with an IP", name)
	}
	return netfault.Endpoint{Name: name, Namespace: cfg.namespace, Pod: name, UID: string(pod.UID),
		IP: pod.Status.PodIP, Port: cfg.memberPort}, nil
}

// operatorEndpoint is the one running manager Pod, as an address only: the
// faults never enter it.
func operatorEndpoint(ctx context.Context, c client.Client, cfg config) (netfault.Endpoint, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(cfg.operatorNamespace),
		client.MatchingLabels{"control-plane": "controller-manager"}); err != nil {
		return netfault.Endpoint{}, fmt.Errorf("list the manager Pods: %w", err)
	}
	var running []corev1.Pod
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" && p.DeletionTimestamp == nil {
			running = append(running, p)
		}
	}
	if len(running) != 1 {
		return netfault.Endpoint{}, fmt.Errorf("want one running manager Pod in %s, found %d", cfg.operatorNamespace,
			len(running))
	}
	// Named by its Pod and UID, so that a replaced manager Pod stops the
	// run, and never entered.
	return netfault.Endpoint{Name: "operator", Namespace: cfg.operatorNamespace, Pod: running[0].Name,
		UID: string(running[0].UID), IP: running[0].Status.PodIP, Port: cfg.operatorPort, NoEnter: true}, nil
}
