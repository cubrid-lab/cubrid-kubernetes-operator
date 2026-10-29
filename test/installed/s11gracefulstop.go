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
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasev1alpha1 "github.com/cubrid-lab/cubrid-kubernetes-operator/api/v1alpha1"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/evidence"
	"github.com/cubrid-lab/cubrid-kubernetes-operator/test/sqlcheck"
)

const (
	s11GracefulStopLane                = "lab-installed-ha-s11-graceful-stop"
	s11GracefulStopVariant             = "graceful-stop"
	s11GracefulStopRecordFile          = "S11/graceful-stop/record.json"
	s11GracefulStopSamplesFile         = "S11/graceful-stop/sampler.jsonl"
	s11GracefulStopLogFile             = "S11/graceful-stop/member-log.jsonl"
	s11GracefulStopSourceDataCheckFile = "S11/graceful-stop/source-data-check.json"
	s11GracefulStopBackupName          = "s11-graceful-stop"
	// s11GracefulStopClusterName is the standalone cluster whose member's Pod
	// is deleted while backupdb runs; s11GracefulStopMember is its one
	// member's Pod.
	s11GracefulStopClusterName = "s11-gracefulstop"
	s11GracefulStopMember      = s11GracefulStopClusterName + "-0"
	// stopReasonPrefix is the failure reason of a backup whose backupdb was
	// ended by a stop of its member: stoppingReason (internal/instancemanager/
	// operations.go) puts errStopping's text before the reason runBackup
	// gives a failed backupdb (internal/instancemanager/server.go).
	stopReasonPrefix = "the member is stopping: backupdb failed: "
	// errStoppingText is errStopping's text (internal/instancemanager/
	// operations.go), and memberStopping that text with the separator
	// stoppingReason puts before any reason while the member stops.
	errStoppingText = "the member is stopping"
	memberStopping  = errStoppingText + ": "
	// backupdbSucceeded starts the reason of a backup that failed after a
	// backupdb that succeeded, before its upload (runBackup).
	backupdbSucceeded = "backupdb succeeded"
	// stagingKept and stagingKeptEnd mark the part of a reason that
	// awaitStagingWriters (internal/instancemanager/stagingwriter.go) adds
	// when a process still held a staging file after its bound: the
	// staging is kept.
	stagingKept    = "; the backup staging "
	stagingKeptEnd = " is kept"
	// The Instance Manager's log lines the variant orders (logging.go,
	// stagingwriter.go and opstore.go in internal/instancemanager).
	logCommandFailed   = "Command failed"
	logRanCommand      = "Ran command"
	logOperationFailed = "Operation failed"
	logEventCommand    = "command"
	logWriterWait      = "backup_staging_writer_wait"
	logWriterClosed    = "backup_staging_writer_closed"
	logWriterTimeout   = "backup_staging_writer_timeout"
	// backupdbCommand starts the command the Instance Manager logs for a
	// backupdb (Backup in internal/instancemanager/operations.go);
	// heartbeatStop and serverStop those of the shutdown (Shutdown).
	backupdbCommand = "cubrid backupdb "
	heartbeatStop   = "cubrid heartbeat stop"
	serverStop      = "cubrid server stop"
	// samplerInterval is the pause between two readings of the sampler.
	samplerInterval = 100 * time.Millisecond
	// streamsEndWait bounds how long the run waits for the sampler and the
	// followed log to end by themselves once the member's replacement was
	// seen or the backup ended without a deletion.
	streamsEndWait = 30 * time.Second
	// The measurements of the variant.
	measBackupdbSeenAfterRequest = "backupdbSeenAfterRequest"
	measDeleteAfterSeen          = "deleteAfterBackupdbSeen"
	measFailedAfterDelete        = "failedAfterDelete"
	measStagingGoneAfterDelete   = "stagingGoneAfterDelete"
	measHoldersAfterGone         = "holdersAfterStagingGone"
	measBackupdbDurationMs       = "backupdbDurationMs"
	measSamples                  = "samplerSamples"
	measReplacedWithin           = "replacementSeenWithin"
	measReadyAgainWithin         = "readyAgainWithin"
	measStopLoggedAfterDelete    = "stopLoggedAfterDelete"
	measFillerRows               = "fillerRows"
	measFillerGrowth             = "fillerGrowthTook"
	measFillerExitErrors         = "fillerExitErrors"
	// fillerTable is the table the run grows in the standalone member's
	// database; fillerRowBytes the width of its one CHAR column, one row's
	// payload; fillerChunk the most rows one statement copies, so that no
	// one transaction holds more than about 64 MiB of log; maxFillerMB the
	// largest filler the variant takes.
	fillerTable    = "gs_filler"
	fillerRowBytes = 1000
	fillerChunk    = 65536
	maxFillerMB    = 8192
	// imDatabasesDir is where the Instance Manager keeps the members'
	// databases on the data volume (restoreTargetRoot in
	// internal/controller/bootstrap.go).
	imDatabasesDir = "/var/lib/cubrid/databases"
	// ownedBackupKind and ownedClusterKind name what the variant created in
	// its cleanup (deleteByUID).
	ownedBackupKind  = "CubridBackup"
	ownedClusterKind = "cluster"
)

// gracefulStopSamplerScript is samplerScript for the backup staging root.
func gracefulStopSamplerScript() string {
	return samplerScript(imBackupStagingRoot)
}

// samplerScript prints, about every samplerInterval until it is ended, one
// reading of the member: a line "T <seconds>.<nanoseconds>" by the
// container's clock, then "P <pid> <comm> <argv>" for each process whose
// command name starts with cub (cubrid, cub_admin, cub_server, cub_master),
// "E <destination>/<entry>" for each entry two levels below the staging root
// root, then, from the /proc/<pid>/fd links that one ls lists, "F <pid>" for
// each of those cub* processes whose links it could list and "H <pid>
// <path>" for each open file at or below root, a deleted file included
// ("<path> (deleted)"), and "." at the end of the reading. The entries are
// listed before the open files, so that a holder in a reading without the
// entry was seen after the entry was gone. It prints no file's contents. It
// runs as the container's user, the one cub_server runs as, so it reads the
// links of CUBRID's processes, as stagingHolders in
// internal/instancemanager/stagingwriter.go does.
func samplerScript(root string) string {
	return `r='` + root + `'; while :; do ` +
		`echo "T $(date -u +%s.%N)"; c=" "; ` +
		`for p in /proc/[0-9]*; do n=""; read -r n < "$p/comm" 2>/dev/null || continue; ` +
		`case "$n" in cub*) ;; *) continue ;; esac; a=(); mapfile -d '' -t a < "$p/cmdline" 2>/dev/null; ` +
		`c="$c${p#/proc/} "; echo "P ${p#/proc/} $n ${a[*]}"; done; ` +
		`for e in "$r"/*/*; do [ -e "$e" ] && echo "E ${e#"$r"/}"; done; ` +
		`ls -l /proc/[0-9]*/fd 2>/dev/null | while IFS= read -r l; do case "$l" in ` +
		`/proc/*/fd:) q=${l#/proc/}; q=${q%/fd:}; case "$c" in *" $q "*) echo "F $q" ;; esac ;; ` +
		`*" -> $r/"*) echo "H $q ${l#* -> }" ;; esac; done; ` +
		`echo .; sleep 0.1; done`
}

// stopSample is one reading of the sampler: when the container's clock and
// the run's took it, the cub* processes ("<pid> <comm> <argv>"), the entries
// two levels below the staging root ("<destination>/<entry>"), the cub*
// processes whose open files ls could list (FdListed) and the open files
// below the staging root ("<pid> <path>").
type stopSample struct {
	At        time.Time `json:"at"`
	Received  time.Time `json:"receivedAt"`
	Processes []string  `json:"processes"`
	Entries   []string  `json:"entries"`
	FdListed  []string  `json:"fdListed"`
	Holders   []string  `json:"holders"`
}

// backupdbProcess returns the first process of the reading whose argv runs
// backupdb, or "".
func (s stopSample) backupdbProcess() string {
	for _, p := range s.Processes {
		if slices.Contains(strings.Fields(p), "backupdb") {
			return p
		}
	}
	return ""
}

// s11StopSampler is what the run took from the sampler and found in it.
type s11StopSampler struct {
	Interval string `json:"interval"`
	Samples  int    `json:"samples"`
	// BytesRead and Truncated are of the sampler's output, which the run
	// reads up to its log limit; Unparsed counts the lines it did not know.
	BytesRead int64 `json:"bytesRead"`
	Truncated bool  `json:"truncated"`
	Unparsed  int   `json:"unparsedLines"`
	// EndedAt is when its output ended, and EndError why, when it was not
	// the end of the output.
	EndedAt  *time.Time `json:"endedAt,omitempty"`
	EndError string     `json:"endError,omitempty"`
	// FirstBackupdbAt and LastBackupdbAt are the first and the last reading,
	// by the run's clock, that showed a backupdb process.
	FirstBackupdbAt *time.Time `json:"firstBackupdbAt,omitempty"`
	LastBackupdbAt  *time.Time `json:"lastBackupdbAt,omitempty"`
	// StagingDir is the operation's staging directory, below the staging
	// root; StagingSeenAt the first reading that showed it, StagingGoneAt
	// the first reading after that which did not.
	StagingDir    string     `json:"stagingDir,omitempty"`
	StagingSeenAt *time.Time `json:"stagingSeenAt,omitempty"`
	StagingGoneAt *time.Time `json:"stagingGoneAt,omitempty"`
	// LastHolder is the last open file below the backup's staging
	// destination before StagingGoneAt, and LastHolderAt its reading;
	// HoldersAfterGone are those at or after StagingGoneAt, which must never
	// be there.
	LastHolder       string     `json:"lastHolder,omitempty"`
	LastHolderAt     *time.Time `json:"lastHolderAt,omitempty"`
	HoldersAfterGone []string   `json:"holdersAfterGone"`
}

