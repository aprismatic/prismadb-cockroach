// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License included
// in the file licenses/BSL.txt and at www.mariadb.com/bsl11.
//
// Change Date: 2022-10-01
//
// On the date above, in accordance with the Business Source License, use
// of this software will be governed by the Apache License, Version 2.0,
// included in the file licenses/APL.txt and at
// https://www.apache.org/licenses/LICENSE-2.0

package distsqlrun

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/distsqlpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/storagebase"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/sqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/opentracing/opentracing-go"
)

func TestClusterFlow(t *testing.T) {
	defer leaktest.AfterTest(t)()
	const numRows = 100

	args := base.TestClusterArgs{ReplicationMode: base.ReplicationManual}
	tc := serverutils.StartTestCluster(t, 3, args)
	defer tc.Stopper().Stop(context.TODO())

	sumDigitsFn := func(row int) tree.Datum {
		sum := 0
		for row > 0 {
			sum += row % 10
			row /= 10
		}
		return tree.NewDInt(tree.DInt(sum))
	}

	sqlutils.CreateTable(t, tc.ServerConn(0), "t",
		"num INT PRIMARY KEY, digitsum INT, numstr STRING, INDEX s (digitsum)",
		numRows,
		sqlutils.ToRowFn(sqlutils.RowIdxFn, sumDigitsFn, sqlutils.RowEnglishFn))

	kvDB := tc.Server(0).DB()
	desc := sqlbase.GetTableDescriptor(kvDB, "test", "t")
	makeIndexSpan := func(start, end int) distsqlpb.TableReaderSpan {
		var span roachpb.Span
		prefix := roachpb.Key(sqlbase.MakeIndexKeyPrefix(desc, desc.Indexes[0].ID))
		span.Key = append(prefix, encoding.EncodeVarintAscending(nil, int64(start))...)
		span.EndKey = append(span.EndKey, prefix...)
		span.EndKey = append(span.EndKey, encoding.EncodeVarintAscending(nil, int64(end))...)
		return distsqlpb.TableReaderSpan{Span: span}
	}

	// Set up table readers on three hosts feeding data into a join reader on
	// the third host. This is a basic test for the distributed flow
	// infrastructure, including local and remote streams.
	//
	// Note that the ranges won't necessarily be local to the table readers, but
	// that doesn't matter for the purposes of this test.

	// Start a span (useful to look at spans using Lightstep).
	sp := tc.Server(0).ClusterSettings().Tracer.StartSpan("cluster test")
	ctx := opentracing.ContextWithSpan(context.Background(), sp)
	defer sp.Finish()

	txnProto := roachpb.MakeTransaction(
		"cluster-test",
		nil, // baseKey
		roachpb.NormalUserPriority,
		tc.Server(0).Clock().Now(),
		0, // maxOffset
	)
	txnCoordMeta := roachpb.MakeTxnCoordMeta(txnProto)

	tr1 := distsqlpb.TableReaderSpec{
		Table:    *desc,
		IndexIdx: 1,
		Spans:    []distsqlpb.TableReaderSpan{makeIndexSpan(0, 8)},
	}

	tr2 := distsqlpb.TableReaderSpec{
		Table:    *desc,
		IndexIdx: 1,
		Spans:    []distsqlpb.TableReaderSpan{makeIndexSpan(8, 12)},
	}

	tr3 := distsqlpb.TableReaderSpec{
		Table:    *desc,
		IndexIdx: 1,
		Spans:    []distsqlpb.TableReaderSpan{makeIndexSpan(12, 100)},
	}

	fid := distsqlpb.FlowID{UUID: uuid.MakeV4()}

	req1 := &distsqlpb.SetupFlowRequest{
		Version:      Version,
		TxnCoordMeta: &txnCoordMeta,
		Flow: distsqlpb.FlowSpec{
			FlowID: fid,
			Processors: []distsqlpb.ProcessorSpec{{
				ProcessorID: 1,
				Core:        distsqlpb.ProcessorCoreUnion{TableReader: &tr1},
				Post: distsqlpb.PostProcessSpec{
					Projection:    true,
					OutputColumns: []uint32{0, 1},
				},
				Output: []distsqlpb.OutputRouterSpec{{
					Type: distsqlpb.OutputRouterSpec_PASS_THROUGH,
					Streams: []distsqlpb.StreamEndpointSpec{
						{Type: distsqlpb.StreamEndpointSpec_REMOTE, StreamID: 0, TargetNodeID: tc.Server(2).NodeID()},
					},
				}},
			}},
		},
	}

	req2 := &distsqlpb.SetupFlowRequest{
		Version:      Version,
		TxnCoordMeta: &txnCoordMeta,
		Flow: distsqlpb.FlowSpec{
			FlowID: fid,
			Processors: []distsqlpb.ProcessorSpec{{
				ProcessorID: 2,
				Core:        distsqlpb.ProcessorCoreUnion{TableReader: &tr2},
				Post: distsqlpb.PostProcessSpec{
					Projection:    true,
					OutputColumns: []uint32{0, 1},
				},
				Output: []distsqlpb.OutputRouterSpec{{
					Type: distsqlpb.OutputRouterSpec_PASS_THROUGH,
					Streams: []distsqlpb.StreamEndpointSpec{
						{Type: distsqlpb.StreamEndpointSpec_REMOTE, StreamID: 1, TargetNodeID: tc.Server(2).NodeID()},
					},
				}},
			}},
		},
	}

	req3 := &distsqlpb.SetupFlowRequest{
		Version:      Version,
		TxnCoordMeta: &txnCoordMeta,
		Flow: distsqlpb.FlowSpec{
			FlowID: fid,
			Processors: []distsqlpb.ProcessorSpec{
				{
					ProcessorID: 3,
					Core:        distsqlpb.ProcessorCoreUnion{TableReader: &tr3},
					Post: distsqlpb.PostProcessSpec{
						Projection:    true,
						OutputColumns: []uint32{0, 1},
					},
					Output: []distsqlpb.OutputRouterSpec{{
						Type: distsqlpb.OutputRouterSpec_PASS_THROUGH,
						Streams: []distsqlpb.StreamEndpointSpec{
							{Type: distsqlpb.StreamEndpointSpec_LOCAL, StreamID: 2},
						},
					}},
				},
				{
					ProcessorID: 4,
					Input: []distsqlpb.InputSyncSpec{{
						Type: distsqlpb.InputSyncSpec_ORDERED,
						Ordering: distsqlpb.Ordering{Columns: []distsqlpb.Ordering_Column{
							{ColIdx: 1, Direction: distsqlpb.Ordering_Column_ASC}}},
						Streams: []distsqlpb.StreamEndpointSpec{
							{Type: distsqlpb.StreamEndpointSpec_REMOTE, StreamID: 0},
							{Type: distsqlpb.StreamEndpointSpec_REMOTE, StreamID: 1},
							{Type: distsqlpb.StreamEndpointSpec_LOCAL, StreamID: 2},
						},
						ColumnTypes: sqlbase.TwoIntCols,
					}},
					Core: distsqlpb.ProcessorCoreUnion{JoinReader: &distsqlpb.JoinReaderSpec{Table: *desc}},
					Post: distsqlpb.PostProcessSpec{
						Projection:    true,
						OutputColumns: []uint32{2},
					},
					Output: []distsqlpb.OutputRouterSpec{{
						Type:    distsqlpb.OutputRouterSpec_PASS_THROUGH,
						Streams: []distsqlpb.StreamEndpointSpec{{Type: distsqlpb.StreamEndpointSpec_SYNC_RESPONSE}},
					}},
				},
			},
		},
	}

	var clients []distsqlpb.DistSQLClient
	for i := 0; i < 3; i++ {
		s := tc.Server(i)
		conn, err := s.RPCContext().GRPCDialNode(s.ServingAddr(), s.NodeID()).Connect(ctx)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, distsqlpb.NewDistSQLClient(conn))
	}

	log.Infof(ctx, "Setting up flow on 0")
	if resp, err := clients[0].SetupFlow(ctx, req1); err != nil {
		t.Fatal(err)
	} else if resp.Error != nil {
		t.Fatal(resp.Error)
	}

	log.Infof(ctx, "Setting up flow on 1")
	if resp, err := clients[1].SetupFlow(ctx, req2); err != nil {
		t.Fatal(err)
	} else if resp.Error != nil {
		t.Fatal(resp.Error)
	}

	log.Infof(ctx, "Running flow on 2")
	stream, err := clients[2].RunSyncFlow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&distsqlpb.ConsumerSignal{SetupFlowRequest: req3})
	if err != nil {
		t.Fatal(err)
	}

	var decoder StreamDecoder
	var rows sqlbase.EncDatumRows
	var metas []distsqlpb.ProducerMetadata
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		err = decoder.AddMessage(ctx, msg)
		if err != nil {
			t.Fatal(err)
		}
		rows, metas = testGetDecodedRows(t, &decoder, rows, metas)
	}
	metas = ignoreMisplannedRanges(metas)
	metas = ignoreTxnCoordMeta(metas)
	if len(metas) != 0 {
		t.Fatalf("unexpected metadata (%d): %+v", len(metas), metas)
	}
	// The result should be all the numbers in string form, ordered by the
	// digit sum (and then by number).
	var results []string
	for sum := 1; sum <= 50; sum++ {
		for i := 1; i <= numRows; i++ {
			if int(tree.MustBeDInt(sumDigitsFn(i))) == sum {
				results = append(results, fmt.Sprintf("['%s']", sqlutils.IntToEnglish(i)))
			}
		}
	}
	expected := strings.Join(results, " ")
	expected = "[" + expected + "]"
	if rowStr := rows.String([]types.T{*types.String}); rowStr != expected {
		t.Errorf("Result: %s\n Expected: %s\n", rowStr, expected)
	}
}

