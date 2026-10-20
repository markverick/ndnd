package nac

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"testing"

	tu "github.com/named-data/ndnd/std/utils/testutils"
	"github.com/stretchr/testify/require"
)

func TestNewKeyPair(t *testing.T) {
	tu.SetT(t)

	kek, kdk, err := NewKeyPair()
	require.NoError(t, err)
	require.Equal(t, kek.ID, kdk.ID)
	require.Equal(t, 16, len(kek.ID))
	require.NotNil(t, kek.PublicKey)
	require.NotNil(t, kdk.PrivateKey)
}

func TestSymEncryptDecrypt(t *testing.T) {
	tu.SetT(t)

	key := make([]byte, 32)
	rand.Read(key)

	plain := []byte("hello nac")
	ct := tu.NoErr(SymEncrypt(key, plain))
	got := tu.NoErr(SymDecrypt(key, ct))
	require.Equal(t, plain, got)
}

func TestSymDecryptWrongKey(t *testing.T) {
	tu.SetT(t)

	k1 := make([]byte, 32)
	k2 := make([]byte, 32)
	rand.Read(k1)
	rand.Read(k2)

	ct, _ := SymEncrypt(k1, []byte("secret"))
	_, err := SymDecrypt(k2, ct)
	require.Error(t, err)
}

func TestAsymEncryptDecrypt(t *testing.T) {
	tu.SetT(t)

	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	plain := []byte("ecies test data")
	ct := tu.NoErr(AsymEncrypt(priv.PublicKey(), plain))
	got := tu.NoErr(AsymDecrypt(priv, ct))
	require.Equal(t, plain, got)
}

func TestFullEncryptionChain(t *testing.T) {
	tu.SetT(t)

	am := tu.NoErr(NewAccessManager("/test", "docs"))

	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, am.AddMember("/test/consumer/KEY/abc", priv.PublicKey()))

	plain := []byte("secret document contents")
	encC, encCK, err := NewEncryptor("/test/data", "/test", "docs", am.KEK()).Encrypt("/test/data/doc1", plain)
	require.NoError(t, err)

	blob, ok := am.GetEncryptedKDK("/test/consumer/KEY/abc")
	require.True(t, ok)

	got := tu.NoErr(NewDecryptor("/test/consumer/KEY/abc", priv).Decrypt(encC, encCK, blob))
	require.Equal(t, plain, got)
}

func TestUnauthorizedConsumer(t *testing.T) {
	tu.SetT(t)

	am, _ := NewAccessManager("/test", "docs")
	_, ok := am.GetEncryptedKDK("/test/eve/KEY/xyz")
	require.False(t, ok)
	require.False(t, am.IsAuthorized("/test/eve/KEY/xyz"))
}

func TestMultipleConsumers(t *testing.T) {
	tu.SetT(t)

	am, _ := NewAccessManager("/test", "docs")

	alicePriv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	am.AddMember("/test/alice/KEY/a1", alicePriv.PublicKey())

	bobPriv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	am.AddMember("/test/bob/KEY/b1", bobPriv.PublicKey())

	require.True(t, am.IsAuthorized("/test/alice/KEY/a1"))
	require.True(t, am.IsAuthorized("/test/bob/KEY/b1"))

	plain := []byte("shared secret")
	encC, encCK, _ := NewEncryptor("/test/data", "/test", "docs", am.KEK()).Encrypt("/test/data/doc1", plain)

	// both can decrypt
	for _, tc := range []struct {
		name string
		key  string
		priv *ecdh.PrivateKey
	}{
		{"alice", "/test/alice/KEY/a1", alicePriv},
		{"bob", "/test/bob/KEY/b1", bobPriv},
	} {
		blob, ok := am.GetEncryptedKDK(tc.key)
		require.True(t, ok, "%s should be authorized", tc.name)
		got := tu.NoErr(NewDecryptor(tc.key, tc.priv).Decrypt(encC, encCK, blob))
		require.Equal(t, plain, got, "%s decrypted wrong content", tc.name)
	}

	// eve can't get a kdk
	_, ok := am.GetEncryptedKDK("/test/eve/KEY/e1")
	require.False(t, ok)

	// eve can't decrypt alice's kdk blob either
	evePriv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	aliceBlob, _ := am.GetEncryptedKDK("/test/alice/KEY/a1")
	_, err := AsymDecrypt(evePriv, aliceBlob)
	require.Error(t, err)
}

func TestNaming(t *testing.T) {
	tu.SetT(t)

	id := []byte{0x01, 0x02}
	require.Equal(t, "/alice/NAC/docs/KEK/0102", KEKName("/alice", "docs", id))
	require.Equal(t, "/alice/NAC/docs/KDK/0102", KDKName("/alice", "docs", id))
	require.Equal(t, "/alice/NAC/docs/KDK/0102/ENCRYPTED-BY/bob/KEY/b1",
		EncryptedKDKName("/alice", "docs", id, "/bob/KEY/b1"))

	ckid := []byte{0xAB, 0xCD}
	require.Equal(t, "/alice/data/CK/abcd", ContentKeyName("/alice/data", ckid))
	require.Equal(t, "/alice/data/CK/abcd/ENCRYPTED-BY/alice/NAC/docs/KEK/0102",
		CKEncryptedName("/alice/data", ckid, KEKName("/alice", "docs", id)))

	base, key, err := ParseEncryptedByName(EncryptedKDKName("/alice", "docs", id, "/bob/KEY/b1"))
	require.NoError(t, err)
	require.Equal(t, "/alice/NAC/docs/KDK/0102", base)
	require.Equal(t, "bob/KEY/b1", key)
}

func TestKeySerialization(t *testing.T) {
	tu.SetT(t)

	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)

	pub2 := tu.NoErr(DeserializePublicKey(priv.PublicKey().Bytes()))
	require.True(t, bytes.Equal(priv.PublicKey().Bytes(), pub2.Bytes()))

	priv2 := tu.NoErr(DeserializePrivateKey(priv.Bytes()))
	require.True(t, bytes.Equal(priv.Bytes(), priv2.Bytes()))
}
