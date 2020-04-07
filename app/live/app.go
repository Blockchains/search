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

package live

import (
	"context"
	"fmt"
	"time"

	"github.com/dfuse-io/bstream"
	"github.com/dfuse-io/dgrpc"
	"github.com/dfuse-io/dmesh"
	dmeshClient "github.com/dfuse-io/dmesh/client"
	"github.com/dfuse-io/dstore"
	pbbstream "github.com/dfuse-io/pbgo/dfuse/bstream/v1"
	pbheadinfo "github.com/dfuse-io/pbgo/dfuse/headinfo/v1"
	pbhealth "github.com/dfuse-io/pbgo/grpc/health/v1"
	"github.com/dfuse-io/search"
	"github.com/dfuse-io/search/live"
	livebackend "github.com/dfuse-io/search/live"
	"github.com/dfuse-io/shutter"
	"go.uber.org/zap"
)

type Config struct {
	Dmesh                    dmeshClient.SearchClient
	Protocol                 pbbstream.Protocol
	ServiceVersion           string        // dmesh service version (v1)
	TierLevel                uint32        // level of the search tier
	GRPCListenAddr           string        // Address to listen for incoming gRPC requests
	PublishDuration          time.Duration // longest duration a dmesh peer will not publish
	BlockmetaAddr            string        // grpc address to blockmeta to decide if the chain is up-to-date
	BlocksStoreURL           string        // Path to read blocks archives
	BlockstreamAddr          string        // gRPC URL to reach a stream of blocks
	HeadDelayTolerance       uint64        // Number of blocks above a backend's head we allow a request query to be served (Live & Router)
	StartBlockDriftTolerance uint64        // Number of blocks behind LIB that the start block is allowed to be
	ShutdownDelay            time.Duration // On shutdown, time to wait before actually leaving, to try and drain connections
	// StartBlockArchiveWaitPeriod time.Time // How long should we wait for an archive search to appear in dmesh before starting anyway

	IndexingRestrictionsJSON string        // optional json-formatted set of indexing restrictions, like a blacklist
	DfuseHooksActionName     string        // The dfuse Hooks event action name to intercept"
	LiveIndexesPath          string        // /tmp/live/indexes", "Location for live indexes (ideally a ramdisk)
	TruncationThreshold      int           //number of available dmesh peers that should serve irreversible blocks before we truncate them from this backend's memory
	RealtimeTolerance        time.Duration // longest delay to consider this service as real-time(ready) on initialization
	EnableReadinessProbe     bool          // Creates a health check probe

}

var LiveAppStartAborted = fmt.Errorf("getting start block aborted by live application")

type App struct {
	*shutter.Shutter
	config         *Config
	readinessProbe pbhealth.HealthClient
}

func New(config *Config) *App {
	return &App{
		Shutter: shutter.New(),
		config:  config,
	}
}
func (a *App) Run() error {
	zlog.Info("running live app ", zap.Reflect("config", a.config))

	blocksStore, err := dstore.NewDBinStore(a.config.BlocksStoreURL)
	if err != nil {
		return fmt.Errorf("failed setting up blocks store: %w", err)
	}

	zlog.Info("creating search peer")
	searchPeer := dmesh.NewSearchHeadPeer(a.config.ServiceVersion, a.config.GRPCListenAddr, 1, a.config.TierLevel, a.config.PublishDuration)

	zlog.Info("publishing search archive peer", zap.String("peer_host", searchPeer.GenericPeer.Host))
	err = a.config.Dmesh.PublishNow(searchPeer)
	if err != nil {
		return fmt.Errorf("publishing peer to dmesh: %w", err)
	}

	lb := livebackend.New(a.config.Protocol, a.config.Dmesh, searchPeer, a.config.HeadDelayTolerance, a.config.ShutdownDelay)

	zlog.Info("setting up blockmeta")
	conn, err := dgrpc.NewInternalClient(a.config.BlockmetaAddr)
	if err != nil {
		return fmt.Errorf("getting blockmeta headinfo client: %w", err)
	}
	headinfoCli := pbheadinfo.NewHeadInfoClient(conn)

	startBlock, err := a.getStartBlock(a.config.Dmesh, headinfoCli)
	if err != nil {
		if err == LiveAppStartAborted {
			return nil
		}
		return err
	}
	//FIXME the tail manager should have two modes of working: 1) based on archive and 2) based on a buffer length, in case the archive has never met its lower bound
	if startBlock == nil {
		zlog.Info("live got a nil start block")
		return nil
	}

	zlog.Info("search live resolved start block",
		zap.String("start_block_id", startBlock.ID()),
		zap.Uint64("start_block_num", startBlock.Num()),
	)

	restrictions, err := search.ParseRestrictionsJSON(a.config.IndexingRestrictionsJSON)
	if err != nil {
		return fmt.Errorf("failed parsing restrictions JSON")
	}
	if len(restrictions) > 0 {
		zlog.Info("Applying restrictions on indexing", zap.Reflect("restrictions", restrictions))
	}

	zlog.Info("setting up subscription hub", zap.Uint64("start_block", startBlock.Num()))
	err = lb.SetupSubscriptionHub(
		startBlock,
		search.MustGetBlockMapper(a.config.Protocol, a.config.DfuseHooksActionName, restrictions),
		blocksStore,
		a.config.BlockstreamAddr,
		a.config.LiveIndexesPath,
		a.config.RealtimeTolerance,
		a.config.TruncationThreshold,
	)
	if err != nil {
		return fmt.Errorf("setting up subscription hub: %w", err)
	}

	a.OnTerminating(lb.Shutdown)
	lb.OnTerminated(a.Shutdown)

	if a.config.EnableReadinessProbe {
		gs, err := dgrpc.NewInternalClient(a.config.GRPCListenAddr)
		if err != nil {
			return fmt.Errorf("cannot create readiness probe")
		}
		a.readinessProbe = pbhealth.NewHealthClient(gs)
	}

	zlog.Info("launching live search")
	go func() {
		lb.WaitHubReady()
		lb.Launch(a.config.GRPCListenAddr)
	}()

	return nil
}

