// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package processor

import (
	"strconv"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/entry"
	"github.com/pingcap/ticdc/cdc/model"
	"github.com/pingcap/ticdc/cdc/puller"
	"github.com/pingcap/ticdc/pkg/context"
	cerror "github.com/pingcap/ticdc/pkg/errors"
	"github.com/pingcap/ticdc/pkg/pipeline"
	"github.com/pingcap/ticdc/pkg/retry"
	"github.com/pingcap/ticdc/pkg/security"
	tidbkv "github.com/pingcap/tidb/kv"
	"go.uber.org/zap"
)

// TablePipeline is a pipeline which capture the change log from tikv in a table
type TablePipeline struct {
	p          *pipeline.Pipeline
	resolvedTs uint64
	stopped    int32
}

// ResolvedTs returns the resolved ts in this table pipeline
func (t *TablePipeline) ResolvedTs() model.Ts {
	return atomic.LoadUint64(&t.resolvedTs)
}

// AsyncStop tells the pipeline to stop, and returns true is the pipeline is already stopped.
func (t *TablePipeline) AsyncStop() bool {
	err := t.p.SendToFirstNode(pipeline.CommandMessage(&pipeline.Command{
		Tp: pipeline.CommandTypeShouldStop,
	}))
	if !cerror.ErrSendToClosedPipeline.Equal(err) {
		log.Panic("unexpect error from send to first node", zap.Error(err))
	}
	return atomic.LoadInt32(&t.stopped) != 0
}

// NewTablePipeline creates a table pipeline
// TODO: the parameters in this function are too much, try to move some parameters into ctx.Vars().
func NewTablePipeline(ctx context.Context,
	credential *security.Credential,
	kvStorage tidbkv.Storage,
	limitter *puller.BlurResourceLimitter,
	mounter entry.Mounter,
	sortEngine model.SortEngine,
	sortDir string,
	tableID model.TableID,
	replicaInfo *model.TableReplicaInfo,
	targetTs model.Ts,
	outputCh chan *model.PolymorphicEvent) (context.Context, *TablePipeline) {
	var tableName string
	err := retry.Run(time.Millisecond*5, 3, func() error {
		if name, ok := ctx.Vars().SchemaStorage.GetLastSnapshot().GetTableNameByID(tableID); ok {
			tableName = name.QuoteString()
			return nil
		}
		return errors.Errorf("failed to get table name, fallback to use table id: %d", tableID)
	})
	if err != nil {
		log.Warn("get table name for metric", zap.Error(err))
		tableName = strconv.Itoa(int(tableID))
	}
	tablePipeline := new(TablePipeline)
	ctx, p := pipeline.NewPipeline(ctx)
	p.AppendNode(ctx, "puller", newPullerNode(credential, kvStorage, limitter, tableID, replicaInfo, tableName))
	p.AppendNode(ctx, "mounter", newMounterNode(mounter))
	p.AppendNode(ctx, "sorter", newSorterNode(sortEngine, sortDir, tableName))
	p.AppendNode(ctx, "safe_stopper", newSafeStopperNode(targetTs))
	p.AppendNode(ctx, "output", newOutputNode(outputCh, &tablePipeline.resolvedTs, &tablePipeline.stopped))
	return ctx, tablePipeline
}