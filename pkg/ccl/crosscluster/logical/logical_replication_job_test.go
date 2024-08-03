// Copyright 2024 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package logical

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdcevent"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvpb"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver"
	"github.com/cockroachdb/cockroach/pkg/repstream/streampb"
	"github.com/cockroachdb/cockroach/pkg/sql"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/desctestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/randgen"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/jobutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/skip"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/testcluster"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/span"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

var (
	testClusterSystemSettings = []string{
		"SET CLUSTER SETTING kv.rangefeed.enabled = true",
		"SET CLUSTER SETTING kv.rangefeed.closed_timestamp_refresh_interval = '200ms'",
		"SET CLUSTER SETTING kv.closed_timestamp.target_duration = '100ms'",
		"SET CLUSTER SETTING kv.closed_timestamp.side_transport_interval = '50ms'",
	}
	testClusterSettings = []string{
		"SET CLUSTER SETTING physical_replication.producer.timestamp_granularity = '0s'",
		"SET CLUSTER SETTING physical_replication.producer.min_checkpoint_frequency='100ms'",
		"SET CLUSTER SETTING logical_replication.consumer.heartbeat_frequency = '1s'",
		"SET CLUSTER SETTING logical_replication.consumer.job_checkpoint_frequency = '100ms'",
	}

	testClusterBaseClusterArgs = base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestIsForStuffThatShouldWorkWithSecondaryTenantsButDoesntYet(127241),
			Knobs: base.TestingKnobs{
				JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
			},
		},
	}

	lwwColumnAdd = "ADD COLUMN crdb_replication_origin_timestamp DECIMAL NOT VISIBLE DEFAULT NULL ON UPDATE NULL"
)

func TestLogicalStreamIngestionJobNameResolution(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	type testCase struct {
		name                string
		setup               func(*testing.T, *sqlutils.SQLRunner, *sqlutils.SQLRunner)
		localStmtTableName  string
		remoteStmtTableName string
		expectedErr         string
	}
	cases := []testCase{
		{
			name: "table in schema",
			setup: func(t *testing.T, dbA *sqlutils.SQLRunner, dbB *sqlutils.SQLRunner) {
				dbA.Exec(t, `CREATE SCHEMA foo`)
				createBasicTable(t, dbA, "foo.bar")
				dbB.Exec(t, `CREATE SCHEMA foo`)
				createBasicTable(t, dbB, "foo.bar")
			},
			localStmtTableName:  "foo.bar",
			remoteStmtTableName: "foo.bar",
		},
		{
			name: "table in schema with special characters",
			setup: func(t *testing.T, dbA *sqlutils.SQLRunner, dbB *sqlutils.SQLRunner) {
				dbA.Exec(t, `CREATE SCHEMA "foo-bar"`)
				createBasicTable(t, dbA, `"foo-bar".bar`)
				dbB.Exec(t, `CREATE SCHEMA "foo-bar"`)
				createBasicTable(t, dbB, `"foo-bar".bar`)
			},
			localStmtTableName:  `"foo-bar".bar`,
			remoteStmtTableName: `"foo-bar".bar`,
		},
		{
			name: "table with special characters in schema with special characters",
			setup: func(t *testing.T, dbA *sqlutils.SQLRunner, dbB *sqlutils.SQLRunner) {
				dbA.Exec(t, `CREATE SCHEMA "foo-bar2"`)
				createBasicTable(t, dbA, `"foo-bar2"."baz-bat"`)
				dbB.Exec(t, `CREATE SCHEMA "foo-bar2"`)
				createBasicTable(t, dbB, `"foo-bar2"."baz-bat"`)
			},
			localStmtTableName:  `"foo-bar2"."baz-bat"`,
			remoteStmtTableName: `"foo-bar2"."baz-bat"`,
		},
		{
			name: "table with periods in schema with periods",
			setup: func(t *testing.T, dbA *sqlutils.SQLRunner, dbB *sqlutils.SQLRunner) {
				dbA.Exec(t, `CREATE SCHEMA "foo.bar"`)
				createBasicTable(t, dbA, `"foo.bar"."baz.bat"`)
				dbB.Exec(t, `CREATE SCHEMA "foo.bar"`)
				createBasicTable(t, dbB, `"foo.bar"."baz.bat"`)
			},
			localStmtTableName:  `"foo.bar"."baz.bat"`,
			remoteStmtTableName: `"foo.bar"."baz.bat"`,
		},
		{
			name: "table in public schema",
			setup: func(t *testing.T, dbA *sqlutils.SQLRunner, dbB *sqlutils.SQLRunner) {
				createBasicTable(t, dbA, "bar")
				createBasicTable(t, dbB, "bar")
			},
			localStmtTableName:  "public.bar",
			remoteStmtTableName: "public.bar",
		},
	}

	server, s, dbA, dbB := setupLogicalTestServer(t, ctx, testClusterBaseClusterArgs)
	defer server.Stopper().Stop(ctx)
	dbBURL, cleanupB := s.PGUrl(t, serverutils.DBName("b"))
	defer cleanupB()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.setup(t, dbA, dbB)
			query := fmt.Sprintf("CREATE LOGICAL REPLICATION STREAM FROM TABLE %s ON $1 INTO TABLE %s",
				c.remoteStmtTableName, c.localStmtTableName)
			if c.expectedErr != "" {
				dbA.ExpectErr(t, c.expectedErr, query, dbBURL.String())
			} else {
				var unusedID int
				dbA.QueryRow(t, query, dbBURL.String()).Scan(&unusedID)
			}
		})
	}
}

