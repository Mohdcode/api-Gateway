package gateway

import (
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

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
