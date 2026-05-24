package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/dgraph-io/ristretto/z"
)

// Metrics holds monitoring data for the edge proxy
type metrics struct {
	// Cache metrics
	cacheHits    atomic.Uint64
	cacheMisses  atomic.Uint64
	cacheSize    atomic.Uint64
	cacheEvicted atomic.Uint64

	// Prefetch metrics
	prefetchScheduled atomic.Uint64
	prefetchSuccess   atomic.Uint64
	prefetchFailures  atomic.Uint64
	prefetchActive    atomic.Int64

	// Origin request metrics (APL only)
	originRequests   atomic.Uint64
	originFailures   atomic.Uint64
	originTimeouts   atomic.Uint64
	originDNSErrors  atomic.Uint64
	originConnErrors atomic.Uint64

	// APL request metrics
	aplRequests atomic.Uint64
	aplFailures atomic.Uint64

	// Performance metrics
	avgResponseTime atomic.Uint64 // in milliseconds
	requestCount    atomic.Uint64

	mu        sync.RWMutex
	startTime time.Time
}

// MetricsSnapshot represents a point-in-time view of metrics
type MetricsSnapshot struct {
	Timestamp time.Time `json:"timestamp"`
	Uptime    string    `json:"uptime"`

	// Cache metrics
	CacheHits     uint64  `json:"cache_hits"`
	CacheMisses   uint64  `json:"cache_misses"`
	CacheHitRatio float64 `json:"cache_hit_ratio"`
	CacheSize     uint64  `json:"cache_size"`
	CacheEvicted  uint64  `json:"cache_evicted"`

	// Prefetch metrics
	PrefetchScheduled   uint64  `json:"prefetch_scheduled"`
	PrefetchSuccess     uint64  `json:"prefetch_success"`
	PrefetchFailures    uint64  `json:"prefetch_failures"`
	PrefetchSuccessRate float64 `json:"prefetch_success_rate"`
	PrefetchActive      int64   `json:"prefetch_active"`

	// Origin request metrics (APL only)
	OriginRequests    uint64  `json:"origin_requests"`
	OriginFailures    uint64  `json:"origin_failures"`
	OriginFailureRate float64 `json:"origin_failure_rate"`
	OriginTimeouts    uint64  `json:"origin_timeouts"`
	OriginDNSErrors   uint64  `json:"origin_dns_errors"`
	OriginConnErrors  uint64  `json:"origin_conn_errors"`

	// APL-specific metrics
	APLRequests    uint64  `json:"apl_requests"`
	APLFailures    uint64  `json:"apl_failures"`
	APLFailureRate float64 `json:"apl_failure_rate"`

	// Performance metrics
	AvgResponseTime uint64 `json:"avg_response_time_ms"`
	RequestCount    uint64 `json:"request_count"`
}

func newMetrics() *metrics {
	return &metrics{
		startTime: time.Now(),
	}
}

// resetMetrics resets all metric counters to zero
func (m *metrics) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Reset cache metrics
	m.cacheHits.Store(0)
	m.cacheMisses.Store(0)
	m.cacheSize.Store(0)
	m.cacheEvicted.Store(0)

	// Reset prefetch metrics
	m.prefetchScheduled.Store(0)
	m.prefetchSuccess.Store(0)
	m.prefetchFailures.Store(0)
	// Note: prefetchActive is not reset as it represents current state

	// Reset origin request metrics
	m.originRequests.Store(0)
	m.originFailures.Store(0)
	m.originTimeouts.Store(0)
	m.originDNSErrors.Store(0)
	m.originConnErrors.Store(0)

	// Reset APL request metrics
	m.aplRequests.Store(0)
	m.aplFailures.Store(0)

	// Reset performance metrics
	m.avgResponseTime.Store(0)
	m.requestCount.Store(0)

	// Reset start time to current time
	m.startTime = time.Now()

	log.Println("Metrics have been reset")
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	proxy, err := newEdgeProxy(cfg)
	if err != nil {
		log.Fatalf("proxy init error: %v", err)
	}

	// Create a mux to handle both proxy and metrics endpoints
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", proxy.ServeMetrics)
	mux.HandleFunc("/dashboard", proxy.ServeDashboard)
	mux.HandleFunc("/cache", proxy.ServeCachePage)
	mux.HandleFunc("/cache/clear", proxy.ServeCacheClear)
	mux.HandleFunc("/cache/toggle", proxy.ServeCacheToggle)
	mux.HandleFunc("/cache/drop", proxy.ServeCacheDrop)
	mux.HandleFunc("/cache/config", proxy.ServeCacheConfig)
	mux.HandleFunc("/cache/status", proxy.ServeCacheStatus)
	mux.HandleFunc("/cache/prefetch", proxy.ServeCachePrefetch)
	mux.HandleFunc("/ssl", proxy.ServeSSLUploadPage)
	mux.HandleFunc("/ssl/upload", proxy.HandleSSLUpload)
	mux.HandleFunc("/reset-metrics", proxy.ServeMetricsReset)
	mux.HandleFunc("/", proxy.ServeHTTP)

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.UpstreamTimeout + 5*time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    http.DefaultMaxHeaderBytes,
	}

	// Start metrics logging
	proxy.startMetricsLogging()

	// Start daily metrics reset if enabled
	if cfg.MetricsResetDaily {
		proxy.startDailyMetricsReset(cfg.MetricsResetTime)
	}

	log.Printf("edge proxy listening on %s (apl=%s)", cfg.ListenAddr, cfg.APLOrigin)
	log.Printf("metrics endpoint available at %s/metrics", cfg.ListenAddr)
	log.Printf("dashboard available at %s/dashboard", cfg.ListenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

type config struct {
	ListenAddr        string
	APLOrigin         string
	UpstreamTimeout   time.Duration
	UpstreamUserAgent string
	CacheEntries      int
	PlaylistTTL       time.Duration
	SegmentTTL        time.Duration
	PrefetchWorkers   int
	PrefetchBatch     int
	PrefetchEnabled   bool
	MetricsResetDaily bool
	MetricsResetTime  string // Format: "HH:MM" (e.g., "00:00" for midnight)
}

type upstreamTarget struct {
	base          *url.URL
	hostOverride  string
	originHeader  string
	refererHeader string
	skipTLSVerify bool
}

type originConfig struct {
	Origin  string
	Host    string
	Referer string
}

type cacheKeyInfo struct {
	path string
	hash cacheHashKey
}

type cacheHashKey struct {
	primary  uint64
	conflict uint64
}

func loadConfig() (*config, error) {
	getenv := func(key, def string) string {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			return val
		}
		return def
	}

	aplOrigin := strings.TrimSpace(os.Getenv("APL_ORIGIN"))
	if aplOrigin == "" {
		return nil, errors.New("APL_ORIGIN is required")
	}

	timeout := 5 * time.Second
	if raw := strings.TrimSpace(os.Getenv("UPSTREAM_TIMEOUT")); raw != "" {
		dur, err := time.ParseDuration(raw)
		if err != nil || dur <= 0 {
			return nil, fmt.Errorf("invalid UPSTREAM_TIMEOUT: %w", err)
		}
		timeout = dur
	}

	cacheEntries, err := parseIntEnv("CACHE_SIZE", 512)
	if err != nil {
		return nil, err
	}

	playlistTTL, err := parseDurationEnv("CACHE_TTL_PLAYLIST", 2*time.Second)
	if err != nil {
		return nil, err
	}

	segmentTTL, err := parseDurationEnv("CACHE_TTL_SEGMENT", 30*time.Second)
	if err != nil {
		return nil, err
	}

	prefetchWorkers, err := parseIntEnv("PREFETCH_WORKERS", 4)
	if err != nil {
		return nil, err
	}

	prefetchBatch, err := parseIntEnv("PREFETCH_BATCH", 5)
	if err != nil {
		return nil, err
	}

	prefetchEnabled := true
	if raw := strings.TrimSpace(os.Getenv("ENABLE_PREFETCH")); raw != "" {
		val, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid ENABLE_PREFETCH: %w", err)
		}
		prefetchEnabled = val
	}

	// Parse metrics reset configuration
	metricsResetDaily := true
	if raw := strings.TrimSpace(os.Getenv("METRICS_RESET_DAILY")); raw != "" {
		val, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid METRICS_RESET_DAILY: %w", err)
		}
		metricsResetDaily = val
	}

	metricsResetTime := getenv("METRICS_RESET_TIME", "00:00")
	// Validate time format
	if _, err := time.Parse("15:04", metricsResetTime); err != nil {
		return nil, fmt.Errorf("invalid METRICS_RESET_TIME format (use HH:MM): %w", err)
	}

	return &config{
		ListenAddr:        getenv("LISTEN_ADDR", ":9000"),
		APLOrigin:         aplOrigin,
		UpstreamTimeout:   timeout,
		UpstreamUserAgent: getenv("EDGE_USER_AGENT", defaultUserAgent),
		CacheEntries:      cacheEntries,
		PlaylistTTL:       playlistTTL,
		SegmentTTL:        segmentTTL,
		PrefetchWorkers:   prefetchWorkers,
		PrefetchBatch:     prefetchBatch,
		PrefetchEnabled:   prefetchEnabled,
		MetricsResetDaily: metricsResetDaily,
		MetricsResetTime:  metricsResetTime,
	}, nil
}

