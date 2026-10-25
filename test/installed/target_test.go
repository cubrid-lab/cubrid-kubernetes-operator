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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func kubeconfigFile(t *testing.T) string {
	t.Helper()
	raw := clientcmdapi.NewConfig()
	raw.CurrentContext = "other"
	raw.Contexts["selected"] = &clientcmdapi.Context{Cluster: "selected-cluster", AuthInfo: "selected-user"}
	raw.Contexts["other"] = &clientcmdapi.Context{Cluster: "other-cluster", AuthInfo: "other-user"}
	raw.Clusters["selected-cluster"] = &clientcmdapi.Cluster{Server: "https://selected.example.invalid"}
	raw.Clusters["other-cluster"] = &clientcmdapi.Cluster{Server: "https://other.example.invalid"}
	raw.AuthInfos["selected-user"] = &clientcmdapi.AuthInfo{Token: "selected-private-token"}
	raw.AuthInfos["other-user"] = &clientcmdapi.AuthInfo{Token: "unrelated-private-token"}
	path := filepath.Join(t.TempDir(), "config")
	if err := clientcmd.WriteToFile(*raw, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTargetPinsOnlyTheSelectedContextAndRemovesItsCredentials(t *testing.T) {
	path := kubeconfigFile(t)
	target, err := OpenTarget(path, "selected")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	for _, check := range []struct {
		path string
		mode os.FileMode
	}{{target.dir, 0o700}, {target.path, 0o600}} {
		info, err := os.Stat(check.path)
		if err != nil || info.Mode().Perm() != check.mode {
			t.Fatalf("private snapshot permissions: %v, %v", info, err)
		}
	}
	pinned, err := clientcmd.LoadFromFile(target.path)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.CurrentContext != "selected" || len(pinned.Contexts) != 1 ||
		len(pinned.Clusters) != 1 || len(pinned.AuthInfos) != 1 {
		t.Fatalf("snapshot exposed another context: contexts=%v", pinned.Contexts)
	}
	if err := os.WriteFile(path, []byte("the caller replaced its kubeconfig"), 0o600); err != nil {
		t.Fatal(err)
	}
	if target.config.Host != "https://selected.example.invalid" || target.config.BearerToken != "selected-private-token" {
		t.Fatal("API client did not retain the selected configuration")
	}
	data, err := os.ReadFile(target.path)
	if err != nil || !strings.Contains(string(data), "selected-private-token") ||
		strings.Contains(string(data), "unrelated-private-token") {
		t.Fatal("snapshot changed with ambient configuration or retained unrelated credentials")
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target.path); !os.IsNotExist(err) {
		t.Fatalf("private snapshot was not removed: %v", err)
	}
}

func fakeKubectl(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestTargetCommandsUseTheSnapshotAndDisableKuberc(t *testing.T) {
	target, err := OpenTarget(kubeconfigFile(t), "selected")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "another-config"))
	t.Setenv("KUBECTL_KUBERC", "true")
	fakeKubectl(t, "printf '%s\\n' \"$KUBECTL_KUBERC\" \"$@\"\n")
	out, err := target.command(t.Context(), "version", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	want := "false\n--kubeconfig\n" + target.path + "\n--context\nselected\nversion\n-o\njson\n"
	if out != want {
		t.Fatalf("kubectl did not use the pinned target: %q", out)
	}
}

func TestTargetCommandsHonorCancellationAndRedactErrors(t *testing.T) {
	target, err := OpenTarget(kubeconfigFile(t), "selected")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	fakeKubectl(t, "printf 'selected-private-token' >&2\nexit 1\n")
	_, err = target.command(t.Context(), "version")
	if err == nil || strings.Contains(err.Error(), "selected-private-token") {
		t.Fatalf("command error exposed credentials: %v", err)
	}
	fakeKubectl(t, "exec sleep 30\n")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := target.command(ctx, "version"); err == nil {
		t.Fatal("cancelled command succeeded")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancelled kubectl outlived its bounded stop")
	}
}

func TestTargetRejectsImplicitOrMissingContexts(t *testing.T) {
	path := kubeconfigFile(t)
	for _, pair := range [][2]string{{"", "selected"}, {path, ""}, {path, "missing"}} {
		if target, err := OpenTarget(pair[0], pair[1]); err == nil {
			_ = target.Close()
			t.Fatal("implicit or missing target was accepted")
		}
	}
}

func TestStartForwardReturnsThePortAndEndsTheProcess(t *testing.T) {
	var cmd *exec.Cmd
	addr, stop, err := startForward(t.Context(), func(ctx context.Context) *exec.Cmd {
		cmd = exec.CommandContext(ctx, "sh", "-c", "echo 'Forwarding from 127.0.0.1:41234 -> 8333'; exec sleep 60")
		return cmd
	}, 10*time.Second)
	if err != nil || addr != "127.0.0.1:41234" {
		t.Fatalf("startForward() = %q, %v; want the forwarded port", addr, err)
	}
	stop()
	if cmd.ProcessState == nil {
		t.Fatal("the port-forward still runs after stop")
	}
}

func TestStartForwardFailsWhenTheProcessEndsBeforeItListens(t *testing.T) {
	addr, stop, err := startForward(t.Context(), func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "echo 'error: services \"seaweedfs\" not found' >&2; exit 1")
	}, 10*time.Second)
	if err == nil || stop != nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("startForward() = %q, %v; want the process's error", addr, err)
	}
}
