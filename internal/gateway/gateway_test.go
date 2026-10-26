package gateway

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

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

func newTestRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	mr := miniredis.RunT(t)
	return mr
}

func newTestGateway(t *testing.T, cfg *Config) *Gateway {
	t.Helper()
	gw, err := NewGateway(cfg)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return gw
}

func testService(name, route string, urls []string, middleware ...string) Service {
	return Service{
		Name:       name,
		Route:      route,
		Urls:       urls,
		Middleware: middleware,
	}
}

func testConfig(mr *miniredis.Miniredis, services ...Service) *Config {
	return &Config{
		Port:      ":0",
		JwtSecret: "test-secret",
		Redis:     RedisConfig{Addr: mr.Addr()},
		MiddlewareConfig: ConfigTypes{
			ApiKeyAuth: ApiConfig{ValidKeys: []string{"key-a", "key-b"}},
		},
		Services: services,
	}
}

func newBackendServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func request(g *Gateway, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	http.HandlerFunc(g.handleGateway).ServeHTTP(rec, req)
	return rec
}

func TestFindServiceUsesLongestPrefix(t *testing.T) {
	users := &Service{Name: "users", Route: "/users"}
	admin := &Service{Name: "admin", Route: "/users/admin"}
	products := &Service{Name: "products", Route: "/products"}
	m := map[string]*Service{"/users": users, "/users/admin": admin, "/products": products}

	g := &Gateway{}
	g.services.Store(&m)

	tests := []struct {
		path string
		want *Service
	}{
		{"/users", users},
		{"/users/42", users},
		{"/users/admin", admin},
		{"/users/admin/settings", admin},
		{"/products/1", products},
		{"/unknown", nil},
	}
	for _, tc := range tests {
		got := g.findService(httptest.NewRequest(http.MethodGet, tc.path, nil))
		if got != tc.want {
			t.Errorf("findService(%q) = %v, want %v", tc.path, nameOf(got), nameOf(tc.want))
		}
	}
}

func nameOf(s *Service) string {
	if s == nil {
		return "<nil>"
	}
	return s.Name
}

func newServiceWithBackends(urls ...string) *Service {
	s := &Service{Name: "test", Route: "/test", Urls: urls}
	for _, u := range urls {
		b := &backend{url: u}
		b.healthy.Store(true)
		s.backends = append(s.backends, b)
	}
	return s
}

func TestPickBackendBreaksTiesRoundRobin(t *testing.T) {
	s := newServiceWithBackends("http://a", "http://b")

	var got []string
	for i := 0; i < 6; i++ {
		b := s.pickBackend()
		if b == nil {
			t.Fatalf("pickBackend returned nil on call %d", i)
		}
		got = append(got, b.url)
	}
	want := []string{"http://a", "http://b", "http://a", "http://b", "http://a", "http://b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pick order = %v, want %v", got, want)
	}
}

func TestPickBackendChoosesFewestConnections(t *testing.T) {
	s := newServiceWithBackends("http://a", "http://b")
	s.backends[0].conns.Store(5)
	s.backends[1].conns.Store(1)

	for i := 0; i < 4; i++ {
		if b := s.pickBackend(); b == nil || b.url != "http://b" {
			t.Fatalf("pickBackend = %v, want http://b", b)
		}
	}
}

func TestPickBackendSkipsUnhealthy(t *testing.T) {
	s := newServiceWithBackends("http://a", "http://b")
	s.backends[0].healthy.Store(false)

	for i := 0; i < 4; i++ {
		if b := s.pickBackend(); b == nil || b.url != "http://b" {
			t.Fatalf("pickBackend = %v, want http://b", b)
		}
	}

	s.backends[1].healthy.Store(false)
	if b := s.pickBackend(); b != nil {
		t.Fatalf("pickBackend = %v, want nil when all backends are unhealthy", b)
	}
}

