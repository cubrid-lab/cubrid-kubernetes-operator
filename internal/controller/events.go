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

package controller

import (
	"k8s.io/client-go/tools/record"
)

const (
	// eventBurst is how many Events about one object are sent in a row, and
	// eventsPerSecond how fast that allowance comes back: one Event in ten
	// seconds. client-go's defaults are 25 and one in five minutes, which a
	// cluster uses up with a few failovers or a token rotation; what exceeds
	// the allowance is dropped without a trace. The Operator records an Event
	// for a change of state, not for an observation, so the larger allowance
	// is used by real changes only.
	eventBurst      = 100
	eventsPerSecond = 0.1
)

// NewEventBroadcaster returns the broadcaster the manager records Events
// with: client-go's, with an allowance per object that fits a cluster whose
// state changes often. A cluster that keeps changing state beyond it still
// loses Events; the Operator's transition log lines are the complete record
// (docs/observability.md).
func NewEventBroadcaster() record.EventBroadcaster {
	return record.NewBroadcasterWithCorrelatorOptions(record.CorrelatorOptions{
		BurstSize: eventBurst,
		QPS:       eventsPerSecond,
	})
}