// s11StopLog is what the run found in the followed log of the member's
// database container: the lines it orders, as their line numbers in the
// log. Lines counts the lines that were JSON.
type s11StopLog struct {
	BytesRead int64      `json:"bytesRead"`
	Truncated bool       `json:"truncated"`
	Lines     int        `json:"lines"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	EndError  string     `json:"endError,omitempty"`
	// BackupdbLine is the line of backupdb's end, BackupdbResult its
	// message (Command failed or Ran command) and BackupdbDurationMs its
	// duration.
	BackupdbLine       int    `json:"backupdbLine,omitempty"`
	BackupdbResult     string `json:"backupdbResult,omitempty"`
	BackupdbDurationMs *int64 `json:"backupdbDurationMs,omitempty"`
	// WriterEvents are the staging writer waits of the operation, as
	// "<line> <event>".
	WriterEvents []string `json:"writerEvents"`
	// OperationFailedLine and OperationFailedReason are of the operation's
	// Operation failed.
	OperationFailedLine   int    `json:"operationFailedLine,omitempty"`
	OperationFailedReason string `json:"operationFailedReason,omitempty"`
	// HeartbeatStopLine and HeartbeatStopResult are of the shutdown's
	// heartbeat stop, which on a standalone member fails with "not
	// configured for HA"; ServerStopLine and ServerStopResult of its server
	// stop.
	HeartbeatStopLine   int    `json:"heartbeatStopLine,omitempty"`
	HeartbeatStopResult string `json:"heartbeatStopResult,omitempty"`
	ServerStopLine      int    `json:"serverStopLine,omitempty"`
	ServerStopResult    string `json:"serverStopResult,omitempty"`
	// StopLine is the first line the stop caused (isStopLine), StopLoggedAt
	// its time by the member's clock and StopLoggedAfterDelete the time from
	// the deletion's acceptance, by the run's clock, to it: an upper bound on
	// the preStop hook's latency, since the Instance Manager logs nothing
	// when a stop begins. The two clocks' skew is not measured.
	StopLine              int        `json:"stopLine,omitempty"`
	StopLoggedAt          *time.Time `json:"stopLoggedAt,omitempty"`
	StopLoggedAfterDelete string     `json:"stopLoggedAfterDelete,omitempty"`
}

// s11Filler is the table the run grew in the standalone member's database
// before the request, so that backupdb runs long enough for the stop to
// land inside it: its name, the size asked for, the rows of fillerRowBytes
// it holds (counted at the end), the statements run, when the growth started
// and how long it took, and the database directory's size in KiB after it
// (du -sk), or why that could not be read.
type s11Filler struct {
	Table      string `json:"table"`
	TargetMB   int    `json:"targetMB"`
	RowBytes   int    `json:"rowBytes"`
	TargetRows int    `json:"targetRows"`
	Rows       int    `json:"rows"`
	Statements int    `json:"statements"`
	// Retries counts the statements run a second time because the count
	// did not grow; ExitErrors those that printed an error although their
	// rows were counted (POC-44). Both are evidence, not verdicts.
	Retries       int        `json:"retries"`
	ExitErrors    int        `json:"fillerExitErrors"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	Took          string     `json:"took,omitempty"`
	DatabaseDirKB int64      `json:"databaseDirKB,omitempty"`
	DuError       string     `json:"duError,omitempty"`
	DataVolume    string     `json:"dataVolume"`
}

// s11StopTrigger is the deletion of the member's Pod and its replacement.
type s11StopTrigger struct {
	// PodUID and ContainerID are the member's Pod and database container
	// read right before the request; the deletion carries the UID as a
	// precondition.
	PodUID      string `json:"podUID,omitempty"`
	ContainerID string `json:"containerID,omitempty"`
	// SeenAt is the sampler's reading, by the run's clock, that showed
	// backupdb while the trigger was armed, and SeenProcess the process.
	SeenAt      *time.Time `json:"seenAt,omitempty"`
	SeenProcess string     `json:"seenProcess,omitempty"`
	// DeleteIssuedAt and DeleteAcceptedAt are when the deletion was sent and
	// when the API server accepted it; DeleteError is its refusal.
	DeleteIssuedAt   *time.Time `json:"deleteIssuedAt,omitempty"`
	DeleteAcceptedAt *time.Time `json:"deleteAcceptedAt,omitempty"`
	DeleteError      string     `json:"deleteError,omitempty"`
	// Missed says why the trigger was not placed.
	Missed string `json:"missed,omitempty"`
	// ReplacementUID is the Pod the StatefulSet created in its place,
	// ReplacementAt the reading that first saw it, ReadyAgainAt the first
	// reading of the cluster Ready after its database container was ready,
	// ReadyAgainWithin the time from the deletion to it, ReadyTransitionAt
	// the Ready condition's lastTransitionTime then, and ReadyWaiting what
	// did not hold yet at the last reading before.
	ReplacementUID    string     `json:"replacementUID,omitempty"`
	ReplacementAt     *time.Time `json:"replacementAt,omitempty"`
	ReadyAgainAt      *time.Time `json:"readyAgainAt,omitempty"`
	ReadyAgainWithin  string     `json:"readyAgainWithin,omitempty"`
	ReadyTransitionAt *time.Time `json:"readyTransitionAt,omitempty"`
	ReadyWaiting      string     `json:"readyWaiting,omitempty"`
	// ReadyUnchanged is set when ReadyTransitionAt is before the second of
	// the deletion's acceptance: Ready never went False through the
	// replacement, as the Operator observed it. It is recorded, not judged.
	ReadyUnchanged string `json:"readyUnchanged,omitempty"`
}

// s11GracefulStopRecord is the evidence of S11 graceful-stop besides the
// sampler's readings, the member's log and the source's data check.
type s11GracefulStopRecord struct {
	Backup             string                `json:"backup"`
	BackupUID          string                `json:"backupUID,omitempty"`
	Bucket             string                `json:"bucket"`
	Prefix             string                `json:"prefix"`
	ObjectStoreService string                `json:"objectStoreService"`
	PodSecurity        map[string]string     `json:"podSecurity"`
	Cluster            *s11StandaloneCluster `json:"cluster,omitempty"`
	ReadyLimit         string                `json:"readyLimit"`
	Filler             *s11Filler            `json:"filler,omitempty"`
	Trigger            *s11StopTrigger       `json:"trigger"`
	Sampler            *s11StopSampler       `json:"sampler"`
	Log                *s11StopLog           `json:"log"`
	RequestedAt        *time.Time            `json:"requestedAt,omitempty"`
	Phases             []s11Phase            `json:"phases"`
	LastPhase          string                `json:"lastPhase,omitempty"`
	// OperationRef is the first operationRef the readings showed, and
	// OperationRefsSeen every one they showed, in order, each once.
	OperationRef      string   `json:"operationRef,omitempty"`
	OperationRefsSeen []string `json:"operationRefsSeen"`
	// EndedAfterDelete is set when the wait's first reading of the phase the
	// backup ended in was taken after the deletion was accepted.
	EndedAfterDelete bool `json:"endedAfterDelete"`
	// FailedWithin runs from the request's return, and FailedAfterDelete from
	// the deletion's acceptance, to the reading that saw Failed.
	FailedWithin      string                                 `json:"failedWithin,omitempty"`
	FailedAfterDelete string                                 `json:"failedAfterDelete,omitempty"`
	Failure           string                                 `json:"failure,omitempty"`
	PhaseAtEnd        string                                 `json:"phaseAtEnd,omitempty"`
	ArtifactAtEnd     *databasev1alpha1.CubridBackupArtifact `json:"artifactAtEnd,omitempty"`
	Artifact          *databasev1alpha1.CubridBackupArtifact `json:"artifact,omitempty"`
	ArtifactsNotDone  []s11ArtifactReading                   `json:"artifactsNotCompleted"`
	s11PrefixAtEnd
	ManagerBefore *managerIdentity `json:"managerBefore,omitempty"`
	ManagerAfter  *managerIdentity `json:"managerAfter,omitempty"`
	Source        *s12Source       `json:"source,omitempty"`
	Credentials   *s11Credentials  `json:"credentials,omitempty"`
	JudgedAt      *time.Time       `json:"judgedAt,omitempty"`
	Notes         []string         `json:"notes,omitempty"`
	Cleanup       string           `json:"cleanup"`
	Residue       string           `json:"residue"`
}

// validateS11GracefulStop checks the inputs of S11 graceful-stop, when it is
// selected; BackupBucket is checked by validateS11.
func (c HAConfig) validateS11GracefulStop() error {
	if !c.BackupGracefulStop {
		return nil
	}
	for _, other := range []struct {
		selected bool
		flag     string
	}{{c.Restore, flagRestore}, {c.BackupMissingDestination, flagMissingDestination},
		{c.BackupDeniedCredentials, flagDeniedCredentials},
		{c.BackupSignatureMismatch, flagSignatureMismatch},
		{c.BackupInterruptedUpload, flagInterruptedUpload},
		{c.BackupOperatorRestart, flagOperatorRestart},
		{c.BackupInstanceManagerRestart, flagInstanceManagerRestart},
		{c.BackupOverlapping, flagOverlapping},
		{c.BackupOperationStoreLoss, flagOperationStoreLoss}} {
		if other.selected {
			return fmt.Errorf("%s and %s select different lanes; a run takes one of them", flagGracefulStop,
				other.flag)
		}
	}
	if c.BackupGracefulStopReadyLimit <= 0 {
		return errors.New(flagGracefulStop + " needs a positive -backup-graceful-stop-ready-limit for its cluster")
	}
	if c.BackupGracefulStopFillerMB < 0 || c.BackupGracefulStopFillerMB > maxFillerMB {
		return fmt.Errorf("%s needs a -backup-graceful-stop-filler-mb of 0 to %d, not %d", flagGracefulStop,
			maxFillerMB, c.BackupGracefulStopFillerMB)
	}
	return nil
}

// planS11GracefulStop returns the lane and the requirements of a run that
// selects S11 graceful-stop after S11 complete, and names it in the
// summary's notes; otherwise lane and requirements as they are.
func planS11GracefulStop(cfg HAConfig, lane string, requirements []evidence.Requirement,
	summary *evidence.Summary) (string, []evidence.Requirement) {
	if !cfg.BackupGracefulStop {
		return lane, requirements
	}
	summary.Notes[0] = "Three-member HA cluster against an installed Operator, then S11 complete and S11 " +
		"graceful-stop: a full backup to object storage, downloaded and checked by the run, and a backup of a " +
		"standalone cluster whose member's Pod is deleted with its grace period while backupdb runs, which must " +
		"end Failed for the stop with no process holding a staging file once the staging is gone; no other " +
		"S00-S15 qualification"
	return s11GracefulStopLane, append(requirements, evidence.Requirement{ID: s11ID, Variant: s11GracefulStopVariant})
}

// stopWatch is the state the sampler, the trigger and the wait for the
// backup share. The sampler's reader appends readings and fires the trigger
// at the first one that shows backupdb while it is armed; a reading of the
// backup that has left Pending or Running before then disarms it.
type stopWatch struct {
	mu      sync.Mutex
	samples []stopSample
	armed   bool
	fired   bool
	seen    chan struct{}
	// output is the sampler's output as read, for the search for keys.
	output bytes.Buffer
}

