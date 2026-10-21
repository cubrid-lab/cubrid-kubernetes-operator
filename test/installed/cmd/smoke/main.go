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

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/installed"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) (runErr error) {
	flags := flag.NewFlagSet("installed-smoke", flag.ContinueOnError)
	var path, targetContext, namespaceMode string
	var cfg installed.Config
	flags.StringVar(&path, "kubeconfig", "", "explicit target kubeconfig (credentials stay private)")
	flags.StringVar(&targetContext, "context", "", "explicit context to snapshot")
	flags.StringVar(&cfg.ClusterUID, "cluster-uid", "", "expected kube-system namespace UID")
	flags.StringVar(&cfg.Namespace, "namespace", "", "explicit disposable run namespace")
	flags.StringVar(&namespaceMode, "namespace-mode", "fresh", "fresh or explicitly preallocated namespace")
	flags.StringVar(&cfg.NamespaceUID, "namespace-uid", "", "expected preallocated namespace UID")
	flags.StringVar(&cfg.OwnerConfigMap, "owner-configmap", "", "external immutable owner ConfigMap name")
	flags.StringVar(&cfg.OwnerUID, "owner-uid", "", "expected external owner ConfigMap UID")
	flags.StringVar(&cfg.OwnerNonce, "owner-nonce", "", "expected external owner ConfigMap data.nonce")
	flags.StringVar(&cfg.OperatorNamespace, "operator-namespace", "", "namespace of the installed Operator")
	flags.StringVar(&cfg.OperatorDeployment, "operator-deployment", "", "installed manager Deployment name")
	flags.StringVar(&cfg.OperatorImage, "operator-image", "", "expected digest or main-commit manager image")
	flags.StringVar(&cfg.InstanceManagerImage, "instance-manager-image", "",
		"expected digest or main-commit database image")
	flags.StringVar(&cfg.Commit, "commit", "", "full selected source revision")
	flags.StringVar(&cfg.StorageClass, "storage-class", "", "explicit StorageClass for the 2Gi data volume")
	flags.StringVar(&cfg.EvidenceDir, "evidence-dir", "", "empty directory for summary, JUnit and SQL evidence")
	flags.StringVar(&cfg.Level, "level", "", "evidence environment: kind or vm-lab")
	flags.DurationVar(&cfg.Timeout, "timeout", 10*time.Minute, "whole smoke deadline")
	flags.DurationVar(&cfg.CleanupTimeout, "cleanup-timeout", 2*time.Minute, "independent cleanup deadline")
	flags.DurationVar(&cfg.Poll, "poll", time.Second, "readiness/cleanup polling interval")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || (cfg.Level != "kind" && cfg.Level != "vm-lab") {
		return errors.New("choose kind or vm-lab explicitly and provide no positional command")
	}
	cfg.NamespaceMode = installed.NamespaceMode(namespaceMode)
	target, err := installed.OpenTarget(path, targetContext)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, target.Close()) }()
	runner, err := target.Runner()
	if err != nil {
		return err
	}
	return runner.Run(ctx, cfg)
}
