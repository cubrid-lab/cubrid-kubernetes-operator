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
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
)

const (
	s12WrongTargetLane                = "lab-installed-ha-s12-wrong-target"
	s12WrongTargetVariant             = "wrong-target"
	s12WrongTargetRecordFile          = "S12/wrong-target/record.json"
	s12WrongTargetSourceDataCheckFile = "S12/wrong-target/source-data-check.json"
	// s12WrongTargetClusterName is both the cluster that leaves its volumes
	// and the cluster restored over them, in the run namespace.
	// s12WrongTargetSecretName is the first one's copy of the seed Secret and
	// s12WrongTargetRecoverySecretName the second one's.
	s12WrongTargetClusterName        = "ha-wt"
	s12WrongTargetSecretName         = "object-storage-wt"
	s12WrongTargetRecoverySecretName = "object-storage-wt-recovery"
	// s12WrongTargetMarker names the marker row written through the first
	// cluster; its value is the run ID.
	s12WrongTargetMarker = "s12wt"
	// restoreTargetDir is the database root the Operator asks a member to
	// restore into (restoreTargetRoot in internal/controller/bootstrap.go).
	restoreTargetDir = "/var/lib/cubrid/databases"
)

// s12Volume is a data PVC of the cluster as the run read it.
type s12Volume struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	VolumeName string `json:"volumeName,omitempty"`
	Phase      string `json:"phase,omitempty"`
}

// s12Formed is the first ha-wt: formed, written to, and deleted with its
// volumes retained.
type s12Formed struct {
	ClusterUID   string          `json:"clusterUID,omitempty"`
	CreatedAt    *time.Time      `json:"createdAt,omitempty"`
	FormedWithin string          `json:"formedWithin,omitempty"`
	Roles        *startingState  `json:"roles,omitempty"`
	Members      []imageIdentity `json:"members"`
	// Marker is the marker written through its read-write Service, and
	// S11MarkerOn the members that held S11's marker before the deletion.
	Marker      *s12NormalWrite `json:"marker,omitempty"`
	S11MarkerOn []string        `json:"s11MarkerOn"`
	Volumes     []s12Volume     `json:"volumes"`
	DeletedAt   *time.Time      `json:"deletedAt,omitempty"`
	// GoneWithin runs from the deletion to the reading that found the
	// cluster, its StatefulSet, Pods and Broker Deployments gone;
	// VolumesRetained is set once its volumes were read again after that.
	GoneWithin      string `json:"goneWithin,omitempty"`
	VolumesRetained bool   `json:"volumesRetained"`
}

// s12ExistingData is the marker table of the members restored over the
// volumes, read after the refusal.
type s12ExistingData struct {
	Checked     bool     `json:"checked"`
	MarkerOn    []string `json:"markerOn"`
	MissingOn   []string `json:"missingOn"`
	S11MarkerOn []string `json:"s11MarkerOn"`
}

// s12WrongTargetRecord is the evidence of S12 wrong-target besides the
// source's data check.
type s12WrongTargetRecord struct {
	Cluster           string     `json:"cluster"`
	SeedPrefix        string     `json:"seedPrefix"`
	SecretRef         string     `json:"credentialsSecretRef"`
	RecoverySecretRef string     `json:"recoveryCredentialsSecretRef"`
	Formed            *s12Formed `json:"formed,omitempty"`
	ArtifactURI       string     `json:"artifactUri,omitempty"`
	RecoveryUID       string     `json:"recoveryClusterUID,omitempty"`
	RecoveryCreatedAt *time.Time `json:"recoveryCreatedAt,omitempty"`
	Bootstrap         []s12Phase `json:"bootstrap"`
	// RefusedWithin runs to the reading that saw the refusal, whose
	// BootstrapReady reason and message Refusal holds.
	RefusedWithin string `json:"refusedWithin,omitempty"`
	Refusal       string `json:"refusal,omitempty"`
	// VolumesAfterRefusal are the data PVCs read after the refusal;
	// SameVolumes is set when they are the formed cluster's, by UID.
	VolumesAfterRefusal []s12Volume      `json:"volumesAfterRefusal"`
	SameVolumes         bool             `json:"sameVolumes"`
	ExistingData        *s12ExistingData `json:"existingData,omitempty"`
	SecretsReferenced   []string         `json:"secretsReferenced"`
	Source              *s12Source       `json:"source,omitempty"`
	Credentials         *s12Credentials  `json:"credentials,omitempty"`
	Notes               []string         `json:"notes,omitempty"`
	Residue             string           `json:"residue"`
}