type fatalDLQ struct{ *testing.T }

func (fatalDLQ) Create(ctx context.Context) error { return nil }

func (t fatalDLQ) Log(
	_ context.Context,
	_ int64,
	_ streampb.StreamEvent_KV,
	cdcEventRow cdcevent.Row,
	reason error,
	_ retryEligibility,
) error {
	t.Fatal(errors.Wrapf(reason, "failed to apply row update: %s", cdcEventRow.DebugString()))
	return nil
}

func TestLogicalStreamIngestionJob(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	defer TestingSetDLQ(fatalDLQ{t})()

	ctx := context.Background()
	// keyPrefix will be set later, but before countPuts is set.
	var keyPrefix []byte
	var countPuts atomic.Bool
	var numPuts, numCPuts atomic.Int64
	// seenPuts and seenCPuts track which transactions have already been counted
	// in the number of Puts and CPuts, respectively (we want to ignore any txn
	// retries).
	seenPuts, seenCPuts := make(map[uuid.UUID]struct{}), make(map[uuid.UUID]struct{})
	var muSeenTxns syncutil.Mutex
	// seenTxn returns whether we've already seen this txn and includes it into
	// the map if not.
	seenTxn := func(seenTxns map[uuid.UUID]struct{}, txnID uuid.UUID) bool {
		muSeenTxns.Lock()
		defer muSeenTxns.Unlock()
		_, seen := seenTxns[txnID]
		seenTxns[txnID] = struct{}{}
		return seen
	}
	clusterArgs := base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestControlsTenantsExplicitly,
			Knobs: base.TestingKnobs{
				JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
				Store: &kvserver.StoreTestingKnobs{
					TestingRequestFilter: func(_ context.Context, ba *kvpb.BatchRequest) *kvpb.Error {
						if !countPuts.Load() || !ba.IsWrite() || len(ba.Requests) > 2 {
							return nil
						}
						switch req := ba.Requests[0].GetInner().(type) {
						case *kvpb.PutRequest:
							if bytes.HasPrefix(req.Key, keyPrefix) && !seenTxn(seenPuts, ba.Txn.ID) {
								numPuts.Add(1)
							}
							return nil
						case *kvpb.ConditionalPutRequest:
							if bytes.HasPrefix(req.Key, keyPrefix) && !seenTxn(seenCPuts, ba.Txn.ID) {
								numCPuts.Add(1)
							}
							return nil
						default:
							return nil
						}
					},
				},
			},
		},
	}

	server, s, dbA, dbB := setupLogicalTestServer(t, ctx, clusterArgs)
	defer server.Stopper().Stop(ctx)

	retryQueueSizeLimit.Override(ctx, &s.ClusterSettings().SV, 0)

	desc := desctestutils.TestingGetPublicTableDescriptor(s.DB(), s.Codec(), "a", "tab")
	keyPrefix = rowenc.MakeIndexKeyPrefix(s.Codec(), desc.GetID(), desc.GetPrimaryIndexID())
	countPuts.Store(true)

	dbA.Exec(t, "INSERT INTO tab VALUES (1, 'hello')")
	dbB.Exec(t, "INSERT INTO tab VALUES (1, 'goodbye')")

	dbAURL, cleanup := s.PGUrl(t, serverutils.DBName("a"))
	defer cleanup()
	dbBURL, cleanupB := s.PGUrl(t, serverutils.DBName("b"))
	defer cleanupB()

	// Swap one of the URLs to external:// to verify this indirection works.
	// TODO(dt): this create should support placeholder for URI.
	dbB.Exec(t, "CREATE EXTERNAL CONNECTION a AS '"+dbAURL.String()+"'")
	dbAURL = url.URL{
		Scheme: "external",
		Host:   "a",
	}

	var (
		jobAID jobspb.JobID
		jobBID jobspb.JobID
	)
	dbA.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab", dbBURL.String()).Scan(&jobAID)
	dbB.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab", dbAURL.String()).Scan(&jobBID)

	now := server.Server(0).Clock().Now()
	t.Logf("waiting for replication job %d", jobAID)
	WaitUntilReplicatedTime(t, now, dbA, jobAID)
	t.Logf("waiting for replication job %d", jobBID)
	WaitUntilReplicatedTime(t, now, dbB, jobBID)

	dbA.Exec(t, "INSERT INTO tab VALUES (2, 'potato')")
	dbB.Exec(t, "INSERT INTO tab VALUES (3, 'celeriac')")
	dbA.Exec(t, "UPSERT INTO tab VALUES (1, 'hello, again')")
	dbB.Exec(t, "UPSERT INTO tab VALUES (1, 'goodbye, again')")

	now = server.Server(0).Clock().Now()
	WaitUntilReplicatedTime(t, now, dbA, jobAID)
	WaitUntilReplicatedTime(t, now, dbB, jobBID)

	expectedRows := [][]string{
		{"1", "goodbye, again"},
		{"2", "potato"},
		{"3", "celeriac"},
	}
	dbA.CheckQueryResults(t, "SELECT * from a.tab", expectedRows)
	dbB.CheckQueryResults(t, "SELECT * from b.tab", expectedRows)

	// Verify that we didn't have the data looping problem. We expect 3 CPuts
	// when inserting new rows and 3 Puts when updating existing rows.
	expPuts, expCPuts := 3, 3
	if tryOptimisticInsertEnabled.Get(&s.ClusterSettings().SV) {
		// When performing 1 update, we don't have the prevValue set, so if
		// we're using the optimistic insert strategy, it would result in an
		// additional CPut (that ultimately fails). The cluster setting is
		// randomized in tests, so we need to handle both cases.
		expCPuts++
	}
	require.Equal(t, int64(expPuts), numPuts.Load())
	require.Equal(t, int64(expCPuts), numCPuts.Load())
}

