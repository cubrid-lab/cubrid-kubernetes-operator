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
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/faults"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/workload"
)

const (
	// FaultPVCLossRebuild selects the rebuild after the loss of a member's
	// volume (scenario S10): once the base HA run (LAB01) has left the
	// cluster in its checked state, the run replaces a slave's data claim and
	// Pod as an administrator replacing lost storage, and requests the
	// rebuild the Operator offers for it while a client keeps writing.
	FaultPVCLossRebuild = "pvc-loss-rebuild"
	s10Lane             = "lab-installed-ha-s10"
	s10ID               = "S10"
	s10RecordFile       = "S10/record.json"
	s10ListingFile      = "S10/new-volume-before-rebuild.txt"
	s10DataCheckFile    = "S10/data-check.json"
	// s10Client writes through the read-write Service from before the
	// rebuild request until the rebuilt member answers as a slave again.
	s10Client = "s10rb"
	// rebuildMemberAnnotation requests the rebuild of a member; its value
	// is "<member>:<token>", or "<member>:<token>:<source>" for an offer
	// with formerPrimary (docs/observability.md, "Rebuilding a member").
	rebuildMemberAnnotation = "database.cubrid.io/rebuild-member"
	// The verdicts of the read-only listing of the new volume's database
	// directory before the rebuild, as the Kind step records them.
	s10NoDatabase    = "no database files"
	s10HoldsDatabase = "holds database files"
	s10NotListed     = "not observed: the database directory of the new Pod could not be listed"
	// s10NotObserved is a measurement this lane does not take.
	s10NotObserved = "not observed"
)

// The bounds of S10's own steps; tests shorten them. They are waits for the
// run to go on, not the contract's limit: the rebuild is judged by
// rebuild_limit alone. s10ReplaceBound bounds the replacement of the claim and
// Pod, s10ListBound the listing of the new volume, s10OfferBound the offer,
// including the manual release of a write quarantine when the target
// outranks the master, s10RebuildBound the rebuild from the request until it
// ends, and s10SlaveBound the rebuilt member's return as a slave. s10Slack
// covers the reads without a bound of their own: the claims, the fail count
// and the client's history.
var (
	s10ReplaceBound = 5 * time.Minute
	s10ListBound    = 2 * time.Minute
	s10OfferBound   = 10 * time.Minute
	s10RebuildBound = 10 * time.Minute
	s10SlaveBound   = 3 * time.Minute
	s10Slack        = time.Minute
	// s10RebuildMargin is added to a set rebuild_limit for the rebuild's
	// wait, so that the wait never ends before the limit could judge it.
	s10RebuildMargin = 5 * time.Minute
)

// s10WriteTimes is s10Record.WriteTimes.
const s10WriteTimes = "client answer times (client node clock) compared with acceptedTime and endTime of " +
	"status.lastMemberRebuild (Operator, manager node clock, whole seconds); clock skew between the hosts is not " +
	"measured or corrected"

// s10RebuildWait bounds the wait for the rebuild: s10RebuildBound, or
// rebuild_limit and s10RebuildMargin when that is longer.
func (c HAConfig) s10RebuildWait() time.Duration {
	if c.RebuildLimit > 0 {
		return max(s10RebuildBound, c.RebuildLimit+s10RebuildMargin)
	}
	return s10RebuildBound
}

// s10Unobserved is a step whose outcome was not seen within its bound: S10 is
// blocked, not failed, because nothing that was seen broke a rule.
type s10Unobserved struct{ error }

// s10ErrorResult is the result of a step that ended with err: blocked when
// the step's outcome was not seen in time, failed otherwise.
func s10ErrorResult(err error) evidence.Result {
	if errors.As(err, new(s10Unobserved)) {
		return evidence.Blocked
	}
	return evidence.Fail
}

// s10ListScript lists, read-only, the database directory of a member's
// container and says whether it holds the database: a directory of the
// database or its line in databases.txt. It is the Kind step's listing.
const s10ListScript = `d="$CUBRID_DATABASES"; db="$CUBRID_DB"; echo "directory: $d, database: $db"; ` +
	`[ -n "$d" ] && [ -n "$db" ] || { echo "verdict: unknown"; exit 0; }; ls -lA "$d" 2>&1; ` +
	`if [ -e "$d/$db" ] || grep -qs "^$db[[:space:]]" "$d/databases.txt"; ` +
	`then echo "verdict: holds-database"; else echo "verdict: no-database"; fi`

// failCountLine is the applier's fail count in cubrid applyinfo's output.
var failCountLine = regexp.MustCompile(`Fail count\s*:\s*(\d+)`)

// s10Claim is a member's data claim as the run read it: its UID, the volume
// it is bound to and whether it is being deleted. An empty UID is a claim
// that does not exist.
type s10Claim struct {
	Name     string `json:"name"`
	UID      string `json:"uid,omitempty"`
	Volume   string `json:"volume,omitempty"`
	Deleting bool   `json:"deleting,omitempty"`
}

// s10Offer is the offer the run requested, without its token.
type s10Offer struct {
	Member        string `json:"member"`
	ClaimUID      string `json:"claimUID"`
	VolumeName    string `json:"volumeName"`
	Source        string `json:"source"`
	FormerPrimary bool   `json:"formerPrimary,omitempty"`
}

// s10Rebuild is status.lastMemberRebuild of the run's request, without its
// token.
type s10Rebuild struct {
	Member       string     `json:"member"`
	Source       string     `json:"source"`
	ClaimUID     string     `json:"claimUID"`
	OperationID  string     `json:"operationID"`
	State        string     `json:"state"`
	Message      string     `json:"message,omitempty"`
	AcceptedTime time.Time  `json:"acceptedTime"`
	EndTime      *time.Time `json:"endTime,omitempty"`
}

