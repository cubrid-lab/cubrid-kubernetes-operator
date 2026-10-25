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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

// Target binds API and subprocess calls to one private, flattened context.
// Its credential file must be removed with Close after the run and cleanup.
type Target struct {
	config      *rest.Config
	path        string
	dir         string
	contextName string
	redactor    *evidence.Redactor
}

// OpenTarget snapshots only the named context of the explicitly selected
// kubeconfig. Ambient context changes cannot redirect either client.
func OpenTarget(path, contextName string) (*Target, error) {
	if path == "" || contextName == "" {
		return nil, errors.New("explicit kubeconfig and context names are required")
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = path
	raw, err := rules.Load()
	if err != nil {
		return nil, errors.New("could not load the selected kubeconfig")
	}
	selected := raw.Contexts[contextName]
	if selected == nil || raw.Clusters[selected.Cluster] == nil {
		return nil, errors.New("the selected context or cluster is absent from the kubeconfig")
	}
	cluster := raw.Clusters[selected.Cluster]
	auth := raw.AuthInfos[selected.AuthInfo]
	snapshot := clientcmdapi.NewConfig()
	snapshot.CurrentContext = contextName
	snapshot.Contexts[contextName] = selected.DeepCopy()
	snapshot.Clusters[selected.Cluster] = cluster.DeepCopy()
	if auth != nil {
		snapshot.AuthInfos[selected.AuthInfo] = auth.DeepCopy()
	}
	if err := clientcmdapi.FlattenConfig(snapshot); err != nil {
		return nil, errors.New("could not flatten the selected kubeconfig")
	}
	data, err := clientcmd.Write(*snapshot)
	if err != nil {
		return nil, errors.New("could not serialize the selected kubeconfig")
	}
	dir, err := os.MkdirTemp("", "cubrid-installed-target-")
	if err != nil {
		return nil, err
	}
	target := &Target{dir: dir, path: filepath.Join(dir, "config"), contextName: contextName,
		redactor: evidence.NewRedactor(cluster.Server)}
	if parsed, err := url.Parse(cluster.Server); err == nil {
		target.redactor.Add(parsed.Hostname())
	}
	if selectedAuth := snapshot.AuthInfos[selected.AuthInfo]; selectedAuth != nil {
		target.redactor.Add(selectedAuth.Token, selectedAuth.Password, string(selectedAuth.ClientKeyData))
		if selectedAuth.AuthProvider != nil {
			for _, value := range selectedAuth.AuthProvider.Config {
				target.redactor.Add(value)
			}
		}
		if selectedAuth.Exec != nil {
			for _, value := range selectedAuth.Exec.Env {
				target.redactor.Add(value.Value)
			}
		}
	}
	if err := os.WriteFile(target.path, data, 0o600); err != nil {
		_ = target.Close()
		return nil, err
	}
	config, err := clientcmd.BuildConfigFromFlags("", target.path)
	if err != nil {
		_ = target.Close()
		return nil, errors.New("could not initialize the selected target")
	}
	config.Timeout = 10 * time.Second
	target.config = config
	return target, nil
}

// Close removes the private credential snapshot.
func (t *Target) Close() error { return os.RemoveAll(t.dir) }

// Runner constructs both clients from this target's snapshot.
func (t *Target) Runner() (*Runner, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := databasev1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := client.New(t.config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, errors.New("could not initialize the selected Kubernetes client")
	}
	return &Runner{Client: c, Command: t.command, Redactor: t.redactor, forward: t.forward}, nil
}

// forwardReady bounds how long a port-forward may take to listen.
const forwardReady = 30 * time.Second

// forward opens kubectl port-forward to port of a Service through this
// target's snapshot, on a free port of 127.0.0.1. It goes through the
// kubelet: the lab's API server cannot reach the Pod network.
func (t *Target) forward(ctx context.Context, namespace, service, port string) (string, func(), error) {
	return startForward(ctx, func(ctx context.Context) *exec.Cmd {
		return t.Kubectl(ctx, "-n", namespace, "port-forward", "--address", "127.0.0.1", "service/"+service, ":"+port)
	}, forwardReady)
}

// forwardStderr bounds what a port-forward's error output keeps.
const forwardStderr = 64 << 10

// headBuffer keeps the first max bytes written to it and drops the rest.
type headBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *headBuffer) String() string { return b.buf.String() }

// forwardedPort is the line kubectl port-forward prints once it listens.
var forwardedPort = regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:(\d+) -> `)

// startForward starts the port-forward start returns and waits until it
// listens. It returns the local address and a func that ends the process
// and waits for it; the process also ends with ctx. On an error the
// process has already ended.
func startForward(ctx context.Context, start func(context.Context) *exec.Cmd, ready time.Duration) (
	string, func(), error) {
	pctx, cancel := context.WithCancel(ctx)
	cmd := start(pctx)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	// Read only after the process ended.
	stderr := &headBuffer{max: forwardStderr}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, err
	}
	done := make(chan struct{})
	ports := make(chan string, 1)
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if m := forwardedPort.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case ports <- m[1]:
				default:
				}
			}
		}
		// A line too long for the scanner ends it: the rest is read and
		// dropped, so that kubectl never blocks on a full pipe.
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()
	}()
	stop := func() {
		cancel()
		<-done
	}
	timer := time.NewTimer(ready)
	defer timer.Stop()
	select {
	case port := <-ports:
		return "127.0.0.1:" + port, stop, nil
	case <-done:
		cancel()
		return "", nil, fmt.Errorf("the port-forward ended before it listened: %s", strings.TrimSpace(stderr.String()))
	case <-timer.C:
		stop()
		return "", nil, fmt.Errorf("the port-forward did not listen within %s: %s", ready,
			strings.TrimSpace(stderr.String()))
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	}
}

// Kubectl returns a kubectl command bound to this target's snapshot, in a
// process group of its own that is killed when ctx ends, for a caller that
// needs the process itself, such as a port-forward.
func (t *Target) Kubectl(ctx context.Context, args ...string) *exec.Cmd {
	prefix := make([]string, 0, 4+len(args))
	prefix = append(prefix, "--kubeconfig", t.path, "--context", t.contextName)
	cmd := exec.CommandContext(ctx, "kubectl", append(prefix, args...)...)
	cmd.Env = append(os.Environ(), "KUBECTL_KUBERC=false")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	cmd.WaitDelay = time.Second
	return cmd
}

func (t *Target) command(ctx context.Context, args ...string) (string, error) {
	cmd := t.Kubectl(ctx, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("kubectl failed: %s: %s", err, t.redactor.Redact(output))
	}
	return string(output), nil
}