func TestLogicalStreamIngestionJobWithCursor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	server, s, dbA, dbB := setupLogicalTestServer(t, ctx, testClusterBaseClusterArgs)
	defer server.Stopper().Stop(ctx)

	dbA.Exec(t, "INSERT INTO tab VALUES (1, 'hello')")
	dbB.Exec(t, "INSERT INTO tab VALUES (1, 'goodbye')")

	dbAURL, cleanup := s.PGUrl(t, serverutils.DBName("a"))
	defer cleanup()
	dbBURL, cleanupB := s.PGUrl(t, serverutils.DBName("b"))
	defer cleanupB()

	// Swap one of the URLs to external:// to verify this indirection works.
	// TODO(dt): this create should support placeholder for URI.
	dbB.Exec(t, "CREATE EXTERNAL CONNECTION a AS '"+dbAURL.String()+"'")
	dbAURL = url.URL{
		Scheme: "external",
		Host:   "a",
	}

	var (
		jobAID jobspb.JobID
		jobBID jobspb.JobID
	)

	// Perform inserts that should not be replicated since
	// they will be before the cursor time.
	dbA.Exec(t, "INSERT INTO tab VALUES (7, 'do not replicate')")
	dbB.Exec(t, "INSERT INTO tab VALUES (8, 'do not replicate')")
	// Perform the inserts first before starting the LDR stream.
	now := server.Server(0).Clock().Now()
	dbA.Exec(t, "INSERT INTO tab VALUES (2, 'potato')")
	dbB.Exec(t, "INSERT INTO tab VALUES (3, 'celeriac')")
	dbA.Exec(t, "UPSERT INTO tab VALUES (1, 'hello, again')")
	dbB.Exec(t, "UPSERT INTO tab VALUES (1, 'goodbye, again')")
	// We should expect starting at the provided now() to replicate all the data from that time.
	dbA.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab WITH CURSOR=$2", dbBURL.String(), now.AsOfSystemTime()).Scan(&jobAID)
	dbB.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab WITH CURSOR=$2", dbAURL.String(), now.AsOfSystemTime()).Scan(&jobBID)

	now = server.Server(0).Clock().Now()
	t.Logf("waiting for replication job %d", jobAID)
	WaitUntilReplicatedTime(t, now, dbA, jobAID)
	t.Logf("waiting for replication job %d", jobBID)
	WaitUntilReplicatedTime(t, now, dbB, jobBID)

	// The rows added before the now time should remain only
	// on their respective side and not replicate.
	expectedRowsA := [][]string{
		{"1", "goodbye, again"},
		{"2", "potato"},
		{"3", "celeriac"},
		{"7", "do not replicate"},
	}
	expectedRowsB := [][]string{
		{"1", "goodbye, again"},
		{"2", "potato"},
		{"3", "celeriac"},
		{"8", "do not replicate"},
	}
	dbA.CheckQueryResults(t, "SELECT * from a.tab", expectedRowsA)
	dbB.CheckQueryResults(t, "SELECT * from b.tab", expectedRowsB)
}

func TestLogicalStreamIngestionErrors(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	server := testcluster.StartTestCluster(t, 1, base.TestClusterArgs{})
	defer server.Stopper().Stop(ctx)
	s := server.Server(0).ApplicationLayer()
	url, cleanup := s.PGUrl(t, serverutils.DBName("a"))
	defer cleanup()
	urlA := url.String()

	_, err := server.Conns[0].Exec("CREATE DATABASE a")
	require.NoError(t, err)
	_, err = server.Conns[0].Exec("CREATE DATABASE B")
	require.NoError(t, err)

	dbA := sqlutils.MakeSQLRunner(s.SQLConn(t, serverutils.DBName("a")))
	dbB := sqlutils.MakeSQLRunner(s.SQLConn(t, serverutils.DBName("b")))

	createStmt := "CREATE TABLE tab (pk int primary key, payload string)"
	dbA.Exec(t, createStmt)
	dbB.Exec(t, createStmt)

	createQ := "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab"

	dbB.ExpectErrWithHint(t, "currently require a .* DECIMAL column", "ADD COLUMN", createQ, urlA)

	dbB.Exec(t, "ALTER TABLE tab ADD COLUMN crdb_replication_origin_timestamp STRING")
	dbB.ExpectErr(t, ".*column must be type DECIMAL for use by logical replication", createQ, urlA)

	dbB.Exec(t, fmt.Sprintf("ALTER TABLE tab RENAME COLUMN %[1]s TO str_col, ADD COLUMN %[1]s DECIMAL", originTimestampColumnName))

	if s.Codec().IsSystem() {
		dbB.ExpectErr(t, "kv.rangefeed.enabled must be enabled on the source cluster for logical replication", createQ, urlA)
		kvserver.RangefeedEnabled.Override(ctx, &server.Server(0).ClusterSettings().SV, true)
	}

	dbB.Exec(t, createQ, urlA)
}