// ignoreMisplannedRanges takes a slice of metadata and returns the entries that
// are not about range info from mis-planned ranges.
func ignoreMisplannedRanges(metas []distsqlpb.ProducerMetadata) []distsqlpb.ProducerMetadata {
	res := make([]distsqlpb.ProducerMetadata, 0)
	for _, m := range metas {
		if len(m.Ranges) == 0 {
			res = append(res, m)
		}
	}
	return res
}

// ignoreTxnCoordMeta takes a slice of metadata and returns the entries excluding
// the transaction coordinator metadata.
func ignoreTxnCoordMeta(metas []distsqlpb.ProducerMetadata) []distsqlpb.ProducerMetadata {
	res := make([]distsqlpb.ProducerMetadata, 0)
	for _, m := range metas {
		if m.TxnCoordMeta == nil {
			res = append(res, m)
		}
	}
	return res
}

// TestLimitedBufferingDeadlock sets up a scenario which leads to deadlock if
// a single consumer can block the entire router (#17097).
func TestLimitedBufferingDeadlock(t *testing.T) {
	defer leaktest.AfterTest(t)()

	tc := serverutils.StartTestCluster(t, 1, base.TestClusterArgs{})
	defer tc.Stopper().Stop(context.TODO())

	// Set up the following network - a simplification of the one described in
	// #17097 (the numbers on the streams are the StreamIDs in the spec below):
	//
	//
	//  +----------+        +----------+
	//  |  Values  |        |  Values  |
	//  +----------+        +-+------+-+
	//         |              | hash |
	//         |              +------+
	//       1 |               |    |
	//         |             2 |    |
	//         v               v    |
	//      +-------------------+   |
	//      |     MergeJoin     |   |
	//      +-------------------+   | 3
	//                |             |
	//                |             |
	//              4 |             |
	//                |             |
	//                v             v
	//              +-----------------+
	//              |  ordered sync   |
	//            +-+-----------------+-+
	//            |       Response      |
	//            +---------------------+
	//
	//
	// This is not something we would end up with from a real SQL query but it's
	// simple and exposes the deadlock: if the hash router outputs a large set of
	// consecutive rows to the left side (which we can ensure by having a bunch of
	// identical rows), the MergeJoiner would be blocked trying to write to the
	// ordered sync, which in turn would block because it's trying to read from
	// the other stream from the hash router. The other stream is blocked because
	// the hash router is already in the process of pushing a row, and we have a
	// deadlock.
	//
	// We set up the left Values processor to emit rows with consecutive values,
	// and the right Values processor to emit groups of identical rows for each
	// value.

	// All our rows have a single integer column.
	typs := []types.T{*types.Int}

	// The left values rows are consecutive values.
	leftRows := make(sqlbase.EncDatumRows, 20)
	for i := range leftRows {
		leftRows[i] = sqlbase.EncDatumRow{
			sqlbase.DatumToEncDatum(&typs[0], tree.NewDInt(tree.DInt(i))),
		}
	}
	leftValuesSpec, err := generateValuesSpec(typs, leftRows, 10 /* rows per chunk */)
	if err != nil {
		t.Fatal(err)
	}

	// The right values rows have groups of identical values (ensuring that large
	// groups of rows go to the same hash bucket).
	rightRows := make(sqlbase.EncDatumRows, 0)
	for i := 1; i <= 20; i++ {
		for j := 1; j <= 4*rowChannelBufSize; j++ {
			rightRows = append(rightRows, sqlbase.EncDatumRow{
				sqlbase.DatumToEncDatum(&typs[0], tree.NewDInt(tree.DInt(i))),
			})
		}
	}

	rightValuesSpec, err := generateValuesSpec(typs, rightRows, 10 /* rows per chunk */)
	if err != nil {
		t.Fatal(err)
	}

	joinerSpec := distsqlpb.MergeJoinerSpec{
		LeftOrdering: distsqlpb.Ordering{
			Columns: []distsqlpb.Ordering_Column{{ColIdx: 0, Direction: distsqlpb.Ordering_Column_ASC}},
		},
		RightOrdering: distsqlpb.Ordering{
			Columns: []distsqlpb.Ordering_Column{{ColIdx: 0, Direction: distsqlpb.Ordering_Column_ASC}},
		},
		Type: sqlbase.InnerJoin,
	}

	txnProto := roachpb.MakeTransaction(
		"deadlock-test",
		nil, // baseKey
		roachpb.NormalUserPriority,
		tc.Server(0).Clock().Now(),
		0, // maxOffset
	)
	txnCoordMeta := roachpb.MakeTxnCoordMeta(txnProto)

	req := distsqlpb.SetupFlowRequest{
		Version:      Version,
		TxnCoordMeta: &txnCoordMeta,
		Flow: distsqlpb.FlowSpec{
			FlowID: distsqlpb.FlowID{UUID: uuid.MakeV4()},
			// The left-hand Values processor in the diagram above.
			Processors: []distsqlpb.ProcessorSpec{
				{
					Core: distsqlpb.ProcessorCoreUnion{Values: &leftValuesSpec},
					Output: []distsqlpb.OutputRouterSpec{{
						Type: distsqlpb.OutputRouterSpec_PASS_THROUGH,
						Streams: []distsqlpb.StreamEndpointSpec{
							{Type: distsqlpb.StreamEndpointSpec_LOCAL, StreamID: 1},
						},
					}},
				},
				// The right-hand Values processor in the diagram above.
				{
					Core: distsqlpb.ProcessorCoreUnion{Values: &rightValuesSpec},
					Output: []distsqlpb.OutputRouterSpec{{
						Type:        distsqlpb.OutputRouterSpec_BY_HASH,
						HashColumns: []uint32{0},
						Streams: []distsqlpb.StreamEndpointSpec{
							{Type: distsqlpb.StreamEndpointSpec_LOCAL, StreamID: 2},
							{Type: distsqlpb.StreamEndpointSpec_LOCAL, StreamID: 3},
						},
					}},
				},
				// The MergeJoin processor.
				{
					Input: []distsqlpb.InputSyncSpec{
						{
							Type:        distsqlpb.InputSyncSpec_UNORDERED,
							Streams:     []distsqlpb.StreamEndpointSpec{{Type: distsqlpb.StreamEndpointSpec_LOCAL, StreamID: 1}},
							ColumnTypes: typs,
						},
						{
							Type:        distsqlpb.InputSyncSpec_UNORDERED,
							Streams:     []distsqlpb.StreamEndpointSpec{{Type: distsqlpb.StreamEndpointSpec_LOCAL, StreamID: 2}},
							ColumnTypes: typs,
						},
					},
					Core: distsqlpb.ProcessorCoreUnion{MergeJoiner: &joinerSpec},
					Post: distsqlpb.PostProcessSpec{
						// Output only one (the left) column.
						Projection:    true,
						OutputColumns: []uint32{0},
					},
					Output: []distsqlpb.OutputRouterSpec{{
						Type: distsqlpb.OutputRouterSpec_PASS_THROUGH,
						Streams: []distsqlpb.StreamEndpointSpec{
							{Type: distsqlpb.StreamEndpointSpec_LOCAL, StreamID: 4},
						},
					}},
				},
				// The final (Response) processor.
				{
					Input: []distsqlpb.InputSyncSpec{{
						Type: distsqlpb.InputSyncSpec_ORDERED,
						Ordering: distsqlpb.Ordering{Columns: []distsqlpb.Ordering_Column{
							{ColIdx: 0, Direction: distsqlpb.Ordering_Column_ASC}}},
						Streams: []distsqlpb.StreamEndpointSpec{
							{Type: distsqlpb.StreamEndpointSpec_LOCAL, StreamID: 4},
							{Type: distsqlpb.StreamEndpointSpec_LOCAL, StreamID: 3},
						},
						ColumnTypes: typs,
					}},
					Core: distsqlpb.ProcessorCoreUnion{Noop: &distsqlpb.NoopCoreSpec{}},
					Output: []distsqlpb.OutputRouterSpec{{
						Type:    distsqlpb.OutputRouterSpec_PASS_THROUGH,
						Streams: []distsqlpb.StreamEndpointSpec{{Type: distsqlpb.StreamEndpointSpec_SYNC_RESPONSE}},
					}},
				},
			},
		},
	}
	s := tc.Server(0)
	conn, err := s.RPCContext().GRPCDialNode(s.ServingAddr(), s.NodeID()).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	stream, err := distsqlpb.NewDistSQLClient(conn).RunSyncFlow(context.TODO())
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&distsqlpb.ConsumerSignal{SetupFlowRequest: &req})
	if err != nil {
		t.Fatal(err)
	}

	var decoder StreamDecoder
	var rows sqlbase.EncDatumRows
	var metas []distsqlpb.ProducerMetadata
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		err = decoder.AddMessage(context.TODO(), msg)
		if err != nil {
			t.Fatal(err)
		}
		rows, metas = testGetDecodedRows(t, &decoder, rows, metas)
	}
	metas = ignoreMisplannedRanges(metas)
	metas = ignoreTxnCoordMeta(metas)
	if len(metas) != 0 {
		t.Errorf("unexpected metadata (%d): %+v", len(metas), metas)
	}
	// TODO(radu): verify the results (should be the same with rightRows)
}

