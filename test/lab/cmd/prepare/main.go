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
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const maxPlanBytes = 64 << 10

var fullCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

type plan struct {
	Commit               string   `json:"commit"`
	Lane                 string   `json:"lane"`
	ScenarioKeys         []string `json:"scenarioKeys"`
	OperatorImage        string   `json:"operatorImage"`
	InstanceManagerImage string   `json:"instanceManagerImage"`
	ClientImage          string   `json:"clientImage"`
	EvidenceDirectory    string   `json:"evidenceDirectory"`
}

func main() {
	path := flag.String("plan", "", "path to a local VM-lab preparation plan")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := prepare(*path, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func prepare(path string, out io.Writer) error {
	p, err := readPlan(path)
	if err != nil {
		return err
	}
	head, err := exec.Command("git", "rev-parse", "--verify", "HEAD").Output()
	if err != nil {
		return errors.New("could not resolve HEAD in the local Git checkout")
	}
	if err := p.validate(strings.TrimSpace(string(head))); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Offline inputs verified for %s at %s (%d required results).\n"+
		"Authorization, live preflight and the campaign Lease are still required.\n",
		p.Lane, p.Commit, len(p.ScenarioKeys))
	return err
}

func readPlan(path string) (plan, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return plan{}, errors.New("the plan must be a readable regular file")
	}
	if info.Size() > maxPlanBytes {
		return plan{}, errors.New("the plan exceeds 64 KiB")
	}
	f, err := os.Open(path)
	if err != nil {
		return plan{}, errors.New("could not open the plan")
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPlanBytes+1))
	if err != nil || len(data) > maxPlanBytes {
		return plan{}, errors.New("could not read the plan within its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var p plan
	if err := decoder.Decode(&p); err != nil {
		return plan{}, errors.New("the plan must use the documented JSON fields and value types")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return plan{}, errors.New("the plan must contain one JSON object")
	}
	return p, nil
}

func (p plan) validate(head string) error {
	switch {
	case !fullCommit.MatchString(p.Commit):
		return errors.New("commit must be a full lowercase Git SHA")
	case p.Commit != head:
		return errors.New("commit does not match the local checkout's HEAD")
	case !evidence.ValidDigestImage(p.OperatorImage):
		return errors.New("operatorImage must be a repository pinned by SHA-256")
	case !evidence.ValidDigestImage(p.InstanceManagerImage):
		return errors.New("instanceManagerImage must be a repository pinned by SHA-256")
	case !evidence.ValidDigestImage(p.ClientImage):
		return errors.New("clientImage must be a repository pinned by SHA-256")
	}
	if _, err := evidence.RequirementsForKeys(p.Lane, evidence.LevelVMLab, p.ScenarioKeys...); err != nil {
		// Registry errors include the supplied strings. Keep them local to the
		// validator so a malformed plan cannot write them into review logs.
		return errors.New("lane and scenarioKeys must name all registered results of one VM-lab lane exactly once")
	}
	if p.EvidenceDirectory == "" {
		return errors.New("evidenceDirectory is required")
	}
	entries, err := os.ReadDir(p.EvidenceDirectory)
	if err != nil {
		return errors.New("evidenceDirectory must be an existing readable local directory")
	}
	if len(entries) > 0 {
		return errors.New("evidenceDirectory must be empty")
	}
	return nil
}