func TestLogicalStreamIngestionJobWithColumnFamilies(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()

	tc, s, serverASQL, serverBSQL := setupLogicalTestServer(t, ctx, testClusterBaseClusterArgs)
	defer tc.Stopper().Stop(ctx)

	createStmt := `CREATE TABLE tab_with_cf (
pk int primary key,
payload string,
v1 int as (pk + 9000) virtual,
v2 int as (pk + 42) stored,
other_payload string,
family f1(pk, payload),
family f2(other_payload, v2))
`
	serverASQL.Exec(t, createStmt)
	serverBSQL.Exec(t, createStmt)
	serverASQL.Exec(t, "ALTER TABLE tab_with_cf "+lwwColumnAdd)
	serverBSQL.Exec(t, "ALTER TABLE tab_with_cf "+lwwColumnAdd)

	serverASQL.Exec(t, "INSERT INTO tab_with_cf(pk, payload, other_payload) VALUES (1, 'hello', 'ruroh1')")

	serverAURL, cleanup := s.PGUrl(t)
	serverAURL.Path = "a"
	defer cleanup()

	var jobBID jobspb.JobID
	serverBSQL.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab_with_cf ON $1 INTO TABLE tab_with_cf", serverAURL.String()).Scan(&jobBID)

	WaitUntilReplicatedTime(t, s.Clock().Now(), serverBSQL, jobBID)
	serverASQL.Exec(t, "INSERT INTO tab_with_cf(pk, payload, other_payload) VALUES (2, 'potato', 'ruroh2')")
	serverASQL.Exec(t, "INSERT INTO tab_with_cf(pk, payload, other_payload) VALUES (4, 'spud', 'shrub')")
	serverASQL.Exec(t, "UPSERT INTO tab_with_cf(pk, payload, other_payload) VALUES (1, 'hello, again', 'ruroh3')")
	serverASQL.Exec(t, "DELETE FROM tab_with_cf WHERE pk = 4")

	WaitUntilReplicatedTime(t, s.Clock().Now(), serverBSQL, jobBID)

	expectedRows := [][]string{
		{"1", "hello, again", "9001", "43", "ruroh3"},
		{"2", "potato", "9002", "44", "ruroh2"},
	}
	serverBSQL.CheckQueryResults(t, "SELECT * from tab_with_cf", expectedRows)
	serverASQL.CheckQueryResults(t, "SELECT * from tab_with_cf", expectedRows)
}

func TestRandomTables(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	tc, s, runnerA, runnerB := setupLogicalTestServer(t, ctx, testClusterBaseClusterArgs)
	defer tc.Stopper().Stop(ctx)

	sqlA := s.SQLConn(t, serverutils.DBName("a"))

	tableName := "rand_table"
	rng, _ := randutil.NewPseudoRand()
	createStmt := randgen.RandCreateTableWithName(
		ctx,
		rng,
		tableName,
		1,
		false, /* isMultiregion */
		// We do not have full support for column families.
		randgen.SkipColumnFamilyMutation())
	stmt := tree.SerializeForDisplay(createStmt)
	t.Logf(stmt)
	runnerA.Exec(t, stmt)
	runnerB.Exec(t, stmt)

	numInserts := 20
	_, err := randgen.PopulateTableWithRandData(rng,
		sqlA, tableName, numInserts, nil)
	require.NoError(t, err)

	addCol := fmt.Sprintf(`ALTER TABLE %s `+lwwColumnAdd, tableName)
	runnerA.Exec(t, addCol)
	runnerB.Exec(t, addCol)

	dbAURL, cleanup := s.PGUrl(t, serverutils.DBName("a"))
	defer cleanup()

	streamStartStmt := fmt.Sprintf("CREATE LOGICAL REPLICATION STREAM FROM TABLE %[1]s ON $1 INTO TABLE %[1]s", tableName)
	var jobBID jobspb.JobID
	runnerB.QueryRow(t, streamStartStmt, dbAURL.String()).Scan(&jobBID)

	t.Logf("waiting for replication job %d", jobBID)
	WaitUntilReplicatedTime(t, s.Clock().Now(), runnerB, jobBID)

	compareReplicatedTables(t, s, "a", "b", tableName, runnerA, runnerB)
}