// runS11GracefulStop runs S11 graceful-stop after S11 complete passed. The
// run creates a standalone cluster with the run's own object store Secret,
// waits until it is Ready, and starts two streams from its member's
// database container: a sampler that prints the cub* processes, the files
// open below the backup staging root and the entries below it every 100 ms
// (gracefulStopSamplerScript), and the container's log, followed. It then
// requests a full backup of the cluster's database to BackupBucket, and as
// soon as the sampler shows a process whose argv runs backupdb, deletes the
// member's Pod with its grace period and its UID as a precondition. The Pod's
// preStop hook asks the Instance Manager to stop
// (preStopShutdown in internal/controller/cubridcluster_controller.go); the
// stop cancels the backup, whose backupdb group is killed, waits until no
// process holds a staging file, removes the staging, records the operation
// Failed, and only then stops CUBRID (Stop and Shutdown in
// internal/instancemanager/operations.go, runBackup in server.go).
//
// The backup must end Failed within BackupLimit with BackupFailed and a
// reason that starts with stopReasonPrefix and does not say that the staging
// is kept, never Completed and without status.artifact, under one
// operationRef; no reading of the sampler at or after the first one without
// the operation's staging directory may show a process holding a file
// below the backup's staging destination; the log must hold backupdb's
// Command failed, then the operation's staging writer events if any, then
// its Operation failed, then the shutdown's heartbeat stop and server stop;
// no manifest.json may be under the prefix at the end; the StatefulSet's
// replacement Pod must be ready and the cluster Ready within the ready limit
// from the deletion; the source and the installed manager must be
// unchanged; and neither seed key may be in a status read, an Event, the
// sampler's output, the followed log, the replacement's log or the
// Operator's. A backupdb never seen while the backup was Pending or Running,
// or a backup that failed or completed after a backupdb that had ended,
// leaves the variant blocked; so does a log or a sampler that ended before
// it showed what is judged.
func (h *haRun) runS11GracefulStop(ctx context.Context) (result evidence.Scenario) {
	defer h.inScenario(s11ID+"/"+s11GracefulStopVariant, &result)()
	result = evidence.Scenario{ID: s11ID, Variant: s11GracefulStopVariant, Result: evidence.Fail,
		NeverEvents: []string{}}
	rec := &s11GracefulStopRecord{Backup: s11GracefulStopBackupName, Bucket: h.cfg.BackupBucket,
		Prefix: h.rec.Seed.Prefix + "/" + s11GracefulStopVariant, ObjectStoreService: h.cfg.ObjectStoreService,
		PodSecurity: map[string]string{},
		Cluster:     &s11StandaloneCluster{Name: s11GracefulStopClusterName, Secret: seedSecretName},
		ReadyLimit:  h.cfg.BackupGracefulStopReadyLimit.String(), Trigger: &s11StopTrigger{},
		Sampler: &s11StopSampler{Interval: samplerInterval.String(), HoldersAfterGone: []string{}},
		Log:     &s11StopLog{WriterEvents: []string{}}, Phases: []s11Phase{}, OperationRefsSeen: []string{},
		ArtifactsNotDone: []s11ArtifactReading{}, s11PrefixAtEnd: newS11PrefixAtEnd(),
		Credentials: &s11Credentials{FoundIn: []string{}},
		Notes: []string{"the run namespace keeps the Pod Security level LAB01 created it with: the variant adds no " +
			"container and needs no privilege; it uses pods/exec, pods/log and the deletion of one member Pod",
			"the sampler runs in the member's database container through one exec session and reads it every " +
				samplerInterval.String() + ": a writer that holds a file for less than that may not be seen, and " +
				"the session ends when the container stops, so nothing after the container's end is sampled",
			"the sampler prints process IDs, command names and argv, and the paths of open files below the backup " +
				"staging root, never a file's contents",
			"the member's log is followed through pods/log from before the request until the deleted container " +
				"ended, whole; the deleted Pod's log is gone once it is replaced, so this is the only read of it",
			"the deletion uses the Pod's own terminationGracePeriodSeconds, so its preStop hook runs " +
				"instance-manager shutdown before the container gets SIGTERM",
			"the run never writes to or removes from the object store; with the seed Secret's keys it lists the " +
				"backup bucket under the prefix at the end",
			"before the request the run grows a table " + fillerTable + " in the standalone member's database, " +
				"through csql in its database container, so that backupdb runs long enough for the stop to land " +
				"inside it; no data check reads that cluster, and the HA source is not touched",
			"the time from the deletion to the first log line the stop caused is an upper bound on the preStop " +
				"hook's latency, across the run's clock and the member's",
			logsNote(h.logLimit())},
		Cleanup: nothingCreated, Residue: "nothing: the run wrote nothing to the object store"}
	if mb := h.cfg.BackupGracefulStopFillerMB; mb > 0 {
		volume := gracefulStopDataVolume(mb)
		rec.Filler = &s11Filler{Table: fillerTable, TargetMB: mb, RowBytes: fillerRowBytes,
			TargetRows: fillerRows(mb), DataVolume: volume.String()}
	}
	var made s11InterruptedMade
	watch := &stopWatch{seen: make(chan struct{})}
	var memberLog []byte
	defer func() {
		cleaned := h.deleteGracefulStop(ctx, made, rec)
		result.CleanupSucceeded = &cleaned
		var errs []error
		if err := h.writeSamples(watch); err != nil {
			errs = append(errs, err)
		}
		if memberLog != nil {
			errs = append(errs, h.write(s11GracefulStopLogFile, memberLog))
		}
		errs = append(errs, h.writeJSON(s11GracefulStopRecordFile, rec))
		// A pass without its evidence is a failure; a blocked or failed S11
		// graceful-stop keeps its result, with the evidence that could not be
		// written.
		if err := errors.Join(errs...); err != nil {
			if result.Result == evidence.Pass {
				result.Result, result.Reason = evidence.Fail, ""
			}
			result.Reason = strings.TrimPrefix(result.Reason+"; write S11 graceful-stop evidence: "+err.Error(),
				"; ")
		}
		result.Evidence = slices.DeleteFunc(slices.Clone(h.files), func(f string) bool {
			return !strings.HasPrefix(f, "S11/"+s11GracefulStopVariant+"/")
		})
	}()
	result.Limits = map[string]string{backupLimitName: h.cfg.BackupLimit.String(),
		readyLimitName: h.cfg.BackupGracefulStopReadyLimit.String()}
	result.Measurements = map[string]any{failedWithinKey: evidence.Unknown, measFailedAfterDelete: evidence.Unknown,
		measBackupdbSeenAfterRequest: evidence.Unknown, measDeleteAfterSeen: evidence.Unknown,
		measStagingGoneAfterDelete: evidence.Unknown, measHoldersAfterGone: evidence.Unknown,
		measBackupdbDurationMs: evidence.Unknown, measSamples: evidence.Unknown,
		measReplacedWithin: evidence.Unknown, measReadyAgainWithin: evidence.Unknown,
		measOperationRefUnchanged: evidence.Unknown, measStopLoggedAfterDelete: evidence.Unknown,
		measFillerRows: evidence.Unknown, measFillerGrowth: evidence.Unknown}
	err := h.gracefulStopSteps(ctx, rec, &made, watch, &memberLog, &result)
	h.judgeGracefulStopRun(ctx, err, rec, &result)
	judged := time.Now().UTC()
	rec.JudgedAt = &judged
	return result
}

// judgeGracefulStopRun sets the result of S11 graceful-stop from the error of
// its steps and the never-events they recorded.
func (h *haRun) judgeGracefulStopRun(ctx context.Context, err error, rec *s11GracefulStopRecord,
	result *evidence.Scenario) {
	if found := rec.Credentials.FoundIn; len(found) > 0 {
		result.NeverEvents = append(result.NeverEvents, credentialEvent(found))
	}
	never := strings.Join(result.NeverEvents, "; ")
	parts := slices.Clone(result.NeverEvents)
	if err != nil {
		for part := range strings.SplitSeq(err.Error(), "\n") {
			if part != "" && !slices.Contains(parts, part) {
				parts = append(parts, part)
			}
		}
	}
	reason := strings.Join(parts, "; ")
	switch {
	case errors.As(err, new(backupVerdict)) || errors.As(err, new(restoreVerdict)) ||
		errors.As(err, new(dataViolation)) || never != "":
		result.Result, result.Reason = evidence.Fail, reason
	case err != nil && ctx.Err() != nil:
		result.Result, result.Reason = evidence.Blocked, "the run ended before S11 graceful-stop was judged: "+
			reason
	case err != nil:
		result.Result, result.Reason = evidence.Blocked, reason
	default:
		t, s, l := rec.Trigger, rec.Sampler, rec.Log
		last := "no process held a staging file in any reading, about every " + samplerInterval.String()
		if s.LastHolder != "" {
			last = fmt.Sprintf("the last open staging file was %s, at %s", s.LastHolder,
				s.LastHolderAt.Format(time.RFC3339Nano))
		}
		result.Result = evidence.Pass
		result.Reason = fmt.Sprintf("the sampler showed %q in %s's database container at %s, and the member's Pod "+
			"was deleted with its grace period, the deletion accepted at %s; the backup ended Failed within "+
			"backup_limit with %s %q, without a kept staging, under its one operation %s, never Completed and "+
			"without status.artifact, also when read again at the end; the sampler saw the operation's staging "+
			"directory gone at %s and no process holding a file below the backup's staging destination then or "+
			"after, in any reading, about every "+samplerInterval.String()+" (%s); the member's log held "+
			"backupdb's %s, %d staging writer event(s), the operation's %s and "+
			"then the shutdown's heartbeat stop, in that order; no manifest.json was under the prefix at the end; "+
			"the replacement Pod was seen and the cluster read Ready %s after the deletion, within the ready limit; "+
			"the source and the installed manager were unchanged; no seed key was in the statuses read, the "+
			"Events, the sampler's output, the deleted member's followed log, the replacement's log or the "+
			"installed Operator's manager log", t.SeenProcess, s11GracefulStopMember,
			t.SeenAt.Format(time.RFC3339Nano), t.DeleteAcceptedAt.Format(time.RFC3339Nano), s11MissingReason,
			rec.Failure, rec.OperationRef, s.StagingGoneAt.Format(time.RFC3339Nano), last, l.BackupdbResult,
			len(l.WriterEvents), logOperationFailed, t.ReadyAgainWithin)
	}
}

