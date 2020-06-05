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

package indexer_bigquery

import (
	"context"
	"fmt"
	"time"

	"github.com/dfuse-io/search/metrics"

	"github.com/dfuse-io/bstream"
	"github.com/dfuse-io/dgrpc"
	"github.com/dfuse-io/dstore"
	pbheadinfo "github.com/dfuse-io/pbgo/dfuse/headinfo/v1"
	pbhealth "github.com/dfuse-io/pbgo/grpc/health/v1"
	"github.com/dfuse-io/search"
	indexerBigQuery "github.com/dfuse-io/search/indexer-bigquery"
	"github.com/dfuse-io/shutter"
	"go.uber.org/zap"
)

type Config struct {
	GRPCListenAddr        string // path for gRPC healthcheck
	IndicesStoreURL       string // Path to upload the written index shards
	BlocksStoreURL        string // Path to read blocks archives
	BlockstreamAddr       string // gRPC URL to reach a stream of blocks
	WritablePath          string // Writable base path for storing index files
	ShardSize             uint64 // Number of blocks to store in a given Bleve index
	StartBlock            int64  // Start indexing from block num
	StopBlock             uint64 // Stop indexing at block num
	IsVerbose             bool   // verbose logging
	EnableBatchMode       bool   // Enabled the indexer in batch mode with a start & stop block
}

type Modules struct {
	BigQueryBlockMapper indexerBigQuery.BigQueryBlockMapper
	StartBlockResolver bstream.StartBlockResolver
}

var IndexerAppStartAborted = fmt.Errorf("getting irr block aborted by indexer application")

type App struct {
	*shutter.Shutter
	config         *Config
	modules        *Modules
	readinessProbe pbhealth.HealthClient
}

func New(config *Config, modules *Modules) *App {
	return &App{
		Shutter: shutter.New(),
		config:  config,
		modules: modules,
	}
}

func (a *App) nextLiveStartBlock() (targetStartBlock uint64, err error) {
	if a.config.StartBlock >= 0 {
		return uint64(a.config.StartBlock), nil
	}

	zlog.Info("trying to resolve negative startblock from blockstream headinfo")
	conn, err := dgrpc.NewInternalClient(a.config.BlockstreamAddr)
	if err != nil {
		return 0, fmt.Errorf("getting headinfo client: %w", err)
	}
	headinfoCli := pbheadinfo.NewHeadInfoClient(conn)
	libRef, err := search.GetLibInfo(headinfoCli)
	if err != nil {
		return 0, fmt.Errorf("fetching LIB with headinfo: %w", err)
	}

	targetStartBlock = uint64(int64(libRef.Num()) + a.config.StartBlock)
	return
}

func (a *App) resolveStartBlock(ctx context.Context, dexer *indexerBigQuery.IndexerBigQuery) (targetStartBlock uint64, filesourceStartBlock uint64, previousIrreversibleID string, err error) {
	if a.config.EnableBatchMode {
		if a.config.StartBlock < 0 {
			return 0, 0, "", fmt.Errorf("invalid negative start block in batch mode")
		}
		targetStartBlock = uint64(a.config.StartBlock)
	} else {
		if a.config.StartBlock >= 0 {
			targetStartBlock = uint64(a.config.StartBlock)
		} else {
			targetStartBlock, err = a.nextLiveStartBlock()
			if err != nil {
				return
			}
		}
		targetStartBlock = dexer.NextBaseBlockAfter(targetStartBlock) // skip already processed indexes
	}

	filesourceStartBlock, previousIrreversibleID, err = a.modules.StartBlockResolver.Resolve(ctx, targetStartBlock)
	return
}

func (a *App) Run() error {
	zlog.Info("running indexer app ", zap.Reflect("config", a.config))

	metrics.Register(metrics.IndexerMetricSet)

	if err := search.ValidateRegistry(); err != nil {
		return err
	}

	indexesStore, err := dstore.NewStore(a.config.IndicesStoreURL, "", "", true)
	if err != nil {
		return fmt.Errorf("failed setting up indexes store: %w", err)
	}

	blocksStore, err := dstore.NewDBinStore(a.config.BlocksStoreURL)
	if err != nil {
		return fmt.Errorf("failed setting up blocks store: %w", err)
	}

	dexer := indexerBigQuery.NewIndexerBigQuery(
		indexesStore,
		blocksStore,
		a.config.BlockstreamAddr,
		a.modules.BigQueryBlockMapper,
		a.config.WritablePath,
		a.config.ShardSize,
		a.config.GRPCListenAddr)

	dexer.StopBlockNum = a.config.StopBlock
	dexer.Verbose = a.config.IsVerbose

	ctx, cancel := context.WithCancel(context.Background())
	a.OnTerminating(func(_ error) { cancel() })

	zlog.Info("resolving start block...")
	targetStartBlockNum, filesourceStartBlockNum, previousIrreversibleID, err := a.resolveStartBlock(ctx, dexer)
	if err != nil {
		return err
	}

	if a.config.EnableBatchMode {
		zlog.Info("setting up indexing batch pipeline",
			zap.Uint64("target_start_block_num", targetStartBlockNum),
			zap.Uint64("filesource_start_block_num", filesourceStartBlockNum),
			zap.String("previous_irreversible_id,", previousIrreversibleID),
		)
		dexer.BuildBatchPipeline(targetStartBlockNum, filesourceStartBlockNum, previousIrreversibleID)
	} else {
		zlog.Info("setting up indexing live pipeline",
			zap.Uint64("target_start_block_num", targetStartBlockNum),
			zap.Uint64("filesource_start_block_num", filesourceStartBlockNum),
			zap.String("previous_irreversible_id,", previousIrreversibleID),
		)
		dexer.BuildLivePipeline(targetStartBlockNum, filesourceStartBlockNum, previousIrreversibleID)
	}

	err = dexer.Bootstrap(targetStartBlockNum)
	if err != nil {
		return fmt.Errorf("failed to bootstrap indexer: %w", err)
	}

	gs, err := dgrpc.NewInternalClient(a.config.GRPCListenAddr)
	if err != nil {
		return fmt.Errorf("cannot create readiness probe")
	}
	a.readinessProbe = pbhealth.NewHealthClient(gs)

	a.OnTerminating(dexer.Shutdown)
	dexer.OnTerminated(a.Shutdown)

	zlog.Info("launching indexer")
	go dexer.Launch()

	return nil
}

func (a *App) IsReady() bool {
	if a.readinessProbe == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	resp, err := a.readinessProbe.Check(ctx, &pbhealth.HealthCheckRequest{})
	if err != nil {
		zlog.Info("readiness probe error", zap.Error(err))
		return false
	}

	if resp.Status == pbhealth.HealthCheckResponse_SERVING {
		return true
	}

	return false
}

func getBlockCount(startBlock int64) (uint64, error) {
	if startBlock >= 0 {
		return 0, fmt.Errorf("start block %d must be a relative value (-) to yield a block count", startBlock)
	}
	return uint64(-1 * startBlock), nil
}
