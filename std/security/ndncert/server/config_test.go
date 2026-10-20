package server

import (
	"os"
	"path/filepath"
	"testing"

	tu "github.com/named-data/ndnd/std/utils/testutils"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigCPP(t *testing.T) {
	tu.SetT(t)

	raw := `{
  "ca-prefix": "/example",
  "ca-info": "An example NDNCERT CA",
  "max-validity-period": "1296000",
	"supported-challenges": [{"challenge": "pin"}, {"challenge": "email"}],
	"email": {
		"smtp-server": "smtp.example.org",
		"smtp-port": 587,
		"smtp-username": "ndncert",
		"smtp-password-env": "NDNCERT_SMTP_PASSWORD",
		"from": "NDN Certificate Robot <noreply@example.org>",
		"subject": "Your NDNCERT verification code"
	}
}`

	dir := t.TempDir()
	path := filepath.Join(dir, "ca.conf")
	require.NoError(t, os.WriteFile(path, []byte(raw), 0644))

	cfg := tu.NoErr(LoadConfig(path))
	require.Equal(t, "/example", cfg.CaPrefix)
	require.Equal(t, "An example NDNCERT CA", cfg.CaInfo)
	require.Equal(t, "1296000", cfg.MaxValidityPeriod)
	require.Equal(t, 2, len(cfg.SupportedChallenges))
	require.True(t, cfg.SupportsChallenge("pin"))
	require.True(t, cfg.SupportsChallenge("email"))
	require.Equal(t, "smtp.example.org", cfg.Email.SMTPServer)
	require.False(t, cfg.SupportsChallenge("dns"))

	maxVal := tu.NoErr(cfg.GetMaxValidityPeriodSeconds())
	require.Equal(t, uint64(1296000), maxVal)
}

func TestLoadConfigSample(t *testing.T) {
	tu.SetT(t)

	path := filepath.Join("..", "..", "..", "examples", "ndncert", "ca.conf.sample")
	cfg := tu.NoErr(LoadConfig(path))
	require.NotEmpty(t, cfg.CaPrefix)
	require.NotEmpty(t, cfg.MaxValidityPeriod)
	require.NotEmpty(t, cfg.SupportedChallenges)
}
