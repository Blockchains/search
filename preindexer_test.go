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

package search

import (
	"context"
	"fmt"
	"io/ioutil"
	"testing"

	pbbstream "github.com/dfuse-io/pbgo/dfuse/bstream/v1"
	pbdeos "github.com/dfuse-io/pbgo/dfuse/codecs/deos"
	"github.com/dfuse-io/bstream"
	"github.com/eoscanada/eos-go"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPreIndexerRunSingleIndexQuery(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	preIndexer := NewPreIndexer(NewEOSBlockMapper("dfuseiohooks:event", nil), tmpDir)

	block, err := ToBStreamBlock(newBlock("00000001a", "00000000a", trxID(1), "eosio.token"))
	require.NoError(t, err)
	matchCollector := MatchCollectorByType[pbbstream.Protocol_EOS]
	if matchCollector == nil {
		panic(fmt.Errorf("no match collector for protocol %s, should not happen, you should define a collector", pbbstream.Protocol_EOS))
	}

	preprocessObj, err := preIndexer.Preprocess(block)
	index := preprocessObj.(*SingleIndex)
	ctx := context.Background()
	sortDesc := false
	lowBlockNum := uint64(0)
	highBlockNum := uint64(1)
	releaseFunc := func() {}
	metrics := NewQueryMetrics(zap.NewNop(), sortDesc, "", 1, 0, 0)
	bleveQuery, err := NewParsedQuery(pbbstream.Protocol_EOS, "account:eosio.token")

	matches, err := RunSingleIndexQuery(ctx, sortDesc, lowBlockNum, highBlockNum, matchCollector, bleveQuery, index.Index, releaseFunc, metrics)
	require.NoError(t, err)
	require.Len(t, matches, 1)
}

func trxID(num int) string {
	out := fmt.Sprintf("%d", num)
	for {
		out = fmt.Sprintf("%s.%d", out, num)
		if len(out) >= 32 {
			return out[:32]
		}
	}
}

func ToBStreamBlock(block *pbdeos.Block) (*bstream.Block, error) {
	time, _ := ptypes.Timestamp(block.Header.Timestamp)
	payload, err := proto.Marshal(block)
	if err != nil {
		return nil, err
	}
	return &bstream.Block{
		Id:             block.Id,
		Number:         uint64(block.Number),
		PreviousId:     block.PreviousID(),
		Timestamp:      time,
		LibNum:         block.LIBNum(),
		PayloadKind:    pbbstream.Protocol_EOS,
		PayloadVersion: 1,
		PayloadBuffer:  payload,
	}, nil
}

func newBlock(id, previous, trxID string, account string) *pbdeos.Block {

	return &pbdeos.Block{
		Id:     id,
		Number: eos.BlockNum(id),
		Header: &pbdeos.BlockHeader{
			Previous:  previous,
			Timestamp: &timestamp.Timestamp{Nanos: 0, Seconds: 0},
		},
		TransactionTraces: []*pbdeos.TransactionTrace{
			{
				Id: trxID,
				Receipt: &pbdeos.TransactionReceiptHeader{
					Status: pbdeos.TransactionStatus_TRANSACTIONSTATUS_EXECUTED,
				},
				ActionTraces: []*pbdeos.ActionTrace{
					{
						Receipt: &pbdeos.ActionReceipt{
							Receiver: "receiver.1",
						},
						Action: &pbdeos.Action{
							Account: account,
							Name:    "transfer",
						},
					},
				},
			},
		},
	}
}