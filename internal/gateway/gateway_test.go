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
	"github.com/prometheus/client_golang/prometheus/testutil"
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
		JwtSecret: "test-test-test-test-test-test-test-test",
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
	g.storeConfiguration(m, nil)

	tests := []struct {
		path string
		want *Service
	}{
		{"/users", users},
		{"/users/42", users},
		{"/usersXYZ", nil},
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
	g.storeConfiguration(m, nil)

	if rec := request(g, http.MethodGet, "/test", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestAuthMiddleware(t *testing.T) {
	g := &Gateway{}
	g.storeConfiguration(nil, &authConfig{
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
	missingExpirationJWT := signJWT(t, "test-secret", jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "user-1",
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
		{"missing expiration", map[string]string{"Authorization": "Bearer " + missingExpirationJWT}, http.StatusUnauthorized, ""},
		{"wrong secret", map[string]string{"Authorization": "Bearer " + wrongSecretJWT}, http.StatusUnauthorized, ""},
		{"wrong algorithm", map[string]string{"Authorization": "Bearer " + wrongAlgJWT}, http.StatusUnauthorized, ""},
		{"alg none", map[string]string{"Authorization": "Bearer " + noneJWT}, http.StatusUnauthorized, ""},
		{"malformed bearer header", map[string]string{"Authorization": "Bearer"}, http.StatusUnauthorized, ""},
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

func TestAuthJWTValidatesConfiguredIssuerAndAudience(t *testing.T) {
	const secret = "test-secret"
	g := &Gateway{}
	g.storeConfiguration(nil, &authConfig{jwtSecret: secret, jwtIssuer: "trusted-issuer", jwtAudience: "gateway-api"})
	handler := g.authAPIKeyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	claims := func(issuer string, audience string) jwt.MapClaims {
		return jwt.MapClaims{
			"sub": "user-1",
			"exp": time.Now().Add(time.Hour).Unix(),
			"iss": issuer,
			"aud": audience,
		}
	}
	for _, tc := range []struct {
		name   string
		claims jwt.MapClaims
		want   int
	}{
		{"valid issuer and audience", claims("trusted-issuer", "gateway-api"), http.StatusOK},
		{"wrong issuer", claims("untrusted-issuer", "gateway-api"), http.StatusUnauthorized},
		{"wrong audience", claims("trusted-issuer", "other-api"), http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := signJWT(t, secret, jwt.SigningMethodHS256, tc.claims)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
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

	now := time.Now().UTC()
	mr.SetTime(now)

	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}
	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec.Code)
	} else if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1 second", got)
	}

	const bucketKey = "rl:ip:192.0.2.1:/users"
	if ttl := mr.TTL(bucketKey); ttl <= 0 {
		t.Fatalf("rate limit key %q has no TTL (got %v)", bucketKey, ttl)
	}

	mr.SetTime(now.Add(2 * time.Second))
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

	before := testutil.ToFloat64(redisOperationErrors.WithLabelValues("users", "rate_limit"))
	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when Redis is unavailable", rec.Code)
	}
	if got := testutil.ToFloat64(redisOperationErrors.WithLabelValues("users", "rate_limit")) - before; got != 1 {
		t.Fatalf("Redis failure metric increment = %v, want 1", got)
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

func circuitBreakerTestKeys() []string {
	return []string{"cb:/users:state", "cb:/users:fails", "cb:/users:last", "cb:/users:probe", "cb:/users:generation"}
}

func seedOpenCircuit(t *testing.T, g *Gateway, now time.Time) {
	t.Helper()
	g.now = func() time.Time { return now }
	if _, err := g.rdb.Eval(context.Background(), cbReportScript, circuitBreakerTestKeys(),
		1, 0, float64(now.UnixNano())/1e9, 0, 0, "").Result(); err != nil {
		t.Fatalf("seed open circuit: %v", err)
	}
}

func TestCircuitBreakerIgnoresLateClosedOutcomes(t *testing.T) {
	mr := newTestRedis(t)
	g := newTestGateway(t, testConfig(mr))
	now := time.Unix(1_700_000_000, 0)
	g.now = func() time.Time { return now }
	allowKeys := []string{circuitBreakerTestKeys()[0], circuitBreakerTestKeys()[2], circuitBreakerTestKeys()[3], circuitBreakerTestKeys()[4]}
	first, err := g.rdb.Eval(context.Background(), cbAllowScript, allowKeys, 1, float64(now.UnixNano())/1e9, cbProbeTTLMillis, "first").Int64Slice()
	if err != nil || len(first) != 3 || first[0] != 0 || first[1] != 1 {
		t.Fatalf("first closed admission = %v, %v", first, err)
	}
	second, err := g.rdb.Eval(context.Background(), cbAllowScript, allowKeys, 1, float64(now.UnixNano())/1e9, cbProbeTTLMillis, "second").Int64Slice()
	if err != nil || len(second) != 3 || second[0] != 0 || second[1] != 1 || second[2] != first[2] {
		t.Fatalf("second closed admission = %v, %v", second, err)
	}

	if _, err := g.rdb.Eval(context.Background(), cbReportScript, circuitBreakerTestKeys(),
		1, 0, float64(now.UnixNano())/1e9, 0, first[2], "first").Result(); err != nil {
		t.Fatalf("report threshold failure: %v", err)
	}
	for _, outcome := range []int64{1, 0} {
		if _, err := g.rdb.Eval(context.Background(), cbReportScript, circuitBreakerTestKeys(),
			1, outcome, float64(now.Add(time.Second).UnixNano())/1e9, 0, second[2], "second").Result(); err != nil {
			t.Fatalf("report late closed outcome %d: %v", outcome, err)
		}
	}
	if state := mustGet(t, mr, "cb:/users:state"); state != "1" {
		t.Fatalf("state after late closed outcomes = %q, want open (1)", state)
	}
	if failures := mustGet(t, mr, "cb:/users:fails"); failures != "1" {
		t.Fatalf("failure count after late closed outcomes = %q, want 1", failures)
	}
}

func TestCircuitBreakerProbeReportRequiresCurrentOwner(t *testing.T) {
	mr := newTestRedis(t)
	g := newTestGateway(t, testConfig(mr))
	now := time.Unix(1_700_000_000, 0)
	seedOpenCircuit(t, g, now)
	g.now = func() time.Time { return now.Add(2 * time.Second) }
	allowKeys := []string{circuitBreakerTestKeys()[0], circuitBreakerTestKeys()[2], circuitBreakerTestKeys()[3], circuitBreakerTestKeys()[4]}
	owner, err := g.rdb.Eval(context.Background(), cbAllowScript, allowKeys, 1,
		float64(g.clock().UnixNano())/1e9, cbProbeTTLMillis, "owner-a").Int64Slice()
	if err != nil || len(owner) != 3 || owner[0] != 2 || owner[1] != 1 {
		t.Fatalf("probe admission = %v, %v", owner, err)
	}
	if _, err := g.rdb.Eval(context.Background(), cbReportScript, circuitBreakerTestKeys(),
		1, 1, float64(g.clock().UnixNano())/1e9, 2, owner[2], "owner-b").Result(); err != nil {
		t.Fatalf("report non-owner success: %v", err)
	}
	if state := mustGet(t, mr, "cb:/users:state"); state != "2" {
		t.Fatalf("state after non-owner report = %q, want half-open (2)", state)
	}
	if token, err := mr.Get("cb:/users:probe"); err != nil || token != "owner-a" {
		t.Fatalf("probe owner after non-owner report = %q, %v; want owner-a", token, err)
	}

	// Expiration recovers an abandoned lease; its late owner must not affect
	// the replacement probe, even though both were admitted in the same epoch.
	mr.FastForward(time.Duration(cbProbeTTLMillis+1) * time.Millisecond)
	replacement, err := g.rdb.Eval(context.Background(), cbAllowScript, allowKeys, 1,
		float64(g.clock().UnixNano())/1e9, cbProbeTTLMillis, "owner-b").Int64Slice()
	if err != nil || len(replacement) != 3 || replacement[0] != 2 || replacement[1] != 1 || replacement[2] != owner[2] {
		t.Fatalf("replacement probe admission = %v, %v", replacement, err)
	}
	if _, err := g.rdb.Eval(context.Background(), cbReportScript, circuitBreakerTestKeys(),
		1, 1, float64(g.clock().UnixNano())/1e9, 2, owner[2], "owner-a").Result(); err != nil {
		t.Fatalf("report expired owner: %v", err)
	}
	if state := mustGet(t, mr, "cb:/users:state"); state != "2" {
		t.Fatalf("state after expired-owner report = %q, want half-open (2)", state)
	}
	if token, err := mr.Get("cb:/users:probe"); err != nil || token != "owner-b" {
		t.Fatalf("replacement probe owner = %q, %v; want owner-b", token, err)
	}
	if _, err := g.rdb.Eval(context.Background(), cbReportScript, circuitBreakerTestKeys(),
		1, 1, float64(g.clock().UnixNano())/1e9, 2, replacement[2], "owner-b").Result(); err != nil {
		t.Fatalf("report current owner: %v", err)
	}
	if state := mustGet(t, mr, "cb:/users:state"); state != "0" {
		t.Fatalf("state after current-owner success = %q, want closed (0)", state)
	}
}

func TestCircuitBreakerProbeTimeoutReopensCircuit(t *testing.T) {
	mr := newTestRedis(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		<-r.Context().Done()
	}))
	defer backend.Close()
	svc := testService("users", "/users", []string{backend.URL}, "circuit_breaker")
	svc.CBFailureThreshold = 1
	svc.CBResetTimeoutSeconds = 1
	g := newTestGateway(t, testConfig(mr, svc))
	g.probeTimeout = 25 * time.Millisecond
	now := time.Unix(1_700_000_000, 0)
	seedOpenCircuit(t, g, now)
	g.now = func() time.Time { return now.Add(2 * time.Second) }

	if rec := request(g, http.MethodGet, "/users", nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("timed-out probe status = %d, want 502", rec.Code)
	}
	if state := mustGet(t, mr, "cb:/users:state"); state != "1" {
		t.Fatalf("state after timed-out probe = %q, want open (1)", state)
	}
	if mr.Exists("cb:/users:probe") {
		t.Fatal("timed-out probe must release its owned probe lock")
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
		[]string{"cb:/users:state", "cb:/users:fails", "cb:/users:last", "cb:/users:probe", "cb:/users:generation"},
		1, 0, float64(g.clock().UnixNano())/1e9, 0, 0, "").Result(); err != nil {
		t.Fatalf("seed failure: %v", err)
	}
	g.now = func() time.Time { return time.Unix(1_700_000_010, 0) }

	first, err := g.rdb.Eval(context.Background(), cbAllowScript,
		[]string{"cb:/users:state", "cb:/users:last", "cb:/users:probe", "cb:/users:generation"},
		1, float64(g.clock().UnixNano())/1e9, cbProbeTTLMillis, "probe-one").Int64Slice()
	if err != nil {
		t.Fatalf("first allow: %v", err)
	}
	if len(first) != 3 || first[0] != 2 || first[1] != 1 {
		t.Fatalf("first allow = %v, want [2 1 generation]", first)
	}

	second, err := g.rdb.Eval(context.Background(), cbAllowScript,
		[]string{"cb:/users:state", "cb:/users:last", "cb:/users:probe", "cb:/users:generation"},
		1, float64(g.clock().UnixNano())/1e9, cbProbeTTLMillis, "probe-two").Int64Slice()
	if err != nil {
		t.Fatalf("second allow: %v", err)
	}
	if len(second) != 3 || second[0] != 2 || second[1] != 0 {
		t.Fatalf("second allow = %v, want [2 0 generation] (probe already in flight)", second)
	}
}

func TestCircuitBreakerAllowsOnlyOneConcurrentProbe(t *testing.T) {
	mr := newTestRedis(t)
	var backendRequests atomic.Int32
	probeEntered := make(chan struct{}, 16)
	releaseProbe := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		backendRequests.Add(1)
		probeEntered <- struct{}{}
		<-releaseProbe
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	svc := testService("users", "/users", []string{backend.URL}, "circuit_breaker")
	svc.CBFailureThreshold = 1
	svc.CBResetTimeoutSeconds = 1
	g := newTestGateway(t, testConfig(mr, svc))
	openTime := time.Unix(1_700_000_000, 0)
	g.now = func() time.Time { return openTime }
	if _, err := g.rdb.Eval(context.Background(), cbReportScript,
		[]string{"cb:/users:state", "cb:/users:fails", "cb:/users:last", "cb:/users:probe", "cb:/users:generation"},
		1, 0, float64(g.clock().UnixNano())/1e9, 0, 0, "").Result(); err != nil {
		t.Fatalf("seed open circuit: %v", err)
	}
	g.now = func() time.Time { return openTime.Add(2 * time.Second) }

	const requests = 16
	start := make(chan struct{})
	statuses := make(chan int, requests)
	for i := 0; i < requests; i++ {
		go func() {
			<-start
			statuses <- request(g, http.MethodGet, "/users", nil).Code
		}()
	}
	close(start)

	select {
	case <-probeEntered:
	case <-time.After(5 * time.Second):
		close(releaseProbe)
		t.Fatal("no half-open probe reached the backend")
	}

	// Keep the winning probe in flight while all competing requests check the
	// half-open state; they must be rejected instead of reaching the backend.
	for i := 0; i < requests-1; i++ {
		select {
		case status := <-statuses:
			if status != http.StatusServiceUnavailable {
				close(releaseProbe)
				t.Fatalf("competing request status = %d, want 503", status)
			}
		case <-time.After(5 * time.Second):
			close(releaseProbe)
			t.Fatal("timed out waiting for competing requests to be rejected")
		}
	}

	close(releaseProbe)
	select {
	case status := <-statuses:
		if status != http.StatusOK {
			t.Fatalf("probe request status = %d, want 200", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the probe response")
	}

	if got := backendRequests.Load(); got != 1 {
		t.Fatalf("backend received %d probe requests, want exactly 1", got)
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
	b := &backend{url: srv.URL, healthURL: srv.URL + "/health"}
	b.healthy.Store(true)
	client := &http.Client{Timeout: time.Second}

	checkBackend(context.Background(), context.Background(), client, svc, b)
	if b.healthy.Load() {
		t.Fatal("backend should be unhealthy after a 500 health response")
	}

	fail.Store(false)
	checkBackend(context.Background(), context.Background(), client, svc, b)
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
jwt_secret: "test-test-test-test-test-test-test-test"
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
	if !g.config.Load().auth.keys["old-key"] {
		t.Fatal("old key should be active after initial load")
	}

	writeConfig(t, path, `
port: ":0"
jwt_secret: "test-test-test-test-test-test-test-test"
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

	auth := g.config.Load().auth
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

	// A weak secret must not replace the currently working auth snapshot.
	writeConfig(t, path, `
jwt_secret: "short"
services:
  - name: replacement
    route_pattern: "/replacement"
    urls:
      - "`+backend.URL+`"
`)
	g.reloadConfig(path)
	if g.findService(httptest.NewRequest(http.MethodGet, "/products", nil)) == nil {
		t.Fatal("weak-secret config must not replace the running services")
	}
	if !g.config.Load().auth.keys["new-key"] {
		t.Fatal("weak-secret config must not replace the running auth config")
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
		{"route without leading slash", &Config{Services: []Service{{Name: "x", Route: "x", Urls: []string{"http://a"}}}}},
		{"route trailing slash", &Config{Services: []Service{{Name: "x", Route: "/x/", Urls: []string{"http://a"}}}}},
		{"negative rate", &Config{Services: []Service{{Name: "x", Route: "/x", Urls: []string{"http://a"}, RateLimitRPS: -1}}}},
		{"negative circuit threshold", &Config{Services: []Service{{Name: "x", Route: "/x", Urls: []string{"http://a"}, CBFailureThreshold: -1}}}},
		{"backend credentials", &Config{Services: []Service{{Name: "x", Route: "/x", Urls: []string{"http://user:secret@a"}}}}},
		{"missing service name", &Config{Services: []Service{{Route: "/x", Urls: []string{"http://a"}}}}},
		{"unknown middleware", &Config{Services: []Service{{Name: "x", Route: "/x", Urls: []string{"http://a"}, Middleware: []string{"auth_api_key"}}}}},
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
