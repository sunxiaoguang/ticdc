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

package pulsar

import (
	"context"
	"net/url"
	"strconv"

	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	"github.com/pingcap/ticdc/cdc/sink/producer"
	cerror "github.com/pingcap/ticdc/pkg/errors"
	"go.uber.org/zap"
)

// NewProducer create a pulsar producer.
func NewProducer(u *url.URL, errCh chan error) (*Producer, error) {
	failpoint.Inject("MockPulsar", func() {
		failpoint.Return(&Producer{
			errCh:      errCh,
			partitions: 4,
		}, nil)
	})

	opt, err := parseSinkOptions(u)
	if err != nil {
		return nil, cerror.WrapError(cerror.ErrPulsarNewProducer, err)
	}
	client, err := pulsar.NewClient(*opt.clientOptions)
	if err != nil {
		return nil, cerror.WrapError(cerror.ErrPulsarNewProducer, err)
	}
	producer, err := client.CreateProducer(*opt.producerOptions)
	if err != nil {
		client.Close()
		return nil, cerror.WrapError(cerror.ErrPulsarNewProducer, err)
	}
	partitions, err := client.TopicPartitions(opt.producerOptions.Topic)
	if err != nil {
		client.Close()
		return nil, cerror.WrapError(cerror.ErrPulsarNewProducer, err)
	}
	return &Producer{
		errCh:      errCh,
		opt:        *opt,
		client:     client,
		producer:   producer,
		partitions: len(partitions),
	}, nil
}

// Producer provide a way to send msg to pulsar.
type Producer struct {
	opt        Option
	client     pulsar.Client
	producer   pulsar.Producer
	errCh      chan error
	partitions int
}

func propertiesFromEventContext(ctx producer.EventContext, partition int32) map[string]string {
	properties := map[string]string{route: strconv.Itoa(int(partition))}
	properties["Ts"] = strconv.FormatUint(ctx.GetTs(), 10)
	properties["Type"] = strconv.Itoa(int(ctx.GetType()))
	if schema := ctx.GetSchema(); schema != nil {
		properties["Schema"] = *schema
	}
	if table := ctx.GetSchema(); table != nil {
		properties["Table"] = *table
	}
	return properties
}

func getEventContext(ctx context.Context) producer.EventContext {
	eventCtx := ctx.Value(producer.EventContextKey)
	if eventCtx != nil {
		if ectx, ok := eventCtx.(producer.EventContext); ok {
			return ectx
		}
	}
	return nil
}

func propertiesForPartition(partition int32) map[string]string {
	return map[string]string{route: strconv.Itoa(int(partition))}
}

func propertiesFromContext(ctx context.Context, partition int32) map[string]string {
	if eventCtx := getEventContext(ctx); eventCtx != nil {
		return propertiesFromEventContext(eventCtx, partition)
	}
	return propertiesForPartition(partition)
}

// SendMessage send key-value msg to target partition.
func (p *Producer) SendMessage(ctx context.Context, key []byte, value []byte, partition int32) error {
	p.producer.SendAsync(ctx, &pulsar.ProducerMessage{
		Payload:    value,
		Key:        string(key),
		Properties: propertiesFromContext(ctx, partition),
	}, p.errors)
	return nil
}

func (p *Producer) errors(_ pulsar.MessageID, _ *pulsar.ProducerMessage, err error) {
	if err != nil {
		select {
		case p.errCh <- cerror.WrapError(cerror.ErrPulsarSendMessage, err):
		default:
			log.Error("error channel is full", zap.Error(err))
		}
	}
}

// SyncBroadcastMessage send key-value msg to all partition.
func (p *Producer) SyncBroadcastMessage(ctx context.Context, key []byte, value []byte) error {
	eventCtx := getEventContext(ctx)
	var properties map[string]string
	for i := 0; i < p.partitions; i++ {
		var partition = int32(i)
		if eventCtx != nil {
			properties = propertiesFromEventContext(eventCtx, partition)
		} else {
			properties = propertiesForPartition(partition)
		}
		_, err := p.producer.Send(ctx, &pulsar.ProducerMessage{
			Payload:    value,
			Key:        string(key),
			Properties: properties,
		})
		if err != nil {
			return cerror.WrapError(cerror.ErrPulsarSendMessage, p.producer.Flush())
		}
	}
	return nil
}

// Flush flush all in memory msgs to server.
func (p *Producer) Flush(_ context.Context) error {
	return cerror.WrapError(cerror.ErrPulsarSendMessage, p.producer.Flush())
}

// GetPartitionNum got current topic's partitions size.
func (p *Producer) GetPartitionNum() int32 {
	return int32(p.partitions)
}

// Close close the producer.
func (p *Producer) Close() error {
	err := p.producer.Flush()
	if err != nil {
		return cerror.WrapError(cerror.ErrPulsarSendMessage, err)
	}
	p.producer.Close()
	p.client.Close()
	return nil
}
