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
	"sort"
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

	log.Printf("edge proxy listening on %s (oryx=%v, perya=%s, sv=%v, su=%v, uk=%s)", cfg.ListenAddr, cfg.OryxOrigins, cfg.PeryaOrigin, listOriginNames(cfg.SVNamedOrigins), listOriginNames(cfg.SUOrigins), cfg.UKOrigin)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

type config struct {
	ListenAddr         string
	OryxOrigins        []string
	OryxSkipTLSVerify  bool
	PeryaOrigin        string
	PeryaSkipTLSVerify bool
	SVOrigin           string
	SVHost             string
	SVNamedOrigins     map[string]originConfig
	SUOrigins          map[string]originConfig
	SUReferer          string
	SUSkipTLSVerify    bool
	UKOrigin           string
	UKHost             string
	UKReferer          string
	UKSkipTLSVerify    bool
	PrimeHost          string
	PrimeOrigin        string
	PrimeReferer       string
	DisableTLSVerify   bool
	UpstreamTimeout    time.Duration
	UpstreamUserAgent  string
	CacheEntries       int
	PlaylistTTL        time.Duration
	SegmentTTL         time.Duration
	PrefetchWorkers    int
	PrefetchBatch      int
	PrefetchEnabled    bool
}

type upstreamTarget struct {
	base          *url.URL
	hostOverride  string
	originHeader  string
	refererHeader string
	skipTLSVerify bool
}

