package config

import (
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Config 应用的总配置
type Config struct {
	RocketMQ         RocketMQConfig         `yaml:"rocketmq"`
	Database         DatabaseConfig         `yaml:"database"`
	SandboxDiscovery SandboxDiscoveryConfig `yaml:"sandbox-discovery"`
	// 可以添加其他配置项，例如日志级别、沙盒路径等
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
	Group string `yaml:"group"`
}

// DatabaseConfig 数据库相关配置
type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Name     string `yaml:"name"`
}

type SandboxDiscoveryConfig struct {
	Namespace       string `yaml:"namespace"`
	Service         string `yaml:"service"`
	PortName        string `yaml:"port-name"`
	RefreshInterval string `yaml:"refresh-interval"`
	ExecuteTimeout  string `yaml:"execute-timeout"`
	Kubeconfig      string `yaml:"kubeconfig"`
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
	overrideString(&config.Database.Host, "DATABASE_HOST")
	overrideString(&config.Database.User, "DATABASE_USERNAME")
	overrideString(&config.Database.Password, "DATABASE_PASSWORD")
	overrideString(&config.Database.Name, "DATABASE_NAME")
	overrideString(&config.RocketMQ.NameServer, "ROCKETMQ_NAME_SERVER")
	overrideString(&config.RocketMQ.Topic, "SUBMISSION_TOPIC")
	overrideString(&config.RocketMQ.Consumer.Group, "ROCKETMQ_CONSUMER_GROUP")
	overrideString(&config.SandboxDiscovery.Namespace, "SANDBOX_NAMESPACE")
	overrideString(&config.SandboxDiscovery.Service, "SANDBOX_SERVICE")
	overrideString(&config.SandboxDiscovery.PortName, "SANDBOX_PORT_NAME")
	overrideString(&config.SandboxDiscovery.RefreshInterval, "SANDBOX_REFRESH_INTERVAL")
	overrideString(&config.SandboxDiscovery.ExecuteTimeout, "SANDBOX_EXECUTE_TIMEOUT")
	overrideString(&config.SandboxDiscovery.Kubeconfig, "KUBECONFIG")
	if value, ok := os.LookupEnv("DATABASE_PORT"); ok {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("DATABASE_PORT must be an integer between 1 and 65535")
		}
		config.Database.Port = port
	}
	return nil
}

func overrideString(target *string, environmentVariable string) {
	if value, ok := os.LookupEnv(environmentVariable); ok {
		*target = value
	}
}
