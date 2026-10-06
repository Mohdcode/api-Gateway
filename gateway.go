package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "net/http/pprof"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
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

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.statusCode = code
	rec.ResponseWriter.WriteHeader(code)
}

type Config struct {
	Port             string      `yaml:"port"`
	Services         []Service   `yaml:"services"`
	MiddlewareConfig ConfigTypes `yaml:"middleware_config"`
	JwtSecret        string      `yaml:"jwt_secret"`
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
	proxies    []*httputil.ReverseProxy
	count      uint64
	healthy    sync.Map // map[string]bool
	activeConns sync.Map // map[string]*int64

	// tuning values
	RateLimitRPS          float64 `yaml:"rate_limit_rps"`
	RateLimitBurst        float64 `yaml:"rate_limit_burst"`
	CBFailureThreshold    int     `yaml:"cb_failure_threshold"`
	CBResetTimeoutSeconds int     `yaml:"cb_reset_timeout_seconds"`
}

type MiddlewareWrapper func(next http.Handler) http.Handler

type TokenBucket struct {
	mu       sync.Mutex
	tokens   float64
	last     time.Time
	rate     float64
	capacity float64
}

func NewTokenBucket(rate, capacity float64) *TokenBucket {
	return &TokenBucket{
		tokens:   capacity,
		last:     time.Now(),
		rate:     rate,
		capacity: capacity,
	}
}

func (tb *TokenBucket) TryConsume() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(tb.last).Seconds()
	tb.last = now
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
	if tb.tokens >= 1.0 {
		tb.tokens -= 1.0
		return true
	}
	return false
}

type CBState int

const (
	CBClosed CBState = iota
	CBOpen
	CBHalfOpen
)

type CircuitBreaker struct {
	mu            sync.Mutex
	state         CBState
	failures      int
	lastFailure   time.Time
	failureThresh int
	resetTimeout  time.Duration
	halfProbes    int
}

func NewCircuitBreaker(failureThresh int, resetTimeoutSeconds int) *CircuitBreaker {
	if failureThresh <= 0 {
		failureThresh = 5
	}
	if resetTimeoutSeconds <= 0 {
		resetTimeoutSeconds = 10
	}
	return &CircuitBreaker{
		state:         CBClosed,
		failures:      0,
		failureThresh: failureThresh,
		resetTimeout:  time.Duration(resetTimeoutSeconds) * time.Second,
		halfProbes:    1,
	}
}

func (cb *CircuitBreaker) AllowRequest() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := time.Now()

	switch cb.state {
	case CBClosed:
		return true
	case CBOpen:
		if now.Sub(cb.lastFailure) >= cb.resetTimeout {
			cb.state = CBHalfOpen
			cb.halfProbes = 1
			return true
		}
		return false
	case CBHalfOpen:
		if cb.halfProbes > 0 {
			cb.halfProbes--
			return true
		}
		return false
	default:
		return false
	}
}

func (cb *CircuitBreaker) Report(success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if success {
		cb.failures = 0
		cb.state = CBClosed
		return
	}
	cb.failures++
	cb.lastFailure = time.Now()
	if cb.failures >= cb.failureThresh {
		cb.state = CBOpen
	}
	if cb.state == CBHalfOpen {
		cb.state = CBOpen
	}
}

func (cb *CircuitBreaker) State() CBState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.state
}

type Gateway struct {
	Ser             map[string]*Service
	wrappers        map[string]MiddlewareWrapper
	apikeys         map[string]bool
	rateLimiters    sync.Map
	circuitBreakers map[string]*CircuitBreaker
	transport       *http.Transport
	rdb             *redis.Client
	jwtSecret       string
}

func (g *Gateway) register(name string, wrapper MiddlewareWrapper) {
	g.wrappers[name] = wrapper
	log.Printf("Registered a middleware %s", name)
}

