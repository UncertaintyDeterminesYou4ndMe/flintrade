package db

import (
	"context"
	"errors"
	"fmt"
)

// columnMigrations add columns introduced after a table was first created.
// schema.sql only shapes fresh databases; existing ones are patched here.
var columnMigrations = []struct{ table, column, ddl string }{
	{"trades", "attribution", "ALTER TABLE trades ADD COLUMN attribution TEXT"},
	// Staged take-profit (2026-08-20).
	{"positions", "target2", "ALTER TABLE positions ADD COLUMN target2 REAL"},
	{"positions", "t1_done", "ALTER TABLE positions ADD COLUMN t1_done INTEGER NOT NULL DEFAULT 0"},
}

// StrategyRecord is the one definition of Flintrade's own trading record — what
// self-assessment, lessons, postmortems and similar_history count. It leaves
// out legacy (the v1 Arena system: other prompts, no risk gate), manual, test
// and outage-degraded fills. prefix is a table alias with its dot ("t.") or "".
func StrategyRecord(prefix string) string {
	return "(" + prefix + "attribution IS NULL OR " + prefix + "attribution = 'strategy')"
}

// Init creates the schema idempotently, applies column migrations and makes
// sure the single risk_state row and schema_version exist. Same contract as
// init_db() in agent/db.py.
func (d *DB) Init(ctx context.Context) error {
	if d.readOnly {
		return ErrReadOnly
	}
	if _, err := d.sql.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	for _, m := range columnMigrations {
		has, err := d.hasColumn(ctx, m.table, m.column)
		if err != nil {
			return err
		}
		if !has {
			if _, err := d.sql.ExecContext(ctx, m.ddl); err != nil {
				return fmt.Errorf("migrate %s.%s: %w", m.table, m.column, err)
			}
		}
	}
	// vectors held embeddings for the Python semantic memory; similar_history
	// is SQL now. Empty in production when dropped (2026-11-09).
	if _, err := d.sql.ExecContext(ctx, `DROP TABLE IF EXISTS vectors`); err != nil {
		return fmt.Errorf("drop vectors: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx,
		`INSERT INTO risk_state(id,equity,day_start_equity)
		 SELECT 1, NULL, NULL WHERE NOT EXISTS (SELECT 1 FROM risk_state)`); err != nil {
		return fmt.Errorf("seed risk_state: %w", err)
	}
	if _, err := d.sql.ExecContext(ctx,
		`INSERT INTO kv(key,value) VALUES('schema_version','1') ON CONFLICT(key) DO NOTHING`); err != nil {
		return fmt.Errorf("seed schema_version: %w", err)
	}
	return nil
}

func (d *DB) hasColumn(ctx context.Context, table, column string) (bool, error) {
	rows, err := d.sql.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if !found {
		// pragma_table_info returns no rows for a missing table; that would be
		// a schema bug, not a migration to apply.
		var n int
		if err := d.sql.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 {
			return false, errors.New("table " + table + " missing after schema apply")
		}
	}
	return found, nil
}