func TestHandleGatewayReturns503WhenNoHealthyBackend(t *testing.T) {
	backend := newServiceWithBackends("http://a")
	backend.backends[0].healthy.Store(false)
	m := map[string]*Service{"/test": backend}
	g := &Gateway{}
	g.services.Store(&m)

	if rec := request(g, http.MethodGet, "/test", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestAuthMiddleware(t *testing.T) {
	g := &Gateway{}
	g.auth.Store(&authConfig{
		keys:      map[string]bool{"key-a": true},
		jwtSecret: "test-secret",
	})

	identity := ""
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, _ = r.Context().Value(identityKey).(string)
		w.WriteHeader(http.StatusOK)
	})
	handler := g.authAPIKeyMiddleware(next)

	validJWT := signJWT(t, "test-secret", jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	expiredJWT := signJWT(t, "test-secret", jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-1",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})
	wrongSecretJWT := signJWT(t, "other-secret", jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	wrongAlgJWT := signJWT(t, "test-secret", jwt.SigningMethodHS512, jwt.MapClaims{
		"sub": "user-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	noneJWT := signJWT(t, "", jwt.SigningMethodNone, jwt.MapClaims{
		"sub": "user-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	tests := []struct {
		name       string
		headers    map[string]string
		wantStatus int
		wantID     string
	}{
		{"valid api key", map[string]string{"X-API-KEY": "key-a"}, http.StatusOK, "key:" + hashSecret("key-a", "test-secret")},
		{"invalid api key", map[string]string{"X-API-KEY": "nope"}, http.StatusUnauthorized, ""},
		{"missing credentials", nil, http.StatusUnauthorized, ""},
		{"valid jwt", map[string]string{"Authorization": "Bearer " + validJWT}, http.StatusOK, "jwt:user-1"},
		{"expired jwt", map[string]string{"Authorization": "Bearer " + expiredJWT}, http.StatusUnauthorized, ""},
		{"wrong secret", map[string]string{"Authorization": "Bearer " + wrongSecretJWT}, http.StatusUnauthorized, ""},
		{"wrong algorithm", map[string]string{"Authorization": "Bearer " + wrongAlgJWT}, http.StatusUnauthorized, ""},
		{"alg none", map[string]string{"Authorization": "Bearer " + noneJWT}, http.StatusUnauthorized, ""},
		{"invalid jwt falls back to api key", map[string]string{"Authorization": "Bearer " + wrongSecretJWT, "X-API-KEY": "key-a"}, http.StatusOK, "key:" + hashSecret("key-a", "test-secret")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			identity = ""
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantID != "" && identity != tc.wantID {
				t.Fatalf("identity = %q, want %q", identity, tc.wantID)
			}
		})
	}
}

func signJWT(t *testing.T, secret string, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(method, claims)
	var (
		signed string
		err    error
	)
	if method == jwt.SigningMethodNone {
		signed, err = token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	} else {
		signed, err = token.SignedString([]byte(secret))
	}
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return signed
}

func TestRateLimiterRejectsBeyondBurstAndRefills(t *testing.T) {
	mr := newTestRedis(t)
	backend := newBackendServer(t, http.StatusOK)

	svc := testService("users", "/users", []string{backend.URL}, "rate_limit")
	svc.RateLimitRPS = 1
	svc.RateLimitBurst = 1
	g := newTestGateway(t, testConfig(mr, svc))

	now := time.Now()
	g.now = func() time.Time { return now }

	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec.Code)
	}

	const bucketKey = "rl:ip:192.0.2.1:/users"
	if ttl := mr.TTL(bucketKey); ttl <= 0 {
		t.Fatalf("rate limit key %q has no TTL (got %v)", bucketKey, ttl)
	}

	now = now.Add(2 * time.Second)
	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusOK {
		t.Fatalf("request after refill status = %d, want 200", rec.Code)
	}
}

func TestRateLimiterIsPerIdentity(t *testing.T) {
	mr := newTestRedis(t)
	backend := newBackendServer(t, http.StatusOK)

	svc := testService("users", "/users", []string{backend.URL}, "auth_apikey", "rate_limit")
	svc.RateLimitRPS = 0.0001
	svc.RateLimitBurst = 1
	g := newTestGateway(t, testConfig(mr, svc))

	now := time.Now()
	g.now = func() time.Time { return now }

	keyA := map[string]string{"X-API-KEY": "key-a"}
	keyB := map[string]string{"X-API-KEY": "key-b"}

	if rec := request(g, http.MethodGet, "/users", keyA); rec.Code != http.StatusOK {
		t.Fatalf("key-a first request = %d, want 200", rec.Code)
	}
	if rec := request(g, http.MethodGet, "/users", keyA); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("key-a second request = %d, want 429", rec.Code)
	}
	if rec := request(g, http.MethodGet, "/users", keyB); rec.Code != http.StatusOK {
		t.Fatalf("key-b first request = %d, want 200 (separate bucket)", rec.Code)
	}
}

