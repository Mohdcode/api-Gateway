package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func RunHealthCheck() int {
	if err := checkHealth(probeAddress()); err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		return 1
	}
	return 0
}

func Run(configPath string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if envConfigPath := os.Getenv("CONFIG_PATH"); envConfigPath != "" {
		configPath = envConfigPath
	} else if configPath == "" {
		configPath = "config.yaml"
	}
	cfg, err := loadConfigWithEnv(configPath)
	if err != nil {
		log.Printf("%v", err)
		return 1
	}
	log.Printf("starting gateway version=%s commit=%s port=%s", version, commit, cfg.Port)

	gw, err := NewGateway(cfg)
	if err != nil {
		log.Printf("failed to initialize gateway: %v", err)
		return 1
	}
	defer gw.Close()
	gw.watchConfigWith(ctx, configPath, 2*time.Second)

	pprofAddr := pprofAddress()
	var pprofServer *http.Server
	if !strings.EqualFold(pprofAddr, "off") {
		pprofServer = &http.Server{Addr: pprofAddr, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			log.Printf("pprof listening on %s", pprofAddr)
			if err := pprofServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("pprof server stopped: %v", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = pprofServer.Shutdown(shutdownCtx)
		}()
	}

	server := &http.Server{
		Addr:              cfg.Port,
		Handler:           gw.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		gw.draining.Store(true)
		if delay := shutdownDelay(); delay > 0 {
			log.Printf("shutdown signal received, waiting %s before draining", delay)
			time.Sleep(delay)
		} else {
			log.Println("shutdown signal received, draining connections")
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
			return 1
		}
		if pprofServer != nil {
			_ = pprofServer.Shutdown(shutdownCtx)
		}
		log.Println("gateway stopped")
		return 0
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("error starting the server on port %s: %v", cfg.Port, err)
			return 1
		}
		return 0
	}
}

func pprofAddress() string {
	if addr := strings.TrimSpace(os.Getenv("PPROF_ADDR")); addr != "" {
		return addr
	}
	return "off"
}
