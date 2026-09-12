package integration

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Snapshot machinery: everything the migration test asserts comes from
// pg_catalog / information_schema queries against the live database — never
// from "the migration command exited 0".

type snapColumn struct {
	Name       string
	DataType   string
	UDTName    string
	Nullable   bool
	HasDefault bool
}

type snapFK struct {
	Column   string
	RefTable string
	RefCol   string
	OnDelete string
}

type snapTable struct {
	Columns         []snapColumn
	PK              []string
	Uniques         [][]string
	Checks          []string
	FKs             []snapFK
	ExplicitIndexes []string // sorted "name:indexdef" of non-constraint indexes
}

type dbSnapshot struct {
	Tables     map[string]snapTable
	Extensions []string
}

// takeSnapshot captures the full structural state of the public schema.
func takeSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) dbSnapshot {
	t.Helper()
	tables := []string{}
	rows, err := pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		ORDER BY table_name`)
	if err != nil {
		t.Fatalf("catalog: list tables: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("catalog: scan table name: %v", err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("catalog: list tables: %v", err)
	}

	snap := make(map[string]snapTable, len(tables))
	for _, name := range tables {
		snap[name] = snapshotTable(t, ctx, pool, name)
	}

	exts := []string{}
	rows, err = pool.Query(ctx, `SELECT extname FROM pg_extension ORDER BY extname`)
	if err != nil {
		t.Fatalf("catalog: list extensions: %v", err)
	}
	for rows.Next() {
		var ext string
		if err := rows.Scan(&ext); err != nil {
			t.Fatalf("catalog: scan extension: %v", err)
		}
		exts = append(exts, ext)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("catalog: list extensions: %v", err)
	}
	return dbSnapshot{Tables: snap, Extensions: exts}
}

func snapshotTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) snapTable {
	t.Helper()
	var st snapTable

	// Columns: exact name, type, nullability, default presence.
	rows, err := pool.Query(ctx, `
		SELECT column_name, data_type, udt_name, is_nullable,
		       (column_default IS NOT NULL)
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
		ORDER BY ordinal_position`, name)
	if err != nil {
		t.Fatalf("catalog: columns of %s: %v", name, err)
	}
	for rows.Next() {
		var col snapColumn
		var udt, nullable string
		if err := rows.Scan(&col.Name, &col.DataType, &udt, &nullable, &col.HasDefault); err != nil {
			t.Fatalf("catalog: scan column of %s: %v", name, err)
		}
		// information_schema reports the element type as udt_name for plain
		// types; keep it only where it matters (arrays, vector).
		if col.DataType == "ARRAY" || col.DataType == "USER-DEFINED" {
			col.UDTName = udt
		}
		col.Nullable = nullable == "YES"
		st.Columns = append(st.Columns, col)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("catalog: columns of %s: %v", name, err)
	}

	// Primary key and unique constraints, with their column lists.
	rows, err = pool.Query(ctx, `
		SELECT con.contype, array_agg(att.attname ORDER BY k.ord)
		FROM pg_constraint con
		JOIN pg_namespace n ON n.oid = con.connamespace
		JOIN pg_class rel ON rel.oid = con.conrelid
		CROSS JOIN LATERAL unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
		JOIN pg_attribute att ON att.attrelid = rel.oid AND att.attnum = k.attnum
		WHERE n.nspname = 'public' AND rel.relname = $1 AND con.contype IN ('p','u')
		GROUP BY con.conname, con.contype
		ORDER BY con.contype, con.conname`, name)
	if err != nil {
		t.Fatalf("catalog: keys of %s: %v", name, err)
	}
	for rows.Next() {
		var contype string
		var cols []string
		if err := rows.Scan(&contype, &cols); err != nil {
			t.Fatalf("catalog: scan key of %s: %v", name, err)
		}
		if contype == "p" {
			st.PK = cols
		} else {
			st.Uniques = append(st.Uniques, cols)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("catalog: keys of %s: %v", name, err)
	}
	sortUniques(st.Uniques)

	// Check constraints: full pg_get_constraintdef text, sorted.
	rows, err = pool.Query(ctx, `
		SELECT pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_namespace n ON n.oid = con.connamespace
		JOIN pg_class rel ON rel.oid = con.conrelid
		WHERE n.nspname = 'public' AND rel.relname = $1 AND con.contype = 'c'
		ORDER BY 1`, name)
	if err != nil {
		t.Fatalf("catalog: checks of %s: %v", name, err)
	}
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("catalog: scan check of %s: %v", name, err)
		}
		st.Checks = append(st.Checks, def)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("catalog: checks of %s: %v", name, err)
	}

	// Foreign keys: column, referenced table/column, delete action.
	rows, err = pool.Query(ctx, `
		SELECT kcu.column_name, ccu.table_name, ccu.column_name, con.confdeltype
		FROM pg_constraint con
		JOIN pg_namespace n ON n.oid = con.connamespace
		JOIN pg_class rel ON rel.oid = con.conrelid
		JOIN information_schema.key_column_usage kcu
		  ON kcu.constraint_name = con.conname AND kcu.table_schema = n.nspname
		 AND kcu.table_name = rel.relname
		JOIN information_schema.constraint_column_usage ccu
		  ON ccu.constraint_name = con.conname AND ccu.table_schema = n.nspname
		WHERE n.nspname = 'public' AND rel.relname = $1 AND con.contype = 'f'
		ORDER BY kcu.column_name`, name)
	if err != nil {
		t.Fatalf("catalog: fks of %s: %v", name, err)
	}
	for rows.Next() {
		var fk snapFK
		var delType string
		if err := rows.Scan(&fk.Column, &fk.RefTable, &fk.RefCol, &delType); err != nil {
			t.Fatalf("catalog: scan fk of %s: %v", name, err)
		}
		switch delType {
		case "r":
			fk.OnDelete = "RESTRICT"
		case "n":
			fk.OnDelete = "SET NULL"
		default:
			fk.OnDelete = delType
		}
		st.FKs = append(st.FKs, fk)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("catalog: fks of %s: %v", name, err)
	}
	sort.Slice(st.FKs, func(i, j int) bool { return st.FKs[i].Column < st.FKs[j].Column })

	// Non-constraint indexes.
	rows, err = pool.Query(ctx, `
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = 'public' AND tablename = $1
		ORDER BY indexname`, name)
	if err != nil {
		t.Fatalf("catalog: indexes of %s: %v", name, err)
	}
	for rows.Next() {
		var idx, def string
		if err := rows.Scan(&idx, &def); err != nil {
			t.Fatalf("catalog: scan index of %s: %v", name, err)
		}
		if strings.HasSuffix(idx, "_pkey") || strings.HasSuffix(idx, "_key") {
			continue // constraint-backed, covered above
		}
		st.ExplicitIndexes = append(st.ExplicitIndexes, idx+" :: "+def)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("catalog: indexes of %s: %v", name, err)
	}
	return st
}

func sortUniques(us [][]string) {
	for _, u := range us {
		sort.Strings(u)
	}
	sort.Slice(us, func(i, j int) bool {
		return strings.Join(us[i], ",") < strings.Join(us[j], ",")
	})
}

// snapshotJSON renders a snapshot deterministically for equality comparison.
func snapshotJSON(t *testing.T, snap dbSnapshot) string {
	t.Helper()
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("catalog: marshal snapshot: %v", err)
	}
	return string(b)
}

// compareCatalog checks a live snapshot against the expected fixture.
func compareCatalog(t *testing.T, got dbSnapshot, want map[string]tableExp) {
	t.Helper()

	if !reflect.DeepEqual(got.Extensions, []string{"pgcrypto", "plpgsql", "vector"}) {
		t.Errorf("catalog: extension set mismatch: got %v", got.Extensions)
	}

	snap := got.Tables

	wantNames := make([]string, 0, len(want))
	for name := range want {
		wantNames = append(wantNames, name)
	}
	sort.Strings(wantNames)
	gotNames := make([]string, 0, len(snap))
	for name := range snap {
		gotNames = append(gotNames, name)
	}
	sort.Strings(gotNames)
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("catalog: table set mismatch\ngot:  %v\nwant: %v", gotNames, wantNames)
		return
	}

	for _, name := range wantNames {
		t.Run("table "+name, func(t *testing.T) {
			exp := want[name]
			actual := snap[name]

			// Columns, in ordinal order.
			gotCols := append([]snapColumn(nil), actual.Columns...)
			var wantCols []snapColumn
			for _, ec := range exp.cols {
				wantCols = append(wantCols, snapColumn{
					Name: ec.name, DataType: ec.dataType, UDTName: ec.udtName,
					Nullable: ec.nullable, HasDefault: ec.hasDefault,
				})
			}
			if !reflect.DeepEqual(gotCols, wantCols) {
				t.Errorf("columns mismatch for %s:\ngot:  %+v\nwant: %+v", name, gotCols, wantCols)
			}

			// Primary key.
			sort.Strings(actual.PK)
			wantPK := append([]string(nil), exp.pk...)
			sort.Strings(wantPK)
			if !reflect.DeepEqual(actual.PK, wantPK) {
				t.Errorf("primary key mismatch for %s: got %v want %v", name, actual.PK, wantPK)
			}

			// Unique constraints.
			sortUniques(actual.Uniques)
			wantUniques := make([][]string, len(exp.uniques))
			for i, u := range exp.uniques {
				wantUniques[i] = append([]string(nil), u...)
			}
			sortUniques(wantUniques)
			if actual.Uniques == nil {
				actual.Uniques = [][]string{}
			}
			if !reflect.DeepEqual(actual.Uniques, wantUniques) {
				t.Errorf("unique constraints mismatch for %s:\ngot:  %v\nwant: %v", name, actual.Uniques, wantUniques)
			}

			// Check constraints: each expected distinguishing substring must
			// match exactly one check definition, and there must be no others.
			if len(actual.Checks) != len(exp.checks) {
				t.Errorf("check constraint count mismatch for %s: got %d (%v) want %d (%v)",
					name, len(actual.Checks), actual.Checks, len(exp.checks), exp.checks)
			} else {
				for _, sub := range exp.checks {
					matches := 0
					for _, def := range actual.Checks {
						if strings.Contains(def, sub) {
							matches++
						}
					}
					if matches != 1 {
						t.Errorf("check substring %q in %s matched %d defs (want 1): %v", sub, name, matches, actual.Checks)
					}
				}
			}

			// Foreign keys.
			var gotFKs []snapFK
			gotFKs = append(gotFKs, actual.FKs...)
			sort.Slice(gotFKs, func(i, j int) bool { return gotFKs[i].Column < gotFKs[j].Column })
			var wantFKs []snapFK
			for _, f := range exp.fks {
				wantFKs = append(wantFKs, snapFK{Column: f.col, RefTable: f.refTable, RefCol: f.refCol, OnDelete: f.onDelete})
			}
			sort.Slice(wantFKs, func(i, j int) bool { return wantFKs[i].Column < wantFKs[j].Column })
			if !reflect.DeepEqual(gotFKs, wantFKs) {
				t.Errorf("foreign keys mismatch for %s:\ngot:  %+v\nwant: %+v", name, gotFKs, wantFKs)
			}
		})
	}

	// The canonical explicit indexes must exist with the right shape, and no
	// other non-constraint index may exist anywhere.
	var gotExplicit []string
	for _, st := range snap {
		gotExplicit = append(gotExplicit, st.ExplicitIndexes...)
	}
	sort.Strings(gotExplicit)
	var wantExplicit []string
	for idx, subs := range explicitIndexes {
		var found string
		for _, entry := range gotExplicit {
			name := strings.SplitN(entry, " :: ", 2)[0]
			if name != idx {
				continue
			}
			found = entry
			def := strings.SplitN(entry, " :: ", 2)[1]
			for _, sub := range subs {
				if !strings.Contains(def, sub) {
					t.Errorf("index %s: definition missing %q: %s", idx, sub, def)
				}
			}
		}
		if found == "" {
			t.Errorf("index %s not found in catalog", idx)
		}
		wantExplicit = append(wantExplicit, idx)
	}
	sort.Strings(wantExplicit)
	if len(gotExplicit) != len(wantExplicit) {
		t.Errorf("explicit index set mismatch: got %v want %v", gotExplicit, wantExplicit)
	}
}