// dataVolumePrefix starts the name of a member's data PVC: the StatefulSet's
// volume claim template is named data (dataPVCTemplate in
// internal/controller/cubridcluster_controller.go).
const dataVolumePrefix = "data-"

// wrongTargetGuards are the failure reasons the Instance Manager gives a
// restore into a target that already holds the database (the wrong-target
// guard of Restore in internal/instancemanager/restore.go), as runRestore
// records them (internal/instancemanager/server.go). The Operator passes
// them on in the BootstrapReady message (restoreAttemptFailed).
func wrongTargetGuards() []string {
	return []string{
		fmt.Sprintf("restore failed: target %s already holds database %q; refusing to overwrite (ADR-0008)",
			restoreTargetDir, databaseName),
		fmt.Sprintf("restore failed: database %q is already registered in %s; refusing to overwrite (ADR-0008)",
			databaseName, filepath.Join(restoreTargetDir, "databases.txt")),
	}
}

// runS12WrongTarget runs S12 wrong-target after S12 exact passed: a new
// cluster ha-wt is formed, takes a marker written through it on every member,
// and is deleted with its PVCs retained. A cluster of the same name restored
// from S11's artifact over those PVCs must have its restore refused within
// RestoreLimit for a target that already holds the database, with the
// bootstrap Failed, never Ready and never Complete; the PVCs must be the
// same, and its members must still hold the marker and not S11's. The source
// must be unchanged. A starting state that is not reached, a run that ends,
// or a check that cannot be made, before S12 wrong-target is judged blocks
// it; what was seen to break stays a failure.
func (h *haRun) runS12WrongTarget(ctx context.Context) (result evidence.Scenario) {
	result = evidence.Scenario{ID: s12ID, Variant: s12WrongTargetVariant, Result: evidence.Fail, NeverEvents: []string{}}
	rec := s12WrongTargetRecord{Cluster: s12WrongTargetClusterName, SeedPrefix: h.rec.Seed.Prefix + "/wt-seed",
		SecretRef: s12WrongTargetSecretName, RecoverySecretRef: s12WrongTargetRecoverySecretName,
		Bootstrap: []s12Phase{}, VolumesAfterRefusal: []s12Volume{}, SecretsReferenced: []string{},
		Credentials: &s12Credentials{FoundIn: []string{}},
		Notes: []string{"each ha-wt uses a copy of the seed Secret of its own, " + s12WrongTargetSecretName +
			" and " + s12WrongTargetRecoverySecretName + "; the source uses " + seedSecretName,
			"the existing data was read through CUBRID on the members of the refused cluster: the entrypoint " +
				"starts a database that is registered on the volume, also in a recovery bootstrap",
			"the Operator's logs and Events were not checked",
			"after the refusal, the cluster was read once more, at the end"},
		Residue: "the refused cluster, the PVCs of ha-wt and its Secrets go with the namespace; the seed backups " +
			"under its seed prefix are not removed by this run"}
	defer func() {
		// A pass without its evidence is a failure; a blocked or failed S12
		// wrong-target keeps its result, with the evidence that could not be
		// written.
		if err := h.writeJSON(s12WrongTargetRecordFile, rec); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S12 wrong-target evidence: "+err.Error(), "; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S12/"+s12WrongTargetVariant+"/")
		})
	}()
	result.Limits = map[string]string{restoreLimitName: h.cfg.RestoreLimit.String()}
	result.Measurements = map[string]any{"refusedWithin": evidence.Unknown, "attempts": evidence.Unknown}
	err := h.wrongTargetSteps(ctx, &result, &rec)
	// A credential found in any status read is a failure, whichever step
	// ended S12 wrong-target.
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	never := strings.Join(result.NeverEvents, "; ")
	switch {
	case errors.As(err, new(restoreVerdict)) || errors.As(err, new(dataViolation)):
		result.Result, result.Reason = evidence.Fail, err.Error()
		switch {
		case slices.Contains(result.NeverEvents, err.Error()):
			result.Reason = never
		case never != "":
			result.Reason = never + "; " + err.Error()
		}
	case err != nil && never != "":
		// What was not checked keeps a confirmed failure a failure.
		result.Result, result.Reason = evidence.Fail, never+"; "+err.Error()
	case err != nil && ctx.Err() != nil:
		result.Result, result.Reason = evidence.Blocked, "the run ended before S12 wrong-target was judged: "+
			err.Error()
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, err.Error()
	case never != "":
		result.Result, result.Reason = evidence.Fail, never
	default:
		result.Result = evidence.Pass
		result.Reason = "the cluster created from S11's artifact over the retained volumes of " +
			s12WrongTargetClusterName + ", which held its database, ended its bootstrap Failed within " +
			"restore_limit because the target already holds the database, on the same volumes, and was never " +
			"Ready or Complete; its members held the marker written before and not S11's; the source was " +
			"unchanged; no credential was in its status and no Secret of the source in its Pods; the " +
			"Operator's logs were not checked"
	}
	return result
}

