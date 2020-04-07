// Copyright 2019 dfuse Platform Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package indexer

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dfuse-io/shutter"
	"github.com/dfuse-io/bstream"
	"github.com/dfuse-io/bstream/blockstream"
	"github.com/dfuse-io/bstream/forkable"
	"github.com/dfuse-io/dstore"
	"github.com/dfuse-io/search"
	"github.com/dfuse-io/search/metrics"
	"go.uber.org/atomic"
	"go.uber.org/zap"

	pbbstream "github.com/dfuse-io/pbgo/dfuse/bstream/v1"
)

// we need to be able to launch it with Start and Stop Block
// we simply a start block

type Indexer struct {
	*shutter.Shutter
	protocol pbbstream.Protocol

	httpListenAddr string
	grpcListenAddr string

	StartBlockNum uint64
	StopBlockNum  uint64
	shardSize     uint64

	pipeline *Pipeline
	source   bstream.Source

	indexesStore         dstore.Store
	blocksStore          dstore.Store
	blockstreamAddr      string
	indexingRestrictions []*search.Restriction
	dfuseHooksActionName string
	writePath            string
	Verbose              bool

	ready        bool
	shuttingDown *atomic.Bool

	// Head block time, solely used to report drift
	headBlockTimeLock sync.RWMutex
	headBlockTime     time.Time

	libBlockLock sync.RWMutex
	libBlock     *bstream.Block
}

func NewIndexer(
	protocol pbbstream.Protocol,
	indexesStore dstore.Store,
	blocksStore dstore.Store,
	blockstreamAddr string,
	dfuseHooksActionName string,
	indexingRestrictions []*search.Restriction,
	writePath string,
	shardSize uint64,
	httpListenAddr string,
	grpcListenAddr string,

) *Indexer {
	indexer := &Indexer{
		Shutter:              shutter.New(),
		shuttingDown:         atomic.NewBool(false),
		protocol:             protocol,
		indexesStore:         indexesStore,
		blocksStore:          blocksStore,
		blockstreamAddr:      blockstreamAddr,
		dfuseHooksActionName: dfuseHooksActionName,
		indexingRestrictions: indexingRestrictions,
		shardSize:            shardSize,
		writePath:            writePath,
		httpListenAddr:       httpListenAddr,
		grpcListenAddr:       grpcListenAddr,
	}

	return indexer
}

func (i *Indexer) setReady() {
	i.ready = true
}

func (i *Indexer) isReady() bool {
	return i.ready
}

func (i *Indexer) Bootstrap(startBlockNum uint64) error {
	zlog.Info("bootstrapping from start blocknum", zap.Uint64("indexer_startblocknum", startBlockNum))
	i.StartBlockNum = startBlockNum
	if i.StartBlockNum%i.shardSize != 0 && i.StartBlockNum != 1 {
		return fmt.Errorf("indexer only starts RIGHT BEFORE the index boundaries, did you specify an irreversible block_id with a round number? It says %d", i.StartBlockNum)
	}
	return i.pipeline.Bootstrap(i.StartBlockNum)
}