func parseIntEnv(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	val, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	if val < 0 {
		return 0, fmt.Errorf("invalid %s: must be >= 0", key)
	}
	return val, nil
}

func parseDurationEnv(key string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	dur, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	if dur < 0 {
		return 0, fmt.Errorf("invalid %s: must be >= 0", key)
	}
	return dur, nil
}

func parseBoolEnv(key string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	val, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("invalid %s: %w", key, err)
	}
	return val, nil
}

const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36"

type edgeProxy struct {
	clientStrict   *http.Client
	aplTarget      *upstreamTarget
	userAgent      string
	upstreamDelay  time.Duration
	cache          *ristretto.Cache
	cacheOn        atomic.Bool
	cacheKeys      *sync.Map
	cacheHashIndex *sync.Map
	playlistTTL    atomic.Int64
	segmentTTL     atomic.Int64
	prefetchBatch  int
	prefetchSem    chan struct{}
	prefetchOn     bool
	metrics        *metrics
	revalidateMap  sync.Map
}

func newEdgeProxy(cfg *config) (*edgeProxy, error) {
	buildURL := func(raw string) (*url.URL, error) {
		if raw == "" {
			return nil, errors.New("empty origin")
		}
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, err
		}
		if u.Scheme == "" {
			u.Scheme = "https"
		}
		u.Path = ""
		u.RawQuery = ""
		u.Fragment = ""
		return u, nil
	}

	buildTarget := func(u *url.URL, hostOverride, originHeader, refererHeader string, skipTLS bool) *upstreamTarget {
		return &upstreamTarget{
			base:          u,
			hostOverride:  strings.TrimSpace(hostOverride),
			originHeader:  strings.TrimSpace(originHeader),
			refererHeader: strings.TrimSpace(refererHeader),
			skipTLSVerify: skipTLS,
		}
	}

	buildTransport := func(skipVerify bool) *http.Transport {
		return &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          256,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: skipVerify, //nolint:gosec
			},
		}
	}

	metrics := newMetrics()
	cacheKeys := &sync.Map{}
	cacheHashIndex := &sync.Map{}
	decCacheSize := func(n uint64) {
		if n == 0 {
			return
		}
		metrics.cacheSize.Add(^uint64(n - 1)) // subtract n
	}

	// Build APL target
	aplURL, err := buildURL(cfg.APLOrigin)
	if err != nil {
		return nil, fmt.Errorf("invalid APL origin: %w", err)
	}
	aplTarget := buildTarget(aplURL, "", "", "", false)

	// Create HTTP client
	clientStrict := &http.Client{
		Transport: buildTransport(false),
		Timeout:   cfg.UpstreamTimeout,
	}

	var cache *ristretto.Cache
	if cfg.CacheEntries > 0 {
		cacheConfig := &ristretto.Config{
			NumCounters: int64(maxInt(cfg.CacheEntries*10, 10)),
			MaxCost:     int64(maxInt(cfg.CacheEntries, 1)),
			BufferItems: 64,
			Cost: func(value interface{}) int64 {
				return 1
			},
			KeyToHash: func(key interface{}) (uint64, uint64) {
				h1, h2 := z.KeyToHash(key)
				if keyStr, ok := key.(string); ok {
					cacheHashIndex.Store(cacheHashKey{primary: h1, conflict: h2}, keyStr)
				}
				return h1, h2
			},
			OnEvict: func(item *ristretto.Item) {
				metrics.cacheEvicted.Add(1)
				hashKey := cacheHashKey{primary: item.Key, conflict: item.Conflict}
				if key, ok := cacheHashIndex.LoadAndDelete(hashKey); ok {
					if keyStr, ok := key.(string); ok {
						if _, existed := cacheKeys.LoadAndDelete(keyStr); existed {
							decCacheSize(1)
						}
					}
				}
			},
		}
		var err error
		cache, err = ristretto.NewCache(cacheConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to init cache: %w", err)
		}
	}

	var prefetchSem chan struct{}
	if cfg.PrefetchEnabled && cfg.PrefetchWorkers > 0 {
		prefetchSem = make(chan struct{}, cfg.PrefetchWorkers)
	}

	proxy := &edgeProxy{
		clientStrict:   clientStrict,
		aplTarget:      aplTarget,
		userAgent:      cfg.UpstreamUserAgent,
		upstreamDelay:  cfg.UpstreamTimeout,
		cache:          cache,
		cacheKeys:      cacheKeys,
		cacheHashIndex: cacheHashIndex,
		prefetchBatch:  cfg.PrefetchBatch,
		prefetchSem:    prefetchSem,
		prefetchOn:     cfg.PrefetchEnabled && cfg.PrefetchWorkers > 0 && cfg.PrefetchBatch > 0,
		metrics:        metrics,
	}

	proxy.playlistTTL.Store(cfg.PlaylistTTL.Nanoseconds())
	proxy.segmentTTL.Store(cfg.SegmentTTL.Nanoseconds())
	proxy.segmentTTL.Store(cfg.SegmentTTL.Nanoseconds())
	proxy.cacheOn.Store(cache != nil)

	return proxy, nil
}