// Test that DistSQL reads fill the BatchRequest.Header.GatewayNodeID field with
// the ID of the gateway (as opposed to the ID of the node that created the
// batch). Important to lease follow-the-workload transfers.
func TestDistSQLReadsFillGatewayID(t *testing.T) {
	defer leaktest.AfterTest(t)()

	// We're going to distribute a table and then read it, and we'll expect all
	// the ScanRequests (produced by the different nodes) to identify the one and
	// only gateway.

	var foundReq int64 // written atomically
	var expectedGateway roachpb.NodeID

	tc := serverutils.StartTestCluster(t, 3, /* numNodes */
		base.TestClusterArgs{
			ReplicationMode: base.ReplicationManual,
			ServerArgs: base.TestServerArgs{
				UseDatabase: "test",
				Knobs: base.TestingKnobs{Store: &storage.StoreTestingKnobs{
					EvalKnobs: storagebase.BatchEvalTestingKnobs{
						TestingEvalFilter: func(filterArgs storagebase.FilterArgs) *roachpb.Error {
							scanReq, ok := filterArgs.Req.(*roachpb.ScanRequest)
							if !ok {
								return nil
							}
							if !strings.HasPrefix(scanReq.Key.String(), "/Table/53/1") {
								return nil
							}

							atomic.StoreInt64(&foundReq, 1)
							if gw := filterArgs.Hdr.GatewayNodeID; gw != expectedGateway {
								return roachpb.NewErrorf(
									"expected all scans to have gateway 3, found: %d",
									gw)
							}
							return nil
						},
					}},
				},
			},
		})
	defer tc.Stopper().Stop(context.TODO())

	db := tc.ServerConn(0)
	sqlutils.CreateTable(t, db, "t",
		"num INT PRIMARY KEY",
		0, /* numRows */
		sqlutils.ToRowFn(sqlutils.RowIdxFn))

	if _, err := db.Exec(`
ALTER TABLE t SPLIT AT VALUES (1), (2), (3);
ALTER TABLE t EXPERIMENTAL_RELOCATE VALUES (ARRAY[2], 1), (ARRAY[1], 2), (ARRAY[3], 3);
`); err != nil {
		t.Fatal(err)
	}

	expectedGateway = tc.Server(2).NodeID()
	if _, err := tc.ServerConn(2).Exec("SELECT * FROM t"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&foundReq) != 1 {
		t.Fatal("TestingEvalFilter failed to find any requests")
	}
}

