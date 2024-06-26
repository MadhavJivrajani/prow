/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package writer

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"google.golang.org/genproto/googleapis/devtools/resultstore/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// Number of UploadRequest messages per batch recommended by the
	// ResultStore maintainers. This is likely not a factor unless
	// this implementation is changed to upload individual tests.
	batchSize = 100
)

var (
	// rpcRetryBackoff returns the Backoff for retrying CreateInvocation
	// and UploadBatch requests to ResultStore.
	rpcRetryBackoff = wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2,
		Cap:      30 * time.Second,
		Steps:    8,
		Jitter:   0.2,
	}
	// rpcRetryDuration returns the time allowed for all retries of a
	// single CreateInvocation or UploadBatch request to ResultStore.
	rpcRetryDuration = 5 * time.Minute
)

func resumeToken() string {
	// ResultStore resume tokens must be unique and be "web safe
	// Base64 encoded bytes."
	return base64.StdEncoding.EncodeToString([]byte(uuid.New().String()))
}

type ResultStoreBatchClient interface {
	CreateInvocation(context.Context, *resultstore.CreateInvocationRequest, ...grpc.CallOption) (*resultstore.Invocation, error)
	GetInvocationUploadMetadata(context.Context, *resultstore.GetInvocationUploadMetadataRequest, ...grpc.CallOption) (*resultstore.UploadMetadata, error)
	TouchInvocation(context.Context, *resultstore.TouchInvocationRequest, ...grpc.CallOption) (*resultstore.TouchInvocationResponse, error)
	UploadBatch(ctx context.Context, in *resultstore.UploadBatchRequest, opts ...grpc.CallOption) (*resultstore.UploadBatchResponse, error)
}

// writer writes results to resultstore using the UpdateBatch API.
type writer struct {
	log         *logrus.Entry
	client      ResultStoreBatchClient
	invID       string
	authToken   string
	resumeToken string
	updates     []*resultstore.UploadRequest
	finalized   bool
}

// IsPermanentError returns whether the error status code is permanent based on
// the ResultStore implementation, according to the ResultStore maintainers.
// (No external documentation is known.) Permanent errors will never succeed
// and should not be retried. Transient errors should be retried with
// exponential backoff.
func IsPermanentError(err error) bool {
	status, _ := status.FromError(err)
	switch status.Code() {
	case codes.AlreadyExists:
		return true
	case codes.NotFound:
		return true
	case codes.InvalidArgument:
		return true
	case codes.FailedPrecondition:
		return true
	case codes.Unimplemented:
		return true
	case codes.PermissionDenied:
		return true
	}
	return false
}

// IsAlreadyExistsErr returns whether the error status code is AlreadyExists.
func IsAlreadyExistsErr(err error) bool {
	status, _ := status.FromError(err)
	return status.Code() == codes.AlreadyExists
}

// New creates Invocation inv in ResultStore and returns a writer to add
// resource protos and finalize the Invocation. If the Invocation already
// exists and is finalized, a permanent error is returned. Otherwise, the
// writer syncs with ResultStore to resume writing. RPCs are retried with
// exponential backoff unless there is a permanent error, which is returned
// immediately. The caller should check whether a returned error is permanent
// using IsPermanentError() and only retry transient errors. The authToken is
// a UUID and must be identical across all calls for the same Invocation.
func New(ctx context.Context, log *logrus.Entry, client ResultStoreBatchClient, inv *resultstore.Invocation, invID, authToken string) (*writer, error) {
	w := &writer{
		log:         log,
		client:      client,
		invID:       invID,
		authToken:   authToken,
		resumeToken: resumeToken(),
		updates:     []*resultstore.UploadRequest{},
	}
	ctx, cancel := context.WithTimeout(ctx, rpcRetryDuration)
	defer cancel()

	err := w.createInvocation(ctx, inv)
	if err == nil {
		return w, nil
	}
	if !IsAlreadyExistsErr(err) {
		return nil, err
	}

	if touchErr := w.touchInvocation(ctx); IsPermanentError(touchErr) {
		// Since it was confirmed above that the Invocation exists, a
		// permanent error here indicates the Invocation is finalized.
		return nil, err
	}

	if err = w.retrieveResumeToken(ctx); err != nil {
		return nil, err
	}

	log.Info("Resuming upload for unfinalized invocation")
	return w, nil
}

