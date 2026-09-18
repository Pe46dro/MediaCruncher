package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the complete MediaCruncher application configuration.
type Config struct {
	Database      DatabaseConfig      `yaml:"database"`
	Filesystem    FilesystemConfig    `yaml:"filesystem"`
	Evaluation    EvaluationConfig    `yaml:"evaluation"`
	Transcoder    TranscoderConfig    `yaml:"transcoder"`
	Concurrency   ConcurrencyConfig   `yaml:"concurrency"`
	Notification  NotificationConfig  `yaml:"notification"`
	Observability ObservabilityConfig `yaml:"observability"`
}

type DatabaseConfig struct {
	Path        string `yaml:"path"`
	BusyTimeout int    `yaml:"busy_timeout"` // milliseconds (default 5000)
}

type FilesystemConfig struct {
	Scopes            []ScanScopeConfig `yaml:"scopes"`
	IngestionCapacity int               `yaml:"ingestion_capacity"` // default 5000
}

type ScanScopeConfig struct {
	Path           string   `yaml:"path"`
	Extensions     []string `yaml:"extensions"`
	Exclusions     []string `yaml:"exclusions"`
	MaxDepth       int      `yaml:"max_depth"`
	FollowSymlinks bool     `yaml:"follow_symlinks"`
}

type EvaluationConfig struct {
	Rules             []RuleConfig      `yaml:"rules"`
	DefaultAction     string            `yaml:"default_action"` // transcode, copy, skip, review
	DefaultPreset     string            `yaml:"default_preset"`
	CodecAliases      map[string]string `yaml:"codec_aliases"`
	AudioPreservation AudioPreservation `yaml:"audio_preservation"`
}

type AudioPreservation struct {
	RetainSurroundTracks bool     `yaml:"retain_surround_tracks"` // e.g. 5.1/7.1 Atmos/DTS
	PreferredLanguages   []string `yaml:"preferred_languages"`   // e.g. ["eng", "ita"]
	CopyLosslessAudio    bool     `yaml:"copy_lossless_audio"`    // e.g. TrueHD, DTS-HD MA
}

type RuleConfig struct {
	Name            string            `yaml:"name"`
	Priority        int               `yaml:"priority"`
	Conditions      map[string]string `yaml:"conditions"`       // e.g. codec: "h264", min_resolution: "1080p"
	Action          string            `yaml:"action"`           // transcode, copy, skip, review
	Preset          string            `yaml:"preset"`
	AudioAction     string            `yaml:"audio_action"`     // copy, aac_stereo, passthrough
	RetainSubtitles bool              `yaml:"retain_subtitles"`
}

type TranscoderConfig struct {
	HardwareAcceleration string        `yaml:"hardware_acceleration"` // auto, nvenc, qsv, amf, vaapi, cpu
	StagingDir           string        `yaml:"staging_dir"`
	VMAFEnabled          bool          `yaml:"vmaf_enabled"`
	VMAFThreshold        float64       `yaml:"vmaf_threshold"`        // default 93.0
	VMAFSampleCount      int           `yaml:"vmaf_sample_count"`     // default 3
	VMAFSampleDuration   int           `yaml:"vmaf_sample_duration"`  // seconds, default 30
	SkipIfLarger         bool          `yaml:"skip_if_larger"`        // default true
	Presets              []PresetConfig `yaml:"presets"`
	MaxJobDuration       time.Duration `yaml:"max_job_duration"`      // default 4 hours
}

type PresetConfig struct {
	Name         string `yaml:"name"`
	VideoCodec   string `yaml:"video_codec"`   // e.g. hevc, av1, h264
	QualityCRF   int    `yaml:"quality_crf"`   // default 20-28
	PresetSpeed  string `yaml:"preset_speed"`  // slow, medium, fast, p5
	AudioCodec   string `yaml:"audio_codec"`   // aac, opus, copy
	AudioBitrate string `yaml:"audio_bitrate"` // 192k, 256k
	ExtraFFmpeg  string `yaml:"extra_ffmpeg"`
}

