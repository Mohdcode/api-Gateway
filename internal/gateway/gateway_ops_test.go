package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOperationalEndpoints(t *testing.T) {
	mr := newTestRedis(t)
	gw := newTestGateway(t, testConfig(mr))
	t.Cleanup(func() { _ = gw.Close() })
	srv := httptest.NewServer(gw.handler())
	t.Cleanup(srv.Close)

	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	if resp := get("/healthz"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	if resp := get("/readyz"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz status = %d, want 200", resp.StatusCode)
	}
	if resp := get("/metrics"); resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}

	resp := get("/version")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/version status = %d, want 200", resp.StatusCode)
	}
	var payload map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode /version: %v", err)
	}
	if payload["version"] != version {
		t.Fatalf("/version version = %q, want %q", payload["version"], version)
	}
	if payload["goVersion"] == "" {
		t.Fatal("/version should report the Go runtime version")
	}
}

func TestReadyzFailsWithoutConfiguration(t *testing.T) {
	g := &Gateway{}
	rec := httptest.NewRecorder()
	g.readyzHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want 503", rec.Code)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("PORT", ":9090")
	t.Setenv("JWT_SECRET", "env-secret")
	t.Setenv("REDIS_ADDR", "redis:6379")
	t.Setenv("REDIS_PASSWORD", "redis-fixture")
	t.Setenv("REDIS_DB", "3")
	t.Setenv("API_KEYS", " key-1, key-2 ,, ")

	cfg := &Config{}
	applyEnvOverrides(cfg)

	if cfg.Port != ":9090" {
		t.Fatalf("port = %q", cfg.Port)
	}
	if cfg.JwtSecret != "env-secret" {
		t.Fatalf("jwt secret = %q", cfg.JwtSecret)
	}
	if cfg.Redis.Addr != "redis:6379" || cfg.Redis.Password != "redis-fixture" || cfg.Redis.DB != 3 {
		t.Fatalf("redis config = %+v", cfg.Redis)
	}
	if got := strings.Join(cfg.MiddlewareConfig.ApiKeyAuth.ValidKeys, ","); got != "key-1,key-2" {
		t.Fatalf("api keys = %q", got)
	}
}

func TestApplyEnvOverridesIgnoresInvalidDB(t *testing.T) {
	t.Setenv("REDIS_DB", "not-a-number")
	cfg := &Config{Redis: RedisConfig{DB: 2}}
	applyEnvOverrides(cfg)
	if cfg.Redis.DB != 2 {
		t.Fatalf("invalid REDIS_DB should be ignored, db = %d", cfg.Redis.DB)
	}
}

func TestCheckHealth(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(healthy.Close)
	if err := checkHealth(strings.TrimPrefix(healthy.URL, "http://")); err != nil {
		t.Fatalf("healthy backend: %v", err)
	}

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unhealthy.Close)
	if err := checkHealth(strings.TrimPrefix(unhealthy.URL, "http://")); err == nil {
		t.Fatal("expected an error for a 503 health response")
	}
}

func TestShutdownDelay(t *testing.T) {
	t.Setenv("SHUTDOWN_DELAY", "5s")
	if got := shutdownDelay(); got != 5*time.Second {
		t.Fatalf("shutdown delay = %s", got)
	}

	t.Setenv("SHUTDOWN_DELAY", "not-a-duration")
	if got := shutdownDelay(); got != 0 {
		t.Fatalf("invalid shutdown delay = %s, want 0", got)
	}

	t.Setenv("SHUTDOWN_DELAY", "-1s")
	if got := shutdownDelay(); got != 0 {
		t.Fatalf("negative shutdown delay = %s, want 0", got)
	}
}

func TestProbeAddress(t *testing.T) {
	t.Setenv("HEALTHCHECK_ADDR", "127.0.0.1:9999")
	if got := probeAddress(); got != "127.0.0.1:9999" {
		t.Fatalf("probe address = %q", got)
	}

	t.Setenv("HEALTHCHECK_ADDR", "")
	t.Setenv("PORT", "0.0.0.0:8080")
	if got := probeAddress(); got != "127.0.0.1:8080" {
		t.Fatalf("probe address = %q", got)
	}

	t.Setenv("PORT", ":8081")
	if got := probeAddress(); got != "127.0.0.1:8081" {
		t.Fatalf("probe address = %q", got)
	}
}
