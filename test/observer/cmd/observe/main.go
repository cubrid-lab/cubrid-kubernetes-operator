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

// Command observe records what an installed Operator reports about one
// CubridCluster, from outside the cluster, for docs/testing/lab-observer.md.
// It only reads: it changes nothing in the cluster.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/installed"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/observer"
)

// targetHandle is the snapshot of the selected context that run reads through.
type targetHandle interface {
	Runner() (*installed.Runner, error)
	Kubectl(ctx context.Context, args ...string) *exec.Cmd
	Close() error
}

// openTarget is replaced in tests.
var openTarget = func(path, contextName string) (targetHandle, error) {
	t, err := installed.OpenTarget(path, contextName)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type config struct {
	kubeconfig, context, clusterUID                   string
	namespace, cluster                                string
	operatorNamespace, operatorDeployment, metricsJob string
	prometheusNamespace, prometheusService            string
	prometheusPort                                    int
	commit, level, evidenceDir                        string
	interval, duration, window, scrapeTimeout         time.Duration
}

var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c config) validate() error {
	for flagName, value := range map[string]string{
		"kubeconfig": c.kubeconfig, "context": c.context, "cluster-uid": c.clusterUID,
		"namespace": c.namespace, "cluster": c.cluster, "operator-namespace": c.operatorNamespace,
		"operator-deployment": c.operatorDeployment, "prometheus-namespace": c.prometheusNamespace,
		"prometheus-service": c.prometheusService, "evidence-dir": c.evidenceDir,
	} {
		if value == "" {
			return fmt.Errorf("-%s is required", flagName)
		}
	}
	if c.level != "kind" && c.level != "vm-lab" {
		return errors.New("-level must be kind or vm-lab")
	}
	if !fullCommit.MatchString(c.commit) {
		return errors.New("-commit must be the full commit of the installed Operator")
	}
	if c.interval <= 0 || c.duration <= 0 || c.window < 2*c.interval || c.window < time.Second {
		return errors.New("-interval and -duration must be positive, and -window at least two intervals and 1s")
	}
	if c.scrapeTimeout <= 0 {
		return errors.New("-scrape-timeout must be positive")
	}
	if c.prometheusPort <= 0 || c.prometheusPort > 65535 {
		return errors.New("-prometheus-port must be a port number")
	}
	return nil
}

// runRecord is what observer.json says about the run besides its summary.
type runRecord struct {
	StartedAt            time.Time `json:"startedAt"`
	EndedAt              time.Time `json:"endedAt"`
	StoppedEarly         string    `json:"stoppedEarly,omitempty"`
	Level                string    `json:"level"`
	SelectedCommit       string    `json:"selectedCommit"`
	KubernetesClusterUID string    `json:"kubernetesClusterUID"`
	Namespace            string    `json:"namespace"`
	Cluster              string    `json:"cluster"`
	OperatorNamespace    string    `json:"operatorNamespace"`
	OperatorDeployment   string    `json:"operatorDeployment"`
	ManagerImage         string    `json:"managerImage"`
	Prometheus           string    `json:"prometheus"`
	ClusterSelector      string    `json:"clusterSelector"`
	UpSelector           string    `json:"upSelector"`
	Interval             string    `json:"interval"`
	Window               string    `json:"window"`
	Duration             string    `json:"duration"`
}

type result struct {
	observer.Summary
	Run runRecord `json:"run"`
}