// wrongTargetSteps runs the steps of S12 wrong-target in order and returns
// the first error.
func (h *haRun) wrongTargetSteps(ctx context.Context, result *evidence.Scenario, rec *s12WrongTargetRecord) error {
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S12 wrong-target could not tell whether a credential is in the cluster's status", err)
	}
	s11 := h.s11Rec
	if s11 == nil || s11.KnownPoint == nil || s11.Artifact == nil {
		return errors.New("S11 recorded no known point or artifact to restore")
	}
	rec.ArtifactURI = s11.Artifact.URI
	rec.Formed = &s12Formed{Members: []imageIdentity{}, S11MarkerOn: []string{}, Volumes: []s12Volume{}}
	if err := h.heldVolumes(ctx, s11.KnownPoint, rec); err != nil {
		return err
	}
	cluster, err := h.createRestored(ctx, s12WrongTargetClusterName, s12WrongTargetRecoverySecretName,
		rec.SeedPrefix, rec.ArtifactURI)
	if err != nil {
		return err
	}
	created := time.Now()
	createdAt := created.UTC()
	rec.RecoveryUID, rec.RecoveryCreatedAt = string(cluster.UID), &createdAt
	refusedAt, err := h.waitWrongTargetRefused(ctx, cluster, rec, result)
	if err != nil {
		return err
	}
	rec.RefusedWithin = refusedAt.Sub(created).Round(time.Millisecond).String()
	result.Measurements["refusedWithin"] = rec.RefusedWithin
	if err := h.checkSameVolumes(ctx, rec); err != nil {
		return err
	}
	if err := h.checkStatefulSetSecrets(ctx, s12WrongTargetClusterName, "the cluster restored over the volumes",
		&rec.SecretsReferenced, result); err != nil {
		return err
	}
	if err := h.checkExistingData(ctx, s11.KnownPoint, rec, result); err != nil {
		return err
	}
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	if err := h.checkSourceS12(ctx, rec.Source, s12WrongTargetSourceDataCheckFile); err != nil {
		return err
	}
	final := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(cluster), final); err != nil {
		return fmt.Errorf("read the cluster restored over the volumes at the end: %w", err)
	}
	refused, err := h.wrongTargetReading(final, "status at the end", rec, result)
	if err != nil {
		return err
	}
	if !refused {
		return restoreVerdict{fmt.Errorf("at the end, the cluster restored over the volumes no longer held the "+
			"refusal: %s", bootstrapReason(final))}
	}
	return nil
}

