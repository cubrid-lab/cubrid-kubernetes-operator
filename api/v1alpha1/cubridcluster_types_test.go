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

package v1alpha1

import (
	"strings"
	"testing"
)

func TestValidateClusterName(t *testing.T) {
	tests := []struct {
		name    string
		cluster string
		wantErr bool
		errMsg  string
	}{
		{
			name:    "valid single character name",
			cluster: "c",
			wantErr: false,
		},
		{
			name:    "valid kebab-case name",
			cluster: "cubrid-cluster-01",
			wantErr: false,
		},
		{
			name:    "valid 52-character boundary name",
			cluster: strings.Repeat("a", 52),
			wantErr: false,
		},
		{
			name:    "empty name rejected",
			cluster: "",
			wantErr: true,
			errMsg:  "cluster name cannot be empty",
		},
		{
			name:    "53-character name rejected",
			cluster: strings.Repeat("a", 53),
			wantErr: true,
			errMsg:  "exceeds maximum allowed length of 52 characters",
		},
		{
			name:    "61-character name rejected",
			cluster: strings.Repeat("a", 61),
			wantErr: true,
			errMsg:  "exceeds maximum allowed length of 52 characters",
		},
		{
			name:    "name with dots rejected",
			cluster: "prod.example",
			wantErr: true,
			errMsg:  msgNotDNS1035Label,
		},
		{
			name:    "name starting with digit rejected",
			cluster: "1cluster",
			wantErr: true,
			errMsg:  msgNotDNS1035Label,
		},
		{
			name:    "name ending with hyphen rejected",
			cluster: "cluster-",
			wantErr: true,
			errMsg:  msgNotDNS1035Label,
		},
		{
			name:    "name with uppercase rejected",
			cluster: "Cluster",
			wantErr: true,
			errMsg:  msgNotDNS1035Label,
		},
		{
			name:    "name with underscore rejected",
			cluster: "cluster_name",
			wantErr: true,
			errMsg:  msgNotDNS1035Label,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateClusterName(tt.cluster)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateClusterName(%q) error = %v, wantErr %v", tt.cluster, err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("ValidateClusterName(%q) error %q does not contain %q", tt.cluster, err.Error(), tt.errMsg)
			}
		})
	}
}
