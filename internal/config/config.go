// Package config loads and validates gateway configuration from YAML files
// and environment variables. Env vars with prefix XBEACON_ override file
// values (e.g. XBEACON_SERVER_ADDR overrides server.addr).
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const envPrefix = "XBEACON"

type Config struct {
	Server        ServerConfig        `mapstructure:"server"`
	Log           LogConfig           `mapstructure:"log"`
	Observability ObservabilityConfig `mapstructure:"observability"`
	Database      DatabaseConfig      `mapstructure:"database"`
	Redis         RedisConfig         `mapstructure:"redis"`
	ProvidersFile string              `mapstructure:"providers_file"`
	Auth          AuthConfig          `mapstructure:"auth"`
	RateLimits    []RateLimitRule     `mapstructure:"rate_limits"`
	Cache         CacheConfig         `mapstructure:"cache"`
}

// AuthConfig holds settings for the API-key auth path. Step 4.4 introduces
// the Redis cache layer; future auth changes (token rotation, key types)
// extend this struct.
type AuthConfig struct {
	Cache AuthCacheConfig `mapstructure:"cache"`
}

// AuthCacheConfig configures the Redis cache that fronts the
// PostgresAuthenticator. PositiveTTL=0 OR an unreachable Redis disables
// the cache; the gateway falls back to direct DB lookups.
type AuthCacheConfig struct {
	PositiveTTL time.Duration `mapstructure:"positive_ttl"`
	NegativeTTL time.Duration `mapstructure:"negative_ttl"`
}

type ServerConfig struct {
	Addr            string        `mapstructure:"addr"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

type LogConfig struct {
	Level  string `mapstructure:"level"`  // debug | info | warn | error
	Format string `mapstructure:"format"` // json | console
}

type ObservabilityConfig struct {
	Metrics MetricsConfig `mapstructure:"metrics"`
	Tracing TracingConfig `mapstructure:"tracing"`
}

type MetricsConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	Path    string `mapstructure:"path"`
}

type TracingConfig struct {
	Enabled     bool    `mapstructure:"enabled"`
	Endpoint    string  `mapstructure:"endpoint"`
	ServiceName string  `mapstructure:"service_name"`
	SampleRatio float64 `mapstructure:"sample_ratio"`
}

type DatabaseConfig struct {
	DSN             string        `mapstructure:"dsn"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	PoolSize int    `mapstructure:"pool_size"`
}

type CacheConfig struct {
	Exact    ExactCacheConfig    `mapstructure:"exact"`
	Semantic SemanticCacheConfig `mapstructure:"semantic"`
}

type ExactCacheConfig struct {
	Enabled bool          `mapstructure:"enabled"`
	TTL     time.Duration `mapstructure:"ttl"`
}

type SemanticCacheConfig struct {
	Enabled        bool    `mapstructure:"enabled"`
	Threshold      float64 `mapstructure:"threshold"`
	EmbeddingModel string  `mapstructure:"embedding_model"`
	TopK           int     `mapstructure:"top_k"`
}

// RateLimitRule is held loosely here; Week 5 fleshes out the full schema.
type RateLimitRule struct {
	Name       string            `mapstructure:"name"`
	Algorithm  string            `mapstructure:"algorithm"`
	Rate       string            `mapstructure:"rate"`
	Window     time.Duration     `mapstructure:"window"`
	Limit      int               `mapstructure:"limit"`
	Burst      int               `mapstructure:"burst"`
	KeyBy      []string          `mapstructure:"key_by"`
	Conditions map[string]string `mapstructure:"conditions"`
}

// Load reads configuration from the given YAML file. An empty path loads
// defaults + env only. The returned *Config is guaranteed to have passed
// Validate().
func Load(path string) (*Config, error) {
	v := viper.New()
	setDefaults(v)

	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %q: %w", path, err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("server.addr", ":8080")
	v.SetDefault("server.read_timeout", 10*time.Second)
	v.SetDefault("server.write_timeout", 600*time.Second)
	v.SetDefault("server.shutdown_timeout", 30*time.Second)

	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")

	v.SetDefault("observability.metrics.enabled", true)
	v.SetDefault("observability.metrics.path", "/metrics")
	v.SetDefault("observability.tracing.enabled", false)
	v.SetDefault("observability.tracing.service_name", "x-beacon")
	v.SetDefault("observability.tracing.sample_ratio", 0.1)

	v.SetDefault("database.max_open_conns", 20)
	v.SetDefault("database.max_idle_conns", 5)
	v.SetDefault("database.conn_max_lifetime", 30*time.Minute)

	v.SetDefault("redis.addr", "localhost:6379")
	v.SetDefault("redis.pool_size", 50)

	v.SetDefault("auth.cache.positive_ttl", 60*time.Second)
	v.SetDefault("auth.cache.negative_ttl", 5*time.Second)

	v.SetDefault("providers_file", "configs/providers.yaml")

	v.SetDefault("cache.exact.enabled", true)
	v.SetDefault("cache.exact.ttl", time.Hour)
	v.SetDefault("cache.semantic.threshold", 0.95)
	v.SetDefault("cache.semantic.embedding_model", "text-embedding-3-small")
	v.SetDefault("cache.semantic.top_k", 5)
}

// Validate checks for conflicting or out-of-range settings.
func (c *Config) Validate() error {
	var errs []error

	if c.Server.Addr == "" {
		errs = append(errs, errors.New("server.addr is required"))
	}
	if c.Server.ReadTimeout <= 0 {
		errs = append(errs, errors.New("server.read_timeout must be > 0"))
	}
	if c.Server.WriteTimeout <= 0 {
		errs = append(errs, errors.New("server.write_timeout must be > 0"))
	}
	if c.Server.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("server.shutdown_timeout must be > 0"))
	}

	switch strings.ToLower(c.Log.Level) {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q invalid (allowed: debug|info|warn|error)", c.Log.Level))
	}
	switch strings.ToLower(c.Log.Format) {
	case "json", "console":
	default:
		errs = append(errs, fmt.Errorf("log.format %q invalid (allowed: json|console)", c.Log.Format))
	}

	if c.Observability.Tracing.Enabled && c.Observability.Tracing.Endpoint == "" {
		errs = append(errs, errors.New("observability.tracing.endpoint is required when tracing is enabled"))
	}
	if r := c.Observability.Tracing.SampleRatio; r < 0 || r > 1 {
		errs = append(errs, fmt.Errorf("observability.tracing.sample_ratio %v out of [0,1]", r))
	}

	if c.Cache.Semantic.Enabled {
		if t := c.Cache.Semantic.Threshold; t <= 0 || t > 1 {
			errs = append(errs, fmt.Errorf("cache.semantic.threshold %v out of (0,1]", t))
		}
		if c.Cache.Semantic.TopK <= 0 {
			errs = append(errs, errors.New("cache.semantic.top_k must be > 0"))
		}
	}

	if c.Database.MaxOpenConns < 0 {
		errs = append(errs, errors.New("database.max_open_conns must be >= 0"))
	}
	if c.Database.MaxIdleConns < 0 {
		errs = append(errs, errors.New("database.max_idle_conns must be >= 0"))
	}

	return errors.Join(errs...)
}
