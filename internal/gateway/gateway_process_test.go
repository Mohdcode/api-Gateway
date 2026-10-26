package gateway

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestProcessLifecycle builds the real binary and verifies the operational
// contract used by containers and Kubernetes: /healthz, the -healthcheck
// subcommand, and graceful shutdown on SIGTERM with exit code 0.
func TestProcessLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping process integration test in short mode")
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, "gateway")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/gateway")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	configPath := filepath.Join(dir, "config.yaml")
	config := fmt.Sprintf(`port: ":%d"
jwt_secret: "0123456789abcdef0123456789abcdef"
redis:
  addr: "127.0.0.1:1"
  db: 0
middleware_config:
  auth_apikey:
    valid_keys:
      - "key-a"
services: []
`, port)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	env := append(os.Environ(), fmt.Sprintf("PORT=:%d", port), "PPROF_ADDR=off", "SHUTDOWN_DELAY=0")
	var output bytes.Buffer
	cmd := exec.Command(binary, configPath)
	cmd.Env = env
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
		}
	})

	healthy := false
	for i := 0; i < 100; i++ {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthy = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthy {
		t.Fatalf("gateway did not become healthy; output:\n%s", output.String())
	}

	healthcheck := exec.Command(binary, "-healthcheck")
	healthcheck.Env = env
	if out, err := healthcheck.CombinedOutput(); err != nil {
		t.Fatalf("-healthcheck: %v\n%s", err, out)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("gateway exited with error after SIGTERM: %v\n%s", err, output.String())
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("gateway did not shut down within 15s")
	}

	if !strings.Contains(output.String(), "gateway stopped") {
		t.Fatalf("expected graceful shutdown log, got:\n%s", output.String())
	}

	failing := exec.Command(binary, "-healthcheck")
	failing.Env = env
	if err := failing.Run(); err == nil {
		t.Fatal("expected -healthcheck to fail after the gateway stopped")
	}
}
