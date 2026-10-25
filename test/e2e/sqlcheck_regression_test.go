//go:build e2e
// +build e2e

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

package e2e

import (
	"strings"
	"testing"
)

// TestSQLChecks preserves the existing Kind helpers' behavior when they forward
// to the installation-independent checks. These cases passed before extraction.
func TestSQLChecks(t *testing.T) {
	var script string
	if err := writeS00Row(func(sql string) (string, error) {
		script = sql
		return "committed", nil
	}, "marker-123"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "INSERT INTO "+s00Table+" (id, marker) SELECT 1, 'marker-123' FROM db_root") {
		t.Fatalf("unexpected write SQL: %q", script)
	}
	if err := readS00Row(func(sql string) (string, error) {
		if sql != "SELECT id, marker FROM "+s00Table+" ORDER BY id;" {
			t.Errorf("unexpected read SQL: %q", sql)
		}
		return "1 'marker-123'\n1 row selected", nil
	}, "marker-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := runSQL(func(string) (string, error) {
		return "ERROR: statement failed", nil
	}, "SELECT 1;"); err == nil {
		t.Fatal("accepted csql error output")
	}
	if err := readS00Row(func(string) (string, error) {
		return "1 'other'\n1 row selected", nil
	}, "marker-123"); err == nil {
		t.Fatal("accepted wrong marker")
	}
}