// heldVolumes reaches the starting state of S12 wrong-target, volumes that
// hold a database: a new cluster ha-wt with the source's spec and no
// recovery is formed, takes a marker written through its read-write Service,
// read back on every member, and holds no S11 marker. Its data PVCs are
// recorded, and it is deleted until it, its StatefulSet, its Pods and its
// Broker Deployments are gone and its PVCs are still there, with the same
// UIDs (spec.storage.retentionPolicy Retain, the default). Any error blocks
// S12 wrong-target.
func (h *haRun) heldVolumes(ctx context.Context, known *s11KnownPoint, rec *s12WrongTargetRecord) error {
	formed := rec.Formed
	cluster, err := h.createWrongTarget(ctx, rec.SeedPrefix)
	if err != nil {
		return err
	}
	created := time.Now()
	createdAt := created.UTC()
	formed.ClusterUID, formed.CreatedAt = string(cluster.UID), &createdAt
	roles, err := h.waitFormed(ctx, cluster, rec)
	if err != nil {
		return err
	}
	formed.FormedWithin = time.Since(created).Round(time.Millisecond).String()
	formed.Roles = roles
	if err := h.markWrongTarget(ctx, roles, known, formed); err != nil {
		return err
	}
	volumes, err := h.readVolumes(ctx)
	if err != nil {
		return fmt.Errorf("read the volumes of %s: %w", cluster.Name, err)
	}
	formed.Volumes = volumes
	if err := h.deleteFormed(ctx, cluster, formed); err != nil {
		return err
	}
	after, err := h.readVolumes(ctx)
	if err == nil {
		err = volumeChange(formed.Volumes, after)
	}
	if err != nil {
		return fmt.Errorf("the volumes of the deleted %s were not retained: %w", cluster.Name, err)
	}
	formed.VolumesRetained = true
	return nil
}

// createWrongTarget creates the first ha-wt in the run namespace: the
// source's spec, seeding its members under seedPrefix with a copy of the
// seed Secret of its own, which no Pod of the source names.
func (h *haRun) createWrongTarget(ctx context.Context, seedPrefix string) (*databasev1alpha1.CubridCluster, error) {
	if err := h.guard(ctx); err != nil {
		return nil, err
	}
	name := s12WrongTargetClusterName
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s12WrongTargetSecretName, Namespace: h.cfg.Namespace},
		Type: corev1.SecretTypeOpaque, Data: map[string][]byte{seedAccessKey: []byte(h.accessKey),
			seedSecretKey: []byte(h.secretKey)}}
	if err := h.Client.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("copy the seed Secret for %s: %w", name, err)
	}
	cluster := &databasev1alpha1.CubridCluster{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: h.cfg.Namespace},
		Spec: h.clusterSpec(seedPrefix, s12WrongTargetSecretName)}
	if err := h.Client.Create(ctx, cluster); err != nil {
		return nil, fmt.Errorf("create the cluster %s: %w", name, err)
	}
	if cluster.UID == "" {
		return nil, fmt.Errorf("created cluster %s has no observed UID", name)
	}
	return cluster, nil
}

