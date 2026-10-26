package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestStatusRecorderIgnoresDuplicateWriteHeaderAndUnwraps(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner, statusCode: http.StatusOK}

	rec.WriteHeader(http.StatusCreated)
	rec.WriteHeader(http.StatusInternalServerError)

	if rec.statusCode != http.StatusCreated {
		t.Fatalf("statusCode = %d, want 201 (first write wins)", rec.statusCode)
	}
	if inner.Code != http.StatusCreated {
		t.Fatalf("inner status = %d, want 201", inner.Code)
	}
	if rec.Unwrap() != inner {
		t.Fatal("Unwrap must return the underlying ResponseWriter")
	}
}

func TestClientIdentityBranches(t *testing.T) {
	g := &Gateway{}
	g.auth.Store(&authConfig{jwtSecret: "test-secret"})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	if got := g.clientIdentity(req); got != "ip:1.2.3.4" {
		t.Fatalf("host:port identity = %q, want ip:1.2.3.4", got)
	}

	req.RemoteAddr = "1.2.3.4"
	if got := g.clientIdentity(req); got != "ip:1.2.3.4" {
		t.Fatalf("bare-host identity = %q, want ip:1.2.3.4", got)
	}

	req.Header.Set("X-API-KEY", "secret-key")
	if got := g.clientIdentity(req); got != "key:"+hashSecret("secret-key", "test-secret") {
		t.Fatalf("api-key identity = %q, want hashed key", got)
	}

	ctx := context.WithValue(req.Context(), identityKey, "jwt:abc")
	if got := g.clientIdentity(req.WithContext(ctx)); got != "jwt:abc" {
		t.Fatalf("context identity = %q, want jwt:abc", got)
	}
}

func TestHashSecretUsesKey(t *testing.T) {
	got := hashSecret("same-secret", "key-one")
	if got != hashSecret("same-secret", "key-one") {
		t.Fatal("hashSecret must be deterministic for the same value and key")
	}
	if got == hashSecret("same-secret", "key-two") {
		t.Fatal("hashSecret must vary when the key changes")
	}
}

func TestClockFallback(t *testing.T) {
	g := &Gateway{}
	if got := g.clock(); got.IsZero() {
		t.Fatal("clock must return the current time when now is unset")
	}

	fixed := time.Unix(1_700_000_000, 0)
	g.now = func() time.Time { return fixed }
	if got := g.clock(); !got.Equal(fixed) {
		t.Fatalf("clock = %v, want %v", got, fixed)
	}
}

func TestAuthMiddlewareWithoutConfiguration(t *testing.T) {
	g := &Gateway{}
	handler := g.authAPIKeyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when auth config is missing", rec.Code)
	}
}

