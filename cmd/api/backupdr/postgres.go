package backupdr

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// The Postgres class of the backup (docs/37 §需要备份).
//
// The dump is pg_dump's own output — the schema verbatim from
// `pg_dump --schema-only`, the rows verbatim from `pg_dump --data-only` —
// with ONE transformation applied afterwards: the values of the
// credential-bearing columns are replaced before the artifact is written.
//
// Why the replacement exists at all. docs/37 §需要备份 names the fourth
// class "critical secrets/config metadata" and adds, in the same line, that
// the secret itself follows the SECRET MANAGER's backup policy. A backup
// artifact carrying a webhook signing secret or a password hash would be
// carrying a secret value under another name, and the task's acceptance
// criterion is explicit that no credential value may appear in a dump. So
// the drill carries the COLUMN (the schema is complete) and the metadata
// (manifest.RedactedColumns names what was replaced and quotes the
// migration that justifies it), and leaves the value to be re-supplied on
// restore — RedactionPlaceholder is what stands in its place until the
// secret manager provides the real one again.
//
// Why it is a post-processing pass and not a hand-rolled dump. pg_dump
// knows the things a hand-rolled dumper gets wrong: extension DDL,
// sequence setval, generated columns, COPY escaping, dependency order,
// triggers, constraints. Reimplementing that to gain a column filter would
// trade a real, small, checkable transformation for a large, unchecked
// one. The pass below touches exactly the fields it names and copies every
// other byte through unchanged — and ScanArtifactsForSecrets then proves
// it, by looking for the values in the artifacts the pass produced.

// RedactionPlaceholder is what stands in for a credential value the backup
// deliberately does not carry. It is not a hash and not a valid secret of
// any kind: a restored environment's users cannot authenticate and its
// webhooks cannot be signed until the real values are re-supplied from the
// secret manager, which is precisely what docs/37 §需要备份 says happens.
const RedactionPlaceholder = "post-restore-placeholder:value-not-in-backup"

// credentialColumns is the closed set of columns the backup replaces. Every
// entry names the migration that introduced the column and quotes its own
// words about why the column is credential material, so the list can be
// audited against the schema instead of trusted.
//
// The list is deliberately short and total: a column that holds a secret
// and is missing from it is a leak, so adding a secret-bearing column to
// the schema means adding it here. Two guards push back on drift, and
// neither of them is a promise about the future:
//
//   - in the wrong direction (an entry names a column the dump does not
//     have), copyBlock.redact FAILS the dump rather than passing over it,
//     so the run stops instead of writing an artifact whose redaction has
//     silently stopped redacting anything. Pinned by
//     TestRedactionRefusesAColumnTheDumpDoesNotCarry, and observed on a
//     real dump: renaming the users entry made the drill fail with
//     "table users has no column ... to redact";
//   - in the other direction (a secret-bearing column the list does not
//     name), the entry-end scan refuses the run when the artifact turns
//     out to carry a value the caller declared as a secret — observed by
//     dropping this entry: "REFUSED — an artifact contains a credential
//     value (postgres/data/users.copy contains the value of
//     users.password_hash)".
//
// The second guard only closes over the values the CALLER declares, so a
// column nobody declares is a residual risk the drill cannot see. That is
// why the entries below quote their migrations: the list is meant to be
// audited against the schema by a reader, not only by the runtime.
var credentialColumns = []Redaction{
	{
		Table:  "users",
		Column: "password_hash",
		Reason: "migration 00016_auth_password.sql adds users.password_hash: the stored credential " +
			"a password login verifies against. A backup that carried it would let anyone holding the " +
			"artifact attempt an offline attack on every account in it.",
	},
	{
		Table:  "git_repository_provisions",
		Column: "webhook_secret",
		Reason: "migration 00022_git_repository_provisioning.sql: 'HMAC secret of the push webhook " +
			"(docs/55 SECRET). Written at provisioning, read only by internal/gitprovider; never exposed " +
			"through the API.' A backup is not an exception to 'never exposed'.",
	},
	{
		Table:  "webhook_endpoints",
		Column: "secret",
		Reason: "migration 00059_webhook_endpoints.sql: 'HMAC-SHA256 signing secret (generated, 32 " +
			"random bytes hex-encoded). Plaintext by necessity — the deliverer signs with it; a stored " +
			"hash could never reproduce a signature. Never rendered by any API read.'",
	},
}

