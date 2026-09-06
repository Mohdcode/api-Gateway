# API Gateway

A high-performance API Gateway built in Go with middleware support for authentication, rate limiting, circuit breaking, load balancing, and Prometheus metrics.

## Overview

This project implements a production-ready API Gateway that sits between clients and backend services. It provides:

- **Load Balancing**: Distributes requests across multiple backend instances using round-robin algorithm
- **API Key Authentication**: Validates requests using API keys in `X-API-KEY` header
- **Rate Limiting**: Token bucket algorithm to prevent service abuse (configurable per service)
- **Circuit Breaker**: Protects backends from cascading failures with automatic recovery
- **Metrics & Monitoring**: Prometheus metrics for request counting, latency tracking, and circuit breaker states
- **Profiling**: Built-in pprof support for performance analysis

## Architecture

```
Client → API Gateway (Port 8080) → Backend Services
                                   ├─ Backend 1 (Port 8081)
                                   ├─ Backend 2 (Port 8082)
                                   └─ Backend 3 (Port 8083)
```

The gateway routes incoming requests to configured services based on URL patterns and applies middleware in a configurable order.

## Testing & Verification

This isn't just a spec — every feature below was run and verified end-to-end locally.

**Routing + authentication:**
```bash
curl -H "X-API-KEY: abc-123" http://localhost:8080/users
# → Hello from the backend-1 running on port 8083

curl http://localhost:8080/users
# → Authentication Failed
```
Confirmed: valid keys route through to a real backend; missing keys are correctly rejected.

**Round-robin load balancing** — 6 consecutive requests, alternating cleanly across the two configured backends:
```bash
for i in {1..6}; do curl -H "X-API-KEY: abc-123" http://localhost:8080/users; echo; done
```
```
Hello from the backend-1 running on port 8083
Hello from the backend-1 running on port 8081
Hello from the backend-1 running on port 8083
Hello from the backend-1 running on port 8081
Hello from the backend-1 running on port 8083
Hello from the backend-1 running on port 8081
```
Confirmed: the atomic counter in `NewProxy()` correctly cycles through backends in order.

**Rate limiter under real concurrent load** — 1000 requests fired with 100 concurrent workers (`xargs -P 100`), against a configured limit of 200 rps / 400 burst:
```bash
seq 1 1000 | xargs -P 100 -I {} curl -s -o /dev/null -w "%{http_code}\n" \
  -H "X-API-KEY: abc-123" http://localhost:8080/users | sort | uniq -c
```
```
 662 200
 338 429
```
Confirmed: the mutex-protected token bucket correctly allowed ~662 requests through and rejected ~338 with `429 Too Many Requests` once burst capacity was exceeded — verifying it's safe under genuine concurrent access, not just sequential requests (a sequential 500-request test with no concurrency, by contrast, produced all `200`s, since the bucket had time to refill between requests — a useful reminder that rate limiters need to be tested concurrently to actually exercise their logic).

## Features

### Middleware Stack

1. **API Key Authentication** (`auth_apikey`)
   - Validates API keys from request headers
   - Configurable valid keys in `config.yaml`

2. **Rate Limiting** (`rate_limit`)
   - Token bucket implementation, protected by a `sync.Mutex` per client/service key
   - Per-client rate limiting (by API key or IP)
   - Configurable requests per second and burst capacity

3. **Circuit Breaker** (`circuit_breaker`)
   - Three states: Closed, Open, Half-Open — implemented as an explicit state machine
   - Automatic recovery after configured timeout via a half-open probe request
   - Protects backends from overload; opens after a configurable failure threshold

4. **Metrics** (`metrics`)
   - HTTP request counter (method, status, service)
   - Request duration histogram
   - Rate limit rejection counter
   - Circuit breaker state gauge
   - Proxy cancellation counter

### Load Balancing

- Round-robin distribution across configured backend URLs
- Implemented with `atomic.AddUint64`, not a mutex — a lighter-weight primitive appropriate for a simple counter increment, avoiding lock contention under high request volume

### High Performance