// waitFormed reads the first ha-wt until it is Ready, its members Ready and
// owned, its Brokers available, one master and two slaves by CUBRID and
// status.currentPrimary naming the master, as S12 exact waits for its
// restored cluster. Every status read is searched for a credential. It
// returns the roles.
func (h *haRun) waitFormed(ctx context.Context, created *databasev1alpha1.CubridCluster, rec *s12WrongTargetRecord) (
	*startingState, error) {
	var waiting string
	var roles *startingState
	err := pollUntil(ctx, h.cfg.Poll, func() (bool, error) {
		if err := h.guard(ctx); err != nil {
			return false, err
		}
		cluster := &databasev1alpha1.CubridCluster{}
		if err := h.Client.Get(ctx, client.ObjectKeyFromObject(created), cluster); err != nil {
			if retryable(err) {
				waiting = "the cluster could not be read: " + err.Error()
				return false, nil
			}
			return false, err
		}
		if cluster.UID != created.UID {
			return false, fmt.Errorf("the cluster %s was replaced", created.Name)
		}
		rec.Credentials.StatusesChecked++
		if err := h.lookForCredentials(&rec.Credentials.FoundIn, "the first "+created.Name+"'s status read",
			cluster.Status); err != nil {
			return false, err
		}
		if unmet, err := clusterReady(cluster); unmet != "" || err != nil {
			waiting = unmet
			return false, err
		}
		members, unmet, err := h.memberIdentities(ctx, cluster)
		if unmet != "" || err != nil {
			waiting = unmet
			return false, err
		}
		if unmet, err := h.brokersAvailable(ctx, cluster); unmet != "" || err != nil {
			waiting = unmet
			return false, err
		}
		state, unmet, err := h.rolesOf(ctx, clusterMembers(cluster.Name))
		if unmet != "" || err != nil {
			waiting = unmet
			return false, err
		}
		state.CurrentPrimary = cluster.Status.CurrentPrimary
		if state.CurrentPrimary != state.Master {
			waiting = fmt.Sprintf("status.currentPrimary is %q and CUBRID reports %s as master",
				state.CurrentPrimary, state.Master)
			return false, nil
		}
		rec.Formed.Members, roles = members, state
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("the first %s was not formed: %s: %w", created.Name, cmp.Or(waiting,
			"no reading was taken"), err)
	}
	return roles, nil
}

// markWrongTarget creates the workload's tables through the first ha-wt's
// read-write Service, writes a marker through it and reads the marker back
// on every member. None of them may hold S11's marker: the run could not
// tell the backup's data from ha-wt's.
func (h *haRun) markWrongTarget(ctx context.Context, roles *startingState, known *s11KnownPoint,
	formed *s12Formed) error {
	write := &s12NormalWrite{Marker: s12WrongTargetMarker, ReadBackOn: []string{}, MissingOn: []string{}}
	formed.Marker = write
	service := s12WrongTargetClusterName + "-rw"
	url := h.jdbcURL(s12WrongTargetClusterName, "rw", 33000)
	if out, err := h.clientRun(ctx, fmt.Sprintf(`JDBC_URL="%s" workload init 2>&1`, url)); err != nil {
		return fmt.Errorf("create the workload tables through %s: %w: %s", service, err, strings.TrimSpace(out))
	}
	out, err := h.clientRun(ctx, fmt.Sprintf(`JDBC_URL="%s" workload marker %s %s 2>&1`, url, s12WrongTargetMarker,
		h.rec.RunID))
	if err != nil {
		return fmt.Errorf("write marker %s through %s: %w: %s", s12WrongTargetMarker, service, err,
			strings.TrimSpace(out))
	}
	if !strings.Contains(out, "marker "+s12WrongTargetMarker+" written") {
		return fmt.Errorf("the marker %s written through %s was not acknowledged: %s", s12WrongTargetMarker, service,
			strings.TrimSpace(out))
	}
	write.Acknowledged = true
	members := clusterMembers(s12WrongTargetClusterName)
	missing, _, err := h.waitMarker(ctx, members, roles.Master, s12WrongTargetMarker, h.rec.RunID)
	if err != nil {
		return err
	}
	write.MissingOn = append(write.MissingOn, missing...)
	write.ReadBackOn = slices.DeleteFunc(slices.Clone(members), func(m string) bool { return slices.Contains(missing, m) })
	if len(missing) > 0 {
		return fmt.Errorf("the marker %s written through %s is not on member(s) %s", s12WrongTargetMarker, service,
			strings.Join(missing, ", "))
	}
	for _, pod := range members {
		table, err := h.markerTable(ctx, pod)
		if err != nil {
			return err
		}
		if _, ok := table[known.Marker]; ok {
			formed.S11MarkerOn = append(formed.S11MarkerOn, pod)
		}
	}
	if len(formed.S11MarkerOn) > 0 {
		return fmt.Errorf("S11's marker %s is on %s before any restore: the backup's data could not be told from "+
			"%s's", known.Marker, strings.Join(formed.S11MarkerOn, ", "), s12WrongTargetClusterName)
	}
	return nil
}

// readVolumes reads the data PVCs of ha-wt's members. One that cannot be
// read or is being deleted is an error.
func (h *haRun) readVolumes(ctx context.Context) ([]s12Volume, error) {
	volumes := []s12Volume{}
	for _, member := range clusterMembers(s12WrongTargetClusterName) {
		pvc := &corev1.PersistentVolumeClaim{}
		key := client.ObjectKey{Namespace: h.cfg.Namespace, Name: dataVolumePrefix + member}
		if err := h.Client.Get(ctx, key, pvc); err != nil {
			return volumes, fmt.Errorf("read PVC %s: %w", key.Name, err)
		}
		volumes = append(volumes, s12Volume{Name: pvc.Name, UID: string(pvc.UID), VolumeName: pvc.Spec.VolumeName,
			Phase: string(pvc.Status.Phase)})
		if pvc.DeletionTimestamp != nil {
			return volumes, fmt.Errorf("PVC %s is being deleted", key.Name)
		}
	}
	return volumes, nil
}

// volumeChange names each PVC of before that after does not hold with the
// same UID, or nil when there is none.
func volumeChange(before, after []s12Volume) error {
	var changes []string
	for _, b := range before {
		i := slices.IndexFunc(after, func(a s12Volume) bool { return a.Name == b.Name })
		switch {
		case i < 0:
			changes = append(changes, "PVC "+b.Name+" is gone")
		case after[i].UID != b.UID:
			changes = append(changes, fmt.Sprintf("PVC %s has UID %s, not %s", b.Name, after[i].UID, b.UID))
		}
	}
	if len(changes) > 0 {
		return errors.New(strings.Join(changes, "; "))
	}
	return nil
}

// deleteFormed deletes the first ha-wt with a UID precondition and waits
// until it, its StatefulSet, its member Pods and its Broker Deployments are
// gone. The garbage collector removes what the cluster owns; a PVC is not
// owned by it under Retain.
func (h *haRun) deleteFormed(ctx context.Context, cluster *databasev1alpha1.CubridCluster, formed *s12Formed) error {
	if err := h.guard(ctx); err != nil {
		return err
	}
	uid := cluster.UID
	if err := h.Client.Delete(ctx, cluster, client.Preconditions{UID: &uid},
		client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		return fmt.Errorf("delete the cluster %s: %w", cluster.Name, err)
	}
	deleted := time.Now()
	deletedAt := deleted.UTC()
	formed.DeletedAt = &deletedAt
	var left string
	err := pollUntil(ctx, h.cfg.Poll, func() (bool, error) {
		var err error
		left, err = h.wrongTargetLeft(ctx, cluster.Name)
		return left == "" && err == nil, err
	})
	if err != nil {
		return fmt.Errorf("the deleted cluster %s did not go: %s: %w", cluster.Name, cmp.Or(left,
			"no reading was taken"), err)
	}
	formed.GoneWithin = time.Since(deleted).Round(time.Millisecond).String()
	return nil
}

// wrongTargetLeft names the first of the cluster, its StatefulSet, its
// member Pods and its Broker Deployments that is still there, or what could
// not be read yet.
func (h *haRun) wrongTargetLeft(ctx context.Context, name string) (string, error) {
	if err := h.guard(ctx); err != nil {
		return "", err
	}
	type object struct {
		kind, name string
		obj        client.Object
	}
	objects := []object{{"CubridCluster", name, &databasev1alpha1.CubridCluster{}},
		{"StatefulSet", name, &appsv1.StatefulSet{}}}
	for _, member := range clusterMembers(name) {
		objects = append(objects, object{"Pod", member, &corev1.Pod{}})
	}
	for _, mode := range []string{"rw", "ro"} {
		objects = append(objects, object{"Deployment", name + "-broker-" + mode, &appsv1.Deployment{}})
	}
	for _, o := range objects {
		err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: o.name}, o.obj)
		switch {
		case apierrors.IsNotFound(err):
			continue
		case err != nil && retryable(err):
			return fmt.Sprintf("%s %s could not be read: %v", o.kind, o.name, err), nil
		case err != nil:
			return "", err
		}
		return o.kind + " " + o.name + " is still there", nil
	}
	return "", nil
}