func run(ctx context.Context, args []string) (runErr error) {
	var cfg config
	flags := flag.NewFlagSet("observe", flag.ContinueOnError)
	flags.StringVar(&cfg.kubeconfig, "kubeconfig", "", "explicit target kubeconfig (credentials stay private)")
	flags.StringVar(&cfg.context, "context", "", "explicit context to snapshot")
	flags.StringVar(&cfg.clusterUID, "cluster-uid", "", "expected kube-system namespace UID")
	flags.StringVar(&cfg.namespace, "namespace", "", "namespace of the CubridCluster")
	flags.StringVar(&cfg.cluster, "cluster", "", "name of the CubridCluster")
	flags.StringVar(&cfg.operatorNamespace, "operator-namespace", "", "namespace of the installed Operator")
	flags.StringVar(&cfg.operatorDeployment, "operator-deployment", "", "installed manager Deployment name")
	flags.StringVar(&cfg.metricsJob, "metrics-job", "",
		"Prometheus job of the manager's metrics (default: <operator-deployment>-metrics-service)")
	flags.StringVar(&cfg.prometheusNamespace, "prometheus-namespace", "", "namespace of the Prometheus Service")
	flags.StringVar(&cfg.prometheusService, "prometheus-service", "", "Prometheus Service to port-forward to")
	flags.IntVar(&cfg.prometheusPort, "prometheus-port", 9090, "Prometheus Service port")
	flags.StringVar(&cfg.commit, "commit", "", "full commit of the installed Operator")
	flags.StringVar(&cfg.level, "level", "", "evidence environment: kind or vm-lab")
	flags.StringVar(&cfg.evidenceDir, "evidence-dir", "", "empty directory for samples.jsonl and observer.json")
	flags.DurationVar(&cfg.interval, "interval", 2*time.Second, "time between samples")
	flags.DurationVar(&cfg.duration, "duration", 5*time.Minute, "how long to observe")
	flags.DurationVar(&cfg.window, "window", 90*time.Second, "how far back each Prometheus query reads raw samples")
	flags.DurationVar(&cfg.scrapeTimeout, "scrape-timeout", 10*time.Second,
		"Prometheus' scrape timeout for the manager's target (its scrape_timeout)")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("observe takes no positional arguments")
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	if cfg.metricsJob == "" {
		cfg.metricsJob = cfg.operatorDeployment + "-metrics-service"
	}
	if err := evidence.RequireEmptyDir(cfg.evidenceDir); err != nil {
		return err
	}

	target, err := openTarget(cfg.kubeconfig, cfg.context)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, target.Close()) }()
	runner, err := target.Runner()
	if err != nil {
		return err
	}
	var system corev1.Namespace
	if err := runner.Client.Get(ctx, client.ObjectKey{Name: "kube-system"}, &system); err != nil {
		return fmt.Errorf("could not read the target's kube-system namespace: %w", err)
	}
	if string(system.UID) != cfg.clusterUID {
		return errors.New("the selected context is not the expected cluster: its kube-system UID differs")
	}
	var deploy appsv1.Deployment
	deployKey := client.ObjectKey{Namespace: cfg.operatorNamespace, Name: cfg.operatorDeployment}
	if err := runner.Client.Get(ctx, deployKey, &deploy); err != nil {
		return fmt.Errorf("could not read the manager Deployment: %w", err)
	}
	if sel := deploy.Spec.Selector; sel == nil || len(sel.MatchLabels) == 0 || len(sel.MatchExpressions) != 0 {
		return errors.New("the manager Deployment's selector is not a plain label match")
	}
	managerImage := ""
	for _, c := range deploy.Spec.Template.Spec.Containers {
		if c.Name == "manager" {
			managerImage = c.Image
		}
	}

	fwd := &forwarder{
		start: func(ctx context.Context) *exec.Cmd {
			return target.Kubectl(ctx, "-n", cfg.prometheusNamespace, "port-forward", "--address", "127.0.0.1",
				"service/"+cfg.prometheusService, ":"+strconv.Itoa(cfg.prometheusPort))
		},
		// Shorter than a read's timeout, so that a port-forward that does not
		// listen is reported as such rather than as the read's deadline.
		ready: 8 * time.Second,
		http:  &http.Client{Timeout: 10 * time.Second},
	}
	defer fwd.Close()
	col := &observer.Collector{
		Client: runner.Client, Metrics: fwd, Namespace: cfg.namespace, Cluster: cfg.cluster,
		OperatorNamespace: cfg.operatorNamespace, OperatorLabels: deploy.Spec.Selector.MatchLabels,
		OperatorDeployment: cfg.operatorDeployment,
		UpSelector:         fmt.Sprintf(`up{namespace=%q,job=%q}`, cfg.operatorNamespace, cfg.metricsJob),
		Window:             cfg.window, Rules: observer.DefaultRules(),
	}
	col.Rules.ScrapeTimeout = cfg.scrapeTimeout

	if err := os.MkdirAll(cfg.evidenceDir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(cfg.evidenceDir, "samples.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	rec := runRecord{
		StartedAt: time.Now().UTC(), Level: cfg.level, SelectedCommit: cfg.commit,
		KubernetesClusterUID: cfg.clusterUID, Namespace: cfg.namespace, Cluster: cfg.cluster,
		OperatorNamespace: cfg.operatorNamespace, OperatorDeployment: cfg.operatorDeployment, ManagerImage: managerImage,
		Prometheus:      fmt.Sprintf("%s/%s:%d", cfg.prometheusNamespace, cfg.prometheusService, cfg.prometheusPort),
		ClusterSelector: col.ClusterSelector(), UpSelector: col.UpSelector,
		Interval: cfg.interval.String(), Window: cfg.window.String(), Duration: cfg.duration.String(),
	}
	samples, runErr := observer.Run(ctx, col, cfg.interval, cfg.duration, f, runner.Redactor)
	runErr = errors.Join(runErr, f.Close())
	rec.EndedAt = time.Now().UTC()
	res := result{Summary: observer.Summarize(samples, col.Rules), Run: rec}
	if runErr != nil {
		rec.StoppedEarly = runErr.Error()
		res.Run = rec
		if res.Verdict == observer.VerdictPassed {
			res.Verdict, res.Reason = observer.VerdictIncomplete, "the run stopped before its duration: "+runErr.Error()
		}
	}
	if err := writeResult(cfg.evidenceDir, runner.Redactor, res); err != nil {
		return errors.Join(runErr, err)
	}
	fmt.Printf("observe: %s, %d samples (%d healthy), %d contradictions\n",
		res.Verdict, res.Samples, res.Counts[observer.Healthy], len(res.Contradictions))
	if res.Verdict != observer.VerdictPassed {
		return errors.Join(runErr, fmt.Errorf("observe: %s: %s", res.Verdict, res.Reason))
	}
	return runErr
}

func writeResult(dir string, r *evidence.Redactor, res result) error {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return r.WriteFile(dir, "observer.json", append(data, '\n'))
}
