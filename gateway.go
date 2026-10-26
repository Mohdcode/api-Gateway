package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "net/http/pprof"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
)

const (
	healthCheckInterval = 10 * time.Second
	healthCheckTimeout  = 2 * time.Second
	cbProbeTTLMillis    = 30_000
)

// Populated at build time with -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

var counter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_request_counter",
		Help: "to count all the http requests",
	}, []string{"method", "status", "service"},
)

var timer = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "http_request_timer",
		Help:    "to calculate the time taken by the request",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "service"},
)

var rateLimited = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gateway_rate_limited_total",
		Help: "Number of requests rejected by rate limiter",
	}, []string{"service", "reason"},
)

var cbState = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gateway_circuit_breaker_state",
		Help: "Circuit breaker state per service (0=closed,1=open,2=half-open)",
	}, []string{"service"},
)

var proxyCanceledCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "gateway_proxy_canceled_total",
	Help: "Count of canceled proxied requests (client canceled).",
})

func init() {
	prometheus.MustRegister(counter, timer, rateLimited, cbState, proxyCanceledCounter)
}

// rateLimitScript implements a token bucket atomically in Redis.
// KEYS[1] = bucket key
// ARGV[1] = rate (tokens/second), ARGV[2] = capacity,
// ARGV[3] = now (unix ms), ARGV[4] = key TTL (ms)
const rateLimitScript = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local bucket = redis.call('HMGET', key, 'tokens', 'last')
local tokens = tonumber(bucket[1])
local last = tonumber(bucket[2])
if tokens == nil then
	tokens = capacity
	last = now
end

local elapsed = math.max(0, now - last) / 1000
tokens = math.min(capacity, tokens + (elapsed * rate))

local allowed = 0
if tokens >= 1 then
	tokens = tokens - 1
	allowed = 1
end

redis.call('HSET', key, 'tokens', tokens, 'last', now)
redis.call('PEXPIRE', key, ttl)
return allowed
`

// cbAllowScript decides whether a request may pass and performs the
// closed -> open -> half-open transition atomically across gateway instances.
// KEYS: state, last failure, probe lock
// ARGV: reset timeout (s), now (unix s), probe TTL (ms)
// Returns {state, allowed}.
const cbAllowScript = `
local stateKey = KEYS[1]
local lastKey = KEYS[2]
local probeKey = KEYS[3]
local reset = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local probeTTL = tonumber(ARGV[3])

local state = tonumber(redis.call('GET', stateKey) or '0')

if state == 0 then
	return {0, 1}
end

if state == 1 then
	local last = tonumber(redis.call('GET', lastKey) or '0')
	if last > 0 and (now - last) >= reset then
		redis.call('SET', stateKey, 2)
		state = 2
	else
		return {1, 0}
	end
end

if redis.call('EXISTS', probeKey) == 0 then
	redis.call('SET', probeKey, '1')
	redis.call('PEXPIRE', probeKey, probeTTL)
	return {2, 1}
end
return {2, 0}
`

// cbReportScript records the outcome of an allowed request.
// KEYS: state, failures, last failure, probe lock
// ARGV: failure threshold, success (1/0), now (unix s)
// Returns the resulting state.
const cbReportScript = `
local stateKey = KEYS[1]
local failsKey = KEYS[2]
local lastKey = KEYS[3]
local probeKey = KEYS[4]
local threshold = tonumber(ARGV[1])
local success = tonumber(ARGV[2])
local now = tonumber(ARGV[3])

redis.call('DEL', probeKey)

if success == 1 then
	redis.call('SET', stateKey, 0)
	redis.call('DEL', failsKey)
	return 0
end

local state = tonumber(redis.call('GET', stateKey) or '0')

if state == 2 then
	redis.call('SET', stateKey, 1)
	redis.call('SET', lastKey, now)
	return 1
end

local fails = redis.call('INCR', failsKey)
if fails >= threshold then
	redis.call('SET', stateKey, 1)
	redis.call('SET', lastKey, now)
	return 1
end

