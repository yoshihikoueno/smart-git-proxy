package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr           string
	MirrorDir            string
	MirrorMaxSize        SizeSpec // Max size (absolute or %), zero means default 80%
	SyncStaleAfter       time.Duration
	AllowedUpstreams     []string
	LogLevel             string
	AuthMode             string
	StaticToken          string
	MetricsPath          string
	HealthPath           string
	AWSCloudMapServiceID string // If set, register with AWS Cloud Map and send heartbeats
	Route53HostedZoneID  string // Route53 hosted zone ID for DNS registration
	Route53RecordName    string // Route53 record name (e.g., git-proxy.example.com)
	SerializeUploadPack  bool
	UploadPackThreads    int
	MaintainAfterSync    bool
	MaintenanceRepo      string // If set, run maintenance on this repo (or "all") and exit
}

func Load() (*Config, error) {
	return LoadArgs(os.Args[1:])
}

func LoadArgs(args []string) (*Config, error) {
	cfg := &Config{}

	fs := flag.NewFlagSet("smart-git-proxy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.StringVar(&cfg.ListenAddr, "listen-addr", envOrDefault("LISTEN_ADDR", ":8080"), "HTTP listen address")
	fs.StringVar(&cfg.MirrorDir, "mirror-dir", envOrDefault("MIRROR_DIR", "/mnt/git-mirrors"), "directory for bare git mirrors")
	fs.StringVar(&cfg.LogLevel, "log-level", envOrDefault("LOG_LEVEL", "info"), "log level: debug,info,warn,error")
	fs.StringVar(&cfg.AuthMode, "auth-mode", envOrDefault("AUTH_MODE", "pass-through"), "auth mode: pass-through|static|none (for upstream sync)")
	fs.StringVar(&cfg.StaticToken, "static-token", envOrDefault("STATIC_TOKEN", ""), "static token used when auth-mode=static")
	fs.StringVar(&cfg.MetricsPath, "metrics-path", envOrDefault("METRICS_PATH", "/metrics"), "path for Prometheus metrics")
	fs.StringVar(&cfg.HealthPath, "health-path", envOrDefault("HEALTH_PATH", "/healthz"), "path for health checks")
	fs.StringVar(&cfg.AWSCloudMapServiceID, "aws-cloud-map-service-id", envOrDefault("AWS_CLOUD_MAP_SERVICE_ID", ""), "AWS Cloud Map service ID for registration and health heartbeat")
	fs.StringVar(&cfg.Route53HostedZoneID, "route53-hosted-zone-id", envOrDefault("ROUTE53_HOSTED_ZONE_ID", ""), "Route53 hosted zone ID for DNS registration")
	fs.StringVar(&cfg.Route53RecordName, "route53-record-name", envOrDefault("ROUTE53_RECORD_NAME", ""), "Route53 record name (e.g., git-proxy.example.com)")
	fs.BoolVar(&cfg.SerializeUploadPack, "serialize-upload-pack", envOrDefaultBool("SERIALIZE_UPLOAD_PACK", false), "serialize upload-pack per repo to reduce concurrent packing CPU")
	fs.IntVar(&cfg.UploadPackThreads, "upload-pack-threads", envOrDefaultInt("UPLOAD_PACK_THREADS", 0), "pack.threads to use for upload-pack (0 means git default)")
	fs.BoolVar(&cfg.MaintainAfterSync, "maintain-after-sync", envOrDefaultBool("MAINTAIN_AFTER_SYNC", false), "run lightweight maintenance (midx bitmap + commit-graph) after sync")
	fs.StringVar(&cfg.MaintenanceRepo, "maintenance-repo", envOrDefault("MAINTENANCE_REPO", ""), "if set, run maintenance on the given repo key (host/owner/repo) or \"all\" and exit")

	allowedUpstreamsStr := fs.String("allowed-upstreams", envOrDefault("ALLOWED_UPSTREAMS", "github.com"), "comma-separated list of allowed upstream hosts")
	syncStaleAfterStr := fs.String("sync-stale-after", envOrDefault("SYNC_STALE_AFTER", "2s"), "sync mirror if older than this duration")
	mirrorMaxSizeStr := fs.String("mirror-max-size", envOrDefault("MIRROR_MAX_SIZE", ""), "max size for mirrors (e.g. 200GiB, 80%), defaults to 80% of available disk")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	var err error
	if cfg.SyncStaleAfter, err = time.ParseDuration(*syncStaleAfterStr); err != nil {
		return nil, fmt.Errorf("invalid sync-stale-after: %w", err)
	}

	// Parse mirror max size (empty string means use default 80% of available)
	if *mirrorMaxSizeStr != "" {
		if cfg.MirrorMaxSize, err = ParseSizeSpec(*mirrorMaxSizeStr); err != nil {
			return nil, fmt.Errorf("invalid mirror-max-size: %w", err)
		}
	}

	// Parse allowed upstreams
	for _, h := range strings.Split(*allowedUpstreamsStr, ",") {
		h = strings.TrimSpace(h)
		if h != "" {
			cfg.AllowedUpstreams = append(cfg.AllowedUpstreams, h)
		}
	}
	if len(cfg.AllowedUpstreams) == 0 {
		return nil, errors.New("at least one allowed upstream is required")
	}

	if err := validateAuth(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

func validateAuth(cfg *Config) error {
	switch cfg.AuthMode {
	case "pass-through", "none":
		return nil
	case "static":
		if cfg.StaticToken == "" {
			return errors.New("auth-mode=static requires STATIC_TOKEN")
		}
		return nil
	default:
		return fmt.Errorf("unknown auth-mode: %s", cfg.AuthMode)
	}
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envOrDefaultBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}

func envOrDefaultInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return def
}