// s10Record is what S10 records besides its data report: the target and why
// it was chosen, every member's claim before and after, the fault and its
// confirmation, the release of a write quarantine when the target outranks
// the master, the offer, the rebuild, and the rebuilt member's return.
type s10Record struct {
	Master string   `json:"master"`
	Slaves []string `json:"slaves"`
	Member string   `json:"member"`
	// OutranksMaster says that the target has a lower ordinal than the only
	// active master: its loss starts a write quarantine only a person may
	// release (rebuildTarget).
	OutranksMaster bool                `json:"outranksMaster"`
	ClaimsBefore   map[string]s10Claim `json:"claimsBefore"`
	PodUIDBefore   string              `json:"podUIDBefore,omitempty"`
	ClaimDeletedAt *time.Time          `json:"claimDeletedAt,omitempty"`
	PodDeletedAt   *time.Time          `json:"podDeletedAt,omitempty"`
	// PodNotDeleted says that the claim delete was accepted and the Pod
	// delete was not: the claim stays Terminating while the Pod holds it.
	PodNotDeleted bool `json:"claimDeletedPodNotDeleted,omitempty"`
	// WriteTimes says how the client's answers were placed inside the
	// rebuild: their times are the client node's clock, the rebuild's
	// acceptance and end the Operator's, on other hosts, and the run does
	// not correct for the difference.
	WriteTimes string    `json:"whileRebuildWriteTimes"`
	NewClaim   *s10Claim `json:"newClaim,omitempty"`
	NewPodUID  string    `json:"newPodUID,omitempty"`
	NewVolume  string    `json:"newVolumeBeforeRebuild,omitempty"`
	// ReleasedQuarantine is the generation of the write quarantine the run
	// released manually, when it released one.
	ReleasedQuarantine string      `json:"releasedQuarantine,omitempty"`
	Offer              *s10Offer   `json:"offer,omitempty"`
	OfferedAt          *time.Time  `json:"offeredAt,omitempty"`
	RequestedAt        *time.Time  `json:"requestedAt,omitempty"`
	Rebuild            *s10Rebuild `json:"rebuild,omitempty"`
	SlaveAgainAt       *time.Time  `json:"slaveAgainAt,omitempty"`
	FailCount          *int        `json:"rebuiltMemberApplierFailCount,omitempty"`
	// ClaimsAfter are the claims of the members that were not rebuilt, read
	// after the data check.
	ClaimsAfter    map[string]s10Claim `json:"claimsAfter,omitempty"`
	ClientStopped  bool                `json:"clientStopped"`
	ClientStarted  bool                `json:"clientStarted"`
	Unmet          string              `json:"unmet,omitempty"`
	FaultConfirmed bool                `json:"faultConfirmed"`
	// ReleaseTokenRemovedAt is when the run removed a release token it had
	// set that no release consumed, and ReleaseTokenRemoveError why that
	// removal failed.
	ReleaseTokenRemovedAt   *time.Time `json:"releaseTokenRemovedByRun,omitempty"`
	ReleaseTokenRemoveError string     `json:"releaseTokenRemoveError,omitempty"`
	// releaseToken is the last release token the run set; it is never
	// written to the evidence.
	releaseToken string
}

// rebuildTarget picks the slave whose storage S10 replaces, by the Kind
// step's rule. members are the cluster's members in the order of their
// ordinals. A member that gives no role while it outranks the only active
// master (has a lower ordinal) is an unverified member, and the Operator
// quarantines the read-write path for it (ADR-0005, section 8); a member with
// an empty data volume never answers, so that quarantine is not released
// automatically. The first slave ranked below the master is therefore
// preferred. When every slave outranks the master, the first slave is
// returned with outranksMaster set, and the run releases the quarantine as an
// administrator would. A master that is not among members counts as ranked
// below every slave. The master is never returned.
func rebuildTarget(master string, slaves, members []string) (member string, outranksMaster bool) {
	if at := slices.Index(members, master); at >= 0 {
		for _, m := range members[at+1:] {
			if slices.Contains(slaves, m) {
				return m, false
			}
		}
	}
	return slaves[0], true
}

// s10StepBounds is the longest S10 may take after LAB01 by the bounds of its
// own steps: the replacement of the claim and Pod, the listing, the offer
// with a manual release, the client's start, the rebuild, the member's
// return, the client's stop, the data check and the reads without a bound.
func (c HAConfig) s10StepBounds() time.Duration {
	return s10ReplaceBound + s10ListBound + s10OfferBound + s06ClientStart + c.s10RebuildWait() + s10SlaveBound +
		soakStopBound + c.DataCheckWithin + readSlack + s10Slack
}

func (c HAConfig) validateS10() error {
	if c.Fault != FaultPVCLossRebuild {
		return nil
	}
	if c.Level != evidence.LevelVMLab {
		return errors.New("S10 deletes a member's data claim: it runs on the vm-lab level only")
	}
	if c.RebuildLimit < 0 {
		return errors.New("S10 needs a rebuild limit that is not negative (0 is unset)")
	}
	if least := c.s10StepBounds(); c.Timeout <= least {
		return fmt.Errorf("S10 needs a -timeout longer than %s, the bounds of its own steps (replacing the claim "+
			"and Pod, listing the new volume, waiting for the offer with a manual release, starting the client, the "+
			"rebuild, the member's return, stopping the client and the data check), plus the time LAB01 takes",
			least)
	}
	return nil
}

// s10Measurements are S10's measurements before anything was measured. The
// names are the Kind step's where they mean the same thing.
func s10Measurements() map[string]any {
	m := map[string]any{"requestNamingMasterRefused": s10NotObserved,
		"requestWithWrongTokenIgnored":                s10NotObserved,
		"rebuiltMemberReportedHealthyBeforeDataMatch": s10NotObserved}
	for _, name := range []string{"newVolumeBeforeRebuild", "offerPublishedAfterLoss", "rebuildRequestedToCompleted",
		"rebuildRequestedToAllRowsChecked", "writeQuarantineReleasedManually", "rebuiltMemberApplierFailCount",
		"rowsOnRebuiltMember", "acknowledgedMissing", "insertsAcknowledgedWhileRebuildRan",
		"insertsFailedWhileRebuildRan", "insertsWithUnknownOutcomeWhileRebuildRan", "rebuildTargetWasMaster",
		"otherMembersClaimsReplaced", "masterMoved"} {
		m[name] = evidence.Unknown
	}
	return m
}