func (i *Indexer) BuildLivePipeline(lastProcessedBlock bstream.BlockRef, enableUpload bool, deleteAfterUpload bool) {
	blockMapper := search.MustGetBlockMapper(i.protocol, i.dfuseHooksActionName, i.indexingRestrictions)
	pipe := i.newPipeline(blockMapper, enableUpload, deleteAfterUpload)

	sf := bstream.SourceFromRefFactory(func(startBlockRef bstream.BlockRef, h bstream.Handler) bstream.Source {
		pipe.SetCatchUpMode()

		if startBlockRef.ID() == "" {
			startBlockRef = lastProcessedBlock
		}

		// Exclusive, we never want to process the same block
		// twice. When doing reprocessing, we'll need to provide the block
		// just before.
		gate := bstream.NewBlockIDGate(startBlockRef.ID(), bstream.GateExclusive, h)

		liveSourceFactory := bstream.SourceFactory(func(subHandler bstream.Handler) bstream.Source {
			source := blockstream.NewSource(
				context.Background(),
				i.blockstreamAddr,
				250,
				subHandler,
			)

			// We will enable parallel reprocessing of live blocks, disabled to fix RAM usage
			//			source.SetParallelPreproc(pipe.mapper.PreprocessBlock, 8)

			return source
		})

		fileSourceFactory := bstream.SourceFactory(func(subHandler bstream.Handler) bstream.Source {
			fs := bstream.NewFileSource(
				i.protocol,
				i.blocksStore,
				startBlockRef.Num(),
				2,   // always 2 download threads, ever
				nil, //pipe.mapper.PreprocessBlock,
				subHandler,
			)
			if i.Verbose {
				fs.SetLogger(zlog)
			}
			return fs
		})

		options := []bstream.JoiningSourceOption{
			bstream.JoiningSourceTargetBlockID(startBlockRef.ID()),
		}
		if i.protocol == pbbstream.Protocol_EOS {
			options = append(options, bstream.JoiningSourceTargetBlockNum(2))
		}
		js := bstream.NewJoiningSource(fileSourceFactory, liveSourceFactory, gate, options...)

		return js
	})

	forkableHandler := forkable.New(pipe, forkable.WithExclusiveLIB(lastProcessedBlock), forkable.WithFilters(forkable.StepNew|forkable.StepIrreversible))

	// note the indexer will listen for the source shutdown signal within the Launch() function
	// hence we do not need to propagate the shutdown signal originating from said source to the indexer. (i.e es.OnTerminating(....))
	es := bstream.NewEternalSource(sf, forkableHandler)

	i.source = es
	i.pipeline = pipe
}

func (i *Indexer) BuildBatchPipeline(lastProcessedBlock bstream.BlockRef, startBlockNum uint64, numberOfBlocksToFetchBeforeStarting uint64, enableUpload bool, deleteAfterUpload bool) {
	blockMapper := search.MustGetBlockMapper(i.protocol, i.dfuseHooksActionName, i.indexingRestrictions)
	pipe := i.newPipeline(blockMapper, enableUpload, deleteAfterUpload)

	gate := bstream.NewBlockNumGate(startBlockNum, bstream.GateInclusive, pipe)
	gate.MaxHoldOff = 1000

	forkableHandler := forkable.New(gate, forkable.WithExclusiveLIB(lastProcessedBlock), forkable.WithFilters(forkable.StepIrreversible))

	getBlocksFrom := startBlockNum
	if getBlocksFrom > numberOfBlocksToFetchBeforeStarting {
		getBlocksFrom = startBlockNum - numberOfBlocksToFetchBeforeStarting // Make sure you cover that irreversible block
	}

	fs := bstream.NewFileSource(
		i.protocol,
		i.blocksStore,
		getBlocksFrom,
		2,
		pipe.mapper.PreprocessBlock,
		forkableHandler,
	)
	if i.Verbose {
		fs.SetLogger(zlog)
	}

	// note the indexer will listen for the source shutdown signal within the Launch() function
	// hence we do not need to propagate the shutdown signal originating from said source to the indexer. (i.e fs.OnTerminating(....))
	i.source = fs
	i.pipeline = pipe
	pipe.SetCatchUpMode()
}

func (i *Indexer) Launch() {
	i.OnTerminating(func(e error) {
		zlog.Info("shutting down indexer's source") // TODO: triple check that we want to shutdown the source. PART OF A MERGE where intent is not clear.
		i.source.Shutdown(e)
		zlog.Info("shutting down indexer", zap.Error(e))
		i.cleanup()
	})

	go metrics.ServeMetrics()
	i.serveHealthz()
	zlog.Info("launching pipeline")
	i.source.Run()

	if err := i.source.Err(); err != nil {
		if strings.HasSuffix(err.Error(), CompletedError.Error()) { // I'm so sorry, it is wrapped somewhere in bstream
			zlog.Info("Search Indexing completed successfully")
			i.Shutdown(nil)
			return
		}

		zlog.Error("search indexer source terminated with error", zap.Error(err))
	}

	i.Shutdown(i.source.Err())
	return
}

func (i *Indexer) cleanup() {
	zlog.Info("cleaning up indexer")
	i.shuttingDown.Store(true)

	zlog.Info("waiting on uploads")
	i.pipeline.WaitOnUploads()

	zlog.Sync()
	zlog.Info("indexer shutdown complete")
}