// BenchmarkInfrastructure sets up a flow that doesn't use KV at all and runs it
// repeatedly. The intention is to profile the distsqlrun infrastructure itself.
func BenchmarkInfrastructure(b *testing.B) {
	defer leaktest.AfterTest(b)()

	args := base.TestClusterArgs{ReplicationMode: base.ReplicationManual}
	tc := serverutils.StartTestCluster(b, 3, args)
	defer tc.Stopper().Stop(context.Background())

	for _, numNodes := range []int{1, 3} {
		b.Run(fmt.Sprintf("n%d", numNodes), func(b *testing.B) {
			for _, numRows := range []int{1, 100, 10000} {
				b.Run(fmt.Sprintf("r%d", numRows), func(b *testing.B) {
					// Generate some data sets, consisting of rows with three values; the first
					// value is increasing.
					rng, _ := randutil.NewPseudoRand()
					lastVal := 1
					valSpecs := make([]distsqlpb.ValuesCoreSpec, numNodes)
					for i := range valSpecs {
						se := StreamEncoder{}
						se.init(sqlbase.ThreeIntCols)
						for j := 0; j < numRows; j++ {
							row := make(sqlbase.EncDatumRow, 3)
							lastVal += rng.Intn(10)
							row[0] = sqlbase.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(lastVal)))
							row[1] = sqlbase.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(rng.Intn(100000))))
							row[2] = sqlbase.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(rng.Intn(100000))))
							if err := se.AddRow(row); err != nil {
								b.Fatal(err)
							}
						}
						msg := se.FormMessage(context.TODO())
						valSpecs[i] = distsqlpb.ValuesCoreSpec{
							Columns:  msg.Typing,
							RawBytes: [][]byte{msg.Data.RawBytes},
						}
					}

					// Set up the following network:
					//
					//         Node 0              Node 1          ...
					//
					//      +----------+        +----------+
					//      |  Values  |        |  Values  |       ...
					//      +----------+        +----------+
					//          |                  |
					//          |       stream 1   |
					// stream 0 |   /-------------/                ...
					//          |  |
					//          v  v
					//     +---------------+
					//     | ordered* sync |
					//  +--+---------------+--+
					//  |        No-op        |
					//  +---------------------+
					//
					// *unordered if we have a single node.

					reqs := make([]distsqlpb.SetupFlowRequest, numNodes)
					streamType := func(i int) distsqlpb.StreamEndpointSpec_Type {
						if i == 0 {
							return distsqlpb.StreamEndpointSpec_LOCAL
						}
						return distsqlpb.StreamEndpointSpec_REMOTE
					}
					txnProto := roachpb.MakeTransaction(
						"cluster-test",
						nil, // baseKey
						roachpb.NormalUserPriority,
						tc.Server(0).Clock().Now(),
						0, // maxOffset
					)
					txnCoordMeta := roachpb.MakeTxnCoordMeta(txnProto)
					for i := range reqs {
						reqs[i] = distsqlpb.SetupFlowRequest{
							Version:      Version,
							TxnCoordMeta: &txnCoordMeta,
							Flow: distsqlpb.FlowSpec{
								Processors: []distsqlpb.ProcessorSpec{{
									Core: distsqlpb.ProcessorCoreUnion{Values: &valSpecs[i]},
									Output: []distsqlpb.OutputRouterSpec{{
										Type: distsqlpb.OutputRouterSpec_PASS_THROUGH,
										Streams: []distsqlpb.StreamEndpointSpec{
											{Type: streamType(i), StreamID: distsqlpb.StreamID(i), TargetNodeID: tc.Server(0).NodeID()},
										},
									}},
								}},
							},
						}
					}

					reqs[0].Flow.Processors[0].Output[0].Streams[0] = distsqlpb.StreamEndpointSpec{
						Type:     distsqlpb.StreamEndpointSpec_LOCAL,
						StreamID: 0,
					}
					inStreams := make([]distsqlpb.StreamEndpointSpec, numNodes)
					for i := range inStreams {
						inStreams[i].Type = streamType(i)
						inStreams[i].StreamID = distsqlpb.StreamID(i)
					}

					lastProc := distsqlpb.ProcessorSpec{
						Input: []distsqlpb.InputSyncSpec{{
							Type: distsqlpb.InputSyncSpec_ORDERED,
							Ordering: distsqlpb.Ordering{Columns: []distsqlpb.Ordering_Column{
								{ColIdx: 0, Direction: distsqlpb.Ordering_Column_ASC}}},
							Streams:     inStreams,
							ColumnTypes: sqlbase.ThreeIntCols,
						}},
						Core: distsqlpb.ProcessorCoreUnion{Noop: &distsqlpb.NoopCoreSpec{}},
						Output: []distsqlpb.OutputRouterSpec{{
							Type:    distsqlpb.OutputRouterSpec_PASS_THROUGH,
							Streams: []distsqlpb.StreamEndpointSpec{{Type: distsqlpb.StreamEndpointSpec_SYNC_RESPONSE}},
						}},
					}
					if numNodes == 1 {
						lastProc.Input[0].Type = distsqlpb.InputSyncSpec_UNORDERED
						lastProc.Input[0].Ordering = distsqlpb.Ordering{}
					}
					reqs[0].Flow.Processors = append(reqs[0].Flow.Processors, lastProc)

					var clients []distsqlpb.DistSQLClient
					for i := 0; i < numNodes; i++ {
						s := tc.Server(i)
						conn, err := s.RPCContext().GRPCDialNode(s.ServingAddr(), s.NodeID()).Connect(context.Background())
						if err != nil {
							b.Fatal(err)
						}
						clients = append(clients, distsqlpb.NewDistSQLClient(conn))
					}

					b.ResetTimer()
					for repeat := 0; repeat < b.N; repeat++ {
						fid := distsqlpb.FlowID{UUID: uuid.MakeV4()}
						for i := range reqs {
							reqs[i].Flow.FlowID = fid
						}

						for i := 1; i < numNodes; i++ {
							if resp, err := clients[i].SetupFlow(context.TODO(), &reqs[i]); err != nil {
								b.Fatal(err)
							} else if resp.Error != nil {
								b.Fatal(resp.Error)
							}
						}
						stream, err := clients[0].RunSyncFlow(context.TODO())
						if err != nil {
							b.Fatal(err)
						}
						err = stream.Send(&distsqlpb.ConsumerSignal{SetupFlowRequest: &reqs[0]})
						if err != nil {
							b.Fatal(err)
						}

						var decoder StreamDecoder
						var rows sqlbase.EncDatumRows
						var metas []distsqlpb.ProducerMetadata
						for {
							msg, err := stream.Recv()
							if err != nil {
								if err == io.EOF {
									break
								}
								b.Fatal(err)
							}
							err = decoder.AddMessage(context.TODO(), msg)
							if err != nil {
								b.Fatal(err)
							}
							rows, metas = testGetDecodedRows(b, &decoder, rows, metas)
						}
						metas = ignoreMisplannedRanges(metas)
						metas = ignoreTxnCoordMeta(metas)
						if len(metas) != 0 {
							b.Fatalf("unexpected metadata (%d): %+v", len(metas), metas)
						}
						if len(rows) != numNodes*numRows {
							b.Errorf("got %d rows, expected %d", len(rows), numNodes*numRows)
						}
						var a sqlbase.DatumAlloc
						for i := range rows {
							if err := rows[i][0].EnsureDecoded(types.Int, &a); err != nil {
								b.Fatal(err)
							}
							if i > 0 {
								last := *rows[i-1][0].Datum.(*tree.DInt)
								curr := *rows[i][0].Datum.(*tree.DInt)
								if last > curr {
									b.Errorf("rows not ordered correctly (%d after %d, row %d)", curr, last, i)
									break
								}
							}
						}
					}
				})
			}
		})
	}
}