// runS10 runs the rebuild after the loss of a member's volume (scenario S10)
// on the cluster the base HA run (LAB01) left in its checked state, as
// docs/observability.md ("Rebuilding a member") tells an administrator to:
//
//  1. a slave is chosen by rebuildTarget, never the master, and every
//     member's data claim UID is recorded;
//  2. the slave's claim is deleted with its UID as a precondition, then its
//     Pod with its UID; the StatefulSet makes an empty claim and a new Pod.
//     The fault is confirmed by a claim with a new UID bound to a new
//     volume, a new Pod, and a read-only listing of the new database
//     directory without the database;
//  3. when the slave outranks the master, the write quarantine its loss
//     starts is released with its current token, as the manual release
//     documents;
//  4. a client starts writing through the read-write Service, and the
//     offered token is set in the rebuild annotation;
//  5. the rebuild must end Completed, naming the slave, the master as source
//     and the new claim, and the slave must answer as a slave again with
//     its new claim recorded;
//  6. the data rules must hold on every member and both Services, including
//     the client's rows, the rebuilt member's applier must have no failure,
//     and no other member's claim may have another UID.
//
// The result is judged by rebuild_limit, from the request until every row was
// checked; unset, as the contract leaves it, the result is blocked. A run
// that ends before S10 was judged leaves it blocked, unless something that
// must never happen was seen.
func (h *haRun) runS10(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s10ID, &result)()
	result = evidence.Scenario{ID: s10ID, Result: evidence.Fail, NeverEvents: []string{}}
	measured := s10Measurements()
	rec := s10Record{ClaimsBefore: map[string]s10Claim{}, WriteTimes: s10WriteTimes}
	defer func() {
		if rec.ClientStarted && !rec.ClientStopped {
			// A client left writing would change the rows after the verdict.
			err := h.stopBackgroundClients(ctx, []string{s10Client})
			rec.ClientStopped = err == nil
			if err != nil && result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, "stop the client: "+err.Error()
			}
		}
		h.removeS10ReleaseToken(ctx, &rec)
		if err := h.writeJSON(s10RecordFile, rec); err != nil {
			result.Result, result.Reason = evidence.Fail, "write S10 evidence: "+err.Error()
		}
		for name, value := range measured {
			if result.Measurements == nil {
				result.Measurements = map[string]any{}
			}
			result.Measurements[name] = value
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, s10ID+"/")
		})
	}()
	end := func(r evidence.Result, format string, args ...any) evidence.Scenario {
		result.Result, result.Reason = r, fmt.Sprintf(format, args...)
		// A run that ended before S10 was judged is blocked, unless it had
		// already seen something that must never happen.
		if ctx.Err() != nil && r != evidence.Pass && len(result.NeverEvents) == 0 {
			result.Result, result.Reason = evidence.Blocked, "the run ended before S10 was judged: "+result.Reason
		}
		return result
	}
	confirmed := false
	result.FaultConfirmed = &confirmed
	result.Limits = map[string]string{faults.RebuildLimit.Name: faults.Limit{Name: faults.RebuildLimit.Name,
		Value: h.cfg.RebuildLimit}.String()}

	if err := h.s10Start(ctx, &rec); err != nil {
		return end(evidence.Blocked, "%v", err)
	}
	member := rec.Member
	measured["rebuildTargetWasMaster"] = false
	err := h.s10ReplaceStorage(ctx, &rec)
	measured["claimDeletedPodNotDeleted"] = rec.PodNotDeleted
	if err != nil {
		if errors.As(err, new(s10Unconfirmed)) {
			return end(evidence.Blocked, "the fault is not confirmed: %v", err)
		}
		return end(evidence.Blocked, "%v", err)
	}
	lostAt := *rec.PodDeletedAt

	rec.NewVolume = h.s10ListNewVolume(ctx, member)
	measured["newVolumeBeforeRebuild"] = rec.NewVolume
	confirmed = rec.NewVolume == s10NoDatabase
	rec.FaultConfirmed = confirmed
	if rec.NewVolume == s10HoldsDatabase {
		return end(evidence.Fail, "the new claim of %s already holds the database before any rebuild", member)
	}

	token, err := h.s10WaitOffer(ctx, &rec)
	measured["writeQuarantineReleasedManually"] = rec.ReleasedQuarantine != ""
	if err != nil {
		if _, wrong := errors.AsType[s10Wrong](err); wrong {
			return end(evidence.Fail, "%v", err)
		}
		return end(evidence.Blocked, "%v", err)
	}
	measured["offerPublishedAfterLoss"] = rec.OfferedAt.Sub(lostAt).Round(time.Millisecond).String()

	if err := h.startBackgroundClient(ctx, s10Client); err != nil {
		return end(evidence.Blocked, "start the client: %v", err)
	}
	rec.ClientStarted = true
	value := member + ":" + token
	if rec.Offer.FormerPrimary {
		// Only the last resolved primary needs its source named; a slave
		// of the starting state normally is not one.
		value += ":" + rec.Offer.Source
	}
	requested := time.Now().UTC()
	rec.RequestedAt = &requested
	if err := h.setS10Annotation(ctx, rebuildMemberAnnotation, value); err != nil {
		return end(evidence.Blocked, "request the rebuild: %v", err)
	}

	done, err := h.s10WaitRebuild(ctx, &rec, token)
	if done != nil {
		measured["rebuildTargetWasMaster"] = done.Member == rec.Master
		if done.Member == rec.Master {
			result.NeverEvents = append(result.NeverEvents, "the rebuild named the master "+rec.Master+
				", whose volume was chosen as a rebuild target")
		}
	}
	if err != nil {
		return end(s10ErrorResult(err), "%v", err)
	}
	measured["rebuildRequestedToCompleted"] = time.Since(requested).Round(time.Millisecond).String()
	if done.EndTime != nil {
		measured["rebuildRequestedToCompleted"] = done.EndTime.Time.Sub(requested).Round(time.Millisecond).String()
	}

	moved, err := h.s10WaitSlave(ctx, &rec)
	measured["masterMoved"] = moved
	if err != nil {
		return end(s10ErrorResult(err), "%v", err)
	}
	cleaned := true
	result.CleanupSucceeded = &cleaned

	r, reason := h.judgeS10(ctx, &rec, &result, measured, requested, done)
	return end(r, "%s", reason)
}