- Shared HTTP transport with connection pooling
- Configurable connection limits (20,000 max idle, 5,000 per host)
- Optimized timeouts for TLS, idle connections, and response headers

## Prerequisites

- **Go 1.25.4** or higher
- **Prometheus** (optional, for metrics visualization)
- Basic understanding of HTTP and RESTful APIs

## Installation

1. **Clone the repository**
   ```bash
   git clone https://github.com/Mohdcode/api-Gateway.git
   cd api-Gateway
   ```

2. **Install dependencies**
   ```bash
   go mod download
   ```

3. **Verify installation**
   ```bash
   go mod verify
   ```

## Configuration

Edit `config.yaml` to configure the gateway:

```yaml
port: ":8080"

# API Key authentication configuration
middleware_config:
  auth_apikey:
    valid_keys:
      - "abc-123"
      - "dwe-569"

# Service definitions
services:
  - name: users
    route_pattern: "/users"
    urls:
      - "http://localhost:8081"
      - "http://localhost:8083"
    middleware:
      - "auth_apikey"
      - "rate_limit"
      - "circuit_breaker"
      - "metrics"
    
    # Rate limiting configuration
    rate_limit_rps: 200        # Requests per second
    rate_limit_burst: 400      # Burst capacity
    
    # Circuit breaker configuration
    cb_failure_threshold: 6              # Failures before opening circuit
    cb_reset_timeout_seconds: 15         # Timeout before attempting recovery
```

### Configuration Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `port` | Gateway listening port | `:8080` |
| `valid_keys` | List of valid API keys | - |
| `route_pattern` | URL pattern to match | - |
| `urls` | Backend service URLs | - |
| `middleware` | Middleware stack (order matters) | - |
| `rate_limit_rps` | Requests per second limit | 100 |
| `rate_limit_burst` | Token bucket capacity | 200 |
| `cb_failure_threshold` | Failures before circuit opens | 5 |
| `cb_reset_timeout_seconds` | Circuit recovery timeout | 10 |

## Running the Project

### 1. Start Backend Services

Open separate terminal windows for each backend:

**Backend 1:**
```bash
cd backend1
go run backend1.go
```

**Backend 2:**
```bash
cd backend2
go run backend2.go
```

**Backend 3:**
```bash
cd backend3
go run backend.go
```

### 2. Start the API Gateway

```bash
go run gateway.go
```

Or specify a custom config file:
```bash
go run gateway.go /path/to/custom-config.yaml
```

### 3. Test the Gateway

**Basic request with API key:**
```bash
curl -H "X-API-KEY: abc-123" http://localhost:8080/users
```

**Expected response:**
```
Hello from the backend-1 running on port 8081
```

or

```
Hello from the backend-1 running on port 8083
```

(Alternates based on round-robin load balancing)

**Without API key (will fail):**
```bash
curl http://localhost:8080/users
```

**Response:**
```
Authentication Failed
```

## Monitoring with Prometheus

### 1. Start Prometheus

If you have Docker:
```bash
docker run -d -p 9090:9090 -v $(pwd)/prometheus.yml:/etc/prometheus/prometheus.yml prom/prometheus
```

Or install Prometheus locally and run:
```bash
prometheus --config.file=prometheus.yml
```

### 2. Access Prometheus

Open your browser: `http://localhost:9090`

### 3. Available Metrics

- `http_request_counter` - Total HTTP requests (by method, status, service)
- `http_request_timer` - Request duration histogram
- `gateway_rate_limited_total` - Rate limited requests
- `gateway_circuit_breaker_state` - Circuit breaker state (0=closed, 1=open, 2=half-open)
- `gateway_proxy_canceled_total` - Client-canceled requests

### Example Queries

```promql
# Request rate per service
rate(http_request_counter[1m])

# 95th percentile latency
histogram_quantile(0.95, rate(http_request_timer_bucket[5m]))

# Rate limited requests
sum(rate(gateway_rate_limited_total[1m])) by (service)

# Circuit breaker state
gateway_circuit_breaker_state
```

## Profiling

