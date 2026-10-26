package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
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

func (rec *statusRecorder) Flush() {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rec *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rec.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

func (rec *statusRecorder) Push(target string, opts *http.PushOptions) error {
	p, ok := rec.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

func (rec *statusRecorder) ReadFrom(r io.Reader) (int64, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	if rf, ok := rec.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(rec.ResponseWriter, r)
}

type Config struct {
	Port             string      `yaml:"port"`
	JwtSecret        string      `yaml:"jwt_secret"`
	JwtIssuer        string      `yaml:"jwt_issuer"`
	JwtAudience      string      `yaml:"jwt_audience"`
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
	url       string
	healthURL string
	proxy     *httputil.ReverseProxy
	conns     atomic.Int64
	healthy   atomic.Bool
	checked   atomic.Bool
}

type MiddlewareWrapper func(next http.Handler) http.Handler

type authConfig struct {
	keys        map[string]bool
	jwtSecret   string
	jwtIssuer   string
	jwtAudience string
}

type configSnapshot struct {
	services map[string]*Service
	auth     *authConfig
}

type Gateway struct {
	config       atomic.Pointer[configSnapshot]
	wrappers     map[string]MiddlewareWrapper
	transport    *http.Transport
	rdb          *redis.Client
	now          func() time.Time
	probeTimeout time.Duration
	workerCtx    context.Context
	workerCancel context.CancelFunc
	workerWG     sync.WaitGroup
	closeOnce    sync.Once
	closeErr     error
	draining     atomic.Bool
}

type ctxKey string

const identityKey ctxKey = "gateway.identity"
const configSnapshotKey ctxKey = "gateway.config_snapshot"
