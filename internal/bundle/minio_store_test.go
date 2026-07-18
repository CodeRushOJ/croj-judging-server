package bundle

import "testing"

func TestNewMinIOStoreRejectsIncompleteConfiguration(t *testing.T) {
	valid := MinIOConfig{
		Endpoint:  "minio.coderushoj.svc:9000",
		Bucket:    "judge-bundles",
		Region:    "us-east-1",
		AccessKey: "judge-readonly",
		SecretKey: "0123456789abcdef0123456789abcdef",
	}
	tests := map[string]func(*MinIOConfig){
		"endpoint": func(config *MinIOConfig) { config.Endpoint = "" },
		"scheme":   func(config *MinIOConfig) { config.Endpoint = "http://minio:9000" },
		"bucket":   func(config *MinIOConfig) { config.Bucket = "" },
		"region":   func(config *MinIOConfig) { config.Region = "" },
		"access":   func(config *MinIOConfig) { config.AccessKey = "" },
		"secret":   func(config *MinIOConfig) { config.SecretKey = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := NewMinIOStore(config); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}