// gracefulStopSteps runs the steps of S11 graceful-stop in order. Before the
// request, the first error ends them. After it, the backup is judged, the
// sampler and the log are judged, and the checks at the end are all made;
// their errors are joined.
func (h *haRun) gracefulStopSteps(ctx context.Context, rec *s11GracefulStopRecord, made *s11InterruptedMade,
	watch *stopWatch, memberLog *[]byte, result *evidence.Scenario) error {
	started := time.Now().UTC()
	if _, err := h.holdsCredential(nil); err != nil {
		return fmt.Errorf("%w; S11 graceful-stop could not tell whether a credential is in a status or an Event", err)
	}
	if h.execStream == nil || h.followLogs == nil {
		return errors.New("S11 graceful-stop needs a streaming exec and a followed log, and this runner has " +
			"neither or only one; nothing was created")
	}
	before := h.rec.OperatorAfter
	rec.ManagerBefore = &before
	if err := h.guard(ctx); err != nil {
		return err
	}
	ns := &corev1.Namespace{}
	if err := h.Client.Get(ctx, client.ObjectKey{Name: h.cfg.Namespace}, ns); err != nil {
		return fmt.Errorf("read the run namespace: %w", err)
	}
	rec.PodSecurity = podSecurityLabels(ns.Labels)
	if err := h.createGracefulStop(ctx, rec.Cluster, made); err != nil {
		return err
	}
	if err := h.waitStandaloneReady(ctx, made.cluster, rec.Cluster, rec.Credentials,
		h.cfg.BackupGracefulStopReadyLimit); err != nil {
		return err
	}
	if err := h.growFiller(ctx, rec.Filler, result); err != nil {
		return err
	}
	if err := h.memberBeforeRequest(ctx, made.cluster, rec.Trigger); err != nil {
		return err
	}
	sctx, stopStreams := context.WithCancel(ctx)
	defer stopStreams()
	samplerDone, err := h.startSampler(sctx, watch, rec.Sampler)
	if err != nil {
		return err
	}
	logDone, err := h.startMemberLog(sctx, rec.Log, memberLog)
	if err != nil {
		stopStreams()
		<-samplerDone
		return err
	}
	backup, waitErr := h.gracefulStopBackup(ctx, rec, made, watch, result)
	if rec.RequestedAt == nil {
		stopStreams()
		<-samplerDone
		<-logDone
		return waitErr
	}
	var replaced error
	if rec.Trigger.DeleteAcceptedAt != nil {
		replaced = h.waitReplacementReady(ctx, made.cluster, rec, result)
	} else {
		// Without a deletion the container runs on, and so do the streams.
		stopStreams()
	}
	awaitStreams(samplerDone, logDone, stopStreams)
	result.Measurements[measSamples] = rec.Sampler.Samples
	// The log is read before the backup is judged: a Completed backup, and a
	// manifest under the prefix, are judged by whether the log shows backupdb
	// end for the stop or succeed before it.
	var logErr, samplesErr error
	switch deleted := rec.Trigger.DeleteAcceptedAt != nil; {
	case deleted && rec.OperationRef == "":
		readStopLog(*memberLog, rec, result)
		logErr = errors.New("no operationRef was read after the deletion, so the sampler and the member's log " +
			"cannot be judged against the backup's operation")
	case deleted:
		logErr, samplesErr = judgeStopLog(*memberLog, rec, result), judgeStopSamples(watch, rec, result)
	}
	errs := make([]error, 0, 9)
	errs = append(errs, judgeStopBackup(backup, waitErr, *rec.RequestedAt, rec, result), samplesErr, logErr,
		replaced)
	// After backupdb succeeded, a manifest is the backup's own: the placement
	// was missed. Only a backupdb that ended for the stop makes one a
	// never-event; without its end in the log, a manifest cannot be judged.
	errs = append(errs, h.prefixAtEnd(ctx, rec.Bucket, rec.Prefix, &rec.s11PrefixAtEnd, &rec.Residue, s11FaultEnd{
		completedBefore: rec.Log.BackupdbResult == logRanCommand,
		endedAfter:      rec.EndedAfterDelete && rec.Log.BackupdbResult == logCommandFailed, lastPhase: rec.LastPhase,
		fault:     "its member's Pod was deleted and backupdb ended for the stop",
		confirmed: "an accepted deletion and backupdb's end for the stop in the member's log",
		variant:   "S11 graceful-stop"}, result))
	errs = append(errs, h.gracefulStopBackupAtEnd(ctx, made.backup, rec, result))
	errs = append(errs, h.gracefulStopCredentials(ctx, made, rec, watch, *memberLog, started))
	rec.Source = &s12Source{MembersAfter: []imageIdentity{}}
	errs = append(errs, h.checkSourceS12(ctx, rec.Source, s11GracefulStopSourceDataCheckFile))
	errs = append(errs, h.sameManager(ctx, "S11 graceful-stop", rec.ManagerBefore, &rec.ManagerAfter))
	return errors.Join(errs...)
}

// awaitStreams waits until the sampler and the followed log ended by
// themselves, for at most streamsEndWait, and then ends them.
func awaitStreams(samplerDone, logDone <-chan struct{}, stop context.CancelFunc) {
	timer := time.NewTimer(streamsEndWait)
	defer timer.Stop()
	for _, done := range []<-chan struct{}{samplerDone, logDone} {
		select {
		case <-done:
		case <-timer.C:
			stop()
			<-done
		}
	}
	stop()
}

// memberBeforeRequest reads the member's Pod right before the request,
// requires the cluster's StatefulSet to control it and its database
// container to be ready, and records its UID and the container's ID.
func (h *haRun) memberBeforeRequest(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	t *s11StopTrigger) error {
	if err := h.guard(ctx); err != nil {
		return err
	}
	pod := &corev1.Pod{}
	if err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: s11GracefulStopMember},
		pod); err != nil {
		return fmt.Errorf("read the member %s: %w", s11GracefulStopMember, err)
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != kindStatefulSet || owner.Name != cluster.Name {
		return fmt.Errorf("the Pod %s is not controlled by the StatefulSet of %s", pod.Name, cluster.Name)
	}
	status := databaseContainerStatus(pod)
	if pod.UID == "" || status == nil || status.ContainerID == "" || !status.Ready {
		return fmt.Errorf("the member %s has no ready database container before the request", pod.Name)
	}
	t.PodUID, t.ContainerID = string(pod.UID), status.ContainerID
	return nil
}

// startSampler starts gracefulStopSamplerScript in the member's database
// container and reads its output until it ends, at most the run's log limit;
// the returned channel is closed then. Each reading is appended to watch,
// and the first that shows backupdb while watch is armed fires it.
func (h *haRun) startSampler(ctx context.Context, watch *stopWatch, s *s11StopSampler) (<-chan struct{}, error) {
	if err := h.guard(ctx); err != nil {
		return nil, err
	}
	stream, err := h.execStream(ctx, "-n", h.cfg.Namespace, "exec", s11GracefulStopMember, "-c", databaseContainer,
		"--", "bash", "-c", gracefulStopSamplerScript())
	if err != nil {
		return nil, fmt.Errorf("start the sampler in %s: %w", s11GracefulStopMember, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = stream.Close() }()
		limit := h.logLimit()
		counted := &countingReader{r: io.LimitReader(stream, limit+1)}
		err := readSamples(io.TeeReader(counted, &lockedWriter{mu: &watch.mu, w: &watch.output}), watch, s)
		watch.mu.Lock()
		defer watch.mu.Unlock()
		at := time.Now().UTC()
		s.EndedAt, s.BytesRead, s.Truncated = &at, counted.n, counted.n > limit
		if err != nil && ctx.Err() == nil {
			s.EndError = h.redactError(err)
		}
	}()
	return done, nil
}

// countingReader counts what is read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// lockedWriter writes to w with mu held.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// readSamples parses the sampler's output into readings and adds each to
// watch as it ends (addSample).
func readSamples(r io.Reader, watch *stopWatch, s *s11StopSampler) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var cur *stopSample
	unparsed := 0
	for sc.Scan() {
		line := sc.Text()
		kind, rest, _ := strings.Cut(line, " ")
		switch {
		case kind == "T":
			at, err := parseSampleTime(rest)
			if err != nil {
				unparsed++
				cur = nil
				continue
			}
			cur = &stopSample{At: at, Processes: []string{}, Entries: []string{}, FdListed: []string{},
				Holders: []string{}}
		case cur == nil:
			unparsed++
		case kind == "P":
			cur.Processes = append(cur.Processes, rest)
		case kind == "F":
			cur.FdListed = append(cur.FdListed, rest)
		case kind == "H":
			cur.Holders = append(cur.Holders, rest)
		case kind == "E":
			cur.Entries = append(cur.Entries, rest)
		case line == ".":
			cur.Received = time.Now().UTC()
			watch.addSample(*cur, s)
			cur = nil
		default:
			unparsed++
		}
	}
	watch.mu.Lock()
	s.Unparsed += unparsed
	watch.mu.Unlock()
	return sc.Err()
}

// parseSampleTime reads "<seconds>.<nanoseconds>" as date +%s.%N prints it.
func parseSampleTime(v string) (time.Time, error) {
	sec, frac, _ := strings.Cut(strings.TrimSpace(v), ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	var ns int64
	if frac != "" {
		frac = (frac + "000000000")[:9]
		if ns, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return time.Time{}, err
		}
	}
	return time.Unix(s, ns).UTC(), nil
}

// addSample records one reading, and fires the trigger at the first that
// shows a backupdb process while it is armed.
func (w *stopWatch) addSample(sample stopSample, s *s11StopSampler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.samples = append(w.samples, sample)
	s.Samples = len(w.samples)
	if p := sample.backupdbProcess(); p != "" {
		at := sample.Received
		if s.FirstBackupdbAt == nil {
			s.FirstBackupdbAt = &at
		}
		s.LastBackupdbAt = &at
		if w.armed && !w.fired {
			w.fired = true
			close(w.seen)
		}
	}
}

// startMemberLog follows the log of the member's database container, whole,
// at most the run's log limit and one byte more, into out; the returned
// channel is closed once the log ended.
func (h *haRun) startMemberLog(ctx context.Context, l *s11StopLog, out *[]byte) (<-chan struct{}, error) {
	if err := h.guard(ctx); err != nil {
		return nil, err
	}
	limit := h.logLimit()
	asked := limit + 1
	stream, err := h.followLogs(ctx, h.cfg.Namespace, s11GracefulStopMember, &corev1.PodLogOptions{
		Container: databaseContainer, Follow: true, LimitBytes: &asked})
	if err != nil {
		return nil, fmt.Errorf("follow the log of %s: %s", s11GracefulStopMember, h.redactError(err))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = stream.Close() }()
		data, err := io.ReadAll(io.LimitReader(stream, asked))
		at := time.Now().UTC()
		*out = data
		l.EndedAt, l.BytesRead, l.Truncated = &at, int64(len(data)), int64(len(data)) > limit
		if err != nil && ctx.Err() == nil {
			l.EndError = h.redactError(err)
		}
	}()
	return done, nil
}

// gracefulStopBackup arms the trigger, requests the backup, deletes the
// member's Pod once the sampler shows backupdb (triggerStop), and waits for
// the backup. It returns the backup as the wait left it and the wait's
// error; the backup is judged once the member's log was read
// (judgeStopBackup).
func (h *haRun) gracefulStopBackup(ctx context.Context, rec *s11GracefulStopRecord, made *s11InterruptedMade,
	watch *stopWatch, result *evidence.Scenario) (*databasev1alpha1.CubridBackup, error) {
	watch.mu.Lock()
	watch.armed = true
	watch.mu.Unlock()
	backup, err := h.createBackup(ctx, rec.Backup, rec.Cluster.Name, rec.Bucket, rec.Prefix)
	if err != nil {
		watch.mu.Lock()
		watch.armed = false
		watch.mu.Unlock()
		return nil, err
	}
	requested := time.Now().UTC()
	made.backup = backup
	rec.BackupUID, rec.RequestedAt = string(backup.UID), &requested
	tctx, cancel := context.WithCancel(ctx)
	triggered := make(chan struct{})
	go func() {
		defer close(triggered)
		select {
		case <-watch.seen:
			h.triggerStop(tctx, watch, rec)
		case <-tctx.Done():
		}
	}()
	backup, waitErr := h.waitBackupWith(ctx, backup, &rec.Phases, rec.Credentials,
		func(ctx context.Context, read *databasev1alpha1.CubridBackup) error {
			return h.gracefulStopReading(ctx, read, rec, made, watch)
		})
	watch.mu.Lock()
	watch.armed = false
	fired := watch.fired
	watch.mu.Unlock()
	if !fired {
		cancel()
	}
	<-triggered
	cancel()
	t := rec.Trigger
	if t.SeenAt != nil && t.DeleteAcceptedAt != nil {
		result.Measurements[measBackupdbSeenAfterRequest] = t.SeenAt.Sub(requested).Round(time.Millisecond).String()
		result.Measurements[measDeleteAfterSeen] = t.DeleteAcceptedAt.Sub(*t.SeenAt).Round(time.Millisecond).String()
	}
	// The first reading of the phase the backup ended in is the last phase
	// recorded: a phase is recorded when it changes.
	if s := backup.Status.Phase; waitErr == nil && t.DeleteAcceptedAt != nil && len(rec.Phases) > 0 &&
		(s == databasev1alpha1.BackupPhaseCompleted || s == databasev1alpha1.BackupPhaseFailed) {
		rec.EndedAfterDelete = rec.Phases[len(rec.Phases)-1].At.After(*t.DeleteAcceptedAt)
	}
	return backup, waitErr
}

