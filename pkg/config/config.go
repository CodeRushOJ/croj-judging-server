package config

import (
	"fmt"
	"math"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config 应用的总配置
type Config struct {
	RocketMQ         RocketMQConfig         `yaml:"rocketmq"`
	Database         DatabaseConfig         `yaml:"database"`
	JudgeResult      JudgeResultConfig      `yaml:"judge-result"`
	TestBundles      TestBundleConfig       `yaml:"test-bundles"`
	SandboxDiscovery SandboxDiscoveryConfig `yaml:"sandbox-discovery"`
	ExternalAPI      ExternalAPIConfig      `yaml:"external-api"`
	// 可以添加其他配置项，例如日志级别、沙盒路径等
}

type ExternalAPIConfig struct {
	Enabled              bool   `yaml:"enabled"`
	ListenAddress        string `yaml:"listen-address"`
	WorkerID             string `yaml:"worker-id"`
	WorkerConcurrency    int    `yaml:"worker-concurrency"`
	LeaseDuration        string `yaml:"lease-duration"`
	HeartbeatInterval    string `yaml:"heartbeat-interval"`
	ControlPollInterval  string `yaml:"control-poll-interval"`
	IdleBackoff          string `yaml:"idle-backoff"`
	RetryDelay           string `yaml:"retry-delay"`
	ShutdownTimeout      string `yaml:"shutdown-timeout"`
	ReadinessTimeout     string `yaml:"readiness-timeout"`
	RedisAddress         string `yaml:"redis-address"`
	RedisPassword        string `yaml:"redis-password"`
	RedisDB              int    `yaml:"redis-db"`
	RedisQuotaPrefix     string `yaml:"redis-quota-prefix"`
	AuthPepperBase64     string `yaml:"auth-pepper-base64"`
	IdempotencyPepperB64 string `yaml:"idempotency-pepper-base64"`
	CursorKeyBase64      string `yaml:"cursor-key-base64"`
	SourceKeyBase64      string `yaml:"source-key-base64"`
	SourceKeyVersion     int    `yaml:"source-key-version"`
	IdempotencyTTL       string `yaml:"idempotency-ttl"`
	QuotaRefillPeriod    string `yaml:"quota-refill-period"`
	JobSubmitCapacity    int64  `yaml:"job-submit-capacity"`
	BundleByteCapacity   int64  `yaml:"bundle-byte-capacity"`
}

// RocketMQConfig RocketMQ 相关配置
type RocketMQConfig struct {
	NameServer string `yaml:"name-server"`
	// Producer   ProducerConfig `yaml:"producer"` // 如果需要生产者配置则取消注释
	Consumer ConsumerConfig `yaml:"consumer"`
	Topic    string         `yaml:"topic"`
}

// ProducerConfig (如果需要)
// type ProducerConfig struct {
//  Group string `yaml:"group"`
// }

// ConsumerConfig RocketMQ 消费者特定配置
type ConsumerConfig struct {
	Group             string `yaml:"group"`
	MaxReconsumeTimes int32  `yaml:"max-reconsume-times"`
}

// DatabaseConfig 数据库相关配置
type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Name     string `yaml:"name"`
}

type JudgeResultConfig struct {
	BackendURL      string `yaml:"backend-url"`
	ServiceToken    string `yaml:"service-token"`
	CallbackTimeout string `yaml:"callback-timeout"`
	CacheCapacity   int    `yaml:"cache-capacity"`
	CacheTTL        string `yaml:"cache-ttl"`
}

type TestBundleConfig struct {
	Endpoint             string `yaml:"endpoint"`
	Bucket               string `yaml:"bucket"`
	Region               string `yaml:"region"`
	UseTLS               bool   `yaml:"use-tls"`
	AccessKey            string `yaml:"access-key"`
	SecretKey            string `yaml:"secret-key"`
	CacheDir             string `yaml:"cache-dir"`
	CacheMaxBytes        int64  `yaml:"cache-max-bytes"`
	MaxObjectBytes       int64  `yaml:"max-object-bytes"`
	CacheTTL             string `yaml:"cache-ttl"`
	MaxFiles             int    `yaml:"max-files"`
	MaxManifestBytes     int64  `yaml:"max-manifest-bytes"`
	MaxCaseBytes         int64  `yaml:"max-case-bytes"`
	MaxUncompressedBytes int64  `yaml:"max-uncompressed-bytes"`
	MaxCompressionRatio  uint64 `yaml:"max-compression-ratio"`
	MaxInfraAttempts     int    `yaml:"max-infra-attempts"`
	MaxTimeLimitMillis   int    `yaml:"max-time-limit-millis"`
	MaxMemoryLimitMiB    int    `yaml:"max-memory-limit-mib"`
}