type ConcurrencyConfig struct {
	WorkerCount         int           `yaml:"worker_count"`           // general workers (default 4)
	GPUSemaphoreLimit   int           `yaml:"gpu_semaphore_limit"`   // max concurrent GPU tasks (default 2)
	CPUSemaphoreLimit   int           `yaml:"cpu_semaphore_limit"`   // max concurrent CPU tasks (default 4)
	PrefetchBufferDepth int           `yaml:"prefetch_buffer_depth"` // default 50
	DrainTimeout        time.Duration `yaml:"drain_timeout"`         // default 5 minutes
	RetryMaxAttempts    int           `yaml:"retry_max_attempts"`    // default 3
	RetryBaseInterval   time.Duration `yaml:"retry_base_interval"`   // default 15s
}

type NotificationConfig struct {
	Enabled       bool             `yaml:"enabled"`
	BatchWindow   time.Duration    `yaml:"batch_window"`   // default 30s
	BatchMaxSize  int              `yaml:"batch_max_size"` // default 10
	RateLimitPerM int              `yaml:"rate_limit_per_minute"`
	Channels      []ChannelConfig  `yaml:"channels"`
}

type ChannelConfig struct {
	Type     string            `yaml:"type"` // discord, telegram, slack, gotify, webhook, smtp
	Target   string            `yaml:"target"`
	Token    string            `yaml:"token"`
	Options  map[string]string `yaml:"options"`
	Events   []string          `yaml:"events"` // job_completed, job_failed, quality_failed, scan_summary
}

type ObservabilityConfig struct {
	LogLevel    string `yaml:"log_level"`    // debug, info, warn, error
	MetricsPort int    `yaml:"metrics_port"` // default 9090
	LogJSON     bool   `yaml:"log_json"`
}

// Manager manages loading, hot-reloading, and concurrent access to Config.
type Manager struct {
	mu        sync.RWMutex
	current   *Config
	filePath  string
	listeners []func(*Config)
}

func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	appData := filepath.Join(home, ".mediacruncher")

	return &Config{
		Database: DatabaseConfig{
			Path:        filepath.Join(appData, "mediacruncher.db"),
			BusyTimeout: 5000,
		},
		Filesystem: FilesystemConfig{
			Scopes: []ScanScopeConfig{
				{
					Path:           filepath.Join(home, "Videos"),
					Extensions:     []string{".mp4", ".mkv", ".mov", ".avi", ".m4v", ".webm"},
					Exclusions:     []string{"*.part", "*.temp", "*tmp*"},
					MaxDepth:       10,
					FollowSymlinks: false,
				},
			},
			IngestionCapacity: 5000,
		},
		Evaluation: EvaluationConfig{
			DefaultAction: "transcode",
			DefaultPreset: "balanced-hevc",
			CodecAliases: map[string]string{
				"h264":  "h264",
				"avc1":  "h264",
				"hevc":  "hevc",
				"h265":  "hevc",
				"hev1":  "hevc",
				"vp9":   "vp9",
				"av01":  "av1",
				"av1":   "av1",
			},
			AudioPreservation: AudioPreservation{
				RetainSurroundTracks: true,
				PreferredLanguages:   []string{"eng", "ita", "und"},
				CopyLosslessAudio:    true,
			},
			Rules: []RuleConfig{
				{
					Name:            "Skip Modern HEVC & AV1",
					Priority:        100,
					Conditions:      map[string]string{"codec": "hevc,av1"},
					Action:          "skip",
				},
				{
					Name:            "Transcode H.264 High Bitrate to HEVC",
					Priority:        50,
					Conditions:      map[string]string{"codec": "h264"},
					Action:          "transcode",
					Preset:          "balanced-hevc",
					AudioAction:     "copy",
					RetainSubtitles: true,
				},
			},
		},
		Transcoder: TranscoderConfig{
			HardwareAcceleration: "auto",
			StagingDir:           filepath.Join(appData, "staging"),
			VMAFEnabled:          true,
			VMAFThreshold:        93.0,
			VMAFSampleCount:      3,
			VMAFSampleDuration:   30,
			SkipIfLarger:         true,
			MaxJobDuration:       4 * time.Hour,
			Presets: []PresetConfig{
				{
					Name:         "balanced-hevc",
					VideoCodec:   "hevc",
					QualityCRF:   22,
					PresetSpeed:  "medium",
					AudioCodec:   "copy",
					AudioBitrate: "192k",
				},
				{
					Name:         "efficient-av1",
					VideoCodec:   "av1",
					QualityCRF:   26,
					PresetSpeed:  "preset=6",
					AudioCodec:   "opus",
					AudioBitrate: "128k",
				},
			},
		},
		Concurrency: ConcurrencyConfig{
			WorkerCount:         4,
			GPUSemaphoreLimit:   2,
			CPUSemaphoreLimit:   4,
			PrefetchBufferDepth: 50,
			DrainTimeout:        5 * time.Minute,
			RetryMaxAttempts:    3,
			RetryBaseInterval:   15 * time.Second,
		},
		Notification: NotificationConfig{
			Enabled:       false,
			BatchWindow:   30 * time.Second,
			BatchMaxSize:  10,
			RateLimitPerM: 30,
			Channels:      []ChannelConfig{},
		},
		Observability: ObservabilityConfig{
			LogLevel:    "info",
			MetricsPort: 9090,
			LogJSON:     true,
		},
	}
}