// triggerStop deletes the member's Pod with its grace period and its UID as
// a precondition, and records when the sampler first saw backupdb and when
// the deletion was sent and accepted. It writes the trigger with watch.mu
// held: the wait for the backup reads it meanwhile.
func (h *haRun) triggerStop(ctx context.Context, watch *stopWatch, rec *s11GracefulStopRecord) {
	t := rec.Trigger
	watch.mu.Lock()
	for _, sample := range watch.samples {
		if p := sample.backupdbProcess(); p != "" {
			at := sample.Received
			t.SeenAt, t.SeenProcess = &at, p
			break
		}
	}
	podUID := t.PodUID
	watch.mu.Unlock()
	failed := func(err error) {
		watch.mu.Lock()
		defer watch.mu.Unlock()
		t.DeleteError = h.redactError(err)
	}
	if err := h.guard(ctx); err != nil {
		failed(err)
		return
	}
	uid := types.UID(podUID)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: s11GracefulStopMember, Namespace: h.cfg.Namespace}}
	issued := time.Now().UTC()
	watch.mu.Lock()
	t.DeleteIssuedAt = &issued
	watch.mu.Unlock()
	if err := h.Client.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil {
		failed(err)
		return
	}
	accepted := time.Now().UTC()
	watch.mu.Lock()
	t.DeleteAcceptedAt = &accepted
	watch.mu.Unlock()
}

// gracefulStopReading acts on one status read of the backup while the run
// waits for it. It records an artifact on a backup that is not Completed and
// the operationRefs read. A reading that has left Pending and Running before
// the sampler saw backupdb disarms the trigger and ends the wait: the
// placement was missed. A target other than the member ends it too. After
// the deletion, the member's replacement is looked at once per reading
// (replacementReady).
func (h *haRun) gracefulStopReading(ctx context.Context, backup *databasev1alpha1.CubridBackup,
	rec *s11GracefulStopRecord, made *s11InterruptedMade, watch *stopWatch) error {
	s := backup.Status
	rec.LastPhase = string(s.Phase)
	if s.Artifact != nil && s.Phase != databasev1alpha1.BackupPhaseCompleted {
		rec.ArtifactsNotDone = append(rec.ArtifactsNotDone, s11ArtifactReading{At: time.Now().UTC(),
			Phase: string(s.Phase), URI: s.Artifact.URI})
	}
	if s.OperationRef != "" {
		if rec.OperationRef == "" {
			rec.OperationRef = s.OperationRef
		}
		if !slices.Contains(rec.OperationRefsSeen, s.OperationRef) {
			rec.OperationRefsSeen = append(rec.OperationRefsSeen, s.OperationRef)
		}
	}
	if s.TargetInstance != "" && s.TargetInstance != s11GracefulStopMember {
		return faultStep{fmt.Errorf("the backup's target is %s, not %s, the member the sampler watches",
			s.TargetInstance, s11GracefulStopMember)}
	}
	watch.mu.Lock()
	if !watch.fired && s.Phase != databasev1alpha1.BackupPhasePending &&
		s.Phase != databasev1alpha1.BackupPhaseRunning && s.Phase != "" {
		watch.armed = false
		rec.Trigger.Missed = fmt.Sprintf("the backup was %s before the sampler showed backupdb in %d reading(s)",
			s.Phase, len(watch.samples))
	}
	missed, accepted, refused := rec.Trigger.Missed, rec.Trigger.DeleteAcceptedAt != nil, rec.Trigger.DeleteError
	watch.mu.Unlock()
	if missed != "" {
		return faultStep{errors.New("placement missed: " + missed)}
	}
	if refused != "" {
		return faultStep{errors.New("the sampler showed backupdb, but the member's Pod could not be deleted: " +
			refused)}
	}
	if accepted && rec.Trigger.ReadyAgainAt == nil {
		if _, err := h.replacementReady(ctx, made.cluster, rec); err != nil {
			return err
		}
	}
	return nil
}

