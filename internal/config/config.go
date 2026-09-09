package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const envPrefix = "MC_"

// ScanScope represents a user-configured root path with inclusion and exclusion rules.
type ScanScope struct {
	RootPath          string   `json:"root_path"`
	IncludedExtensions []string `json:"included_extensions"`
	ExclusionPatterns  []string `json:"exclusion_patterns"`
	MaxDepth          int      `json:"max_depth,omitempty"`
	SymlinkPolicy     string   `json:"symlink_policy"` // "follow", "skip", "dereference"
	IOTimeout         string   `json:"io_timeout,omitempty"`
}

// NotificationChannel represents a notification channel configuration.
type NotificationChannel struct {
	Type       string `json:"type"`         // "telegram", "discord", "slack", "gotify", "smtp"
	Enabled    bool   `json:"enabled"`
	WebhookURL string `json:"webhook_url,omitempty"`
	Token      string `json:"token,omitempty"`
	ChannelID  string `json:"channel_id,omitempty"`
	Server     string `json:"server,omitempty"`
	Port       int    `json:"port,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
}

// EvaluationSettings holds rule-matching engine configuration.
type EvaluationSettings struct {
	DefaultAction    string `json:"default_action"`     // "transcode", "stream_copy", "skip", "review"
	ProbeTimeout     string `json:"probe_timeout"`
	RuleSetFile      string `json:"rule_set_file,omitempty"`
	CodecAliasFile   string `json:"codec_alias_file,omitempty"`
}

// TranscoderSettings holds transcoding configuration.
type TranscoderSettings struct {
	HardwareAcceleration bool    `json:"hardware_acceleration"`
	PreferredDevice      int     `json:"preferred_device"`
	VMAFThreshold        float64 `json:"vmaf_threshold"`
	VPreset              string  `json:"preset"`
	MaxEncodingDuration  string  `json:"max_encoding_duration"`
	FallbackToSoftware   bool    `json:"fallback_to_software"`
	TargetCodec          string  `json:"target_codec,omitempty"`
	Codec                string  `json:"codec,omitempty"`
}

// ConcurrencySettings holds worker pool configuration.
type ConcurrencySettings struct {
	WorkerCount      int           `json:"worker_count"`
	QueueCapacity    int           `json:"queue_capacity"`
	MaxRetries       int           `json:"max_retries"`
	BaseBackoff      string        `json:"base_backoff"`
	MaxBackoff       string        `json:"max_backoff"`
	DrainTimeout     string        `json:"drain_timeout"`
	BatchSplitSize   int           `json:"batch_split_size"`
}

// NotificationSettings holds notification engine configuration.
type NotificationSettings struct {
	Channels           []NotificationChannel `json:"channels"`
	RateLimitPerMinute int                   `json:"rate_limit_per_minute"`
	BatchSize          int                   `json:"batch_size"`
	BatchAge           string                `json:"batch_age"`
	RetryMax           int                   `json:"retry_max"`
	DeadLetterRetention string               `json:"dead_letter_retention"`
}

// PersistenceSettings holds database configuration.
type PersistenceSettings struct {
	DatabasePath       string        `json:"database_path"`
	Synchronous        string        `json:"synchronous"` // "full", "normal", "off", "nosync"
	MigrationAuto      bool          `json:"migration_auto"`
	AuditRetentionDays int           `json:"audit_retention_days"`
	AuditMinEntries    int           `json:"audit_min_entries"`
}

// ObservabilitySettings holds logging and metrics configuration.
type ObservabilitySettings struct {
	LogLevel          string `json:"log_level"`
	MetricsAddr       string `json:"metrics_addr"`
	EnableHealthCheck bool   `json:"enable_health_check"`
}

// Config is the complete application configuration, loaded from defaults,
// environment variables, and optional config file.
type Config struct {
	Persistence   PersistenceSettings    `json:"persistence"`
	ScanScopes    []ScanScope            `json:"scan_scopes"`
	Evaluation    EvaluationSettings     `json:"evaluation"`
	Transcoder    TranscoderSettings     `json:"transcoder"`
	Concurrency   ConcurrencySettings    `json:"concurrency"`
	Notification  NotificationSettings   `json:"notification"`
	Observability ObservabilitySettings  `json:"observability"`
}

// DefaultConfig returns a Config with all built-in defaults applied.
func DefaultConfig() Config {
	return Config{
		Persistence: PersistenceSettings{
			DatabasePath:       "mediacruncher.db",
			Synchronous:        "full",
			MigrationAuto:      true,
			AuditRetentionDays: 90,
			AuditMinEntries:    1000,
		},
		ScanScopes: []ScanScope{},
		Evaluation: EvaluationSettings{
			DefaultAction: "transcode",
			ProbeTimeout:  "30s",
		},
		Transcoder: TranscoderSettings{
			HardwareAcceleration: true,
			PreferredDevice:      0,
			VMAFThreshold:        90.0,
			VPreset:              "medium",
			MaxEncodingDuration:  "2h",
			FallbackToSoftware:   true,
			TargetCodec:          "h.265",
		},
		Concurrency: ConcurrencySettings{
			WorkerCount:     4,
			QueueCapacity:   1000,
			MaxRetries:      3,
			BaseBackoff:     "1s",
			MaxBackoff:      "1m",
			DrainTimeout:    "5m",
			BatchSplitSize:  10,
		},
		Notification: NotificationSettings{
			Channels:           nil,
			RateLimitPerMinute: 60,
			BatchSize:          10,
			BatchAge:           "30s",
			RetryMax:           3,
			DeadLetterRetention: "30d",
		},
		Observability: ObservabilitySettings{
			LogLevel:          "info",
			MetricsAddr:       "127.0.0.1:9090/metrics",
			EnableHealthCheck: true,
		},
	}
}

// ParseDuration is a helper that parses a duration string and returns time.Duration.
// Returns 0 on error, which callers should treat as "use default".
func ParseDuration(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// LoadConfig loads configuration from defaults, environment variables, and an optional config file.
// The configDir parameter is the directory to search for mediacruncher.json.
// It validates the configuration and returns an error if any required fields are invalid.
func LoadConfig(configDir string) (*Config, error) {
	cfg := DefaultConfig()

	// Load from config file if it exists
	if configDir != "" {
		configPath := filepath.Join(configDir, "mediacruncher.json")
		if data, err := os.ReadFile(configPath); err == nil {
			if err := json.Unmarshal(data, &cfg); err != nil {
				return nil, fmt.Errorf("parse config file %s: %w", configPath, err)
			}
		}
	}

	// Override with environment variables
	applyEnv(&cfg)

	// Validate
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}

	return &cfg, nil
}

// applyEnv reads environment variables with the MC_ prefix and overrides config values.
func applyEnv(cfg *Config) {
	if v := os.Getenv(envPrefix + "DATABASE_PATH"); v != "" {
		cfg.Persistence.DatabasePath = v
	}
	if v := os.Getenv(envPrefix + "SYNCHRONOUS"); v != "" {
		cfg.Persistence.Synchronous = v
	}
	if v := os.Getenv(envPrefix + "AUDIT_RETENTION_DAYS"); v != "" {
		if n := parseInt(v); n > 0 {
			cfg.Persistence.AuditRetentionDays = n
		}
	}
	if v := os.Getenv(envPrefix + "AUDIT_MIN_ENTRIES"); v != "" {
		if n := parseInt(v); n > 0 {
			cfg.Persistence.AuditMinEntries = n
		}
	}
	if v := os.Getenv(envPrefix + "LOG_LEVEL"); v != "" {
		cfg.Observability.LogLevel = v
	}
	if v := os.Getenv(envPrefix + "METRICS_ADDR"); v != "" {
		cfg.Observability.MetricsAddr = v
	}
	if v := os.Getenv(envPrefix + "WORKER_COUNT"); v != "" {
		if n := parseInt(v); n > 0 {
			cfg.Concurrency.WorkerCount = n
		}
	}
	if v := os.Getenv(envPrefix + "QUEUE_CAPACITY"); v != "" {
		if n := parseInt(v); n > 0 {
			cfg.Concurrency.QueueCapacity = n
		}
	}
	if v := os.Getenv(envPrefix + "MAX_RETRIES"); v != "" {
		if n := parseInt(v); n > 0 {
			cfg.Concurrency.MaxRetries = n
		}
	}
	if v := os.Getenv(envPrefix + "HARDWARE_ACCELERATION"); v != "" {
		cfg.Transcoder.HardwareAcceleration = strings.EqualFold(v, "true")
	}
	if v := os.Getenv(envPrefix + "TARGET_CODEC"); v != "" {
		cfg.Transcoder.TargetCodec = v
	}
	if v := os.Getenv(envPrefix + "CODEC"); v != "" {
		cfg.Transcoder.Codec = v
	}
	if v := os.Getenv(envPrefix + "VMAF_THRESHOLD"); v != "" {
		if f := parseFloat(v); f > 0 {
			cfg.Transcoder.VMAFThreshold = f
		}
	}
	if v := os.Getenv(envPrefix + "DEFAULT_ACTION"); v != "" {
		cfg.Evaluation.DefaultAction = v
	}
	if v := os.Getenv(envPrefix + "CONFIG_DIR"); v != "" {
		if data, err := os.ReadFile(filepath.Join(v, "mediacruncher.json")); err == nil {
			if err := json.Unmarshal(data, cfg); err != nil {
				fmt.Fprintf(os.Stderr, "warning: failed to re-read config from MC_CONFIG_DIR=%s: %v\n", v, err)
			}
		}
	}
}

// ParsedScopes returns the configured scan scopes as parsed objects.
func (c *Config) ParsedScopes() []ParsedScanScope {
	scopes := make([]ParsedScanScope, 0, len(c.ScanScopes))
	for _, s := range c.ScanScopes {
		p := ParsedScanScope{
			RootPath:           s.RootPath,
			IncludedExtensions: s.IncludedExtensions,
			ExclusionPatterns:  s.ExclusionPatterns,
			MaxDepth:           s.MaxDepth,
			SymlinkPolicy:      s.SymlinkPolicy,
		}
		if s.IOTimeout != "" {
			p.IOTimeout = ParseDuration(s.IOTimeout)
		}
		if p.IncludedExtensions == nil {
			p.IncludedExtensions = []string{"mp4", "mkv", "avi", "mov", "wmv", "flv", "webm", "m4v", "mpg", "mpeg", "ts", "m2ts"}
		}
		if p.SymlinkPolicy == "" {
			p.SymlinkPolicy = "skip"
		}
		if p.IOTimeout == 0 {
			p.IOTimeout = 10 * time.Second
		}
		scopes = append(scopes, p)
	}
	return scopes
}

// ParsedScanScope is a ScanScope with resolved defaults and parsed durations.
type ParsedScanScope struct {
	RootPath           string
	IncludedExtensions []string
	ExclusionPatterns  []string
	MaxDepth           int
	SymlinkPolicy      string
	IOTimeout          time.Duration
}

// ParsedConcurrency returns concurrency settings with parsed durations.
func (c *Config) ParsedConcurrency() ParsedConcurrency {
	return ParsedConcurrency{
		WorkerCount:    c.Concurrency.WorkerCount,
		QueueCapacity:  c.Concurrency.QueueCapacity,
		MaxRetries:     c.Concurrency.MaxRetries,
		BaseBackoff:    ParseDuration(c.Concurrency.BaseBackoff),
		MaxBackoff:     ParseDuration(c.Concurrency.MaxBackoff),
		DrainTimeout:   ParseDuration(c.Concurrency.DrainTimeout),
		BatchSplitSize: c.Concurrency.BatchSplitSize,
	}
}

// ParsedConcurrency holds concurrency settings with parsed time durations.
type ParsedConcurrency struct {
	WorkerCount    int
	QueueCapacity  int
	MaxRetries     int
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	DrainTimeout   time.Duration
	BatchSplitSize int
}

// ParsedTranscoder returns transcoder settings with parsed durations.
func (c *Config) ParsedTranscoder() ParsedTranscoder {
	codec := c.Transcoder.TargetCodec
	if codec == "" {
		codec = c.Transcoder.Codec
	}
	if codec == "" {
		codec = "h.265"
	}
	return ParsedTranscoder{
		HardwareAcceleration: c.Transcoder.HardwareAcceleration,
		PreferredDevice:      c.Transcoder.PreferredDevice,
		VMAFThreshold:        c.Transcoder.VMAFThreshold,
		Preset:               c.Transcoder.VPreset,
		MaxEncodingDuration:  ParseDuration(c.Transcoder.MaxEncodingDuration),
		FallbackToSoftware:   c.Transcoder.FallbackToSoftware,
		TargetCodec:          codec,
	}
}

// ParsedTranscoder holds transcoder settings with parsed time durations.
type ParsedTranscoder struct {
	HardwareAcceleration bool
	PreferredDevice      int
	VMAFThreshold        float64
	Preset               string
	MaxEncodingDuration  time.Duration
	FallbackToSoftware   bool
	TargetCodec          string
}

// ParsedNotification returns notification settings with parsed durations.
func (c *Config) ParsedNotification() ParsedNotification {
	return ParsedNotification{
		Channels:            c.Notification.Channels,
		RateLimitPerMinute:  c.Notification.RateLimitPerMinute,
		BatchSize:           c.Notification.BatchSize,
		BatchAge:            ParseDuration(c.Notification.BatchAge),
		RetryMax:            c.Notification.RetryMax,
		DeadLetterRetention: ParseDuration(c.Notification.DeadLetterRetention),
	}
}

// ParsedNotification holds notification settings with parsed time durations.
type ParsedNotification struct {
	Channels            []NotificationChannel
	RateLimitPerMinute  int
	BatchSize           int
	BatchAge            time.Duration
	RetryMax            int
	DeadLetterRetention time.Duration
}

// ParsedEvaluation returns evaluation settings with parsed durations.
func (c *Config) ParsedEvaluation() ParsedEvaluation {
	return ParsedEvaluation{
		DefaultAction: c.Evaluation.DefaultAction,
		ProbeTimeout:  ParseDuration(c.Evaluation.ProbeTimeout),
		RuleSetFile:   c.Evaluation.RuleSetFile,
		CodecAliasFile: c.Evaluation.CodecAliasFile,
	}
}

// ParsedEvaluation holds evaluation settings with parsed time durations.
type ParsedEvaluation struct {
	DefaultAction  string
	ProbeTimeout   time.Duration
	RuleSetFile    string
	CodecAliasFile string
}

// validate checks the configuration for required constraints and invalid values.
func validate(cfg *Config) error {
	switch cfg.Persistence.Synchronous {
	case "full", "normal", "off", "nosync":
		// valid
	default:
		return fmt.Errorf("persistence.synchronous: invalid value %q, must be full, normal, off, or nosync", cfg.Persistence.Synchronous)
	}
	if cfg.Concurrency.WorkerCount <= 0 {
		return fmt.Errorf("concurrency.worker_count: must be positive, got %d", cfg.Concurrency.WorkerCount)
	}
	if cfg.Concurrency.QueueCapacity <= 0 {
		return fmt.Errorf("concurrency.queue_capacity: must be positive, got %d", cfg.Concurrency.QueueCapacity)
	}
	if cfg.Transcoder.VMAFThreshold < 0 || cfg.Transcoder.VMAFThreshold > 100 {
		return fmt.Errorf("transcoder.vmaf_threshold: must be between 0 and 100, got %f", cfg.Transcoder.VMAFThreshold)
	}
	switch cfg.Evaluation.DefaultAction {
	case "transcode", "stream_copy", "skip", "review":
		// valid
	default:
		return fmt.Errorf("evaluation.default_action: invalid value %q, must be transcode, stream_copy, skip, or review", cfg.Evaluation.DefaultAction)
	}
	switch cfg.Observability.LogLevel {
	case "debug", "info", "warn", "error", "critical":
		// valid
	default:
		return fmt.Errorf("observability.log_level: invalid value %q, must be debug, info, warn, error, or critical", cfg.Observability.LogLevel)
	}
	for i, scope := range cfg.ScanScopes {
		if scope.RootPath == "" {
			return fmt.Errorf("scan_scopes[%d].root_path: must not be empty", i)
		}
		if scope.SymlinkPolicy != "" && scope.SymlinkPolicy != "follow" && scope.SymlinkPolicy != "skip" && scope.SymlinkPolicy != "dereference" {
			return fmt.Errorf("scan_scopes[%d].symlink_policy: invalid value %q", i, scope.SymlinkPolicy)
		}
	}
	return nil
}

func parseInt(s string) int {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	if err != nil {
		return 0
	}
	return n
}

func parseFloat(s string) float64 {
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	if err != nil {
		return 0
	}
	return f
}
