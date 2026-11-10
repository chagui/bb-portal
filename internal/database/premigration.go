package database

import (
	"context"
	"database/sql"

	"github.com/buildbarn/bb-storage/pkg/util"
)

// preMigrationLockID is an arbitrary key for the Postgres advisory lock
// that serializes RunPreMigrations across concurrently starting
// replicas.
const preMigrationLockID = 0x62625f706d6967 // "bb_pmig"

// RunPreMigrations applies schema changes that Ent's automatic
// migration cannot express, such as renames, which it would otherwise
// perform as an add of a new, empty column. It must run before
// Schema.Create. Every step runs on every startup, so it must be
// idempotent.
func RunPreMigrations(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return util.StatusWrap(err, "Failed to start pre-migration transaction")
	}
	// Rollback after a successful Commit is a no-op.
	defer tx.Rollback()

	// Without this lock, a replica could observe the legacy layout and
	// then fail to rename a column another replica just renamed.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", preMigrationLockID); err != nil {
		return util.StatusWrap(err, "Failed to acquire pre-migration lock")
	}
	if err := renameActionCacheStatisticsForeignKey(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return util.StatusWrap(err, "Failed to commit pre-migrations")
	}
	return nil
}

// renameActionCacheStatisticsForeignKey renames the foreign key column
// from action_cache_statistics to action_summaries. Its former name made
// the name of its implicit unique index exceed the 63 byte identifier
// limit of Postgres, so Postgres stored it truncated. Ent did not
// recognize the truncated name, and recreated the index on every
// startup.
func renameActionCacheStatisticsForeignKey(ctx context.Context, tx *sql.Tx) error {
	var legacyColumnExists bool
	if err := tx.QueryRowContext(
		ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			AND table_name = 'action_cache_statistics'
			AND column_name = 'action_summary_action_cache_statistics'
		)`,
	).Scan(&legacyColumnExists); err != nil {
		return util.StatusWrap(err, "Failed to look up legacy action_cache_statistics foreign key column")
	}
	if !legacyColumnExists {
		return nil
	}

	if _, err := tx.ExecContext(ctx, `ALTER TABLE "action_cache_statistics" RENAME COLUMN "action_summary_action_cache_statistics" TO "action_summary_id"`); err != nil {
		return util.StatusWrap(err, "Failed to rename legacy action_cache_statistics foreign key column")
	}
	// Renaming a column does not rename its indexes. Give the unique
	// index the name Ent expects, so that it is not recreated.
	if _, err := tx.ExecContext(ctx, `ALTER INDEX IF EXISTS "action_cache_statistics_action_summary_action_cache_statistics_" RENAME TO "action_cache_statistics_action_summary_id_key"`); err != nil {
		return util.StatusWrap(err, "Failed to rename legacy action_cache_statistics unique index")
	}
	return nil
}