// replacementReady reports whether the StatefulSet's replacement of the
// deleted Pod runs a ready database container and a reading of the cluster
// after that shows Ready. The deleted Pod, while it terminates, and no Pod
// at all are waited for; a Pod that the StatefulSet does not control, or a
// cluster that was replaced, is an error. Every status of the cluster read
// is searched for a credential; what does not hold yet is recorded in
// ReadyWaiting.
func (h *haRun) replacementReady(ctx context.Context, created *databasev1alpha1.CubridCluster,
	rec *s11GracefulStopRecord) (bool, error) {
	t := rec.Trigger
	pod := &corev1.Pod{}
	err := h.Client.Get(ctx, client.ObjectKey{Namespace: h.cfg.Namespace, Name: s11GracefulStopMember}, pod)
	switch {
	case apierrors.IsNotFound(err):
		t.ReadyWaiting = "the StatefulSet has not created the replacement Pod yet"
		return false, nil
	case err != nil:
		t.ReadyWaiting = "the Pod" + unread + h.redactError(err)
		return false, nil
	case string(pod.UID) == t.PodUID:
		t.ReadyWaiting = "the deleted Pod is still terminating"
		return false, nil
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.Kind != kindStatefulSet || owner.Name != created.Name {
		return false, fmt.Errorf("the Pod %s that replaced the deleted one is not controlled by the StatefulSet of "+
			"%s", pod.Name, created.Name)
	}
	if t.ReplacementUID != string(pod.UID) {
		at := time.Now().UTC()
		t.ReplacementUID, t.ReplacementAt = string(pod.UID), &at
	}
	status := databaseContainerStatus(pod)
	if status == nil || !status.Ready {
		t.ReadyWaiting = "the replacement's database container is not ready"
		return false, nil
	}
	cluster := &databasev1alpha1.CubridCluster{}
	if err := h.Client.Get(ctx, client.ObjectKeyFromObject(created), cluster); err != nil {
		t.ReadyWaiting = "the cluster" + unread + h.redactError(err)
		return false, nil
	}
	if cluster.UID != created.UID {
		return false, fmt.Errorf("the cluster %s was replaced", created.Name)
	}
	rec.Credentials.StatusesChecked++
	if err := h.lookForCredentials(&rec.Credentials.FoundIn, "the status read of "+created.Name,
		cluster.Status); err != nil {
		return false, err
	}
	unmet, err := standaloneReady(cluster)
	if err != nil || unmet != "" {
		t.ReadyWaiting = unmet
		return false, err
	}
	at := time.Now().UTC()
	t.ReadyAgainAt, t.ReadyWaiting = &at, ""
	if c := meta.FindStatusCondition(cluster.Status.Conditions, clusterReadyCondition); c != nil {
		transition := c.LastTransitionTime.UTC()
		t.ReadyTransitionAt = &transition
	}
	return true, nil
}

// waitReplacementReady waits, until the ready limit from the deletion's
// acceptance has passed, for the replacement to be ready
// (replacementReady), and requires it within the limit: a member not
// recreated or a cluster not Ready again by then fails the variant.
func (h *haRun) waitReplacementReady(ctx context.Context, cluster *databasev1alpha1.CubridCluster,
	rec *s11GracefulStopRecord, result *evidence.Scenario) error {
	t, limit := rec.Trigger, h.cfg.BackupGracefulStopReadyLimit
	deadline := t.DeleteAcceptedAt.Add(limit)
	if t.ReadyAgainAt == nil {
		wctx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		err := pollUntil(wctx, h.cfg.Poll, func() (bool, error) {
			if err := h.guard(wctx); err != nil {
				return false, err
			}
			return h.replacementReady(wctx, cluster, rec)
		})
		if err != nil && ctx.Err() != nil {
			return err
		}
		if err != nil && wctx.Err() == nil {
			return err
		}
	}
	if t.ReplacementAt != nil {
		measureReplacement(rec, result)
	}
	switch {
	case t.ReplacementUID == "":
		return backupVerdict{fmt.Errorf("the StatefulSet did not replace the deleted Pod %s within the ready "+
			"limit %s: %s", s11GracefulStopMember, limit, cmp.Or(t.ReadyWaiting, "no reading was taken"))}
	case t.ReadyAgainAt == nil:
		return backupVerdict{fmt.Errorf("%s was not Ready again within the ready limit %s after its member's "+
			"Pod was deleted: %s", cluster.Name, limit, cmp.Or(t.ReadyWaiting, "no reading was taken"))}
	case t.ReadyAgainAt.After(deadline):
		return backupVerdict{fmt.Errorf("time limit exceeded: the cluster read Ready %s after the deletion, "+
			"later than the ready limit %s", t.ReadyAgainAt.Sub(*t.DeleteAcceptedAt).Round(time.Millisecond),
			limit)}
	}
	return nil
}

// measureReplacement records the time from the deletion to the first
// reading of the replacement and of the cluster Ready again.
func measureReplacement(rec *s11GracefulStopRecord, result *evidence.Scenario) {
	t := rec.Trigger
	result.Measurements[measReplacedWithin] = t.ReplacementAt.Sub(*t.DeleteAcceptedAt).Round(time.Millisecond).
		String()
	if t.ReadyAgainAt != nil {
		t.ReadyAgainWithin = t.ReadyAgainAt.Sub(*t.DeleteAcceptedAt).Round(time.Millisecond).String()
		result.Measurements[measReadyAgainWithin] = t.ReadyAgainWithin
	}
	t.ReadyUnchanged = readyUnchanged(t)
}

// readyUnchanged says, when the Ready condition's lastTransitionTime in the
// reading that found the cluster Ready again is before the second of the
// deletion's acceptance, that Ready never went False through the
// replacement; it is empty otherwise. lastTransitionTime has whole seconds:
// a transition in the deletion's second counts as after it.
func readyUnchanged(t *s11StopTrigger) string {
	if t.ReadyTransitionAt == nil || t.DeleteAcceptedAt == nil ||
		!t.ReadyTransitionAt.Before(t.DeleteAcceptedAt.Truncate(time.Second)) {
		return ""
	}
	return fmt.Sprintf("the cluster's Ready lastTransitionTime %s is before the deletion was accepted at %s: Ready "+
		"never went False while the member was replaced, as the Operator observed it",
		t.ReadyTransitionAt.Format(time.RFC3339), t.DeleteAcceptedAt.Format(time.RFC3339Nano))
}

// judgeStopBackup judges the backup as the wait left it.
//
// The deletion runs the Pod's preStop hook, instance-manager shutdown, which
// asks the Instance Manager to stop (preStopShutdown in
// internal/controller/cubridcluster_controller.go, shutdown in
// internal/instancemanager/server.go, with the 100 s Shutdown budget inside
// the Pod's 120 s grace period). Stop cancels every operation's context and
// waits for them before it runs heartbeat stop and server stop (Stop in
// internal/instancemanager/operations.go). The cancellation kills
// backupdb's process group, and Run returns once the group is gone
// (internal/instancemanager/cli.go). runBackup then fails the operation:
// stoppingReason puts "the member is stopping: " before "backupdb failed:
// ...", awaitStagingWriters waits up to 30 s while cub_server still runs
// until no process holds a staging file, and only then is the staging
// removed and the operation recorded Failed; a holder left after the bound
// keeps the staging and adds that to the reason. The record is on the data
// volume, so the Operator reads it from the old manager or the
// replacement's, and copies the reason into the backup's Ready message
// (failedBackup in internal/controller/cubridbackup_controller.go).
//
// So once the deletion was accepted and the backup was first read ended
// after it (EndedAfterDelete): Failed must be BackupFailed with a reason that
// starts with stopReasonPrefix and does not say that the staging is kept. A
// reason of an upload, or of a backupdb that succeeded, shows a stop that
// reached the manager after backupdb had ended, and blocks the variant, as
// does Completed when the member's log shows backupdb's own success: the
// placement was missed, and the Completed backup is its own. Completed when
// the log shows backupdb end for the stop (Command failed), a
// status.artifact on a reading that is not Completed, and another
// operationRef are never-events; Completed when the log shows no end of
// backupdb cannot be judged and blocks it. RecordedOperationLost is the
// documented escalation and blocks it; any other failure fails it. A backup
// that left Running before the sampler saw backupdb, a deletion that was
// refused, or a backup read ended before the deletion was accepted, blocks
// it.
func judgeStopBackup(backup *databasev1alpha1.CubridBackup, waitErr error, requested time.Time,
	rec *s11GracefulStopRecord, result *evidence.Scenario) error {
	s, t := backup.Status, rec.Trigger
	rec.Artifact = s.Artifact
	completed := s.Phase == databasev1alpha1.BackupPhaseCompleted
	if completed || s.Phase == databasev1alpha1.BackupPhaseFailed {
		rec.Failure = readyReason(backup)
	}
	deleted := t.DeleteAcceptedAt != nil
	result.Measurements[measOperationRefUnchanged] = len(rec.OperationRefsSeen) <= 1
	if events := stopBackupEvents(s, rec); len(events) > 0 {
		result.NeverEvents = append(result.NeverEvents, events...)
		errs := make([]error, 0, len(events))
		for _, event := range events {
			errs = append(errs, backupVerdict{errors.New(event)})
		}
		return errors.Join(errs...)
	}
	if waitErr != nil {
		if errors.As(waitErr, new(backupVerdict)) || errors.As(waitErr, new(faultStep)) {
			return waitErr
		}
		return fmt.Errorf("read the CubridBackup: %w", waitErr)
	}
	switch {
	case !deleted && t.DeleteError != "":
		return fmt.Errorf("the sampler showed backupdb, but the member's Pod could not be deleted: %s; the backup "+
			"ended %s", t.DeleteError, rec.Failure)
	case !deleted:
		return fmt.Errorf("placement missed: the backup ended %s (%s) before the sampler showed backupdb",
			s.Phase, rec.Failure)
	case !rec.EndedAfterDelete:
		return fmt.Errorf("the backup was read %s before its member's Pod was deleted, so the stop cannot have "+
			"ended it: %s", s.Phase, rec.Failure)
	case completed && rec.Log.BackupdbResult == logRanCommand:
		return fmt.Errorf("placement missed: the backup ended Completed, and the member's log shows backupdb "+
			"succeed before the stop reached the Instance Manager: %s", rec.Failure)
	case completed:
		return fmt.Errorf("the backup ended Completed after its member's Pod was deleted, but the member's log "+
			"shows no end of backupdb, so whether the stop came before it is not known: %s", rec.Failure)
	}
	c := meta.FindStatusCondition(s.Conditions, backupReady)
	message := ""
	if c != nil {
		message = c.Message
	}
	rest := strings.TrimPrefix(message, memberStopping)
	switch {
	case c != nil && c.Reason == s11OperationLostReason:
		return fmt.Errorf("the backup reached the documented escalation %s after its member's Pod was deleted, an "+
			"unknown outcome: %s", s11OperationLostReason, rec.Failure)
	case c == nil || c.Reason != s11MissingReason:
		return backupVerdict{fmt.Errorf("the backup ended Failed after its member's Pod was deleted while backupdb "+
			"ran, but not with %s: %s", s11MissingReason, rec.Failure)}
	case strings.Contains(message, stagingKept) && strings.Contains(message, stagingKeptEnd):
		return backupVerdict{fmt.Errorf("the backup ended Failed with a kept staging: a process still held a "+
			"staging file after the Instance Manager's wait: %s", rec.Failure)}
	case strings.HasPrefix(message, stopReasonPrefix):
	case strings.HasPrefix(rest, uploadFailed) || strings.HasPrefix(rest, backupdbSucceeded):
		return fmt.Errorf("placement missed: the stop reached the Instance Manager after backupdb had ended, so "+
			"the backup failed after it: %s", rec.Failure)
	default:
		return backupVerdict{fmt.Errorf("the backup ended Failed after its member's Pod was deleted while backupdb "+
			"ran, but its reason does not start with %q: %s", stopReasonPrefix, rec.Failure)}
	}
	failed := rec.Phases[len(rec.Phases)-1].At
	rec.FailedWithin = failed.Sub(requested).Round(time.Millisecond).String()
	rec.FailedAfterDelete = failed.Sub(*t.DeleteAcceptedAt).Round(time.Millisecond).String()
	result.Measurements[failedWithinKey] = rec.FailedWithin
	result.Measurements[measFailedAfterDelete] = rec.FailedAfterDelete
	return nil
}

// stopBackupEvents returns the never-events of the backup as the wait left
// it, s: another operationRef, a status.artifact on a reading that was not
// Completed, and Completed, with its artifact, after the deletion when the
// member's log shows backupdb end for the stop.
func stopBackupEvents(s databasev1alpha1.CubridBackupStatus, rec *s11GracefulStopRecord) []string {
	var events []string
	if len(rec.OperationRefsSeen) > 1 {
		events = append(events, fmt.Sprintf("the backup's operationRef was %s, then %s: a second operation",
			rec.OperationRefsSeen[0], strings.Join(rec.OperationRefsSeen[1:], ", ")))
	}
	for _, a := range rec.ArtifactsNotDone {
		event := "status.artifact names " + a.URI + " for a backup of " + rec.Cluster.Name + " in phase " + a.Phase
		if !slices.Contains(events, event) {
			events = append(events, event)
		}
	}
	if s.Phase == databasev1alpha1.BackupPhaseCompleted && rec.EndedAfterDelete &&
		rec.Log.BackupdbResult == logCommandFailed {
		events = append(events, "the backup of "+rec.Cluster.Name+" ended Completed after its member's Pod was "+
			"deleted and the member's log shows backupdb end for the stop")
		if s.Artifact != nil {
			events = append(events, "status.artifact names "+s.Artifact.URI+" for a backup of "+rec.Cluster.Name+
				" whose member was stopped while backupdb ran")
		}
	}
	return events
}

// judgeStopSamples finds, in the sampler's readings, the first that showed
// the operation's staging directory and the first after it that did not,
// and requires that no reading from that one on shows a process holding a
// file below the backup's staging destination. It records the last holder
// before. A staging directory never seen gone blocks the variant.
func judgeStopSamples(watch *stopWatch, rec *s11GracefulStopRecord, result *evidence.Scenario) error {
	watch.mu.Lock()
	samples := slices.Clone(watch.samples)
	watch.mu.Unlock()
	s := rec.Sampler
	destination := imBackupStagingRoot + "/" + rec.BackupUID
	s.StagingDir = rec.BackupUID + "/" + rec.OperationRef
	seen, gone := -1, -1
	for i, sample := range samples {
		present := slices.Contains(sample.Entries, s.StagingDir)
		if seen < 0 && present {
			seen = i
			at := sample.Received
			s.StagingSeenAt = &at
		}
		if seen >= 0 && !present {
			gone = i
			at := sample.Received
			s.StagingGoneAt = &at
			break
		}
	}
	holds := func(sample stopSample) []string {
		var held []string
		for _, holder := range sample.Holders {
			_, path, _ := strings.Cut(holder, " ")
			if path == destination || strings.HasPrefix(path, destination+"/") {
				held = append(held, holder)
			}
		}
		return held
	}
	end := len(samples)
	if gone >= 0 {
		end = gone
	}
	visible := false
	for _, sample := range samples[:end] {
		visible = visible || len(sample.Holders) > 0 || len(sample.FdListed) > 0
		if held := holds(sample); len(held) > 0 {
			at := sample.Received
			s.LastHolder, s.LastHolderAt = held[len(held)-1], &at
		}
	}
	if !visible {
		// No holder and no listing of a cub* process's open files: an empty
		// answer of ls would pass the check below without showing anything.
		before := "the end of its output"
		if s.StagingGoneAt != nil {
			before = "the staging directory was gone"
		}
		return fmt.Errorf("the sampler could not see open files: no reading before %s showed an open file below "+
			"the staging root or the open files of a cub* process", before)
	}
	if gone < 0 {
		why := "its output ended"
		if s.Truncated {
			why = "its output reached the run's limit"
		}
		if seen < 0 {
			return fmt.Errorf("the sampler never showed the staging directory %s before %s, so whether a process "+
				"held a staging file once it was gone is not known", s.StagingDir, why)
		}
		return fmt.Errorf("the sampler did not show the staging directory %s gone before %s, so whether a "+
			"process held a staging file once it was gone is not known", s.StagingDir, why)
	}
	if t := rec.Trigger; t.DeleteAcceptedAt != nil {
		result.Measurements[measStagingGoneAfterDelete] = s.StagingGoneAt.Sub(*t.DeleteAcceptedAt).
			Round(time.Millisecond).String()
	}
	for _, sample := range samples[gone:] {
		for _, held := range holds(sample) {
			s.HoldersAfterGone = append(s.HoldersAfterGone, sample.Received.Format(time.RFC3339Nano)+" "+held)
		}
	}
	result.Measurements[measHoldersAfterGone] = len(s.HoldersAfterGone)
	if len(s.HoldersAfterGone) > 0 {
		event := fmt.Sprintf("a process held a file below %s at or after %s, when the operation's staging "+
			"directory was gone: %s", destination, s.StagingGoneAt.Format(time.RFC3339Nano),
			strings.Join(s.HoldersAfterGone, "; "))
		result.NeverEvents = append(result.NeverEvents, event)
		return backupVerdict{errors.New(event)}
	}
	return nil
}

// stopLogLine is the part of one line of the Instance Manager's JSON log the
// variant reads.
type stopLogLine struct {
	Msg         string `json:"msg"`
	Event       string `json:"event"`
	Command     string `json:"command"`
	OperationID string `json:"operationID"`
	Reason      string `json:"reason"`
	Err         string `json:"err"`
	Time        string `json:"time"`
	DurationMs  *int64 `json:"durationMs"`
}

// readStopLog scans the followed log (scanStopLog) and measures backupdb's
// duration and the time from the deletion to the first line the stop
// caused.
func readStopLog(data []byte, rec *s11GracefulStopRecord, result *evidence.Scenario) ([]int, bool) {
	l := rec.Log
	writers, kept := scanStopLog(data, rec)
	if l.BackupdbDurationMs != nil {
		result.Measurements[measBackupdbDurationMs] = *l.BackupdbDurationMs
	}
	if t := rec.Trigger.DeleteAcceptedAt; l.StopLoggedAt != nil && t != nil {
		l.StopLoggedAfterDelete = l.StopLoggedAt.Sub(*t).Round(time.Millisecond).String()
		result.Measurements[measStopLoggedAfterDelete] = l.StopLoggedAfterDelete
	}
	return writers, kept
}

// judgeStopLog finds in the followed log of the member's database container
// backupdb's end for the backup's staging destination, the operation's
// staging writer events and Operation failed, and the shutdown's heartbeat
// stop and server stop (readStopLog), and requires the staging settled
// before CUBRID is stopped: backupdb's end, then the writer events, then
// Operation failed, then heartbeat stop and server stop. A backupdb that
// succeeded blocks the variant: the stop came after it, and nothing else of
// the log is judged. A missing line, other than server stop, blocks it: the
// log is incomplete. A line out of order, or a writer wait that ended with
// the staging kept, fails it.
func judgeStopLog(data []byte, rec *s11GracefulStopRecord, result *evidence.Scenario) error {
	l := rec.Log
	writers, kept := readStopLog(data, rec, result)
	if l.BackupdbResult == logRanCommand {
		return fmt.Errorf("placement missed: the member's log shows backupdb succeed (line %d) before the stop "+
			"reached the Instance Manager", l.BackupdbLine)
	}
	errs := []error{stopLogGaps(rec)}
	order := stopLogOrder(l, writers)
	if kept {
		order = append(order, "a staging writer wait that ended with the staging kept ("+logWriterTimeout+")")
	}
	for _, o := range order {
		event := "the member's log holds " + o
		result.NeverEvents = append(result.NeverEvents, event)
		errs = append(errs, backupVerdict{errors.New(event)})
	}
	return errors.Join(errs...)
}

// scanStopLog records in rec.Log the first line of each kind judgeStopLog
// orders, and returns the line numbers of the operation's staging writer
// events and whether one of them was a wait that ended with the staging
// kept.
func scanStopLog(data []byte, rec *s11GracefulStopRecord) ([]int, bool) {
	l := rec.Log
	destination := " -D " + imBackupStagingRoot + "/" + rec.BackupUID
	var writers []int
	kept := false
	for i, raw := range bytes.Split(data, []byte("\n")) {
		var line stopLogLine
		if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &line) != nil {
			continue
		}
		n := i + 1
		l.Lines++
		own := rec.OperationRef != "" && line.OperationID == rec.OperationRef
		ours := strings.HasPrefix(line.Command, backupdbCommand) && strings.Contains(line.Command, destination)
		if l.StopLine == 0 && isStopLine(line, ours, own) {
			l.StopLine = n
			if at, err := time.Parse(time.RFC3339Nano, line.Time); err == nil {
				at = at.UTC()
				l.StopLoggedAt = &at
			}
		}
		switch {
		case l.BackupdbLine == 0 && ours && (line.Msg == logCommandFailed || line.Msg == logRanCommand):
			l.BackupdbLine, l.BackupdbResult, l.BackupdbDurationMs = n, line.Msg, line.DurationMs
		case own && slices.Contains([]string{logWriterWait, logWriterClosed, logWriterTimeout}, line.Event):
			writers = append(writers, n)
			l.WriterEvents = append(l.WriterEvents, fmt.Sprintf("%d %s", n, line.Event))
			kept = kept || line.Event == logWriterTimeout
		case l.OperationFailedLine == 0 && own && line.Msg == logOperationFailed:
			l.OperationFailedLine, l.OperationFailedReason = n, line.Reason
		case line.Event != logEventCommand:
		case l.HeartbeatStopLine == 0 && line.Command == heartbeatStop:
			l.HeartbeatStopLine, l.HeartbeatStopResult = n, line.Msg
		case l.ServerStopLine == 0 && strings.HasPrefix(line.Command, serverStop):
			l.ServerStopLine, l.ServerStopResult = n, line.Msg
		}
	}
	return writers, kept
}

