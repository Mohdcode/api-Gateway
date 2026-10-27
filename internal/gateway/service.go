package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

func NewGateway(cfg *Config) (*Gateway, error) {
	if cfg == nil {
		return nil, fmt.Errorf("gateway config must not be nil")
	}
	if err := validateJWTSecret(cfg.JwtSecret); err != nil {
		return nil, err
	}

	sharedTransport := &http.Transport{
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   128,
		MaxConnsPerHost:       512,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())

	redisAddr := cfg.Redis.Addr
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	gw := &Gateway{
		wrappers:     make(map[string]MiddlewareWrapper),
		transport:    sharedTransport,
		now:          time.Now,
		probeTimeout: cbProbeTimeout,
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
		rdb: redis.NewClient(&redis.Options{
			Addr:         redisAddr,
			Password:     cfg.Redis.Password,
			DB:           cfg.Redis.DB,
			MaxRetries:   1,
			DialTimeout:  time.Second,
			ReadTimeout:  time.Second,
			WriteTimeout: time.Second,
			PoolTimeout:  time.Second,
		}),
	}

	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := gw.rdb.Ping(pingCtx).Err(); err != nil {
		log.Printf("warning: Redis unavailable at %s: %v (rate limiting and circuit breaking will fail open)", redisAddr, err)
	}

	services, err := gw.buildServices(cfg)
	if err != nil {
		workerCancel()
		_ = gw.rdb.Close()
		sharedTransport.CloseIdleConnections()
		return nil, err
	}
	gw.storeConfiguration(services, newAuthConfig(cfg))
	// Do not serve through unverified backends. The bounded initial check gives
	// readiness and routing a known state before NewGateway returns.
	checkAllBackends(gw.workerCtx, gw.workerCtx, gw.config.Load(), &http.Client{Timeout: healthCheckTimeout})

	gw.register("auth_apikey", gw.authAPIKeyMiddleware)
	gw.register("metrics", gw.metricsMiddleware)
	gw.register("rate_limit", gw.rateLimitMiddleware)
	gw.register("circuit_breaker", gw.circuitBreakerMiddleware)

	gw.startHealthChecks()
	return gw, nil
}

func (g *Gateway) storeConfiguration(services map[string]*Service, auth *authConfig) {
	g.config.Store(&configSnapshot{services: services, auth: auth})
}

func (g *Gateway) configurationForRequest(r *http.Request) *configSnapshot {
	if r != nil {
		if snapshot, ok := r.Context().Value(configSnapshotKey).(*configSnapshot); ok {
			return snapshot
		}
	}
	return g.config.Load()
}

func validateJWTSecret(secret string) error {
	if len(strings.TrimSpace(secret)) < 32 {
		return fmt.Errorf("jwt_secret must be at least 32 bytes; provide a strong random value in config or JWT_SECRET")
	}
	return nil
}

func newAuthConfig(cfg *Config) *authConfig {
	keys := make(map[string]bool, len(cfg.MiddlewareConfig.ApiKeyAuth.ValidKeys))
	for _, key := range cfg.MiddlewareConfig.ApiKeyAuth.ValidKeys {
		keys[key] = true
	}
	return &authConfig{keys: keys, jwtSecret: cfg.JwtSecret, jwtIssuer: cfg.JwtIssuer, jwtAudience: cfg.JwtAudience}
}

func supportedMiddleware(name string) bool {
	switch name {
	case "metrics", "auth_apikey", "rate_limit", "circuit_breaker":
		return true
	default:
		return false
	}
}

func (g *Gateway) buildServices(cfg *Config) (map[string]*Service, error) {
	services := make(map[string]*Service, len(cfg.Services))
	names := make(map[string]struct{}, len(cfg.Services))
	for i := range cfg.Services {
		c := &cfg.Services[i]
		if strings.TrimSpace(c.Name) == "" {
			return nil, fmt.Errorf("service at index %d has no name", i)
		}
		if _, exists := names[c.Name]; exists {
			return nil, fmt.Errorf("duplicate service name %q", c.Name)
		}
		names[c.Name] = struct{}{}
		if err := validateRoute(c.Route); err != nil {
			return nil, fmt.Errorf("service %q: %w", c.Name, err)
		}
		if len(c.Urls) == 0 {
			return nil, fmt.Errorf("service %q does not have any url defined", c.Name)
		}
		if math.IsNaN(c.RateLimitRPS) || math.IsInf(c.RateLimitRPS, 0) || c.RateLimitRPS < 0 ||
			math.IsNaN(c.RateLimitBurst) || math.IsInf(c.RateLimitBurst, 0) || c.RateLimitBurst < 0 {
			return nil, fmt.Errorf("service %q has invalid rate-limit settings", c.Name)
		}
		rate, burst := c.RateLimitRPS, c.RateLimitBurst
		if rate == 0 {
			rate = 100
		}
		if burst == 0 {
			burst = 200
		}
		if burst/rate > float64(int64(^uint64(0)>>1))/2000 {
			return nil, fmt.Errorf("service %q rate-limit TTL exceeds supported range", c.Name)
		}
		if c.CBFailureThreshold < 0 || c.CBResetTimeoutSeconds < 0 {
			return nil, fmt.Errorf("service %q has invalid circuit-breaker settings", c.Name)
		}
		for _, name := range c.Middleware {
			if !supportedMiddleware(name) {
				return nil, fmt.Errorf("service %q uses unsupported middleware %q", c.Name, name)
			}
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
			if err != nil || target.Host == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" ||
				(target.Scheme != "http" && target.Scheme != "https") {
				return nil, fmt.Errorf("invalid backend URL for service %q", c.Name)
			}

			backendURL := rawURL
			healthTarget := *target
			healthTarget.Path = path.Join(target.Path, "health")
			healthTarget.RawPath = ""
			healthURL := healthTarget.String()
			proxy := httputil.NewSingleHostReverseProxy(target)
			proxy.Transport = g.transport
			proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
				if errors.Is(err, context.Canceled) {
					proxyCanceledCounter.Inc()
					return
				}
				log.Printf("proxy error: %v (backend_host=%s client=%s path=%s)", err, target.Host, req.RemoteAddr, req.URL.Path)
				http.Error(rw, "bad gateway", http.StatusBadGateway)
			}

			b := &backend{url: backendURL, healthURL: healthURL, proxy: proxy}
			service.backends = append(service.backends, b)
		}
		services[service.Route] = service
	}
	return services, nil
}