Access pprof endpoints at `http://localhost:6060/debug/pprof/`

**CPU Profile:**
```bash
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
```

**Memory Profile:**
```bash
go tool pprof http://localhost:6060/debug/pprof/heap
```

**Goroutine Profile:**
```bash
go tool pprof http://localhost:6060/debug/pprof/goroutine
```

## Load Testing

You can use tools like `wrk` or `hey` for load testing:

**Using wrk:**
```bash
wrk -t4 -c100 -d60s -H "X-API-KEY: abc-123" http://localhost:8080/users
```

**Using hey:**
```bash
hey -n 10000 -c 100 -H "X-API-KEY: abc-123" http://localhost:8080/users
```

Or the quick concurrent test used above, with no extra tools required:
```bash
seq 1 1000 | xargs -P 100 -I {} curl -s -o /dev/null -w "%{http_code}\n" \
  -H "X-API-KEY: abc-123" http://localhost:8080/users | sort | uniq -c
```

## Project Structure

```
.
├── gateway.go              # Main gateway implementation
├── config.yaml            # Gateway configuration
├── go.mod                 # Go module dependencies
├── prometheus.yml         # Prometheus configuration
├── backend1/
│   └── backend1.go       # Backend service 1 (port 8081)
├── backend2/
│   └── backend2.go       # Backend service 2 (port 8082)
└── backend3/
    └── backend.go        # Backend service 3 (port 8083)
```

## Development

### Adding New Services

1. Add service configuration to `config.yaml`:
   ```yaml
   services:
     - name: products
       route_pattern: "/products"
       urls:
         - "http://localhost:9001"
       middleware:
         - "auth_apikey"
         - "rate_limit"
         - "circuit_breaker"
         - "metrics"
   ```

2. Start your backend service on the configured port

3. Restart the gateway to load new configuration

### Creating Custom Middleware

Add your middleware function in `gateway.go`:

```go
func (g *Gateway) customMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Your logic here
        next.ServeHTTP(w, r)
    })
}
```

Register it in `NewGateway()`:
```go
gw.register("custom", gw.customMiddleware)
```

Use it in `config.yaml`:
```yaml
middleware:
  - "custom"
```

## Troubleshooting

### Gateway won't start
- Check if port 8080 is already in use: `lsof -i :8080`
- Verify `config.yaml` syntax is correct
- Ensure all required fields are present in configuration

### Authentication failures
- Verify API key in request header: `X-API-KEY: abc-123`
- Check that the key exists in `config.yaml` under `valid_keys`

### Rate limiting issues
- Adjust `rate_limit_rps` and `rate_limit_burst` in service configuration
- Rate limits are per-client (by API key or IP address)

### Circuit breaker opening
- Check backend service health
- Review `cb_failure_threshold` and `cb_reset_timeout_seconds` settings
- Monitor Prometheus metrics for backend errors

### Backend connection errors
- Ensure all backend services are running
- Verify URLs in `config.yaml` match backend ports
- Check firewall/network settings

## Performance Considerations

- **Connection Pooling**: Shared transport with 20K max idle connections
- **Timeouts**: 
  - Idle connection: 90s
  - TLS handshake: 10s
  - Response header: 30s
- **Concurrency**: Rate limiting uses a `sync.Mutex`-protected token bucket per client; load balancing uses `atomic.AddUint64` for the round-robin counter; `sync.Map` stores per-client rate limiters safely across concurrent goroutines
- **Memory**: Consider adjusting connection pool sizes for your workload

## Why I built this

I wanted hands-on experience with the core reliability patterns used in real production API gateways — rate limiting, circuit breaking, and load balancing — and to understand Go's concurrency primitives (mutexes, atomics, sync.Map) by applying them to genuine shared-state problems, rather than just reading about them.

## Contributing

Contributions are welcome! Please:
1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests if applicable
5. Submit a pull request

## Author

Built by [Mohd Kamaal](https://github.com/Mohdcode)

## Support

For issues, questions, or suggestions:
- Check existing documentation
- Review Prometheus metrics for insights
- Enable debug logging for troubleshooting