func TestAuthJWTWithoutSubjectUsesTokenHash(t *testing.T) {
	g := &Gateway{}
	g.auth.Store(&authConfig{keys: map[string]bool{}, jwtSecret: "test-secret"})

	identity := ""
	handler := g.authAPIKeyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, _ = r.Context().Value(identityKey).(string)
		w.WriteHeader(http.StatusOK)
	}))

	token := signJWT(t, "test-secret", jwt.SigningMethodHS256, jwt.MapClaims{
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.HasPrefix(identity, "jwt:") || identity == "jwt:" {
		t.Fatalf("identity = %q, want jwt:<hash> for a token without sub", identity)
	}
}

func TestNewAuthConfigDefaults(t *testing.T) {
	auth := newAuthConfig(&Config{})
	if auth.jwtSecret != "default_secret_key" {
		t.Fatalf("jwtSecret = %q, want default_secret_key", auth.jwtSecret)
	}
	if len(auth.keys) != 0 {
		t.Fatalf("keys = %v, want empty", auth.keys)
	}
}

func TestNewGatewayWarnsWhenRedisIsUnavailable(t *testing.T) {
	backend := backendWithBody(t, "ok")
	cfg := &Config{
		Port:      ":0",
		JwtSecret: "test-secret",
		Redis:     RedisConfig{Addr: "127.0.0.1:1"},
		Services:  []Service{testService("users", "/users", []string{backend.URL}, "auth_apikey")},
	}
	gw, err := NewGateway(cfg)
	if err != nil || gw == nil {
		t.Fatalf("NewGateway = (%v, %v), want a gateway that starts without Redis", gw, err)
	}
}

func TestNewGatewayReturnsErrorForInvalidServices(t *testing.T) {
	mr := newTestRedis(t)
	cfg := &Config{
		Port:     ":0",
		Redis:    RedisConfig{Addr: mr.Addr()},
		Services: []Service{{Name: "broken", Route: "/broken"}},
	}
	if _, err := NewGateway(cfg); err == nil {
		t.Fatal("expected an error for a service with no urls")
	}
}

func TestBuildServicesValidConfigCopiesFields(t *testing.T) {
	g := &Gateway{transport: http.DefaultTransport.(*http.Transport)}
	middleware := []string{"metrics", "auth_apikey"}
	cfg := &Config{Services: []Service{
		{
			Name:                  "users",
			Route:                 "/users",
			Urls:                  []string{"http://localhost:8081", "http://localhost:8082"},
			Middleware:            middleware,
			RateLimitRPS:          10,
			RateLimitBurst:        20,
			CBFailureThreshold:    3,
			CBResetTimeoutSeconds: 4,
		},
	}}

	services, err := g.buildServices(cfg)
	if err != nil {
		t.Fatalf("buildServices: %v", err)
	}
	svc := services["/users"]
	if svc == nil {
		t.Fatal("service /users was not built")
	}
	if len(svc.backends) != 2 {
		t.Fatalf("backends = %d, want 2", len(svc.backends))
	}
	for _, b := range svc.backends {
		if !b.healthy.Load() {
			t.Fatalf("backend %s should start healthy", b.url)
		}
		if b.proxy == nil {
			t.Fatalf("backend %s has no proxy", b.url)
		}
	}
	if svc.RateLimitRPS != 10 || svc.RateLimitBurst != 20 || svc.CBFailureThreshold != 3 || svc.CBResetTimeoutSeconds != 4 {
		t.Fatalf("tuning fields were not copied: %+v", svc)
	}

	middleware[0] = "mutated"
	if svc.Middleware[0] != "metrics" {
		t.Fatal("middleware must be copied, not aliased")
	}
}

func TestFindServiceWithEmptyStore(t *testing.T) {
	g := &Gateway{}
	if got := g.findService(httptest.NewRequest(http.MethodGet, "/users", nil)); got != nil {
		t.Fatalf("findService = %v, want nil", got)
	}
}

func TestPickBackendWithNoBackends(t *testing.T) {
	s := &Service{Name: "empty", Route: "/empty"}
	if got := s.pickBackend(); got != nil {
		t.Fatalf("pickBackend = %v, want nil", got)
	}
}

func TestHandleGatewayUnknownRoute(t *testing.T) {
	g := &Gateway{}
	rec := request(g, http.MethodGet, "/missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRateLimitMiddlewareWithoutRedis(t *testing.T) {
	g := &Gateway{}
	handler := g.rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when Redis is not configured", rec.Code)
	}
}

func TestRateLimitMiddlewareWithoutMatchingService(t *testing.T) {
	mr := newTestRedis(t)
	g := newTestGateway(t, testConfig(mr))
	handler := g.rateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/unmatched", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestCircuitBreakerMiddlewareWithoutServiceOrRedis(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	noRedis := &Gateway{}
	rec := httptest.NewRecorder()
	noRedis.circuitBreakerMiddleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without Redis", rec.Code)
	}

	mr := newTestRedis(t)
	noService := newTestGateway(t, testConfig(mr))
	rec = httptest.NewRecorder()
	noService.circuitBreakerMiddleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/unmatched", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a matching service", rec.Code)
	}
}

func TestCircuitBreakerReportFailureDoesNotBreakResponse(t *testing.T) {
	mr := newTestRedis(t)
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		once.Do(func() { mr.Close() })
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	svc := testService("report", "/report", []string{srv.URL}, "circuit_breaker")
	svc.CBFailureThreshold = 2
	svc.CBResetTimeoutSeconds = 1
	g := newTestGateway(t, testConfig(mr, svc))
	g.now = func() time.Time { return time.Unix(1_700_000_000, 0) }

	rec := request(g, http.MethodGet, "/report", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when recording the outcome fails", rec.Code)
	}
}

func TestRateLimiterUsesHashedKeysAndTTL(t *testing.T) {
	mr := newTestRedis(t)
	backend := backendWithBody(t, "ok")
	svc := testService("hashed", "/hashed", []string{backend.URL}, "auth_apikey", "rate_limit")
	g := newTestGateway(t, testConfig(mr, svc))

	rec := request(g, http.MethodGet, "/hashed", map[string]string{"X-API-KEY": "key-a"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	keys := mr.Keys()
	var bucket string
	for _, k := range keys {
		if strings.HasPrefix(k, "rl:") {
			bucket = k
		}
	}
	if bucket == "" {
		t.Fatalf("no rate-limit key was created, got %v", keys)
	}
	if strings.Contains(bucket, "key-a") {
		t.Fatalf("rate-limit key %q leaks the raw API key", bucket)
	}
	if !strings.Contains(bucket, hashSecret("key-a", "test-secret")) {
		t.Fatalf("rate-limit key %q does not contain the hashed identity", bucket)
	}
	if ttl := mr.TTL(bucket); ttl <= 0 {
		t.Fatalf("rate-limit key %q has no TTL", bucket)
	}
}