// TestPreviouslyInterestingTables tests some schemas from previous failed runs of TestRandomTables.
func TestPreviouslyInterestingTables(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	tc, s, runnerA, runnerB := setupLogicalTestServer(t, ctx, testClusterBaseClusterArgs)
	defer tc.Stopper().Stop(ctx)

	sqlA := s.SQLConn(t, serverutils.DBName("a"))

	type testCase struct {
		name   string
		schema string
		useUDF bool
		delete bool
	}

	testCases := []testCase{
		{
			// This caught a problem with the comparison we were
			// using rather than the replication process itself. We
			// leave it here as an example of how to add new
			// schemas.
			name:   "comparison-invariant-to-different-covering-indexes",
			schema: `CREATE TABLE rand_table (col1_0 DECIMAL, INDEX (col1_0) VISIBILITY 0.17, UNIQUE (col1_0 DESC), UNIQUE (col1_0 ASC), INDEX (col1_0 ASC), UNIQUE (col1_0 ASC))`,
		},
		{

			// Single column tables previously had a bug
			// with tuple parsing the UDF apply query.
			name:   "single column table with udf",
			schema: `CREATE TABLE rand_table (pk INT PRIMARY KEY)`,
			useUDF: true,
		},
	}

	baseTableName := "rand_table"
	rng, _ := randutil.NewPseudoRand()
	numInserts := 20
	dbAURL, cleanup := s.PGUrl(t, serverutils.DBName("a"))
	defer cleanup()
	for i, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.useUDF {
				defaultSQLProcessor = udfApplierProcessor
				defer func() {
					defaultSQLProcessor = lwwProcessor
				}()
			}

			tableName := fmt.Sprintf("%s%d", baseTableName, i)
			schemaStmt := strings.ReplaceAll(tc.schema, baseTableName, tableName)
			addCol := fmt.Sprintf(`ALTER TABLE %s `+lwwColumnAdd, tableName)
			runnerA.Exec(t, schemaStmt)
			runnerB.Exec(t, schemaStmt)
			runnerA.Exec(t, addCol)
			runnerB.Exec(t, addCol)
			if tc.useUDF {
				runnerB.Exec(t, fmt.Sprintf(testingUDFAcceptProposedBase, tableName))
			}
			_, err := randgen.PopulateTableWithRandData(rng,
				sqlA, tableName, numInserts, nil)
			require.NoError(t, err)

			var streamStartStmt string
			if tc.useUDF {
				streamStartStmt = fmt.Sprintf("CREATE LOGICAL REPLICATION STREAM FROM TABLE %[1]s ON $1 INTO TABLE %[1]s WITH FUNCTION repl_apply FOR TABLE %[1]s", tableName)
			} else {
				streamStartStmt = fmt.Sprintf("CREATE LOGICAL REPLICATION STREAM FROM TABLE %[1]s ON $1 INTO TABLE %[1]s", tableName)
			}
			var jobBID jobspb.JobID
			runnerB.QueryRow(t, streamStartStmt, dbAURL.String()).Scan(&jobBID)

			t.Logf("waiting for replication job %d", jobBID)
			WaitUntilReplicatedTime(t, s.Clock().Now(), runnerB, jobBID)

			if tc.delete {
				runnerA.Exec(t, fmt.Sprintf("DELETE FROM %s LIMIT 5", tableName))
				WaitUntilReplicatedTime(t, s.Clock().Now(), runnerB, jobBID)
			}

			compareReplicatedTables(t, s, "a", "b", tableName, runnerA, runnerB)
		})
	}
}

// TestLogicalAutoReplan asserts that if a new node can participate in the
// logical replication job, it will trigger distSQL replanning.
func TestLogicalAutoReplan(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t, "multi cluster/node config exhausts hardware")

	ctx := context.Background()

	// Double the number of nodes
	retryErrorChan := make(chan error, 4)
	turnOffReplanning := make(chan struct{})
	var alreadyReplanned atomic.Bool

	// Track the number of unique addresses that we're connected to.
	clientAddresses := make(map[string]struct{})
	var addressesMu syncutil.Mutex

	clusterArgs := base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestControlsTenantsExplicitly,
			Knobs: base.TestingKnobs{
				JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
				DistSQL: &execinfra.TestingKnobs{
					StreamingTestingKnobs: &sql.StreamingTestingKnobs{
						BeforeClientSubscribe: func(addr string, token string, _ span.Frontier) {
							addressesMu.Lock()
							defer addressesMu.Unlock()
							clientAddresses[addr] = struct{}{}
						},
					},
				},
				Streaming: &sql.StreamingTestingKnobs{
					AfterRetryIteration: func(err error) {
						if err != nil && !alreadyReplanned.Load() {
							retryErrorChan <- err
							<-turnOffReplanning
							alreadyReplanned.Swap(true)
						}
					},
				},
			},
		},
	}

	server, s, dbA, dbB := setupLogicalTestServer(t, ctx, clusterArgs)
	defer server.Stopper().Stop(ctx)

	// Don't allow for replanning until the new nodes and scattered table have been created.
	serverutils.SetClusterSetting(t, server, "logical_replication.replan_flow_threshold", 0)
	serverutils.SetClusterSetting(t, server, "logical_replication.replan_flow_frequency", time.Millisecond*500)

	dbAURL, cleanup := s.PGUrl(t, serverutils.DBName("a"))
	defer cleanup()
	dbBURL, cleanupB := s.PGUrl(t, serverutils.DBName("b"))
	defer cleanupB()

	var (
		jobAID jobspb.JobID
		jobBID jobspb.JobID
	)
	dbA.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab", dbBURL.String()).Scan(&jobAID)
	dbB.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab", dbAURL.String()).Scan(&jobBID)

	now := server.Server(0).Clock().Now()
	t.Logf("waiting for replication job %d", jobAID)
	WaitUntilReplicatedTime(t, now, dbA, jobAID)
	t.Logf("waiting for replication job %d", jobBID)
	WaitUntilReplicatedTime(t, now, dbB, jobBID)

	server.AddAndStartServer(t, clusterArgs.ServerArgs)
	server.AddAndStartServer(t, clusterArgs.ServerArgs)
	t.Logf("New nodes added")

	// Only need at least two nodes as leaseholders for test.
	CreateScatteredTable(t, dbA, 2)

	// Configure the ingestion job to replan eagerly.
	serverutils.SetClusterSetting(t, server, "logical_replication.replan_flow_threshold", 0.1)

	// The ingestion job should eventually retry because it detects new nodes to add to the plan.
	require.ErrorContains(t, <-retryErrorChan, sql.ErrPlanChanged.Error())

	// Prevent continuous replanning to reduce test runtime. dsp.PartitionSpans()
	// on the src cluster may return a different set of src nodes that can
	// participate in the replication job (especially under stress), so if we
	// repeatedly replan the job, we will repeatedly restart the job, preventing
	// job progress.
	serverutils.SetClusterSetting(t, server, "logical_replication.replan_flow_threshold", 0)
	serverutils.SetClusterSetting(t, server, "logical_replication.replan_flow_frequency", time.Minute*10)
	close(turnOffReplanning)

	require.Greater(t, len(clientAddresses), 1)
}

