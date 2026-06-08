package egress

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	configv1 "github.com/dio/transit/examples/orange/api/orange/config/v1"
	configv1connect "github.com/dio/transit/examples/orange/api/orange/config/v1/configv1connect"
	egressv1 "github.com/dio/transit/examples/orange/api/orange/egress/v1"
	egressv1connect "github.com/dio/transit/examples/orange/api/orange/egress/v1/egressv1connect"
	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/vtprotocodec"
)

// Client authenticates to orange CP using bundle credentials and exposes
// Heartbeat and Fetch RPCs. Cursor state (lastVersion/lastChecksum) is tracked
// internally so callers get incremental Fetch semantics automatically.
type Client struct {
	Resolver *config.CachedResolver

	heartbeatClient egressv1connect.EgressServiceClient
	snapshotClient  configv1connect.SnapshotServiceClient

	mu           sync.Mutex
	lastVersion  uint64
	lastChecksum []byte
}

// NewClient builds a Client from a loaded bundle. Returns an error if the
// private key in the bundle cannot be parsed.
func NewClient(bundle *BundleData) (*Client, error) {
	privKey, err := ParseEd25519PrivateKey(bundle.EgressKey)
	if err != nil {
		return nil, fmt.Errorf("parse egress.key: %w", err)
	}

	transport := &AssertionTransport{
		Base:        http.DefaultTransport,
		PrivKey:     privKey,
		EgressID:    bundle.EgressID,
		WorkspaceID: bundle.WorkspaceID,
	}
	httpClient := &http.Client{Timeout: 15 * time.Second, Transport: transport}
	opts := []connect.ClientOption{connect.WithCodec(vtprotocodec.Codec{})}

	return &Client{
		heartbeatClient: egressv1connect.NewEgressServiceClient(httpClient, bundle.ServerURL, opts...),
		snapshotClient:  configv1connect.NewSnapshotServiceClient(httpClient, bundle.ServerURL, opts...),
		Resolver:        config.NewDefaultResolver(5 * time.Minute),
	}, nil
}

// Heartbeat sends a single Heartbeat RPC to the management plane. Returns the
// server time reported by the CP on success (useful for display); returns the
// zero time on error.
func (c *Client) Heartbeat(ctx context.Context) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.heartbeatClient.Heartbeat(ctx, connect.NewRequest(&egressv1.HeartbeatRequest{}))
	if err != nil {
		return time.Time{}, err
	}
	return resp.Msg.GetServerTime().AsTime(), nil
}

// Fetch polls the snapshot service. Returns (snap, raw, true, nil) when a new
// snapshot arrived, (nil, nil, false, nil) for Unchanged, or (nil, nil, false,
// err) on failure. The cursor (lastVersion/lastChecksum) is advanced
// automatically on success so subsequent calls get incremental responses.
func (c *Client) Fetch(ctx context.Context) (*configv1.SnapshotEnvelope, *config.RawConfig, bool, error) {
	c.mu.Lock()
	lastVersion := c.lastVersion
	lastChecksum := c.lastChecksum
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	resp, err := c.snapshotClient.Fetch(ctx, connect.NewRequest(&configv1.FetchRequest{
		LastVersion:  lastVersion,
		LastChecksum: lastChecksum,
	}))
	if err != nil {
		return nil, nil, false, err
	}

	if resp.Msg.GetUnchanged() != nil {
		return nil, nil, false, nil
	}

	snap := resp.Msg.GetSnapshot()
	if snap == nil {
		return nil, nil, false, nil
	}

	raw, err := config.DecodeRawFromProtoEnvelope(snap)
	if err != nil {
		return nil, nil, false, fmt.Errorf("decode snapshot: %w", err)
	}

	c.mu.Lock()
	c.lastVersion = snap.GetVersion()
	c.lastChecksum = snap.GetChecksum()
	c.mu.Unlock()

	return snap, raw, true, nil
}