// judgeS10 stops and reads the client once the rebuilt member answers as a
// slave again, applies the data rules to every member and both Services,
// reads the rebuilt member's applier fail count and the other members'
// claims, and returns S10's result and reason: an outcome that must never
// happen, a broken data rule or applier failures fail it; an observation that
// could not be made blocks it; otherwise rebuild_limit judges it, and a pass
// needs the fault confirmed and an insert acknowledged while the rebuild ran.
func (h *haRun) judgeS10(ctx context.Context, rec *s10Record, result *evidence.Scenario, measured map[string]any,
	requested time.Time, done *databasev1alpha1.MemberRebuild) (evidence.Result, string) {
	member := rec.Member
	if err := h.stopBackgroundClients(ctx, []string{s10Client}); err != nil {
		return evidence.Fail, "stop the client: " + err.Error()
	}
	rec.ClientStopped = true
	answers, err := h.collectBackgroundClients(ctx, []string{s10Client}, requested)
	if err != nil {
		return evidence.Blocked, "collect the client: " + err.Error()
	}
	result.Operations = &answers.operations
	during := insertsWhileRebuilt(answers.events[s10Client], done)
	measured["insertsAcknowledgedWhileRebuildRan"] = during[workload.Acknowledged]
	measured["insertsFailedWhileRebuildRan"] = during[workload.Failed]
	measured["insertsWithUnknownOutcomeWhileRebuildRan"] = during[workload.Unknown]

	report, _, dataErr := h.checkDataOf(ctx, h.history.String(), h.sourceTarget(), time.Now(), s10DataCheckFile)
	allRows := time.Since(requested)
	measured["rebuildRequestedToAllRowsChecked"] = allRows.Round(time.Millisecond).String()
	if report != nil {
		measured["acknowledgedMissing"] = report.AcknowledgedMissing
		if m, ok := report.Members[member]; ok {
			measured["rowsOnRebuiltMember"] = m.Rows
		}
	}

	failCount, failErr := h.s10FailCount(ctx, member, rec.Master)
	if failErr == nil {
		rec.FailCount = &failCount
		measured["rebuiltMemberApplierFailCount"] = failCount
	}

	replaced, claimsErr := h.s10OtherClaims(ctx, rec)
	if claimsErr == nil {
		measured["otherMembersClaimsReplaced"] = len(replaced) > 0
	}
	result.NeverEvents = append(result.NeverEvents, replaced...)

	switch {
	case len(result.NeverEvents) > 0:
		return evidence.Fail, strings.Join(result.NeverEvents, "; ")
	case errors.As(dataErr, new(dataViolation)):
		return evidence.Fail, dataErr.Error()
	case failErr == nil && failCount != 0:
		return evidence.Fail, fmt.Sprintf("the applier of the rebuilt member %s has fail count %d", member, failCount)
	case dataErr != nil:
		return evidence.Blocked, "the data rules could not be checked: " + dataErr.Error()
	case failErr != nil:
		return evidence.Blocked, "the fail count of the rebuilt member's applier was not read: " + failErr.Error()
	case claimsErr != nil:
		return evidence.Blocked, "the claims of the members that were not rebuilt were not read again: " +
			claimsErr.Error()
	}

	verdict := faults.Judge(faults.Bound{Limit: faults.Limit{Name: faults.RebuildLimit.Name,
		Value: h.cfg.RebuildLimit}, Measured: allRows})
	result.Limits = verdict.Limits
	if verdict.Result != evidence.Pass {
		return verdict.Result, verdict.Reason
	}
	// A time-limit pass is not a pass of S10 unless the fault and the rows
	// written while the rebuild ran were observed.
	switch {
	case !rec.FaultConfirmed:
		return evidence.Blocked, "the fault is not confirmed: " + rec.NewVolume
	case during[workload.Acknowledged] == 0:
		return evidence.Blocked, "no insert was acknowledged while the rebuild ran, so the data check " +
			"includes no row written during it"
	}
	return evidence.Pass, fmt.Sprintf("%s was rebuilt from %s on a new empty claim within rebuild_limit; the data rules "+
		"held on every member and both Services, its applier has no failure, and no other member's claim was "+
		"replaced", member, rec.Master)
}

// s10Unconfirmed is a fault the run could not confirm: the claim or the Pod
// was not seen replaced.
type s10Unconfirmed struct{ error }

// s10Wrong is an Operator answer S10 does not accept, as against one it did
// not see.
type s10Wrong struct{ error }

