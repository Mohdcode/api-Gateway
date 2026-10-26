package gateway

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func (g *Gateway) Close() error {
	if g.rdb == nil {
		return nil
	}
	return g.rdb.Close()
}

func (g *Gateway) healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (g *Gateway) readyzHandler(w http.ResponseWriter, r *http.Request) {
	if g.services.Load() == nil {
		http.Error(w, "configuration not loaded", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

func versionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version":   version,
			"commit":    commit,
			"buildDate": buildDate,
			"goVersion": runtime.Version(),
		})
	}
}

// handler builds the HTTP mux shared by the server and the tests.
func (g *Gateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(g.handleGateway))
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", g.healthzHandler)
	mux.HandleFunc("/readyz", g.readyzHandler)
	mux.HandleFunc("/version", versionHandler())
	return mux
}

func checkHealth(addr string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func probeAddress() string {
	if addr := os.Getenv("HEALTHCHECK_ADDR"); addr != "" {
		return addr
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = ":8080"
	}
	if strings.HasPrefix(port, ":") {
		return "127.0.0.1" + port
	}
	if host, p, err := net.SplitHostPort(port); err == nil {
		_ = host
		return net.JoinHostPort("127.0.0.1", p)
	}
	return "127.0.0.1:" + port
}