// waitWrongTargetRefused reads the cluster restored over the volumes until
// its restore is refused for a target that already holds the database, for
// at most RestoreLimit. Each change of the bootstrap is recorded, and every
// status read is searched for a credential.
func (h *haRun) waitWrongTargetRefused(ctx context.Context, created *databasev1alpha1.CubridCluster,
	rec *s12WrongTargetRecord, result *evidence.Scenario) (time.Time, error) {
	wctx, cancel := context.WithTimeout(ctx, h.cfg.RestoreLimit)
	defer cancel()
	// readErr is the last reading's error, nil once a reading succeeded.
	var readErr error
	var refusedAt time.Time
	err := pollUntil(wctx, h.cfg.Poll, func() (bool, error) {
		cluster := &databasev1alpha1.CubridCluster{}
		if err := h.Client.Get(wctx, client.ObjectKeyFromObject(created), cluster); err != nil {
			if wctx.Err() != nil {
				return false, nil
			}
			if retryable(err) {
				readErr = err
				return false, nil
			}
			return false, err
		}
		readErr = nil
		if cluster.UID != created.UID {
			return false, restoreVerdict{errors.New("the cluster restored over the volumes was replaced")}
		}
		refused, err := h.wrongTargetReading(cluster, "status read", rec, result)
		if err != nil || !refused {
			return false, err
		}
		refusedAt = time.Now()
		return true, nil
	})
	switch {
	case err == nil:
		return refusedAt, nil
	case ctx.Err() == nil && wctx.Err() != nil && readErr != nil:
		// The state at the limit is not known: the run could not judge.
		return time.Time{}, fmt.Errorf("the cluster restored over the volumes could not be read before "+
			"restore_limit %s passed: %w", h.cfg.RestoreLimit, readErr)
	case ctx.Err() == nil && wctx.Err() != nil:
		phase, reason := noBootstrapPhase, noBootstrapCondition
		if n := len(rec.Bootstrap); n > 0 {
			last := rec.Bootstrap[n-1]
			phase = last.Phase
			if c := meta.FindStatusCondition(last.Conditions, bootstrapReadyCondition); c != nil {
				reason = c.Reason + ": " + c.Message
			}
		}
		return time.Time{}, restoreVerdict{fmt.Errorf("the restore over the volumes that hold a database was not "+
			"refused within restore_limit %s; its last bootstrap phase was %q, BootstrapReady %s",
			h.cfg.RestoreLimit, phase, reason)}
	}
	return time.Time{}, err
}

