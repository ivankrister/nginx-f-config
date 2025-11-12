package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto"
)

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	proxy, err := newEdgeProxy(cfg)
	if err != nil {
		log.Fatalf("proxy init error: %v", err)
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           proxy,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.UpstreamTimeout + 5*time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    http.DefaultMaxHeaderBytes,
	}

	log.Printf("edge proxy listening on %s (oryx=%v, perya=%s)", cfg.ListenAddr, cfg.OryxOrigins, cfg.PeryaOrigin)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

type config struct {
	ListenAddr        string
	OryxOrigins       []string
	PeryaOrigin       string
	PrimeHost         string
	PrimeOrigin       string
	PrimeReferer      string
	DisableTLSVerify  bool
	UpstreamTimeout   time.Duration
	UpstreamUserAgent string
	CacheEntries      int
	PlaylistTTL       time.Duration
	SegmentTTL        time.Duration
	PrefetchWorkers   int
	PrefetchBatch     int
	PrefetchEnabled   bool
}

func loadConfig() (*config, error) {
	getenv := func(key, def string) string {
		if val := strings.TrimSpace(os.Getenv(key)); val != "" {
			return val
		}
		return def
	}

	parseOrigins := func(vars ...string) []string {
		var out []string
		for _, v := range vars {
			if val := strings.TrimSpace(os.Getenv(v)); val != "" {
				out = append(out, val)
			}
		}
		return out
	}

	oryxOrigins := parseOrigins("ORYX_SERVER", "ORYX_SERVER2")
	if len(oryxOrigins) == 0 {
		return nil, errors.New("at least one ORYX_SERVER is required")
	}

	perya := strings.TrimSpace(os.Getenv("PERYA_SERVER"))
	if perya == "" {
		return nil, errors.New("PERYA_SERVER is required")
	}

	timeout := 5 * time.Second
	if raw := strings.TrimSpace(os.Getenv("UPSTREAM_TIMEOUT")); raw != "" {
		dur, err := time.ParseDuration(raw)
		if err != nil || dur <= 0 {
			return nil, fmt.Errorf("invalid UPSTREAM_TIMEOUT: %w", err)
		}
		timeout = dur
	}

	disableTLS := false
	if raw := strings.TrimSpace(os.Getenv("DISABLE_TLS_VERIFY")); raw != "" {
		val, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid DISABLE_TLS_VERIFY: %w", err)
		}
		disableTLS = val
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

	return &config{
		ListenAddr:        getenv("LISTEN_ADDR", ":9000"),
		OryxOrigins:       oryxOrigins,
		PeryaOrigin:       perya,
		PrimeHost:         getenv("PRIME_STREAM_HOST", ""),
		PrimeOrigin:       getenv("PRIME_STREAM_ORIGIN", ""),
		PrimeReferer:      getenv("PRIME_STREAM_REFERER", ""),
		DisableTLSVerify:  disableTLS,
		UpstreamTimeout:   timeout,
		UpstreamUserAgent: getenv("EDGE_USER_AGENT", defaultUserAgent),
		CacheEntries:      cacheEntries,
		PlaylistTTL:       playlistTTL,
		SegmentTTL:        segmentTTL,
		PrefetchWorkers:   prefetchWorkers,
		PrefetchBatch:     prefetchBatch,
		PrefetchEnabled:   prefetchEnabled,
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

const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36"

type edgeProxy struct {
	client        *http.Client
	oryxTargets   []*url.URL
	peryaTarget   *url.URL
	primeHost     string
	primeOrigin   string
	primeReferer  string
	userAgent     string
	oryxCounter   atomic.Uint64
	upstreamDelay time.Duration
	cache         *ristretto.Cache
	playlistTTL   time.Duration
	segmentTTL    time.Duration
	prefetchBatch int
	prefetchSem   chan struct{}
	prefetchOn    bool
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

	var oryxURLs []*url.URL
	for _, origin := range cfg.OryxOrigins {
		u, err := buildURL(origin)
		if err != nil {
			return nil, fmt.Errorf("invalid ORYX origin %q: %w", origin, err)
		}
		oryxURLs = append(oryxURLs, u)
	}

	peryaURL, err := buildURL(cfg.PeryaOrigin)
	if err != nil {
		return nil, fmt.Errorf("invalid PERYA origin: %w", err)
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          256,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.DisableTLSVerify, //nolint:gosec
		},
	}

	client := &http.Client{
		Transport: transport,
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

	return &edgeProxy{
		client:        client,
		oryxTargets:   oryxURLs,
		peryaTarget:   peryaURL,
		primeHost:     cfg.PrimeHost,
		primeOrigin:   cfg.PrimeOrigin,
		primeReferer:  cfg.PrimeReferer,
		userAgent:     cfg.UpstreamUserAgent,
		upstreamDelay: cfg.UpstreamTimeout,
		cache:         cache,
		playlistTTL:   cfg.PlaylistTTL,
		segmentTTL:    cfg.SegmentTTL,
		prefetchBatch: cfg.PrefetchBatch,
		prefetchSem:   prefetchSem,
		prefetchOn:    cfg.PrefetchEnabled && cfg.PrefetchWorkers > 0 && cfg.PrefetchBatch > 0,
	}, nil
}

func (p *edgeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), p.upstreamDelay)
	defer cancel()

	target, upstreamPath, err := p.selectUpstream(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	reqURL := buildRequestURL(target, upstreamPath, r.URL.RawQuery)
	cacheKey := cacheKeyForURL(&reqURL)

	if entry, prefetched, ok := p.getFromCache(cacheKey); ok {
		p.writeResponse(w, entry.header, entry.status, entry.body, "HIT", boolToPrefetch(prefetched))
		return
	}

	resp, err := p.fetchAndStore(ctx, &reqURL, false)
	if err != nil {
		log.Printf("upstream request to %s failed: %v", reqURL.Redacted(), err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}

	prefetchCount := 0
	if shouldCache(resp.status) && isPlaylistPath(upstreamPath) {
		prefetchCount = p.schedulePrefetch(target, upstreamPath, resp.body)
	}

	p.writeResponse(w, resp.header, resp.status, resp.body, "MISS", strconv.Itoa(prefetchCount))
}

func (p *edgeProxy) fetchAndStore(ctx context.Context, reqURL *url.URL, prefetched bool) (*cachedResponse, error) {
	resp, err := p.fetchFromOrigin(ctx, reqURL)
	if err != nil {
		return nil, err
	}
	p.storeCacheEntry(reqURL, resp, prefetched)
	return resp, nil
}

func (p *edgeProxy) fetchFromOrigin(ctx context.Context, reqURL *url.URL) (*cachedResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return nil, err
	}

	for key, value := range p.forwardHeaders() {
		if value != "" {
			req.Header.Set(key, value)
		}
	}

	if host := p.primeHost; host != "" {
		req.Host = host
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	header := sanitizeHeader(resp.Header)
	header.Set("Access-Control-Allow-Origin", "*")
	p.applyCacheHeaders(header, reqURL.Path)
	header.Set("X-Edge-Go", "1")

	return &cachedResponse{status: resp.StatusCode, header: header, body: body}, nil
}

func (p *edgeProxy) storeCacheEntry(reqURL *url.URL, resp *cachedResponse, prefetched bool) {
	if p.cache == nil || !shouldCache(resp.status) {
		return
	}
	ttl := p.ttlForPath(reqURL.Path)
	if ttl <= 0 {
		return
	}
	value := &cacheValue{resp: resp, prefetched: prefetched}
	p.cache.SetWithTTL(cacheKeyForURL(reqURL), value, 1, ttl)
}

func (p *edgeProxy) getFromCache(key string) (*cachedResponse, bool, bool) {
	if p.cache == nil {
		return nil, false, false
	}
	if raw, ok := p.cache.Get(key); ok {
		if value, valid := raw.(*cacheValue); valid {
			return value.resp, value.prefetched, true
		}
	}
	return nil, false, false
}

func (p *edgeProxy) cacheContains(key string) bool {
	if p.cache == nil {
		return false
	}
	_, ok := p.cache.Get(key)
	return ok
}

func (p *edgeProxy) applyCacheHeaders(header http.Header, path string) {
	switch {
	case isPlaylistPath(path):
		header.Set("Cache-Control", "no-store, no-cache, must-revalidate")
		header.Set("Pragma", "no-cache")
		header.Set("Expires", "0")
	case isSegmentPath(path):
		header.Set("Cache-Control", "public, max-age=30")
	}
}

func (p *edgeProxy) ttlForPath(path string) time.Duration {
	if isPlaylistPath(path) {
		return p.playlistTTL
	}
	if isSegmentPath(path) {
		return p.segmentTTL
	}
	return p.segmentTTL
}

func (p *edgeProxy) forwardHeaders() map[string]string {
	return map[string]string{
		"User-Agent":         p.userAgent,
		"Origin":             p.primeOrigin,
		"Referer":            p.primeReferer,
		"sec-ch-ua":          `"Chromium";v="142", "Google Chrome";v="142", "Not_A Brand";v="99"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"macOS"`,
	}
}

func (p *edgeProxy) selectUpstream(path string) (*url.URL, string, error) {
	switch {
	case strings.HasPrefix(path, "/__prefetch/perya"):
		return p.peryaTarget, trimPrefix(path, "/__prefetch"), nil
	case strings.HasPrefix(path, "/perya/"):
		return p.peryaTarget, trimPrefix(path, "/perya"), nil
	case strings.HasPrefix(path, "/__prefetch/"):
		return p.pickOryx(), trimPrefix(path, "/__prefetch"), nil
	case path == "":
		return nil, "", errors.New("empty request path")
	default:
		return p.pickOryx(), path, nil
	}
}

func (p *edgeProxy) pickOryx() *url.URL {
	if len(p.oryxTargets) == 1 {
		return p.oryxTargets[0]
	}
	index := p.oryxCounter.Add(1)
	return p.oryxTargets[int(index)%len(p.oryxTargets)]
}

func (p *edgeProxy) schedulePrefetch(target *url.URL, playlistPath string, body []byte) int {
	if !p.prefetchOn || p.prefetchSem == nil || p.prefetchBatch <= 0 || len(body) == 0 {
		return 0
	}

	playlistURL := buildRequestURL(target, playlistPath, "")
	lines := bytes.Split(body, []byte("\n"))
	scheduled := 0

	for _, rawLine := range lines {
		line := strings.TrimSpace(string(rawLine))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.Contains(line, ".ts") {
			continue
		}

		segRef, err := url.Parse(line)
		if err != nil {
			continue
		}
		if segRef.IsAbs() && segRef.Host != playlistURL.Host {
			continue
		}

		segURL := playlistURL.ResolveReference(segRef)
		key := cacheKeyForURL(segURL)
		if p.cacheContains(key) {
			continue
		}

		scheduled++
		p.spawnPrefetch(segURL)
		if scheduled >= p.prefetchBatch {
			break
		}
	}

	return scheduled
}

func (p *edgeProxy) spawnPrefetch(segURL *url.URL) {
	if p.prefetchSem == nil {
		return
	}

	p.prefetchSem <- struct{}{}
	go func() {
		defer func() { <-p.prefetchSem }()
		ctx, cancel := context.WithTimeout(context.Background(), p.upstreamDelay)
		defer cancel()
		if _, err := p.fetchAndStore(ctx, segURL, true); err != nil {
			log.Printf("prefetch %s failed: %v", segURL.Redacted(), err)
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
}

func (p *edgeProxy) writeResponse(w http.ResponseWriter, header http.Header, status int, body []byte, cacheStatus, prefetchInfo string) {
	for k, vv := range header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Go-Cache", cacheStatus)
	w.Header().Set("X-Go-Cahce", cacheStatus)
	w.Header().Set("X-Go-Prefetch", prefetchInfo)
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		log.Printf("write response error: %v", err)
	}
}

func sanitizeHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, vv := range src {
		if hopByHopHeaders[textproto.CanonicalMIMEHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	dst.Del("X-Go-Cache")
	dst.Del("X-Go-Cahce")
	dst.Del("X-Go-Prefetch")
	dst.Del("X-Edge-Go")
	dst.Del("Access-Control-Allow-Origin")
	return dst
}

func cacheKeyForURL(u *url.URL) string {
	clone := *u
	clone.User = nil
	clone.Fragment = ""
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func isPlaylistPath(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".m3u8")
}

func isSegmentPath(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".ts")
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
