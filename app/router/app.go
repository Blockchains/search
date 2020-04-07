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

package router

import (
	"context"
	"fmt"
	"time"

	pbblockmeta "github.com/dfuse-io/pbgo/dfuse/blockmeta/v1"
	pbbstream "github.com/dfuse-io/pbgo/dfuse/bstream/v1"
	pbhealth "github.com/dfuse-io/pbgo/grpc/health/v1"
	"github.com/dfuse-io/shutter"
	"github.com/dfuse-io/dgrpc"
	dmeshClient "github.com/dfuse-io/dmesh/client"
	"github.com/dfuse-io/search/router"
	"go.uber.org/zap"
)

type Config struct {
	Dmesh                dmeshClient.SearchClient
	Protocol             pbbstream.Protocol
	BlockmetaAddr        string // Blockmeta endpoint is queried to validate cursors that are passed LIB and forked out
	GRPCListenAddr       string // Address to listen for incoming gRPC requests
	HeadDelayTolerance   uint64 // Number of blocks above a backend's head we allow a request query to be served (Live & Router)
	LibDelayTolerance    uint64 // Number of blocks above a backend's lib we allow a request query to be served (Live & Router)
	EnableRetry          bool   // Enable the router's attempt to retry a backend search if there is an error. This could have adverse consequences when search through the live
	EnableReadinessProbe bool   // Creates a health check probe
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
	zlog.Info("running router app ", zap.Reflect("config", a.config))

	conn, err := dgrpc.NewInternalClient(a.config.BlockmetaAddr)
	if err != nil {
		return fmt.Errorf("getting blockmeta client: %w", err)
	}

	blockmetaCli := pbblockmeta.NewBlockIDClient(conn)
	forksCli := pbblockmeta.NewForksClient(conn)

	router := router.New(a.config.Protocol, a.config.Dmesh, a.config.HeadDelayTolerance, a.config.LibDelayTolerance, blockmetaCli, forksCli, a.config.EnableRetry)

	a.OnTerminating(router.Shutdown)
	router.OnTerminated(a.Shutdown)

	if a.config.EnableReadinessProbe {
		gs, err := dgrpc.NewInternalClient(a.config.GRPCListenAddr)
		if err != nil {
			return fmt.Errorf("cannot create readiness probe")
		}
		a.readinessProbe = pbhealth.NewHealthClient(gs)
	}

	zlog.Info("launching router")
	go router.Launch(a.config.GRPCListenAddr)

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