func (g *Gateway) authAPIKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try JWT first
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
			
			token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
				return []byte(g.jwtSecret), nil 
			})

			if err == nil && token.Valid {
				next.ServeHTTP(w, r)
				return
			}
		}

		// Fallback to API Key
		apiKey := r.Header.Get("X-API-KEY")
		if _, valid := g.apikeys[apiKey]; !valid {
			log.Printf("Authentication failed, API KEY %s not found", apiKey)
			http.Error(w, "Authentication Failed", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
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
		service := g.findService(r)
		if service != nil {
			serviceName = service.Name
		}
		next.ServeHTTP(recorder, r)
		duration := time.Since(startTime).Seconds()
		counter.WithLabelValues(r.Method, fmt.Sprintf("%d", recorder.statusCode), serviceName).Inc()
		timer.WithLabelValues(r.Method, serviceName).Observe(duration)
	})
}

func (g *Gateway) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service := g.findService(r)

		apiKey := r.Header.Get("X-API-KEY")
		key := apiKey
		if key == "" {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				key = r.RemoteAddr
			} else {
				key = host
			}
		}

		rate := 100.0
		burst := 200.0
		if service != nil {
			if service.RateLimitRPS > 0 {
				rate = service.RateLimitRPS
			}
			if service.RateLimitBurst > 0 {
				burst = service.RateLimitBurst
			}
		}

		mapKey := "rl:" + key
		if service != nil {
			mapKey = mapKey + ":" + service.Route
		}

		ctx := r.Context()
		now := time.Now().Unix()
		
		// Lua script for atomic Token Bucket in Redis
		script := `
		local key = KEYS[1]
		local rate = tonumber(ARGV[1])
		local capacity = tonumber(ARGV[2])
		local now = tonumber(ARGV[3])
		
		local bucket = redis.call("HMGET", key, "tokens", "last")
		local tokens = tonumber(bucket[1]) or capacity
		local last = tonumber(bucket[2]) or now
		
		local elapsed = math.max(0, now - last)
		tokens = math.min(capacity, tokens + (elapsed * rate))
		
		if tokens >= 1 then
			tokens = tokens - 1
			redis.call("HMSET", key, "tokens", tokens, "last", now)
			return 1
		else
			return 0
		end
		`
		
		allowed, err := g.rdb.Eval(ctx, script, []string{mapKey}, rate, burst, now).Int()
		if err != nil || allowed == 0 {
			serviceName := "unknown"
			if service != nil {
				serviceName = service.Name
			}
			rateLimited.WithLabelValues(serviceName, "redis_token_bucket").Inc()
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) circuitBreakerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service := g.findService(r)
		if service == nil {
			next.ServeHTTP(w, r)
			return
		}
		
		cbKey := "cb:" + service.Route
		ctx := r.Context()
		
		// Distributed state check in Redis
		state, err := g.rdb.Get(ctx, cbKey).Int()
		if err != nil {
			state = int(CBClosed) // Default to closed if not found
		}
		
		cbState.WithLabelValues(service.Name).Set(float64(state))

		if state == int(CBOpen) {
			// Check if timeout has passed to allow probe
			lastFail, _ := g.rdb.Get(ctx, cbKey+":last").Time()
			if time.Since(lastFail) < time.Duration(service.CBResetTimeoutSeconds)*time.Second {
				log.Printf("circuit open for service %s, rejecting request", service.Name)
				http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			// Transition to Half-Open for this request
			state = int(CBHalfOpen)
		}

		rec := &statusRecorder{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}
		next.ServeHTTP(rec, r)
		
		if rec.statusCode >= 500 {
			// Fail: Increment failures and potentially open circuit
			fails, _ := g.rdb.Incr(ctx, cbKey+":fails").Result()
			if fails >= int64(service.CBFailureThreshold) {
				g.rdb.Set(ctx, cbKey, int(CBOpen), 0)
				g.rdb.Set(ctx, cbKey+":last", time.Now(), 0)
			}
			// If we were half-open and failed, go back to open
			if state == int(CBHalfOpen) {
				g.rdb.Set(ctx, cbKey, int(CBOpen), 0)
				g.rdb.Set(ctx, cbKey+":last", time.Now(), 0)
			}
		} else {
			// Success: Close circuit and reset failures
			g.rdb.Set(ctx, cbKey, int(CBClosed), 0)
			g.rdb.Del(ctx, cbKey+":fails")
		}
		
		cbState.WithLabelValues(service.Name).Set(float64(state))
	})
}

