package gateway

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func loadConfigPath(p string) (*Config, error) {
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("error loading the config %s: %w", p, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("error parsing config yaml %s: %w", p, err)
	}
	return &cfg, nil
}

// applyEnvOverrides lets container platforms configure the gateway without
// rewriting the mounted config file. Explicit environment values win.
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("PORT"); v != "" {
		cfg.Port = v
	}
	if v := os.Getenv("JWT_SECRET"); v != "" {
		cfg.JwtSecret = v
	}
	if v := os.Getenv("JWT_ISSUER"); v != "" {
		cfg.JwtIssuer = v
	}
	if v := os.Getenv("JWT_AUDIENCE"); v != "" {
		cfg.JwtAudience = v
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}
	if v := os.Getenv("REDIS_DB"); v != "" {
		db, err := strconv.Atoi(v)
		if err != nil {
			log.Printf("ignoring invalid REDIS_DB %q: %v", v, err)
		} else {
			cfg.Redis.DB = db
		}
	}
	if v, ok := os.LookupEnv("API_KEYS"); ok {
		keys := make([]string, 0)
		for _, key := range strings.Split(v, ",") {
			if key = strings.TrimSpace(key); key != "" {
				keys = append(keys, key)
			}
		}
		cfg.MiddlewareConfig.ApiKeyAuth.ValidKeys = keys
	}
}

func shutdownDelay() time.Duration {
	raw := os.Getenv("SHUTDOWN_DELAY")
	if raw == "" {
		return 0
	}
	delay, err := time.ParseDuration(raw)
	if err != nil || delay < 0 {
		log.Printf("ignoring invalid SHUTDOWN_DELAY %q", raw)
		return 0
	}
	return delay
}

func loadConfigWithEnv(p string) (*Config, error) {
	cfg, err := loadConfigPath(p)
	if err != nil {
		return nil, err
	}
	applyEnvOverrides(cfg)
	return cfg, nil
}

// Close releases resources held by the gateway.