// s10Start checks the state S10 starts from on the run's own cluster, chooses
// the target and records every member's claim and the target's Pod.
func (h *haRun) s10Start(ctx context.Context, rec *s10Record) error {
	cluster, err := h.s07OwnCluster(ctx)
	if err != nil {
		return err
	}
	switch {
	case cluster.Status.WriteQuarantine != nil:
		return errors.New("a write quarantine is in force before the fault, and no rebuild is offered under one")
	case cluster.Status.MemberRebuild != nil:
		return fmt.Errorf("a rebuild of %s is in progress before the fault", cluster.Status.MemberRebuild.Member)
	}
	roles, unmet, err := h.roles(ctx)
	if err != nil || unmet != "" {
		return fmt.Errorf("read the roles before the fault: %w", errors.Join(err, errors.New(unmet)))
	}
	rec.Master, rec.Slaves = roles.Master, roles.Slaves
	// Never the master: the Operator refuses to rebuild the current
	// primary, and losing the master's storage is a failover, not S10.
	rec.Member, rec.OutranksMaster = rebuildTarget(roles.Master, roles.Slaves, h.members)
	if rec.Member == rec.Master {
		return fmt.Errorf("the target %s is the master", rec.Member)
	}
	recorded := slices.IndexFunc(cluster.Status.MemberDataClaims, func(d databasev1alpha1.MemberDataClaim) bool {
		return d.Name == rec.Member
	})
	if recorded < 0 {
		return fmt.Errorf("status.memberDataClaims records no claim of %s holding the database", rec.Member)
	}
	for _, m := range h.members {
		claim, err := h.s10ReadClaim(ctx, m)
		if err != nil {
			return err
		}
		if claim.UID == "" || claim.Volume == "" || claim.Deleting {
			return fmt.Errorf("the claim %s is not a bound claim that is not being deleted: %+v", claim.Name, claim)
		}
		rec.ClaimsBefore[m] = claim
	}
	// The claim the run deletes must be the one the Operator saw holding
	// the database, or the run would not be replacing lost storage.
	want, live := cluster.Status.MemberDataClaims[recorded].ClaimUID, rec.ClaimsBefore[rec.Member].UID
	if want != live {
		return fmt.Errorf("status.memberDataClaims records the claim %s of %s, not its live claim %s; nothing was "+
			"deleted", want, rec.Member, live)
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.Member}, pod); err != nil {
		return fmt.Errorf("read the Pod %s: %w", rec.Member, err)
	}
	rec.PodUIDBefore = string(pod.UID)
	return nil
}

// s10ReadClaim reads a member's data claim; a claim that does not exist has
// no UID.
func (h *haRun) s10ReadClaim(ctx context.Context, member string) (s10Claim, error) {
	claim := s10Claim{Name: dataVolumePrefix + member}
	pvc := &corev1.PersistentVolumeClaim{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: claim.Name}, pvc)
	if apierrors.IsNotFound(err) {
		return claim, nil
	}
	if err != nil {
		return claim, fmt.Errorf("read the claim %s: %w", claim.Name, err)
	}
	claim.UID, claim.Volume, claim.Deleting = string(pvc.UID), pvc.Spec.VolumeName, pvc.DeletionTimestamp != nil
	return claim, nil
}

// s10ReplaceStorage deletes the target's claim, then its Pod, each in the run
// namespace with its recorded UID as a precondition, so that the API server
// refuses to delete a claim or Pod that replaced the one the run checked. A
// refused claim delete deletes no Pod. It then waits for the StatefulSet's
// empty claim, bound to a new volume, and the new Pod.
func (h *haRun) s10ReplaceStorage(ctx context.Context, rec *s10Record) error {
	if err := h.guard(ctx); err != nil {
		return err
	}
	if err := h.s10StillTarget(ctx, rec); err != nil {
		return fmt.Errorf("%w; nothing was deleted", err)
	}
	old := rec.ClaimsBefore[rec.Member]
	claimUID, podUID := types.UID(old.UID), types.UID(rec.PodUIDBefore)
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: old.Name, Namespace: h.cfg.Namespace}}
	if err := h.Client.Delete(ctx, claim, client.Preconditions{UID: &claimUID}); err != nil {
		return fmt.Errorf("delete the claim %s (UID %s); nothing was deleted: %w", old.Name, old.UID, err)
	}
	deleted := time.Now().UTC()
	rec.ClaimDeletedAt = &deleted
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: rec.Member, Namespace: h.cfg.Namespace}}
	if err := h.Client.Delete(ctx, pod, client.Preconditions{UID: &podUID}); err != nil {
		rec.PodNotDeleted = true
		rec.Unmet = fmt.Sprintf("claim %s deleted, Pod %s not deleted (%v); the claim is Terminating and the "+
			"namespace needs attention", old.Name, rec.Member, err)
		return errors.New(rec.Unmet)
	}
	gone := time.Now().UTC()
	rec.PodDeletedAt = &gone
	deadline := time.Now().Add(s10ReplaceBound)
	for {
		claim, err := h.s10ReadClaim(ctx, rec.Member)
		unmet := ""
		switch {
		case err != nil:
			unmet = err.Error()
		case claim.UID == "" || claim.UID == old.UID:
			unmet = "the claim " + old.Name + " is not replaced"
		case claim.Deleting:
			unmet = "the new claim " + old.Name + " is being deleted"
		case claim.Volume == "" || claim.Volume == old.Volume:
			unmet = "the new claim " + old.Name + " is not bound to a new volume"
		default:
			now := &corev1.Pod{}
			err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.Member}, now)
			switch {
			case apierrors.IsNotFound(err), err == nil && string(now.UID) == rec.PodUIDBefore:
				unmet = "the Pod " + rec.Member + " is not replaced"
			case err != nil:
				unmet = err.Error()
			default:
				rec.NewClaim, rec.NewPodUID, rec.Unmet = &claim, string(now.UID), ""
				return nil
			}
		}
		rec.Unmet = unmet
		if time.Now().After(deadline) || ctx.Err() != nil {
			return s10Unconfirmed{fmt.Errorf("%s within %s of the deletion", unmet, s10ReplaceBound)}
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Poll):
		}
	}
}