// isStopLine reports whether a line of the member's log was caused by the
// stop: backupdb's failure for the backup (ours), a staging writer event of
// the operation (own), a reason or error that says the member is stopping,
// or a command of the shutdown. The Instance Manager logs nothing when a stop
// begins, so the first of them is the earliest sign of it in the log.
func isStopLine(line stopLogLine, ours, own bool) bool {
	return (ours && line.Msg == logCommandFailed) ||
		(own && slices.Contains([]string{logWriterWait, logWriterClosed, logWriterTimeout}, line.Event)) ||
		strings.Contains(line.Reason, errStoppingText) || strings.Contains(line.Err, errStoppingText) ||
		(line.Event == logEventCommand && (line.Command == heartbeatStop || strings.HasPrefix(line.Command, serverStop)))
}

// stopLogGaps names the lines judgeStopLog needs that the log does not hold.
func stopLogGaps(rec *s11GracefulStopRecord) error {
	l := rec.Log
	var missing []string
	if l.BackupdbLine == 0 {
		missing = append(missing, "backupdb's end for "+imBackupStagingRoot+"/"+rec.BackupUID)
	}
	if l.OperationFailedLine == 0 {
		missing = append(missing, logOperationFailed+" for "+rec.OperationRef)
	}
	if l.HeartbeatStopLine == 0 {
		missing = append(missing, "the shutdown's "+heartbeatStop)
	}
	if len(missing) == 0 {
		return nil
	}
	cut := ""
	if l.Truncated {
		cut = ", which reached the run's limit,"
	}
	return fmt.Errorf("the member's followed log%s is incomplete: it holds no %s", cut,
		strings.Join(missing, ", no "))
}

// stopLogOrder names each pair of the lines found that is out of order.
func stopLogOrder(l *s11StopLog, writers []int) []string {
	var order []string
	for _, w := range writers {
		if l.BackupdbLine != 0 && w < l.BackupdbLine {
			order = append(order, fmt.Sprintf("a staging writer event (line %d) before backupdb's end (line %d)", w,
				l.BackupdbLine))
		}
		if l.OperationFailedLine != 0 && w > l.OperationFailedLine {
			order = append(order, fmt.Sprintf("a staging writer event (line %d) after %s (line %d)", w,
				logOperationFailed, l.OperationFailedLine))
		}
	}
	if l.OperationFailedLine == 0 {
		return order
	}
	if l.BackupdbLine != 0 && l.OperationFailedLine < l.BackupdbLine {
		order = append(order, fmt.Sprintf("%s (line %d) before backupdb's end (line %d)", logOperationFailed,
			l.OperationFailedLine, l.BackupdbLine))
	}
	for _, stop := range []struct {
		command string
		line    int
	}{{heartbeatStop, l.HeartbeatStopLine}, {serverStop, l.ServerStopLine}} {
		if stop.line != 0 && stop.line < l.OperationFailedLine {
			order = append(order, fmt.Sprintf("%s (line %d) before the operation's %s (line %d)", stop.command,
				stop.line, logOperationFailed, l.OperationFailedLine))
		}
	}
	return order
}

// gracefulStopBackupAtEnd reads the backup once more, after the prefix was
// listed, so that a backup driven on after the wait is not missed. When the
// wait saw it end after the deletion, Completed, a status.artifact or
// another operationRef then must never happen; otherwise either of the
// first two is recorded and blocks the variant.
func (h *haRun) gracefulStopBackupAtEnd(ctx context.Context, created *databasev1alpha1.CubridBackup,
	rec *s11GracefulStopRecord, result *evidence.Scenario) error {
	s, err := h.readBackupAtEnd(ctx, created)
	if err != nil {
		return err
	}
	rec.PhaseAtEnd, rec.ArtifactAtEnd = string(s.Phase), s.Artifact
	completed := s.Phase == databasev1alpha1.BackupPhaseCompleted
	endedForStop := rec.EndedAfterDelete && rec.Log.BackupdbResult == logCommandFailed
	switch {
	case rec.LastPhase == string(databasev1alpha1.BackupPhaseCompleted),
		rec.Log.BackupdbResult == logRanCommand:
		// The judgement named it already, or the placement was missed and a
		// Completed backup is its own.
		return nil
	case !endedForStop && !completed && s.Artifact == nil:
		return nil
	case !endedForStop:
		return fmt.Errorf("the CubridBackup read at the end is %s with status.artifact %t, and the wait did not see "+
			"it end after the stop ended backupdb: recorded, and S11 graceful-stop cannot be judged", s.Phase,
			s.Artifact != nil)
	}
	var events []string
	if completed {
		events = append(events, "the backup of "+rec.Cluster.Name+", seen "+rec.LastPhase+" after its member's "+
			"Pod was deleted, is Completed when read again at the end")
	}
	if s.Artifact != nil {
		events = append(events, "status.artifact names "+s.Artifact.URI+" for the backup of "+rec.Cluster.Name+
			" read again at the end, after its member's Pod was deleted")
	}
	if s.OperationRef != rec.OperationRef {
		events = append(events, fmt.Sprintf("the backup's operationRef was %s, then %q when read again at the "+
			"end: a second operation", rec.OperationRef, s.OperationRef))
	}
	result.NeverEvents = append(result.NeverEvents, events...)
	errs := make([]error, 0, len(events))
	for _, event := range events {
		errs = append(errs, backupVerdict{errors.New(event)})
	}
	return errors.Join(errs...)
}

// gracefulStopCredentials looks for the object store's keys in the backup's
// status and the Events (checkS11Credentials), the cluster's status at the
// end, the sampler's output, the followed log of the deleted member, the
// replacement's log, whole, and each installed manager Pod's log from
// logSinceMargin before started. A followed log or sampler output cut at
// the run's limit, and a log that could not be read, leave the search
// incomplete.
func (h *haRun) gracefulStopCredentials(ctx context.Context, made *s11InterruptedMade,
	rec *s11GracefulStopRecord, watch *stopWatch, memberLog []byte, started time.Time) error {
	creds := rec.Credentials
	var errs []error
	if made.backup != nil {
		if err := h.checkS11Credentials(ctx, made.backup, nil, creds); err != nil {
			errs = append(errs, fmt.Errorf("look for credentials: %w", err))
		}
	}
	if made.cluster != nil {
		if err := h.standaloneAtEnd(ctx, made.cluster, creds); err != nil {
			errs = append(errs, fmt.Errorf("look for credentials: %w", err))
		}
	}
	watch.mu.Lock()
	sampled := slices.Clone(watch.output.Bytes())
	watch.mu.Unlock()
	for _, stream := range []struct {
		where     string
		data      []byte
		truncated bool
	}{{"the sampler's output in " + s11GracefulStopMember, sampled, rec.Sampler.Truncated},
		{"the followed log of the deleted member " + s11GracefulStopMember, memberLog, rec.Log.Truncated}} {
		holds, err := h.holdsCredential(stream.data)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if holds {
			creds.FoundIn = append(creds.FoundIn, stream.where)
		}
		if stream.truncated {
			errs = append(errs, fmt.Errorf("%s reached the run's limit of %d bytes and was not fully searched",
				stream.where, h.logLimit()))
		}
	}
	if creds.Logs == nil {
		creds.Logs = []s11LogCheck{}
	}
	since := started.Add(-logSinceMargin).Truncate(time.Second)
	managers := h.rec.OperatorAfter.Pods
	if len(managers) == 0 {
		errs = append(errs, errors.New("no installed manager Pod was recorded, so the Operator's log was not "+
			"searched"))
	}
	for _, m := range managers {
		errs = append(errs, h.searchLog(ctx, creds, s11LogCheck{Log: operatorLog, Namespace: h.cfg.OperatorNamespace,
			Pod: m.Pod, UID: m.UID, Container: managerContainer, ReadFrom: logFromSinceTime, SinceTime: since}, nil))
	}
	if t := rec.Trigger; t.ReplacementUID != "" && made.cluster != nil {
		errs = append(errs, h.searchLog(ctx, creds, s11LogCheck{Log: instanceManagerLog, Namespace: h.cfg.Namespace,
			Pod: s11GracefulStopMember, UID: t.ReplacementUID, Container: databaseContainer, ReadFrom: logWhole},
			func(pod *corev1.Pod) error {
				owner := metav1.GetControllerOf(pod)
				if owner == nil || owner.Kind != kindStatefulSet || owner.Name != made.cluster.Name {
					return fmt.Errorf("the Pod is not controlled by the StatefulSet of %s", made.cluster.Name)
				}
				return nil
			}))
	}
	return errors.Join(errs...)
}

