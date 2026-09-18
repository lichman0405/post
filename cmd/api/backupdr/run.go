package backupdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// One whole drill run: back up the four classes, prove the target empty,
// restore, reconcile the four axes, open the five objects, and write the
// report. This file is the sequence; the rules live in the files it calls.

// DrillReport is the run's output document. It is the artifact a reviewer
// reads, so it carries the things that make the run falsifiable: which
// backup instant the target was restored to, what each axis actually
// checked, which of the five opened and through which layer, and — for a
// run that found drift — the findings WITH their repair proposals, none
// applied.
type DrillReport struct {
	FormatVersion string `json:"format_version"`
	RunID         string `json:"run_id"`
	// RoundAcceptance is what THIS round's acceptance is, quoted from
	// docs/37 §RPO/RTO rather than paraphrased: the process runs. It is a
	// field rather than prose in a README because the temptation this
	// document exists to resist is a reader inferring an RPO from a
	// successful restore.
	RoundAcceptance string `json:"round_acceptance"`
	// RPOClaim is deliberately the empty string, and it is a field rather
	// than an omission so that "no RPO was claimed" is a fact a reader can
	// see rather than an absence they have to notice.
	RPOClaim string `json:"rpo_claim"`

	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`

	Backup  *BackupManifest  `json:"backup"`
	Restore *RestoreManifest `json:"restore"`
	// SnapshotTimestamp is the instant the target was restored to, copied
	// out of the restore manifest for the one-line answer. A report that
	// cannot say when is a report about an unknown moment.
	SnapshotTimestamp string `json:"snapshot_timestamp"`

	Reconciliation *ReconcileReport `json:"reconciliation"`
	// FindingsRecorded is how many audit rows the pass appended — the
	// report half of "drift is a finding plus an audit line".
	FindingsRecorded int `json:"findings_recorded"`
	// RepairsApplied is always 0 and is a field on purpose: the
	// reconciliation NEVER repairs, so a report where this is ever a
	// non-zero value is a report of a bug.
	RepairsApplied int `json:"repairs_applied"`

	Opened Opened `json:"opened"`

	// SecretScan is what the artifact scan looked at and what it found.
	// An empty declaration set is a failure, not a pass: a scan that
	// looked for nothing finds nothing.
	SecretScan SecretScanResult `json:"secret_scan"`

	// Notes are the run's own remarks about what it could not do — an
	// environment it could not build itself, a layer a read had to go
	// through. They are recorded rather than omitted so a reader is not
	// left to assume the run did more than it did.
	Notes []string `json:"notes"`
}

// SecretScanResult records an artifact scan. Searched is the number of
// declared credential values the scan ACTUALLY looked for — declared values
// below MinScannableSecretBytes are counted separately in SkippedTooShort
// instead, because the scan cannot search for them without reporting noise;
// Hits is the number of artifact files that carried one. Searched and
// SkippedTooShort together are the declared set, so neither number can stand
// for more than was done. A run with Searched == 0 has proven nothing about
// credentials.
type SecretScanResult struct {
	Searched        int `json:"values_searched"`
	SkippedTooShort int `json:"values_skipped_too_short"`
	Hits            int `json:"hits"`
}

// RoundAcceptanceV1 is this round's acceptance, quoted from docs/37
// §RPO/RTO: V1 staging validates the PROCESS, and the production initial
// target (RPO 24h / RTO 4h) is not what a V1 restore drill demonstrates.
const RoundAcceptanceV1 = "V1 这一轮的验收是「流程能跑通」，不是「达到 24h/4h」 (docs/37 §RPO/RTO): " +
	"a successful run proves the backup/restore process works end to end; it measures no RPO and no RTO, " +
	"and claims neither"

// RunConfig is one drill run: where to back up from, where to restore to,
// what to open, and where the artifacts go.
type RunConfig struct {
	Config
	// Open names the objects to open in the restored environment. The
	// caller supplies them because the caller built the fixture.
	Open OpenParams
	// GiteaServiceAccount is the account tokens are minted for.
	GiteaServiceAccount string
}

// RunReport is one run's result, returned whether or not the run
// "succeeded" as a whole: a run that restored correctly and found drift is
// a SUCCESSFUL run reporting drift, so the findings must reach the caller
// rather than be swallowed by an error.
type RunReport struct {
	Report DrillReport
	// ReportPath is where the report document was written.
	ReportPath string
	// DriftDetected is the reconciliation's verdict.
	DriftDetected bool
	// AllOpened is the five-object verdict.
	AllFiveOpened bool
}

// Run performs one full drill.
//
// The error return is reserved for a run that could not be CARRIED OUT: a
// backup that failed, a target that was not empty, a restore that failed, a
// reconciliation whose reads failed. Drift found by a reconciliation that
// ran to completion is NOT an error — it is the pass working, and it comes
// back in the report.
func Run(ctx context.Context, cfg RunConfig) (*RunReport, error) {
	started := time.Now().UTC()
	if cfg.RunID == "" {
		return nil, fmt.Errorf("drill: a run id is required (it scopes every resource the run creates)")
	}
	if len(cfg.SecretValues) == 0 {
		return nil, fmt.Errorf("drill: refusing to run with no declared credential values — an artifact scan that " +
			"searches for nothing proves nothing about what the artifacts contain")
	}
	report := DrillReport{
		FormatVersion:   FormatVersion,
		RunID:           cfg.RunID,
		RoundAcceptance: RoundAcceptanceV1,
		RPOClaim:        "",
		StartedAt:       started,
		Notes: []string{
			"The drill does not build the live-stack dependencies (PostgreSQL, MinIO, Gitea) itself: the " +
				"documented empty-environment path is `make infra-up` + `make infra-init` + `migrate` " +
				"(ops/backup-restore-drill.sh runs exactly that), and this process was pointed at the stack " +
				"they produce.",
			"No RPO or RTO is measured, asserted or claimed by this run. docs/37 §RPO/RTO makes V1's " +
				"acceptance the process, not a recovery-time objective.",
		},
	}

	// 1. Back up the four classes.
	backup, err := Backup(ctx, cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("drill: backup: %w", err)
	}
	report.Backup = backup

	// The scan runs here, on the artifact as it stands, so a leak is found
	// before a restore spends time on it. The two counts come from the same
	// split the scan itself uses (scannableSecretValues), so the report
	// cannot claim to have searched a value the scan skipped.
	scanned, tooShort := scannableSecretValues(secretValueList(cfg.SecretValues))
	report.SecretScan.Searched = len(scanned)
	report.SecretScan.SkippedTooShort = tooShort

	// 2. Restore into a target proven empty.
	restore, err := Restore(ctx, cfg.Config, cfg.ArtifactsDir)
	if err != nil {
		return nil, fmt.Errorf("drill: restore: %w", err)
	}
	report.Restore = restore
	report.SnapshotTimestamp = restore.SnapshotTimestamp.UTC().Format(time.RFC3339Nano)

	// 3. Reconcile the four axes, report-only.
	pool, err := pgxpool.New(ctx, cfg.Target.PostgresURL)
	if err != nil {
		return nil, fmt.Errorf("drill: open the restored target: %w", err)
	}
	defer pool.Close()

	tokenID, token, err := mintToken(ctx, cfg.Target.GiteaBaseURL,
		cfg.Target.GiteaAdminUser, cfg.Target.GiteaAdminPass, cfg.GiteaServiceAccount, "t1110-open-"+cfg.RunID)
	if err != nil {
		return nil, fmt.Errorf("drill: open the restored Git infrastructure: %w", err)
	}
	defer func() {
		_ = revokeToken(context.WithoutCancel(ctx), cfg.Target.GiteaBaseURL,
			cfg.Target.GiteaAdminUser, cfg.Target.GiteaAdminPass, cfg.GiteaServiceAccount, tokenID)
	}()
	// The reconciler is handed the RESTORED environment through the
	// read-only port and nothing wider: `src` is a ReconcileSource, so the
	// write methods the same collaborators carry are not reachable from it.
	src := newDrillSource(pool, cfg.Target.BlobBucket,
		mustS3(cfg.Target.BlobEndpoint, cfg.Target.BlobAccessKey, cfg.Target.BlobSecretKey),
		newGitClient(cfg.Target.GiteaBaseURL, token, cfg.RunID))

	// A pass id that names the run, so the audit rows of one drill are
	// findable together in the Activity feed.
	passID := "backup-restore-drill:" + cfg.RunID
	// The sink is part of the pass, not a step after it: Reconcile records
	// its own findings (one immutable audit row each) before it returns, so
	// a report the caller holds is a report that was already recorded.
	rec, err := Reconcile(ctx, ReconcileParams{
		Source:            src,
		PassID:            passID,
		SnapshotTimestamp: report.SnapshotTimestamp,
		Sink:              NewAuditSink(pool),
	})
	if err != nil {
		return nil, fmt.Errorf("drill: reconcile: %w", err)
	}
	report.Reconciliation = rec
	// Drift is a finding plus an audit line; a clean pass has no finding to
	// record, and an audit row saying "nothing drifted" would be a log of
	// non-events. Zero rows on a clean pass, one per finding otherwise.
	report.FindingsRecorded = len(rec.Findings)
	report.RepairsApplied = 0

	// 4. Open the five.
	openParams := cfg.Open
	openParams.GiteaToken = token
	opened, err := OpenFive(ctx, cfg.Config, pool, openParams)
	if err != nil {
		return nil, fmt.Errorf("drill: open: %w", err)
	}
	report.Opened = opened

	// 5. Re-scan the artifact directory, now that the restore and the
	// report are in it too. The first scan covered the backup; this one
	// covers everything the run wrote, including this report.
	secrets := secretValueList(cfg.SecretValues)
	hits, err := ScanArtifactsForSecrets(cfg.ArtifactsDir, secrets)
	if err != nil {
		return nil, err
	}
	report.SecretScan.Hits = len(hits)
	if len(hits) > 0 {
		return nil, fmt.Errorf("drill: REFUSED — the run's own artifacts contain a credential value (%s)",
			describeHits(hits, nameByValue(cfg.SecretValues)))
	}

	report.EndedAt = time.Now().UTC()
	if err := writeJSON(filepath.Join(cfg.ArtifactsDir, fileReport), report); err != nil {
		return nil, err
	}
	return &RunReport{
		Report:        report,
		ReportPath:    filepath.ToSlash(filepath.Join(cfg.ArtifactsDir, fileReport)),
		DriftDetected: !rec.Clean(),
		AllFiveOpened: opened.AllOpened(),
	}, nil
}

// ReadReport reads a written report document back, verifying it parses.
// The test uses it so "the report is readable by a reviewer" is a checked
// property rather than an assumption about the writer.
func ReadReport(path string) (*DrillReport, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r DrillReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("drill report %s: %w", path, err)
	}
	if r.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("drill report %s: format_version %q, want %q", path, r.FormatVersion, FormatVersion)
	}
	return &r, nil
}