// s10StillTarget reads the roles and the target's Pod again right before the
// delete: the master must be the one the target was chosen against, the
// target must still answer as a slave, and its Pod must be the one recorded.
func (h *haRun) s10StillTarget(ctx context.Context, rec *s10Record) error {
	roles, unmet, err := h.roles(ctx)
	switch {
	case err != nil || unmet != "":
		return fmt.Errorf("read the roles before the delete: %w", errors.Join(err, errors.New(unmet)))
	case roles.Master != rec.Master:
		return fmt.Errorf("the master is %s before the delete, not %s as when %s was chosen", roles.Master,
			rec.Master, rec.Member)
	case !slices.Contains(roles.Slaves, rec.Member):
		return fmt.Errorf("%s no longer answers as a slave before the delete", rec.Member)
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: rec.Member}, pod); err != nil {
		return fmt.Errorf("read the Pod %s before the delete: %w", rec.Member, err)
	}
	if string(pod.UID) != rec.PodUIDBefore {
		return fmt.Errorf("the Pod %s has UID %s before the delete, not %s", rec.Member, pod.UID, rec.PodUIDBefore)
	}
	return nil
}

// s10ListNewVolume lists, read-only, the database directory of the new Pod
// before any rebuild and keeps the listing. The container may not accept a
// command at first; when it accepts none within s10ListBound, the listing is
// not observed.
func (h *haRun) s10ListNewVolume(ctx context.Context, member string) string {
	verdict, listing := s10NotListed, ""
	deadline := time.Now().Add(s10ListBound)
	for {
		out, err := h.member(ctx, member, "sh", "-c", s10ListScript)
		if err == nil {
			listing = out
			if strings.Contains(out, "verdict: no-database") {
				verdict = s10NoDatabase
				break
			}
			if strings.Contains(out, "verdict: holds-database") {
				verdict = s10HoldsDatabase
				break
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Poll):
		}
	}
	_ = h.write(s10ListingFile, []byte(verdict+"\n"+listing))
	return verdict
}

// s10WaitOffer waits for the Operator's offer for the target's new claim and
// returns its token, which goes to the redactor and nowhere else. When the
// target outranks the master, a WriteQuarantinedMemberUnverified quarantine is
// released first with its current token, as the manual release documents. A
// manual release accepts, for every member the quarantine records, that writes
// only that member acknowledged may be lost. The run accepts that only for the
// target alone: its claim was replaced by an empty one, so it holds no write
// to lose. A quarantine is therefore released only when it records exactly
// one member, the target, with the target's new Pod UID; a quarantine that
// records any other member, or has another reason, is not released and S10
// is blocked. s10Start needs one master and two answering slaves, so the
// target is never the last resolved primary. An offer whose source is not the
// master is an s10Wrong.
func (h *haRun) s10WaitOffer(ctx context.Context, rec *s10Record) (string, error) {
	deadline := time.Now().Add(s10OfferBound)
	lastSet := ""
	for {
		cluster, err := h.s07OwnCluster(ctx)
		unmet := ""
		switch {
		case err != nil:
			unmet = err.Error()
		case cluster.Status.WriteQuarantine != nil:
			q := cluster.Status.WriteQuarantine
			unmet = fmt.Sprintf("a write quarantine (%s) is in force", q.Reason)
			if !rec.OutranksMaster {
				break
			}
			if q.Reason != databasev1alpha1.WriteQuarantinedMemberUnverified {
				return "", fmt.Errorf("the write quarantine in force has the reason %s, not %s; S10 releases only "+
					"the quarantine of a member without storage", q.Reason,
					databasev1alpha1.WriteQuarantinedMemberUnverified)
			}
			names := make([]string, 0, len(q.Members))
			for _, m := range q.Members {
				names = append(names, m.Name)
			}
			if len(q.Members) != 1 || q.Members[0].Name != rec.Member {
				return "", fmt.Errorf("the write quarantine %s records the members %v; S10 releases a quarantine "+
					"only when it records the target %s alone, and released nothing", q.GenerationID, names,
					rec.Member)
			}
			// The release covers the Pod the quarantine records; a record
			// made before the new Pod existed is revised, with a new token,
			// once the Operator sees the new Pod.
			if q.Members[0].PodUID != rec.NewPodUID || q.ReleaseToken == "" {
				unmet += ", not yet naming the new Pod of " + rec.Member + " with a release token"
				break
			}
			h.redactor.Add(q.ReleaseToken)
			if q.ReleaseToken != lastSet {
				if err := h.setS10Annotation(ctx, releaseWriteQuarantineAnnotation, q.ReleaseToken); err != nil {
					return "", fmt.Errorf("release the write quarantine %s: %w", q.GenerationID, err)
				}
				lastSet = q.ReleaseToken
				rec.releaseToken = lastSet
			}
			rec.ReleasedQuarantine = q.GenerationID
		case rec.ReleasedQuarantine != "" && !s10Released(cluster, rec.ReleasedQuarantine):
			unmet = "status.lastWriteQuarantineRelease does not record the manual release of " +
				rec.ReleasedQuarantine
		default:
			if rec.ReleasedQuarantine != "" {
				if ready, what := h.s10BrokersBack(ctx); !ready {
					unmet = what
					break
				}
			}
			i := slices.IndexFunc(cluster.Status.MemberRebuildOffers, func(o databasev1alpha1.MemberRebuildOffer) bool {
				return o.Member == rec.Member && o.ClaimUID == rec.NewClaim.UID && o.Token != ""
			})
			if i < 0 {
				unmet = "no offer for " + rec.Member + " names its new claim " + rec.NewClaim.UID
				break
			}
			offer := cluster.Status.MemberRebuildOffers[i]
			h.redactor.Add(offer.Token)
			at := time.Now().UTC()
			rec.OfferedAt, rec.Unmet = &at, ""
			rec.Offer = &s10Offer{Member: offer.Member, ClaimUID: offer.ClaimUID, VolumeName: offer.VolumeName,
				Source: offer.Source, FormerPrimary: offer.FormerPrimary}
			if offer.Source != rec.Master {
				return "", s10Wrong{fmt.Errorf("the offer for %s names the source %s, not the master %s",
					rec.Member, offer.Source, rec.Master)}
			}
			return offer.Token, nil
		}
		rec.Unmet = unmet
		if time.Now().After(deadline) || ctx.Err() != nil {
			return "", fmt.Errorf("no rebuild was offered for %s within %s: %s", rec.Member, s10OfferBound, unmet)
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Poll):
		}
	}
}