func (p *edgeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	p.metrics.requestCount.Add(1)

	ctx, cancel := context.WithTimeout(r.Context(), p.upstreamDelay)
	defer cancel()

	target, upstreamPath, err := p.selectUpstream(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if target == nil || target.base == nil {
		http.Error(w, "upstream target not configured", http.StatusBadGateway)
		return
	}

	// Build URL for origin request (upstreamPath has timestamp stripped for APL segments)
	reqURL := buildRequestURL(target.base, upstreamPath, r.URL.RawQuery)
	cacheKey := cacheKeyForURL(&reqURL)

	if entry, prefetched, ok, stale := p.getFromCache(cacheKey); ok {
		p.metrics.cacheHits.Add(1)
		cacheMark := "HIT"
		if stale {
			cacheMark = "STALE"
			p.scheduleRevalidate(target, &reqURL)
		}
		if target == p.aplTarget && isSegmentPath(upstreamPath) {
			log.Printf("APL: cache %s for segment %s (prefetched=%v)", cacheMark, upstreamPath, prefetched)
		}
		p.writeResponse(w, entry.header, entry.status, entry.body, cacheMark, boolToPrefetch(prefetched))
		p.updateResponseTime(start)
		return
	}

	p.metrics.cacheMisses.Add(1)
	if target == p.aplTarget && isSegmentPath(upstreamPath) {
		log.Printf("APL: cache MISS for segment %s", upstreamPath)
	}
	resp, err := p.fetchAndStore(ctx, &reqURL, target, false)
	if err != nil {
		p.recordOriginFailure(target, err)
		log.Printf("upstream request to %s failed: %v", reqURL.Redacted(), err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}

	prefetchCount := 0
	if shouldCache(resp.status) && isPlaylistPath(upstreamPath) {
		// For APL, immediately cache the first upcoming segment synchronously before async prefetch
		if target == p.aplTarget {
			prefetchCount = p.scheduleAPLPrefetchWithImmediate(target, upstreamPath, resp.body)
		} else {
			prefetchCount = p.schedulePrefetch(target, upstreamPath, resp.body)
		}
		if target == p.aplTarget && prefetchCount > 0 {
			log.Printf("APL: prefetched %d segments for playlist %s", prefetchCount, upstreamPath)
		}
	}

	p.writeResponse(w, resp.header, resp.status, resp.body, "MISS", strconv.Itoa(prefetchCount))
	p.updateResponseTime(start)
}

func (p *edgeProxy) fetchAndStore(ctx context.Context, reqURL *url.URL, target *upstreamTarget, prefetched bool) (*cachedResponse, error) {
	resp, err := p.fetchFromOrigin(ctx, reqURL, target)
	if err != nil {
		// Serve stale playlist/segment on error if available.
		if isPlaylistPath(reqURL.Path) || isSegmentPath(reqURL.Path) {
			if cached, prefetchedEntry, ok, _ := p.getFromCache(cacheKeyForURL(reqURL)); ok {
				header := cloneHeader(cached.header)
				header.Set("X-Go-Cache", "STALE")
				header.Set("X-Go-Prefetch", boolToPrefetch(prefetchedEntry))
				header.Set("X-Go-Fallback", "stale-error")
				return &cachedResponse{status: cached.status, header: header, body: cached.body}, nil
			}
		}
		return nil, err
	}

	// Serve stale playlist/segment on 5xx responses if available.
	if (isPlaylistPath(reqURL.Path) || isSegmentPath(reqURL.Path)) && resp != nil && resp.status >= 500 {
		if cached, prefetchedEntry, ok, _ := p.getFromCache(cacheKeyForURL(reqURL)); ok {
			header := cloneHeader(cached.header)
			header.Set("X-Go-Cache", "STALE")
			header.Set("X-Go-Prefetch", boolToPrefetch(prefetchedEntry))
			header.Set("X-Go-Fallback", "stale-5xx")
			return &cachedResponse{status: cached.status, header: header, body: cached.body}, nil
		}
	}

	if !p.cacheActive() {
		return resp, nil
	}

	ttl := p.ttlForPath(reqURL.Path)
	grace := time.Duration(0)
	if isPlaylistPath(reqURL.Path) {
		ttl = time.Duration(p.playlistTTL.Load())
	}

	p.storeCacheEntry(reqURL, resp, prefetched, ttl, grace)
	return resp, nil
}

func (p *edgeProxy) fetchFromOrigin(ctx context.Context, reqURL *url.URL, target *upstreamTarget) (*cachedResponse, error) {
	p.metrics.originRequests.Add(1)
	p.incrementOriginRequest(target)

	// For APL segments with timestamps, strip the timestamp before requesting from origin
	originURL := *reqURL
	if target == p.aplTarget && isSegmentPath(reqURL.Path) {
		lastSlash := strings.LastIndex(originURL.Path, "/")
		if lastSlash >= 0 {
			dir := originURL.Path[:lastSlash+1]
			filename := originURL.Path[lastSlash+1:]
			strippedFilename := stripTimestampFromSegment(filename)
			originURL.Path = dir + strippedFilename
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, originURL.String(), nil)
	if err != nil {
		return nil, err
	}

	for key, value := range p.forwardHeaders(target) {
		if value != "" {
			req.Header.Set(key, value)
		}
	}

	if target == nil {
		return nil, errors.New("missing upstream target")
	}

	if target.hostOverride != "" {
		req.Host = target.hostOverride
	}

	client := p.clientStrict

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	header := sanitizeHeader(resp.Header)
	
	// Transform APL playlists to add timestamps to segment names
	if target == p.aplTarget && isPlaylistPath(reqURL.Path) {
		body = transformAPLPlaylist(body)
		// Remove Content-Length as body size changed
		header.Del("Content-Length")
	}

	header.Set("Access-Control-Allow-Origin", "*")
	p.applyCacheHeaders(header, reqURL.Path)
	header.Set("X-Edge-Go", "1")
	if target == p.aplTarget {
		header.Del("Set-Cookie") // APL responses should not emit session cookies
	}

	return &cachedResponse{status: resp.StatusCode, header: header, body: body}, nil
}

func (p *edgeProxy) storeCacheEntry(reqURL *url.URL, resp *cachedResponse, prefetched bool, ttl time.Duration, grace time.Duration) {
	if !p.cacheActive() || !shouldCache(resp.status) {
		return
	}
	if ttl <= 0 {
		return
	}
	key := cacheKeyForURL(reqURL)
	p.storeCacheEntryWithKey(key, reqURL.Path, resp, prefetched, ttl, grace)
}

func (p *edgeProxy) storeCacheEntryWithKey(key string, path string, resp *cachedResponse, prefetched bool, ttl time.Duration, grace time.Duration) {
	if !p.cacheActive() || !shouldCache(resp.status) {
		return
	}
	if ttl <= 0 {
		return
	}
	storeTTL := ttl
	if grace > 0 {
		storeTTL += grace
	}
	value := &cacheValue{resp: resp, prefetched: prefetched, storedAt: time.Now(), path: path, ttl: ttl, grace: grace}
	p.incrementCacheSizeIfNew(key, path)
	p.cache.SetWithTTL(key, value, 1, storeTTL)
}

func (p *edgeProxy) getFromCache(key string) (*cachedResponse, bool, bool, bool) {
	if !p.cacheActive() {
		return nil, false, false, false
	}
	if raw, ok := p.cache.Get(key); ok {
		if value, valid := raw.(*cacheValue); valid {
			if value.ttl > 0 && value.grace > 0 {
				age := time.Since(value.storedAt)
				if age > value.ttl+value.grace {
					return nil, false, false, false
				}
				if age > value.ttl {
					return value.resp, value.prefetched, true, true
				}
			}
			return value.resp, value.prefetched, true, false
		}
	}
	return nil, false, false, false
}

func (p *edgeProxy) cacheContains(key string) bool {
	if !p.cacheActive() {
		return false
	}
	_, ok := p.cache.Get(key)
	return ok
}

// updateResponseTime calculates and stores average response time
func (p *edgeProxy) updateResponseTime(start time.Time) {
	duration := time.Since(start).Milliseconds()
	if duration >= 0 {
		p.metrics.avgResponseTime.Store(uint64(duration))
	}
}

// recordOriginFailure records failures and categorizes them by error type
func (p *edgeProxy) recordOriginFailure(target *upstreamTarget, err error) {
	p.metrics.originFailures.Add(1)
	p.incrementOriginFailure(target)

	if err == nil {
		return
	}

	errStr := err.Error()
	switch {
	case strings.Contains(errStr, "timeout") || strings.Contains(errStr, "context deadline exceeded"):
		p.metrics.originTimeouts.Add(1)
	case strings.Contains(errStr, "no such host") || strings.Contains(errStr, "dns"):
		p.metrics.originDNSErrors.Add(1)
	case strings.Contains(errStr, "connection refused") || strings.Contains(errStr, "connect"):
		p.metrics.originConnErrors.Add(1)
	}
}

// incrementOriginRequest increments request counter for specific origin
func (p *edgeProxy) incrementOriginRequest(target *upstreamTarget) {
	if target == nil || target.base == nil {
		return
	}

	if target == p.aplTarget {
		p.metrics.aplRequests.Add(1)
	}
}

// incrementOriginFailure increments failure counter for specific origin
func (p *edgeProxy) incrementOriginFailure(target *upstreamTarget) {
	if target == nil || target.base == nil {
		return
	}

	if target == p.aplTarget {
		p.metrics.aplFailures.Add(1)
	}
}



// getSnapshot returns a point-in-time snapshot of all metrics
func (m *metrics) getSnapshot() MetricsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cacheHits := m.cacheHits.Load()
	cacheMisses := m.cacheMisses.Load()
	totalCache := cacheHits + cacheMisses

	prefetchScheduled := m.prefetchScheduled.Load()
	prefetchSuccess := m.prefetchSuccess.Load()
	prefetchFailures := m.prefetchFailures.Load()
	totalPrefetch := prefetchSuccess + prefetchFailures

	originRequests := m.originRequests.Load()
	originFailures := m.originFailures.Load()

	var cacheHitRatio, prefetchSuccessRate, originFailureRate float64

	if totalCache > 0 {
		cacheHitRatio = float64(cacheHits) / float64(totalCache) * 100
	}

	if totalPrefetch > 0 {
		prefetchSuccessRate = float64(prefetchSuccess) / float64(totalPrefetch) * 100
	}

	if originRequests > 0 {
		originFailureRate = float64(originFailures) / float64(originRequests) * 100
	}

	// APL failure rate
	aplFailureRate := calculateFailureRate(m.aplRequests.Load(), m.aplFailures.Load())

	originStats := map[string]OriginMetrics{
		"apl": {
			Requests:    m.aplRequests.Load(),
			Failures:    m.aplFailures.Load(),
			FailureRate: aplFailureRate,
		},
	}

	return MetricsSnapshot{
		Timestamp:           time.Now(),
		Uptime:              time.Since(m.startTime).String(),
		CacheHits:           cacheHits,
		CacheMisses:         cacheMisses,
		CacheHitRatio:       cacheHitRatio,
		CacheSize:           m.cacheSize.Load(),
		CacheEvicted:        m.cacheEvicted.Load(),
		PrefetchScheduled:   prefetchScheduled,
		PrefetchSuccess:     prefetchSuccess,
		PrefetchFailures:    prefetchFailures,
		PrefetchSuccessRate: prefetchSuccessRate,
		PrefetchActive:      m.prefetchActive.Load(),
		OriginRequests:      originRequests,
		OriginFailures:      originFailures,
		OriginFailureRate:   originFailureRate,
		OriginTimeouts:      m.originTimeouts.Load(),
		OriginDNSErrors:     m.originDNSErrors.Load(),
		OriginConnErrors:    m.originConnErrors.Load(),
		OriginStats:         originStats,
		AvgResponseTime:     m.avgResponseTime.Load(),
		RequestCount:        m.requestCount.Load(),
	}
}

func calculateFailureRate(requests, failures uint64) float64 {
	if requests == 0 {
		return 0
	}
	return float64(failures) / float64(requests) * 100
}

// ServeMetrics handles HTTP requests for metrics endpoint
func (p *edgeProxy) ServeMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	snapshot := p.metrics.getSnapshot()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if err := json.NewEncoder(w).Encode(snapshot); err != nil {
		log.Printf("error encoding metrics: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// ServeDashboard serves the metrics dashboard HTML page
func (p *edgeProxy) ServeDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read the dashboard HTML file
	dashboardHTML, err := os.ReadFile("dashboard.html")
	if err != nil {
		log.Printf("error reading dashboard.html: %v", err)
		http.Error(w, "Dashboard not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	if _, err := w.Write(dashboardHTML); err != nil {
		log.Printf("error serving dashboard: %v", err)
	}
}

// ServeCachePage serves the cache management HTML page
func (p *edgeProxy) ServeCachePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	content, err := os.ReadFile("cache.html")
	if err != nil {
		log.Printf("error reading cache.html: %v", err)
		http.Error(w, "Cache page not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	if _, err := w.Write(content); err != nil {
		log.Printf("error serving cache page: %v", err)
	}
}

type certUploadRequest struct {
	Target      string `json:"target"`
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"privateKey"`
}

// ServeSSLUploadPage provides a simple UI for pasting certificate and key PEMs
func (p *edgeProxy) ServeSSLUploadPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.authorizeCertUpload(w, r) {
		return
	}

	content, err := os.ReadFile("ssl.html")
	if err != nil {
		log.Printf("error reading ssl.html: %v", err)
		http.Error(w, "SSL upload page not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	if _, err := w.Write(content); err != nil {
		log.Printf("error serving ssl upload page: %v", err)
	}
}

// HandleSSLUpload accepts certificate/key material and writes it to disk
func (p *edgeProxy) HandleSSLUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed - use POST", http.StatusMethodNotAllowed)
		return
	}
	if !p.authorizeCertUpload(w, r) {
		return
	}
	defer r.Body.Close()

	var payload certUploadRequest
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Invalid form payload", http.StatusBadRequest)
			return
		}
		payload.Target = r.FormValue("target")
		payload.Certificate = r.FormValue("certificate")
		payload.PrivateKey = r.FormValue("privateKey")
	}

	payload.Target = strings.ToLower(strings.TrimSpace(payload.Target))
	if payload.Target == "" {
		payload.Target = "default"
	}
	payload.Certificate = strings.TrimSpace(payload.Certificate)
	payload.PrivateKey = strings.TrimSpace(payload.PrivateKey)

	if payload.Certificate == "" || payload.PrivateKey == "" {
		http.Error(w, "Both certificate and private key are required", http.StatusBadRequest)
		return
	}
	if !strings.Contains(payload.Certificate, "BEGIN CERTIFICATE") {
		http.Error(w, "Certificate must include BEGIN CERTIFICATE", http.StatusBadRequest)
		return
	}
	if !strings.Contains(payload.PrivateKey, "BEGIN") || !strings.Contains(payload.PrivateKey, "PRIVATE KEY") {
		http.Error(w, "Private key must include BEGIN ... PRIVATE KEY", http.StatusBadRequest)
		return
	}

	certPath, keyPath, targetName, err := p.saveCertificate(payload.Target, payload.Certificate, payload.PrivateKey)
	if err != nil {
		log.Printf("error saving certificate for %s: %v", targetName, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := map[string]string{
		"message":    fmt.Sprintf("Certificate updated for %s", targetName),
		"cert_path":  certPath,
		"key_path":   keyPath,
		"next_steps": "Reload the nginx container or process to apply the new certificate files",
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("error encoding ssl upload response: %v", err)
	}
}

func (p *edgeProxy) saveCertificate(target, cert, key string) (string, string, string, error) {
	certPath, keyPath, targetName, err := p.certPaths(target)
	if err != nil {
		return "", "", targetName, err
	}
	if err := os.MkdirAll(p.certStorageDir, 0o755); err != nil {
		return "", "", targetName, fmt.Errorf("failed to prepare certificate directory: %w", err)
	}

	certPEM := ensureTrailingNewline(cert)
	keyPEM := ensureTrailingNewline(key)

	if err := writeAtomicFile(certPath, []byte(certPEM), 0o644); err != nil {
		return "", "", targetName, fmt.Errorf("failed to write certificate: %w", err)
	}
	if err := writeAtomicFile(keyPath, []byte(keyPEM), 0o600); err != nil {
		return "", "", targetName, fmt.Errorf("failed to write key: %w", err)
	}

	log.Printf("certificate files updated for %s (cert=%s, key=%s)", targetName, certPath, keyPath)
	return certPath, keyPath, targetName, nil
}

func (p *edgeProxy) certPaths(target string) (string, string, string, error) {
	switch target {
	case "", "default", "server", "primary":
		return filepath.Join(p.certStorageDir, "server.crt"), filepath.Join(p.certStorageDir, "server.key"), "default", nil
	case "apl":
		return filepath.Join(p.certStorageDir, "apl.crt"), filepath.Join(p.certStorageDir, "apl.key"), "apl", nil
	default:
		return "", "", target, fmt.Errorf("unsupported certificate target %q", target)
	}
}

func ensureTrailingNewline(s string) string {
	if s == "" {
		return s
	}
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

func (p *edgeProxy) authorizeCertUpload(w http.ResponseWriter, r *http.Request) bool {
	if !p.certUploadEnabled {
		http.NotFound(w, r)
		return false
	}
	if p.certUploadUser == "" && p.certUploadPass == "" {
		return true
	}
	user, pass, ok := r.BasicAuth()
	if !ok || subtle.ConstantTimeCompare([]byte(user), []byte(p.certUploadUser)) != 1 ||
		subtle.ConstantTimeCompare([]byte(pass), []byte(p.certUploadPass)) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="ssl-upload"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// ServeCacheClear handles POST requests to clear the cache
func (p *edgeProxy) ServeCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed - use POST", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")

	cacheConfigured := p.cache != nil
	cacheEnabled := p.cacheActive()
	cleared := false

	if cacheConfigured {
		p.cache.Clear()
		p.clearCacheKeys()
		p.metrics.cacheSize.Store(0)
		cleared = true
		log.Println("cache cleared via /cache/clear")
	}

	response := map[string]interface{}{
		"success":          cleared,
		"cache_enabled":    cacheEnabled,
		"cache_configured": cacheConfigured,
		"cleared":          cleared,
		"timestamp":        time.Now().Format(time.RFC3339),
	}

	switch {
	case !cacheConfigured:
		response["message"] = "Cache is not configured"
	case cacheEnabled:
		response["message"] = "Cache cleared"
	default:
		response["message"] = "Cache is disabled; cleared stored items but responses will bypass cache"
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("error encoding cache clear response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
}

// ServeCacheToggle enables or disables the cache without restarting the process
func (p *edgeProxy) ServeCacheToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed - use POST", http.StatusMethodNotAllowed)
		return
	}
	if p.cache == nil {
		http.Error(w, "Cache is not configured", http.StatusBadRequest)
		return
	}

	var payload struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	if payload.Enabled == nil {
		http.Error(w, "enabled is required", http.StatusBadRequest)
		return
	}

	p.cacheOn.Store(*payload.Enabled)

	response := map[string]interface{}{
		"success":       true,
		"cache_enabled": p.cacheActive(),
		"message":       "Cache state updated",
		"timestamp":     time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("error encoding cache toggle response: %v", err)
	}
}

// ServeCacheDrop removes cached entries for a specific path or prefix
func (p *edgeProxy) ServeCacheDrop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed - use POST", http.StatusMethodNotAllowed)
		return
	}
	if !p.cacheActive() {
		http.Error(w, "Cache is disabled or not configured", http.StatusBadRequest)
		return
	}

	var payload struct {
		Path   string `json:"path"`
		Prefix bool   `json:"prefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	payload.Path = strings.TrimSpace(payload.Path)
	if payload.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	removed := p.dropCacheEntries(payload.Path, payload.Prefix)

	resp := map[string]interface{}{
		"success":   true,
		"removed":   removed,
		"prefix":    payload.Prefix,
		"path":      payload.Path,
		"timestamp": time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("error encoding cache drop response: %v", err)
	}
}

// ServeCacheConfig updates TTL/grace configuration at runtime
func (p *edgeProxy) ServeCacheConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed - use POST", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		PlaylistTTLSeconds      *float64 `json:"playlist_ttl_seconds"`
		PlaylistGraceSeconds    *float64 `json:"playlist_grace_seconds"`
		WCCPlaylistTTLSeconds   *float64 `json:"wcc_playlist_ttl_seconds"`
		WCCPlaylistGraceSeconds *float64 `json:"wcc_playlist_grace_seconds"`
		SegmentTTLSeconds       *float64 `json:"segment_ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	if payload.PlaylistTTLSeconds == nil &&
		payload.PlaylistGraceSeconds == nil &&
		payload.WCCPlaylistTTLSeconds == nil &&
		payload.WCCPlaylistGraceSeconds == nil &&
		payload.SegmentTTLSeconds == nil {
		http.Error(w, "At least one TTL field is required", http.StatusBadRequest)
		return
	}

	if payload.PlaylistTTLSeconds != nil {
		dur := time.Duration(*payload.PlaylistTTLSeconds * float64(time.Second))
		if dur <= 0 {
			http.Error(w, "playlist_ttl_seconds must be greater than zero", http.StatusBadRequest)
			return
		}
		p.playlistTTL.Store(dur.Nanoseconds())
	}
	if payload.PlaylistGraceSeconds != nil {
		if *payload.PlaylistGraceSeconds < 0 {
			http.Error(w, "playlist_grace_seconds cannot be negative", http.StatusBadRequest)
			return
		}
		dur := time.Duration(*payload.PlaylistGraceSeconds * float64(time.Second))
		p.playlistGrace.Store(dur.Nanoseconds())
	}
	if payload.WCCPlaylistTTLSeconds != nil {
		dur := time.Duration(*payload.WCCPlaylistTTLSeconds * float64(time.Second))
		if dur <= 0 {
			http.Error(w, "wcc_playlist_ttl_seconds must be greater than zero", http.StatusBadRequest)
			return
		}
		p.wccPlaylistTTL.Store(dur.Nanoseconds())
	}
	if payload.WCCPlaylistGraceSeconds != nil {
		if *payload.WCCPlaylistGraceSeconds < 0 {
			http.Error(w, "wcc_playlist_grace_seconds cannot be negative", http.StatusBadRequest)
			return
		}
		dur := time.Duration(*payload.WCCPlaylistGraceSeconds * float64(time.Second))
		p.wccPlaylistGrace.Store(dur.Nanoseconds())
	}
	if payload.SegmentTTLSeconds != nil {
		dur := time.Duration(*payload.SegmentTTLSeconds * float64(time.Second))
		if dur <= 0 {
			http.Error(w, "segment_ttl_seconds must be greater than zero", http.StatusBadRequest)
			return
		}
		p.segmentTTL.Store(dur.Nanoseconds())
	}

	resp := map[string]interface{}{
		"success":       true,
		"message":       "Cache TTL configuration updated",
		"cache_enabled": p.cacheActive(),
		"config":        p.cacheConfigSnapshot(),
		"timestamp":     time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("error encoding cache config response: %v", err)
	}
}

// ServeCacheStatus exposes cache state and TTL configuration
func (p *edgeProxy) ServeCacheStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := map[string]interface{}{
		"cache_configured": p.cache != nil,
		"cache_enabled":    p.cacheActive(),
		"cache_size":       p.metrics.cacheSize.Load(),
		"cache_evicted":    p.metrics.cacheEvicted.Load(),
		"config":           p.cacheConfigSnapshot(),
		"timestamp":        time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("error encoding cache status response: %v", err)
	}
}

// ServeCachePrefetch triggers a manual prefetch for the given path
func (p *edgeProxy) ServeCachePrefetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed - use POST", http.StatusMethodNotAllowed)
		return
	}
	if !p.cacheActive() {
		http.Error(w, "Cache is disabled or not configured", http.StatusBadRequest)
		return
	}

	var payload struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	payload.Path = strings.TrimSpace(payload.Path)
	if payload.Path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	target, upstreamPath, err := p.selectUpstream(payload.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if target == nil || target.base == nil {
		http.Error(w, "No upstream available for path", http.StatusBadRequest)
		return
	}

	reqURL := buildRequestURL(target.base, upstreamPath, "")
	ctx, cancel := context.WithTimeout(context.Background(), p.upstreamDelay)
	defer cancel()

	resp, fetchErr := p.fetchAndStore(ctx, &reqURL, target, true)
	if fetchErr != nil {
		log.Printf("manual prefetch failed for %s: %v", reqURL.Redacted(), fetchErr)
		http.Error(w, fmt.Sprintf("prefetch failed: %v", fetchErr), http.StatusBadGateway)
		return
	}

	scheduled := 0
	if resp != nil && shouldCache(resp.status) && isPlaylistPath(upstreamPath) {
		parsed := parsePlaylistEntries(&reqURL, resp.body)
		scheduled = p.scheduleSegmentPrefetches(target, parsed.segments)
	}

	statusCode := 0
	if resp != nil {
		statusCode = resp.status
	}

	result := map[string]interface{}{
		"success":            true,
		"path":               payload.Path,
		"status":             statusCode,
		"prefetch_scheduled": scheduled,
		"timestamp":          time.Now().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log.Printf("error encoding cache prefetch response: %v", err)
	}
}

// ServeMetricsReset handles POST requests to reset metrics
func (p *edgeProxy) ServeMetricsReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed - use POST", http.StatusMethodNotAllowed)
		return
	}

	// Reset the metrics
	p.metrics.reset()

	// Return success response
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	response := map[string]interface{}{
		"success":   true,
		"message":   "Metrics reset successfully",
		"timestamp": time.Now().Format(time.RFC3339),
	}

	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("error encoding reset response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
} // startMetricsLogging starts a goroutine that logs metrics periodically
func (p *edgeProxy) startMetricsLogging() {
	go func() {
		ticker := time.NewTicker(60 * time.Second) // Log every minute
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				snapshot := p.metrics.getSnapshot()
				log.Printf("METRICS: requests=%d cache_hit_ratio=%.1f%% prefetch_success=%.1f%% origin_failures=%.1f%% active_prefetch=%d avg_response_ms=%d",
					snapshot.RequestCount,
					snapshot.CacheHitRatio,
					snapshot.PrefetchSuccessRate,
					snapshot.OriginFailureRate,
					snapshot.PrefetchActive,
					snapshot.AvgResponseTime,
				)
			}
		}
	}()
}

// startDailyMetricsReset starts a goroutine that resets metrics daily at specified time
func (p *edgeProxy) startDailyMetricsReset(resetTime string) {
	if resetTime == "" {
		return
	}

	go func() {
		for {
			// Calculate next reset time
			now := time.Now()
			resetTimeParsed, err := time.Parse("15:04", resetTime)
			if err != nil {
				log.Printf("Error parsing reset time %s: %v", resetTime, err)
				return
			}

			// Set the reset time to today
			nextReset := time.Date(now.Year(), now.Month(), now.Day(),
				resetTimeParsed.Hour(), resetTimeParsed.Minute(), 0, 0, now.Location())

			// If the reset time for today has passed, schedule for tomorrow
			if nextReset.Before(now) {
				nextReset = nextReset.Add(24 * time.Hour)
			}

			// Wait until the next reset time
			timeUntilReset := nextReset.Sub(now)
			log.Printf("Scheduled daily metrics reset at %s (in %s)", nextReset.Format("2006-01-02 15:04:05"), timeUntilReset.Round(time.Minute))

			timer := time.NewTimer(timeUntilReset)
			<-timer.C

			// Reset metrics
			p.metrics.reset()
			log.Printf("Daily metrics reset completed at %s", time.Now().Format("2006-01-02 15:04:05"))
		}
	}()
}

func (p *edgeProxy) applyCacheHeaders(header http.Header, path string) {
	switch {
	case isPlaylistPath(path):
		header.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		header.Set("Pragma", "no-cache")
		header.Set("Expires", "0")
	case isSegmentPath(path):
		header.Set("Cache-Control", "public, max-age=10")
	}
}

func (p *edgeProxy) ttlForPath(path string) time.Duration {
	if isPlaylistPath(path) {
		return time.Duration(p.playlistTTL.Load())
	}
	if isSegmentPath(path) {
		return time.Duration(p.segmentTTL.Load())
	}
	return time.Duration(p.segmentTTL.Load())
}

func (p *edgeProxy) forwardHeaders(target *upstreamTarget) map[string]string {
	var origin string
	var referer string
	if target != nil {
		if target.refererHeader != "" {
			referer = target.refererHeader
		}
		if target.originHeader != "" {
			origin = target.originHeader
		} else {
			origin = originFromReferer(referer)
		}
	}
	if origin == "" {
		origin = p.primeOrigin
	}
	if referer == "" {
		referer = p.primeReferer
	}
	headers := map[string]string{
		"User-Agent":         p.userAgent,
		"Origin":             origin,
		"Referer":            referer,
		"sec-ch-ua":          `"Chromium";v="142", "Google Chrome";v="142", "Not_A Brand";v="99"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"macOS"`,
	}
	if target == p.aplTarget {
		headers["Cookie"] = "JSESSIONID=853C565B76C69AF5EE8F8BE3D6AC13B0"
	}
	if target == p.wccTarget {
		headers["Cookie"] = "JSESSIONID=E8D7CF0EA66945D405FD2EA87F55E7A7"
	}
	return headers
}

func (p *edgeProxy) selectUpstream(path string) (*upstreamTarget, string, error) {
	switch {
	case strings.HasPrefix(path, "/__prefetch/apl"):
		if p.aplTarget == nil {
			return nil, "", errors.New("APL origin not configured")
		}
		translated := trimPrefix(path, "/__prefetch/apl")
		return p.aplTarget, translated, nil
	case strings.HasPrefix(path, "/apl"):
		if p.aplTarget == nil {
			return nil, "", errors.New("APL origin not configured")
		}
		translated := trimPrefix(path, "/apl")
		// Keep timestamp in the path for cache key differentiation
		// We'll strip it when making the actual origin request
		return p.aplTarget, translated, nil
	case path == "":
		return nil, "", errors.New("empty request path")
	default:
		return nil, "", errors.New("only APL origin is supported")
	}
}



func (p *edgeProxy) schedulePrefetch(target *upstreamTarget, playlistPath string, body []byte) int {
	if target == nil || target.base == nil {
		return 0
	}
	if !p.cacheActive() {
		return 0
	}
	if !p.prefetchOn || p.prefetchSem == nil || p.prefetchBatch <= 0 || len(body) == 0 {
		return 0
	}

	playlistURL := buildRequestURL(target.base, playlistPath, "")
	return p.schedulePrefetchFromPlaylist(target, &playlistURL, body)
}

// scheduleAPLPrefetchWithImmediate immediately caches future segments and 
// schedules async prefetch for the rest to maximize APL cache hit rate
func (p *edgeProxy) scheduleAPLPrefetchWithImmediate(target *upstreamTarget, playlistPath string, body []byte) int {
	if target == nil || target.base == nil {
		return 0
	}
	if !p.cacheActive() {
		return 0
	}
	if !p.prefetchOn || p.prefetchSem == nil || p.prefetchBatch <= 0 || len(body) == 0 {
		return 0
	}

	playlistURL := buildRequestURL(target.base, playlistPath, "")
	parsed := parsePlaylistEntries(&playlistURL, body)
	
	// Generate extended lookahead with many more future segments
	segmentsWithLookahead := p.generateAPLLookahead(parsed.segments)
	
	// For APL, immediately cache the NEXT 5-10 segments that will likely be requested
	// The player often requests segments ahead of the playlist update
	immediateCacheCount := 10
	if len(segmentsWithLookahead) < immediateCacheCount {
		immediateCacheCount = len(segmentsWithLookahead)
	}
	
	// Start from the END of the actual playlist (most likely to be requested next)
	startIdx := len(parsed.segments) - 3
	if startIdx < 0 {
		startIdx = 0
	}
	
	cached := 0
	for i := startIdx; i < len(segmentsWithLookahead) && cached < immediateCacheCount; i++ {
		segURL := segmentsWithLookahead[i]
		if segURL == nil {
			continue
		}
		key := cacheKeyForURL(segURL)
		if p.cacheContains(key) {
			continue
		}
		
		// Fetch immediately (synchronously) to ensure it's cached before player requests it
		ctx, cancel := context.WithTimeout(context.Background(), p.upstreamDelay)
		if _, err := p.fetchAndStore(ctx, segURL, target, true); err == nil {
			log.Printf("APL: immediate cached segment %s", segURL.Path)
			cached++
		}
		cancel()
	}
	
	// Now schedule async prefetch for earlier segments and remaining future segments
	scheduled := 0
	for i := 0; i < len(segmentsWithLookahead) && scheduled < p.prefetchBatch; i++ {
		segURL := segmentsWithLookahead[i]
		if segURL == nil {
			continue
		}
		key := cacheKeyForURL(segURL)
		if p.cacheContains(key) {
			continue
		}
		
		scheduled++
		p.metrics.prefetchScheduled.Add(1)
		p.spawnPrefetch(target, segURL)
	}
	
	return cached + scheduled
}

func (p *edgeProxy) schedulePrefetchFromPlaylist(target *upstreamTarget, playlistURL *url.URL, body []byte) int {
	parsed := parsePlaylistEntries(playlistURL, body)
	return p.scheduleSegmentPrefetches(target, parsed.segments)
}

func (p *edgeProxy) scheduleSegmentPrefetches(target *upstreamTarget, segments []*url.URL) int {
	if !p.cacheActive() || p.prefetchSem == nil {
		return 0
	}
	scheduled := 0
	
	// For APL, prefetch ALL segments in playlist + generate lookahead URLs
	segmentsToFetch := segments
	if target == p.aplTarget && len(segments) > 0 {
		// Extract segment number pattern and generate lookahead segments
		segmentsToFetch = p.generateAPLLookahead(segments)
	}
	
	for _, segURL := range segmentsToFetch {
		if segURL == nil {
			continue
		}
		key := cacheKeyForURL(segURL)
		if p.cacheContains(key) {
			continue
		}

		scheduled++
		p.metrics.prefetchScheduled.Add(1)
		p.spawnPrefetch(target, segURL)
		if scheduled >= p.prefetchBatch {
			break
		}
	}
	return scheduled
}

type playlistParseResult struct {
	segments []*url.URL
	variants []variantPlaylist
}

type variantPlaylist struct {
	url       *url.URL
	bandwidth int
	order     int
}

func parsePlaylistEntries(playlistURL *url.URL, body []byte) playlistParseResult {
	lines := bytes.Split(body, []byte("\n"))
	result := playlistParseResult{}

	var pendingBandwidth int
	var hasPendingVariant bool
	var order int

	for _, rawLine := range lines {
		line := strings.TrimSpace(string(rawLine))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "#") {
			switch {
			case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
				pendingBandwidth = parseBandwidth(line)
				hasPendingVariant = true
			case strings.HasPrefix(line, "#EXT-X-I-FRAME-STREAM-INF"):
				uri := parseAttribute(line, "URI")
				if uri == "" {
					continue
				}
				if ref, err := url.Parse(uri); err == nil {
					resolved := playlistURL.ResolveReference(ref)
					if ref.Host == "" || ref.Host == playlistURL.Host {
						result.variants = append(result.variants, variantPlaylist{
							url:       resolved,
							bandwidth: parseBandwidth(line),
							order:     order,
						})
						order++
					}
				}
				hasPendingVariant = false
				pendingBandwidth = 0
			case strings.HasPrefix(line, "#EXT-X-MEDIA"):
				uri := parseAttribute(line, "URI")
				if uri == "" {
					continue
				}
				if ref, err := url.Parse(uri); err == nil {
					resolved := playlistURL.ResolveReference(ref)
					if (ref.Host == "" || ref.Host == playlistURL.Host) && isPlaylistPath(resolved.Path) {
						result.variants = append(result.variants, variantPlaylist{
							url:       resolved,
							bandwidth: 0,
							order:     order,
						})
						order++
					}
				}
				hasPendingVariant = false
				pendingBandwidth = 0
			}
			continue
		}

		ref, err := url.Parse(line)
		if err != nil {
			hasPendingVariant = false
			pendingBandwidth = 0
			continue
		}

		resolved := playlistURL.ResolveReference(ref)
		if ref.Host != "" && ref.Host != playlistURL.Host {
			hasPendingVariant = false
			pendingBandwidth = 0
			continue
		}

		if isSegmentPath(resolved.Path) {
			result.segments = append(result.segments, resolved)
		} else if isPlaylistPath(resolved.Path) {
			bw := 0
			if hasPendingVariant {
				bw = pendingBandwidth
			}
			result.variants = append(result.variants, variantPlaylist{
				url:       resolved,
				bandwidth: bw,
				order:     order,
			})
			order++
		}

		hasPendingVariant = false
		pendingBandwidth = 0
	}

	return result
}

func selectVariantPlaylist(variants []variantPlaylist) *variantPlaylist {
	if len(variants) == 0 {
		return nil
	}
	best := variants[0]
	for _, candidate := range variants[1:] {
		if candidate.bandwidth > best.bandwidth {
			best = candidate
			continue
		}
		if candidate.bandwidth == best.bandwidth && candidate.order < best.order {
			best = candidate
		}
	}
	return &best
}

func parseBandwidth(line string) int {
	raw := parseAttribute(line, "BANDWIDTH")
	if raw == "" {
		return 0
	}
	bw, err := strconv.Atoi(raw)
	if err != nil || bw < 0 {
		return 0
	}
	return bw
}

// generateAPLLookahead creates additional segment URLs beyond what's in the playlist
// to preemptively cache segments that will be requested soon
func (p *edgeProxy) generateAPLLookahead(segments []*url.URL) []*url.URL {
	if len(segments) == 0 {
		return segments
	}
	
	result := make([]*url.URL, 0, len(segments)+10)
	result = append(result, segments...)
	
	// Find the last segment and extract its number
	lastSeg := segments[len(segments)-1]
	if lastSeg == nil {
		return result
	}
	
	// Parse segment number from URL like "apexgaming000006163.ts"
	path := lastSeg.Path
	lastSlash := strings.LastIndex(path, "/")
	if lastSlash < 0 {
		return result
	}
	
	filename := path[lastSlash+1:]
	// Extract number from filename
	var prefix string
	var segNum int
	var suffix string
	
	// Match pattern: prefix + digits + suffix (e.g., "apexgaming000006163.ts")
	for i := len(filename) - 1; i >= 0; i-- {
		if filename[i] >= '0' && filename[i] <= '9' {
			continue
		}
		if i < len(filename)-1 {
			// Found the start of the number
			prefix = filename[:i+1]
			numStr := ""
			j := i + 1
			for j < len(filename) && filename[j] >= '0' && filename[j] <= '9' {
				numStr += string(filename[j])
				j++
			}
			suffix = filename[j:]
			segNum, _ = strconv.Atoi(numStr)
			break
		}
	}
	
	if prefix == "" || segNum == 0 {
		return result
	}
	
	// Generate next 20 segments to handle aggressive players
	numDigits := len(strconv.Itoa(segNum))
	for i := 1; i <= 20; i++ {
		nextNum := segNum + i
		nextNumStr := strconv.Itoa(nextNum)
		// Pad with zeros to match original format
		for len(nextNumStr) < numDigits {
			nextNumStr = "0" + nextNumStr
		}
		nextFilename := prefix + nextNumStr + suffix
		nextPath := path[:lastSlash+1] + nextFilename
		
		nextURL := *lastSeg // Copy the URL
		nextURL.Path = nextPath
		result = append(result, &nextURL)
	}
	
	return result
}

func parseAttribute(line, key string) string {
	idx := strings.Index(line, ":")
	if idx == -1 || idx == len(line)-1 {
		return ""
	}
	attrs := strings.Split(line[idx+1:], ",")
	for _, attr := range attrs {
		parts := strings.SplitN(strings.TrimSpace(attr), "=", 2)
		if len(parts) != 2 {
			continue
		}
		if !strings.EqualFold(parts[0], key) {
			continue
		}
		return strings.Trim(parts[1], `"`)
	}
	return ""
}

func (p *edgeProxy) spawnPrefetch(target *upstreamTarget, segURL *url.URL) {
	if p.prefetchSem == nil || target == nil {
		return
	}

	p.prefetchSem <- struct{}{}
	p.metrics.prefetchActive.Add(1)
	go func() {
		defer func() {
			<-p.prefetchSem
			p.metrics.prefetchActive.Add(-1)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), p.upstreamDelay)
		defer cancel()
		if _, err := p.fetchAndStore(ctx, segURL, target, true); err != nil {
			p.metrics.prefetchFailures.Add(1)
			if target == p.aplTarget {
				log.Printf("APL prefetch %s failed: %v", segURL.Redacted(), err)
			} else {
				log.Printf("prefetch %s failed: %v", segURL.Redacted(), err)
			}
		} else {
			p.metrics.prefetchSuccess.Add(1)
			if target == p.aplTarget {
				log.Printf("APL prefetch %s succeeded", segURL.Redacted())
			}
		}
	}()
}

type cachedResponse struct {
	status int
	header http.Header
	body   []byte
}

type cacheValue struct {
	resp       *cachedResponse
	prefetched bool
	storedAt   time.Time
	path       string
	ttl        time.Duration
	grace      time.Duration
}

func (p *edgeProxy) writeResponse(w http.ResponseWriter, header http.Header, status int, body []byte, cacheStatus, prefetchInfo string) {
	for k, vv := range header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Go-Cache", cacheStatus)
	w.Header().Set("X-Go-Prefetch", prefetchInfo)
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Printf("write response error: %v", err)
	}
}

