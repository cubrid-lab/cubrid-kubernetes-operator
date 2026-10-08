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

package faults

import "testing"

const (
	m0, m1, m2 = "m-0", "m-1", "m-2"
	id1, id2   = "uid-1/0", "uid-2/0"
)

func TestCheckOneMaster(t *testing.T) {
	for name, c := range map[string]struct {
		first, second []string
		fail          bool
	}{
		"one master":                   {[]string{m1}, []string{m1}, false},
		"no master yet":                {nil, nil, false},
		"handover seen in one sweep":   {[]string{m0, m1}, []string{m1}, false},
		"handover across sweeps":       {[]string{m0}, []string{m1}, false},
		"two masters in both sweeps":   {[]string{m0, m1}, []string{m1, m0}, true},
		"three masters, two confirmed": {[]string{m0, m1, m2}, []string{m0, m2}, true},
	} {
		if err := CheckOneMaster(c.first, c.second); (err != nil) != c.fail {
			t.Errorf("%s: CheckOneMaster(%v, %v) = %v, want failure %v", name, c.first, c.second, err, c.fail)
		}
	}
}

func TestCheckUnchanged(t *testing.T) {
	before := map[string]string{m1: id1, m2: id2}
	if err := CheckUnchanged(before, map[string]string{m0: "uid-new/0", m1: id1, m2: id2}); err != nil {
		t.Errorf("untouched members unchanged, the deleted one replaced: %v", err)
	}
	for name, after := range map[string]map[string]string{
		"replaced":         {m1: "uid-9/0", m2: id2},
		"restarted":        {m1: "uid-1/1", m2: id2},
		"not observed":     {m1: UnknownIdentity, m2: id2},
		"missing":          {m2: id2},
		"both changed":     {m1: "uid-1/2", m2: "uid-8/0"},
		"nothing observed": {},
	} {
		if err := CheckUnchanged(before, after); err == nil {
			t.Errorf("%s: judged unchanged", name)
		}
	}
	if err := CheckUnchanged(map[string]string{m1: UnknownIdentity}, map[string]string{m1: UnknownIdentity}); err == nil {
		t.Error("a member not observed before or after: judged unchanged")
	}
}