// Load reads config from file path (if provided) and applies environment variable overrides.
func Load(configPath string) (*Manager, error) {
	cfg := DefaultConfig()

	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("failed to read config file %s: %w", configPath, err)
			}
		} else {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("failed to parse yaml config %s: %w", configPath, err)
			}
		}
	}

	applyEnvOverrides(cfg)
	normalizeConfigPaths(cfg)

	mgr := &Manager{
		current:  cfg,
		filePath: configPath,
	}
	return mgr, nil
}

func applyEnvOverrides(cfg *Config) {
	if val := os.Getenv("MEDIACRUNCHER_DB_PATH"); val != "" {
		cfg.Database.Path = val
	}
	if val := os.Getenv("MEDIACRUNCHER_WORKERS"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			cfg.Concurrency.WorkerCount = n
		}
	}
	if val := os.Getenv("MEDIACRUNCHER_GPU_LIMIT"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			cfg.Concurrency.GPUSemaphoreLimit = n
		}
	}
	if val := os.Getenv("MEDIACRUNCHER_LOG_LEVEL"); val != "" {
		cfg.Observability.LogLevel = val
	}
	if val := os.Getenv("MEDIACRUNCHER_METRICS_PORT"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			cfg.Observability.MetricsPort = n
		}
	}
	if val := os.Getenv("MEDIACRUNCHER_HWACCEL"); val != "" {
		cfg.Transcoder.HardwareAcceleration = strings.ToLower(val)
	}
}

func normalizeConfigPaths(cfg *Config) {
	cfg.Database.Path = NormalizePath(cfg.Database.Path)
	cfg.Transcoder.StagingDir = NormalizePath(cfg.Transcoder.StagingDir)
	for i := range cfg.Filesystem.Scopes {
		cfg.Filesystem.Scopes[i].Path = NormalizePath(cfg.Filesystem.Scopes[i].Path)
	}
}

// Get returns the current immutable config snapshot.
func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Reload reloads the configuration file and notifies listeners.
func (m *Manager) Reload() error {
	if m.filePath == "" {
		return nil
	}
	fresh, err := Load(m.filePath)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.current = fresh.current
	listeners := append([]func(*Config){}, m.listeners...)
	m.mu.Unlock()

	for _, l := range listeners {
		l(m.current)
	}
	return nil
}

// OnReload registers a callback for configuration updates.
func (m *Manager) OnReload(fn func(*Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listeners = append(m.listeners, fn)
}