func (p *edgeProxy) cacheActive() bool {
	return p.cache != nil && p.cacheOn.Load()
}

func (p *edgeProxy) incrementCacheSizeIfNew(key, path string) cacheHashKey {
	var hash cacheHashKey
	if key != "" {
		h1, h2 := z.KeyToHash(key)
		hash = cacheHashKey{primary: h1, conflict: h2}
	}
	if p.cacheKeys != nil {
		if _, exists := p.cacheKeys.LoadOrStore(key, cacheKeyInfo{path: path, hash: hash}); !exists {
			p.metrics.cacheSize.Add(1)
		}
	}
	if p.cacheHashIndex != nil && hash.primary != 0 {
		p.cacheHashIndex.Store(hash, key)
	}
	return hash
}

func (p *edgeProxy) removeCacheKey(key string) bool {
	if p.cacheKeys == nil {
		return false
	}
	if info, exists := p.cacheKeys.LoadAndDelete(key); exists {
		if keyInfo, ok := info.(cacheKeyInfo); ok && p.cacheHashIndex != nil {
			p.cacheHashIndex.Delete(keyInfo.hash)
		}
		p.metrics.cacheSize.Add(^uint64(0))
		return true
	}
	return false
}

func (p *edgeProxy) clearCacheKeys() {
	if p.cacheKeys == nil {
		return
	}
	p.cacheKeys.Range(func(key, value interface{}) bool {
		p.cacheKeys.Delete(key)
		return true
	})
	if p.cacheHashIndex != nil {
		p.cacheHashIndex.Range(func(key, value interface{}) bool {
			p.cacheHashIndex.Delete(key)
			return true
		})
	}
	p.metrics.cacheSize.Store(0)
}

