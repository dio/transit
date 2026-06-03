// Package clusterasyncrouter demonstrates body-driven upstream selection using
// the async ClusterLBCompletion pattern documented at
// docs/envoy-dynamic-module-upstream-selection.md (lines 154–235).
//
// The phase-ordering problem: when an HTTP filter wants to route based on data
// parsed from the request body, ChooseHost runs before the body callback. By
// the time the body handler writes filter state, the cluster has already
// returned nil and the request fails with upstream_cx_none_healthy.
//
// The fix: at headers phase, mint a per-request token and register a Pending.
// Write the token to filter state. ChooseHost reads the token, looks up the
// Pending, and returns an async completion. A goroutine parks on Pending.Done;
// when the body handler parses the routing field and calls Pending.Resolve, the
// goroutine hops back to the cluster main thread via handle.Schedule and calls
// completion.Complete(host, "").
//
// The expected body shape is JSON of the form {"target":"a"}, where the value
// names one of the upstreams listed in the cluster config.
package clusterasyncrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dio/transit/up"
)

const (
	FilterName  = "body-router-writer"
	ClusterName = "body-router-cluster"

	// StateToken is the per-request handoff key written at headers phase. The
	// cluster extension reads it in ChooseHost to find the matching Pending.
	StateToken = "body-router.token"
)

func init() {
	up.Register(FilterName, requestHandler, up.WithMutableBody(bodyHandler))
	up.RegisterCluster(ClusterName, &factory{})
}

// =============================================================================
// HTTP filter
// =============================================================================

// streamState lives in the per-stream context slot so the body handler can find
// the token it minted at headers phase without re-deriving anything.
type streamState struct {
	token string
	p     *Pending
}

var tokenSeq atomic.Uint64

func mintToken() string {
	return "tr-" + strconv.FormatUint(tokenSeq.Add(1), 36)
}

func requestHandler(w *up.Writer, r *up.Request) {
	if r.Context == nil {
		return
	}
	token := mintToken()
	p := Register(token)
	*r.Context = &streamState{token: token, p: p}
	w.SetFilterState(StateToken, token)
}

func bodyHandler(_ *up.Writer, chunk *up.BodyChunk) {
	if !chunk.EndStream {
		return
	}
	if chunk.Context == nil || *chunk.Context == nil {
		return
	}
	st, ok := (*chunk.Context).(*streamState)
	if !ok || st == nil {
		return
	}
	defer Delete(st.token)

	target, err := ExtractTarget(chunk.Data)
	if err != nil {
		st.p.Resolve(Result{Err: err.Error()})
		return
	}
	st.p.Resolve(Result{Upstream: target})
}

// ExtractTarget parses {"target":"<name>"} from body and returns the name.
func ExtractTarget(data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("empty body")
	}
	var m struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("invalid json")
	}
	if m.Target == "" {
		return "", fmt.Errorf("missing target")
	}
	return m.Target, nil
}

// =============================================================================
// Pending registry (token -> handoff)
// =============================================================================

// Result is what the body handler resolves a Pending with. Err names the
// failure when set; Upstream is the configured upstream name otherwise.
type Result struct {
	Upstream string
	Err      string
}

type Pending struct {
	done chan struct{}
	res  atomic.Pointer[Result]
}

func newPending() *Pending { return &Pending{done: make(chan struct{})} }

// Resolve publishes r. First call wins.
func (p *Pending) Resolve(r Result) bool {
	if !p.res.CompareAndSwap(nil, &r) {
		return false
	}
	close(p.done)
	return true
}

func (p *Pending) Done() <-chan struct{} { return p.done }

func (p *Pending) Result() (Result, bool) {
	r := p.res.Load()
	if r == nil {
		return Result{}, false
	}
	return *r, true
}

var registry sync.Map // token -> *Pending

func Register(token string) *Pending {
	p := newPending()
	actual, loaded := registry.LoadOrStore(token, p)
	if loaded {
		return actual.(*Pending)
	}
	return p
}

func Lookup(token string) *Pending {
	v, ok := registry.Load(token)
	if !ok {
		return nil
	}
	return v.(*Pending)
}

func Delete(token string) { registry.Delete(token) }

// =============================================================================
// Cluster extension
// =============================================================================

type clusterConfig struct {
	Hosts []hostEntry `json:"hosts"`
}

type hostEntry struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	SNI     string `json:"sni,omitempty"`    // if set, host metadata "sni" is populated and Hostname is used for auto_host_sni
	Bucket  string `json:"bucket,omitempty"` // if set, host metadata "bucket" is populated for metadata-driven examples
}

