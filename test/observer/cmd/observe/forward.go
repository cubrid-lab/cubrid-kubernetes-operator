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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/observer"
)

// forwardedPort is the line kubectl port-forward prints once it listens.
var forwardedPort = regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:(\d+) -> `)

// forwarder reaches Prometheus through kubectl port-forward, which goes
// through the kubelet: the lab's API server cannot reach the Pod network, so
// its service proxy cannot be used. A port-forward that ended, for example
// because the Prometheus Pod was replaced, is started again at the next
// query; the query that finds it ended fails, and its tick records the
// metrics as unavailable.
type forwarder struct {
	// start returns the port-forward command, bound to ctx.
	start func(context.Context) *exec.Cmd
	// ready bounds how long a port-forward may take to listen.
	ready time.Duration
	http  *http.Client

	mu     sync.Mutex
	url    string
	done   chan struct{}
	cancel context.CancelFunc
}

type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := 4096 - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}

// ensure returns the URL of a running port-forward, starting one when none
// runs.
func (f *forwarder) ensure(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done != nil {
		select {
		case <-f.done:
		default:
			return f.url, nil
		}
	}
	f.stopLocked()
	pctx, cancel := context.WithCancel(context.Background())
	cmd := f.start(pctx)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("could not start the port-forward to prometheus: %w", err)
	}
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("could not start the port-forward to prometheus: %w", err)
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
		_ = cmd.Wait()
	}()
	timer := time.NewTimer(f.ready)
	defer timer.Stop()
	listening := func(port string) (string, error) {
		f.url, f.done, f.cancel = "http://127.0.0.1:"+port, done, cancel
		return f.url, nil
	}
	select {
	case port := <-ports:
		return listening(port)
	case <-done:
		select {
		case port := <-ports:
			// It listened, then ended: the next query finds it ended.
			return listening(port)
		default:
		}
		cancel()
		return "", fmt.Errorf("the port-forward to prometheus ended before it listened: %s", stderr.String())
	case <-timer.C:
		cancel()
		<-done
		return "", fmt.Errorf("the port-forward to prometheus did not listen within %s: %s", f.ready, stderr.String())
	case <-ctx.Done():
		cancel()
		<-done
		return "", ctx.Err()
	}
}

func (f *forwarder) exited() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.done == nil {
		return false
	}
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

func (f *forwarder) stopLocked() {
	if f.cancel != nil {
		f.cancel()
		<-f.done
	}
	f.url, f.done, f.cancel = "", nil, nil
}

// Close stops the port-forward.
func (f *forwarder) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopLocked()
}

// Range queries Prometheus through the port-forward.
func (f *forwarder) Range(
	ctx context.Context, selector string, window time.Duration, at time.Time,
) ([]observer.Series, error) {
	url, err := f.ensure(ctx)
	if err != nil {
		return nil, err
	}
	p := observer.Prometheus{URL: url, HTTP: f.http}
	return p.Range(ctx, selector, window, at)
}
