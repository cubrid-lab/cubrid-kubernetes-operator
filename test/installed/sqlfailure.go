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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
)

// Each read of the state after a failed csql statement has its own bound, so
// that the whole capture takes at most 18 s before cleanup starts.
const (
	podReadTimeout       = 3 * time.Second
	serverAgeTimeout     = 10 * time.Second
	serverLogTailTimeout = 5 * time.Second
	// serverIdentityLines bounds the lines kept of the identity read.
	serverIdentityLines = 12
)

// sqlFailureState is what the smoke read from the database Pod after a csql
// statement failed or printed -199 and -111 (#238), to tell a restart of
// cub_server or of its container from a client-side hang-up. A read that
// failed is recorded in ReadError; none of it changes the verdict.
type sqlFailureState struct {
	// Step is the index in "sql" of the first statement that failed or printed
	// the pair.
	Step int `json:"step"`
	// ReadAt is when the Pod was read. The reads below follow it.
	ReadAt time.Time `json:"readAt"`
	// RestartCountAtStart and RestartCount are the database container's
	// restart count when the smoke began and at ReadAt. They are left out when
	// the Pod could not be read.
	RestartCountAtStart *int32 `json:"restartCountAtStart,omitempty"`
	RestartCount        *int32 `json:"restartCount,omitempty"`
	// ContainerIDAtStart and ContainerID are the container's ID then and now.
	ContainerIDAtStart string `json:"containerIDAtStart"`
	ContainerID        string `json:"containerID,omitempty"`
	// ServerAtPair is what the member printed: "now <epoch seconds>" and
	// "uptime <seconds>" of the member, cub_server's identity ("cub_server
	// <pid> <start time in clock ticks since boot>", "ha_mode <mode>", where
	// "ha_mode ?" is expected on a standalone member) and "clk_tck <ticks per
	// second>". The server's age is uptime - start/clk_tck: if it is less than
	// the time since the failed statement, the server restarted after it.
	ServerAtPair []string `json:"serverAtPair,omitempty"`
	// ServerLogTail is the last lines of the newest server error log.
	ServerLogTail []string `json:"serverLogTail,omitempty"`
	// ReadError says why a field above is missing or partial.
	ReadError string `json:"readError,omitempty"`
}

// databaseRestartCount returns the restart count of the database container,
// or nil when the Pod cannot be read.
func (r *Runner) databaseRestartCount(ctx context.Context, cfg Config, rec *record) *int32 {
	bounded, cancel := context.WithTimeout(ctx, podReadTimeout)
	defer cancel()
	pod := &corev1.Pod{}
	if err := r.Client.Get(bounded, client.ObjectKey{Namespace: cfg.Namespace, Name: rec.Database.Pod}, pod); err != nil {
		return nil
	}
	if status := databaseContainerStatus(pod); status != nil {
		return &status.RestartCount
	}
	return nil
}

// readSQLFailureState reads the database Pod and cub_server's state in it. It
// does not go through the identity check of the SQL calls, which refuses a
// Pod whose container changed: that change is what it must show. It does not
// end with ctx, which may be cancelled before cleanup, and a failed read is
// recorded, never returned.
func (r *Runner) readSQLFailureState(
	ctx context.Context, cfg Config, rec *record, step int, startRestarts *int32,
) *sqlFailureState {
	ctx = context.WithoutCancel(ctx)
	state := &sqlFailureState{Step: step, ReadAt: time.Now().UTC(),
		RestartCountAtStart: startRestarts, ContainerIDAtStart: rec.Database.ContainerID}
	var problems []string
	podCtx, cancel := context.WithTimeout(ctx, podReadTimeout)
	pod := &corev1.Pod{}
	if err := r.Client.Get(podCtx, client.ObjectKey{Namespace: cfg.Namespace, Name: rec.Database.Pod}, pod); err != nil {
		problems = append(problems, "pod: "+err.Error())
	} else if status := databaseContainerStatus(pod); status != nil {
		state.RestartCount, state.ContainerID = &status.RestartCount, status.ContainerID
	} else {
		problems = append(problems, "pod: no status of the database container")
	}
	cancel()
	read := func(script string, timeout time.Duration) (string, error) {
		bounded, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return r.Command(bounded, "-n", cfg.Namespace, "exec", rec.Database.Pod, "-c", databaseContainer, "--",
			"bash", "-c", script, databaseName)
	}
	// The identity comes first: the age rule is only as good as the time
	// between the failed statement and this read.
	if out, err := read(sqlcheck.ServerAgeScript, serverAgeTimeout); err != nil {
		problems = append(problems, "identity: "+err.Error())
	} else {
		state.ServerAtPair = sqlcheck.BoundLines(out, serverIdentityLines, sqlcheck.ServerLogLineWidth)
	}
	if out, err := read(sqlcheck.ServerLogTailScript, serverLogTailTimeout); err != nil {
		problems = append(problems, "server log: "+err.Error())
	} else {
		state.ServerLogTail = sqlcheck.BoundLines(out, sqlcheck.ServerLogTailLines, sqlcheck.ServerLogLineWidth)
	}
	state.ReadError = sqlcheck.Cut(strings.Join(problems, "; "), sqlcheck.ServerLogLineWidth)
	return state
}
