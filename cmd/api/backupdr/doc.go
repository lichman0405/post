// Package backupdr is the backup / restore drill of docs/37_BACKUP_DR.md
// (task T1110): it takes a backup of the four classes that document names,
// restores it into an EMPTY environment, reconciles the four axes it names,
// and opens the five objects its V1 acceptance criterion names.
//
// # The four backup classes (docs/37 §需要备份, verbatim)
//
//	Postgres, S3 blobs/manifests, Gitea repositories,
//	critical secrets/config metadata
//
// Each class is one recorded section of the backup manifest, so a restored
// environment can be accounted for class by class. The fourth class is
// deliberately the narrow one the document's parenthesis names: the VALUES
// of secrets follow the secret manager's own backup policy and are not this
// drill's product. What the drill captures is their metadata — which
// configuration keys exist, where their value comes from, and which
// database columns are credential-bearing — and what it captures of the
// Postgres class is redacted of exactly those columns. A backup artifact
// that carried a password hash or a webhook signing secret would be a
// credential leak wearing a backup's clothes, and every artifact this
// package writes is scanned for the values the caller declares as secrets
// before it is accepted (see secrets.go and ScanArtifactsForSecrets).
//
// # The snapshot timestamp (docs/37 §一致性)
//
// "备份需记录 snapshot timestamp" — every manifest carries the instant the
// backup was taken, and the restore carries it forward: the restored
// environment's record names the instant it was restored TO. Without it
// "restored to which moment" is unanswerable, so the drill treats a
// missing or unreadable timestamp as a failure, not as an inconvenience.
//
// # The four reconciliation axes (docs/37 §一致性, verbatim)
//
//	DB ↔ Git refs ↔ blob hashes ↔ release manifests
//
// Reconcile runs those comparisons against a restored environment and
// reports what it finds.
//
// # The reconciler NEVER repairs
//
// This is the project's established reconciliation semantics, stated for
// the Git ↔ RSG loop in cmd/api/reconciliation.go and pinned at the storage
// layer by migration 00047 ("the reconciler NEVER repairs — the proposal is
// recorded, not applied"). This package follows the same shape and makes it
// structural rather than promised:
//
//   - the ports the reconciler reads through (sourceReader) expose reads
//     only, so there is no method it could repair with even if it wanted
//     to — the same discipline internal/gitprovider's ReconcilerPort
//     documents for the provider half;
//   - the one write the reconciler performs is the audit row a new finding
//     appends (audit_log, via='system', severity 'high'), which is the
//     project's immutable report surface, not a repair;
//   - every finding carries a repair_proposal: what a repair WOULD do,
//     stored and never applied, so the decision stays a human's.
//
// Why the findings land in audit_log and not in git_reconciliation_findings:
// that table's kind column is CHECK-pinned to the seven Git ↔ RSG drift
// classes (migration 00047), and a restore drill's drift is not one of them
// — blob content that no longer hashes to the recorded digest is not
// ref_missing. Reusing it would file the drift under a class that is false
// about it, and this task may not add a migration (infra/migrations is
// outside its scope), so the finding rides the general immutable audit
// surface instead. RunReport.Findings keeps the structured form.
//
// # V1 scope (docs/37 §RPO/RTO 目标)
//
// "V1 staging 先验证流程；生产初始目标可设 RPO 24h/RTO 4h" — V1's
// acceptance is that the PROCESS runs end to end, not that any RPO or RTO
// has been achieved. Nothing in this package measures, asserts or claims an
// RPO/RTO, and RunReport consequently has no such field. A drill that
// reported "RPO 24h met" from a run on a laptop would be stating something
// it cannot know.
package backupdr
