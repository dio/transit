// Package pick is the dynamic-modules cluster extension that selects an
// upstream host for each request based on the match.Decision stored in the
// per-stream object bag by the orange-match downstream filter.
//
// See README.md for design details: lifecycle, STRICT_DNS-style DNS refresh,
// multi-IP round-robin, cluster main-thread contract, and the async
// body-driven host-selection pattern.
//
// All hosts on the orange-pick cluster are added at runtime via
// ClusterHandle.AddHosts from reconcileSnapshot — never via xDS. The custom
// Envoy build enables auto_host_sni and a bounded SNI-scoped TLS session
// cache on the orange-pick upstream (see
// https://gist.github.com/dio/965d1e555909c02013ca882a2b3caa78), so
// runtime-added hosts handshake with their own SNI without xDS supplying
// it. New providers, bindings, or fallback targets are introduced by
// loading them into the config snapshot; do not reach for xDS for this.
package pick

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/dio/transit/examples/orange/internal/config"
	"github.com/dio/transit/examples/orange/internal/observability"
	"github.com/dio/transit/examples/orange/internal/pipeline/match"
	"github.com/dio/transit/up"
)

const (
	ClusterName = "orange-pick"

	// 5s accommodates Kubernetes ndots search-domain chaining, which can add
	// multiple failing lookups before the real hostname is tried.
	defaultResolveTimeout = 5 * time.Second

	// defaultDNSRefreshInterval is the fallback wake interval used by the
	// refresh loop when no hosts are registered yet (e.g. all Init resolves
	// failed). In the normal path the loop wakes at the earliest TTL expiry.
	defaultDNSRefreshInterval = 30 * time.Second

	// minTTLFloor clamps pathologically short TTLs and sets the retry delay
	// after a transient DNS failure so we don't hammer the resolver.
	minTTLFloor = 10 * time.Second
)

func init() {
	Register(observability.Logger("orange/pick"))
}

// Register registers the pick cluster factory. Pass a non-nil logger to
// override the default (Orange component logger with component="orange/pick").
// Call before Envoy initializes clusters; init() registers with nil as default.
func Register(logger *slog.Logger) {
	up.RegisterCluster(ClusterName, &factory{logger: logger})
}

type factory struct{ logger *slog.Logger }

func (f factory) Create(_ []byte) (up.ClusterConfigFactory, error) {
	return &cfgFactory{logger: f.logger}, nil
}

type cfgFactory struct{ logger *slog.Logger }

func (f cfgFactory) NewCluster(h up.ClusterHandle) up.Cluster {
	return &cluster{handle: h, logger: f.logger}
}
func (cfgFactory) Close() {}

// provBindingKey is the composite key for the hosts map. It combines the
// provider backend name with the binding name so multi-binding providers get
// one DNS-refresh entry per binding. Binding is "default" for providers that
// use only a top-level endpoint and for all MCP servers.
type provBindingKey struct {
	provider string
	binding  string
}

func (k provBindingKey) String() string {
	if k.binding == "" || k.binding == "default" {
		return k.provider
	}
	return k.provider + ":" + k.binding
}

// hostEntry holds all resolved IPs for one provider-binding pair plus their
// HostPtrs. TTL and refresh scheduling are owned by DNSDiscovery, not here.
//
// rr is an atomic round-robin counter; lookupHost increments it on every call
// and selects ptrs[rr % len(ptrs)], distributing requests across all IPs.
// hostEntry is always heap-allocated (stored as *hostEntry) so rr can be
// mutated without copying the struct (atomic.Uint64 is noCopy).
type hostEntry struct {
	addrs []string
	ptrs  []up.HostPtr
	rr    atomic.Uint64
}

// Discovery emits periodic snapshots of resolved upstream addresses.
type Discovery interface {
	Run(ctx context.Context, updates chan<- *Snapshot)
}

// Snapshot is a point-in-time view of all resolved upstream addresses.
type Snapshot struct {
	Entries map[provBindingKey]Entry
}

// Entry holds the resolved hostname and IP addresses for one provider-binding
// pair. Empty Addresses signals a resolution failure; the reconciler preserves
// any existing healthy hosts rather than removing them.
type Entry struct {
	Hostname  string
	Addresses []string
}