type SandboxDiscoveryConfig struct {
	Target                   string `yaml:"target"`
	AllowLegacyEndpointSlice bool   `yaml:"allow-legacy-endpoint-slice"`
	Namespace                string `yaml:"namespace"`
	Service                  string `yaml:"service"`
	PortName                 string `yaml:"port-name"`
	RefreshInterval          string `yaml:"refresh-interval"`
	ExecuteTimeout           string `yaml:"execute-timeout"`
	MaxConnections           int    `yaml:"max-connections"`
	ConnectionIdleTTL        string `yaml:"connection-idle-ttl"`
	Kubeconfig               string `yaml:"kubeconfig"`
}

// LoadConfig 从指定路径加载配置文件
func LoadConfig(configPath string) (*Config, error) {
	fmt.Printf("Loading config from: %s\n", configPath)
	config := &Config{}

	file, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file %s: %w", configPath, err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(config); err != nil {
		return nil, fmt.Errorf("failed to decode config file %s: %w", configPath, err)
	}

	if err := config.applyEnvironment(); err != nil {
		return nil, err
	}

	return config, nil
}

func (config *Config) applyEnvironment() error {
	if value, ok := os.LookupEnv("EXTERNAL_API_ENABLED"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("EXTERNAL_API_ENABLED must be true or false")
		}
		config.ExternalAPI.Enabled = parsed
	}
	overrideString(&config.ExternalAPI.ListenAddress, "EXTERNAL_API_LISTEN_ADDRESS")
	overrideString(&config.ExternalAPI.WorkerID, "EXTERNAL_WORKER_ID")
	overrideString(&config.ExternalAPI.LeaseDuration, "EXTERNAL_WORKER_LEASE_DURATION")
	overrideString(&config.ExternalAPI.HeartbeatInterval, "EXTERNAL_WORKER_HEARTBEAT_INTERVAL")
	overrideString(&config.ExternalAPI.ControlPollInterval, "EXTERNAL_WORKER_CONTROL_POLL_INTERVAL")
	overrideString(&config.ExternalAPI.IdleBackoff, "EXTERNAL_WORKER_IDLE_BACKOFF")
	overrideString(&config.ExternalAPI.RetryDelay, "EXTERNAL_WORKER_RETRY_DELAY")
	overrideString(&config.ExternalAPI.ShutdownTimeout, "EXTERNAL_API_SHUTDOWN_TIMEOUT")
	overrideString(&config.ExternalAPI.ReadinessTimeout, "EXTERNAL_API_READINESS_TIMEOUT")
	overrideString(&config.ExternalAPI.RedisAddress, "REDIS_ADDRESS")
	overrideString(&config.ExternalAPI.RedisPassword, "REDIS_PASSWORD")
	overrideString(&config.ExternalAPI.RedisQuotaPrefix, "EXTERNAL_REDIS_QUOTA_PREFIX")
	overrideString(&config.ExternalAPI.AuthPepperBase64, "EXTERNAL_API_AUTH_PEPPER_BASE64")
	overrideString(&config.ExternalAPI.IdempotencyPepperB64, "EXTERNAL_IDEMPOTENCY_PEPPER_BASE64")
	overrideString(&config.ExternalAPI.CursorKeyBase64, "EXTERNAL_CURSOR_KEY_BASE64")
	overrideString(&config.ExternalAPI.SourceKeyBase64, "EXTERNAL_SOURCE_KEY_BASE64")
	overrideString(&config.ExternalAPI.IdempotencyTTL, "EXTERNAL_IDEMPOTENCY_TTL")
	overrideString(&config.ExternalAPI.QuotaRefillPeriod, "EXTERNAL_QUOTA_REFILL_PERIOD")
	if err := overridePositiveInt(&config.ExternalAPI.WorkerConcurrency, "EXTERNAL_WORKER_CONCURRENCY"); err != nil {
		return err
	}
	if err := overridePositiveInt(&config.ExternalAPI.SourceKeyVersion, "EXTERNAL_SOURCE_KEY_VERSION"); err != nil {
		return err
	}
	if value, ok := os.LookupEnv("REDIS_DB"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			return fmt.Errorf("REDIS_DB must be a non-negative integer")
		}
		config.ExternalAPI.RedisDB = parsed
	}
	if err := overridePositiveInt64(&config.ExternalAPI.JobSubmitCapacity, "EXTERNAL_JOB_SUBMIT_CAPACITY"); err != nil {
		return err
	}
	if err := overridePositiveInt64(&config.ExternalAPI.BundleByteCapacity, "EXTERNAL_BUNDLE_BYTE_CAPACITY"); err != nil {
		return err
	}
	overrideString(&config.Database.Host, "DATABASE_HOST")
	overrideString(&config.Database.User, "DATABASE_USERNAME")
	overrideString(&config.Database.Password, "DATABASE_PASSWORD")
	overrideString(&config.Database.Name, "DATABASE_NAME")
	overrideString(&config.RocketMQ.NameServer, "ROCKETMQ_NAME_SERVER")
	overrideString(&config.RocketMQ.Topic, "SUBMISSION_TOPIC")
	overrideString(&config.RocketMQ.Consumer.Group, "ROCKETMQ_CONSUMER_GROUP")
	if value, ok := os.LookupEnv("ROCKETMQ_MAX_RECONSUME_TIMES"); ok {
		parsed, err := strconv.ParseInt(value, 10, 32)
		if err != nil || parsed <= 0 || parsed > math.MaxInt32 {
			return fmt.Errorf("ROCKETMQ_MAX_RECONSUME_TIMES must be an integer between 1 and %d", math.MaxInt32)
		}
		config.RocketMQ.Consumer.MaxReconsumeTimes = int32(parsed)
	}
	overrideString(&config.JudgeResult.BackendURL, "BACKEND_INTERNAL_URL")
	overrideString(&config.JudgeResult.ServiceToken, "JUDGE_RESULT_SERVICE_TOKEN")
	overrideString(&config.JudgeResult.CallbackTimeout, "JUDGE_RESULT_CALLBACK_TIMEOUT")
	overrideString(&config.JudgeResult.CacheTTL, "JUDGE_TASK_CACHE_TTL")
	overrideString(&config.TestBundles.Endpoint, "TEST_BUNDLE_ENDPOINT")
	overrideString(&config.TestBundles.Bucket, "TEST_BUNDLE_BUCKET")
	overrideString(&config.TestBundles.Region, "TEST_BUNDLE_REGION")
	overrideString(&config.TestBundles.AccessKey, "TEST_BUNDLE_ACCESS_KEY")
	overrideString(&config.TestBundles.SecretKey, "TEST_BUNDLE_SECRET_KEY")
	overrideString(&config.TestBundles.CacheDir, "TEST_BUNDLE_CACHE_DIR")
	overrideString(&config.TestBundles.Endpoint, "OBJECT_STORAGE_ENDPOINT")
	overrideString(&config.TestBundles.Bucket, "OBJECT_STORAGE_BUCKET")
	overrideString(&config.TestBundles.Region, "OBJECT_STORAGE_REGION")
	overrideString(&config.TestBundles.AccessKey, "OBJECT_STORAGE_ACCESS_KEY")
	overrideString(&config.TestBundles.SecretKey, "OBJECT_STORAGE_SECRET_KEY")
	overrideString(&config.TestBundles.CacheDir, "JUDGE_BUNDLE_CACHE_DIR")
	overrideString(&config.TestBundles.CacheTTL, "TEST_BUNDLE_CACHE_TTL")
	overrideString(&config.TestBundles.CacheTTL, "JUDGE_BUNDLE_CACHE_TTL")
	tlsValue, tlsConfigured := os.LookupEnv("TEST_BUNDLE_USE_TLS")
	if canonicalValue, ok := os.LookupEnv("OBJECT_STORAGE_USE_TLS"); ok {
		tlsValue, tlsConfigured = canonicalValue, true
	}
	if tlsConfigured {
		parsed, err := strconv.ParseBool(tlsValue)
		if err != nil {
			return fmt.Errorf("OBJECT_STORAGE_USE_TLS must be true or false")
		}
		config.TestBundles.UseTLS = parsed
	}
	overrideString(&config.SandboxDiscovery.Namespace, "SANDBOX_NAMESPACE")
	overrideString(&config.SandboxDiscovery.Target, "SANDBOX_GRPC_TARGET")
	overrideString(&config.SandboxDiscovery.Service, "SANDBOX_SERVICE")
	overrideString(&config.SandboxDiscovery.PortName, "SANDBOX_PORT_NAME")
	overrideString(&config.SandboxDiscovery.RefreshInterval, "SANDBOX_REFRESH_INTERVAL")
	overrideString(&config.SandboxDiscovery.ExecuteTimeout, "SANDBOX_EXECUTE_TIMEOUT")
	overrideString(&config.SandboxDiscovery.ConnectionIdleTTL, "SANDBOX_CONNECTION_IDLE_TTL")
	overrideString(&config.SandboxDiscovery.Kubeconfig, "KUBECONFIG")
	if value, ok := os.LookupEnv("SANDBOX_ALLOW_LEGACY_ENDPOINT_SLICE"); ok {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("SANDBOX_ALLOW_LEGACY_ENDPOINT_SLICE must be true or false")
		}
		config.SandboxDiscovery.AllowLegacyEndpointSlice = parsed
	}
	if value, ok := os.LookupEnv("DATABASE_PORT"); ok {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("DATABASE_PORT must be an integer between 1 and 65535")
		}
		config.Database.Port = port
	}
	if err := overridePositiveInt(&config.JudgeResult.CacheCapacity, "JUDGE_TASK_CACHE_CAPACITY"); err != nil {
		return err
	}
	if err := overridePositiveInt(&config.SandboxDiscovery.MaxConnections, "SANDBOX_MAX_CONNECTIONS"); err != nil {
		return err
	}
	for _, override := range []struct {
		target *int
		name   string
	}{
		{&config.TestBundles.MaxFiles, "TEST_BUNDLE_MAX_FILES"},
		{&config.TestBundles.MaxInfraAttempts, "TEST_BUNDLE_MAX_INFRA_ATTEMPTS"},
	} {
		if err := overridePositiveInt(override.target, override.name); err != nil {
			return err
		}
	}
	for _, override := range []struct {
		target *int
		name   string
	}{
		{&config.TestBundles.MaxFiles, "JUDGE_BUNDLE_MAX_FILES"},
		{&config.TestBundles.MaxInfraAttempts, "JUDGE_BUNDLE_MAX_INFRA_ATTEMPTS"},
		{&config.TestBundles.MaxTimeLimitMillis, "JUDGE_MAX_TIME_LIMIT_MILLIS"},
		{&config.TestBundles.MaxMemoryLimitMiB, "JUDGE_MAX_MEMORY_LIMIT_MIB"},
	} {
		if err := overridePositiveInt(override.target, override.name); err != nil {
			return err
		}
	}
	for _, override := range []struct {
		target *int64
		name   string
	}{
		{&config.TestBundles.CacheMaxBytes, "TEST_BUNDLE_CACHE_MAX_BYTES"},
		{&config.TestBundles.MaxObjectBytes, "TEST_BUNDLE_MAX_OBJECT_BYTES"},
		{&config.TestBundles.MaxManifestBytes, "TEST_BUNDLE_MAX_MANIFEST_BYTES"},
		{&config.TestBundles.MaxCaseBytes, "TEST_BUNDLE_MAX_CASE_BYTES"},
		{&config.TestBundles.MaxUncompressedBytes, "TEST_BUNDLE_MAX_UNCOMPRESSED_BYTES"},
	} {
		if err := overridePositiveInt64(override.target, override.name); err != nil {
			return err
		}
	}
	for _, override := range []struct {
		target *int64
		name   string
	}{
		{&config.TestBundles.CacheMaxBytes, "JUDGE_BUNDLE_CACHE_MAX_BYTES"},
		{&config.TestBundles.MaxObjectBytes, "JUDGE_BUNDLE_MAX_OBJECT_BYTES"},
		{&config.TestBundles.MaxManifestBytes, "JUDGE_BUNDLE_MAX_MANIFEST_BYTES"},
		{&config.TestBundles.MaxCaseBytes, "JUDGE_BUNDLE_MAX_CASE_BYTES"},
		{&config.TestBundles.MaxUncompressedBytes, "JUDGE_BUNDLE_MAX_UNCOMPRESSED_BYTES"},
	} {
		if err := overridePositiveInt64(override.target, override.name); err != nil {
			return err
		}
	}
	compressionRatio, compressionRatioConfigured := os.LookupEnv("TEST_BUNDLE_MAX_COMPRESSION_RATIO")
	if canonicalValue, ok := os.LookupEnv("JUDGE_BUNDLE_MAX_COMPRESSION_RATIO"); ok {
		compressionRatio, compressionRatioConfigured = canonicalValue, true
	}
	if compressionRatioConfigured {
		parsed, err := strconv.ParseUint(compressionRatio, 10, 64)
		if err != nil || parsed == 0 {
			return fmt.Errorf("JUDGE_BUNDLE_MAX_COMPRESSION_RATIO must be a positive integer")
		}
		config.TestBundles.MaxCompressionRatio = parsed
	}
	return nil
}

func overrideString(target *string, environmentVariable string) {
	if value, ok := os.LookupEnv(environmentVariable); ok {
		*target = value
	}
}

func overridePositiveInt(target *int, environmentVariable string) error {
	value, ok := os.LookupEnv(environmentVariable)
	if !ok {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("%s must be a positive integer", environmentVariable)
	}
	*target = parsed
	return nil
}

func overridePositiveInt64(target *int64, environmentVariable string) error {
	value, ok := os.LookupEnv(environmentVariable)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("%s must be a positive integer", environmentVariable)
	}
	*target = parsed
	return nil
}