func TestRateLimiterFailsOpenWhenRedisIsDown(t *testing.T) {
	mr := newTestRedis(t)
	backend := newBackendServer(t, http.StatusOK)
	svc := testService("users", "/users", []string{backend.URL}, "rate_limit")

	g := newTestGateway(t, testConfig(mr, svc))
	addr := mr.Addr()
	mr.Close()
	g.rdb = redis.NewClient(&redis.Options{
		Addr:        addr,
		MaxRetries:  -1,
		DialTimeout: 50 * time.Millisecond,
	})

	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when Redis is unavailable", rec.Code)
	}
}

func TestCircuitBreakerOpensRejectsAndRecovers(t *testing.T) {
	mr := newTestRedis(t)
	var fail atomic.Bool
	fail.Store(true)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	svc := testService("users", "/users", []string{backend.URL}, "circuit_breaker")
	svc.CBFailureThreshold = 2
	svc.CBResetTimeoutSeconds = 1
	g := newTestGateway(t, testConfig(mr, svc))

	now := time.Now()
	g.now = func() time.Time { return now }

	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("request 1 = %d, want 500", rec.Code)
	}
	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("request 2 = %d, want 500", rec.Code)
	}
	if state := mustGet(t, mr, "cb:/users:state"); state != "1" {
		t.Fatalf("circuit state = %q, want \"1\" (open)", state)
	}

	// Open circuit must reject before the reset timeout elapses.
	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request 3 = %d, want 503 while circuit is open", rec.Code)
	}

	// After the timeout a single probe is allowed; a failed probe reopens.
	now = now.Add(2 * time.Second)
	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("probe request = %d, want 500 from backend", rec.Code)
	}
	if state := mustGet(t, mr, "cb:/users:state"); state != "1" {
		t.Fatalf("circuit state after failed probe = %q, want \"1\" (open)", state)
	}
	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request after failed probe = %d, want 503", rec.Code)
	}

	// A successful probe closes the circuit.
	fail.Store(false)
	now = now.Add(2 * time.Second)
	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusOK {
		t.Fatalf("successful probe = %d, want 200", rec.Code)
	}
	if state := mustGet(t, mr, "cb:/users:state"); state != "0" {
		t.Fatalf("circuit state after successful probe = %q, want \"0\" (closed)", state)
	}
	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusOK {
		t.Fatalf("request after recovery = %d, want 200", rec.Code)
	}
}

func TestCircuitBreakerFailsOpenWhenRedisIsDown(t *testing.T) {
	mr := newTestRedis(t)
	backend := newBackendServer(t, http.StatusOK)
	svc := testService("users", "/users", []string{backend.URL}, "circuit_breaker")

	g := newTestGateway(t, testConfig(mr, svc))
	addr := mr.Addr()
	mr.Close()
	g.rdb = redis.NewClient(&redis.Options{
		Addr:        addr,
		MaxRetries:  -1,
		DialTimeout: 50 * time.Millisecond,
	})

	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when Redis is unavailable", rec.Code)
	}
}

func TestCircuitBreakerAllowsOnlyOneProbe(t *testing.T) {
	mr := newTestRedis(t)
	svc := testService("users", "/users", []string{"http://127.0.0.1:1"}, "circuit_breaker")
	svc.CBFailureThreshold = 1
	svc.CBResetTimeoutSeconds = 1
	g := newTestGateway(t, testConfig(mr, svc))
	g.now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	if _, err := g.rdb.Eval(context.Background(), cbReportScript,
		[]string{"cb:/users:state", "cb:/users:fails", "cb:/users:last", "cb:/users:probe"},
		1, 0, float64(g.clock().UnixNano())/1e9).Result(); err != nil {
		t.Fatalf("seed failure: %v", err)
	}
	g.now = func() time.Time { return time.Unix(1_700_000_010, 0) }

	first, err := g.rdb.Eval(context.Background(), cbAllowScript,
		[]string{"cb:/users:state", "cb:/users:last", "cb:/users:probe"},
		1, float64(g.clock().UnixNano())/1e9, cbProbeTTLMillis).Int64Slice()
	if err != nil {
		t.Fatalf("first allow: %v", err)
	}
	if len(first) != 2 || first[0] != 2 || first[1] != 1 {
		t.Fatalf("first allow = %v, want [2 1]", first)
	}

	second, err := g.rdb.Eval(context.Background(), cbAllowScript,
		[]string{"cb:/users:state", "cb:/users:last", "cb:/users:probe"},
		1, float64(g.clock().UnixNano())/1e9, cbProbeTTLMillis).Int64Slice()
	if err != nil {
		t.Fatalf("second allow: %v", err)
	}
	if len(second) != 2 || second[0] != 2 || second[1] != 0 {
		t.Fatalf("second allow = %v, want [2 0] (probe already in flight)", second)
	}
}