func (p *edgeProxy) cacheConfigSnapshot() map[string]float64 {
	return map[string]float64{
		"playlist_ttl_seconds":       float64(time.Duration(p.playlistTTL.Load())) / float64(time.Second),
		"playlist_grace_seconds":     float64(time.Duration(p.playlistGrace.Load())) / float64(time.Second),
		"wcc_playlist_ttl_seconds":   float64(time.Duration(p.wccPlaylistTTL.Load())) / float64(time.Second),
		"wcc_playlist_grace_seconds": float64(time.Duration(p.wccPlaylistGrace.Load())) / float64(time.Second),
		"segment_ttl_seconds":        float64(time.Duration(p.segmentTTL.Load())) / float64(time.Second),
	}
}

func (p *edgeProxy) dropCacheEntries(path string, prefix bool) int {
	if !p.cacheActive() {
		return 0
	}
	normalized := strings.TrimSpace(path)
	if normalized == "" {
		return 0
	}

	var removed int
	p.cacheKeys.Range(func(key, value interface{}) bool {
		keyStr, ok := key.(string)
		info, okInfo := value.(cacheKeyInfo)
		if !ok || !okInfo {
			return true
		}
		match := info.path == normalized
		if !match && prefix {
			match = strings.HasPrefix(info.path, normalized)
		}
		if match {
			p.cache.Del(keyStr)
			if p.removeCacheKey(keyStr) {
				p.metrics.cacheEvicted.Add(1)
			}
			removed++
		}
		return true
	})
	return removed
}

