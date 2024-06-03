package client

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sync"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/raft/constants"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/raft/rbuilder"
	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/util/canary"
	"github.com/buildbuddy-io/buildbuddy/server/util/grpc_client"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/proto"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/buildbuddy-io/buildbuddy/server/util/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/lni/dragonboat/v4"
	"github.com/lni/dragonboat/v4/client"

	rfpb "github.com/buildbuddy-io/buildbuddy/proto/raft"
	rfspb "github.com/buildbuddy-io/buildbuddy/proto/raft_service"
	dbsm "github.com/lni/dragonboat/v4/statemachine"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	gstatus "google.golang.org/grpc/status"
)

var (
	sessionLifetime = flag.Duration("cache.raft.client_session_lifetime", 1*time.Hour, "The duration of a client session before it's reset")
)

// A default timeout that can be applied to raft requests that do not have one
// set.
const DefaultContextTimeout = 10 * time.Second

const DefaultSessionLifetime = 1 * time.Hour

type NodeHost interface {
	ID() string
	GetNoOPSession(shardID uint64) *client.Session
	SyncPropose(ctx context.Context, session *client.Session, cmd []byte) (dbsm.Result, error)
	SyncRead(ctx context.Context, shardID uint64, query interface{}) (interface{}, error)
	ReadIndex(shardID uint64, timeout time.Duration) (*dragonboat.RequestState, error)
	ReadLocalNode(rs *dragonboat.RequestState, query interface{}) (interface{}, error)
	StaleRead(shardID uint64, query interface{}) (interface{}, error)
}

type IRegistry interface {
	// Lookup the grpc address given a replica's shardID and replicaID
	ResolveGRPC(shardID uint64, replicaID uint64) (string, string, error)
}

type APIClient struct {
	env      environment.Env
	log      log.Logger
	mu       sync.Mutex
	clients  map[string]*grpc_client.ClientConnPool
	registry IRegistry
}

func NewAPIClient(env environment.Env, name string, registry IRegistry) *APIClient {
	return &APIClient{
		env:      env,
		log:      log.NamedSubLogger(fmt.Sprintf("Coordinator(%s)", name)),
		clients:  make(map[string]*grpc_client.ClientConnPool),
		registry: registry,
	}
}

func (c *APIClient) getClient(ctx context.Context, peer string) (rfspb.ApiClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if client, ok := c.clients[peer]; ok {
		conn, err := client.GetReadyConnection()
		if err != nil {
			return nil, status.UnavailableErrorf("no connections to peer %q are ready", peer)
		}
		return rfspb.NewApiClient(conn), nil
	}
	log.Debugf("Creating new client for peer: %q", peer)
	conn, err := grpc_client.DialSimple("grpc://" + peer)
	if err != nil {
		return nil, err
	}
	c.clients[peer] = conn
	return rfspb.NewApiClient(conn), nil
}

func (c *APIClient) Get(ctx context.Context, peer string) (rfspb.ApiClient, error) {
	return c.getClient(ctx, peer)
}

func (c *APIClient) GetForReplica(ctx context.Context, rd *rfpb.ReplicaDescriptor) (rfspb.ApiClient, error) {
	addr, _, err := c.registry.ResolveGRPC(rd.GetShardId(), rd.GetReplicaId())
	if err != nil {
		return nil, err
	}
	return c.getClient(ctx, addr)
}

func singleOpTimeout(ctx context.Context) time.Duration {
	// This value should be approximately 10x the config.RTTMilliseconds,
	// but we want to include a little more time for the operation itself to
	// complete.
	const maxTimeout = time.Second
	if deadline, ok := ctx.Deadline(); ok {
		dur := time.Until(deadline)
		if dur <= 0 {
			return dur
		}
		if dur < maxTimeout {
			// ensure that the returned duration / constants.RTTMillisecond > 0.
			return dur + constants.RTTMillisecond
		}
	}
	return maxTimeout
}

func RunNodehostFn(ctx context.Context, nhf func(ctx context.Context) error) error {
	// Ensure that the outer context has a timeout set to limit the total
	// time we'll attempt to run an operation.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultContextTimeout)
		defer cancel()
	}
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		default:
			// continue with for loop
		}

		timeout := singleOpTimeout(ctx)
		if timeout <= 0 {
			// The deadline has already passed.
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, timeout)
		err := nhf(opCtx)
		cancel()

		if err != nil {
			isTimeoutTooSmall := errors.Is(err, dragonboat.ErrTimeoutTooSmall)
			if !isTimeoutTooSmall {
				// ErrTimeoutTooSmall is less interesting than the prior error.
				lastErr = err
			}
			if dragonboat.IsTempError(err) {
				continue
			}
			if isTimeoutTooSmall && lastErr != nil {
				return lastErr
			}
			return err
		}
		return nil
	}
}