func TestHeartbeatCancel(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	skip.UnderRace(t, "multi cluster/node config exhausts hardware")

	ctx := context.Background()

	// Make size of channel double the number of nodes
	retryErrorChan := make(chan error, 4)
	var alreadyCancelled atomic.Bool

	clusterArgs := base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestControlsTenantsExplicitly,
			Knobs: base.TestingKnobs{
				JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
				Streaming: &sql.StreamingTestingKnobs{
					AfterRetryIteration: func(err error) {
						if err != nil && !alreadyCancelled.Load() {
							retryErrorChan <- err
							alreadyCancelled.Store(true)
						}
					},
				},
			},
		},
	}

	server, s, dbA, dbB := setupLogicalTestServer(t, ctx, clusterArgs)
	defer server.Stopper().Stop(ctx)

	serverutils.SetClusterSetting(t, server, "logical_replication.consumer.heartbeat_frequency", time.Second*1)

	dbAURL, cleanup := s.PGUrl(t, serverutils.DBName("a"))
	defer cleanup()
	dbBURL, cleanupB := s.PGUrl(t, serverutils.DBName("b"))
	defer cleanupB()

	var (
		jobAID jobspb.JobID
		jobBID jobspb.JobID
	)
	dbA.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab", dbBURL.String()).Scan(&jobAID)
	dbB.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab", dbAURL.String()).Scan(&jobBID)

	now := server.Server(0).Clock().Now()
	t.Logf("waiting for replication job %d", jobAID)
	WaitUntilReplicatedTime(t, now, dbA, jobAID)
	t.Logf("waiting for replication job %d", jobBID)
	WaitUntilReplicatedTime(t, now, dbB, jobBID)

	var prodAID jobspb.JobID
	dbA.QueryRow(t, "SELECT job_ID FROM [SHOW JOBS] WHERE job_type='REPLICATION STREAM PRODUCER'").Scan(&prodAID)

	// Cancel the producer job and wait for the hearbeat to pick up that the stream is inactive
	t.Logf("Canceling  replication producer %s", prodAID)
	dbA.QueryRow(t, "CANCEL JOB $1", prodAID)

	// The ingestion job should eventually retry because it detects 2 nodes are dead
	require.ErrorContains(t, <-retryErrorChan, fmt.Sprintf("replication stream %s is not running, status is STREAM_INACTIVE", prodAID))
}

func setupLogicalTestServer(
	t *testing.T, ctx context.Context, clusterArgs base.TestClusterArgs,
) (
	*testcluster.TestCluster,
	serverutils.ApplicationLayerInterface,
	*sqlutils.SQLRunner,
	*sqlutils.SQLRunner,
) {
	server := testcluster.StartTestCluster(t, 1, clusterArgs)
	s := server.Server(0).ApplicationLayer()

	_, err := server.Conns[0].Exec("SET CLUSTER SETTING physical_replication.producer.timestamp_granularity = '0s'")
	require.NoError(t, err)
	_, err = server.Conns[0].Exec("CREATE DATABASE a")
	require.NoError(t, err)
	_, err = server.Conns[0].Exec("CREATE DATABASE B")
	require.NoError(t, err)

	dbA := sqlutils.MakeSQLRunner(s.SQLConn(t, serverutils.DBName("a")))
	dbB := sqlutils.MakeSQLRunner(s.SQLConn(t, serverutils.DBName("b")))

	sysDB := sqlutils.MakeSQLRunner(server.Server(0).SystemLayer().SQLConn(t))
	for _, s := range testClusterSystemSettings {
		sysDB.Exec(t, s)
	}

	for _, s := range testClusterSettings {
		dbA.Exec(t, s)
	}
	createBasicTable(t, dbA, "tab")
	createBasicTable(t, dbB, "tab")
	return server, s, dbA, dbB
}