type cluster struct {
	handle        up.ClusterHandle
	stopConfig    func()
	stopFileWatch func()
	stopRefresh   context.CancelFunc
	logger        *slog.Logger

	// disc is the Discovery instance used for both the synchronous Init resolve
	// and the background Run loop. Tests pre-populate this field with a
	// DNSDiscovery carrying a stub resolveFunc; production code creates it lazily
	// in Init when the field is nil.
	disc *DNSDiscovery

	// hosts is the source of truth ChooseHost reads from. Published atomically
	// after each reconcileSnapshot call to keep ChooseHost lock-free. Values are
	// pointers so the per-entry rr counter survives map republishes. The map is
	// keyed by (provider, binding) so multi-binding providers have independent
	// round-robin counters and HostPtr sets.
	hosts atomic.Pointer[map[provBindingKey]*hostEntry]
}

func (c *cluster) Init(h up.ClusterHandle) {
	c.handle = h
	if c.logger == nil {
		c.logger = observability.Logger("orange/pick")
	}
	config.EnsureLogger()
	if c.disc == nil {
		c.disc = &DNSDiscovery{logger: c.logger}
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultResolveTimeout)
	defer cancel()
	// Init is called on the main thread, so buildSnapshot (DNS) + reconcileSnapshot
	// (host mutations) can run in sequence here without scheduling.
	c.reconcileSnapshot(h, c.disc.buildSnapshot(ctx))
	h.PreInitComplete()
}

func (c *cluster) ServerInitialized(h up.ClusterHandle) {
	c.stopConfig = config.Start(context.Background())

	// Enable file watching for the orange config file to trigger immediate refreshes.
	c.stopFileWatch = config.EnableFileWatch(os.Getenv(config.EnvVar))

	ctx, cancel := context.WithCancel(context.Background())
	c.stopRefresh = cancel

	updates := make(chan *Snapshot)
	go c.disc.Run(ctx, updates)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case snap, ok := <-updates:
				if !ok {
					return
				}
				done := make(chan struct{})
				c.handle.Schedule(func() {
					c.reconcileSnapshot(h, snap)
					close(done)
				})
				select {
				case <-ctx.Done():
					return
				case <-done:
				}
			}
		}
	}()
}

// lookupHostN maps a match.Decision and an attempt number to a HostPtr.
// attempt == 0 selects the primary (d.ProviderBackend / d.Binding).
// attempt > 0 selects d.Fallbacks[attempt-1], clamped to the last entry so
// that extra Envoy retries beyond the chain length keep hitting the last
// fallback rather than returning nil and causing Envoy to reuse a stale host.
// For chain targets the binding is always "default".
func (c *cluster) lookupHostN(d match.Decision, attempt uint32) up.HostResult {
	if d.Err != "" {
		return up.HostResult{ErrDetail: d.Err}
	}
	var providerBackend, binding string
	if attempt == 0 {
		providerBackend = d.ProviderBackend
		binding = d.Binding
	} else {
		if len(d.Fallbacks) == 0 {
			return up.HostResult{ErrDetail: "orange.fallback_exhausted"}
		}
		idx := int(attempt) - 1
		if idx >= len(d.Fallbacks) {
			idx = len(d.Fallbacks) - 1
		}
		fb := d.Fallbacks[idx]
		providerBackend = fb.ProviderBackend
		binding = "default"
	}
	if binding == "" {
		binding = "default"
	}
	key := provBindingKey{providerBackend, binding}
	if m := c.hosts.Load(); m != nil {
		if r := (*m)[key]; r != nil && len(r.ptrs) > 0 {
			idx := r.rr.Add(1) % uint64(len(r.ptrs))
			return up.HostResult{Host: r.ptrs[idx]}
		}
	}
	return up.HostResult{ErrDetail: "orange.unknown_upstream"}
}

// lookupHost is a convenience wrapper for attempt 0 (primary host).
// Used by the AsyncHostSelector callback which always starts at attempt 0.
func (c *cluster) lookupHost(d match.Decision) up.HostResult {
	return c.lookupHostN(d, 0)
}