// wrongTargetReading records one reading of the cluster restored over the
// volumes and reports whether it shows the refusal: bootstrap phase Failed
// with BootstrapReady reason RestoreFailed and a message that holds one of
// wrongTargetGuards. Ready True and phase Complete must never happen; Failed
// for another reason fails S12 wrong-target.
func (h *haRun) wrongTargetReading(cluster *databasev1alpha1.CubridCluster, where string, rec *s12WrongTargetRecord,
	result *evidence.Scenario) (bool, error) {
	phase, attempts := "", int32(0)
	if boot := cluster.Status.Bootstrap; boot != nil {
		phase, attempts = string(boot.Phase), boot.Attempts
	}
	recordBootstrap(cluster, &rec.Bootstrap)
	rec.Credentials.StatusesChecked++
	if err := h.lookForCredentials(&rec.Credentials.FoundIn, "the refused cluster's "+where+" in bootstrap "+
		"phase "+cmp.Or(phase, noBootstrapPhase), cluster.Status); err != nil {
		return false, err
	}
	var events []string
	if ready := meta.FindStatusCondition(cluster.Status.Conditions, startingConditions[0]); ready != nil &&
		ready.Status == metav1.ConditionTrue {
		events = append(events, fmt.Sprintf("the cluster restored over the volumes that hold a database reported "+
			"Ready in bootstrap phase %q", phase))
	}
	if phase == string(databasev1alpha1.BootstrapComplete) {
		events = append(events, "the restore over the volumes that hold a database completed: the bootstrap "+
			"phase is Complete")
	}
	if len(events) > 0 {
		result.NeverEvents = append(result.NeverEvents, events...)
		return false, restoreVerdict{errors.New(strings.Join(events, "; "))}
	}
	if phase != string(databasev1alpha1.BootstrapFailed) {
		return false, nil
	}
	c := meta.FindStatusCondition(cluster.Status.Conditions, bootstrapReadyCondition)
	if c == nil || c.Reason != s12RefusedReason || !slices.ContainsFunc(wrongTargetGuards(), func(guard string) bool {
		return strings.Contains(c.Message, guard)
	}) {
		return false, restoreVerdict{fmt.Errorf("the restore over the volumes that hold a database ended Failed "+
			"for another reason than %s for a target that already holds database %s: %s", s12RefusedReason,
			databaseName, bootstrapReason(cluster))}
	}
	rec.Refusal = bootstrapReason(cluster)
	result.Measurements["attempts"] = attempts
	return true, nil
}