return state
`

type statusRecorder struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (rec *statusRecorder) WriteHeader(code int) {
	if rec.wroteHeader {
		return
	}
	rec.statusCode = code
	rec.wroteHeader = true
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *statusRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

type Config struct {
	Port             string      `yaml:"port"`
	JwtSecret        string      `yaml:"jwt_secret"`
	Redis            RedisConfig `yaml:"redis"`
	Services         []Service   `yaml:"services"`
	MiddlewareConfig ConfigTypes `yaml:"middleware_config"`
}

type RedisConfig struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	DB       int    `yaml:"db"`
}

type ConfigTypes struct {
	ApiKeyAuth ApiConfig `yaml:"auth_apikey"`
}

type ApiConfig struct {
	ValidKeys []string `yaml:"valid_keys"`
}

type Service struct {
	Name       string   `yaml:"name"`
	Route      string   `yaml:"route_pattern"`
	Urls       []string `yaml:"urls"`
	Middleware []string `yaml:"middleware"`

	RateLimitRPS          float64 `yaml:"rate_limit_rps"`
	RateLimitBurst        float64 `yaml:"rate_limit_burst"`
	CBFailureThreshold    int     `yaml:"cb_failure_threshold"`
	CBResetTimeoutSeconds int     `yaml:"cb_reset_timeout_seconds"`

	backends []*backend
	next     uint64
}

type backend struct {
	url     string
	proxy   *httputil.ReverseProxy
	conns   atomic.Int64
	healthy atomic.Bool
}

type MiddlewareWrapper func(next http.Handler) http.Handler

type authConfig struct {
	keys      map[string]bool
	jwtSecret string
}

type Gateway struct {
	services  atomic.Pointer[map[string]*Service]
	auth      atomic.Pointer[authConfig]
	wrappers  map[string]MiddlewareWrapper
	transport *http.Transport
	rdb       *redis.Client
	now       func() time.Time
}

type ctxKey string

const identityKey ctxKey = "gateway.identity"

func hashSecret(s, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(s))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func (g *Gateway) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *Gateway) register(name string, wrapper MiddlewareWrapper) {
	g.wrappers[name] = wrapper
	log.Printf("Registered a middleware %s", name)
}

func (g *Gateway) authAPIKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := g.auth.Load()
		if auth == nil {
			http.Error(w, "Authentication Failed", http.StatusUnauthorized)
			return
		}

		if scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
			token, err := jwt.Parse(strings.TrimSpace(raw), func(token *jwt.Token) (any, error) {
				return []byte(auth.jwtSecret), nil
			}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
			if err == nil && token.Valid {
				subject, _ := token.Claims.GetSubject()
				identity := "jwt:" + subject
				if subject == "" {
					identity = "jwt:" + hashSecret(raw, auth.jwtSecret)
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, identity)))
				return
			}
			log.Printf("invalid bearer token: %v", err)
		}

		apiKey := r.Header.Get("X-API-KEY")
		if apiKey == "" || !auth.keys[apiKey] {
			http.Error(w, "Authentication Failed", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, "key:"+hashSecret(apiKey, auth.jwtSecret))))
	})
}

func (g *Gateway) metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()
		recorder := &statusRecorder{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}
		serviceName := ""
		if service := g.findService(r); service != nil {
			serviceName = service.Name
		}
		next.ServeHTTP(recorder, r)
		counter.WithLabelValues(r.Method, fmt.Sprintf("%d", recorder.statusCode), serviceName).Inc()
		timer.WithLabelValues(r.Method, serviceName).Observe(time.Since(startTime).Seconds())
	})
}

func (g *Gateway) clientIdentity(r *http.Request) string {
	if id, ok := r.Context().Value(identityKey).(string); ok && id != "" {
		return id
	}
	if apiKey := r.Header.Get("X-API-KEY"); apiKey != "" {
		if auth := g.auth.Load(); auth != nil && auth.jwtSecret != "" {
			return "key:" + hashSecret(apiKey, auth.jwtSecret)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "ip:" + r.RemoteAddr
	}
	return "ip:" + host
}

func (g *Gateway) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service := g.findService(r)
		rate := 100.0
		burst := 200.0
		serviceName := "unknown"
		route := ""
		if service != nil {
			serviceName = service.Name
			route = service.Route
			if service.RateLimitRPS > 0 {
				rate = service.RateLimitRPS
			}
			if service.RateLimitBurst > 0 {
				burst = service.RateLimitBurst
			}
		}

		if g.rdb == nil {
			next.ServeHTTP(w, r)
			return
		}

		key := "rl:" + g.clientIdentity(r) + ":" + route
		ttl := int64(math.Max(60_000, (burst/rate)*2_000))
		allowed, err := g.rdb.Eval(r.Context(), rateLimitScript, []string{key},
			rate, burst, g.clock().UnixMilli(), ttl).Int()

		if err != nil {
			// Fail open: Redis being down must not take the whole gateway down.
			log.Printf("rate limiter unavailable, allowing request: %v", err)
			rateLimited.WithLabelValues(serviceName, "redis_error").Inc()
			next.ServeHTTP(w, r)
			return
		}
		if allowed == 0 {
			rateLimited.WithLabelValues(serviceName, "token_bucket").Inc()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) circuitBreakerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service := g.findService(r)
		if service == nil || g.rdb == nil {
			next.ServeHTTP(w, r)
			return
		}

		threshold := service.CBFailureThreshold
		if threshold <= 0 {
			threshold = 5
		}
		resetSeconds := service.CBResetTimeoutSeconds
		if resetSeconds <= 0 {
			resetSeconds = 10
		}

		base := "cb:" + service.Route
		keys := []string{base + ":state", base + ":fails", base + ":last", base + ":probe"}

		res, err := g.rdb.Eval(r.Context(), cbAllowScript,
			[]string{keys[0], keys[2], keys[3]},
			resetSeconds, float64(g.clock().UnixNano())/1e9, cbProbeTTLMillis).Int64Slice()
		if err != nil || len(res) != 2 {
			// Fail open on Redis errors.
			log.Printf("circuit breaker unavailable for %s, allowing request: %v", service.Name, err)
			next.ServeHTTP(w, r)
			return
		}

		state, allowed := res[0], res[1] == 1
		cbState.WithLabelValues(service.Name).Set(float64(state))
		if !allowed {
			http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
			return
		}

		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)

		success := int64(0)
		if rec.statusCode < 500 {
			success = 1
		}
		newState, err := g.rdb.Eval(context.Background(), cbReportScript, keys,
			threshold, success, float64(g.clock().UnixNano())/1e9).Int64()
		if err != nil {
			log.Printf("failed to record circuit breaker outcome for %s: %v", service.Name, err)
			return
		}
		cbState.WithLabelValues(service.Name).Set(float64(newState))
	})
}

func NewGateway(cfg *Config) (*Gateway, error) {
	sharedTransport := &http.Transport{
		MaxIdleConns:          20000,
		MaxIdleConnsPerHost:   5000,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	redisAddr := cfg.Redis.Addr
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	gw := &Gateway{
		wrappers:  make(map[string]MiddlewareWrapper),
		transport: sharedTransport,
		now:       time.Now,
		rdb: redis.NewClient(&redis.Options{
			Addr:     redisAddr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		}),
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := gw.rdb.Ping(pingCtx).Err(); err != nil {
		log.Printf("warning: Redis unavailable at %s: %v (rate limiting and circuit breaking will fail open)", redisAddr, err)
	}

	services, err := gw.buildServices(cfg)
	if err != nil {
		return nil, err
	}
	gw.services.Store(&services)
	gw.auth.Store(newAuthConfig(cfg))

	gw.register("auth_apikey", gw.authAPIKeyMiddleware)
	gw.register("metrics", gw.metricsMiddleware)
	gw.register("rate_limit", gw.rateLimitMiddleware)
	gw.register("circuit_breaker", gw.circuitBreakerMiddleware)

	gw.startHealthChecks()
	return gw, nil
}

func newAuthConfig(cfg *Config) *authConfig {
	keys := make(map[string]bool, len(cfg.MiddlewareConfig.ApiKeyAuth.ValidKeys))
	for _, key := range cfg.MiddlewareConfig.ApiKeyAuth.ValidKeys {
		keys[key] = true
	}
	secret := cfg.JwtSecret
	if secret == "" {
		secret = "default_secret_key"
		log.Println("warning: jwt_secret is not set, using an insecure default")
	}
	return &authConfig{keys: keys, jwtSecret: secret}
}

func (g *Gateway) buildServices(cfg *Config) (map[string]*Service, error) {
	services := make(map[string]*Service, len(cfg.Services))
	for i := range cfg.Services {
		c := &cfg.Services[i]
		if c.Route == "" {
			return nil, fmt.Errorf("service %q has no route_pattern", c.Name)
		}
		if len(c.Urls) == 0 {
			return nil, fmt.Errorf("service %q does not have any url defined", c.Name)
		}
		if _, exists := services[c.Route]; exists {
			return nil, fmt.Errorf("duplicate route_pattern %q", c.Route)
		}

		service := &Service{
			Name:                  c.Name,
			Route:                 c.Route,
			Urls:                  append([]string(nil), c.Urls...),
			Middleware:            append([]string(nil), c.Middleware...),
			RateLimitRPS:          c.RateLimitRPS,
			RateLimitBurst:        c.RateLimitBurst,
			CBFailureThreshold:    c.CBFailureThreshold,
			CBResetTimeoutSeconds: c.CBResetTimeoutSeconds,
		}

		for _, rawURL := range c.Urls {
			target, err := url.Parse(rawURL)
			if err != nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
				return nil, fmt.Errorf("invalid url %q for service %q", rawURL, c.Name)
			}

			backendURL := rawURL
			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.Transport = g.transport
			proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
				if errors.Is(err, context.Canceled) {
					proxyCanceledCounter.Inc()
					return
				}
				log.Printf("proxy error: %v (backend=%s client=%s path=%s)", err, backendURL, req.RemoteAddr, req.URL.Path)
				http.Error(rw, "bad gateway", http.StatusBadGateway)
			}

			b := &backend{url: backendURL, proxy: proxy}
			b.healthy.Store(true)
			service.backends = append(service.backends, b)
		}
		services[service.Route] = service
	}
	return services, nil
}

func (g *Gateway) findService(r *http.Request) *Service {
	services := g.services.Load()
	if services == nil {
		return nil
	}
	var best *Service
	bestLen := -1
	for route, service := range *services {
		if strings.HasPrefix(r.URL.Path, route) && len(route) > bestLen {
			best = service
			bestLen = len(route)
		}
	}
	return best
}

func (g *Gateway) startHealthChecks() {
	g.startHealthChecksWith(context.Background(), healthCheckInterval)
}

func (g *Gateway) startHealthChecksWith(ctx context.Context, interval time.Duration) {
	client := &http.Client{Timeout: healthCheckTimeout}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			services := g.services.Load()
			if services == nil {
				continue
			}
			for _, service := range *services {
				for _, b := range service.backends {
					go checkBackend(client, service, b)
				}
			}
		}
	}()
}

func checkBackend(client *http.Client, service *Service, b *backend) {
	resp, err := client.Get(strings.TrimRight(b.url, "/") + "/health")
	healthy := false
	if err == nil {
		healthy = resp.StatusCode == http.StatusOK
		if resp.Body != nil {
			resp.Body.Close()
		}
	}

	if b.healthy.Swap(healthy) != healthy {
		if healthy {
			log.Printf("backend %s for service %s is healthy again", b.url, service.Name)
		} else {
			log.Printf("health check failed for %s at %s: %v", service.Name, b.url, err)
		}
	}
}

// pickBackend implements least-connections load balancing with a rotating
// tie-break, so equally loaded backends share traffic instead of the first
// one always winning.
func (s *Service) pickBackend() *backend {
	n := len(s.backends)
	if n == 0 {
		return nil
	}

	start := int(atomic.AddUint64(&s.next, 1) - 1)
	var best *backend
	var bestConns int64
	for i := 0; i < n; i++ {
		b := s.backends[(start+i)%n]
		if !b.healthy.Load() {
			continue
		}
		conns := b.conns.Load()
		if best == nil || conns < bestConns {
			best = b
			bestConns = conns
		}
	}
	return best
}

func (g *Gateway) handleGateway(w http.ResponseWriter, r *http.Request) {
	service := g.findService(r)
	if service == nil {
		http.Error(w, "No service found for this url", http.StatusNotFound)
		return
	}

	var curr http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := service.pickBackend()
		if b == nil {
			http.Error(w, "no healthy backend available", http.StatusServiceUnavailable)
			return
		}
		b.conns.Add(1)
		defer b.conns.Add(-1)
		b.proxy.ServeHTTP(w, r)
	})

	for i := len(service.Middleware) - 1; i >= 0; i-- {
		name := service.Middleware[i]
		middleware, ok := g.wrappers[name]
		if !ok {
			log.Printf("middleware with name %s is not present", name)
			continue
		}
		curr = middleware(curr)
	}

	curr.ServeHTTP(w, r)
}

func (g *Gateway) reloadConfig(path string) {
	cfg, err := loadConfigWithEnv(path)
	if err != nil {
		log.Printf("failed to reload config, keeping old configuration: %v", err)
		return
	}

	services, err := g.buildServices(cfg)
	if err != nil {
		log.Printf("failed to reload config, keeping old configuration: %v", err)
		return
	}

	g.services.Store(&services)
	g.auth.Store(newAuthConfig(cfg))
	log.Println("Configuration reloaded successfully")
}

func (g *Gateway) watchConfigWith(ctx context.Context, path string, interval time.Duration) {
	go func() {
		var lastMod time.Time
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			info, err := os.Stat(path)
			if err != nil {
				log.Printf("Error stating config file: %v", err)
				continue
			}
			if info.ModTime().After(lastMod) {
				if !lastMod.IsZero() {
					g.reloadConfig(path)
				}
				lastMod = info.ModTime()
			}
		}
	}()
}

func loadConfigPath(p string) (*Config, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("error loading the config %s: %w", p, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("error parsing config yaml %s: %w", p, err)
	}
	return &cfg, nil
}

// applyEnvOverrides lets container platforms configure the gateway without
// rewriting the mounted config file. Explicit environment values win.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("PORT"); v != "" {
		cfg.Port = v
	}
	if v := os.Getenv("JWT_SECRET"); v != "" {
		cfg.JwtSecret = v
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}
	if v := os.Getenv("REDIS_DB"); v != "" {
		db, err := strconv.Atoi(v)
		if err != nil {
			log.Printf("ignoring invalid REDIS_DB %q: %v", v, err)
		} else {
			cfg.Redis.DB = db
		}
	}
	if v, ok := os.LookupEnv("API_KEYS"); ok {
		keys := make([]string, 0)
		for _, key := range strings.Split(v, ",") {
			if key = strings.TrimSpace(key); key != "" {
				keys = append(keys, key)
			}
		}
		cfg.MiddlewareConfig.ApiKeyAuth.ValidKeys = keys
	}
}

func shutdownDelay() time.Duration {
	raw := os.Getenv("SHUTDOWN_DELAY")
	if raw == "" {
		return 0
	}
	delay, err := time.ParseDuration(raw)
	if err != nil || delay < 0 {
		log.Printf("ignoring invalid SHUTDOWN_DELAY %q", raw)
		return 0
	}
	return delay
}

func loadConfigWithEnv(p string) (*Config, error) {
	cfg, err := loadConfigPath(p)
	if err != nil {
		return nil, err
	}
	applyEnvOverrides(cfg)
	return cfg, nil
}

// Close releases resources held by the gateway.
func (g *Gateway) Close() error {
	if g.rdb == nil {
		return nil
	}
	return g.rdb.Close()
}

func (g *Gateway) healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (g *Gateway) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if g.services.Load() == nil {
		http.Error(w, "configuration not loaded", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

func versionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version":   version,
			"commit":    commit,
			"buildDate": buildDate,
			"goVersion": runtime.Version(),
		})
	}
}

// handler builds the HTTP mux shared by the server and the tests.
func (g *Gateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(g.handleGateway))
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", g.healthzHandler)
	mux.HandleFunc("/readyz", g.readyzHandler)
	mux.HandleFunc("/version", versionHandler())
	return mux
}

func checkHealth(addr string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func probeAddress() string {
	if addr := os.Getenv("HEALTHCHECK_ADDR"); addr != "" {
		return addr
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = ":8080"
	}
	if strings.HasPrefix(port, ":") {
		return "127.0.0.1" + port
	}
	if host, p, err := net.SplitHostPort(port); err == nil {
		_ = host
		return net.JoinHostPort("127.0.0.1", p)
	}
	return "127.0.0.1:" + port
}

func runHealthCheck() int {
	if err := checkHealth(probeAddress()); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		return 1
	}
	return 0
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pprofAddr := os.Getenv("PPROF_ADDR")
	if pprofAddr == "" {
		pprofAddr = ":6060"
	}
	var pprofServer *http.Server
	if !strings.EqualFold(pprofAddr, "off") {
		pprofServer = &http.Server{Addr: pprofAddr, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			log.Printf("pprof listening on %s", pprofAddr)
			if err := pprofServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("pprof server stopped: %v", err)
			}
		}()
	}

	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "config.yaml"
		if flag.NArg() > 0 {
			configPath = flag.Arg(0)
		}
	}
	cfg, err := loadConfigWithEnv(configPath)
	if err != nil {
		log.Printf("%v", err)
		return 1
	}
	log.Printf("starting gateway version=%s commit=%s port=%s", version, commit, cfg.Port)

	gw, err := NewGateway(cfg)
	if err != nil {
		log.Printf("failed to initialize gateway: %v", err)
		return 1
	}
	defer gw.Close()
	gw.watchConfigWith(ctx, configPath, 2*time.Second)

	server := &http.Server{
		Addr:              cfg.Port,
		Handler:           gw.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		if delay := shutdownDelay(); delay > 0 {
			log.Printf("shutdown signal received, waiting %s before draining", delay)
			time.Sleep(delay)
		} else {
			log.Println("shutdown signal received, draining connections")
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
			return 1
		}
		if pprofServer != nil {
			_ = pprofServer.Shutdown(shutdownCtx)
		}
		log.Println("gateway stopped")
		return 0
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("error starting the server on port %s: %v", cfg.Port, err)
			return 1
		}
		return 0
	}
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit")
	flag.Parse()
	if *healthcheck {
		os.Exit(runHealthCheck())
	}
	os.Exit(run())
}