// reconcileSnapshot reconciles the cluster host set from a pre-resolved snapshot.
// Must be called on the cluster main thread (AddHosts/RemoveHosts/UpdateHostHealth
// are main-thread-only per the ClusterHandle contract).
//
// Each provider may resolve to multiple A-record IPs; all are registered so the
// round-robin in lookupHost can distribute across them. Reconciliation rules:
//   - empty Addresses (resolve failed) → keep existing entry
//   - IP set unchanged                  → keep existing HostPtrs and rr counter
//   - IP added                          → AddHosts for new addr, retain ptrs for unchanged addrs
//   - IP removed                        → RemoveHosts for gone addr
//   - new provider                      → AddHosts for all resolved addrs
//   - provider deleted from snapshot    → RemoveHosts for all its ptrs
func (c *cluster) reconcileSnapshot(h up.ClusterHandle, snap *Snapshot) {
	current := c.hosts.Load()
	out := make(map[provBindingKey]*hostEntry, len(snap.Entries))

	for name, e := range snap.Entries {
		if len(e.Addresses) == 0 {
			c.logger.Warn("skipping upstream: resolve failed", "upstream", name.String())
			// Preserve the existing entry so a DNS hiccup doesn't pull a healthy host.
			if current != nil {
				if old, ok := (*current)[name]; ok {
					entry := &hostEntry{addrs: old.addrs, ptrs: old.ptrs}
					entry.rr.Store(old.rr.Load())
					out[name] = entry
				}
			}
			continue
		}

		// Build a map of existing addr -> HostPtr for this (provider, binding).
		existing := map[string]up.HostPtr{}
		var oldRR uint64
		var old *hostEntry
		if current != nil {
			if o, ok := (*current)[name]; ok {
				old = o
				for i, addr := range old.addrs {
					existing[addr] = old.ptrs[i]
				}
				oldRR = old.rr.Load()
			}
		}

		// Remove IPs that are no longer in the snapshot.
		newAddrSet := make(map[string]struct{}, len(e.Addresses))
		for _, addr := range e.Addresses {
			newAddrSet[addr] = struct{}{}
		}

		// IP set unchanged (order-independent) — keep existing HostPtrs and rr counter.
		if old != nil && len(existing) == len(newAddrSet) {
			unchanged := true
			for addr := range existing {
				if _, ok := newAddrSet[addr]; !ok {
					unchanged = false
					break
				}
			}
			if unchanged {
				entry := &hostEntry{addrs: old.addrs, ptrs: old.ptrs}
				entry.rr.Store(oldRR)
				out[name] = entry
				continue
			}
		}

		for addr, ptr := range existing {
			if _, keep := newAddrSet[addr]; !keep {
				h.RemoveHosts([]up.HostPtr{ptr})
				c.logger.Debug("upstream addr removed", "upstream", name.String(), "addr", addr)
			}
		}

		// Build the new ptrs slice, registering any IPs not yet in the cluster.
		ptrs := make([]up.HostPtr, 0, len(e.Addresses))
		for _, addr := range e.Addresses {
			if ptr, ok := existing[addr]; ok {
				ptrs = append(ptrs, ptr)
			} else {
				added := h.AddHosts([]up.HostSpec{{
					Address:  addr,
					Hostname: e.Hostname,
				}})
				if len(added) == 0 {
					c.logger.Warn("skipping addr: AddHosts returned no ptrs", "upstream", name.String(), "addr", addr)
					continue
				}
				h.UpdateHostHealth(added[0], up.HostHealthy)
				ptrs = append(ptrs, added[0])
				c.logger.Debug("upstream addr added", "upstream", name.String(), "addr", addr)
			}
		}

		if len(ptrs) == 0 {
			c.logger.Warn("skipping upstream: no valid addrs after reconcile", "upstream", name.String())
			continue
		}

		entry := &hostEntry{addrs: e.Addresses, ptrs: ptrs}
		entry.rr.Store(oldRR)
		out[name] = entry
	}

	// Remove providers/bindings absent from the snapshot (deleted from config).
	if current != nil {
		for name, old := range *current {
			if _, kept := out[name]; !kept {
				h.RemoveHosts(old.ptrs)
				c.logger.Info("upstream removed from config", "upstream", name.String(), "addrs", old.addrs)
			}
		}
	}

	c.hosts.Store(&out)
}

func (c *cluster) NewClusterLB() up.ClusterLB {
	log := c.logger
	sel := up.NewAsyncHostSelector(
		c.handle,
		match.DecisionKey,
		c.lookupHost,
		up.SelectorObserver{
			OnSelected:       func(host up.HostPtr) { log.Debug("host selected", "host", host) },
			OnFailed:         func(errDetail string) { log.Warn("host selection failed", "err", errDetail) },
			OnCancelled:      func() { log.Debug("host selection cancelled") },
			OnMissingPromise: func() { log.Warn("no match promise in stream bag") },
		},
	)
	return &lb{
		sel:         sel,
		lookupHostN: c.lookupHostN,
		log:         log,
	}
}