func createBasicTable(t *testing.T, db *sqlutils.SQLRunner, tableName string) {
	createStmt := fmt.Sprintf("CREATE TABLE %s (pk int primary key, payload string)", tableName)
	db.Exec(t, createStmt)
	db.Exec(t, fmt.Sprintf("ALTER TABLE %s %s", tableName, lwwColumnAdd))
}

func compareReplicatedTables(
	t *testing.T,
	s serverutils.ApplicationLayerInterface,
	dbA, dbB, tableName string,
	runnerA, runnerB *sqlutils.SQLRunner,
) {
	descA := desctestutils.TestingGetPublicTableDescriptor(s.DB(), s.Codec(), dbA, tableName)
	descB := desctestutils.TestingGetPublicTableDescriptor(s.DB(), s.Codec(), dbB, tableName)

	for _, indexA := range descA.AllIndexes() {
		if indexA.GetType() == descpb.IndexDescriptor_INVERTED {
			t.Logf("skipping fingerprinting of inverted index %s", indexA.GetName())
			continue
		}

		indexB, err := catalog.MustFindIndexByName(descB, indexA.GetName())
		require.NoError(t, err)

		aFingerprintQuery, err := sql.BuildFingerprintQueryForIndex(descA, indexA, []string{originTimestampColumnName})
		require.NoError(t, err)
		bFingerprintQuery, err := sql.BuildFingerprintQueryForIndex(descB, indexB, []string{originTimestampColumnName})
		require.NoError(t, err)
		t.Logf("fingerprinting index %s", indexA.GetName())
		runnerB.CheckQueryResults(t, bFingerprintQuery, runnerA.QueryStr(t, aFingerprintQuery))
	}
}

func CreateScatteredTable(t *testing.T, db *sqlutils.SQLRunner, numNodes int) {
	// Create a source table with multiple ranges spread across multiple nodes. We
	// need around 50 or more ranges because there are already over 50 system
	// ranges, so if we write just a few ranges those might all be on a single
	// server, which will cause the test to flake.
	numRanges := 50
	rowsPerRange := 20
	db.Exec(t, "INSERT INTO tab (pk) SELECT * FROM generate_series(1, $1)",
		numRanges*rowsPerRange)
	db.Exec(t, "ALTER TABLE tab SPLIT AT (SELECT * FROM generate_series($1::INT, $2::INT, $3::INT))",
		rowsPerRange, (numRanges-1)*rowsPerRange, rowsPerRange)
	db.Exec(t, "ALTER TABLE tab SCATTER")
	timeout := 45 * time.Second
	if skip.Duress() {
		timeout *= 5
	}
	testutils.SucceedsWithin(t, func() error {
		var leaseHolderCount int
		db.QueryRow(t,
			`SELECT count(DISTINCT lease_holder) FROM [SHOW RANGES FROM DATABASE A WITH DETAILS]`).
			Scan(&leaseHolderCount)
		require.Greater(t, leaseHolderCount, 0)
		if leaseHolderCount < numNodes {
			return errors.New("leaseholders not scattered yet")
		}
		return nil
	}, timeout)
}

func WaitUntilReplicatedTime(
	t *testing.T, targetTime hlc.Timestamp, db *sqlutils.SQLRunner, ingestionJobID jobspb.JobID,
) {
	testutils.SucceedsSoon(t, func() error {
		progress := jobutils.GetJobProgress(t, db, ingestionJobID)
		replicatedTime := progress.Details.(*jobspb.Progress_LogicalReplication).LogicalReplication.ReplicatedTime
		if replicatedTime.IsEmpty() {
			return errors.Newf("stream ingestion has not recorded any progress yet, waiting to advance pos %s",
				targetTime)
		}
		if replicatedTime.Less(targetTime) {
			return errors.Newf("waiting for stream ingestion job progress %s to advance beyond %s",
				replicatedTime, targetTime)
		}
		return nil
	})
}

type mockBatchHandler bool

var _ BatchHandler = mockBatchHandler(true)

func (m mockBatchHandler) HandleBatch(
	_ context.Context, _ []streampb.StreamEvent_KV,
) (batchStats, error) {
	if m {
		return batchStats{}, errors.New("batch processing failure")
	}
	return batchStats{}, nil
}
func (m mockBatchHandler) GetLastRow() cdcevent.Row            { return cdcevent.Row{} }
func (m mockBatchHandler) SetSyntheticFailurePercent(_ uint32) {}

type mockDLQ int

func (m *mockDLQ) Create(_ context.Context) error {
	return nil
}

func (m *mockDLQ) Log(
	_ context.Context,
	_ int64,
	_ streampb.StreamEvent_KV,
	_ cdcevent.Row,
	_ error,
	_ retryEligibility,
) error {
	*m++
	return nil
}