// redactionsFor returns the credential columns of one table.
func redactionsFor(table string) []Redaction {
	var out []Redaction
	for _, r := range credentialColumns {
		if r.Table == table {
			out = append(out, r)
		}
	}
	return out
}

// kindForPGURL names the driver from a postgres URL. Only used for error
// text, so a mistyped URL says what was mistyped.
func kindForPGURL(url string) string {
	if strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://") {
		return "postgresql"
	}
	return "unknown"
}

// runPG runs one of the postgres client binaries with the environment's
// connection supplied explicitly. The binaries come from PATH (pg_dump,
// pg_restore and psql are part of the documented dev toolchain,
// ops/DEV_COMMANDS.md); a missing one is a loud failure with the name in
// it, never a silent skip.
func runPG(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("postgres: no command given")
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		return nil, fmt.Errorf("postgres: %s not on PATH — install the postgresql client "+
			"(ops/DEV_COMMANDS.md): %w", args[0], err)
	}
	cmd := exec.CommandContext(ctx, bin, args[1:]...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("postgres: %s failed: %w: %s", args[0], err, truncate(stderr.String(), 2000))
	}
	return stdout.Bytes(), nil
}

// dumpSchema writes the schema half: pg_dump's own DDL, no data. It is
// `--no-owner --no-privileges` because the restoring role is not
// necessarily the dumping one (a restore target is a fresh environment
// with its own role), and neither ownership nor grants are part of what
// docs/37 §需要备份 asks a backup to carry.
//
// Comments are NOT suppressed: `COMMENT ON` statements are schema — the
// platform's tables document their own invariants in the catalog, and a
// schema restored without them is a schema whose rules are no longer
// readable where they are enforced.
func dumpSchema(ctx context.Context, sourceURL, dest string) error {
	raw, err := runPG(ctx, nil, "pg_dump",
		"--schema-only", "--no-owner", "--no-privileges",
		"--dbname", sourceURL)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return fmt.Errorf("postgres: pg_dump --schema-only produced nothing for %s", kindForPGURL(sourceURL))
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, raw, 0o644)
}

// dumpData writes the data half and applies the redaction. It returns the
// per-table records (file, row count, digest, redacted columns) the
// manifest carries.
func dumpData(ctx context.Context, sourceURL, dir string) ([]TableRecord, []Redaction, int, error) {
	raw, err := runPG(ctx, nil, "pg_dump",
		"--data-only", "--no-owner", "--no-privileges",
		"--dbname", sourceURL)
	if err != nil {
		return nil, nil, 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, 0, err
	}

	blocks, err := splitCopyBlocks(raw)
	if err != nil {
		return nil, nil, 0, err
	}
	sequences := sequenceStatements(raw)
	if len(sequences) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "_sequences.sql"), sequences, 0o644); err != nil {
			return nil, nil, 0, err
		}
	}

	var records []TableRecord
	var applied []Redaction
	totalRows := 0
	for i := range blocks {
		b := &blocks[i]
		reds := redactionsFor(b.Table)
		if len(reds) > 0 {
			if err := b.redact(reds); err != nil {
				return nil, nil, 0, err
			}
			applied = append(applied, reds...)
		}
		file := filepath.Join(dir, b.Table+".copy")
		if err := os.WriteFile(file, b.render(), 0o644); err != nil {
			return nil, nil, 0, err
		}
		cols := make([]string, 0, len(reds))
		for _, r := range reds {
			cols = append(cols, r.Column)
		}
		records = append(records, TableRecord{
			Name:     b.Table,
			File:     filepath.ToSlash(file),
			Rows:     len(b.Rows),
			SHA256:   sha256Hex(b.render()),
			Redacted: cols,
		})
		totalRows += len(b.Rows)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Name < records[j].Name })
	return records, applied, totalRows, nil
}