// onlyPermanentError returns err only if it is permanent. Used to prevent
// retries for RPC errors which will never succeed.
func onlyPermanentError(err error) error {
	if IsPermanentError(err) {
		return err
	}
	return nil
}

func (w *writer) createInvocation(ctx context.Context, inv *resultstore.Invocation) error {
	return wait.ExponentialBackoffWithContext(ctx, rpcRetryBackoff, func(ctx context.Context) (bool, error) {
		_, err := w.client.CreateInvocation(ctx, w.createInvocationRequest(inv))
		if err != nil {
			w.log.Errorf("resultstore.CreateInvocation: %v", err)
			return false, onlyPermanentError(err)
		}
		return true, nil
	})
}

func (w *writer) touchInvocation(ctx context.Context) error {
	return wait.ExponentialBackoffWithContext(ctx, rpcRetryBackoff, func(ctx context.Context) (bool, error) {
		_, err := w.client.TouchInvocation(ctx, w.touchInvocationRequest())
		if err != nil {
			w.log.Errorf("resultstore.TouchInvocation: %v", err)
			return false, onlyPermanentError(err)
		}
		return true, nil
	})
}

func (w *writer) retrieveResumeToken(ctx context.Context) error {
	return wait.ExponentialBackoffWithContext(ctx, rpcRetryBackoff, func(ctx context.Context) (bool, error) {
		meta, err := w.client.GetInvocationUploadMetadata(ctx, w.getInvocationUploadMetadataRequest())
		if err != nil {
			w.log.Errorf("resultstore.GetInvocationUploadMetadata: %v", err)
			return false, onlyPermanentError(err)
		}
		w.resumeToken = meta.ResumeToken
		return true, nil
	})
}

func (w *writer) WriteConfiguration(ctx context.Context, c *resultstore.Configuration) error {
	return w.addUploadRequest(ctx, createConfigurationUploadRequest(c))
}

func (w *writer) WriteTarget(ctx context.Context, t *resultstore.Target) error {
	return w.addUploadRequest(ctx, createTargetUploadRequest(t))
}

func (w *writer) WriteConfiguredTarget(ctx context.Context, ct *resultstore.ConfiguredTarget) error {
	return w.addUploadRequest(ctx, createConfiguredTargetUploadRequest(ct))
}

func (w *writer) WriteAction(ctx context.Context, a *resultstore.Action) error {
	return w.addUploadRequest(ctx, createActionUploadRequest(a))
}

func (w *writer) Finalize(ctx context.Context) error {
	return w.addUploadRequest(ctx, w.finalizeRequest())
}

func (w *writer) createInvocationRequest(inv *resultstore.Invocation) *resultstore.CreateInvocationRequest {
	return &resultstore.CreateInvocationRequest{
		InvocationId:       w.invID,
		Invocation:         inv,
		AuthorizationToken: w.authToken,
		InitialResumeToken: w.resumeToken,
	}
}

func (w *writer) invocationName() string {
	return fmt.Sprintf("invocations/%s", w.invID)
}

func (w *writer) touchInvocationRequest() *resultstore.TouchInvocationRequest {
	return &resultstore.TouchInvocationRequest{
		Name:               w.invocationName(),
		AuthorizationToken: w.authToken,
	}
}

func (w *writer) uploadMetadataName() string {
	return fmt.Sprintf("invocations/%s/uploadMetadata", w.invID)
}

func (w *writer) getInvocationUploadMetadataRequest() *resultstore.GetInvocationUploadMetadataRequest {
	return &resultstore.GetInvocationUploadMetadataRequest{
		Name:               w.uploadMetadataName(),
		AuthorizationToken: w.authToken,
	}
}

