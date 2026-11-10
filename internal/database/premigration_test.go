package database_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	// Needed to avoid cyclic dependencies in ent (https://entgo.io/docs/privacy#privacy-policy-registration)
	_ "github.com/buildbarn/bb-portal/ent/gen/ent/runtime"

	"github.com/buildbarn/bb-portal/ent/gen/ent/migrate"
	"github.com/buildbarn/bb-portal/internal/database"
	"github.com/buildbarn/bb-portal/internal/database/dbauthservice"
	"github.com/buildbarn/bb-portal/internal/database/embedded"
	"github.com/stretchr/testify/require"
)

var dbProvider *embedded.DatabaseProvider

func TestMain(m *testing.M) {
	var err error
	dbProvider, err = embedded.NewDatabaseProvider(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not start embedded DB: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	dbProvider.Cleanup()
	os.Exit(code)
}

func newTestClient(t *testing.T) (*sql.DB, database.Client) {
	connection, err := dbProvider.CreateDatabase()
	require.NoError(t, err)
	t.Cleanup(func() { connection.Close() })
	client, err := database.New("postgres", connection)
	require.NoError(t, err)
	return connection, client
}

// requireNoPendingSchemaChanges asserts that the startup migration
// would not change anything.
func requireNoPendingSchemaChanges(ctx context.Context, t *testing.T, client database.Client) {
	var pending bytes.Buffer
	require.NoError(t, client.Ent().Schema.WriteTo(ctx, &pending, migrate.WithDropIndex(true)))
	require.Empty(t, pending.String())
}

func TestSchemaCreateIsIdempotent(t *testing.T) {
	// Regression test: identifiers longer than 63 bytes are truncated by
	// Postgres, which made Ent drop and recreate the affected index on
	// every startup.
	ctx := context.Background()
	_, client := newTestClient(t)
	require.NoError(t, client.Ent().Schema.Create(ctx, migrate.WithDropIndex(true)))
	requireNoPendingSchemaChanges(ctx, t, client)
}

func TestRunPreMigrations(t *testing.T) {
	ctx := dbauthservice.NewContextWithDbAuthServiceBypass(context.Background())

	t.Run("EmptyDatabase", func(t *testing.T) {
		connection, client := newTestClient(t)
		require.NoError(t, database.RunPreMigrations(ctx, connection))
		require.NoError(t, client.Ent().Schema.Create(ctx, migrate.WithDropIndex(true)))
		requireNoPendingSchemaChanges(ctx, t, client)
	})

	t.Run("CurrentSchemaIsUnchanged", func(t *testing.T) {
		connection, client := newTestClient(t)
		require.NoError(t, client.Ent().Schema.Create(ctx, migrate.WithDropIndex(true)))
		require.NoError(t, database.RunPreMigrations(ctx, connection))
		require.NoError(t, database.RunPreMigrations(ctx, connection))
		requireNoPendingSchemaChanges(ctx, t, client)
	})

	t.Run("LegacyActionCacheStatisticsForeignKey", func(t *testing.T) {
		connection, client := newTestClient(t)
		require.NoError(t, client.Ent().Schema.Create(ctx, migrate.WithDropIndex(true)))

		actionSummary, err := client.Ent().ActionSummary.Create().Save(ctx)
		require.NoError(t, err)
		actionCacheStatistics, err := client.Ent().ActionCacheStatistics.Create().
			SetActionSummary(actionSummary).
			Save(ctx)
		require.NoError(t, err)

		// Reproduce the layout created by earlier versions: the long
		// column name, and the unique index under the name Postgres
		// truncated it to.
		_, err = connection.ExecContext(ctx, `ALTER TABLE "action_cache_statistics" RENAME COLUMN "action_summary_id" TO "action_summary_action_cache_statistics"`)
		require.NoError(t, err)
		_, err = connection.ExecContext(ctx, `ALTER INDEX "action_cache_statistics_action_summary_id_key" RENAME TO "action_cache_statistics_action_summary_action_cache_statistics_"`)
		require.NoError(t, err)

		require.NoError(t, database.RunPreMigrations(ctx, connection))
		// The pre-migrations alone must yield the current schema.
		// Otherwise Schema.Create would rebuild the unique index, and
		// externally managed schemas would need more than these renames.
		requireNoPendingSchemaChanges(ctx, t, client)
		require.NoError(t, client.Ent().Schema.Create(ctx, migrate.WithDropIndex(true)))
		requireNoPendingSchemaChanges(ctx, t, client)

		// The relation stored before the migration must survive it.
		gotActionSummaryID, err := client.Ent().ActionCacheStatistics.
			QueryActionSummary(actionCacheStatistics).
			OnlyID(ctx)
		require.NoError(t, err)
		require.Equal(t, actionSummary.ID, gotActionSummaryID)
	})
}
