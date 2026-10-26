package main

import (
	"flag"
	"os"

	"github.com/Mohdcode/api-Gateway/internal/gateway"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /healthz endpoint and exit")
	flag.Parse()

	if *healthcheck {
		os.Exit(gateway.RunHealthCheck())
	}

	configPath := ""
	if flag.NArg() > 0 {
		configPath = flag.Arg(0)
	}
	os.Exit(gateway.Run(configPath))
}