// TestFlushErrorHandling exercises the flush path in cases where writes fail.
func TestFlushErrorHandling(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	dlq := mockDLQ(0)
	lrw := &logicalReplicationWriterProcessor{
		metrics:      MakeMetrics(0).(*Metrics),
		getBatchSize: func() int { return 1 },
		dlqClient:    &dlq,
	}
	lrw.purgatory.flush = lrw.flushBuffer
	lrw.purgatory.bytesGauge = lrw.metrics.RetryQueueBytes
	lrw.purgatory.eventsGauge = lrw.metrics.RetryQueueEvents

	lrw.bh = []BatchHandler{(mockBatchHandler(true))}

	lrw.purgatory.byteLimit = func() int64 { return 1 }
	// One failure immediately means a 1-byte purgatory is full.
	require.NoError(t, lrw.handleStreamBuffer(ctx, []streampb.StreamEvent_KV{skv("a")}))
	require.Equal(t, int64(1), lrw.metrics.RetryQueueEvents.Value())
	require.True(t, lrw.purgatory.full())
	require.Equal(t, 0, int(dlq))

	// Another failure causes a forced drain of purgatory, incrementing DLQ count.
	require.NoError(t, lrw.handleStreamBuffer(ctx, []streampb.StreamEvent_KV{skv("b")}))
	require.Equal(t, int64(1), lrw.metrics.RetryQueueEvents.Value())
	require.Equal(t, 1, int(dlq))

	// Bump up the purgatory size limit and observe no more DLQ'ed items.
	lrw.purgatory.byteLimit = func() int64 { return 1 << 20 }
	require.False(t, lrw.purgatory.full())
	require.NoError(t, lrw.handleStreamBuffer(ctx, []streampb.StreamEvent_KV{skv("c")}))
	require.NoError(t, lrw.handleStreamBuffer(ctx, []streampb.StreamEvent_KV{skv("d")}))
	require.Equal(t, 1, int(dlq))
	require.Equal(t, int64(3), lrw.metrics.RetryQueueEvents.Value())
}

func TestLogicalStreamIngestionJobWithFallbackUDF(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	server, s, dbA, dbB := setupLogicalTestServer(t, ctx, base.TestClusterArgs{
		ServerArgs: base.TestServerArgs{
			DefaultTestTenant: base.TestControlsTenantsExplicitly,
			Knobs: base.TestingKnobs{
				JobsTestingKnobs: jobs.NewTestingKnobsWithShortIntervals(),
			},
		},
	})
	defer server.Stopper().Stop(ctx)

	lwwFunc := `CREATE OR REPLACE FUNCTION repl_apply(action STRING, proposed tab, existing tab, prev tab, existing_mvcc_timestamp DECIMAL, existing_origin_timestamp DECIMAL,proposed_mvcc_timestamp DECIMAL, proposed_previous_mvcc_timestamp DECIMAL)
	RETURNS string
	AS $$
	BEGIN
	IF existing_origin_timestamp IS NULL THEN
	    IF existing_mvcc_timestamp < proposed_mvcc_timestamp THEN
			SELECT crdb_internal.log('case 1');
			RETURN 'accept_proposed';
		ELSE
			SELECT crdb_internal.log('case 2');
			RETURN 'ignore_proposed';
		END IF;
	ELSE
		IF existing_origin_timestamp < proposed_mvcc_timestamp THEN
			SELECT crdb_internal.log('case 3');
			RETURN 'accept_proposed';
		ELSE
			SELECT crdb_internal.log('case 4');
			RETURN 'ignore_proposed';
		END IF;
	END IF;
	END
	$$ LANGUAGE plpgsql`
	dbB.Exec(t, lwwFunc)
	dbA.Exec(t, lwwFunc)

	dbAURL, cleanup := s.PGUrl(t, serverutils.DBName("a"))
	defer cleanup()
	dbBURL, cleanupB := s.PGUrl(t, serverutils.DBName("b"))
	defer cleanupB()

	// Swap one of the URLs to external:// to verify this indirection works.
	// TODO(dt): this create should support placeholder for URI.
	dbB.Exec(t, "CREATE EXTERNAL CONNECTION a AS '"+dbAURL.String()+"'")
	dbAURL = url.URL{
		Scheme: "external",
		Host:   "a",
	}

	var (
		jobAID jobspb.JobID
		jobBID jobspb.JobID
	)
	dbA.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab WITH FUNCTION repl_apply FOR TABLE tab", dbBURL.String()).Scan(&jobAID)
	dbB.QueryRow(t, "CREATE LOGICAL REPLICATION STREAM FROM TABLE tab ON $1 INTO TABLE tab WITH FUNCTION repl_apply FOR TABLE tab", dbAURL.String()).Scan(&jobBID)

	now := s.Clock().Now()
	t.Logf("waiting for replication job %d", jobAID)
	WaitUntilReplicatedTime(t, now, dbA, jobAID)
	t.Logf("waiting for replication job %d", jobBID)
	WaitUntilReplicatedTime(t, now, dbB, jobBID)

	dbA.Exec(t, "INSERT INTO tab VALUES (2, 'potato')")
	dbB.Exec(t, "INSERT INTO tab VALUES (3, 'celeriac')")
	dbA.Exec(t, "UPSERT INTO tab VALUES (1, 'hello, again')")
	dbB.Exec(t, "UPSERT INTO tab VALUES (1, 'goodbye, again')")

	now = s.Clock().Now()
	WaitUntilReplicatedTime(t, now, dbA, jobAID)
	WaitUntilReplicatedTime(t, now, dbB, jobBID)

	expectedRows := [][]string{
		{"1", "goodbye, again"},
		{"2", "potato"},
		{"3", "celeriac"},
	}
	dbA.CheckQueryResults(t, "SELECT * from a.tab", expectedRows)
	dbB.CheckQueryResults(t, "SELECT * from b.tab", expectedRows)
}