func (p *edgeProxy) scheduleRevalidate(target *upstreamTarget, reqURL *url.URL) {
	if target == nil || target.base == nil {
		return
	}
	key := cacheKeyForURL(reqURL)
	if _, loaded := p.revalidateMap.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	go func() {
		defer p.revalidateMap.Delete(key)
		ctx, cancel := context.WithTimeout(context.Background(), p.upstreamDelay)
		defer cancel()
		if _, err := p.fetchAndStore(ctx, reqURL, target, false); err != nil {
			log.Printf("revalidate %s failed: %v", reqURL.Redacted(), err)
		}
	}()
}

func sanitizeHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, vv := range src {
		canonical := textproto.CanonicalMIMEHeaderKey(k)
		if hopByHopHeaders[canonical] || stripResponseHeaders[canonical] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	dst.Del("X-Go-Cache")
	dst.Del("X-Go-Prefetch")
	dst.Del("X-Edge-Go")
	dst.Del("Access-Control-Allow-Origin")
	return dst
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	return dst
}

func cacheKeyForURL(u *url.URL) string {
	clone := *u
	clone.User = nil
	clone.Fragment = ""
	clone.RawQuery = ""
	return clone.String()
}

func buildRequestURL(base *url.URL, path, rawQuery string) url.URL {
	reqURL := *base
	reqURL.Path = path
	reqURL.RawQuery = rawQuery
	reqURL.Fragment = ""
	return reqURL
}

