package main

import (
	"bytes"
	"context"
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto"
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

	// Origin request metrics
	originRequests   atomic.Uint64
	originFailures   atomic.Uint64
	originTimeouts   atomic.Uint64
	originDNSErrors  atomic.Uint64
	originConnErrors atomic.Uint64

	// Request metrics by origin type
	oryxRequests  atomic.Uint64
	oryxFailures  atomic.Uint64
	peryaRequests atomic.Uint64
	peryaFailures atomic.Uint64
	svRequests    atomic.Uint64
	svFailures    atomic.Uint64
	suRequests    atomic.Uint64
	suFailures    atomic.Uint64
	acfRequests   atomic.Uint64
	acfFailures   atomic.Uint64
	wccRequests   atomic.Uint64
	wccFailures   atomic.Uint64
	ukRequests    atomic.Uint64
	ukFailures    atomic.Uint64
	aplRequests   atomic.Uint64
	aplFailures   atomic.Uint64

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

	// Origin request metrics
	OriginRequests    uint64  `json:"origin_requests"`
	OriginFailures    uint64  `json:"origin_failures"`
	OriginFailureRate float64 `json:"origin_failure_rate"`
	OriginTimeouts    uint64  `json:"origin_timeouts"`
	OriginDNSErrors   uint64  `json:"origin_dns_errors"`
	OriginConnErrors  uint64  `json:"origin_conn_errors"`

	// Origin-specific metrics
	OriginStats map[string]OriginMetrics `json:"origin_stats"`

	// Performance metrics
	AvgResponseTime uint64 `json:"avg_response_time_ms"`
	RequestCount    uint64 `json:"request_count"`
}