func (c *cluster) DrainStarted(_ up.ClusterHandle) {} // no in-flight requests to drain
func (c *cluster) Shutdown(_ up.ClusterHandle, done func()) {
	if c.stopRefresh != nil {
		c.stopRefresh()
	}
	if c.stopFileWatch != nil {
		c.stopFileWatch()
	}
	if c.stopConfig != nil {
		c.stopConfig()
	}
	done()
}
func (c *cluster) Close() {} // required by up.Cluster; Shutdown handles teardown

type lb struct {
	up.EmptyClusterLB
	sel         *up.AsyncHostSelector[match.Decision]
	lookupHostN func(match.Decision, uint32) up.HostResult
	log         *slog.Logger
}

func (l *lb) ChooseHost(h up.ClusterLBHandle, ctx up.ClusterLBContext) (up.HostPtr, *up.ClusterLBCompletion) {
	if ctx != nil {
		if provider, ok := ctx.GetFilterState(match.StateUpstream); ok && provider != "" {
			// Read the HTTP-attempt counter written by adapt (1-based; absent on
			// the first attempt). GetHostSelectionRetryCount() cannot be used here
			// because it counts within-attempt host-selection retries and resets to
			// 0 at the start of every new HTTP retry — it does not reflect which
			// HTTP attempt number we are on.
			var attempt uint32
			if v, ok := ctx.GetFilterState(match.StateAttempt); ok && v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					attempt = uint32(n)
				}
			}
			binding, _ := ctx.GetFilterState(match.StateBinding)

			// Build a Decision with fallbacks decoded from filter state on retries.
			d := match.Decision{ProviderBackend: provider, Binding: binding}
			if attempt > 0 {
				if fbJSON, ok := ctx.GetFilterState(match.StateFallbacks); ok && fbJSON != "" {
					var fallbacks []match.Target
					if err := json.Unmarshal([]byte(fbJSON), &fallbacks); err == nil {
						d.Fallbacks = fallbacks
					}
				}
			}

			res := l.lookupHostN(d, attempt)
			if res.ErrDetail != "" {
				if l.log != nil {
					l.log.Warn("host selection failed", "err", res.ErrDetail, "provider", provider, "binding", binding, "attempt", attempt)
				}
				completion := ctx.NewCompletion()
				if completion != nil {
					completion.Complete(nil, res.ErrDetail)
					return nil, completion
				}
				return nil, nil
			}
			if l.log != nil {
				l.log.Debug("host selected", "host", res.Host, "provider", provider, "binding", binding, "attempt", attempt)
			}
			return res.Host, nil
		}
	}
	return l.sel.ChooseHost(h, ctx)
}

func (l *lb) CancelHostSelection(completion *up.ClusterLBCompletion) {
	l.sel.Cancel(completion)
}

// DNSDiscovery implements Discovery using DNS. It owns all TTL bookkeeping,
// refresh scheduling, and retry logic. The cluster only sees Snapshots.
type DNSDiscovery struct {
	logger *slog.Logger

	// resolveFunc is called for each provider endpoint. Nil means use the
	// package-level resolveUpstream (real DNS). Set in tests to avoid network calls.
	resolveFunc func(ctx context.Context, endpoint string) (addrs []string, ttl time.Duration, err error)

	// nextRefresh tracks when each key's DNS TTL expires. Updated by buildSnapshot.
	nextRefresh map[provBindingKey]time.Time
}

func (d *DNSDiscovery) resolve(ctx context.Context, endpoint string) ([]string, time.Duration, error) {
	if d.resolveFunc != nil {
		return d.resolveFunc(ctx, endpoint)
	}
	return resolveUpstream(ctx, endpoint)
}