func boolToPrefetch(flag bool) string {
	if flag {
		return "1"
	}
	return "0"
}

func originFromReferer(referer string) string {
	if referer == "" {
		return ""
	}
	u, err := url.Parse(referer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.Path = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func matchNamed(path, base string, prefixes []string, targets map[string]*upstreamTarget) (*upstreamTarget, string, bool) {
	if len(prefixes) == 0 || len(targets) == 0 {
		return nil, "", false
	}
	for _, name := range prefixes {
		candidate := base + "/" + name
		if base == "" {
			candidate = "/" + name
		}
		if path == candidate || strings.HasPrefix(path, candidate+"/") {
			target := targets[name]
			if target == nil {
				continue
			}
			return target, trimPrefix(path, candidate), true
		}
	}
	return nil, "", false
}



func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func writeAtomicFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func isPlaylistPath(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".m3u8")
}

func isSegmentPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".ts") || strings.HasSuffix(lower, ".mp4")
}

func shouldCache(status int) bool {
	return status == http.StatusOK || status == http.StatusFound
}

var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"TE":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Keep-Alive":          true,
}

var stripResponseHeaders = map[string]bool{
	"Alt-Svc":         true,
	"Cf-Cache-Status": true,
	"Cf-Ray":          true,
	"Date":            true,
	"Etag":            true,
	"Last-Modified":   true,
	"Nel":             true,
	"Report-To":       true,
	"Priority":        true,
	"Server":          true,
	"Server-Timing":   true,
	"Set-Cookie":      true,
}