type factory struct{}

func (factory) Create(cfg []byte) (up.ClusterConfigFactory, error) {
	if len(cfg) == 0 {
		return nil, fmt.Errorf("cluster-async-router: config required")
	}
	var parsed clusterConfig
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		return nil, fmt.Errorf("cluster-async-router: parse config: %w", err)
	}
	if len(parsed.Hosts) == 0 {
		return nil, fmt.Errorf("cluster-async-router: config must include at least one host")
	}
	for i, h := range parsed.Hosts {
		if h.Name == "" || h.Address == "" {
			return nil, fmt.Errorf("cluster-async-router: host %d requires name and address", i)
		}
	}
	return &cfgFactory{hosts: parsed.Hosts}, nil
}

type cfgFactory struct {
	hosts []hostEntry
}

func (f *cfgFactory) NewCluster(_ up.ClusterHandle) up.Cluster {
	return &cluster{hosts: f.hosts}
}
func (f *cfgFactory) Close() {}

type cluster struct {
	handle up.ClusterHandle
	hosts  []hostEntry
	byName map[string]up.HostPtr
}

func (c *cluster) Init(h up.ClusterHandle) {
	c.handle = h
	c.byName = make(map[string]up.HostPtr, len(c.hosts))
	for _, he := range c.hosts {
		addr := resolveHostAddr(he.Address)
		spec := up.HostSpec{Address: addr}
		if he.SNI != "" {
			spec.Hostname = he.SNI
			spec.Metadata = map[string]string{"sni": he.SNI}
		}
		if he.Bucket != "" {
			if spec.Metadata == nil {
				spec.Metadata = make(map[string]string)
			}
			spec.Metadata["bucket"] = he.Bucket
		}
		ptrs := h.AddHosts([]up.HostSpec{spec})
		if len(ptrs) == 0 {
			continue
		}
		h.UpdateHostHealth(ptrs[0], up.HostHealthy)
		c.byName[he.Name] = ptrs[0]
	}
	h.PreInitComplete()
}

// resolveHostAddr resolves a "hostname:port" address to "ip:port" for Envoy's
// dynamic-module cluster, which expects numeric IPs. Returns addr unchanged if
// the host is already an IP or if resolution fails.
func resolveHostAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if net.ParseIP(host) != nil {
		return addr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil || len(ips) == 0 {
		return addr
	}
	return net.JoinHostPort(ips[0], port)
}

func (c *cluster) ServerInitialized(_ up.ClusterHandle)     {}
func (c *cluster) DrainStarted(_ up.ClusterHandle)          {}
func (c *cluster) Shutdown(_ up.ClusterHandle, done func()) { done() }
func (c *cluster) Close()                                   {}

func (c *cluster) NewClusterLB() up.ClusterLB {
	return &lb{
		owner:   c,
		waiters: make(map[*up.ClusterLBCompletion]struct{}),
	}
}

type lb struct {
	up.EmptyClusterLB
	owner *cluster

	mu      sync.Mutex
	waiters map[*up.ClusterLBCompletion]struct{}
}

func (l *lb) ChooseHost(_ up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	if ctx == nil {
		return nil, nil
	}
	token, ok := ctx.GetFilterState(StateToken)
	if !ok || token == "" {
		return nil, nil
	}
	p := Lookup(token)
	if p == nil {
		return nil, nil
	}

	completion := ctx.NewCompletion()
	l.mu.Lock()
	l.waiters[completion] = struct{}{}
	l.mu.Unlock()

	go l.waitAndComplete(p, completion)
	return nil, completion
}

func (l *lb) waitAndComplete(p *Pending, completion *up.ClusterLBCompletion) {
	<-p.Done()

	l.mu.Lock()
	_, alive := l.waiters[completion]
	delete(l.waiters, completion)
	l.mu.Unlock()
	if !alive {
		return
	}

	res, _ := p.Result()
	if res.Err != "" {
		l.owner.handle.Schedule(func() {
			completion.Complete(nil, res.Err)
		})
		return
	}
	host := l.owner.byName[res.Upstream]
	l.owner.handle.Schedule(func() {
		if host == nil {
			completion.Complete(nil, "unknown_upstream")
			return
		}
		completion.Complete(host, "")
	})
}

func (l *lb) CancelHostSelection(completion *up.ClusterLBCompletion) {
	l.mu.Lock()
	delete(l.waiters, completion)
	l.mu.Unlock()
}
