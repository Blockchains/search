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

package forkresolver

import (
	"context"
	"fmt"
	"time"

	pbbstream "github.com/dfuse-io/pbgo/dfuse/bstream/v1"
	pbhealth "github.com/dfuse-io/pbgo/grpc/health/v1"
	"github.com/dfuse-io/shutter"
	"github.com/dfuse-io/dgrpc"
	"github.com/dfuse-io/dmesh"
	dmeshClient "github.com/dfuse-io/dmesh/client"
	"github.com/dfuse-io/dstore"
	"github.com/dfuse-io/search"
	"github.com/dfuse-io/search/forkresolver"
	"go.uber.org/zap"
)

type Config struct {
	Dmesh                    dmeshClient.SearchClient
	Protocol                 pbbstream.Protocol
	ServiceVersion           string        // dmesh service version (v1)
	GRPCListenAddr           string        // Address to listen for incoming gRPC requests
	HttpListenAddr           string        // Address to listen for incoming http requests
	PublishDuration          time.Duration // longest duration a dmesh peer will not publish
	IndicesPath              string        // Location for inflight indices
	BlocksStoreURL           string        // Path to read blocks archives
	DfuseHooksActionName     string        // The dfuse Hooks event action name to intercept"
	IndexingRestrictionsJSON string        // optional json-formatted set of indexing restrictions, like a blacklist
	EnableReadinessProbe     bool          // Creates a health check probe
}

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
	zlog.Info("running forkresolver app ", zap.Reflect("config", a.config))

	blocksStore, err := dstore.NewDBinStore(a.config.BlocksStoreURL)
	if err != nil {
		return fmt.Errorf("failed setting up blocks store: %w", err)
	}

	zlog.Info("creating search peer")
	searchPeer := dmesh.NewSearchForkResolverPeer(a.config.ServiceVersion, a.config.GRPCListenAddr, a.config.PublishDuration)

	zlog.Info("publishing search archive peer", zap.String("peer_host", searchPeer.GenericPeer.Host))
	err = a.config.Dmesh.PublishNow(searchPeer)
	if err != nil {
		return fmt.Errorf("publishing peer to dmesh: %w", err)
	}

	restrictions, err := search.ParseRestrictionsJSON(a.config.IndexingRestrictionsJSON)
	if err != nil {
		return fmt.Errorf("failed parsing restrictions JSON")
	}
	if len(restrictions) > 0 {
		zlog.Info("Applying restrictions on indexing", zap.Reflect("restrictions", restrictions))
	}

	fr := forkresolver.NewForkResolver(
		blocksStore,
		a.config.Dmesh,
		searchPeer,
		a.config.Protocol,
		a.config.DfuseHooksActionName,
		a.config.GRPCListenAddr,
		a.config.HttpListenAddr,
		restrictions,
		a.config.IndicesPath)

	if a.config.EnableReadinessProbe {
		gs, err := dgrpc.NewInternalClient(a.config.GRPCListenAddr)
		if err != nil {
			return fmt.Errorf("cannot create readiness probe")
		}
		a.readinessProbe = pbhealth.NewHealthClient(gs)
	}

	a.OnTerminating(fr.Shutdown)
	fr.OnTerminated(a.Shutdown)

	zlog.Info("launching forkresolver search")
	go fr.Launch()

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