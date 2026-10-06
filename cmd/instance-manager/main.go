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
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/internal/instancemanager"
)

func main() {
	logger, err := newLogger()
	if err != nil {
		fmt.Fprintln(os.Stderr, "instance-manager:", err)
		os.Exit(1)
	}
	// "instance-manager shutdown" is the one trigger of a stop: the preStop
	// hook and the entrypoint both run it (ADR-0003).
	if len(os.Args) > 1 && os.Args[1] == "shutdown" {
		if err := shutdown(logger); err != nil {
			logger.Error("Shutdown failed", "event", "shutdown_failed", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(logger); err != nil {
		logger.Error("Instance Manager stopped", "err", err)
		os.Exit(1)
	}
}

// shutdown asks the manager of this Pod to stop CUBRID, or stops it itself
// when no manager listens.
func shutdown(logger *slog.Logger) error {
	timeouts, err := timeoutsFromEnv()
	if err != nil {
		return err
	}
	budget := instancemanager.ShutdownBudget(timeouts)
	_, port, err := net.SplitHostPort(envOr("IM_ADDR", fmt.Sprintf(":%d", instancemanager.DefaultPort)))
	if err != nil {
		return fmt.Errorf("IM_ADDR: %w", err)
	}
	cli := instancemanager.LoggingCLI{CLI: instancemanager.ExecCLI{Timeout: budget}, Logger: logger}
	logger.Info("Shutdown requested", "event", "shutdown_requested")
	return instancemanager.RequestShutdown(context.Background(), "http://127.0.0.1:"+port,
		envOr("CUBRID_DB", "appdb"), cli, budget)
}

// newLogger builds the structured log on stdout. IM_LOG_LEVEL sets its level
// (debug, info, warn, error; info when unset). The token and the
// object-storage keys are registered so that they are never written.
func newLogger() (*slog.Logger, error) {
	level, err := instancemanager.ParseLogLevel(os.Getenv("IM_LOG_LEVEL"))
	if err != nil {
		return nil, fmt.Errorf("IM_LOG_LEVEL: %w", err)
	}
	member, _ := os.Hostname()
	return instancemanager.NewLogger(os.Stdout, instancemanager.LogSettings{
		Level:    level,
		Member:   member,
		Database: envOr("CUBRID_DB", "appdb"),
		Secrets:  []string{os.Getenv("IM_TOKEN"), os.Getenv("IM_S3_ACCESS_KEY"), os.Getenv("IM_S3_SECRET_KEY")},
	}), nil
}

func run(logger *slog.Logger) error {
	addr := envOr("IM_ADDR", fmt.Sprintf(":%d", instancemanager.DefaultPort))
	token := os.Getenv("IM_TOKEN")

	cli := instancemanager.LoggingCLI{CLI: instancemanager.ExecCLI{Timeout: 10 * time.Second}, Logger: logger}
	server := instancemanager.NewServer(cli, token).
		WithLogger(logger).
		// A backup may stage only below this root; it is also what a failed
		// backup removes. It matches the operator's backup staging path.
		WithBackupStagingRoot(envOr("IM_BACKUP_STAGING_ROOT", "/var/lib/cubrid/backup-staging")).
		WithRestoreRoots(instancemanager.RestoreRoots{
			// A restore may write only into the database root and stage only
			// below the staging root; both come from this process, never a request.
			Target:  envOr("CUBRID_DATABASES", "/var/lib/cubrid/databases"),
			Staging: envOr("IM_RESTORE_STAGING_ROOT", "/var/lib/cubrid/restore-staging"),
		})

	// A standalone member has no HA role; its readiness is its server's
	// status. The operator sets CUBRID_COMPONENTS=SERVER for one member.
	if os.Getenv("CUBRID_COMPONENTS") == "SERVER" {
		server = server.WithStandaloneDatabase(envOr("CUBRID_DB", "appdb"))
	} else {
		// An HA member reports, as a slave, how its applier is doing for the
		// master's log, which it copies next to its own database.
		server = server.WithReplication(envOr("CUBRID_DB", "appdb"),
			envOr("CUBRID_DATABASES", "/var/lib/cubrid/databases"))
	}

	// What an HA member needs to create the cluster's first database; the
	// endpoint refuses a standalone member.
	server = server.WithHAConfig(instancemanager.HAConfig{
		ConfPath:   envOr("CUBRID_HA_CONF", "/etc/cubrid-ha/cubrid_ha.conf"),
		VolumeSize: os.Getenv("CUBRID_VOLUME_SIZE"),
		Locale:     os.Getenv("CUBRID_LOCALE"),
	})

	timeouts, err := timeoutsFromEnv()
	if err != nil {
		return err
	}
	server = server.WithTimeouts(timeouts)

	// Durable operation store on the PVC enables the async /v1/backup +
	// /v1/operations endpoints; without it those endpoints stay disabled.
	if opsDir := os.Getenv("IM_OPERATIONS_DIR"); opsDir != "" {
		store, err := instancemanager.NewOperationStore(opsDir)
		if err != nil {
			return fmt.Errorf("open operation store: %w", err)
		}
		server = server.WithOperationStore(store)
	}

	// Object-storage credentials come only from the manager's env (never a
	// request body); without an endpoint configured, a backup cannot complete.
	if endpoint := os.Getenv("IM_S3_ENDPOINT"); endpoint != "" {
		objects, err := instancemanager.NewMinioObjectStore(instancemanager.ObjectStoreConfig{
			Endpoint:  endpoint,
			AccessKey: os.Getenv("IM_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("IM_S3_SECRET_KEY"),
			Region:    os.Getenv("IM_S3_REGION"),
			Secure:    os.Getenv("IM_S3_INSECURE") != "true",
		})
		if err != nil {
			return fmt.Errorf("init object store: %w", err)
		}
		server = server.WithObjectStore(objects)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	logger.Info("Started listening", "address", addr, "authenticated", token != "")

	// A CUBRID process that is gone without a request ends this process, and
	// with it the container, which Kubernetes then starts again (ADR-0003).
	grace, err := processGrace()
	if err != nil {
		return err
	}
	required := []string{"cub_master", "cub_server"}
	if os.Getenv("CUBRID_COMPONENTS") == "SERVER" {
		required = []string{"cub_server"}
	}
	watchdog := &instancemanager.Watchdog{
		Required: required, Running: instancemanager.RunningProcesses, Busy: server.StopIntended,
		Grace: grace, Logger: logger,
	}
	goneCh := make(chan error, 1)
	go func() { goneCh <- watchdog.Run(ctx) }()

	select {
	case err := <-goneCh:
		if err == nil {
			return nil // ctx ended first
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// timeoutsFromEnv reads the operation deadlines (Go durations, e.g. "3h").
// An unset variable keeps the default; an unparseable one is an error rather
// than a silently ignored setting.
func timeoutsFromEnv() (instancemanager.Timeouts, error) {
	var t instancemanager.Timeouts
	for key, dst := range map[string]*time.Duration{
		"IM_BACKUP_TIMEOUT":    &t.Backup,
		"IM_RESTORE_TIMEOUT":   &t.Restore,
		"IM_SHUTDOWN_TIMEOUT":  &t.Shutdown,
		"IM_BOOTSTRAP_TIMEOUT": &t.Bootstrap,
	} {
		v := os.Getenv(key)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return t, fmt.Errorf("%s=%q is not a positive duration", key, v)
		}
		*dst = d
	}
	return t, nil
}

// processGrace is how long a CUBRID process may be missing before the
// manager ends itself: IM_PROCESS_GRACE, a duration, 30s when unset.
func processGrace() (time.Duration, error) {
	value := os.Getenv("IM_PROCESS_GRACE")
	if value == "" {
		return 30 * time.Second, nil
	}
	grace, err := time.ParseDuration(value)
	if err != nil || grace <= 0 {
		return 0, fmt.Errorf("IM_PROCESS_GRACE=%q is not a positive duration such as 30s", value)
	}
	return grace, nil
}