type OriginMetrics struct {
	Requests    uint64  `json:"requests"`
	Failures    uint64  `json:"failures"`
	FailureRate float64 `json:"failure_rate"`
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

	// Reset request metrics by origin type
	m.oryxRequests.Store(0)
	m.oryxFailures.Store(0)
	m.peryaRequests.Store(0)
	m.peryaFailures.Store(0)
	m.svRequests.Store(0)
	m.svFailures.Store(0)
	m.suRequests.Store(0)
	m.suFailures.Store(0)
	m.acfRequests.Store(0)
	m.acfFailures.Store(0)
	m.wccRequests.Store(0)
	m.wccFailures.Store(0)
	m.ukRequests.Store(0)
	m.ukFailures.Store(0)
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

	log.Printf("edge proxy listening on %s (oryx=%v, perya=%s, sv=%v, su=%v, acf=%v, wcc=%s, uk=%s)", cfg.ListenAddr, cfg.OryxOrigins, cfg.PeryaOrigin, listOriginNames(cfg.SVNamedOrigins), listOriginNames(cfg.SUOrigins), listOriginNames(cfg.ACFOrigins), cfg.WCCOrigin, cfg.UKOrigin)
	log.Printf("metrics endpoint available at %s/metrics", cfg.ListenAddr)
	log.Printf("dashboard available at %s/dashboard", cfg.ListenAddr)
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
	ACFOrigins         map[string]originConfig
	ACFReferer         string
	APLOrigin          string
	WCCOrigin          string
	WCCHost            string
	WCCReferer         string
	WCCSkipTLSVerify   bool
	WCCPlaylistTTL     time.Duration
	WCCPlaylistGrace   time.Duration
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
	PlaylistGrace      time.Duration
	PrefetchWorkers    int
	PrefetchBatch      int
	PrefetchEnabled    bool
	MetricsResetDaily  bool
	MetricsResetTime   string // Format: "HH:MM" (e.g., "00:00" for midnight)
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
	suOrigins := collectSUOrigins(suReferer)
	acfReferer := strings.TrimSpace(os.Getenv("ACF_REFERER"))
	acfOrigins := collectACFOriginsWithDefault(acfReferer)
	aplOrigin := strings.TrimSpace(os.Getenv("APL_ORIGIN"))
	wccOrigin := strings.TrimSpace(os.Getenv("WCC_ORIGIN"))
	wccHost := strings.TrimSpace(os.Getenv("WCC_HOST_HEADER"))
	wccReferer := strings.TrimSpace(os.Getenv("WCC_REFERER"))
	if wccReferer == "" {
		wccReferer = "https://stream.wccgames7.xyz/"
	}
	wccSkipVerify, err := parseBoolEnv("WCC_SKIP_TLS_VERIFY", true)
	if err != nil {
		return nil, err
	}
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

	wccPlaylistTTL := playlistTTL
	playlistGrace := time.Duration(0)
	if raw := strings.TrimSpace(os.Getenv("CACHE_FLEXIBLE")); raw != "" {
		parts := strings.Split(raw, ",")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			sec, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil || sec <= 0 {
				return nil, fmt.Errorf("invalid CACHE_FLEXIBLE primary ttl: %w", err)
			}
			playlistTTL = time.Duration(sec) * time.Second
		}
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			sec, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil || sec < 0 {
				return nil, fmt.Errorf("invalid CACHE_FLEXIBLE grace ttl: %w", err)
			}
			playlistGrace = time.Duration(sec) * time.Second
		}
	}

	wccPlaylistGrace := playlistGrace
	if raw := strings.TrimSpace(os.Getenv("WCC_CACHE_FLEXIBLE")); raw != "" {
		parts := strings.Split(raw, ",")
		if len(parts) > 0 && strings.TrimSpace(parts[0]) != "" {
			sec, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err != nil || sec <= 0 {
				return nil, fmt.Errorf("invalid WCC_CACHE_FLEXIBLE primary ttl: %w", err)
			}
			wccPlaylistTTL = time.Duration(sec) * time.Second
		}
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			sec, err := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err != nil || sec < 0 {
				return nil, fmt.Errorf("invalid WCC_CACHE_FLEXIBLE grace ttl: %w", err)
			}
			wccPlaylistGrace = time.Duration(sec) * time.Second
		}
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
		ACFOrigins:         acfOrigins,
		ACFReferer:         acfReferer,
		APLOrigin:          aplOrigin,
		WCCOrigin:          wccOrigin,
		WCCHost:            wccHost,
		WCCReferer:         wccReferer,
		WCCSkipTLSVerify:   wccSkipVerify,
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
		PlaylistGrace:      playlistGrace,
		WCCPlaylistTTL:     wccPlaylistTTL,
		WCCPlaylistGrace:   wccPlaylistGrace,
		SegmentTTL:         segmentTTL,
		PrefetchWorkers:    prefetchWorkers,
		PrefetchBatch:      prefetchBatch,
		PrefetchEnabled:    prefetchEnabled,
		MetricsResetDaily:  metricsResetDaily,
		MetricsResetTime:   metricsResetTime,
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
	clientStrict     *http.Client
	clientInsecure   *http.Client
	oryxTargets      []*upstreamTarget
	oryxNamed        map[string]*upstreamTarget
	oryxPrefixes     []string
	peryaTarget      *upstreamTarget
	svTargets        map[string]*upstreamTarget
	svPrefixes       []string
	suTargets        map[string]*upstreamTarget
	suPrefixes       []string
	acfTargets       map[string]*upstreamTarget
	acfPrefixes      []string
	aplTarget        *upstreamTarget
	wccTarget        *upstreamTarget
	ukTarget         *upstreamTarget
	primeOrigin      string
	primeReferer     string
	userAgent        string
	oryxCounter      atomic.Uint64
	upstreamDelay    time.Duration
	cache            *ristretto.Cache
	playlistTTL      time.Duration
	playlistGrace    time.Duration
	wccPlaylistTTL   time.Duration
	wccPlaylistGrace time.Duration
	segmentTTL       time.Duration
	prefetchBatch    int
	prefetchSem      chan struct{}
	prefetchOn       bool
	metrics          *metrics
	revalidateMap    sync.Map
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
			referer := spec.Referer
			if referer == "" {
				referer = cfg.SUReferer
			}
			suTargets[name] = buildTarget(suURL, spec.Host, "", referer, cfg.SUSkipTLSVerify)
			suPrefixes = append(suPrefixes, name)
		}
		sortNamedPrefixes(suPrefixes)
	}

	var acfTargets map[string]*upstreamTarget
	var acfPrefixes []string
	if len(cfg.ACFOrigins) > 0 {
		acfTargets = make(map[string]*upstreamTarget, len(cfg.ACFOrigins))
		for name, spec := range cfg.ACFOrigins {
			acfURL, err := buildURL(spec.Origin)
			if err != nil {
				return nil, fmt.Errorf("invalid ACF origin %s: %w", name, err)
			}
			acfTargets[name] = buildTarget(acfURL, spec.Host, "", spec.Referer, false)
			acfPrefixes = append(acfPrefixes, name)
		}
		sortNamedPrefixes(acfPrefixes)
	}

	var aplTarget *upstreamTarget
	if cfg.APLOrigin != "" {
		aplURL, err := buildURL(cfg.APLOrigin)
		if err != nil {
			return nil, fmt.Errorf("invalid APL origin: %w", err)
		}
		aplTarget = buildTarget(aplURL, "", "", "", false)
	}

	var wccTarget *upstreamTarget
	if cfg.WCCOrigin != "" {
		wccURL, err := buildURL(cfg.WCCOrigin)
		if err != nil {
			return nil, fmt.Errorf("invalid WCC origin: %w", err)
		}
		wccTarget = buildTarget(wccURL, cfg.WCCHost, "", cfg.WCCReferer, cfg.WCCSkipTLSVerify)
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
		clientStrict:     clientStrict,
		clientInsecure:   clientInsecure,
		oryxTargets:      oryxTargets,
		oryxNamed:        oryxNamed,
		oryxPrefixes:     oryxPrefixes,
		peryaTarget:      peryaTarget,
		svTargets:        svTargets,
		svPrefixes:       svPrefixes,
		suTargets:        suTargets,
		suPrefixes:       suPrefixes,
		acfTargets:       acfTargets,
		acfPrefixes:      acfPrefixes,
		aplTarget:        aplTarget,
		wccTarget:        wccTarget,
		ukTarget:         ukTarget,
		primeOrigin:      cfg.PrimeOrigin,
		primeReferer:     cfg.PrimeReferer,
		userAgent:        cfg.UpstreamUserAgent,
		upstreamDelay:    cfg.UpstreamTimeout,
		cache:            cache,
		playlistTTL:      cfg.PlaylistTTL,
		playlistGrace:    cfg.PlaylistGrace,
		wccPlaylistTTL:   cfg.WCCPlaylistTTL,
		wccPlaylistGrace: cfg.WCCPlaylistGrace,
		segmentTTL:       cfg.SegmentTTL,
		prefetchBatch:    cfg.PrefetchBatch,
		prefetchSem:      prefetchSem,
		prefetchOn:       cfg.PrefetchEnabled && cfg.PrefetchWorkers > 0 && cfg.PrefetchBatch > 0,
		metrics:          newMetrics(),
	}, nil
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

	reqURL := buildRequestURL(target.base, upstreamPath, r.URL.RawQuery)
	cacheKey := cacheKeyForURL(&reqURL)

	if entry, prefetched, ok, stale := p.getFromCache(cacheKey); ok {
		p.metrics.cacheHits.Add(1)
		cacheMark := "HIT"
		if stale {
			cacheMark = "STALE"
			p.scheduleRevalidate(target, &reqURL)
		}
		p.writeResponse(w, entry.header, entry.status, entry.body, cacheMark, boolToPrefetch(prefetched))
		p.updateResponseTime(start)
		return
	}

	p.metrics.cacheMisses.Add(1)
	resp, err := p.fetchAndStore(ctx, &reqURL, target, false)
	if err != nil {
		p.recordOriginFailure(target, err)
		log.Printf("upstream request to %s failed: %v", reqURL.Redacted(), err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}

	prefetchCount := 0
	if shouldCache(resp.status) && isPlaylistPath(upstreamPath) {
		prefetchCount = p.schedulePrefetch(target, upstreamPath, resp.body)
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

	if target == p.wccTarget && resp != nil && resp.status == 525 {
		// Retry once to smooth over transient TLS issues at the origin.
		if ctx.Err() == nil {
			log.Printf("wcc origin returned 525 for %s, retrying once", reqURL.Redacted())
			if retryResp, retryErr := p.fetchFromOrigin(ctx, reqURL, target); retryErr == nil {
				resp = retryResp
			}
		}

		// Serve stale cache if retry also hit 525 (prevents visible errors).
		if resp != nil && resp.status == 525 {
			if cached, prefetchedEntry, ok, _ := p.getFromCache(cacheKeyForURL(reqURL)); ok {
				header := cloneHeader(cached.header)
				header.Set("X-Go-Cache", "STALE")
				header.Set("X-Go-Prefetch", boolToPrefetch(prefetchedEntry))
				header.Set("X-Go-Fallback", "wcc-stale-cache")
				return &cachedResponse{status: cached.status, header: header, body: cached.body}, nil
			}
		}
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

	ttl := p.ttlForPath(reqURL.Path)
	grace := time.Duration(0)
	if isPlaylistPath(reqURL.Path) {
		ttl = p.playlistTTL
		grace = p.playlistGrace
		if target == p.wccTarget {
			ttl = p.wccPlaylistTTL
			grace = p.wccPlaylistGrace
		}
	}

	p.storeCacheEntry(reqURL, resp, prefetched, ttl, grace)
	return resp, nil
}

func (p *edgeProxy) fetchFromOrigin(ctx context.Context, reqURL *url.URL, target *upstreamTarget) (*cachedResponse, error) {
	p.metrics.originRequests.Add(1)
	p.incrementOriginRequest(target)

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
	if target.skipTLSVerify && p.clientInsecure != nil {
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
	if target == p.aplTarget {
		header.Del("Set-Cookie") // APL responses should not emit session cookies
	}

	return &cachedResponse{status: resp.StatusCode, header: header, body: body}, nil
}

func (p *edgeProxy) storeCacheEntry(reqURL *url.URL, resp *cachedResponse, prefetched bool, ttl time.Duration, grace time.Duration) {
	if p.cache == nil || !shouldCache(resp.status) {
		return
	}
	if ttl <= 0 {
		return
	}
	storeTTL := ttl
	if grace > 0 {
		storeTTL += grace
	}
	value := &cacheValue{resp: resp, prefetched: prefetched, storedAt: time.Now(), path: reqURL.Path, ttl: ttl, grace: grace}
	p.cache.SetWithTTL(cacheKeyForURL(reqURL), value, 1, storeTTL)
}

func (p *edgeProxy) getFromCache(key string) (*cachedResponse, bool, bool, bool) {
	if p.cache == nil {
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
	if p.cache == nil {
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

	switch {
	case p.isOryxTarget(target):
		p.metrics.oryxRequests.Add(1)
	case target == p.peryaTarget:
		p.metrics.peryaRequests.Add(1)
	case p.isSVTarget(target):
		p.metrics.svRequests.Add(1)
	case p.isSUTarget(target):
		p.metrics.suRequests.Add(1)
	case p.isACFTarget(target):
		p.metrics.acfRequests.Add(1)
	case target == p.wccTarget:
		p.metrics.wccRequests.Add(1)
	case target == p.ukTarget:
		p.metrics.ukRequests.Add(1)
	case target == p.aplTarget:
		p.metrics.aplRequests.Add(1)
	}
}

// incrementOriginFailure increments failure counter for specific origin
func (p *edgeProxy) incrementOriginFailure(target *upstreamTarget) {
	if target == nil || target.base == nil {
		return
	}

	switch {
	case p.isOryxTarget(target):
		p.metrics.oryxFailures.Add(1)
	case target == p.peryaTarget:
		p.metrics.peryaFailures.Add(1)
	case p.isSVTarget(target):
		p.metrics.svFailures.Add(1)
	case p.isSUTarget(target):
		p.metrics.suFailures.Add(1)
	case p.isACFTarget(target):
		p.metrics.acfFailures.Add(1)
	case target == p.wccTarget:
		p.metrics.wccFailures.Add(1)
	case target == p.ukTarget:
		p.metrics.ukFailures.Add(1)
	case target == p.aplTarget:
		p.metrics.aplFailures.Add(1)
	}
}

// Helper methods to identify target types
func (p *edgeProxy) isOryxTarget(target *upstreamTarget) bool {
	for _, t := range p.oryxTargets {
		if t == target {
			return true
		}
	}
	return false
}

func (p *edgeProxy) isSVTarget(target *upstreamTarget) bool {
	for _, t := range p.svTargets {
		if t == target {
			return true
		}
	}
	return false
}

func (p *edgeProxy) isSUTarget(target *upstreamTarget) bool {
	for _, t := range p.suTargets {
		if t == target {
			return true
		}
	}
	return false
}

func (p *edgeProxy) isACFTarget(target *upstreamTarget) bool {
	for _, t := range p.acfTargets {
		if t == target {
			return true
		}
	}
	return false
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

	originStats := map[string]OriginMetrics{
		"oryx": {
			Requests:    m.oryxRequests.Load(),
			Failures:    m.oryxFailures.Load(),
			FailureRate: calculateFailureRate(m.oryxRequests.Load(), m.oryxFailures.Load()),
		},
		"perya": {
			Requests:    m.peryaRequests.Load(),
			Failures:    m.peryaFailures.Load(),
			FailureRate: calculateFailureRate(m.peryaRequests.Load(), m.peryaFailures.Load()),
		},
		"sv": {
			Requests:    m.svRequests.Load(),
			Failures:    m.svFailures.Load(),
			FailureRate: calculateFailureRate(m.svRequests.Load(), m.svFailures.Load()),
		},
		"su": {
			Requests:    m.suRequests.Load(),
			Failures:    m.suFailures.Load(),
			FailureRate: calculateFailureRate(m.suRequests.Load(), m.suFailures.Load()),
		},
		"acf": {
			Requests:    m.acfRequests.Load(),
			Failures:    m.acfFailures.Load(),
			FailureRate: calculateFailureRate(m.acfRequests.Load(), m.acfFailures.Load()),
		},
		"wcc": {
			Requests:    m.wccRequests.Load(),
			Failures:    m.wccFailures.Load(),
			FailureRate: calculateFailureRate(m.wccRequests.Load(), m.wccFailures.Load()),
		},
		"uk": {
			Requests:    m.ukRequests.Load(),
			Failures:    m.ukFailures.Load(),
			FailureRate: calculateFailureRate(m.ukRequests.Load(), m.ukFailures.Load()),
		},
		"apl": {
			Requests:    m.aplRequests.Load(),
			Failures:    m.aplFailures.Load(),
			FailureRate: calculateFailureRate(m.aplRequests.Load(), m.aplFailures.Load()),
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
		return p.playlistTTL
	}
	if isSegmentPath(path) {
		return p.segmentTTL
	}
	return p.segmentTTL
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
	case strings.HasPrefix(path, "/__prefetch/apl"):
		if p.aplTarget == nil {
			return nil, "", errors.New("APL origin not configured")
		}
		translated := trimPrefix(path, "/__prefetch/apl")
		return p.aplTarget, translated, nil
	case strings.HasPrefix(path, "/su"):
		if target, trimmed, ok := matchNamed(path, "", p.suPrefixes, p.suTargets); ok {
			return target, trimmed, nil
		}
		return nil, "", errors.New("SU origin not configured")
	case strings.HasPrefix(path, "/apl"):
		if p.aplTarget == nil {
			return nil, "", errors.New("APL origin not configured")
		}
		translated := trimPrefix(path, "/apl")
		return p.aplTarget, translated, nil
	case strings.HasPrefix(path, "/__prefetch/acf"):
		if target, trimmed, ok := matchNamed(path, "/__prefetch", p.acfPrefixes, p.acfTargets); ok {
			return target, trimmed, nil
		}
		return nil, "", errors.New("ACF origin not configured")
	case strings.HasPrefix(path, "/acf"):
		if target, trimmed, ok := matchNamed(path, "", p.acfPrefixes, p.acfTargets); ok {
			return target, trimmed, nil
		}
		return nil, "", errors.New("ACF origin not configured")
	case strings.HasPrefix(path, "/__prefetch/wcc"):
		if p.wccTarget == nil {
			return nil, "", errors.New("WCC origin not configured")
		}
		return p.wccTarget, trimPrefix(path, "/__prefetch/wcc"), nil
	case strings.HasPrefix(path, "/wcc"):
		if p.wccTarget == nil {
			return nil, "", errors.New("WCC origin not configured")
		}
		return p.wccTarget, trimPrefix(path, "/wcc"), nil
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

		segRef, err := url.Parse(line)
		if err != nil {
			continue
		}
		if segRef.IsAbs() && segRef.Host != playlistURL.Host {
			continue
		}

		segURL := playlistURL.ResolveReference(segRef)
		if !isSegmentPath(segURL.Path) {
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
			log.Printf("prefetch %s failed: %v", segURL.Redacted(), err)
		} else {
			p.metrics.prefetchSuccess.Add(1)
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

func collectSUOrigins(defaultReferer string) map[string]originConfig {
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
		referer := strings.TrimSpace(os.Getenv("SU_REFERER" + suffix))
		if referer == "" {
			referer = defaultReferer
		}
		if entries == nil {
			entries = make(map[string]originConfig)
		}
		entries[name] = originConfig{
			Origin:  origin,
			Host:    host,
			Referer: referer,
		}
	}
	return entries
}

func collectACFOrigins() map[string]originConfig {
	return collectACFOriginsWithDefault("")
}

func collectACFOriginsWithDefault(defaultReferer string) map[string]originConfig {
	var entries map[string]originConfig
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		if !strings.HasPrefix(key, "ACF_ORIGIN") {
			continue
		}
		origin := strings.TrimSpace(parts[1])
		if origin == "" {
			continue
		}
		suffix := strings.TrimPrefix(key, "ACF_ORIGIN")
		nameSuffix := strings.ToLower(strings.TrimSpace(suffix))
		name := "acf"
		if nameSuffix != "" {
			name += nameSuffix
		}
		host := strings.TrimSpace(os.Getenv("ACF_HOST_HEADER" + suffix))
		referer := strings.TrimSpace(os.Getenv("ACF_REFERER" + suffix))
		if referer == "" {
			referer = defaultReferer
		}
		if entries == nil {
			entries = make(map[string]originConfig)
		}
		entries[name] = originConfig{
			Origin:  origin,
			Host:    host,
			Referer: referer,
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