func startBlockFromDmesh(dmesh dmeshClient.SearchClient) bstream.BlockRef {
	startBlock := live.GetMeshLIB(dmesh.Peers, 1)
	if startBlock != nil {
		if startBlock.Num() < 2 {
			startBlock = bstream.NewBlockRef("", 2)
		}
		return startBlock
	}
	return nil
}
func libFromHeadInfo(headinfoCli pbheadinfo.HeadInfoClient, source pbheadinfo.HeadInfoRequest_Source) bstream.BlockRef {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	streamInfo, err := headinfoCli.GetHeadInfo(ctx, &pbheadinfo.HeadInfoRequest{
		Source: source,
	})
	if err != nil {
		zlog.Debug("cannot get lib from headinfo", zap.Error(err))
		return nil
	}

	return bstream.NewBlockRef(streamInfo.LibID, streamInfo.LibNum)
}

func tweakStartBlock(blk bstream.BlockRef) bstream.BlockRef {
	if blk.Num() < 2 {
		return bstream.NewBlockRef("", 2)
	}
	return blk
}

func (a *App) getStartBlock(dmesh dmeshClient.SearchClient, headinfoCli pbheadinfo.HeadInfoClient) (startBlockRef bstream.BlockRef, err error) {

	sleepTime := time.Duration(0)
	for {
		if a.IsTerminating() {
			zlog.Info("leaving getStartBlock because app is terminating")
			err = LiveAppStartAborted
			return
		}
		time.Sleep(sleepTime)
		sleepTime = time.Second * 2

		fromArchive := startBlockFromDmesh(dmesh)
		if fromArchive == nil {
			zlog.Debug("waiting for archive to appear before starting")
			continue
		}

		fromStream := libFromHeadInfo(headinfoCli, pbheadinfo.HeadInfoRequest_STREAM)
		if fromStream == nil {
			zlog.Debug("waiting for headinfo service to appear before starting")
			continue
		}

		fromNetwork := libFromHeadInfo(headinfoCli, pbheadinfo.HeadInfoRequest_NETWORK)
		if fromNetwork == nil {
			if fromStream.Num() >= fromArchive.Num() && fromStream.Num()-fromArchive.Num() < a.config.StartBlockDriftTolerance {
				zlog.Warn("no network head info, but archive head is close to stream LIB, starting from archive LIB")
				return fromArchive, nil
			}
			zlog.Debug("waiting because network LIB is unavailable and archive is too far from stream LIB")
			continue
		}

		// archive close to network
		if fromNetwork.Num() >= fromArchive.Num() && fromNetwork.Num()-fromArchive.Num() < a.config.StartBlockDriftTolerance {
			zlog.Info("starting from the lib from search archive")
			return fromArchive, nil
		}

		// starting from stream Not Implemented: this requires a different tail truncator based on irreversible block at HEAD-x ...
		//if fromNetwork.Num() >= fromStream.Num() && fromNetwork.Num()-fromStream.Num() < a.config.StartBlockDriftTolerance {
		//	zlog.Warn("archive search is late, starting from stream LIB", zap.Uint64("stream_libnum", fromStream.Num()), zap.Uint64("archive_libnum", fromArchive.Num()))
		//	return fromStream, false, nil
		//}
		zlog.Debug("waiting no start-block condition matched",
			zap.Uint64("network_head_block_num", fromNetwork.Num()),
			zap.String("network_head_block_id", fromNetwork.ID()),
			zap.Uint64("stream_head_block_num", fromStream.Num()),
			zap.String("stream_head_block_id", fromStream.ID()),
			zap.Uint64("start_block_tolerance", a.config.StartBlockDriftTolerance),
		)
	}
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