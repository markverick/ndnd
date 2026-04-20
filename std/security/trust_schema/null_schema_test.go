package trust_schema_test

import (
	"testing"

	enc "github.com/named-data/ndnd/std/encoding"
	ndn "github.com/named-data/ndnd/std/ndn"
	"github.com/named-data/ndnd/std/object/storage"
	sec "github.com/named-data/ndnd/std/security"
	"github.com/named-data/ndnd/std/security/keychain"
	sig "github.com/named-data/ndnd/std/security/signer"
	"github.com/named-data/ndnd/std/security/trust_schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestKeychain creates an in-memory keychain pre-loaded with one Ed25519 key
// under the given identity (e.g. "/router"). Returns the keychain and the signer.
func newTestKeychain(t *testing.T, identity string) (ndn.KeyChain, ndn.Signer) {
	t.Helper()
	idName, err := enc.NameFromStr(identity)
	require.NoError(t, err)
	keyName := sec.MakeKeyName(idName) // /identity/KEY/<random-id>
	ed25519Signer, err := sig.KeygenEd25519(keyName)
	require.NoError(t, err)
	store := storage.NewMemoryStore()
	kc := keychain.NewKeyChainMem(store)
	require.NoError(t, kc.InsertKey(ed25519Signer))
	return kc, ed25519Signer
}

// TestNullSchema_SuggestAlwaysSha256 verifies that NullSchema.Suggest always
// returns a SHA-256 digest signer regardless of what is in the keychain.
// This ensures NullSchema does not change production signing behavior.
func TestNullSchema_SuggestAlwaysSha256(t *testing.T) {
	schema := trust_schema.NewNullSchema()
	name, _ := enc.NameFromStr("/test/data")

	// With nil keychain: should return a signer (SHA-256).
	s := schema.Suggest(name, nil)
	require.NotNil(t, s, "Suggest must not return nil for nil keychain")
	assert.Equal(t, ndn.SignatureDigestSha256, s.Type(),
		"NullSchema must return SHA-256 signer regardless of keychain")

	// With a populated keychain: must still return SHA-256, not the identity key.
	kc, _ := newTestKeychain(t, "/test")
	s2 := schema.Suggest(name, kc)
	require.NotNil(t, s2)
	assert.Equal(t, ndn.SignatureDigestSha256, s2.Type(),
		"NullSchema must ignore the keychain and always return SHA-256")
}

// TestNullSchema_CheckAlwaysTrue verifies that NullSchema.Check always returns true.
func TestNullSchema_CheckAlwaysTrue(t *testing.T) {
	schema := trust_schema.NewNullSchema()
	pkt, _ := enc.NameFromStr("/a/b/c")
	cert, _ := enc.NameFromStr("/trust/anchor/KEY/self")
	assert.True(t, schema.Check(pkt, cert))
}


