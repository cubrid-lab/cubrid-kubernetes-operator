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
	"errors"
	"fmt"

	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults/netfault"
)

// -partition-direction makes the cut between the cut-off member and each
// other member one-way, the contract's asymmetric shape (#115). The manager
// Pod stays cut both ways whatever the direction: the faults never enter it,
// so only a cut both ways can be confirmed and replaced
// (-partition-operator-restart), and its traffic with the member is TCP,
// whose connections a drop in either direction ends anyway.
const (
	// s07DirectionBoth drops the member traffic both ways: the lane's cut
	// before the option existed, and its default.
	s07DirectionBoth = "both"
	// s07DirectionInbound drops what the other members send to the cut-off
	// member; what it sends them still arrives.
	s07DirectionInbound = "inbound"
	// s07DirectionOutbound drops what the cut-off member sends to the other
	// members; what they send it still arrives.
	s07DirectionOutbound = "outbound"
)

// validateS07Direction refuses a direction that is not both, inbound or
// outbound, and a one-way direction without an S07 partition fault. Every
// other S07 option is allowed with a one-way cut: none of them depends on
// the direction of the member cuts.
func (c HAConfig) validateS07Direction() error {
	switch c.PartitionDirection {
	case "", s07DirectionBoth:
		return nil
	case s07DirectionInbound, s07DirectionOutbound:
		if !c.partitionFault() {
			return errors.New("-partition-direction " + c.PartitionDirection + " needs an S07 partition fault")
		}
		return nil
	}
	return fmt.Errorf("-partition-direction must be %s, %s or %s, not %q", s07DirectionBoth, s07DirectionInbound,
		s07DirectionOutbound, c.PartitionDirection)
}

// s07Direction is the direction of the run's member cuts.
func (c HAConfig) s07Direction() string { return s07DirectionOf(c.PartitionDirection) }

// s07DirectionOf reads a recorded direction: a record written before the
// option existed has none, and its cut was both ways.
func s07DirectionOf(direction string) string {
	if direction == "" {
		return s07DirectionBoth
	}
	return direction
}

// s07MemberCut is the netfault direction of a cut between the cut-off
// member, the partition's target, and another member. A value that is none
// of the three is passed on as it is, so that the partition's own validation
// refuses it rather than a different cut being made.
func s07MemberCut(direction string) netfault.Direction {
	switch s07DirectionOf(direction) {
	case s07DirectionBoth:
		return netfault.Both
	case s07DirectionInbound:
		return netfault.In
	case s07DirectionOutbound:
		return netfault.Out
	}
	return netfault.Direction(direction)
}

// direction is the direction of the record's member cuts.
func (rec *s07Record) direction() string { return s07DirectionOf(rec.PartitionDirection) }

// s07CutDirection names a netfault cut's direction as -partition-direction
// does, from the target's side, for cut.json.
func s07CutDirection(d netfault.Direction) string {
	switch d {
	case netfault.Both:
		return s07DirectionBoth
	case netfault.In:
		return s07DirectionInbound
	case netfault.Out:
		return s07DirectionOutbound
	}
	return string(d)
}
