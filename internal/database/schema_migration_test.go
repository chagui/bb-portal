package database_test

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	// Needed to avoid cyclic dependencies in ent (https://entgo.io/docs/privacy#privacy-policy-registration)
	_ "github.com/buildbarn/bb-portal/ent/gen/ent/runtime"

	"github.com/buildbarn/bb-portal/ent/gen/ent/migrate"
	"github.com/buildbarn/bb-portal/internal/database"
	"github.com/buildbarn/bb-portal/internal/database/dbauthservice"
	"github.com/buildbarn/bb-portal/internal/database/embedded"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestMigrateLegacyBuildURL(t *testing.T) {
	provider, err := embedded.NewDatabaseProvider(os.Stderr)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Cleanup()) })
	connection, err := provider.CreateDatabase()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, connection.Close()) })
	client, err := database.New("postgres", connection)
	require.NoError(t, err)
	ctx := dbauthservice.NewContextWithDbAuthServiceBypass(context.Background())
	require.NoError(t, client.Ent().Schema.Create(ctx, migrate.WithDropIndex(true)))
	instance, err := client.Ent().InstanceName.Create().SetName("legacy").Save(ctx)
	require.NoError(t, err)
	legacyBuild, err := client.Ent().Build.Create().SetBuildUUID(uuid.New()).
		SetInstanceNameID(instance.ID).SetTimestamp(time.Now()).Save(ctx)
	require.NoError(t, err)

	// Reproduce the column left behind by older versions. Ent does not drop
	// columns unless explicitly requested, so removing the field from the model
	// left a required column that the current build writer no longer supplies.
	_, err = connection.ExecContext(ctx, `ALTER TABLE "builds" ADD COLUMN IF NOT EXISTS "build_url" character varying`)
	require.NoError(t, err)
	_, err = connection.ExecContext(ctx, `UPDATE "builds" SET "build_url" = 'https://ci.example/legacy' WHERE "id" = $1`, legacyBuild.ID)
	require.NoError(t, err)
	_, err = connection.ExecContext(ctx, `ALTER TABLE "builds" ALTER COLUMN "build_url" SET NOT NULL`)
	require.NoError(t, err)

	// Startup's ordinary automatic migration must make the legacy column
	// optional without requiring column dropping or a separate migration step.
	require.NoError(t, client.Ent().Schema.Create(ctx, migrate.WithDropIndex(true)))
	invocation, err := client.Ent().BazelInvocation.Create().
		SetInvocationID(uuid.New()).SetInstanceNameID(instance.ID).
		SetCreatedTimestamp(time.Now()).Save(ctx)
	require.NoError(t, err)
	newBuild, err := client.Ent().Build.Create().SetBuildUUID(uuid.New()).
		SetInstanceNameID(instance.ID).SetTimestamp(time.Now()).
		AddInvocationIDs(invocation.ID).Save(ctx)
	require.NoError(t, err)
	gotBuildID, err := invocation.QueryBuild().OnlyID(ctx)
	require.NoError(t, err)
	require.Equal(t, newBuild.ID, gotBuildID)
	var legacyURL string
	require.NoError(t, connection.QueryRowContext(ctx, `SELECT "build_url" FROM "builds" WHERE "id" = $1`, legacyBuild.ID).Scan(&legacyURL))
	require.Equal(t, "https://ci.example/legacy", legacyURL)
	var newURL sql.NullString
	require.NoError(t, connection.QueryRowContext(ctx, `SELECT "build_url" FROM "builds" WHERE "id" = $1`, newBuild.ID).Scan(&newURL))
	require.False(t, newURL.Valid)

	var pending bytes.Buffer
	require.NoError(t, client.Ent().Schema.WriteTo(ctx, &pending, migrate.WithDropIndex(true)))
	require.Empty(t, pending.String(), "a later startup must not modify the schema again")
}
