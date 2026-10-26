package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func backendWithBody(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

// TestFullStackHTTPWithMetrics runs the shipped middleware stack over HTTP
// (metrics -> auth -> rate limit -> circuit breaker -> proxy) and verifies the
// responses and the Prometheus series they produce.
func TestFullStackHTTPWithMetrics(t *testing.T) {
	mr := newTestRedis(t)
	stackBackendA := backendWithBody(t, "backend-a")
	stackBackendB := backendWithBody(t, "backend-b")

	cfg := &Config{
		Port:      ":0",
		JwtSecret: "test-secret",
		Redis:     RedisConfig{Addr: mr.Addr()},
		MiddlewareConfig: ConfigTypes{
			ApiKeyAuth: ApiConfig{ValidKeys: []string{"key-a"}},
		},
		Services: []Service{
			{
				Name:                  "e2e-stack",
				Route:                 "/e2e-stack",
				Urls:                  []string{stackBackendA.URL, stackBackendB.URL},
				Middleware:            []string{"metrics", "auth_apikey", "rate_limit", "circuit_breaker"},
				RateLimitRPS:          1000,
				RateLimitBurst:        1000,
				CBFailureThreshold:    5,
				CBResetTimeoutSeconds: 10,
			},
			{
				Name:           "e2e-limited",
				Route:          "/e2e-limited",
				Urls:           []string{stackBackendA.URL},
				Middleware:     []string{"metrics", "auth_apikey", "rate_limit"},
				RateLimitRPS:   0.0001,
				RateLimitBurst: 1,
			},
		},
	}
	g := newTestGateway(t, cfg)
	fixed := time.Now()
	g.now = func() time.Time { return fixed }

	okCounter := counter.WithLabelValues("GET", "200", "e2e-stack")
	unauthorizedCounter := counter.WithLabelValues("GET", "401", "e2e-stack")
	limitedCounter := counter.WithLabelValues("GET", "429", "e2e-limited")
	limitedMetric := rateLimited.WithLabelValues("e2e-limited", "token_bucket")

	okBefore := testutil.ToFloat64(okCounter)
	unauthorizedBefore := testutil.ToFloat64(unauthorizedCounter)
	limitedBefore := testutil.ToFloat64(limitedCounter)
	limitedMetricBefore := testutil.ToFloat64(limitedMetric)

	if rec := request(g, http.MethodGet, "/e2e-stack", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want 401", rec.Code)
	}

	keyHeader := map[string]string{"X-API-KEY": "key-a"}
	if rec := request(g, http.MethodGet, "/e2e-stack", keyHeader); rec.Code != http.StatusOK {
		t.Fatalf("valid key status = %d, want 200", rec.Code)
	}

	jwtToken := signJWT(t, "test-secret", jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if rec := request(g, http.MethodGet, "/e2e-stack", map[string]string{"Authorization": "Bearer " + jwtToken}); rec.Code != http.StatusOK {
		t.Fatalf("valid jwt status = %d, want 200", rec.Code)
	}

	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		rec := request(g, http.MethodGet, "/e2e-stack", keyHeader)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, rec.Code)
		}
		seen[rec.Body.String()] = true
	}
	if !seen["backend-a"] || !seen["backend-b"] {
		t.Fatalf("expected both backends to serve traffic, saw %v", seen)
	}

	if rec := request(g, http.MethodGet, "/e2e-limited", keyHeader); rec.Code != http.StatusOK {
		t.Fatalf("first limited request = %d, want 200", rec.Code)
	}
	rec := request(g, http.MethodGet, "/e2e-limited", keyHeader)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second limited request = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want \"1\"", got)
	}

	if delta := testutil.ToFloat64(okCounter) - okBefore; delta != 8 {
		t.Errorf("200 counter delta = %v, want 8", delta)
	}
	if delta := testutil.ToFloat64(unauthorizedCounter) - unauthorizedBefore; delta != 1 {
		t.Errorf("401 counter delta = %v, want 1 (metrics should wrap auth)", delta)
	}
	if delta := testutil.ToFloat64(limitedCounter) - limitedBefore; delta != 1 {
		t.Errorf("429 counter delta = %v, want 1 (metrics should wrap rate limiting)", delta)
	}
	if delta := testutil.ToFloat64(limitedMetric) - limitedMetricBefore; delta != 1 {
		t.Errorf("gateway_rate_limited_total delta = %v, want 1", delta)
	}
	if state := testutil.ToFloat64(cbState.WithLabelValues("e2e-stack")); state != 0 {
		t.Errorf("circuit breaker gauge = %v, want 0 (closed)", state)
	}

	scrape := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(scrape, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := scrape.Body.String()
	for _, want := range []string{
		`http_request_counter{method="GET",service="e2e-stack",status="200"}`,
		`http_request_counter{method="GET",service="e2e-stack",status="401"}`,
		`http_request_counter{method="GET",service="e2e-limited",status="429"}`,
		`http_request_timer_count{method="GET",service="e2e-stack"}`,
		`gateway_circuit_breaker_state{service="e2e-stack"} 0`,
		`gateway_rate_limited_total{reason="token_bucket",service="e2e-limited"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}
}

func TestProxyErrorReturns502(t *testing.T) {
	mr := newTestRedis(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	svc := testService("dead", "/dead", []string{deadURL})
	g := newTestGateway(t, testConfig(mr, svc))

	if rec := request(g, http.MethodGet, "/dead", nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for an unreachable backend", rec.Code)
	}
}

func TestClientDisconnectIsCounted(t *testing.T) {
	mr := newTestRedis(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	g := newTestGateway(t, testConfig(mr, testService("slow", "/slow", []string{srv.URL})))
	before := testutil.ToFloat64(proxyCanceledCounter)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/slow", nil).WithContext(ctx)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	rec := httptest.NewRecorder()
	http.HandlerFunc(g.handleGateway).ServeHTTP(rec, req)

	if delta := testutil.ToFloat64(proxyCanceledCounter) - before; delta != 1 {
		t.Fatalf("gateway_proxy_canceled_total delta = %v, want 1", delta)
	}
}

func TestHealthCheckLoopTracksBackendState(t *testing.T) {
	empty := &Gateway{}
	emptyCtx, emptyCancel := context.WithCancel(context.Background())
	empty.startHealthChecksWith(emptyCtx, 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	emptyCancel()

	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc := &Service{Name: "looped", Route: "/looped"}
	b := &backend{url: srv.URL}
	b.healthy.Store(true)
	svc.backends = []*backend{b}
	m := map[string]*Service{"/looped": svc}

	g := &Gateway{}
	g.services.Store(&m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.startHealthChecksWith(ctx, 10*time.Millisecond)

	waitFor(t, 2*time.Second, func() bool { return !b.healthy.Load() })
	fail.Store(false)
	waitFor(t, 2*time.Second, func() bool { return b.healthy.Load() })
}

func TestConfigWatcherReloadsAndKeepsLastGoodConfig(t *testing.T) {
	mr := newTestRedis(t)
	backend := backendWithBody(t, "ok")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	writeConfig(t, path, watchedConfig(mr.Addr(), backend.URL, "key-old"))
	cfg, err := loadConfigPath(path)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	g := newTestGateway(t, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.watchConfigWith(ctx, path, 20*time.Millisecond)
	time.Sleep(100 * time.Millisecond)

	writeConfig(t, path, watchedConfig(mr.Addr(), backend.URL, "key-new"))
	waitFor(t, 2*time.Second, func() bool { return g.auth.Load().keys["key-new"] })

	writeConfig(t, path, "services: [\n")
	time.Sleep(150 * time.Millisecond)
	if !g.auth.Load().keys["key-new"] {
		t.Fatal("invalid config must not replace the running auth config")
	}
	if g.findService(httptest.NewRequest(http.MethodGet, "/watched", nil)) == nil {
		t.Fatal("invalid config must not remove running services")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	writeConfig(t, path, watchedConfig(mr.Addr(), backend.URL, "key-third"))
	waitFor(t, 2*time.Second, func() bool { return g.auth.Load().keys["key-third"] })

	if rec := request(g, http.MethodGet, "/watched", map[string]string{"X-API-KEY": "key-third"}); rec.Code != http.StatusOK {
		t.Fatalf("status after watcher reload = %d, want 200", rec.Code)
	}
}

func watchedConfig(redisAddr, backendURL, key string) string {
	return `
port: ":0"
jwt_secret: "test-secret"
redis:
  addr: "` + redisAddr + `"
middleware_config:
  auth_apikey:
    valid_keys:
      - "` + key + `"
services:
  - name: watched
    route_pattern: "/watched"
    urls:
      - "` + backendURL + `"
    middleware:
      - "metrics"
      - "auth_apikey"
`
}