func NewGateway(cfg *Config) *Gateway {
	sharedTransport := &http.Transport{
		MaxIdleConns:          20000,
		MaxIdleConnsPerHost:   5000,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})

	gw := &Gateway{
		Ser:             make(map[string]*Service),
		wrappers:        make(map[string]MiddlewareWrapper),
		apikeys:         make(map[string]bool),
		circuitBreakers: make(map[string]*CircuitBreaker),
		transport:       sharedTransport,
		rdb:             rdb,
		jwtSecret:       cfg.JwtSecret,
	}
	if gw.jwtSecret == "" {
		gw.jwtSecret = "default_secret_key"
	}
	for _, key := range cfg.MiddlewareConfig.ApiKeyAuth.ValidKeys {
		gw.apikeys[key] = true
	}
	gw.register("auth_apikey", gw.authAPIKeyMiddleware)
	gw.register("metrics", gw.metricsMiddleware)
	gw.register("rate_limit", gw.rateLimitMiddleware)
	gw.register("circuit_breaker", gw.circuitBreakerMiddleware)

	for i := range cfg.Services {
		currSer := &cfg.Services[i]
		if len(currSer.Urls) == 0 {
			log.Printf("service %s does not have any url defined", currSer.Name)
			return nil
		}
		for j := 0; j < len(currSer.Urls); j++ {
			target, err := url.Parse(currSer.Urls[j])
			if err != nil {
				log.Printf("invalid url %s for service %s: %v", currSer.Urls[j], currSer.Name, err)
				return nil
			}
			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.Transport = sharedTransport
			proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
				if err == context.Canceled {
					proxyCanceledCounter.Inc()
					return
				}
				log.Printf("proxy error: %v (backend=%s client=%s path=%s)", err, target.String(), req.RemoteAddr, req.URL.Path)
				http.Error(rw, "bad gateway", http.StatusBadGateway)
			}
			currSer.proxies = append(currSer.proxies, proxy)
			currSer.healthy.Store(currSer.Urls[j], true) // Assume healthy initially
			
			var connCount int64
			currSer.activeConns.Store(currSer.Urls[j], &connCount)
		}
		gw.Ser[currSer.Route] = currSer
	}

	gw.startHealthChecks()
	return gw
}

func (g *Gateway) findService(r *http.Request) *Service {
	for route, service := range g.Ser {
		if strings.HasPrefix(r.URL.Path, route) {
			return service
		}
	}
	return nil
}

func (g *Gateway) startHealthChecks() {
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for range ticker.C {
			for _, service := range g.Ser {
				for _, url := range service.Urls {
					go func(s *Service, targetUrl string) {
						client := http.Client{Timeout: 2 * time.Second}
						resp, err := client.Get(targetUrl + "/health")
						if err != nil || resp.StatusCode != http.StatusOK {
							s.healthy.Store(targetUrl, false)
							log.Printf("Health check failed for %s at %s", s.Name, targetUrl)
						} else {
							s.healthy.Store(targetUrl, true)
						}
						if resp != nil {
							resp.Body.Close()
						}
					}(service, url)
				}
			}
		}
	}()
}

func (s *Service) NewProxy() *httputil.ReverseProxy {
	// Implement Least Connections Load Balancing
	var bestProxyIndex = -1
	var minConns int64 = -1

	numProxies := len(s.proxies)
	for i := 0; i < numProxies; i++ {
		targetUrl := s.Urls[i]
		
		// Only consider healthy backends
		if val, ok := s.healthy.Load(targetUrl); ok && val.(bool) == false {
			continue
		}

		if connsVal, ok := s.activeConns.Load(targetUrl); ok {
			conns := atomic.LoadInt64(connsVal.(*int64))
			if minConns == -1 || conns < minConns {
				minConns = conns
				bestProxyIndex = i
			}
		}
	}

	if bestProxyIndex == -1 {
		// Fallback to first proxy if all unhealthy or error
		return s.proxies[0]
	}

	return s.proxies[bestProxyIndex]
}

