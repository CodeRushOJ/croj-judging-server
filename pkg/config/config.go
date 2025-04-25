package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config 应用的总配置
type Config struct {
	RocketMQ  RocketMQConfig  `yaml:"rocketmq"`
	Database  DatabaseConfig  `yaml:"database"`
	Zookeeper ZookeeperConfig `yaml:"zookeeper"`
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

// ZookeeperConfig Zookeeper 相关配置
type ZookeeperConfig struct {
	Servers []string `yaml:"servers"`
	Path    string   `yaml:"path"` // 沙盒节点在 ZK 中的基础路径
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

	// 可以添加配置校验逻辑

	return config, nil
}
