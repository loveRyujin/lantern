package workspace

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/kb/*.sql
var kbMigrations embed.FS

//go:embed migrations/analytics/*.sql
var analyticsMigrations embed.FS

//go:embed migrations/sensitive/*.sql
var sensitiveMigrations embed.FS

//go:embed migrations/runs/*.sql
var runsMigrations embed.FS

const migrationTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`

// migrationFS 是 embed.FS 里本包用到的那一部分。
type migrationFS interface {
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
}

func migrateDir(ctx context.Context, db *sql.DB, fsys migrationFS, dir string) error {
	if _, err := db.ExecContext(ctx, migrationTable); err != nil {
		return fmt.Errorf("schema_migrations: %w", err)
	}
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	applied := map[int]struct{}{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			_ = rows.Close()
			return err
		}
		applied[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if _, ok := applied[version]; ok {
			continue
		}
		body, err := fsys.ReadFile(path.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if err := applyMigration(ctx, db, version, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func migrationVersion(name string) (int, error) {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("migration %s: missing numeric prefix", name)
	}
	version, err := strconv.Atoi(name[:i])
	if err != nil {
		return 0, fmt.Errorf("migration %s: %w", name, err)
	}
	return version, nil
}

func applyMigration(ctx context.Context, db *sql.DB, version int, name, script string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %s: begin: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, script); err != nil {
		return fmt.Errorf("migration %s: %w", name, err)
	}
	appliedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		version, name, appliedAt,
	); err != nil {
		return fmt.Errorf("migration %s: record: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: commit: %w", name, err)
	}
	return nil
}

// dropVectorsNew 实现技术设计 §15：启动时如果影子表还在，就删掉，避免新旧向量混在一起。
func dropVectorsNew(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS vectors_new`); err != nil {
		return fmt.Errorf("drop vectors_new: %w", err)
	}
	return nil
}