// checkSameVolumes reads the data PVCs after the refusal: they must be the
// ones the first ha-wt left, by UID. Another one means the restore was not
// refused over a volume that holds a database, so S12 wrong-target was not
// tested: it is blocked.
func (h *haRun) checkSameVolumes(ctx context.Context, rec *s12WrongTargetRecord) error {
	after, err := h.readVolumes(ctx)
	rec.VolumesAfterRefusal = after
	if err == nil {
		err = volumeChange(rec.Formed.Volumes, after)
	}
	if err != nil {
		return fmt.Errorf("after the refusal, the volumes of %s are not those that hold the database, so the "+
			"refusal was not one of a held volume: %w", s12WrongTargetClusterName, err)
	}
	rec.SameVolumes = true
	return nil
}

// checkExistingData reads the marker table of every member of the refused
// cluster, which CUBRID serves from the volumes: each must hold the marker
// written through the first ha-wt, and none S11's marker. Otherwise the
// existing database was changed, which must never happen. A table that
// cannot be read blocks S12 wrong-target.
func (h *haRun) checkExistingData(ctx context.Context, known *s11KnownPoint, rec *s12WrongTargetRecord,
	result *evidence.Scenario) error {
	data := &s12ExistingData{MarkerOn: []string{}, MissingOn: []string{}, S11MarkerOn: []string{}}
	rec.ExistingData = data
	members := clusterMembers(s12WrongTargetClusterName)
	missing, _, err := h.waitMarker(ctx, members, "", s12WrongTargetMarker, h.rec.RunID)
	if err != nil {
		return fmt.Errorf("read the existing data on the refused cluster: %w", err)
	}
	for _, pod := range members {
		table, err := h.markerTable(ctx, pod)
		if err != nil {
			return fmt.Errorf("read the existing data on the refused cluster: %w", err)
		}
		if _, ok := table[known.Marker]; ok {
			data.S11MarkerOn = append(data.S11MarkerOn, pod)
		}
	}
	data.Checked = true
	data.MissingOn = append(data.MissingOn, missing...)
	data.MarkerOn = slices.DeleteFunc(slices.Clone(members), func(m string) bool { return slices.Contains(missing, m) })
	var problems []string
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("the marker %s written through the first %s is not on %s",
			s12WrongTargetMarker, s12WrongTargetClusterName, strings.Join(missing, ", ")))
	}
	if len(data.S11MarkerOn) > 0 {
		problems = append(problems, fmt.Sprintf("S11's marker %s is on %s", known.Marker,
			strings.Join(data.S11MarkerOn, ", ")))
	}
	if len(problems) > 0 {
		event := "the existing database on the volumes of " + s12WrongTargetClusterName + " changed: " +
			strings.Join(problems, "; ")
		result.NeverEvents = append(result.NeverEvents, event)
		return restoreVerdict{errors.New(event)}
	}
	return nil
}
