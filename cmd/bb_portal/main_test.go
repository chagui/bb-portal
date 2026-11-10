package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/buildbarn/bb-portal/ent/gen/ent/migrate"
	"github.com/buildbarn/bb-portal/internal/database"
	"github.com/buildbarn/bb-portal/internal/database/dbauthservice"
	"github.com/buildbarn/bb-portal/internal/database/embedded"
	prometheusmetrics "github.com/buildbarn/bb-portal/pkg/prometheus_metrics"
	"github.com/buildbarn/bb-portal/pkg/proto/configuration/bb_portal"
	bb_zstd "github.com/buildbarn/bb-storage/pkg/zstd"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/protobuf/encoding/protojson"
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

// parseDatabaseConfiguration parses the JSON form of a Database message,
// with extraFields (each followed by a comma) added to a valid base.
// Configurations are parsed from JSON rather than built as Go literals so
// that an omitted field and an empty message are genuinely different
// inputs, as they are for operators writing jsonnet.
func parseDatabaseConfiguration(t *testing.T, extraFields string) *bb_portal.Database {
	var configuration bb_portal.Database
	require.NoError(t, protojson.Unmarshal([]byte(`{
		`+extraFields+`
		"cleanupConfiguration": {
			"cleanupInterval": "60s",
			"invocationMessageTimeout": "3600s",
			"invocationRetention": "604800s"
		}
	}`), &configuration))
	return &configuration
}

func newTestConnection(t *testing.T) *sql.DB {
	connection, err := dbProvider.CreateDatabase()
	require.NoError(t, err)
	t.Cleanup(func() { connection.Close() })
	return connection
}

func countPublicTables(t *testing.T, connection *sql.DB) int {
	var count int
	require.NoError(t, connection.QueryRow(
		"SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'",
	).Scan(&count))
	return count
}

func TestInitializeDatabaseSchemaManagement(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extraFields string
		wantApplied bool
	}{
		{name: "Omitted", extraFields: "", wantApplied: true},
		{name: "EmptyMessage", extraFields: `"schemaManagement": {},`, wantApplied: true},
		{name: "Apply", extraFields: `"schemaManagement": {"apply": {}},`, wantApplied: true},
		{name: "Skip", extraFields: `"schemaManagement": {"skip": {}},`, wantApplied: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connection := newTestConnection(t)
			configuration := parseDatabaseConfiguration(t, tc.extraFields)

			dbClient, dbCleanupService, err := initializeDatabase(
				context.Background(),
				configuration,
				"postgres",
				connection,
				noop.NewTracerProvider(),
				bb_zstd.NewUnboundedPool(nil, nil),
			)
			require.NoError(t, err)

			// The remaining initialization must happen regardless of
			// whether the schema was applied.
			require.NotNil(t, dbClient)
			require.NotNil(t, dbCleanupService)

			if tc.wantApplied {
				require.NotZero(t, countPublicTables(t, connection), "Schema.Create should have created tables")
			} else {
				require.Zero(t, countPublicTables(t, connection), "Schema.Create should not have been called")
			}
		})
	}
}

// TestInitializeDatabaseSkipUsesExistingSchema simulates
// an externally managed schema: the schema is installed before startup,
// and initialization with schema management skipped must still sync metrics
// from it.
func TestInitializeDatabaseSkipUsesExistingSchema(t *testing.T) {
	ctx := dbauthservice.NewContextWithDbAuthServiceBypass(context.Background())
	connection := newTestConnection(t)

	externalMigrator, err := database.New("postgres", connection)
	require.NoError(t, err)
	require.NoError(t, externalMigrator.Ent().Schema.Create(ctx, migrate.WithDropIndex(true)))
	_, err = externalMigrator.Ent().AuthenticatedUser.Create().
		SetUserUUID(uuid.New()).
		SetExternalID("external-id").
		Save(ctx)
	require.NoError(t, err)

	// Use a sentinel so the assertion below cannot pass on a value
	// left behind by another test.
	prometheusmetrics.AuthenticatedUsersCount.Set(-1)

	dbClient, dbCleanupService, err := initializeDatabase(
		context.Background(),
		parseDatabaseConfiguration(t, `"schemaManagement": {"skip": {}},`),
		"postgres",
		connection,
		noop.NewTracerProvider(),
		bb_zstd.NewUnboundedPool(nil, nil),
	)
	require.NoError(t, err)
	require.NotNil(t, dbClient)
	require.NotNil(t, dbCleanupService)
	require.Equal(t, 1.0, testutil.ToFloat64(prometheusmetrics.AuthenticatedUsersCount))
}
