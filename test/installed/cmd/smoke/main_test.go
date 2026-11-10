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
	"strings"
	"testing"
)

func TestMissingOrImplicitLabInputsAreRejected(t *testing.T) {
	for _, args := range [][]string{nil, {"--level=unit"}, {"--level=vm-lab"}, {"--level=kind", "arbitrary-command"}} {
		if err := run(t.Context(), args); err == nil {
			t.Fatalf("unsafe/incomplete command was accepted: %v", args)
		}
	}
}

func TestHelpDoesNotConnectToACluster(t *testing.T) {
	if err := run(t.Context(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
}

const vmLab = "--level=vm-lab"

// A check named by a key that is not one of the smoke command's, or with
// -namespace-mode beside it, is refused before any cluster is opened.
func TestAWrongCheckIsRefusedBeforeACluster(t *testing.T) {
	for _, args := range [][]string{
		{vmLab, "-check=installed-ha-sql-smoke"},
		{vmLab, "-check=LAB00"},
		{vmLab, "-check=standalone-database-sql"},
		{vmLab, "-check=installed-standalone-sql-smoke", "-namespace-mode=fresh"},
	} {
		err := run(t.Context(), args)
		if err == nil || !strings.Contains(err.Error(), "-check") {
			t.Errorf("%q = %v, want a refusal of -check", args, err)
		}
	}
}