// sequenceStatements extracts the dump's sequence positions.
//
// Only one statement shape is taken — pg_dump's own
// `SELECT pg_catalog.setval('...', N, true);` — rather than "everything
// outside a COPY block". Everything-outside-a-COPY would also carry the
// dump's comments, its SET lines and its ownership statements, and a
// replayed SET could change the restored session's meaning; matching one
// statement form keeps the pass to exactly the data it means to carry.
func sequenceStatements(script []byte) []byte {
	var out []byte
	for _, line := range strings.Split(string(script), "\n") {
		if strings.HasPrefix(line, "SELECT pg_catalog.setval(") {
			out = append(out, line...)
			out = append(out, '\n')
		}
	}
	return out
}

// copyBlock is one `COPY <table> (<cols>) FROM stdin;` section of a
// pg_dump --data-only script: the exact header, the row lines, and the
// terminator. Anything outside a block is not represented here at all —
// the caller keeps the rest of the script untouched.
type copyBlock struct {
	Table   string
	Header  string
	Columns []string
	Rows    [][]string
}

// splitCopyBlocks parses a pg_dump --data-only script into its COPY
// blocks. It is deliberately strict: a line that looks like a COPY header
// but does not parse, or a block that never terminates, is an error rather
// than a silently unredacted section.
func splitCopyBlocks(script []byte) ([]copyBlock, error) {
	lines := strings.Split(string(script), "\n")
	var blocks []copyBlock
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !strings.HasPrefix(line, "COPY ") {
			continue
		}
		b, next, err := parseCopyBlock(lines, i)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
		i = next
	}
	return blocks, nil
}

func parseCopyBlock(lines []string, start int) (copyBlock, int, error) {
	header := lines[start]
	rest := strings.TrimPrefix(header, "COPY ")
	open := strings.Index(rest, " (")
	from := strings.Index(rest, ") FROM stdin;")
	if open < 0 || from < 0 || from < open {
		return copyBlock{}, 0, fmt.Errorf("postgres: unparseable COPY header: %q", truncate(header, 200))
	}
	table := strings.TrimSpace(rest[:open])
	// `public.users` / `users` — the manifest records the unqualified name,
	// which is what the drill's SQL reads use.
	if dot := strings.LastIndex(table, "."); dot >= 0 {
		table = table[dot+1:]
	}
	table = strings.Trim(table, `"`)
	cols := strings.Split(rest[open+2:from], ", ")
	for i := range cols {
		cols[i] = strings.Trim(strings.TrimSpace(cols[i]), `"`)
	}

	b := copyBlock{Table: table, Header: header, Columns: cols}
	for j := start + 1; j < len(lines); j++ {
		if lines[j] == `\.` {
			return b, j, nil
		}
		b.Rows = append(b.Rows, strings.Split(lines[j], "\t"))
	}
	return copyBlock{}, 0, fmt.Errorf("postgres: COPY block for %s never terminated (unredacted section would have been written)", table)
}