// jsonPatchTest is the JSON patch operation that makes a patch apply only
// while a value is as the run read it.
const jsonPatchTest = "test"

// removeS10ReleaseToken removes the release annotation the run set when it
// still holds the run's token, as the Operator removes a consumed one, so that
// a release the Operator did not make, or a run that ended first, leaves no
// token it could accept later. It is a JSON patch that tests the run's
// cluster UID and the value before it removes the annotation, made within
// s07RemoveTokenBound also after ctx ended.
func (h *haRun) removeS10ReleaseToken(ctx context.Context, rec *s10Record) {
	if rec.releaseToken == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s07RemoveTokenBound)
	defer cancel()
	cluster, err := h.s07OwnCluster(ctx)
	if err == nil && cluster.Annotations[releaseWriteQuarantineAnnotation] != rec.releaseToken {
		return
	}
	if err == nil {
		annotation := "/metadata/annotations/" + strings.ReplaceAll(strings.ReplaceAll(
			releaseWriteQuarantineAnnotation, "~", "~0"), "/", "~1")
		type operation struct {
			Op    string `json:"op"`
			Path  string `json:"path"`
			Value string `json:"value,omitempty"`
		}
		var patch []byte
		patch, err = json.Marshal([]operation{{jsonPatchTest, "/metadata/uid", string(cluster.UID)},
			{jsonPatchTest, annotation, rec.releaseToken}, {"remove", annotation, ""}})
		if err == nil {
			target := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name,
				Namespace: cluster.Namespace}}
			err = h.Client.Patch(ctx, target, client.RawPatch(types.JSONPatchType, patch))
		}
	}
	if err != nil {
		rec.ReleaseTokenRemoveError = err.Error()
		return
	}
	now := time.Now().UTC()
	rec.ReleaseTokenRemovedAt = &now
}

// s10Released reports whether the cluster records the manual release of the
// quarantine generation.
func s10Released(cluster *databasev1alpha1.CubridCluster, generation string) bool {
	rel := cluster.Status.LastWriteQuarantineRelease
	return rel != nil && rel.GenerationID == generation && rel.Method == databasev1alpha1.WriteQuarantineReleasedManually
}

// s10BrokersBack reports whether the read-write Brokers are scaled back and
// available after the release, and what does not hold otherwise.
func (h *haRun) s10BrokersBack(ctx context.Context) (bool, string) {
	deployment := &appsv1.Deployment{}
	name := haClusterName + "-broker-rw"
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: name}, deployment); err != nil {
		return false, "read the Deployment " + name + ": " + err.Error()
	}
	s := deployment.Status
	if spec := deployment.Spec.Replicas; (spec != nil && *spec == 0) || s.Replicas == 0 ||
		s.UpdatedReplicas != s.Replicas || s.AvailableReplicas != s.Replicas {
		return false, fmt.Sprintf("the read-write Brokers are not back after the release: replicas %d, updated %d, "+
			"available %d", s.Replicas, s.UpdatedReplicas, s.AvailableReplicas)
	}
	return true, ""
}

// setS10Annotation sets an annotation on the run's own cluster: the namespace
// guard and the cluster's UID are checked first, and the patch names the
// UID, so the API server refuses it for another cluster of the same name.
func (h *haRun) setS10Annotation(ctx context.Context, key, value string) error {
	cluster, err := h.s07OwnCluster(ctx)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": string(cluster.UID),
		"annotations": map[string]string{key: value}}})
	if err != nil {
		return err
	}
	target := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: cluster.Name,
		Namespace: cluster.Namespace}}
	return h.Client.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch))
}

// s10WaitRebuild waits for the rebuild the token requested to end, and
// returns it once it was seen. It must end Completed and name the target, the
// master as source and the new claim; a rebuild that names the master is
// added to the never-events by the caller through the returned rebuild.
func (h *haRun) s10WaitRebuild(ctx context.Context, rec *s10Record, token string) (*databasev1alpha1.MemberRebuild,
	error) {
	bound := h.cfg.s10RebuildWait()
	deadline := time.Now().Add(bound)
	for {
		cluster, err := h.s07OwnCluster(ctx)
		unmet := "the rebuild has not ended"
		if err != nil {
			unmet = err.Error()
		} else if last := cluster.Status.LastMemberRebuild; last != nil && last.Token == token {
			rec.Rebuild = &s10Rebuild{Member: last.Member, Source: last.Source, ClaimUID: last.ClaimUID,
				OperationID: last.OperationID, State: string(last.State), Message: last.Message,
				AcceptedTime: last.AcceptedTime.UTC()}
			if last.EndTime != nil {
				end := last.EndTime.UTC()
				rec.Rebuild.EndTime = &end
			}
			rec.Unmet = ""
			switch {
			case last.Member == rec.Master:
				return last, fmt.Errorf("the rebuild named the master %s, whose volume must never be a rebuild "+
					"target", rec.Master)
			case last.State != databasev1alpha1.MemberRebuildCompleted:
				return last, fmt.Errorf("the rebuild of %s ended %s: %s", rec.Member, last.State, last.Message)
			case last.Member != rec.Member || last.Source != rec.Master || last.ClaimUID != rec.NewClaim.UID:
				return last, fmt.Errorf("the rebuild names member %s, source %s and claim %s, not %s, %s and %s",
					last.Member, last.Source, last.ClaimUID, rec.Member, rec.Master, rec.NewClaim.UID)
			}
			return last, nil
		}
		rec.Unmet = unmet
		if time.Now().After(deadline) || ctx.Err() != nil {
			// With rebuild_limit set, a wait that ran past the limit with no
			// end seen is a rebuild over the limit, measured as a lower
			// bound; only a run that ended, or an unset limit, leaves it
			// unobserved.
			if waited := time.Since(*rec.RequestedAt); ctx.Err() == nil && h.cfg.RebuildLimit > 0 &&
				waited > h.cfg.RebuildLimit {
				return nil, fmt.Errorf("rebuild_limit exceeded: no end of the rebuild of %s seen within %s of the "+
					"request, over rebuild_limit %s: %s", rec.Member, waited.Round(time.Millisecond),
					h.cfg.RebuildLimit, unmet)
			}
			return nil, s10Unobserved{fmt.Errorf("the rebuild of %s was not seen to end within %s: %s", rec.Member,
				bound, unmet)}
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Poll):
		}
	}
}