func validateRoute(route string) error {
	if route == "" {
		return fmt.Errorf("has no route_pattern")
	}
	if !strings.HasPrefix(route, "/") || strings.ContainsAny(route, "?#") || path.Clean(route) != route || (route != "/" && strings.HasSuffix(route, "/")) {
		return fmt.Errorf("has invalid route_pattern %q; use a clean absolute path without a trailing slash", route)
	}
	return nil
}

func (g *Gateway) findService(r *http.Request) *Service {
	snapshot := g.configurationForRequest(r)
	if snapshot == nil {
		return nil
	}
	var best *Service
	bestLen := -1
	for route, service := range snapshot.services {
		matches := route == "/" && strings.HasPrefix(r.URL.Path, "/")
		if route != "/" {
			matches = r.URL.Path == route || strings.HasPrefix(r.URL.Path, route+"/")
		}
		if matches && len(route) > bestLen {
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
	if interval <= 0 {
		interval = healthCheckInterval
	}
	client := &http.Client{Timeout: healthCheckTimeout}
	workerCtx := g.workerCtx
	if workerCtx == nil {
		workerCtx = ctx
	}
	g.workerWG.Add(1)
	go func() {
		defer g.workerWG.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-workerCtx.Done():
				return
			case <-ticker.C:
			}
			checkAllBackends(workerCtx, ctx, g.config.Load(), client)
		}
	}()
}

func checkAllBackends(workerCtx, callerCtx context.Context, snapshot *configSnapshot, client *http.Client) {
	if snapshot == nil {
		return
	}
	type target struct {
		service *Service
		backend *backend
	}
	jobs := make(chan target)
	workers := 16
	total := 0
	for _, service := range snapshot.services {
		total += len(service.backends)
	}
	if total == 0 {
		return
	}
	if workers > total {
		workers = total
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for target := range jobs {
				checkBackend(workerCtx, callerCtx, client, target.service, target.backend)
			}
		}()
	}

sendLoop:
	for _, service := range snapshot.services {
		for _, b := range service.backends {
			select {
			case jobs <- target{service: service, backend: b}:
			case <-workerCtx.Done():
				break sendLoop
			case <-callerCtx.Done():
				break sendLoop
			}
		}
	}
	close(jobs)
	wg.Wait()
}

func checkBackend(workerCtx, callerCtx context.Context, client *http.Client, service *Service, b *backend) {
	ctx, cancel := context.WithTimeout(workerCtx, healthCheckTimeout)
	stop := context.AfterFunc(callerCtx, cancel)
	defer func() { stop(); cancel() }()
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, b.healthURL, nil)
	var resp *http.Response
	var err error
	if reqErr != nil {
		err = reqErr
	} else {
		resp, err = client.Do(req)
	}
	healthy := false
	if err == nil {
		healthy = resp.StatusCode == http.StatusOK
		if !healthy {
			err = fmt.Errorf("health endpoint returned status %d", resp.StatusCode)
		}
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
			_ = resp.Body.Close()
		}
	}

	wasHealthy := b.healthy.Swap(healthy)
	firstCheck := !b.checked.Swap(true)
	if wasHealthy != healthy || firstCheck && !healthy {
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
	snapshot := g.config.Load()
	if snapshot == nil {
		http.Error(w, "configuration not loaded", http.StatusServiceUnavailable)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), configSnapshotKey, snapshot))
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
	if err := validateJWTSecret(cfg.JwtSecret); err != nil {
		log.Printf("failed to reload config, keeping old configuration: %v", err)
		return
	}

	services, err := g.buildServices(cfg)
	if err != nil {
		log.Printf("failed to reload config, keeping old configuration: %v", err)
		return
	}
	checkAllBackends(g.workerCtx, g.workerCtx, &configSnapshot{services: services}, &http.Client{Timeout: healthCheckTimeout})

	g.storeConfiguration(services, newAuthConfig(cfg))
	log.Println("Configuration reloaded successfully")
}

func (g *Gateway) watchConfigWith(ctx context.Context, path string, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	workerCtx := g.workerCtx
	if workerCtx == nil {
		workerCtx = ctx
	}
	g.workerWG.Add(1)
	go func() {
		defer g.workerWG.Done()
		var lastMod time.Time
		lastErr := ""
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-workerCtx.Done():
				return
			case <-ticker.C:
			}
			info, err := os.Stat(path)
			if err != nil {
				if err.Error() != lastErr {
					log.Printf("Error stating config file: %v", err)
					lastErr = err.Error()
				}
				continue
			}
			lastErr = ""
			if info.ModTime().After(lastMod) {
				if !lastMod.IsZero() {
					g.reloadConfig(path)
				}
				lastMod = info.ModTime()
			}
		}
	}()
}
