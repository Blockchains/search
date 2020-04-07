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

package archive

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/dfuse-io/bstream"
	"github.com/dfuse-io/dgrpc"
	"github.com/dfuse-io/dmesh"
	dmeshClient "github.com/dfuse-io/dmesh/client"
	"github.com/dfuse-io/logging"
	pbbstream "github.com/dfuse-io/pbgo/dfuse/bstream/v1"
	pbhead "github.com/dfuse-io/pbgo/dfuse/headinfo/v1"
	pbsearch "github.com/dfuse-io/pbgo/dfuse/search/v1"
	pbhealth "github.com/dfuse-io/pbgo/grpc/health/v1"
	"github.com/dfuse-io/search"
	"github.com/dfuse-io/search/metrics"
	pmetrics "github.com/dfuse-io/search/metrics"
	"github.com/dfuse-io/shutter"
	"github.com/gorilla/mux"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"
)

// Search is the top-level object, embodying the rest.
type ArchiveBackend struct {
	*shutter.Shutter

	pool            *IndexPool
	searchPeer      *dmesh.SearchPeer
	dmeshClient     dmeshClient.Client
	protocol        pbbstream.Protocol
	grpcListenAddr  string
	httpListenAddr  string
	matchCollector  search.MatchCollector
	httpServer      *http.Server
	maxQueryThreads int
	shuttingDown    *atomic.Bool
	shutdownDelay   time.Duration
}

func NewBackend(
	protocol pbbstream.Protocol,
	pool *IndexPool,
	dmeshClient dmeshClient.Client,
	searchPeer *dmesh.SearchPeer,
	grpcListenAddr string,
	httpListenAddr string,
	shutdownDelay time.Duration,
) *ArchiveBackend {

	matchCollector := search.MatchCollectorByType[protocol]
	if matchCollector == nil {
		panic(fmt.Errorf("no match collector for protocol %s, should not happen, you should define a collector", protocol))
	}

	archive := &ArchiveBackend{
		Shutter:        shutter.New(),
		protocol:       protocol,
		pool:           pool,
		dmeshClient:    dmeshClient,
		searchPeer:     searchPeer,
		grpcListenAddr: grpcListenAddr,
		httpListenAddr: httpListenAddr,
		matchCollector: matchCollector,
		shuttingDown:   atomic.NewBool(false),
		shutdownDelay:  shutdownDelay,
	}

	return archive
}

func (b *ArchiveBackend) SetMaxQueryThreads(threads int) {
	b.maxQueryThreads = threads
}

// FIXME: are we *really* servicing some things through REST ?!  That
// `indexed_fields` should be served via gRPC.. all those middlewares,
// gracking, logging, etc.. wuuta
func (b *ArchiveBackend) startServer() {
	router := mux.NewRouter()

	metricsRouter := router.PathPrefix("/").Subrouter()
	coreRouter := router.PathPrefix("/").Subrouter()

	// Metrics & health endpoints
	metricsRouter.HandleFunc("/healthz", b.healthzHandler())

	// Core endpoints
	coreRouter.Use(openCensusMiddleware)
	coreRouter.Use(loggingMiddleware)
	coreRouter.Use(trackingMiddleware)

	coreRouter.HandleFunc("/v0/search/indexed_fields", func(w http.ResponseWriter, r *http.Request) {
		bstream.MustDoForProtocol(b.protocol, map[pbbstream.Protocol]func(){
			pbbstream.Protocol_EOS: func() { writeJSON(r.Context(), w, search.GetEOSIndexedFields()) },
			// pbbstream.Protocol_ETH: func() { writeJSON(r.Context(), w, search.GetETHIndexedFields()) },
		})
	})

	// HTTP
	b.httpServer = &http.Server{Addr: b.httpListenAddr, Handler: router}
	go func() {
		zlog.Info("listening & serving HTTP content", zap.String("http_listen_addr", b.httpListenAddr))
		if err := b.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			b.Shutter.Shutdown(fmt.Errorf("failed listening http %q: %w", b.httpListenAddr, err))
			return
		}
	}()

	// gRPC
	lis, err := net.Listen("tcp", b.grpcListenAddr)
	if err != nil {
		b.Shutter.Shutdown(fmt.Errorf("failed listening grpc %q: %w", b.grpcListenAddr, err))
		return
	}

	s := dgrpc.NewServer(dgrpc.WithLogger(zlog))
	go metrics.ServeMetrics()
	pbsearch.RegisterBackendServer(s, b)
	pbhead.RegisterStreamingHeadInfoServer(s, b)
	pbhealth.RegisterHealthServer(s, b)

	go func() {
		zlog.Info("listening & serving gRPC content", zap.String("grpc_listen_addr", b.grpcListenAddr))
		if err := s.Serve(lis); err != nil {
			b.Shutter.Shutdown(fmt.Errorf("error on gs.Serve: %w", err))
			return
		}
	}()
}

func (b *ArchiveBackend) GetHeadInfo(ctx context.Context, r *pbhead.HeadInfoRequest) (*pbhead.HeadInfoResponse, error) {
	resp := &pbhead.HeadInfoResponse{
		LibNum: b.pool.LastReadOnlyIndexedBlock(),
	}
	return resp, nil
}

