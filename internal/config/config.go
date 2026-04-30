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
	Router        RouterConfig        `mapstructure:"router"`
	Routing       RoutingConfig       `mapstructure:"routing"`
	Billing       BillingConfig       `mapstructure:"billing"`
	Cache         CacheConfig         `mapstructure:"cache"`
	Prompt        PromptConfig        `mapstructure:"prompt"`
}

// PromptConfig configures the Week 12 context-truncation layer. Disabled
// by default; opt-in by setting `prompt.compression.enabled: true`.
// Triggered only when prompt tokens exceed `window * trigger_ratio`,
// so well-behaved short prompts never pay any cost.
type PromptConfig struct {
	Compression PromptCompressionConfig `mapstructure:"compression"`
}

type PromptCompressionConfig struct {
	Enabled         bool           `mapstructure:"enabled"`
	TriggerRatio    float64        `mapstructure:"trigger_ratio"`
	MinKeepMessages int            `mapstructure:"min_keep_messages"`
	DefaultWindow   int            `mapstructure:"default_window"`
	ModelWindows    map[string]int `mapstructure:"model_windows"`
}

// RoutingConfig is Week 11's smart-routing layer. The chat handler
// runs Classify before cache + router, and the rules listed here are
// evaluated in order — first match wins (no cascading). Disable by
// setting enabled=false; the chat handler then skips classification
// entirely and requests pass through with their declared model.
type RoutingConfig struct {
	Enabled bool          `mapstructure:"enabled"`
	Rules   []RoutingRule `mapstructure:"rules"`
}

// RoutingRule mirrors internal/route.Rule shape so cmd/gateway can
// translate without per-field mapping. Keep field names aligned with
// the YAML the operator writes.
type RoutingRule struct {
	Name    string           `mapstructure:"name"`
	RouteTo string           `mapstructure:"route_to"`
	When    RoutingCondition `mapstructure:"when"`
}

// RoutingCondition mirrors route.Condition. Multiple fields AND
// together; keyword lists OR within themselves; zero values mean
// "don't check".
type RoutingCondition struct {
	MaxTokens    int      `mapstructure:"max_tokens"`
	MinTokens    int      `mapstructure:"min_tokens"`
	KeywordsAny  []string `mapstructure:"keywords_any"`
	KeywordsNone []string `mapstructure:"keywords_none"`
}

// BillingConfig configures the async request_logs writer + pricing
// cache. Both sub-blocks have safe defaults; production usually only
// changes pricing_reload_interval after observing operational pain.
type BillingConfig struct {
	Worker                BillingWorkerConfig `mapstructure:"worker"`
	PricingReloadInterval time.Duration       `mapstructure:"pricing_reload_interval"`
}

type BillingWorkerConfig struct {
	BufferSize   int           `mapstructure:"buffer_size"`
	Workers      int           `mapstructure:"workers"`
	FlushTimeout time.Duration `mapstructure:"flush_timeout"`
}

// RouterConfig tunes Week 6's retry / fail-over / circuit-breaker layer.
// Zero values fall back to library defaults; only override when load
// testing or operating against a flaky upstream surfaces a need.
type RouterConfig struct {
	Retry   RouterRetryConfig   `mapstructure:"retry"`
	Breaker RouterBreakerConfig `mapstructure:"breaker"`
}

type RouterRetryConfig struct {
	MaxRetries  int           `mapstructure:"max_retries"`
	MaxTotal    time.Duration `mapstructure:"max_total"`
	BaseBackoff time.Duration `mapstructure:"base_backoff"`
	MaxBackoff  time.Duration `mapstructure:"max_backoff"`
}

type RouterBreakerConfig struct {
	MaxRequests  uint32        `mapstructure:"max_requests"`
	Interval     time.Duration `mapstructure:"interval"`
	Timeout      time.Duration `mapstructure:"timeout"`
	FailureRatio float64       `mapstructure:"failure_ratio"`
	MinRequests  uint32        `mapstructure:"min_requests"`
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
	Enabled            bool    `mapstructure:"enabled"`
	Threshold          float64 `mapstructure:"threshold"`
	EmbeddingModel     string  `mapstructure:"embedding_model"`
	EmbeddingEndpoint  string  `mapstructure:"embedding_endpoint"`
	EmbeddingAPIKey    string  `mapstructure:"embedding_api_key"`
	EmbeddingDimensions int    `mapstructure:"embedding_dimensions"`
	TopK               int     `mapstructure:"top_k"`
	QueryLRUCapacity   int     `mapstructure:"query_lru_capacity"`
	IndexNamePrefix    string  `mapstructure:"index_name_prefix"`
}

// RateLimitRule is one entry in `rate_limits:` of config.yaml. Mirrors
// the runtime ratelimit.RuleConfig so main can translate without copying
// per-field; future fields (e.g. Conditions for selector-based rules)
// extend both.
type RateLimitRule struct {
	Name      string        `mapstructure:"name"`
	Algorithm string        `mapstructure:"algorithm"` // memory_bucket | redis_window
	Rate      string        `mapstructure:"rate"`      // memory_bucket: "100/s" | "60/m" | "1000/h"
	Window    time.Duration `mapstructure:"window"`    // redis_window
	Limit     int           `mapstructure:"limit"`     // redis_window
	Burst     int           `mapstructure:"burst"`     // memory_bucket; 0 → defaults to int(Rate)
	KeyBy     []string      `mapstructure:"key_by"`    // [] | [api_key] | [api_key, model]
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

	v.SetDefault("router.retry.max_retries", 2)
	v.SetDefault("router.retry.max_total", 30*time.Second)
	v.SetDefault("router.retry.base_backoff", 100*time.Millisecond)
	v.SetDefault("router.retry.max_backoff", 5*time.Second)
	v.SetDefault("router.breaker.max_requests", 1)
	v.SetDefault("router.breaker.interval", 60*time.Second)
	v.SetDefault("router.breaker.timeout", 30*time.Second)
	v.SetDefault("router.breaker.failure_ratio", 0.5)
	v.SetDefault("router.breaker.min_requests", 5)

	v.SetDefault("billing.worker.buffer_size", 10000)
	v.SetDefault("billing.worker.workers", 2)
	v.SetDefault("billing.worker.flush_timeout", 5*time.Second)
	v.SetDefault("billing.pricing_reload_interval", 30*time.Minute)

	v.SetDefault("providers_file", "configs/providers.yaml")

	v.SetDefault("cache.exact.enabled", true)
	v.SetDefault("cache.exact.ttl", time.Hour)
	v.SetDefault("cache.semantic.threshold", 0.95)
	v.SetDefault("cache.semantic.embedding_model", "text-embedding-3-small")
	v.SetDefault("cache.semantic.top_k", 5)

	v.SetDefault("prompt.compression.enabled", false)
	v.SetDefault("prompt.compression.trigger_ratio", 0.8)
	v.SetDefault("prompt.compression.min_keep_messages", 2)
	v.SetDefault("prompt.compression.default_window", 128_000)
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

	if c.Prompt.Compression.Enabled {
		if r := c.Prompt.Compression.TriggerRatio; r <= 0 || r > 1 {
			errs = append(errs, fmt.Errorf("prompt.compression.trigger_ratio %v out of (0,1]", r))
		}
		if c.Prompt.Compression.DefaultWindow <= 0 {
			errs = append(errs, errors.New("prompt.compression.default_window must be > 0"))
		}
		if c.Prompt.Compression.MinKeepMessages < 0 {
			errs = append(errs, errors.New("prompt.compression.min_keep_messages must be >= 0"))
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
