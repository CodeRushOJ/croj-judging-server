package httpapi

type LanguageCapability struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Runtime     string `json:"runtime"`
}

type CapabilityLimits struct {
	MaxSourceBytes     int64 `json:"maxSourceBytes"`
	MaxBundleBytes     int64 `json:"maxBundleBytes"`
	MaxCaseBytes       int64 `json:"maxCaseBytes"`
	MaxCaseCount       int   `json:"maxCaseCount"`
	MaxTimeLimitMillis int   `json:"maxTimeLimitMillis"`
	MaxMemoryLimitMiB  int   `json:"maxMemoryLimitMiB"`
}

type Capabilities struct {
	APIVersion string               `json:"apiVersion"`
	Languages  []LanguageCapability `json:"languages"`
	JudgeModes []string             `json:"judgeModes"`
	Checkers   []string             `json:"checkers"`
	Limits     CapabilityLimits     `json:"limits"`
}