// Wrapper to track active connections
func (g *Gateway) trackConnections(service *Service, proxy *httputil.ReverseProxy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetUrl := ""
		// Find which URL this proxy corresponds to (simplified for this implementation)
		// In a full implementation, the proxy would be wrapped in a struct containing its URL
		for i, p := range service.proxies {
			if p == proxy {
				targetUrl = service.Urls[i]
				break
			}
		}

		if targetUrl == "" {
			proxy.ServeHTTP(w, r)
			return
		}

		if connsVal, ok := service.activeConns.Load(targetUrl); ok {
			atomic.AddInt64(connsVal.(*int64), 1)
			defer atomic.AddInt64(connsVal.(*int64), -1)
		}

		proxy.ServeHTTP(w, r)
	})
}

func (g *Gateway) handleGateway(w http.ResponseWriter, r *http.Request) {
	service := g.findService(r)
	if service == nil {
		log.Printf("No service found for the url %s", r.URL.Path)
		http.Error(w, "No service found for this url", http.StatusNotFound)
		return
	}

	proxy := service.NewProxy()
	var curr http.Handler = g.trackConnections(service, proxy)
	
	for i := len(service.Middleware) - 1; i >= 0; i-- {
		middlewareName := service.Middleware[i]
		middleware, ok := g.wrappers[middlewareName]
		if !ok {
			log.Printf("middleware with name %s is not present", middlewareName)
			continue
		}
		curr = middleware(curr)
	}

	log.Printf("forwarding the request of %s path to the service %s with %d middlewares", r.URL.Path, service.Name, len(service.Middleware))
	curr.ServeHTTP(w, r)
}

func (g *Gateway) reloadConfig(path string) {
	newCfg := loadConfigPath(path)
	if newCfg == nil {
		log.Println("Failed to reload config, keeping old configuration")
		return
	}

	log.Println("Reloading configuration...")

	// Update API Keys
	newKeys := make(map[string]bool)
	for _, key := range newCfg.MiddlewareConfig.ApiKeyAuth.ValidKeys {
		newKeys[key] = true
	}
	g.apikeys = newKeys

	// Update Services
	for route, service := range g.Ser {
		var found *Service
		for i := range newCfg.Services {
			if newCfg.Services[i].Route == route {
				found = &newCfg.Services[i]
				break
			}
		}

		if found != nil {
			service.RateLimitRPS = found.RateLimitRPS
			service.RateLimitBurst = found.RateLimitBurst
			service.CBFailureThreshold = found.CBFailureThreshold
			service.CBResetTimeoutSeconds = found.CBResetTimeoutSeconds
			service.Middleware = found.Middleware
		}
	}
	log.Println("Configuration reloaded successfully")
}

func (g *Gateway) watchConfig(path string) {
	go func() {
		var lastMod time.Time
		for {
			info, err := os.Stat(path)
			if err != nil {
				log.Printf("Error stating config file: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}

			if info.ModTime().After(lastMod) {
				if !lastMod.IsZero() {
					g.reloadConfig(path)
				}
				lastMod = info.ModTime()
			}
			time.Sleep(2 * time.Second)
		}
	}()
}

func loadConfigPath(p string) *Config {
	var cfg Config
	data, err := os.ReadFile(p)
	if err != nil {
		log.Fatalf("error loading the config %s: %v", p, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("error parsing config yaml: %v", err)
	}
	return &cfg
}

func main() {
	go func() {
		log.Println("pprof listening on :6060")
		log.Println(http.ListenAndServe(":6060", nil))
	}()

	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}
	cfg := loadConfigPath(configPath)
	log.Printf("port %s", cfg.Port)
	gw := NewGateway(cfg)
	if gw == nil {
		log.Fatal("failed to initialize gateway")
	}
	gw.watchConfig(configPath)
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(gw.handleGateway))
	mux.Handle("/metrics", promhttp.Handler())
	server := &http.Server{
		Addr:    cfg.Port,
		Handler: mux,
	}
	err := server.ListenAndServe()
	if err != nil {
		log.Fatalf("error starting the server on port %s: %v", cfg.Port, err)
	}
}
