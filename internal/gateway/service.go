package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

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