// writeSamples stores the sampler's readings as JSON lines.
func (h *haRun) writeSamples(watch *stopWatch) error {
	watch.mu.Lock()
	samples := slices.Clone(watch.samples)
	watch.mu.Unlock()
	if len(samples) == 0 {
		return nil
	}
	var b bytes.Buffer
	for _, sample := range samples {
		data, err := json.Marshal(sample)
		if err != nil {
			return err
		}
		b.Write(append(data, '\n'))
	}
	return h.write(s11GracefulStopSamplesFile, b.Bytes())
}

// deleteGracefulStop deletes the CubridBackup and the cluster the run
// created, in that order and each by its UID (deleteByUID), and records each
// outcome. The cluster's data PVC is retained and goes with the namespace. It
// does not judge the variant.
func (h *haRun) deleteGracefulStop(ctx context.Context, made s11InterruptedMade, rec *s11GracefulStopRecord) bool {
	var objects []ownedObject
	if made.backup != nil {
		objects = append(objects, ownedObject{ownedBackupKind, made.backup})
	}
	if made.cluster != nil && made.cluster.UID != "" {
		objects = append(objects, ownedObject{ownedClusterKind, made.cluster})
		rec.Residue += "; the data PVC of " + made.cluster.Name + " is retained when the cluster is deleted and " +
			"goes with the namespace, with whatever the member's backup staging root holds"
	}
	if len(objects) == 0 {
		return true
	}
	outcome, cleaned := h.deleteByUID(ctx, objects)
	rec.Cleanup = outcome
	return cleaned
}

// fillerRows is how many rows of fillerRowBytes make mb MiB of filler.
func fillerRows(mb int) int {
	return mb * (1 << 20) / fillerRowBytes
}

// gracefulStopDataVolume is the data volume of the variant's cluster: the
// standalone cluster's 2Gi, and four times the filler for its pages, the log
// and archive volumes its inserts write, and the uncompressed backupdb
// staged on the same volume.
func gracefulStopDataVolume(mb int) resource.Quantity {
	q := resource.MustParse("2Gi")
	q.Add(*resource.NewQuantity(int64(4*mb)<<20, resource.BinarySI))
	return q
}

// createGracefulStop creates the standalone cluster rec names in the run
// namespace (standaloneSpec) with the run's copy of the seed Secret, and a
// data volume large enough for the filler (gracefulStopDataVolume).
func (h *haRun) createGracefulStop(ctx context.Context, rec *s11StandaloneCluster, made *s11InterruptedMade) error {
	if err := h.guard(ctx); err != nil {
		return err
	}
	spec := h.standaloneSpec(seedSecretName)
	if mb := h.cfg.BackupGracefulStopFillerMB; mb > 0 {
		spec.Storage.Data.Size = gracefulStopDataVolume(mb)
	}
	cluster := &databasev1alpha1.CubridCluster{
		ObjectMeta: metav1.ObjectMeta{Name: rec.Name, Namespace: h.cfg.Namespace}, Spec: spec}
	if err := h.Client.Create(ctx, cluster); err != nil {
		return fmt.Errorf("create the cluster %s: %w", rec.Name, err)
	}
	created := time.Now().UTC()
	made.cluster, rec.UID, rec.CreatedAt = cluster, string(cluster.UID), &created
	if cluster.UID == "" {
		return fmt.Errorf("created cluster %s has no observed UID", rec.Name)
	}
	return nil
}

// fillerCSQL runs csql as dba on the member's database, the way the run
// reads members (csqlRead), without csqlRead's second read: a statement that
// writes is never sent again by it.
const fillerCSQL = `PATH="${CUBRID}/bin:${PATH}" csql -u dba "$0" -c "$1"`

// fillerCreate, fillerSeed and fillerCount are the filler's statements that
// create its table, insert its first row and count its rows;
// fillerCopy(k) copies k rows of the table into itself.
var (
	fillerCreate = "CREATE TABLE " + fillerTable + " (pad CHAR(" + strconv.Itoa(fillerRowBytes) + ")); COMMIT;"
	fillerSeed   = "INSERT INTO " + fillerTable + " (pad) VALUES (REPEAT('x', " + strconv.Itoa(fillerRowBytes) +
		")); COMMIT;"
	fillerCount = "SELECT CAST(COUNT(*) AS VARCHAR(20)) FROM " + fillerTable + ";"
)

func fillerCopy(k int) string {
	return "INSERT INTO " + fillerTable + " (pad) SELECT pad FROM " + fillerTable + " LIMIT " + strconv.Itoa(k) +
		"; COMMIT;"
}

// fillerStatement is one statement of the filler and what csql answered.
type fillerStatement struct {
	sql      string
	out      string
	err      error
	exitText bool
}

// fillerSQL runs sql with csql in the standalone member and reports whether
// its output, or the command's error, names an error. csql has been seen to
// print -199 and -111 after a statement it had committed (POC-44), so the
// text alone does not say whether the statement took effect.
func (h *haRun) fillerSQL(ctx context.Context, sql string) fillerStatement {
	out, err := h.member(ctx, s11GracefulStopMember, "bash", "-c", fillerCSQL, databaseName+"@localhost", sql)
	_, textErr := sqlcheck.Run(func(string) (string, error) { return out, err }, sql)
	return fillerStatement{sql: sql, out: out, err: err, exitText: textErr != nil}
}

// fillerRowsNow counts the filler table's rows in a csql call of its own.
// The count is read from the result table whatever else csql printed; a
// count that cannot be read is tried once more.
func (h *haRun) fillerRowsNow(ctx context.Context) (int, error) {
	var last error
	for range 2 {
		st := h.fillerSQL(ctx, fillerCount)
		values, err := sqlcheck.Values(st.out)
		if err == nil && len(values) == 1 {
			if n, convErr := strconv.Atoi(values[0]); convErr == nil {
				return n, nil
			}
		}
		last = fmt.Errorf("the count of %s could not be read: %s", fillerTable,
			h.redactError(errors.Join(st.err, errors.New(strings.TrimSpace(st.out)))))
	}
	return 0, last
}

// fillerStep runs one statement that must bring the table to at least want
// rows, and reads the count after it. A statement counts as failed only when
// the count did not reach want: then it is run once more, and a count that
// still has not grown past before blocks the variant with the statement's
// output. An error printed by a statement whose rows were counted is
// recorded in filler.ExitErrors, not judged. It returns the count.
func (h *haRun) fillerStep(ctx context.Context, filler *s11Filler, sql string, before, want int) (int, error) {
	var outputs []string
	for try := range 2 {
		st := h.fillerSQL(ctx, sql)
		filler.Statements++
		if try > 0 {
			filler.Retries++
		}
		outputs = append(outputs, strings.TrimSpace(st.out))
		n, err := h.fillerRowsNow(ctx)
		if err != nil {
			return before, err
		}
		if n >= want || (try > 0 && n > before) {
			if st.exitText || st.err != nil {
				filler.ExitErrors++
			}
			return n, nil
		}
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
	}
	return before, fmt.Errorf("the filler table %s did not grow past %d rows after %q, run twice: %s", fillerTable,
		before, sql, h.redactError(errors.New(strings.Join(outputs, " | "))))
}

// growFiller grows the filler table in the standalone member's database
// before the request, and records its rows, the statements run, the retries,
// the statements that printed an error after their rows were counted, the
// database directory's size and how long it took. Nothing is grown when
// filler is nil. It creates the table with one CHAR(fillerRowBytes) column,
// inserts one row, and then copies at most fillerChunk rows of the table
// into itself per statement, each committed on its own and each followed by
// a count in a csql call of its own (fillerStep), until TargetRows rows are
// there. A statement that does not grow the table, run twice, or a final
// count below TargetRows, blocks the variant: backupdb might end before the
// stop. The table is in the variant's own cluster, which no data check
// reads.
func (h *haRun) growFiller(ctx context.Context, filler *s11Filler, result *evidence.Scenario) error {
	if filler == nil {
		return nil
	}
	started := time.Now().UTC()
	filler.StartedAt = &started
	defer func() {
		filler.Took = time.Since(started).Round(time.Millisecond).String()
		result.Measurements[measFillerGrowth] = filler.Took
		result.Measurements[measFillerExitErrors] = filler.ExitErrors
	}()
	create := h.fillerSQL(ctx, fillerCreate)
	filler.Statements++
	n, err := h.fillerRowsNow(ctx)
	if err != nil {
		return fmt.Errorf("create the filler table %s in %s: %s; %w", fillerTable, s11GracefulStopMember,
			h.redactError(errors.Join(create.err, errors.New(strings.TrimSpace(create.out)))), err)
	}
	if create.exitText || create.err != nil {
		filler.ExitErrors++
	}
	if n == 0 {
		if n, err = h.fillerStep(ctx, filler, fillerSeed, 0, 1); err != nil {
			return fmt.Errorf("grow the filler table %s in %s: %w", fillerTable, s11GracefulStopMember, err)
		}
	}
	for n < filler.TargetRows {
		k := min(n, fillerChunk, filler.TargetRows-n)
		if n, err = h.fillerStep(ctx, filler, fillerCopy(k), n, n+k); err != nil {
			return fmt.Errorf("grow the filler table %s in %s: %w", fillerTable, s11GracefulStopMember, err)
		}
	}
	if filler.Rows, err = h.fillerRowsNow(ctx); err != nil {
		return err
	}
	result.Measurements[measFillerRows] = filler.Rows
	filler.DatabaseDirKB, filler.DuError = h.databaseDirKB(ctx)
	if filler.Rows < filler.TargetRows {
		return fmt.Errorf("the filler table %s holds %d rows, fewer than the %d asked for", fillerTable,
			filler.Rows, filler.TargetRows)
	}
	return nil
}

// databaseDirKB is the size in KiB of the databases' directory on the
// member's data volume (du -sk), or why it could not be read.
func (h *haRun) databaseDirKB(ctx context.Context) (int64, string) {
	out, err := h.member(ctx, s11GracefulStopMember, "du", "-sk", imDatabasesDir)
	field, _, _ := strings.Cut(strings.TrimSpace(out), "\t")
	kb, convErr := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
	if err != nil || convErr != nil {
		return 0, h.redactError(errors.Join(err, convErr))
	}
	return kb, ""
}