// buildSnapshot resolves every provider binding and MCP server in the current
// config and returns a complete Snapshot. Failed resolutions produce entries
// with empty Addresses so the reconciler can preserve existing healthy hosts.
// The nextRefresh map is updated as a side-effect so Run knows when to wake.
func (d *DNSDiscovery) buildSnapshot(ctx context.Context) *Snapshot {
	if d.nextRefresh == nil {
		d.nextRefresh = make(map[provBindingKey]time.Time)
	}
	cfg := config.Get()
	snap := &Snapshot{Entries: make(map[provBindingKey]Entry)}

	for name, p := range cfg.Providers {
		for _, b := range p.AllBindings() {
			key := provBindingKey{name, b.Name}
			hostname := hostnameOf(b.Endpoint)
			addrs, ttl, err := d.resolve(ctx, b.Endpoint)
			if err != nil {
				if d.logger != nil {
					d.logger.Warn("resolve failed", "upstream", key.String(), "err", err)
				}
				d.nextRefresh[key] = time.Now().Add(retryDelay())
				snap.Entries[key] = Entry{Hostname: hostname}
			} else {
				ttl = max(ttl, minTTLFloor)
				d.nextRefresh[key] = time.Now().Add(ttl + jitter(minTTLFloor))
				snap.Entries[key] = Entry{Hostname: hostname, Addresses: addrs}
			}
		}
	}

	if cfg.MCP != nil {
		return snap
	}

	for name, s := range cfg.MCP.Servers {
		key := provBindingKey{name, "default"}
		hostname := s.Host()
		addrs, ttl, err := d.resolve(ctx, s.Endpoint)
		if err != nil {
			if d.logger != nil {
				d.logger.Warn("resolve failed", "upstream", key.String(), "err", err)
			}
			d.nextRefresh[key] = time.Now().Add(retryDelay())
			snap.Entries[key] = Entry{Hostname: hostname}
		} else {
			ttl = max(ttl, minTTLFloor)
			d.nextRefresh[key] = time.Now().Add(ttl + jitter(minTTLFloor))
			snap.Entries[key] = Entry{Hostname: hostname, Addresses: addrs}
		}
	}

	return snap
}

