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
	"math"
	"net"
	"sort"

	pbbstream "github.com/dfuse-io/pbgo/dfuse/bstream/v1"
	pbsearch "github.com/dfuse-io/pbgo/dfuse/search/v1"
	pbhealth "github.com/dfuse-io/pbgo/grpc/health/v1"
	"github.com/dfuse-io/shutter"
	"github.com/dfuse-io/bstream"
	"github.com/dfuse-io/derr"
	"github.com/dfuse-io/dgrpc"
	"github.com/dfuse-io/dmesh"
	dmeshClient "github.com/dfuse-io/dmesh/client"
	"github.com/dfuse-io/dstore"
	"github.com/dfuse-io/logging"
	"github.com/dfuse-io/search"
	"github.com/dfuse-io/search/metrics"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
)

var MaxLookupBlocks = uint64(10000)

type ForkResolver struct {
	*shutter.Shutter
	grpcListenAddr       string
	httpListenAddr       string
	searchPeer           *dmesh.SearchPeer
	dmeshClient          dmeshClient.SearchClient
	protocol             pbbstream.Protocol
	blocksStore          dstore.Store
	indexingRestrictions []*search.Restriction
	createSingleIndex    func(blk *bstream.Block) (interface{}, error)
}

func NewForkResolver(
	blocksStore dstore.Store,
	dmeshClient dmeshClient.SearchClient,
	searchPeer *dmesh.SearchPeer,
	protocol pbbstream.Protocol,
	dfuseHooksActionName string,
	grpcListenAddr string,
	httpListenAddr string,
	indexingRestrictions []*search.Restriction,
	indicesPath string) *ForkResolver {
	mapper := search.MustGetBlockMapper(
		protocol,
		dfuseHooksActionName,
		indexingRestrictions)
	p := search.NewPreIndexer(mapper, indicesPath)

	return &ForkResolver{
		Shutter:           shutter.New(),
		dmeshClient:       dmeshClient,
		searchPeer:        searchPeer,
		protocol:          protocol,
		blocksStore:       blocksStore,
		grpcListenAddr:    grpcListenAddr,
		httpListenAddr:    httpListenAddr,
		createSingleIndex: p.Preprocess,
	}
}

func (f *ForkResolver) startGrpcServer() {
	// gRPC
	lis, err := net.Listen("tcp", f.grpcListenAddr)
	if err != nil {
		f.Shutter.Shutdown(fmt.Errorf("failed listening grpc %q: %w", f.grpcListenAddr, err))
		return
	}

	s := dgrpc.NewServer(dgrpc.WithLogger(zlog))
	go metrics.ServeMetrics()
	pbsearch.RegisterForkResolverServer(s, f)
	pbhealth.RegisterHealthServer(s, f)

	zlog.Info("ready to serve")
	f.searchPeer.Locked(func() {
		f.searchPeer.Ready = true
	})

	err = f.dmeshClient.PublishNow(f.searchPeer)
	if err != nil {
		f.Shutter.Shutdown(fmt.Errorf("unable to publisher fork resolver search peer on launch: %w", err))
		return
	}

	go func() {
		zlog.Info("listening & serving gRPC content", zap.String("grpc_listen_addr", f.grpcListenAddr))
		if err := s.Serve(lis); err != nil {
			f.Shutter.Shutdown(fmt.Errorf("error on gs.Serve: %w", err))
		}
	}()
}

func (f *ForkResolver) Launch() {

	f.serveHealthz()
	f.startGrpcServer()

	select {
	case <-f.Terminating():
		if err := f.Err(); err != nil {
			zlog.Error("search forkresolver terminated with error", zap.Error(err))
		} else {
			zlog.Info("search forkresolver terminated")
		}
	}
}