// s10WaitSlave waits for the rebuilt member to answer as a slave of the same
// master again, with its ownership marker released, Degraded not True, its
// new claim recorded as holding the database, its lost claim kept in
// status.formerDataClaims and the consumed request removed. A master other
// than the starting one ends the wait at once; moved reports it.
func (h *haRun) s10WaitSlave(ctx context.Context, rec *s10Record) (moved bool, _ error) {
	deadline := time.Now().Add(s10SlaveBound)
	old := rec.ClaimsBefore[rec.Member]
	for {
		unmet := ""
		roles, rolesUnmet, err := h.roles(ctx)
		cluster, clusterErr := h.s07OwnCluster(ctx)
		switch {
		case err != nil || rolesUnmet != "":
			unmet = "read the roles: " + errors.Join(err, errors.New(rolesUnmet)).Error()
		case roles.Master != rec.Master:
			return true, fmt.Errorf("the master moved from %s to %s during the rebuild", rec.Master, roles.Master)
		case !slices.Contains(roles.Slaves, rec.Member):
			unmet = rec.Member + " does not answer as a slave"
		case clusterErr != nil:
			unmet = clusterErr.Error()
		default:
			unmet = s10StatusUnmet(cluster, rec.Member, rec.NewClaim.UID, old.UID)
		}
		if unmet == "" {
			at := time.Now().UTC()
			rec.SlaveAgainAt, rec.Unmet = &at, ""
			return false, nil
		}
		rec.Unmet = unmet
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false, s10Unobserved{fmt.Errorf("%s was not seen to return as a slave with its new claim within "+
				"%s: %s", rec.Member, s10SlaveBound, unmet)}
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.cfg.Poll):
		}
	}
}

// s10StatusUnmet says what of the rebuilt member's return the cluster's
// status does not show yet, or "".
func s10StatusUnmet(cluster *databasev1alpha1.CubridCluster, member, newClaim, oldClaim string) string {
	i := slices.IndexFunc(cluster.Status.Instances, func(s databasev1alpha1.InstanceStatus) bool {
		return s.Name == member
	})
	switch {
	case i < 0:
		return "status.instances has no " + member
	// The ownership marker of the restore is cleared only by the release
	// that ends a caught-up rebuild.
	case cluster.Status.Instances[i].UnfinishedDatabase != nil:
		return "status.instances shows the ownership marker on " + member
	case meta.IsStatusConditionTrue(cluster.Status.Conditions, "Degraded"):
		return "Degraded is True"
	case !slices.ContainsFunc(cluster.Status.MemberDataClaims, func(d databasev1alpha1.MemberDataClaim) bool {
		return d.Name == member && d.ClaimUID == newClaim
	}):
		return "status.memberDataClaims does not record the new claim " + newClaim + " of " + member
	case !slices.ContainsFunc(cluster.Status.FormerDataClaims, func(d databasev1alpha1.MemberDataClaim) bool {
		return d.Name == member && d.ClaimUID == oldClaim
	}):
		return "status.formerDataClaims does not keep the lost claim " + oldClaim + " of " + member
	case cluster.Annotations[rebuildMemberAnnotation] != "":
		return "the consumed rebuild request is not removed"
	}
	return ""
}

// insertsWhileRebuilt counts, by outcome, the client's inserts answered while
// the rebuild ran, from its acceptance until its end as the Operator recorded
// them. The API server keeps both to the second, so only answers after the
// second that follows the acceptance and before the recorded end count.
func insertsWhileRebuilt(events []workload.Event, done *databasev1alpha1.MemberRebuild) map[string]int {
	counted := map[string]int{}
	if done == nil || done.EndTime == nil {
		return counted
	}
	from, to := done.AcceptedTime.Add(time.Second), done.EndTime.Time
	for _, e := range events {
		if e.Op == workload.OpInsert && e.Event != workload.Attempted && e.T.After(from) && e.T.Before(to) {
			counted[e.Event]++
		}
	}
	return counted
}

// s10FailCount reads the fail count of the rebuilt member's applier for the
// master's log.
func (h *haRun) s10FailCount(ctx context.Context, member, master string) (int, error) {
	out, err := h.member(ctx, member, "bash", "-c", applyinfoScript, "applyinfo", master)
	if err != nil {
		return 0, err
	}
	m := failCountLine.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no fail count in the output of applyinfo on %s: %q", member, out)
	}
	return strconv.Atoi(m[1])
}

// s10OtherClaims reads again the claims of the members that were not
// rebuilt and names each whose UID changed: a rebuild that replaced or
// targeted one of them would change it. What this observes is the claim's
// identity; a restore into an unchanged claim would not show here, and the
// data check is what shows those members' rows intact.
func (h *haRun) s10OtherClaims(ctx context.Context, rec *s10Record) ([]string, error) {
	rec.ClaimsAfter = map[string]s10Claim{}
	var replaced []string
	for _, m := range h.members {
		if m == rec.Member {
			continue
		}
		claim, err := h.s10ReadClaim(ctx, m)
		if err != nil {
			return replaced, err
		}
		rec.ClaimsAfter[m] = claim
		if before := rec.ClaimsBefore[m]; claim.UID != before.UID {
			replaced = append(replaced, fmt.Sprintf("the claim %s of a member that was not rebuilt was replaced "+
				"(%s, then %q)", claim.Name, before.UID, claim.UID))
		}
	}
	return replaced, nil
}
