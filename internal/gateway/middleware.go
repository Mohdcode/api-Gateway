package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	healthCheckInterval = 10 * time.Second
	healthCheckTimeout  = 2 * time.Second
	cbProbeTTLMillis    = 30_000
)

// Populated at build time with -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
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

// rateLimitScript implements a token bucket atomically in Redis.
// KEYS[1] = bucket key
// ARGV[1] = rate (tokens/second), ARGV[2] = capacity,
// ARGV[3] = now (unix ms), ARGV[4] = key TTL (ms)
const rateLimitScript = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local bucket = redis.call('HMGET', key, 'tokens', 'last')
local tokens = tonumber(bucket[1])
local last = tonumber(bucket[2])
if tokens == nil then
	tokens = capacity
	last = now
end

local elapsed = math.max(0, now - last) / 1000
tokens = math.min(capacity, tokens + (elapsed * rate))

local allowed = 0
if tokens >= 1 then
	tokens = tokens - 1
	allowed = 1
end

redis.call('HSET', key, 'tokens', tokens, 'last', now)
redis.call('PEXPIRE', key, ttl)
return allowed
`

// cbAllowScript decides whether a request may pass and performs the
// closed -> open -> half-open transition atomically across gateway instances.
// KEYS: state, last failure, probe lock
// ARGV: reset timeout (s), now (unix s), probe TTL (ms)
// Returns {state, allowed}.
const cbAllowScript = `
local stateKey = KEYS[1]
local lastKey = KEYS[2]
local probeKey = KEYS[3]
local reset = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local probeTTL = tonumber(ARGV[3])

local state = tonumber(redis.call('GET', stateKey) or '0')

if state == 0 then
	return {0, 1}
end

if state == 1 then
	local last = tonumber(redis.call('GET', lastKey) or '0')
	if last > 0 and (now - last) >= reset then
		redis.call('SET', stateKey, 2)
		state = 2
	else
		return {1, 0}
	end
end

if redis.call('EXISTS', probeKey) == 0 then
	redis.call('SET', probeKey, '1')
	redis.call('PEXPIRE', probeKey, probeTTL)
	return {2, 1}
end
return {2, 0}
`

// cbReportScript records the outcome of an allowed request.
// KEYS: state, failures, last failure, probe lock
// ARGV: failure threshold, success (1/0), now (unix s)
// Returns the resulting state.
const cbReportScript = `
local stateKey = KEYS[1]
local failsKey = KEYS[2]
local lastKey = KEYS[3]
local probeKey = KEYS[4]
local threshold = tonumber(ARGV[1])
local success = tonumber(ARGV[2])
local now = tonumber(ARGV[3])

redis.call('DEL', probeKey)

if success == 1 then
	redis.call('SET', stateKey, 0)
	redis.call('DEL', failsKey)
	return 0
end

local state = tonumber(redis.call('GET', stateKey) or '0')

if state == 2 then
	redis.call('SET', stateKey, 1)
	redis.call('SET', lastKey, now)
	return 1
end

local fails = redis.call('INCR', failsKey)
if fails >= threshold then
	redis.call('SET', stateKey, 1)
	redis.call('SET', lastKey, now)
	return 1
end