func (f *ForkResolver) StreamUndoMatches(req *pbsearch.ForkResolveRequest, stream pbsearch.ForkResolver_StreamUndoMatchesServer) error {

	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()

	zlogger := logging.Logger(ctx, zlog)

	zlogger.Debug("starting live backend query", zap.Reflect("request", req))

	if len(req.ForkedBlockRefs) == 0 {
		zlogger.Warn("get_blocks called with no refs")
		return derr.Statusf(codes.InvalidArgument, "invalid argument: no refs requested")
	}
	bquery, err := search.NewParsedQuery(f.protocol, req.Query)
	if err != nil {
		if err == context.Canceled {
			return derr.Status(codes.Canceled, "context canceled")
		}
		return err
	}

	blocks, libnum, err := f.getBlocksDescending(ctx, req.ForkedBlockRefs)

	collector := search.MatchCollectorByType[f.protocol]
	for _, blk := range blocks {
		zlog.Debug("getting block", zap.String("id", blk.ID()), zap.Uint64("num", blk.Num()))
		obj, err := f.createSingleIndex(blk)
		if err != nil {
			return err
		}
		idx := obj.(*search.SingleIndex)

		matches, err := search.RunSingleIndexQuery(ctx, false, 0, math.MaxUint64, collector, bquery, idx.Index, func() {}, nil)
		if err != nil {
			if err == context.Canceled {
				return derr.Status(codes.Canceled, "context canceled")
			}
			zlogger.Error("error running single index query", zap.Error(err))
			return fmt.Errorf("failed running single-index query")
		}

		for i := len(matches) - 1; i >= 0; i-- {
			match := matches[i]
			pbMatch := &pbsearch.SearchMatch{
				TrxIdPrefix: match.TransactionIDPrefix(),
				Index:       match.GetIndex(),
				Cursor:      search.NewCursor(blk.Num(), blk.ID(), match.TransactionIDPrefix()),
				IrrBlockNum: libnum,
				BlockNum:    blk.Num(),
				Undo:        true,
			}

			err := match.FillProtoSpecific(pbMatch, blk)
			if err != nil {
				return err
			}

			if ctx.Err() != nil { // not for us to return context errors
				return nil
			}

			err = stream.Send(pbMatch)
			if err != nil {
				return err
			}
		}

	}

	return nil
}

func boundaries(refs []*pbsearch.BlockRef) (lowest uint64, highest uint64) {
	lowest = refs[0].BlockNum
	highest = refs[0].BlockNum
	for _, ref := range refs {
		if ref.BlockNum < lowest {
			lowest = ref.BlockNum
		}
		if ref.BlockNum > highest {
			highest = ref.BlockNum
		}
	}
	return
}

func toMap(refs []*pbsearch.BlockRef) map[string]bool {
	out := make(map[string]bool)
	for _, ref := range refs {
		out[ref.BlockID] = true
	}
	return out
}

func sortDescending(blocks []*bstream.Block) {
	sort.SliceStable(blocks, func(i, j int) bool {
		return blocks[i].Number > blocks[j].Number
	})
}

func (f *ForkResolver) getBlocksDescending(ctx context.Context, refs []*pbsearch.BlockRef) ([]*bstream.Block, uint64, error) {
	lowest, highest := boundaries(refs)
	libnum := lowest - 1

	out := []*bstream.Block{}
	refsMap := toMap(refs)
	complete := false

	h := bstream.HandlerFunc(func(blk *bstream.Block, obj interface{}) error {
		if blk.Num() > highest+MaxLookupBlocks {
			return derr.Statusf(codes.NotFound, "not found within %d blocks", MaxLookupBlocks)
		}
		if refsMap[blk.ID()] {
			out = append(out, blk)
			delete(refsMap, blk.ID())
		}
		if len(refsMap) == 0 {
			complete = true
			return search.ErrEndOfRange
		}
		return nil
	})

	src := bstream.NewFileSource(f.protocol, f.blocksStore, lowest, 1, nil, h)
	src.SetNotFoundCallback(func(_ uint64) {
		src.Shutdown(fmt.Errorf("cannot run forkresolver on missing block files")) // ensure we don't stall here if request was for blocks future
	})
	go src.Run()

	select {
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	case <-src.Terminating():
	}

	if !complete {
		return nil, 0, src.Err()
	}

	sortDescending(out)

	return out, libnum, nil
}