type originConfig struct {
	Origin string
	Host   string
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
	oryxSkipVerify, err := parseBoolEnv("ORYX_SKIP_TLS_VERIFY", false)
	if err != nil {
		return nil, err
	}

	perya := strings.TrimSpace(os.Getenv("PERYA_SERVER"))
	if perya == "" {
		return nil, errors.New("PERYA_SERVER is required")
	}
	peryaSkipVerify, err := parseBoolEnv("PERYA_SKIP_TLS_VERIFY", false)
	if err != nil {
		return nil, err
	}

	svOrigin := strings.TrimSpace(os.Getenv("SV_ORIGIN"))
	svHost := strings.TrimSpace(os.Getenv("SV_HOST_HEADER"))
	suReferer := strings.TrimSpace(os.Getenv("SU_REFERER"))
	suSkipVerify, err := parseBoolEnv("SU_SKIP_TLS_VERIFY", false)
	if err != nil {
		return nil, err
	}
	suOrigins := collectSUOrigins()
	ukOrigin := strings.TrimSpace(os.Getenv("UK_ORIGIN"))
	ukHost := strings.TrimSpace(os.Getenv("UK_HOST_HEADER"))
	ukReferer := strings.TrimSpace(os.Getenv("UK_REFERER"))
	ukSkipVerify, err := parseBoolEnv("UK_SKIP_TLS_VERIFY", false)
	if err != nil {
		return nil, err
	}
	svNamedOrigins := collectSVOrigins(svOrigin, svHost)

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
		ListenAddr:         getenv("LISTEN_ADDR", ":9000"),
		OryxOrigins:        oryxOrigins,
		OryxSkipTLSVerify:  oryxSkipVerify,
		PeryaOrigin:        perya,
		PeryaSkipTLSVerify: peryaSkipVerify,
		SVOrigin:           svOrigin,
		SVHost:             svHost,
		SVNamedOrigins:     svNamedOrigins,
		SUOrigins:          suOrigins,
		SUReferer:          suReferer,
		SUSkipTLSVerify:    suSkipVerify,
		UKOrigin:           ukOrigin,
		UKHost:             ukHost,
		UKReferer:          ukReferer,
		UKSkipTLSVerify:    ukSkipVerify,
		PrimeHost:          getenv("PRIME_STREAM_HOST", ""),
		PrimeOrigin:        getenv("PRIME_STREAM_ORIGIN", ""),
		PrimeReferer:       getenv("PRIME_STREAM_REFERER", ""),
		DisableTLSVerify:   disableTLS,
		UpstreamTimeout:    timeout,
		UpstreamUserAgent:  getenv("EDGE_USER_AGENT", defaultUserAgent),
		CacheEntries:       cacheEntries,
		PlaylistTTL:        playlistTTL,
		SegmentTTL:         segmentTTL,
		PrefetchWorkers:    prefetchWorkers,
		PrefetchBatch:      prefetchBatch,
		PrefetchEnabled:    prefetchEnabled,
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
	clientInsecure *http.Client
	oryxTargets    []*upstreamTarget
	oryxNamed      map[string]*upstreamTarget
	oryxPrefixes   []string
	peryaTarget    *upstreamTarget
	svTargets      map[string]*upstreamTarget
	svPrefixes     []string
	suTargets      map[string]*upstreamTarget
	suPrefixes     []string
	ukTarget       *upstreamTarget
	primeOrigin    string
	primeReferer   string
	userAgent      string
	oryxCounter    atomic.Uint64
	upstreamDelay  time.Duration
	cache          *ristretto.Cache
	playlistTTL    time.Duration
	segmentTTL     time.Duration
	prefetchBatch  int
	prefetchSem    chan struct{}
	prefetchOn     bool
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

	var oryxTargets []*upstreamTarget
	var oryxNamed map[string]*upstreamTarget
	var oryxPrefixes []string
	for idx, origin := range cfg.OryxOrigins {
		u, err := buildURL(origin)
		if err != nil {
			return nil, fmt.Errorf("invalid ORYX origin %q: %w", origin, err)
		}
		target := buildTarget(u, cfg.PrimeHost, "", "", cfg.OryxSkipTLSVerify)
		oryxTargets = append(oryxTargets, target)
		if oryxNamed == nil {
			oryxNamed = make(map[string]*upstreamTarget, len(cfg.OryxOrigins))
		}
		name := fmt.Sprintf("ps%d", idx+1)
		oryxNamed[name] = target
		oryxPrefixes = append(oryxPrefixes, name)
	}
	sortNamedPrefixes(oryxPrefixes)

	peryaURL, err := buildURL(cfg.PeryaOrigin)
	if err != nil {
		return nil, fmt.Errorf("invalid PERYA origin: %w", err)
	}
	peryaTarget := buildTarget(peryaURL, cfg.PrimeHost, "", "", cfg.PeryaSkipTLSVerify)

	var svTargets map[string]*upstreamTarget
	var svPrefixes []string
	if len(cfg.SVNamedOrigins) > 0 {
		svTargets = make(map[string]*upstreamTarget, len(cfg.SVNamedOrigins))
		for name, spec := range cfg.SVNamedOrigins {
			svURL, err := buildURL(spec.Origin)
			if err != nil {
				return nil, fmt.Errorf("invalid SV origin %s: %w", name, err)
			}
			svTargets[name] = buildTarget(svURL, spec.Host, "", "", false)
			svPrefixes = append(svPrefixes, name)
		}
		sortNamedPrefixes(svPrefixes)
	}

	var suTargets map[string]*upstreamTarget
	var suPrefixes []string
	if len(cfg.SUOrigins) > 0 {
		suTargets = make(map[string]*upstreamTarget, len(cfg.SUOrigins))
		for name, spec := range cfg.SUOrigins {
			suURL, err := buildURL(spec.Origin)
			if err != nil {
				return nil, fmt.Errorf("invalid SU origin %s: %w", name, err)
			}
			suTargets[name] = buildTarget(suURL, spec.Host, "", cfg.SUReferer, cfg.SUSkipTLSVerify)
			suPrefixes = append(suPrefixes, name)
		}
		sortNamedPrefixes(suPrefixes)
	}

	var ukTarget *upstreamTarget
	if cfg.UKOrigin != "" {
		ukURL, err := buildURL(cfg.UKOrigin)
		if err != nil {
			return nil, fmt.Errorf("invalid UK origin: %w", err)
		}
		ukTarget = buildTarget(ukURL, cfg.UKHost, "", cfg.UKReferer, cfg.UKSkipTLSVerify)
	}
	strictTransport := buildTransport(cfg.DisableTLSVerify)
	clientStrict := &http.Client{
		Transport: strictTransport,
		Timeout:   cfg.UpstreamTimeout,
	}
	clientInsecure := clientStrict
	if !cfg.DisableTLSVerify {
		clientInsecure = &http.Client{
			Transport: buildTransport(true),
			Timeout:   cfg.UpstreamTimeout,
		}
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
		clientStrict:   clientStrict,
		clientInsecure: clientInsecure,
		oryxTargets:    oryxTargets,
		oryxNamed:      oryxNamed,
		oryxPrefixes:   oryxPrefixes,
		peryaTarget:    peryaTarget,
		svTargets:      svTargets,
		svPrefixes:     svPrefixes,
		suTargets:      suTargets,
		suPrefixes:     suPrefixes,
		ukTarget:       ukTarget,
		primeOrigin:    cfg.PrimeOrigin,
		primeReferer:   cfg.PrimeReferer,
		userAgent:      cfg.UpstreamUserAgent,
		upstreamDelay:  cfg.UpstreamTimeout,
		cache:          cache,
		playlistTTL:    cfg.PlaylistTTL,
		segmentTTL:     cfg.SegmentTTL,
		prefetchBatch:  cfg.PrefetchBatch,
		prefetchSem:    prefetchSem,
		prefetchOn:     cfg.PrefetchEnabled && cfg.PrefetchWorkers > 0 && cfg.PrefetchBatch > 0,
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

	if target == nil || target.base == nil {
		http.Error(w, "upstream target not configured", http.StatusBadGateway)
		return
	}

	reqURL := buildRequestURL(target.base, upstreamPath, r.URL.RawQuery)
	cacheKey := cacheKeyForURL(&reqURL)

	if entry, prefetched, ok := p.getFromCache(cacheKey); ok {
		p.writeResponse(w, entry.header, entry.status, entry.body, "HIT", boolToPrefetch(prefetched))
		return
	}

	resp, err := p.fetchAndStore(ctx, &reqURL, target, false)
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

func (p *edgeProxy) fetchAndStore(ctx context.Context, reqURL *url.URL, target *upstreamTarget, prefetched bool) (*cachedResponse, error) {
	resp, err := p.fetchFromOrigin(ctx, reqURL, target)
	if err != nil {
		return nil, err
	}
	p.storeCacheEntry(reqURL, resp, prefetched)
	return resp, nil
}

func (p *edgeProxy) fetchFromOrigin(ctx context.Context, reqURL *url.URL, target *upstreamTarget) (*cachedResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
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
	if target != nil && target.skipTLSVerify && p.clientInsecure != nil {
		client = p.clientInsecure
	}

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

func (p *edgeProxy) forwardHeaders(target *upstreamTarget) map[string]string {
	origin := p.primeOrigin
	referer := p.primeReferer
	if target != nil {
		if target.originHeader != "" {
			origin = target.originHeader
		}
		if target.refererHeader != "" {
			referer = target.refererHeader
		}
	}
	return map[string]string{
		"User-Agent":         p.userAgent,
		"Origin":             origin,
		"Referer":            referer,
		"sec-ch-ua":          `"Chromium";v="142", "Google Chrome";v="142", "Not_A Brand";v="99"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"macOS"`,
	}
}

func (p *edgeProxy) selectUpstream(path string) (*upstreamTarget, string, error) {
	switch {
	case strings.HasPrefix(path, "/__prefetch/perya"):
		if p.peryaTarget == nil {
			return nil, "", errors.New("PERYA origin not configured")
		}
		return p.peryaTarget, trimPrefix(path, "/__prefetch"), nil
	case strings.HasPrefix(path, "/perya/"):
		if p.peryaTarget == nil {
			return nil, "", errors.New("PERYA origin not configured")
		}
		return p.peryaTarget, trimPrefix(path, "/perya"), nil
	case strings.HasPrefix(path, "/__prefetch/ps"):
		if target, trimmed, ok := matchNamed(path, "/__prefetch", p.oryxPrefixes, p.oryxNamed); ok {
			return target, trimmed, nil
		}
		return nil, "", errors.New("PS origin not configured")
	case strings.HasPrefix(path, "/ps"):
		if target, trimmed, ok := matchNamed(path, "", p.oryxPrefixes, p.oryxNamed); ok {
			return target, trimmed, nil
		}
		return nil, "", errors.New("PS origin not configured")
	case strings.HasPrefix(path, "/__prefetch/sv"):
		if target, trimmed, ok := p.matchSV(path, "/__prefetch"); ok {
			return target, trimmed, nil
		}
		return nil, "", errors.New("SV origin not configured")
	case strings.HasPrefix(path, "/sv"):
		if target, trimmed, ok := p.matchSV(path, ""); ok {
			return target, trimmed, nil
		}
		return nil, "", errors.New("SV origin not configured")
	case strings.HasPrefix(path, "/__prefetch/su"):
		if target, trimmed, ok := matchNamed(path, "/__prefetch", p.suPrefixes, p.suTargets); ok {
			return target, trimmed, nil
		}
		return nil, "", errors.New("SU origin not configured")
	case strings.HasPrefix(path, "/su"):
		if target, trimmed, ok := matchNamed(path, "", p.suPrefixes, p.suTargets); ok {
			return target, trimmed, nil
		}
		return nil, "", errors.New("SU origin not configured")
	case strings.HasPrefix(path, "/__prefetch/uk"):
		if p.ukTarget == nil {
			return nil, "", errors.New("UK origin not configured")
		}
		return p.ukTarget, trimPrefix(path, "/__prefetch/uk"), nil
	case strings.HasPrefix(path, "/uk/"):
		if p.ukTarget == nil {
			return nil, "", errors.New("UK origin not configured")
		}
		return p.ukTarget, trimPrefix(path, "/uk"), nil
	case strings.HasPrefix(path, "/__prefetch/"):
		return p.pickOryx(), trimPrefix(path, "/__prefetch"), nil
	case path == "":
		return nil, "", errors.New("empty request path")
	default:
		target := p.pickOryx()
		if target == nil {
			return nil, "", errors.New("ORYX origin not configured")
		}
		return target, path, nil
	}
}

func (p *edgeProxy) matchSV(path, base string) (*upstreamTarget, string, bool) {
	return matchNamed(path, base, p.svPrefixes, p.svTargets)
}

func (p *edgeProxy) pickOryx() *upstreamTarget {
	if len(p.oryxTargets) == 0 {
		return nil
	}
	if len(p.oryxTargets) == 1 {
		return p.oryxTargets[0]
	}
	index := p.oryxCounter.Add(1)
	return p.oryxTargets[int(index)%len(p.oryxTargets)]
}

func (p *edgeProxy) schedulePrefetch(target *upstreamTarget, playlistPath string, body []byte) int {
	if target == nil || target.base == nil {
		return 0
	}
	if !p.prefetchOn || p.prefetchSem == nil || p.prefetchBatch <= 0 || len(body) == 0 {
		return 0
	}

	playlistURL := buildRequestURL(target.base, playlistPath, "")
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
		p.spawnPrefetch(target, segURL)
		if scheduled >= p.prefetchBatch {
			break
		}
	}

	return scheduled
}

func (p *edgeProxy) spawnPrefetch(target *upstreamTarget, segURL *url.URL) {
	if p.prefetchSem == nil || target == nil {
		return
	}

	p.prefetchSem <- struct{}{}
	go func() {
		defer func() { <-p.prefetchSem }()
		ctx, cancel := context.WithTimeout(context.Background(), p.upstreamDelay)
		defer cancel()
		if _, err := p.fetchAndStore(ctx, segURL, target, true); err != nil {
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

func sortNamedPrefixes(prefixes []string) {
	if len(prefixes) <= 1 {
		return
	}
	sort.Slice(prefixes, func(i, j int) bool {
		if len(prefixes[i]) == len(prefixes[j]) {
			return prefixes[i] < prefixes[j]
		}
		return len(prefixes[i]) > len(prefixes[j])
	})
}

func collectSVOrigins(defaultOrigin, defaultHost string) map[string]originConfig {
	entries := make(map[string]originConfig)
	addEntry := func(name, origin, host string) {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			return
		}
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			return
		}
		entries[key] = originConfig{
			Origin: origin,
			Host:   strings.TrimSpace(host),
		}
	}
	if strings.TrimSpace(defaultOrigin) != "" {
		addEntry("sv", defaultOrigin, defaultHost)
	}
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		if !strings.HasPrefix(key, "SV_ORIGIN") || key == "SV_ORIGIN" {
			continue
		}
		origin := strings.TrimSpace(parts[1])
		if origin == "" {
			continue
		}
		suffix := strings.TrimPrefix(key, "SV_ORIGIN")
		host := strings.TrimSpace(os.Getenv("SV_HOST_HEADER" + suffix))
		name := "sv" + strings.ToLower(strings.TrimSpace(suffix))
		addEntry(name, origin, host)
	}
	if len(entries) == 0 {
		return nil
	}
	return entries
}

func collectSUOrigins() map[string]originConfig {
	var entries map[string]originConfig
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		if !strings.HasPrefix(key, "SU_ORIGIN") {
			continue
		}
		origin := strings.TrimSpace(parts[1])
		if origin == "" {
			continue
		}
		suffix := strings.TrimPrefix(key, "SU_ORIGIN")
		nameSuffix := strings.ToLower(strings.TrimSpace(suffix))
		name := "su"
		if nameSuffix != "" {
			name += nameSuffix
		}
		host := strings.TrimSpace(os.Getenv("SU_HOST_HEADER" + suffix))
		if entries == nil {
			entries = make(map[string]originConfig)
		}
		entries[name] = originConfig{
			Origin: origin,
			Host:   host,
		}
	}
	return entries
}

func listOriginNames(origins map[string]originConfig) []string {
	if len(origins) == 0 {
		return nil
	}
	names := make([]string, 0, len(origins))
	for name := range origins {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