return state
`

func hashSecret(s, key string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(s))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func (g *Gateway) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *Gateway) register(name string, wrapper MiddlewareWrapper) {
	g.wrappers[name] = wrapper
	log.Printf("Registered a middleware %s", name)
}

func (g *Gateway) authAPIKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := g.auth.Load()
		if auth == nil {
			http.Error(w, "Authentication Failed", http.StatusUnauthorized)
			return
		}

		if scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
			token, err := jwt.Parse(strings.TrimSpace(raw), func(token *jwt.Token) (any, error) {
				return []byte(auth.jwtSecret), nil
			}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
			if err == nil && token.Valid {
				subject, _ := token.Claims.GetSubject()
				identity := "jwt:" + subject
				if subject == "" {
					identity = "jwt:" + hashSecret(raw, auth.jwtSecret)
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, identity)))
				return
			}
			log.Printf("invalid bearer token: %v", err)
		}

		apiKey := r.Header.Get("X-API-KEY")
		if apiKey == "" || !auth.keys[apiKey] {
			http.Error(w, "Authentication Failed", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, "key:"+hashSecret(apiKey, auth.jwtSecret))))
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
		if service := g.findService(r); service != nil {
			serviceName = service.Name
		}
		next.ServeHTTP(recorder, r)
		counter.WithLabelValues(r.Method, fmt.Sprintf("%d", recorder.statusCode), serviceName).Inc()
		timer.WithLabelValues(r.Method, serviceName).Observe(time.Since(startTime).Seconds())
	})
}

func (g *Gateway) clientIdentity(r *http.Request) string {
	if id, ok := r.Context().Value(identityKey).(string); ok && id != "" {
		return id
	}
	if apiKey := r.Header.Get("X-API-KEY"); apiKey != "" {
		if auth := g.auth.Load(); auth != nil && auth.jwtSecret != "" {
			return "key:" + hashSecret(apiKey, auth.jwtSecret)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "ip:" + r.RemoteAddr
	}
	return "ip:" + host
}

func (g *Gateway) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service := g.findService(r)
		rate := 100.0
		burst := 200.0
		serviceName := "unknown"
		route := ""
		if service != nil {
			serviceName = service.Name
			route = service.Route
			if service.RateLimitRPS > 0 {
				rate = service.RateLimitRPS
			}
			if service.RateLimitBurst > 0 {
				burst = service.RateLimitBurst
			}
		}

		if g.rdb == nil {
			next.ServeHTTP(w, r)
			return
		}

		key := "rl:" + g.clientIdentity(r) + ":" + route
		ttl := int64(math.Max(60_000, (burst/rate)*2_000))
		allowed, err := g.rdb.Eval(r.Context(), rateLimitScript, []string{key},
			rate, burst, g.clock().UnixMilli(), ttl).Int()

		if err != nil {
			// Fail open: Redis being down must not take the whole gateway down.
			log.Printf("rate limiter unavailable, allowing request: %v", err)
			rateLimited.WithLabelValues(serviceName, "redis_error").Inc()
			next.ServeHTTP(w, r)
			return
		}
		if allowed == 0 {
			rateLimited.WithLabelValues(serviceName, "token_bucket").Inc()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) circuitBreakerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		service := g.findService(r)
		if service == nil || g.rdb == nil {
			next.ServeHTTP(w, r)
			return
		}

		threshold := service.CBFailureThreshold
		if threshold <= 0 {
			threshold = 5
		}
		resetSeconds := service.CBResetTimeoutSeconds
		if resetSeconds <= 0 {
			resetSeconds = 10
		}

		base := "cb:" + service.Route
		keys := []string{base + ":state", base + ":fails", base + ":last", base + ":probe"}

		res, err := g.rdb.Eval(r.Context(), cbAllowScript,
			[]string{keys[0], keys[2], keys[3]},
			resetSeconds, float64(g.clock().UnixNano())/1e9, cbProbeTTLMillis).Int64Slice()
		if err != nil || len(res) != 2 {
			// Fail open on Redis errors.
			log.Printf("circuit breaker unavailable for %s, allowing request: %v", service.Name, err)
			next.ServeHTTP(w, r)
			return
		}

		state, allowed := res[0], res[1] == 1
		cbState.WithLabelValues(service.Name).Set(float64(state))
		if !allowed {
			http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
			return
		}

		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)

		success := int64(0)
		if rec.statusCode < 500 {
			success = 1
		}
		newState, err := g.rdb.Eval(context.Background(), cbReportScript, keys,
			threshold, success, float64(g.clock().UnixNano())/1e9).Int64()
		if err != nil {
			log.Printf("failed to record circuit breaker outcome for %s: %v", service.Name, err)
			return
		}
		cbState.WithLabelValues(service.Name).Set(float64(newState))
	})
}
