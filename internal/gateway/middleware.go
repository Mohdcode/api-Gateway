package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
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
	cbProbeTTLMillis    = 60_000
	cbProbeTimeout      = 30 * time.Second
	cbReportTimeout     = 2 * time.Second
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
	}, []string{"service"},
)

var redisOperationErrors = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gateway_redis_operation_errors_total",
		Help: "Redis operation failures that affected gateway behavior",
	}, []string{"service", "operation"},
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
	prometheus.MustRegister(counter, timer, rateLimited, redisOperationErrors, cbState, proxyCanceledCounter)
}

// rateLimitScript implements a token bucket atomically in Redis.
// KEYS[1] = bucket key
// ARGV[1] = rate (tokens/second), ARGV[2] = capacity,
// ARGV[3] = key TTL (ms). Redis TIME provides a consistent clock across instances.
const rateLimitScript = `
local key = KEYS[1]
local rate = tonumber(ARGV[1])
local capacity = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local redisTime = redis.call('TIME')
local now = (tonumber(redisTime[1]) * 1000) + math.floor(tonumber(redisTime[2]) / 1000)

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
local retryAfterMs = 0
if tokens >= 1 then
	tokens = tokens - 1
	allowed = 1
else
	retryAfterMs = math.ceil(((1 - tokens) / rate) * 1000)
end

redis.call('HSET', key, 'tokens', tokens, 'last', now)
redis.call('PEXPIRE', key, ttl)
return {allowed, retryAfterMs}
`

// cbAllowScript decides whether a request may pass and performs the
// closed -> open -> half-open transition atomically across gateway instances.
// KEYS: state, last failure, probe lock, generation
// ARGV: reset timeout (s), now (unix s), probe TTL (ms), probe token
// Returns {state, allowed, generation}.
const cbAllowScript = `
local stateKey = KEYS[1]
local lastKey = KEYS[2]
local probeKey = KEYS[3]
local generationKey = KEYS[4]
local reset = tonumber(ARGV[1])
local now = tonumber(ARGV[2])
local probeTTL = tonumber(ARGV[3])
local probeToken = ARGV[4]

local state = tonumber(redis.call('GET', stateKey) or '0')
local generation = tonumber(redis.call('GET', generationKey) or '0')

if state == 0 then
	return {0, 1, generation}
end

if state == 1 then
	local last = tonumber(redis.call('GET', lastKey) or '0')
	if last > 0 and (now - last) >= reset then
		redis.call('SET', stateKey, 2)
		generation = redis.call('INCR', generationKey)
		state = 2
	else
		return {1, 0, generation}
	end
end

if redis.call('EXISTS', probeKey) == 0 then
	redis.call('SET', probeKey, probeToken)
	redis.call('PEXPIRE', probeKey, probeTTL)
	return {2, 1, generation}
end
return {2, 0, generation}
`

