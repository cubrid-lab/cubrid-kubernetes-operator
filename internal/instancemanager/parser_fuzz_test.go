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

package instancemanager

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Keep parser mutations small. The manifest size and object count limits have
// their own boundary tests in restore_bounds_test.go.
const parserFuzzInputLimit = 4 * 1024

func FuzzParseHAStatus(f *testing.F) {
	const localMember = "Node cub-0 (priority 1, state master)"
	header, _, _ := strings.Cut(masterOut, "\n")
	seeds := []struct {
		input string
		want  Role
	}{
		{masterOut, RoleMaster},
		{slaveOut, RoleSlave},
		{strings.ReplaceAll(slaveOut, "state slave)", "state replica)"), RoleReplica},
		{transitionOut, RoleUnknown},
		{"", RoleUnknown},
		{header, RoleUnknown},
		{strings.Replace(masterOut, "\n   "+localMember, "", 1), RoleUnknown},
		{strings.Replace(masterOut, localMember, localMember+"\n   "+localMember, 1), RoleUnknown},
		{strings.Replace(masterOut, localMember, "Node cub-0 (priority 1, state slave)", 1), RoleUnknown},
	}
	expected := make(map[string]Role, len(seeds))
	for _, seed := range seeds {
		expected[seed.input] = seed.want
		f.Add(seed.input)
	}

	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > parserFuzzInputLimit {
			t.Skip()
		}
		got := ParseHAStatus(input)
		if want, ok := expected[input]; ok && got.Role != want {
			t.Fatalf("seed role = %q, want %q", got.Role, want)
		}
		if got.Role == RoleUnknown {
			if got.Reason == "" {
				t.Error("an unknown role should have an explanation")
			}
			return
		}
		matches := 0
		for _, node := range got.Nodes {
			if node.Name != got.Current {
				continue
			}
			matches++
			if node.State != string(got.Role) {
				t.Errorf("local node %q has state %q, but parser reported role %q", got.Current, node.State, got.Role)
			}
		}
		if matches != 1 {
			t.Errorf("known role %q has %d local node entries, want exactly one", got.Role, matches)
		}
	})
}

func FuzzParseManifest(f *testing.F) {
	checksum := strings.Repeat("a", 64)
	manifest := func(version, size int, digest string) string {
		return fmt.Sprintf(`{"manifestVersion":%d,"database":"seeddb","sourceRole":"master",`+
			`"objects":[{"key":"seeddb_bk0v000","sizeBytes":%d,"sha256":%q}]}`, version, size, digest)
	}
	seeds := []struct {
		input string
		valid bool
	}{
		{manifest(manifestVersionRequestedRole, 17, checksum), true},
		{manifest(ManifestVersion, 17, checksum), true},
		{manifest(ManifestVersion+1, 17, checksum), false},
		{manifest(ManifestVersion, -1, checksum), false},
		{manifest(ManifestVersion, 17, "incomplete-checksum"), false},
		{manifest(ManifestVersion, 17, strings.ToUpper(checksum)), false},
		{`{"manifestVersion":`, false},
	}
	expected := make(map[string]bool, len(seeds))
	for _, seed := range seeds {
		expected[seed.input] = seed.valid
		f.Add(seed.input)
	}

	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > parserFuzzInputLimit {
			t.Skip()
		}
		got, err := ParseManifest([]byte(input))
		if valid, ok := expected[input]; ok && (err == nil) != valid {
			t.Fatalf("seed acceptance = %t, want %t, error = %v", err == nil, valid, err)
		}
		if err != nil {
			return
		}
		canonical, err := got.Marshal()
		if err != nil {
			t.Fatalf("canonical encoding of a parsed manifest: %v", err)
		}
		roundTrip, err := ParseManifest(canonical)
		if err != nil {
			t.Fatalf("canonical manifest could not be parsed: %v", err)
		}
		if !reflect.DeepEqual(got, roundTrip) {
			t.Error("canonical manifest encoding changed its fields")
		}
	})
}