func trimPrefix(path, prefix string) string {
	out := strings.TrimPrefix(path, prefix)
	if out == "" {
		return "/"
	}
	if !strings.HasPrefix(out, "/") {
		return "/" + out
	}
	return out
}

// stripTimestampFromSegment removes the timestamp suffix from APL segment filenames
// Example: "apexgaming0007-1770464225.ts" -> "apexgaming0007.ts"
func stripTimestampFromSegment(filename string) string {
	// Check if filename has timestamp pattern: name-timestamp.ext
	if !strings.Contains(filename, "-") {
		return filename
	}
	
	// Find the extension
	extIdx := strings.LastIndex(filename, ".")
	if extIdx < 0 {
		return filename
	}
	
	// Find the last dash before the extension
	dashIdx := strings.LastIndex(filename[:extIdx], "-")
	if dashIdx < 0 {
		return filename
	}
	
	// Check if what's after the dash is a timestamp (all digits)
	timestampPart := filename[dashIdx+1 : extIdx]
	for _, c := range timestampPart {
		if c < '0' || c > '9' {
			// Not a timestamp, return original
			return filename
		}
	}
	
	// Strip the timestamp and return: prefix + extension
	return filename[:dashIdx] + filename[extIdx:]
}

// addTimestampToSegment adds a stable hash-based suffix to segment filenames
// The hash is based on the segment name + date + 30-minute session ID.
// This ensures:
// - Same segment gets same hash during playlist refreshes (iOS Safari compatibility)
// - Different stream sessions (even same day) get different hashes (no stale cache)
// - Session changes every 30 minutes to differentiate stream restarts
// Example: "apexgaming0007.ts" -> "apexgaming0007-3456789012.ts"
func addTimestampToSegment(filename string) string {
	// Get current date and 30-minute session ID
	now := time.Now()
	dateStr := now.Format("20060102") // e.g., "20260325"
	
	// Create session ID that changes every 30 minutes
	// This makes each stream session unique while staying stable during playback
	hour := now.Hour()
	sessionID := fmt.Sprintf("%02d%d", hour, now.Minute()/30) // e.g., "140" = 14:00-14:29, "141" = 14:30-14:59
	
	// Find the extension
	extIdx := strings.LastIndex(filename, ".")
	if extIdx < 0 {
		// No extension, use simple hash of filename + date + session
		hash := uint64(0)
		combined := filename + dateStr + sessionID
		for _, c := range combined {
			hash = hash*31 + uint64(c)
		}
		return fmt.Sprintf("%s-%d", filename, hash%10000000000)
	}
	
	// Create a stable hash from the base filename + date + session
	base := filename[:extIdx]
	combined := base + dateStr + sessionID
	hash := uint64(0)
	for _, c := range combined {
		hash = hash*31 + uint64(c)
	}
	
	// Insert stable hash before the extension
	return fmt.Sprintf("%s-%d%s", base, hash%10000000000, filename[extIdx:])
}

// transformAPLPlaylist adds timestamps to segment names in APL m3u8 playlists
// This prevents issues when the origin stream rolls back
func transformAPLPlaylist(body []byte) []byte {
	lines := bytes.Split(body, []byte("\n"))
	var result [][]byte
	
	for _, line := range lines {
		lineStr := string(line)
		trimmed := strings.TrimSpace(lineStr)
		
		// Check if this line is a segment (not a comment and ends with .ts or .mp4)
		if !strings.HasPrefix(trimmed, "#") && trimmed != "" && isSegmentPath(trimmed) {
			// Add timestamp to the segment filename
			timestampedSegment := addTimestampToSegment(trimmed)
			result = append(result, []byte(timestampedSegment))
		} else {
			result = append(result, line)
		}
	}
	
	return bytes.Join(result, []byte("\n"))
}
