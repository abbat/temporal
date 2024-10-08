// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package eventhandler

import (
	"context"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/server/api/adminservice/v1"
	historyspb "go.temporal.io/server/api/history/v1"
	"go.temporal.io/server/client"
	"go.temporal.io/server/common/collection"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/namespace"
	"go.temporal.io/server/common/persistence/serialization"
	"go.temporal.io/server/common/rpc"
	"go.uber.org/fx"
)

const (
	resendContextTimeout = 30 * time.Second
)

//go:generate mockgen -copyright_file ../../../../LICENSE -package $GOPACKAGE -source $GOFILE -destination remote_history_paginated_fetcher_mock.go

type (
	// HistoryPaginatedFetcher is the interface for fetching history from remote cluster
	// Start and End event ID is exclusive
	HistoryPaginatedFetcher interface {
		GetSingleWorkflowHistoryPaginatedIteratorExclusive(
			ctx context.Context,
			remoteClusterName string,
			namespaceID namespace.ID,
			workflowID string,
			runID string,
			startEventID int64, // exclusive
			startEventVersion int64,
			endEventID int64, // exclusive
			endEventVersion int64,
		) collection.Iterator[*HistoryBatch]

		GetSingleWorkflowHistoryPaginatedIteratorInclusive(
			ctx context.Context,
			remoteClusterName string,
			namespaceID namespace.ID,
			workflowID string,
			runID string,
			startEventID int64, // inclusive
			startEventVersion int64,
			endEventID int64, // inclusive
			endEventVersion int64,
		) collection.Iterator[*HistoryBatch]
	}

	HistoryPaginatedFetcherImpl struct {
		fx.In

		NamespaceRegistry namespace.Registry
		ClientBean        client.Bean
		Serializer        serialization.Serializer
		Logger            log.Logger
	}

	HistoryBatch struct {
		VersionHistory *historyspb.VersionHistory
		RawEventBatch  *commonpb.DataBlob
	}
)

const (
	defaultPageSize = int32(100)
)

func NewHistoryPaginatedFetcher(
	namespaceRegistry namespace.Registry,
	clientBean client.Bean,
	serializer serialization.Serializer,
	logger log.Logger,
) HistoryPaginatedFetcher {
	return &HistoryPaginatedFetcherImpl{
		NamespaceRegistry: namespaceRegistry,
		ClientBean:        clientBean,
		Serializer:        serializer,
		Logger:            logger,
	}
}

func (n *HistoryPaginatedFetcherImpl) GetSingleWorkflowHistoryPaginatedIteratorInclusive(
	ctx context.Context,
	remoteClusterName string,
	namespaceID namespace.ID,
	workflowID string,
	runID string,
	startEventID int64,
	startEventVersion int64,
	endEventID int64,
	endEventVersion int64,
) collection.Iterator[*HistoryBatch] {
	return collection.NewPagingIterator(n.getPaginationFn(
		ctx,
		remoteClusterName,
		namespaceID,
		workflowID,
		runID,
		startEventID,
		startEventVersion,
		endEventID,
		endEventVersion,
		true,
	))
}

func (n *HistoryPaginatedFetcherImpl) GetSingleWorkflowHistoryPaginatedIteratorExclusive(
	ctx context.Context,
	remoteClusterName string,
	namespaceID namespace.ID,
	workflowID string,
	runID string,
	startEventID int64,
	startEventVersion int64,
	endEventID int64,
	endEventVersion int64,
) collection.Iterator[*HistoryBatch] {
	return collection.NewPagingIterator(n.getPaginationFn(
		ctx,
		remoteClusterName,
		namespaceID,
		workflowID,
		runID,
		startEventID,
		startEventVersion,
		endEventID,
		endEventVersion,
		false,
	))
}

func (n *HistoryPaginatedFetcherImpl) getPaginationFn(
	ctx context.Context,
	remoteClusterName string,
	namespaceID namespace.ID,
	workflowID string,
	runID string,
	startEventID int64,
	startEventVersion int64,
	endEventID int64,
	endEventVersion int64,
	inclusive bool,
) collection.PaginationFn[*HistoryBatch] {
	return func(paginationToken []byte) ([]*HistoryBatch, []byte, error) {
		return n.getHistory(
			ctx,
			remoteClusterName,
			namespaceID,
			workflowID,
			runID,
			startEventID,
			startEventVersion,
			endEventID,
			endEventVersion,
			paginationToken,
			defaultPageSize,
			inclusive,
		)
	}
}

func (n *HistoryPaginatedFetcherImpl) getHistory(
	ctx context.Context,
	remoteClusterName string,
	namespaceID namespace.ID,
	workflowID string,
	runID string,
	startEventID int64,
	startEventVersion int64,
	endEventID int64,
	endEventVersion int64,
	token []byte,
	pageSize int32,
	inclusive bool,
) ([]*HistoryBatch, []byte, error) {

	logger := log.With(n.Logger, tag.WorkflowRunID(runID))

	ctx, cancel := rpc.NewContextFromParentWithTimeoutAndVersionHeaders(ctx, resendContextTimeout)
	defer cancel()

	adminClient, err := n.ClientBean.GetRemoteAdminClient(remoteClusterName)
	if err != nil {
		return nil, nil, err
	}
	getResponse := func() ([]*commonpb.DataBlob, *historyspb.VersionHistory, []byte, error) {
		if inclusive {
			response, err := adminClient.GetWorkflowExecutionRawHistory(ctx, &adminservice.GetWorkflowExecutionRawHistoryRequest{
				NamespaceId: namespaceID.String(),
				Execution: &commonpb.WorkflowExecution{
					WorkflowId: workflowID,
					RunId:      runID,
				},
				StartEventId:      startEventID,
				StartEventVersion: startEventVersion,
				EndEventId:        endEventID,
				EndEventVersion:   endEventVersion,
				MaximumPageSize:   pageSize,
				NextPageToken:     token,
			})
			if err != nil {
				return nil, nil, nil, err
			}
			return response.GetHistoryBatches(), response.GetVersionHistory(), response.NextPageToken, nil
		}
		response, err := adminClient.GetWorkflowExecutionRawHistoryV2(ctx, &adminservice.GetWorkflowExecutionRawHistoryV2Request{
			NamespaceId: namespaceID.String(),
			Execution: &commonpb.WorkflowExecution{
				WorkflowId: workflowID,
				RunId:      runID,
			},
			StartEventId:      startEventID,
			StartEventVersion: startEventVersion,
			EndEventId:        endEventID,
			EndEventVersion:   endEventVersion,
			MaximumPageSize:   pageSize,
			NextPageToken:     token,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		return response.GetHistoryBatches(), response.GetVersionHistory(), response.NextPageToken, nil
	}

	dataBlobs, versionHistory, nextPageToken, err := getResponse()
	if err != nil {
		logger.Error("error getting history", tag.Error(err))
		return nil, nil, err
	}

	batches := make([]*HistoryBatch, 0, len(dataBlobs))
	for _, history := range dataBlobs {
		batch := &HistoryBatch{
			VersionHistory: versionHistory,
			RawEventBatch:  history,
		}
		batches = append(batches, batch)
	}
	return batches, nextPageToken, nil
}