func (w *writer) addUploadRequest(ctx context.Context, r *resultstore.UploadRequest) error {
	if w.finalized {
		return fmt.Errorf("addUploadRequest after finalized for %v", r)
	}
	if r.UploadOperation == resultstore.UploadRequest_FINALIZE {
		w.finalized = true
	}
	w.updates = append(w.updates, r)
	if !w.finalized && len(w.updates) < batchSize {
		return nil
	}
	return w.flushUpdates(ctx)
}

func (w *writer) flushUpdates(ctx context.Context) error {
	b := w.uploadBatchRequest(w.updates)
	ctx, cancel := context.WithTimeout(ctx, rpcRetryDuration)
	defer cancel()
	return wait.ExponentialBackoffWithContext(ctx, rpcRetryBackoff, func(ctx context.Context) (bool, error) {
		if _, err := w.client.UploadBatch(ctx, b); err != nil {
			w.log.Errorf("resultstore.UploadBatch: %v", err)
			if IsPermanentError(err) {
				// End retries by returning error.
				return false, err
			}
			return false, nil
		}
		w.updates = []*resultstore.UploadRequest{}
		return true, nil
	})
}

func (w *writer) uploadBatchRequest(reqs []*resultstore.UploadRequest) *resultstore.UploadBatchRequest {
	nextToken := resumeToken()
	req := &resultstore.UploadBatchRequest{
		Parent:             w.invocationName(),
		ResumeToken:        w.resumeToken,
		NextResumeToken:    nextToken,
		AuthorizationToken: w.authToken,
		UploadRequests:     reqs,
	}
	w.resumeToken = nextToken
	return req
}

func (w *writer) finalizeRequest() *resultstore.UploadRequest {
	return &resultstore.UploadRequest{
		UploadOperation: resultstore.UploadRequest_FINALIZE,
		Resource:        &resultstore.UploadRequest_Invocation{},
	}
}

func createConfigurationUploadRequest(c *resultstore.Configuration) *resultstore.UploadRequest {
	id := &resultstore.UploadRequest_Id{
		ConfigurationId: c.Id.ConfigurationId,
	}
	c.Id = nil
	return &resultstore.UploadRequest{
		Id:              id,
		UploadOperation: resultstore.UploadRequest_CREATE,
		Resource: &resultstore.UploadRequest_Configuration{
			Configuration: c,
		},
	}
}

func createTargetUploadRequest(t *resultstore.Target) *resultstore.UploadRequest {
	id := &resultstore.UploadRequest_Id{
		TargetId: t.Id.GetTargetId(),
	}
	t.Id = nil
	return &resultstore.UploadRequest{
		Id:              id,
		UploadOperation: resultstore.UploadRequest_CREATE,
		Resource: &resultstore.UploadRequest_Target{
			Target: t,
		},
	}
}

func createConfiguredTargetUploadRequest(ct *resultstore.ConfiguredTarget) *resultstore.UploadRequest {
	id := &resultstore.UploadRequest_Id{
		TargetId:        ct.Id.GetTargetId(),
		ConfigurationId: ct.Id.GetConfigurationId(),
	}
	ct.Id = nil
	return &resultstore.UploadRequest{
		Id:              id,
		UploadOperation: resultstore.UploadRequest_CREATE,
		Resource: &resultstore.UploadRequest_ConfiguredTarget{
			ConfiguredTarget: ct,
		},
	}
}

func createActionUploadRequest(a *resultstore.Action) *resultstore.UploadRequest {
	id := &resultstore.UploadRequest_Id{
		TargetId:        a.Id.GetTargetId(),
		ConfigurationId: a.Id.GetConfigurationId(),
		ActionId:        a.Id.GetActionId(),
	}
	a.Id = nil
	return &resultstore.UploadRequest{
		Id:              id,
		UploadOperation: resultstore.UploadRequest_CREATE,
		Resource: &resultstore.UploadRequest_Action{
			Action: a,
		},
	}
}