func TestHealthCheckMarksBackendUnhealthyAndRecovers(t *testing.T) {
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

	svc := &Service{Name: "users"}
	b := &backend{url: srv.URL}
	b.healthy.Store(true)
	client := &http.Client{Timeout: time.Second}

	checkBackend(client, svc, b)
	if b.healthy.Load() {
		t.Fatal("backend should be unhealthy after a 500 health response")
	}

	fail.Store(false)
	checkBackend(client, svc, b)
	if !b.healthy.Load() {
		t.Fatal("backend should recover after a 200 health response")
	}
}

func TestLoadConfigPathReturnsErrorInsteadOfExiting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("services: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfigPath(path); err == nil {
		t.Fatal("expected an error for invalid yaml")
	}
	if _, err := loadConfigPath(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestReloadConfigReplacesServicesAndKeys(t *testing.T) {
	mr := newTestRedis(t)
	backend := newBackendServer(t, http.StatusOK)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeConfig(t, path, `
port: ":0"
jwt_secret: "test-secret"
redis:
  addr: "`+mr.Addr()+`"
middleware_config:
  auth_apikey:
    valid_keys:
      - "old-key"
services:
  - name: users
    route_pattern: "/users"
    urls:
      - "`+backend.URL+`"
    middleware:
      - "auth_apikey"
`)

	cfg, err := loadConfigPath(path)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	g := newTestGateway(t, cfg)
	if !g.auth.Load().keys["old-key"] {
		t.Fatal("old key should be active after initial load")
	}

	writeConfig(t, path, `
port: ":0"
jwt_secret: "test-secret"
redis:
  addr: "`+mr.Addr()+`"
middleware_config:
  auth_apikey:
    valid_keys:
      - "new-key"
services:
  - name: products
    route_pattern: "/products"
    urls:
      - "`+backend.URL+`"
    middleware:
      - "auth_apikey"
`)
	g.reloadConfig(path)

	auth := g.auth.Load()
	if auth.keys["old-key"] {
		t.Fatal("old key should be gone after reload")
	}
	if !auth.keys["new-key"] {
		t.Fatal("new key should be active after reload")
	}
	if g.findService(httptest.NewRequest(http.MethodGet, "/products", nil)) == nil {
		t.Fatal("new service route should be registered after reload")
	}
	if g.findService(httptest.NewRequest(http.MethodGet, "/users", nil)) != nil {
		t.Fatal("removed service route should no longer be registered")
	}

	// An invalid update must be ignored rather than replacing working state.
	writeConfig(t, path, "services: [\n")
	g.reloadConfig(path)
	if g.findService(httptest.NewRequest(http.MethodGet, "/products", nil)) == nil {
		t.Fatal("invalid config must not wipe the running configuration")
	}

	writeConfig(t, path, `
port: ":0"
services:
  - name: broken
    route_pattern: "/broken"
    urls: []
`)
	g.reloadConfig(path)
	if g.findService(httptest.NewRequest(http.MethodGet, "/broken", nil)) != nil {
		t.Fatal("service with no urls must not be loaded")
	}

	rec := request(g, http.MethodGet, "/products", map[string]string{"X-API-KEY": "old-key"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old key status = %d, want 401 after reload", rec.Code)
	}
	rec = request(g, http.MethodGet, "/products", map[string]string{"X-API-KEY": "new-key"})
	if rec.Code != http.StatusOK {
		t.Fatalf("new key status = %d, want 200 after reload", rec.Code)
	}
}

func TestBuildServicesRejectsInvalidConfig(t *testing.T) {
	g := &Gateway{transport: http.DefaultTransport.(*http.Transport)}

	tests := []struct {
		name string
		cfg  *Config
	}{
		{"no urls", &Config{Services: []Service{{Name: "x", Route: "/x"}}}},
		{"no route", &Config{Services: []Service{{Name: "x", Urls: []string{"http://a"}}}}},
		{"bad scheme", &Config{Services: []Service{{Name: "x", Route: "/x", Urls: []string{"ftp://a"}}}}},
		{"duplicate route", &Config{Services: []Service{
			{Name: "x", Route: "/x", Urls: []string{"http://a"}},
			{Name: "y", Route: "/x", Urls: []string{"http://b"}},
		}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := g.buildServices(tc.cfg); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustGet(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	v, err := mr.Get(key)
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	return v
}
