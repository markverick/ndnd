package server

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	enc "github.com/named-data/ndnd/std/encoding"
	spec "github.com/named-data/ndnd/std/ndn/spec_2022"
	"github.com/named-data/ndnd/std/security/ndncert/tlv"
)

type ChallengeConfig struct {
	Name string `json:"challenge"`
}

type CaConfig struct {
	CaPrefix            string            `json:"ca-prefix"`
	CaInfo              string            `json:"ca-info"`
	MaxValidityPeriod   string            `json:"max-validity-period"`
	SupportedChallenges []ChallengeConfig `json:"supported-challenges"`

	CaCertPath string `json:"-"`
	CaKeyPath  string `json:"-"`
}

func LoadConfig(path string) (*CaConfig, error) {
	// log.Printf("loading ca config: %s", path)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg CaConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config json: %w", err)
	}

	if cfg.CaPrefix == "" {
		return nil, fmt.Errorf("ca-prefix is required")
	}
	if cfg.MaxValidityPeriod == "" {
		return nil, fmt.Errorf("max-validity-period is required")
	}

	return &cfg, nil
}

func (c *CaConfig) ToCaProfile(caCertWire enc.Wire) (*tlv.CaProfile, error) {
	caName, err := enc.NameFromStr(c.CaPrefix)
	if err != nil {
		return nil, fmt.Errorf("invalid ca-prefix: %w", err)
	}

	mv, err := strconv.ParseUint(c.MaxValidityPeriod, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid max-validity-period: %w", err)
	}

	return &tlv.CaProfile{
		CaPrefix:       &spec.NameContainer{Name: caName},
		CaInfo:         c.CaInfo,
		MaxValidPeriod: mv,
		CaCert:         caCertWire,
	}, nil
}

func (c *CaConfig) GetMaxValidityPeriodSeconds() (uint64, error) {
	return strconv.ParseUint(c.MaxValidityPeriod, 10, 64)
}

func (c *CaConfig) SupportsChallenge(name string) bool {
	for _, ch := range c.SupportedChallenges {
		if ch.Name == name {
			return true
		}
	}
	return false
}
