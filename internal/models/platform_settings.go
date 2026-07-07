package models

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// PlatformSettings stores config like platform fee percentages.
type PlatformSettings struct {
	ID        uuid.UUID       `db:"id" json:"id"`
	Key       string          `db:"key" json:"key"`
	Value     json.RawMessage `db:"value" json:"value"`
	CreatedAt time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt time.Time       `db:"updated_at" json:"updated_at"`
}

// PlatformFeeConfig is the structure for the "platform_fee" key.
type PlatformFeeConfig struct {
	HostPercentage     int `json:"host_percentage"`     // e.g. 70
	PlatformPercentage int `json:"platform_percentage"` // e.g. 30
}

// EffectiveFeeConfig applies a host's commission override (Host.PlatformFeePercentage)
// on top of the global default. Pass nil for hostOverridePct to get the global
// config back unchanged. Callers must use the returned config (not the global
// one) when computing a booking's fee split, so per-host rates take effect.
func EffectiveFeeConfig(global *PlatformFeeConfig, hostOverridePct *int) *PlatformFeeConfig {
	if hostOverridePct == nil {
		return global
	}
	pct := *hostOverridePct
	return &PlatformFeeConfig{
		HostPercentage:     100 - pct,
		PlatformPercentage: pct,
	}
}