// redact replaces the named columns' values with RedactionPlaceholder. A
// column the block does not carry is an error: it would mean the
// redaction list and the schema have drifted apart, and passing over it
// would leak the very value the entry exists to remove.
func (b *copyBlock) redact(reds []Redaction) error {
	for _, r := range reds {
		idx := -1
		for i, c := range b.Columns {
			if c == r.Column {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("postgres: table %s has no column %s to redact (the redaction list and the schema have drifted; "+
				"refusing to write a dump that may carry the value)", b.Table, r.Column)
		}
		for i := range b.Rows {
			if len(b.Rows[i]) != len(b.Columns) {
				return fmt.Errorf("postgres: table %s row %d has %d fields, want %d",
					b.Table, i, len(b.Rows[i]), len(b.Columns))
			}
			b.Rows[i][idx] = RedactionPlaceholder
		}
	}
	return nil
}

// render reproduces the block as pg_dump wrote it, with the redaction in
// place. Fields are joined with a tab and the terminator is restored, so a
// restored block is byte-identical to the original apart from the redacted
// fields.
func (b copyBlock) render() []byte {
	var sb strings.Builder
	sb.WriteString(b.Header)
	sb.WriteByte('\n')
	for _, row := range b.Rows {
		sb.WriteString(strings.Join(row, "\t"))
		sb.WriteByte('\n')
	}
	sb.WriteString("\\.\n")
	return []byte(sb.String())
}

// restoreSchema applies the schema half to an empty target.
func restoreSchema(ctx context.Context, targetURL, schemaFile string) error {
	raw, err := os.ReadFile(schemaFile)
	if err != nil {
		return err
	}
	// Every statement stops the restore on failure: a schema that half
	// applied would leave a target that looks restorable and is not.
	_, err = runPG(ctx, raw, "psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "--dbname", targetURL)
	return err
}

// restoreData loads the per-table blocks into a target.
//
// One transaction, with `session_replication_role = replica` set for its
// duration. The role is what a snapshot load needs and only that: the
// platform's guards are write-path guards (migration 00014's append-only
// triggers, and the DEFERRABLE constraint triggers that assert a
// projection row and its references arrive together), and a bulk load
// arriving table by table would trip them for reasons that say nothing
// about the data being wrong. Setting it for the load is also what makes
// the load atomic: the whole snapshot lands or none of it does.
func restoreData(ctx context.Context, targetURL, dataDir string, records []TableRecord) (int, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return 0, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".copy") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var script bytes.Buffer
	script.WriteString("\\set ON_ERROR_STOP on\n")
	script.WriteString("BEGIN;\n")
	script.WriteString("SET LOCAL session_replication_role = replica;\n")
	rows := 0
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dataDir, name))
		if err != nil {
			return 0, err
		}
		script.Write(raw)
		if !bytes.HasSuffix(raw, []byte("\n")) {
			script.WriteByte('\n')
		}
	}
	// The sequence positions come after the rows, inside the same
	// transaction: setval before the rows would be overwritten by nothing,
	// but a load that failed after setval would leave positions ahead of
	// the data it never inserted.
	if seq, err := os.ReadFile(filepath.Join(dataDir, "_sequences.sql")); err == nil {
		script.Write(seq)
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	script.WriteString("COMMIT;\n")

	if _, err := runPG(ctx, script.Bytes(), "psql", "-X", "-q", "--dbname", targetURL); err != nil {
		return 0, err
	}
	for _, r := range records {
		rows += r.Rows
	}
	return rows, nil
}

// tableCounts reads the target's EXACT row count per table.
//
// pg_class.reltuples is not used, and that is a deliberate refusal rather
// than a performance oversight. reltuples is a planner statistic: it is -1
// on a table that has never been analyzed, and it lags writes until
// autovacuum catches up. This function exists to answer "is the target
// empty", where a -1 or a stale estimate would report an empty environment
// that has rows in it — and an "empty environment" proven by a statistic
// that says -1 for a populated table is exactly the kind of green light
// this drill exists to refuse. The target is expected to be empty, so the
// exact count is also cheap.
func tableCounts(ctx context.Context, q querier) (map[string]int, error) {
	rows, err := q.Query(ctx, `SELECT c.relname
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r'
		ORDER BY c.relname`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := map[string]int{}
	for _, name := range names {
		var n int
		// The identifier comes from pg_class, never from a caller.
		if err := q.QueryRow(ctx, `SELECT count(*) FROM public.`+quoteIdent(name)).Scan(&n); err != nil {
			return nil, fmt.Errorf("postgres: count %s: %w", name, err)
		}
		out[name] = n
	}
	return out, nil
}

// quoteIdent quotes a table name for interpolation. The names come from
// pg_class, but quoting them is still the difference between reading a
// catalog and building a statement out of catalog text.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// querier is the small slice of pgx the drill's reads need — nothing here
// writes to the source, and keeping the interface to the read method alone
// is how that stays true by construction. *pgxpool.Pool satisfies it, as
// does a single *pgx.Conn or a pgx.Tx, so a read helper can be pointed at
// whichever of them the caller already holds.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