// earliestNext returns the soonest scheduled refresh across all known keys,
// or now+defaultDNSRefreshInterval when no keys have been resolved yet.
func (d *DNSDiscovery) earliestNext() time.Time {
	if len(d.nextRefresh) == 0 {
		return time.Now().Add(defaultDNSRefreshInterval)
	}
	var earliest time.Time
	for _, t := range d.nextRefresh {
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run resolves upstreams periodically and emits complete Snapshots on updates.
// It wakes at the earliest TTL expiry across all registered keys rather than
// a fixed interval. The channel send blocks until the consumer is ready,
// naturally serialising DNS resolves with Envoy main-thread reconciliation.
func (d *DNSDiscovery) Run(ctx context.Context, updates chan<- *Snapshot) {
	if d.nextRefresh == nil {
		d.nextRefresh = make(map[provBindingKey]time.Time)
	}
	for {
		delay := max(time.Until(d.earliestNext()), minTTLFloor)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}

		rctx, cancel := context.WithTimeout(ctx, defaultResolveTimeout)
		snap := d.buildSnapshot(rctx)
		cancel()

		select {
		case updates <- snap:
		case <-ctx.Done():
			return
		}
	}
}

// hostnameOf parses an endpoint URL and returns its hostname.
func hostnameOf(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// resolveUpstream parses an endpoint URL, resolves the hostname to IP addresses
// via DNS, and returns all "ip:port" strings plus the minimum TTL observed in
// the A-record answer. Multiple IPs are returned when DNS returns multiple A
// records; all are registered as hosts so lookupHost can round-robin among them.
func resolveUpstream(ctx context.Context, endpoint string) (addrs []string, ttl time.Duration, err error) {
	host, port, err := splitEndpoint(endpoint)
	if err != nil {
		return nil, 0, err
	}
	ipAddrs, ttl, err := lookupWithTTL(ctx, host)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve %q: %w", host, err)
	}
	result := pickAddrs(ipAddrs, port)
	if len(result) == 0 {
		return nil, 0, fmt.Errorf("resolve %q: no addresses", host)
	}
	return result, ttl, nil
}

// pickAddrs prefers IPv4 addresses: if any IPv4 address is present only those
// are returned. Otherwise all IPv6 addresses are returned as fallback. The
// result is sorted by byte value so DNS round-robin rotation yields a stable
// slice regardless of the order DNS returns addresses.
func pickAddrs(addrs []net.IPAddr, port string) []string {
	var v4, v6 []net.IPAddr
	for _, a := range addrs {
		if a.IP.To4() != nil {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	selected := v4
	if len(selected) == 0 {
		selected = v6
	}
	sort.Slice(selected, func(i, j int) bool {
		return bytes.Compare(selected[i].IP, selected[j].IP) < 0
	})
	out := make([]string, len(selected))
	for i, a := range selected {
		out[i] = net.JoinHostPort(a.IP.String(), port)
	}
	return out
}

// fallbackDNSConfig is used when /etc/resolv.conf is missing or unreadable
// (e.g. scratch/distroless containers). It points at well-known public resolvers
// that are reachable from any internet-connected host.
var fallbackDNSConfig = &dns.ClientConfig{
	Servers: []string{"8.8.8.8", "1.1.1.1"},
	Port:    "53",
}

// dnsConfig is resolved once at startup from /etc/resolv.conf, falling back
// to fallbackDNSConfig when the file is missing or unreadable. Immutable after
// process start.
var dnsConfig = func() *dns.ClientConfig {
	conf, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	if err != nil {
		slog.Warn("orange/pick: /etc/resolv.conf unavailable, falling back to public resolvers", "err", err)
		return fallbackDNSConfig
	}
	return conf
}()

// lookupWithTTL queries A records for host using the system resolver config
// and returns the resolved addresses together with the minimum TTL from the
// answer section. The TTL is used to schedule the next re-resolve.
//
// When host is already a literal IP address (IPv4 or IPv6), the DNS query is
// skipped and the address is returned directly with a 24-hour synthetic TTL.
// This allows endpoint URLs to carry IP addresses directly (e.g. for test stubs
// or on-premise deployments that don't use DNS).
func lookupWithTTL(ctx context.Context, host string) ([]net.IPAddr, time.Duration, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IPAddr{{IP: ip}}, 24 * time.Hour, nil
	}
	conf := dnsConfig

	c := &dns.Client{}
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(host), dns.TypeA)
	m.RecursionDesired = true

	var lastErr error
	for _, server := range conf.Servers {
		r, _, err := c.ExchangeContext(ctx, m, net.JoinHostPort(server, conf.Port))
		if err != nil {
			lastErr = err
			continue
		}
		if r.Rcode != dns.RcodeSuccess {
			lastErr = fmt.Errorf("DNS rcode %s for %q", dns.RcodeToString[r.Rcode], host)
			continue
		}

		var addrs []net.IPAddr
		minTTL := ^uint32(0)
		for _, rr := range r.Answer {
			if a, ok := rr.(*dns.A); ok {
				addrs = append(addrs, net.IPAddr{IP: a.A})
				if a.Hdr.Ttl < minTTL {
					minTTL = a.Hdr.Ttl
				}
			}
		}
		if len(addrs) == 0 {
			lastErr = fmt.Errorf("no A records for %q", host)
			continue
		}
		return addrs, time.Duration(minTTL) * time.Second, nil
	}

	if lastErr != nil {
		return nil, 0, lastErr
	}
	return nil, 0, fmt.Errorf("all DNS servers exhausted for %q", host)
}

// splitEndpoint extracts host + port from an endpoint URL or "host:port" form.
// "https://api.openai.com" → ("api.openai.com", "443"). Exposed for tests.
func splitEndpoint(endpoint string) (string, string, error) {
	if endpoint == "" {
		return "", "", fmt.Errorf("empty endpoint")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", fmt.Errorf("parse %q: %w", endpoint, err)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		// Accepted no-port forms: "https://host" (443), "http://host" (80).
		// A bare "host" (no scheme, no port) also reaches here with scheme ""
		// and defaults to 443; "host:port" without a scheme parses as Opaque
		// and will fail the host == "" check below.
		switch u.Scheme {
		case "https", "":
			port = "443"
		case "http":
			port = "80"
		default:
			return "", "", fmt.Errorf("endpoint %q: unsupported scheme %q", endpoint, u.Scheme)
		}
	}
	if host == "" {
		return "", "", fmt.Errorf("endpoint %q: missing host", endpoint)
	}
	return host, port, nil
}

// retryDelay returns a jittered retry interval for a failed DNS resolve:
// minTTLFloor + rand[0, minTTLFloor). Spreading retries over a [10s, 20s)
// window prevents instances that synchronised on a failure from storming the
// in-cluster DNS server when it recovers.
func retryDelay() time.Duration {
	// #nosec G404 — jitter is not security-sensitive
	return minTTLFloor + time.Duration(rand.Int63n(int64(minTTLFloor)))
}

// jitter returns a random duration in [0, spread) to add to a TTL-based
// nextRefresh, desynchronising instances that resolved the same hostname at
// the same time.
func jitter(spread time.Duration) time.Duration {
	// #nosec G404 — jitter is not security-sensitive
	return time.Duration(rand.Int63n(int64(spread)))
}