// headinfo.StreamingHeadInfo gRPC implementation
func (b *ArchiveBackend) StreamHeadInfo(r *pbhead.HeadInfoRequest, stream pbhead.StreamingHeadInfo_StreamHeadInfoServer) error {
	for {
		resp, _ := b.GetHeadInfo(stream.Context(), r)

		if err := stream.Send(resp); err != nil {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (b *ArchiveBackend) WarmupWithQuery(query string, low, high uint64) error {
	bquery, err := search.NewParsedQuery(b.protocol, query)
	if err != nil {
		return err
	}

	return b.WarmUpArchive(context.Background(), low, high, bquery)
}

// Archive.StreamMatches gRPC implementation
func (b *ArchiveBackend) StreamMatches(req *pbsearch.BackendRequest, stream pbsearch.Backend_StreamMatchesServer) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	if req.WithReversible {
		return fmt.Errorf("archive backend does not support WithReversible == true")
	}
	if req.StopAtVirtualHead {
		return fmt.Errorf("archive backend does not support StopAtVirtualHead == true")
	}
	if req.LiveMarkerInterval != 0 {
		return fmt.Errorf("archive backend does not support LiveMarkerInterval != 0")
	}
	if req.NavigateFromBlockID != "" {
		return fmt.Errorf("archive backend does not support NavigateFromBlockID != ''")
	}
	if req.NavigateFromBlockNum != 0 {
		return fmt.Errorf("archive backend does not support NavigateFromBlockNum != 0")
	}

	zlogger := logging.Logger(ctx, zlog)
	zlogger.Info("starting streaming search query processing")

	bquery, err := search.NewParsedQuery(b.protocol, req.Query)
	if err != nil {
		return err // status.New(codes.InvalidArgument, err.Error())
	}

	metrics := search.NewQueryMetrics(zlogger, req.Descending, bquery.Raw, b.pool.ShardSize, req.LowBlockNum, req.HighBlockNum)
	defer metrics.Finalize()

	pmetrics.ActiveQueryCount.Inc()
	defer pmetrics.ActiveQueryCount.Dec()

	trailer := metadata.New(nil)
	defer stream.SetTrailer(trailer)

	// set the trailer as a default -1 in case we error out
	trailer.Set("last-block-read", fmt.Sprint("-1"))

	archiveQuery := b.newArchiveQuery(ctx, req.Descending, req.LowBlockNum, req.HighBlockNum, bquery, metrics)

	first, _, irr, _, _, _ := b.searchPeer.HeadBlockPointers()
	if err := archiveQuery.checkBoundaries(first, irr); err != nil {
		return err
	}

	go archiveQuery.run()

	for {
		select {
		case err := <-archiveQuery.Errors:
			if err != nil {
				if ctx.Err() == context.Canceled { // error is most likely not from us, but happened upstream
					return nil
				}

				zlogger.Error("archive query received error from channel", zap.Error(err))
				return err
			}
			return nil

		case match, ok := <-archiveQuery.Results:
			if !ok {
				trailer.Set("last-block-read", fmt.Sprintf("%d", archiveQuery.LastBlockRead.Load()))
				return nil
			}

			metrics.TransactionSeenCount++

			response, err := archiveSearchMatchToProto(match)
			if err != nil {
				return fmt.Errorf("unable to obtain search match proto: %s", err)
			}

			metrics.MarkFirstResult()
			if err := stream.Send(response); err != nil {
				// Upstream wants us to stop, this is `io.EOF` ?
				zlogger.Info("we've had a failure sending this upstream, interrupt all this search now")
				return err
			}
		}
	}
}

func (b *ArchiveBackend) Launch() {
	b.OnTerminating(func(e error) {
		zlog.Info("shutting down search archive", zap.Error(e))
		b.stop()
	})

	b.startServer()

	select {
	case <-b.Terminating():
		zlog.Info("archive backend terminated")
		if err := b.Err(); err != nil {
			err = fmt.Errorf("archive backend terminated with error: %s", err)
		}
	}
}

func (b *ArchiveBackend) shutdownHTTPServer() error { /* gracefully */
	if b.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		return b.httpServer.Shutdown(ctx)
	}
	return nil
}

func (b *ArchiveBackend) stop() {
	zlog.Info("cleaning up archive backend", zap.Duration("shutdown_delay", b.shutdownDelay))
	// allow kube service the time to finish in-flight request before the service stops
	// routing traffic
	// We are probably on batch mode where no search peer exists, so don't publish it
	if b.searchPeer != nil {
		b.searchPeer.Locked(func() {
			b.searchPeer.Ready = false
		})
		err := b.dmeshClient.PublishNow(b.searchPeer)
		if err != nil {
			zlog.Error("could not set search peer to not ready", zap.Error(err))
		}
	}

	b.shuttingDown.Store(true)
	time.Sleep(b.shutdownDelay)

	// Graceful shutdown of HTTP server, drain connections, before closing indexes.
	zlog.Info("gracefully shutting down http server, draining connections")
	err := b.shutdownHTTPServer()
	zlog.Info("shutdown http server", zap.Error(err))

	zlog.Info("closing indexes cleanly")
	err = b.pool.CloseIndexes()
	if err != nil {
		zlog.Error("error closing indexes", zap.Error(err))
	}
}