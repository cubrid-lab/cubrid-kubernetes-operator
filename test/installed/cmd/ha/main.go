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

// Command ha runs LAB01 against an installed Operator: a three-member HA
// cluster in a fresh namespace, S01's starting state, the SQL workload client
// and the data rules on every member and both Services, and with
// -fault operator-restart S06 restart after it, or with -backup-bucket S11
// complete, and with -restore also S12 exact, followed by S12 tampered,
// wrong-target or operator-restart when selected
// (docs/testing/installed-ha.md).
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
	flags := flag.NewFlagSet("installed-ha", flag.ContinueOnError)
	var path, targetContext string
	var cfg installed.HAConfig
	flags.StringVar(&path, "kubeconfig", "", "explicit target kubeconfig (credentials stay private)")
	flags.StringVar(&targetContext, "context", "", "explicit context to snapshot")
	flags.StringVar(&cfg.ClusterUID, "cluster-uid", "", "expected kube-system namespace UID")
	flags.StringVar(&cfg.Namespace, "namespace", "", "explicit fresh run namespace (cubrid-lab-*)")
	flags.StringVar(&cfg.OperatorNamespace, "operator-namespace", "", "namespace of the installed Operator")
	flags.StringVar(&cfg.OperatorDeployment, "operator-deployment", "", "installed manager Deployment name")
	flags.StringVar(&cfg.OperatorImage, "operator-image", "", "expected digest or main-commit manager image")
	flags.StringVar(&cfg.InstanceManagerImage, "instance-manager-image", "",
		"expected digest or main-commit database image")
	flags.StringVar(&cfg.WorkloadImage, "workload-image", "", "SQL workload client image, pinned by digest")
	flags.StringVar(&cfg.Commit, "commit", "", "full selected source revision")
	flags.StringVar(&cfg.StorageClass, "storage-class", "", "explicit StorageClass for the 2Gi data volumes")
	flags.StringVar(&cfg.SeedEndpoint, "seed-endpoint", "", "object store host[:port] the members are seeded through")
	flags.StringVar(&cfg.SeedBucket, "seed-bucket", "", "bucket for the seed backups")
	flags.BoolVar(&cfg.SeedInsecure, "seed-insecure", false, "reach the object store over plain HTTP")
	flags.StringVar(&cfg.SeedSecretNamespace, "seed-secret-namespace", "", "namespace of the object store Secret")
	flags.StringVar(&cfg.SeedSecret, "seed-secret", "", "Secret with accessKey and secretKey, copied into the run")
	flags.IntVar(&cfg.Operations, "operations", 200, "workload operations through the read-write Service")
	flags.DurationVar(&cfg.DataCheckWithin, "data-check-within", 2*time.Minute,
		"how long after the workload the data rules may take to hold")
	flags.DurationVar(&cfg.StableFor, "stable-for", 15*time.Second,
		"how long the starting state must keep holding before the workload starts")
	flags.StringVar(&cfg.Fault, "fault", "",
		"scenario after LAB01: empty for none, or operator-restart for S06 restart (deletes the manager Pod)")
	flags.DurationVar(&cfg.StablePeriod, "stable-period", 30*time.Second,
		"S06: how long the clients must stay acknowledged after the fault (stable_period)")
	flags.DurationVar(&cfg.ResyncLimit, "resync-limit", 2*time.Minute,
		"S06: bound for the replacement manager's first fresh status, S12 operator-restart: for its return "+
			"(operator_resync_limit)")
	flags.StringVar(&cfg.LeaderLease, "leader-lease", "91d4fe35.cubrid.io",
		"S06 and S12 operator-restart: the manager's leader election Lease in its namespace")
	flags.StringVar(&cfg.BackupBucket, "backup-bucket", "",
		"scenario after LAB01: S11 complete, a full backup to this bucket; not with -fault")
	flags.DurationVar(&cfg.BackupLimit, "backup-limit", 15*time.Minute,
		"S11: bound from the backup request until Completed or Failed (backup_limit)")
	flags.StringVar(&cfg.ObjectStoreService, "object-store-service", "",
		"S11: <namespace>/<service>:<port> of the object store, port-forwarded for the run's own download")
	flags.BoolVar(&cfg.Restore, "restore", false,
		"scenario after S11: S12 exact, a new cluster restored from S11's artifact; needs -backup-bucket")
	flags.DurationVar(&cfg.RestoreLimit, "restore-limit", 20*time.Minute,
		"S12: bound from the restored cluster's creation until it is Ready (restore_limit)")
	flags.BoolVar(&cfg.RestoreTampered, "restore-tampered", false,
		"scenario after S12 exact: S12 tampered, a copy of S11's artifact with one byte changed must be "+
			"refused by a new cluster; needs -restore")
	flags.BoolVar(&cfg.RestoreWrongTarget, "restore-wrong-target", false,
		"scenario after S12 exact: S12 wrong-target, S11's artifact must be refused by a new cluster over the "+
			"retained volumes of another that hold a database; needs -restore, not with -restore-tampered")
	flags.BoolVar(&cfg.RestoreOperatorRestart, "restore-operator-restart", false,
		"scenario after S12 exact: S12 operator-restart, S11's artifact restored into another new cluster while "+
			"the manager Pod is deleted during the restore; needs -restore, not with -restore-tampered or "+
			"-restore-wrong-target")
	flags.StringVar(&cfg.EvidenceDir, "evidence-dir", "", "empty directory for summary, JUnit and LAB01 evidence")
	flags.StringVar(&cfg.Level, "level", "", "evidence environment: kind or vm-lab")
	flags.DurationVar(&cfg.Timeout, "timeout", 30*time.Minute, "whole run deadline")
	flags.DurationVar(&cfg.CleanupTimeout, "cleanup-timeout", 5*time.Minute, "independent cleanup deadline")
	flags.DurationVar(&cfg.Poll, "poll", 3*time.Second, "polling interval")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || (cfg.Level != "kind" && cfg.Level != "vm-lab") {
		return errors.New("choose kind or vm-lab explicitly and provide no positional command")
	}
	cfg.NamespaceMode = installed.FreshNamespace
	target, err := installed.OpenTarget(path, targetContext)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, target.Close()) }()
	runner, err := target.Runner()
	if err != nil {
		return err
	}
	return runner.RunHA(ctx, cfg)
}