type Session struct {
	id        string
	index     uint64
	expiredAt time.Time

	clock    clockwork.Clock
	lifetime time.Duration
	mu       sync.Mutex
}

func (s *Session) ToProto() *rfpb.Session {
	return &rfpb.Session{
		Id:         []byte(s.id),
		Index:      s.index,
		ExpiryUsec: s.expiredAt.UnixMicro(),
	}
}

func SessionLifetime() time.Duration {
	return *sessionLifetime
}

func NewDefaultSession() *Session {
	return NewSession(clockwork.NewRealClock(), *sessionLifetime)
}

func NewSession(clock clockwork.Clock, lifetime time.Duration) *Session {
	return &Session{
		id:        uuid.New(),
		index:     0,
		lifetime:  lifetime,
		clock:     clock,
		expiredAt: clock.Now().Add(lifetime),
	}

}

// maybeRefresh resets the id and index when the session expired.
func (s *Session) maybeRefresh() {
	now := s.clock.Now()
	if s.expiredAt.Before(now) {
		return
	}

	s.id = uuid.New()
	s.index = 0
	s.expiredAt = now.Add(s.lifetime)
}

func (s *Session) SyncProposeLocal(ctx context.Context, nodehost NodeHost, shardID uint64, batch *rfpb.BatchCmdRequest) (*rfpb.BatchCmdResponse, error) {
	// At most one SyncProposeLocal can be run per session.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Refreshes the session if necessary
	s.maybeRefresh()

	sesh := nodehost.GetNoOPSession(shardID)

	s.index++
	batch.Session = s.ToProto()
	buf, err := proto.Marshal(batch)
	if err != nil {
		return nil, err
	}
	var raftResponse dbsm.Result
	err = RunNodehostFn(ctx, func(ctx context.Context) error {
		defer canary.Start("nodehost.SyncPropose", time.Second)()
		result, err := nodehost.SyncPropose(ctx, sesh, buf)
		if err != nil {
			return err
		}
		if result.Value == constants.EntryErrorValue {
			s := &statuspb.Status{}
			if err := proto.Unmarshal(result.Data, s); err != nil {
				return err
			}
			if err := gstatus.FromProto(s).Err(); err != nil {
				return err
			}
		}
		raftResponse = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	batchResponse := &rfpb.BatchCmdResponse{}
	if err := proto.Unmarshal(raftResponse.Data, batchResponse); err != nil {
		return nil, err
	}
	return batchResponse, err
}

func SyncReadLocal(ctx context.Context, nodehost NodeHost, shardID uint64, batch *rfpb.BatchCmdRequest) (*rfpb.BatchCmdResponse, error) {
	buf, err := proto.Marshal(batch)
	if err != nil {
		return nil, err
	}

	if batch.Header == nil {
		return nil, status.FailedPreconditionError("Header must be set")
	}
	var raftResponseIface interface{}
	err = RunNodehostFn(ctx, func(ctx context.Context) error {
		switch batch.GetHeader().GetConsistencyMode() {
		case rfpb.Header_LINEARIZABLE:
			rs, err := nodehost.ReadIndex(shardID, singleOpTimeout(ctx))
			if err != nil {
				return err
			}
			v := <-rs.ResultC()
			if v.Completed() {
				raftResponseIface, err = nodehost.ReadLocalNode(rs, buf)
				return err
			} else if v.Rejected() {
				return dragonboat.ErrRejected
			} else if v.Timeout() {
				return dragonboat.ErrTimeout
			} else if v.Terminated() {
				return dragonboat.ErrShardClosed
			} else if v.Dropped() {
				return dragonboat.ErrShardNotReady
			} else if v.Aborted() {
				return dragonboat.ErrAborted
			} else {
				return status.FailedPreconditionError("Failed to read result")
			}

		case rfpb.Header_STALE, rfpb.Header_RANGELEASE:
			raftResponseIface, err = nodehost.StaleRead(shardID, buf)
			return err
		default:
			return status.UnknownError("Unknown consistency mode")
		}

	})
	if err != nil {
		return nil, err
	}
	buf, ok := raftResponseIface.([]byte)
	if !ok {
		return nil, status.FailedPreconditionError("SyncRead returned a non-[]byte response.")
	}

	batchResponse := &rfpb.BatchCmdResponse{}
	if err := proto.Unmarshal(buf, batchResponse); err != nil {
		return nil, err
	}
	return batchResponse, nil
}

func SyncReadLocalBatch(ctx context.Context, nodehost *dragonboat.NodeHost, shardID uint64, builder *rbuilder.BatchBuilder) (*rbuilder.BatchResponse, error) {
	batch, err := builder.ToProto()
	if err != nil {
		return nil, err
	}
	rsp, err := SyncReadLocal(ctx, nodehost, shardID, batch)
	if err != nil {
		return nil, err
	}
	return rbuilder.NewBatchResponseFromProto(rsp), nil
}
