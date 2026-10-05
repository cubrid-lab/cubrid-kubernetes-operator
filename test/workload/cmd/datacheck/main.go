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

// Command datacheck compares the ledger rows read from one or more members
// with the history a workload client recorded, and prints the result as
// data-check.json. It exits with 1 when a data rule is broken and with 2 when
// its input cannot be read.
//
//	datacheck -history history.jsonl -rows rw=rw.jsonl -rows ro=ro.jsonl
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

// rowFiles collects the repeated -rows name=file flag.
type rowFiles map[string]string

func (f rowFiles) String() string { return fmt.Sprint(map[string]string(f)) }

func (f rowFiles) Set(value string) error {
	name, file, found := strings.Cut(value, "=")
	if !found || name == "" || file == "" {
		return fmt.Errorf("want name=file, got %q", value)
	}
	if _, seen := f[name]; seen {
		return fmt.Errorf("%s was given twice", name)
	}
	f[name] = file
	return nil
}

func main() {
	os.Exit(run())
}

func run() int {
	historyFile := flag.String("history", "", "the history.jsonl a workload client wrote")
	rows := rowFiles{}
	flag.Var(rows, "rows", "name=file: the ledger rows dumped from one member or endpoint (repeatable)")
	flag.Parse()
	if *historyFile == "" || len(rows) == 0 {
		flag.Usage()
		return 2
	}

	f, err := os.Open(*historyFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	history, err := workload.ReadHistory(f)
	_ = f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	members := map[string][]workload.Row{}
	for name, file := range rows {
		f, err := os.Open(file)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		members[name], err = workload.ReadRows(f)
		_ = f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
			return 2
		}
	}

	report := workload.Check(history, members)
	out, err := json.MarshalIndent(struct {
		Operations map[string]int `json:"operations"`
		workload.Report
	}{history.Counts, report}, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Println(string(out))
	if !report.OK {
		return 1
	}
	return 0
}