// cbReportScript records the outcome of an allowed request.
// KEYS: state, failures, last failure, probe lock, generation
// ARGV: failure threshold, success (1/0), now (unix s), admitted state,
// generation at admission, probe token
// Returns the resulting state.
const cbReportScript = `
local stateKey = KEYS[1]
local failsKey = KEYS[2]
local lastKey = KEYS[3]
local probeKey = KEYS[4]
local generationKey = KEYS[5]
local threshold = tonumber(ARGV[1])
local success = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local admittedState = tonumber(ARGV[4])
local admittedGeneration = tonumber(ARGV[5])
local probeToken = ARGV[6]

local state = tonumber(redis.call('GET', stateKey) or '0')
local generation = tonumber(redis.call('GET', generationKey) or '0')
if generation ~= admittedGeneration then
	return state
end

if success == -1 then
	if admittedState == 2 and state == 2 and redis.call('GET', probeKey) == probeToken then
		redis.call('DEL', probeKey)
	end
	return state
end

if admittedState == 2 then
	if state ~= 2 or redis.call('GET', probeKey) ~= probeToken then
		return state
	end
	redis.call('DEL', probeKey)
	if success == 1 then
		redis.call('SET', stateKey, 0)
		redis.call('DEL', failsKey)
		redis.call('DEL', lastKey)
		redis.call('INCR', generationKey)
		return 0
	end
	redis.call('SET', stateKey, 1)
	redis.call('SET', lastKey, now)
	redis.call('INCR', generationKey)
	return 1
end

if admittedState ~= 0 or state ~= 0 then
	return state
end

if success == 1 then
	redis.call('DEL', failsKey)
	return 0
end

local fails = redis.call('INCR', failsKey)
if fails >= threshold then
	redis.call('SET', stateKey, 1)
	redis.call('SET', lastKey, now)
	redis.call('INCR', generationKey)
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
		snapshot := g.configurationForRequest(r)
		if snapshot == nil || snapshot.auth == nil {
			http.Error(w, "Authentication Failed", http.StatusUnauthorized)
			return
		}
		auth := snapshot.auth

		if scheme, raw, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
			parserOptions := []jwt.ParserOption{
				jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
				jwt.WithExpirationRequired(),
			}
			if auth.jwtIssuer != "" {
				parserOptions = append(parserOptions, jwt.WithIssuer(auth.jwtIssuer))
			}
			if auth.jwtAudience != "" {
				parserOptions = append(parserOptions, jwt.WithAudience(auth.jwtAudience))
			}
			token, err := jwt.Parse(strings.TrimSpace(raw), func(token *jwt.Token) (any, error) {
				return []byte(auth.jwtSecret), nil
			}, parserOptions...)
			if err == nil && token.Valid {
				subject, _ := token.Claims.GetSubject()
				identity := "jwt:" + subject
				if subject == "" {
					identity = "jwt:" + hashSecret(raw, auth.jwtSecret)
				}
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey, identity)))
				return
			}
			log.Printf("invalid bearer token rejected")
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
		var auth *authConfig
		if snapshot := g.configurationForRequest(r); snapshot != nil {
			auth = snapshot.auth
		}
		if auth != nil && auth.jwtSecret != "" && auth.keys[apiKey] {
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
		result, err := g.rdb.Eval(r.Context(), rateLimitScript, []string{key}, rate, burst, ttl).Int64Slice()

		if err != nil || len(result) != 2 {
			// Fail open: Redis being down must not take the whole gateway down.
			log.Printf("rate limiter unavailable, allowing request: %v", err)
			redisOperationErrors.WithLabelValues(serviceName, "rate_limit").Inc()
			next.ServeHTTP(w, r)
			return
		}
		if result[0] == 0 {
			rateLimited.WithLabelValues(serviceName).Inc()
			retrySeconds := (result[1] + 999) / 1000
			if retrySeconds < 1 {
				retrySeconds = 1
			}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retrySeconds))
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

		probeTokenBytes := make([]byte, 16)
		if _, err := rand.Read(probeTokenBytes); err != nil {
			log.Printf("circuit breaker token generation failed for %s: %v", service.Name, err)
			http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		probeToken := hex.EncodeToString(probeTokenBytes)
		base := "cb:" + service.Route
		keys := []string{base + ":state", base + ":fails", base + ":last", base + ":probe", base + ":generation"}

		res, err := g.rdb.Eval(r.Context(), cbAllowScript,
			[]string{keys[0], keys[2], keys[3], keys[4]},
			resetSeconds, float64(g.clock().UnixNano())/1e9, cbProbeTTLMillis, probeToken).Int64Slice()
		if err != nil || len(res) != 3 {
			// Fail open on Redis errors.
			log.Printf("circuit breaker unavailable for %s, allowing request: %v", service.Name, err)
			redisOperationErrors.WithLabelValues(service.Name, "circuit_breaker_check").Inc()
			next.ServeHTTP(w, r)
			return
		}

		state, allowed, generation := res[0], res[1] == 1, res[2]
		cbState.WithLabelValues(service.Name).Set(float64(state))
		if !allowed {
			http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
			return
		}

		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		nextReq := r
		if state == 2 {
			probeTimeout := g.probeTimeout
			if probeTimeout <= 0 {
				probeTimeout = cbProbeTimeout
			}
			probeCtx, cancel := context.WithTimeout(r.Context(), probeTimeout)
			defer cancel()
			nextReq = r.WithContext(probeCtx)
		}
		next.ServeHTTP(rec, nextReq)

		success := int64(-1)
		if r.Context().Err() == nil {
			success = 0
			if rec.statusCode < 500 {
				success = 1
			}
		}
		reportCtx, cancel := context.WithTimeout(context.Background(), cbReportTimeout)
		defer cancel()
		newState, err := g.rdb.Eval(reportCtx, cbReportScript, keys,
			threshold, success, float64(g.clock().UnixNano())/1e9, state, generation, probeToken).Int64()
		if err != nil {
			log.Printf("failed to record circuit breaker outcome for %s: %v", service.Name, err)
			redisOperationErrors.WithLabelValues(service.Name, "circuit_breaker_report").Inc()
			return
		}
		cbState.WithLabelValues(service.Name).Set(float64(newState))
	})
}
