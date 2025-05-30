// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package colflow_test

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/col/coldatatestutils"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexecargs"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexectestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecerror"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecop"
	"github.com/cockroachdb/cockroach/pkg/sql/colflow"
	"github.com/cockroachdb/cockroach/pkg/sql/colflow/colrpc"
	"github.com/cockroachdb/cockroach/pkg/sql/colmem"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/flowinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/testutils/colcontainerutils"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

type testScenario struct {
	string
}

var (
	consumerDone     = testScenario{"ConsumerDone"}
	consumerClosed   = testScenario{"ConsumerClosed"}
	useBatchReceiver = testScenario{"BatchReceiver"}
	testScenarios    = []testScenario{consumerDone, consumerClosed, useBatchReceiver}
)

// TestVectorizedFlowShutdown tests that closing the FlowCoordinator correctly
// closes all the infrastructure corresponding to the flow ending in that
// FlowCoordinator. Namely:
// - on a remote node, it creates a colflow.HashRouter with 3 outputs (with a
// corresponding to each colrpc.Outbox) as well as 3 standalone Outboxes;
// - on a local node, it creates 6 colrpc.Inboxes that feed into an unordered
// synchronizer which then outputs all the data into a materializer.
// The resulting scheme looks as follows:
//
// |            Remote Node             |                  Local Node
// |                                    |
// |             -> output -> Outbox -> | -> Inbox -> |
// |            |                       |
// | Hash Router -> output -> Outbox -> | -> Inbox -> |
// |            |                       |
// |             -> output -> Outbox -> | -> Inbox -> |
// |                                    |              -> Synchronizer -> materializer -> FlowCoordinator
// |                          Outbox -> | -> Inbox -> |
// |                                    |
// |                          Outbox -> | -> Inbox -> |
// |                                    |
// |                          Outbox -> | -> Inbox -> |
//
// Also, with 50% probability, another remote node with the chain of an Outbox
// and Inbox is placed between the synchronizer and materializer. The resulting
// scheme then looks as follows:
//
// |            Remote Node             |            Another Remote Node             |         Local Node
// |                                    |                                            |
// |             -> output -> Outbox -> | -> Inbox ->                                |
// |            |                       |             |                              |
// | Hash Router -> output -> Outbox -> | -> Inbox ->                                |
// |            |                       |             |                              |
// |             -> output -> Outbox -> | -> Inbox ->                                |
// |                                    |             | -> Synchronizer -> Outbox -> | -> Inbox -> materializer -> FlowCoordinator
// |                          Outbox -> | -> Inbox ->                                |
// |                                    |             |                              |
// |                          Outbox -> | -> Inbox ->                                |
// |                                    |             |                              |
// |                          Outbox -> | -> Inbox ->                                |
//
// Remote nodes are simulated by having separate contexts and separate outbox
// registries.
//
// We also replace the pair of materializer -> FlowCoordinator with the
// BatchFlowCoordinator to verify the behavior of the latter too.
//
// Additionally, all Outboxes have a single metadata source. In ConsumerDone
// shutdown scenario, we check that the metadata has been successfully
// propagated from all of the metadata sources.
func TestVectorizedFlowShutdown(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	ctx := context.Background()
	stopper := stop.NewStopper()
	defer stopper.Stop(ctx)
	_, mockServer, addr, err := flowinfra.StartMockDistSQLServer(ctx,
		hlc.NewClockForTesting(nil), stopper, execinfra.StaticSQLInstanceID,
	)
	require.NoError(t, err)
	dialer := &flowinfra.MockDialer{Addr: addr}
	defer dialer.Close()

	queueCfg, cleanup := colcontainerutils.NewTestingDiskQueueCfg(t, true /* inMem */)
	defer cleanup()

	for run := 0; run < 10; run++ {
		for _, scenario := range testScenarios {
			t.Run(fmt.Sprintf("testScenario=%s", scenario.string), func(t *testing.T) {
				ctxLocal, cancelLocal := context.WithCancel(ctx)
				ctxRemote, cancelRemote := context.WithCancel(ctx)
				// Linter says there is a possibility of "context leak" because
				// cancelRemote variable may not be used, so we defer the call to it.
				// This does not change anything about the test since we're blocking on
				// the wait group and we will call cancelRemote() below, so this defer
				// is actually a noop.
				defer cancelRemote()
				st := cluster.MakeTestingClusterSettings()
				evalCtx := eval.MakeTestingEvalContext(st)
				defer evalCtx.Stop(ctxLocal)
				flowCtx := &execinfra.FlowCtx{
					EvalCtx: &evalCtx,
					Mon:     evalCtx.TestingMon,
					Cfg:     &execinfra.ServerConfig{Settings: st},
				}
				rng, _ := randutil.NewTestRand()
				var (
					err                error
					wg                 sync.WaitGroup
					typs               = []*types.T{types.Int}
					hashRouterInput, _ = coldatatestutils.NewRandomDataOp(
						testAllocator,
						rng,
						coldatatestutils.RandomDataOpArgs{
							DeterministicTyps: typs,
							// Set a high number of batches to ensure that the HashRouter is
							// very far from being finished when the flow is shut down.
							NumBatches: math.MaxInt64,
							Selection:  true,
						},
					)
					numHashRouterOutputs = 3
					numInboxes           = numHashRouterOutputs + 3
					inboxes              = make([]*colrpc.Inbox, 0, numInboxes+1)
					handleStreamErrCh    = make([]chan error, numInboxes+1)
					synchronizerInputs   = make([]colexecargs.OpWithMetaInfo, 0, numInboxes)
					streamID             = 0
					addAnotherRemote     = rng.Float64() < 0.5
				)

				// Create an allocator for each output.
				allocators := make([]*colmem.Allocator, numHashRouterOutputs)
				diskAccounts := make([]*mon.BoundAccount, numHashRouterOutputs)
				diskQueueMemAccounts := make([]*mon.BoundAccount, numHashRouterOutputs)
				for i := range allocators {
					acc := testMemMonitor.MakeBoundAccount()
					defer acc.Close(ctxRemote)
					allocators[i] = colmem.NewAllocator(ctxRemote, &acc, testColumnFactory)
					diskAcc := testDiskMonitor.MakeBoundAccount()
					diskAccounts[i] = &diskAcc
					defer diskAcc.Close(ctxRemote)
					diskQueueMemAcc := testMemMonitor.MakeBoundAccount()
					diskQueueMemAccounts[i] = &diskQueueMemAcc
					defer diskQueueMemAcc.Close(ctx)
				}
				createMetadataSourceForID := func(id int) colexecop.MetadataSource {
					return colexectestutils.CallbackMetadataSource{
						DrainMetaCb: func() []execinfrapb.ProducerMetadata {
							return []execinfrapb.ProducerMetadata{{Err: errors.Errorf("%d", id)}}
						},
					}
				}
				// The first numHashRouterOutputs streamIDs are allocated to the
				// outboxes that drain these outputs. The outboxes will drain the router
				// outputs which should in turn drain the HashRouter that will return
				// this metadata.
				toDrain := make([]colexecop.MetadataSource, numHashRouterOutputs)
				for i := range toDrain {
					toDrain[i] = createMetadataSourceForID(i)
				}
				hashRouter, hashRouterOutputs := colflow.NewHashRouter(
					&execinfra.FlowCtx{Gateway: false},
					0, /* processorID */
					allocators,
					colexecargs.OpWithMetaInfo{
						Root:            hashRouterInput,
						MetadataSources: toDrain,
					},
					typs,
					[]uint32{0}, /* hashCols */
					execinfra.DefaultMemoryLimit,
					queueCfg,
					&colexecop.TestingSemaphore{},
					diskAccounts,
					diskQueueMemAccounts,
				)
				for i := 0; i < numInboxes; i++ {
					inboxMemAccount := testMemMonitor.MakeBoundAccount()
					defer inboxMemAccount.Close(ctxLocal)
					inbox, err := colrpc.NewInbox(colmem.NewAllocator(ctxLocal, &inboxMemAccount, testColumnFactory), typs, execinfrapb.StreamID(streamID))
					require.NoError(t, err)
					inboxes = append(inboxes, inbox)
					synchronizerInputs = append(
						synchronizerInputs,
						colexecargs.OpWithMetaInfo{
							Root:            colexecop.Operator(inbox),
							MetadataSources: []colexecop.MetadataSource{inbox},
						},
					)
				}
				syncFlowCtx := &execinfra.FlowCtx{Local: false, Gateway: !addAnotherRemote}
				synchronizer := colexec.NewParallelUnorderedSynchronizer(syncFlowCtx, 0 /* processorID */, testAllocator, typs, synchronizerInputs, &wg)
				inputMetadataSource := colexecop.MetadataSource(synchronizer)

				runOutboxInbox := func(
					outboxCtx context.Context,
					flowCtxCancel context.CancelFunc,
					outboxMemAcc *mon.BoundAccount,
					outboxConverterMemAcc *mon.BoundAccount,
					outboxInput colexecop.Operator,
					inbox *colrpc.Inbox,
					id int,
					outboxMetadataSources []colexecop.MetadataSource,
				) {
					outbox, err := colrpc.NewOutbox(
						&execinfra.FlowCtx{Gateway: false},
						0, /* processorID */
						colmem.NewAllocator(outboxCtx, outboxMemAcc, testColumnFactory),
						outboxConverterMemAcc,
						colexecargs.OpWithMetaInfo{
							Root:            outboxInput,
							MetadataSources: outboxMetadataSources,
						},
						typs,
						nil, /* getStats */
					)

					require.NoError(t, err)
					wg.Add(1)
					go func(id int) {
						outbox.Run(
							outboxCtx,
							dialer,
							execinfra.StaticSQLInstanceID,
							execinfrapb.StreamID(id),
							flowCtxCancel,
							0, /* connectionTimeout */
						)
						wg.Done()
					}(id)

					require.NoError(t, err)
					serverStreamNotification := <-mockServer.InboundStreams
					serverStream := serverStreamNotification.Stream
					handleStreamErrCh[id] = make(chan error, 1)
					doneFn := func() { close(serverStreamNotification.Donec) }
					wg.Add(1)
					go func(id int, stream execinfrapb.DistSQL_FlowStreamServer, doneFn func()) {
						handleStreamErrCh[id] <- inbox.RunWithStream(stream.Context(), stream)
						doneFn()
						wg.Done()
					}(id, serverStream, doneFn)
				}

				wg.Add(1)
				go func() {
					hashRouter.Run(ctxRemote)
					wg.Done()
				}()
				for i := 0; i < numInboxes; i++ {
					outboxMemAccount := testMemMonitor.MakeBoundAccount()
					defer outboxMemAccount.Close(ctxRemote)
					outboxConverterMemAcc := testMemMonitor.MakeBoundAccount()
					defer outboxConverterMemAcc.Close(ctxRemote)
					if i < numHashRouterOutputs {
						runOutboxInbox(
							ctxRemote,
							cancelRemote,
							&outboxMemAccount,
							&outboxConverterMemAcc,
							hashRouterOutputs[i],
							inboxes[i],
							streamID,
							[]colexecop.MetadataSource{hashRouterOutputs[i]},
						)
					} else {
						sourceMemAccount := testMemMonitor.MakeBoundAccount()
						defer sourceMemAccount.Close(ctxRemote)
						remoteAllocator := colmem.NewAllocator(ctxRemote, &sourceMemAccount, testColumnFactory)
						batch := remoteAllocator.NewMemBatchWithMaxCapacity(typs)
						batch.SetLength(coldata.BatchSize())
						runOutboxInbox(
							ctxRemote,
							cancelRemote,
							&outboxMemAccount,
							&outboxConverterMemAcc,
							colexecop.NewRepeatableBatchSource(remoteAllocator, batch, typs),
							inboxes[i],
							streamID,
							[]colexecop.MetadataSource{createMetadataSourceForID(streamID)},
						)
					}
					streamID++
				}

				var input colexecop.Operator
				ctxAnotherRemote, cancelAnotherRemote := context.WithCancel(ctx)
				if addAnotherRemote {
					// Add another "remote" node to the flow.
					inboxMemAccount := testMemMonitor.MakeBoundAccount()
					defer inboxMemAccount.Close(ctxAnotherRemote)
					inbox, err := colrpc.NewInbox(colmem.NewAllocator(ctxAnotherRemote, &inboxMemAccount, testColumnFactory), typs, execinfrapb.StreamID(streamID))
					require.NoError(t, err)
					inboxes = append(inboxes, inbox)
					outboxMemAccount := testMemMonitor.MakeBoundAccount()
					defer outboxMemAccount.Close(ctxAnotherRemote)
					outboxConverterMemAcc := testMemMonitor.MakeBoundAccount()
					defer outboxConverterMemAcc.Close(ctxRemote)
					runOutboxInbox(
						ctxAnotherRemote,
						cancelAnotherRemote,
						&outboxMemAccount,
						&outboxConverterMemAcc,
						synchronizer,
						inbox,
						streamID,
						[]colexecop.MetadataSource{inputMetadataSource, createMetadataSourceForID(streamID)},
					)
					streamID++
					// There is now only a single Inbox on the "local" node which is the
					// only metadata source.
					inputMetadataSource = inbox
					input = inbox
				} else {
					input = synchronizer
				}

				inputInfo := colexecargs.OpWithMetaInfo{
					Root:            input,
					MetadataSources: colexecop.MetadataSources{inputMetadataSource},
				}

				// runFlowCoordinator creates a pair of a materializer and a
				// FlowCoordinator, requests 10 rows from it, and returns the
				// coordinator.
				runFlowCoordinator := func() *colflow.FlowCoordinator {
					materializer := colexec.NewMaterializer(
						nil, /* streamingMemAcc */
						flowCtx,
						1, /* processorID */
						inputInfo,
						typs,
					)
					coordinator := colflow.NewFlowCoordinator(
						flowCtx,
						1, /* processorID */
						materializer,
						cancelLocal,
					)
					coordinator.Start(ctxLocal)

					for i := 0; i < 10; i++ {
						row, meta := coordinator.Next()
						require.NotNil(t, row)
						require.Nil(t, meta)
					}
					return coordinator
				}

				// checkMetadata verifies that all the metadata from all
				// outboxes has been received.
				checkMetadata := func(receivedMeta []execinfrapb.ProducerMetadata) {
					receivedMetaFromID := make([]bool, streamID)
					for _, meta := range receivedMeta {
						require.NotNil(t, meta.Err)
						id, err := strconv.Atoi(meta.Err.Error())
						require.NoError(t, err)
						receivedMetaFromID[id] = true
					}
					for id, received := range receivedMetaFromID {
						require.True(t, received, "did not receive metadata from Outbox %d", id)
					}
				}

				switch scenario {
				case consumerDone:
					coordinator := runFlowCoordinator()
					coordinator.ConsumerDone()
					var receivedMeta []execinfrapb.ProducerMetadata
					for {
						row, meta := coordinator.Next()
						require.Nil(t, row)
						if meta == nil {
							break
						}
						receivedMeta = append(receivedMeta, *meta)
					}
					checkMetadata(receivedMeta)

				case consumerClosed:
					coordinator := runFlowCoordinator()
					coordinator.ConsumerClosed()

				case useBatchReceiver:
					// Use a batch receiver that will ask for 10 batches and
					// then will transition to draining.
					recv := &batchReceiver{numMoreBatchesNeeded: 10}
					coordinator := colflow.NewBatchFlowCoordinator(
						flowCtx,
						1, /* processorID */
						inputInfo,
						recv,
						cancelLocal,
					)
					coordinator.Run(ctxLocal)
					checkMetadata(recv.receivedMeta)
				}

				// When Outboxes are setup through vectorizedFlowCreator, the latter
				// keeps track of how many outboxes are on the node. When the last one
				// exits (and if there is no FlowCoordinator on that node),
				// vectorizedFlowCreator will cancel the flow context of the node. To
				// simulate this, we manually cancel contexts of both remote nodes.
				cancelRemote()
				cancelAnotherRemote()

				for i := range inboxes {
					err = <-handleStreamErrCh[i]
					// We either should get no error or a context cancellation error.
					if err != nil {
						require.True(t, testutils.IsError(err, "context canceled"), err)
					}
				}
				wg.Wait()
			})
		}
	}
}

// batchReceiver is a utility execinfra.BatchReceiver that will request the
// specified number of batches before transitioning to draining.
//
// numMoreBatchesReceived is expected to be set to a positive number before the
// usage of the receiver.
type batchReceiver struct {
	numMoreBatchesNeeded int
	receivedMeta         []execinfrapb.ProducerMetadata
}

var _ execinfra.BatchReceiver = &batchReceiver{}

func (b *batchReceiver) ProducerDone() {}

func (b *batchReceiver) PushBatch(
	batch coldata.Batch, meta *execinfrapb.ProducerMetadata,
) execinfra.ConsumerStatus {
	if batch != nil {
		b.numMoreBatchesNeeded--
		if b.numMoreBatchesNeeded < 0 {
			colexecerror.InternalError(errors.New("unexpectedly received a batch after drain was requested"))
		}
	} else if meta != nil {
		b.receivedMeta = append(b.receivedMeta, *meta)
	} else {
		colexecerror.InternalError(errors.New("unexpectedly PushBatch is called with two nil arguments"))
	}
	if b.numMoreBatchesNeeded == 0 {
		return execinfra.DrainRequested
	}
	return execinfra.NeedMoreRows
}
