package nac

import (
	"crypto/ecdh"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	basic_engine "github.com/named-data/ndnd/std/engine/basic"
	"github.com/named-data/ndnd/std/engine/face"
	"github.com/named-data/ndnd/std/ndn"
	spec "github.com/named-data/ndnd/std/ndn/spec_2022"
	"github.com/named-data/ndnd/std/object/storage"
	"github.com/named-data/ndnd/std/security"
	"github.com/named-data/ndnd/std/security/keychain"
	sig "github.com/named-data/ndnd/std/security/signer"
	"github.com/named-data/ndnd/std/security/trust_schema"
	"github.com/named-data/ndnd/std/types/optional"
	tu "github.com/named-data/ndnd/std/utils/testutils"
	"github.com/stretchr/testify/require"
)

var caSeq int

func makeCA(t *testing.T) (ndn.Signer, enc.Wire, ndn.Data) {
	t.Helper()
	caSeq++
	name := tu.NoErr(enc.NameFromStr(fmt.Sprintf("/test/KEY/ca-%d", caSeq)))
	s := tu.NoErr(sig.KeygenEcc(name, elliptic.P256()))
	wire := tu.NoErr(security.SelfSign(security.SignCertArgs{
		Signer:    s,
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
	}))
	data, _, err := spec.Spec{}.ReadData(enc.NewWireView(wire))
	require.NoError(t, err)
	return s, wire, data
}

func makeUserCert(t *testing.T, ca ndn.Signer, id string) (ndn.Signer, enc.Wire, ndn.Data) {
	t.Helper()
	name := tu.NoErr(enc.NameFromStr(id + "/KEY/k1"))
	s := tu.NoErr(sig.KeygenEcc(name, elliptic.P256()))
	keyData := tu.NoErr(sig.MarshalSecretToData(s))
	wire := tu.NoErr(security.SignCert(security.SignCertArgs{
		Signer:    ca,
		Data:      keyData,
		IssuerId:  enc.NewGenericComponent("NDNCERT"),
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(24 * time.Hour),
	}))
	data, _, err := spec.Spec{}.ReadData(enc.NewWireView(wire))
	require.NoError(t, err)
	return s, wire, data
}

func dummyEngine(t *testing.T) ndn.Engine {
	t.Helper()
	e := basic_engine.NewEngine(face.NewDummyFace(), basic_engine.NewDummyTimer())
	require.NoError(t, e.Start())
	t.Cleanup(func() { e.Stop() })
	return e
}

func makeTrust(t *testing.T, caCertWire enc.Wire, caCert ndn.Data) *security.TrustConfig {
	t.Helper()
	kc := keychain.NewKeyChainMem(storage.NewMemoryStore())
	require.NoError(t, kc.InsertCert(caCertWire.Join()))
	trust := tu.NoErr(security.NewTrustConfig(kc, trust_schema.NewNullSchema(), []enc.Name{caCert.Name()}))
	return trust
}

func makeKeyServer(t *testing.T, e ndn.Engine, caCertWire enc.Wire, caCert ndn.Data) *KeyServer {
	t.Helper()
	name := tu.NoErr(enc.NameFromStr("/test/KEY/nac"))
	s := tu.NoErr(sig.KeygenEd25519(name))
	trust := makeTrust(t, caCertWire, caCert)
	ks := tu.NoErr(NewKeyServer(e, s, "/test", "docs", trust))
	return ks
}

func keyName(cert ndn.Data) enc.Name {
	n := cert.Name()
	if len(n) >= 2 {
		n = n[:len(n)-2]
	}
	return n
}

func TestEnrollAndDecrypt(t *testing.T) {
	tu.SetT(t)

	ca, caCertWire, caCert := makeCA(t)
	_, certWire, cert := makeUserCert(t, ca, "/test/alice")

	e := dummyEngine(t)
	ks := makeKeyServer(t, e, caCertWire, caCert)

	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	payload := append(priv.PublicKey().Bytes(), certWire.Join()...)

	// parse cert back out of enrollment payload and validate
	parsed, sigCov, err := spec.Spec{}.ReadData(enc.NewBufferView(payload[32:]))
	require.NoError(t, err)
	require.True(t, tu.NoErr(sig.ValidateData(parsed, sigCov, caCert)))

	kn := keyName(cert)
	require.NoError(t, ks.AddMember(kn.String(), priv.PublicKey()))

	am := ks.AccessManager()
	require.True(t, am.IsAuthorized(kn.String()))

	plain := []byte("temp=22.5")
	enc1, ck, err := NewEncryptor("/test/d", "/test", "docs", am.KEK()).Encrypt("/test/d/x", plain)
	require.NoError(t, err)

	blob, ok := am.GetEncryptedKDK(kn.String())
	require.True(t, ok)

	got, err := NewDecryptor(kn.String(), priv).Decrypt(enc1, ck, blob)
	require.NoError(t, err)
	require.Equal(t, plain, got)
}

func TestRejectUntrustedCert(t *testing.T) {
	tu.SetT(t)

	_, realCACertWire, realCACert := makeCA(t)
	rogueCA, _, _ := makeCA(t)
	_, rogueCertWire, _ := makeUserCert(t, rogueCA, "/test/mallory")

	// build trust config anchored to the real CA
	trust := makeTrust(t, realCACertWire, realCACert)

	// parse rogue cert and validate against the real CA's trust config
	rogueParsed, rogueSigCov, err := spec.Spec{}.ReadData(enc.NewBufferView(rogueCertWire.Join()))
	require.NoError(t, err)

	done := make(chan bool, 1)
	trust.Validate(security.TrustConfigValidateArgs{
		Data:       rogueParsed,
		DataSigCov: rogueSigCov,
		Fetch: func(name enc.Name, cfg *ndn.InterestConfig, cb ndn.ExpressCallbackFunc) {
			cb(ndn.ExpressCallbackArgs{Result: ndn.InterestResultTimeout})
		},
		Callback: func(valid bool, err error) {
			done <- valid
		},
	})
	require.False(t, <-done)
}

func TestKDKInStore(t *testing.T) {
	tu.SetT(t)

	ca, caCertWire, caCert := makeCA(t)
	_, _, cert := makeUserCert(t, ca, "/test/bob")

	e := dummyEngine(t)
	ks := makeKeyServer(t, e, caCertWire, caCert)
	require.NoError(t, ks.produceKEK())

	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	kn := keyName(cert)
	ks.AddMember(kn.String(), priv.PublicKey())

	// kek should be 32 bytes (x25519 pub)
	kekN := tu.NoErr(enc.NameFromStr(KEKName("/test", "docs", ks.am.KEK().ID)))
	raw, err := ks.store.Get(kekN, false)
	require.NoError(t, err)
	require.NotNil(t, raw)
	d, _, _ := spec.Spec{}.ReadData(enc.NewBufferView(raw))
	require.Equal(t, 32, len(d.Content().Join()))

	// kdk should have content-type key
	kdkN := tu.NoErr(enc.NameFromStr(EncryptedKDKName("/test", "docs", ks.am.KDKID(), kn.String())))
	raw, err = ks.store.Get(kdkN, false)
	require.NoError(t, err)
	require.NotNil(t, raw)
	d, _, _ = spec.Spec{}.ReadData(enc.NewBufferView(raw))
	require.Equal(t, ndn.ContentTypeKey, d.ContentType().Unwrap())
}

func TestPutCatRoundTrip(t *testing.T) {
	tu.SetT(t)

	ca, caCertWire, caCert := makeCA(t)
	_, _, cert := makeUserCert(t, ca, "/test/user1")

	e := dummyEngine(t)
	ks := makeKeyServer(t, e, caCertWire, caCert)
	require.NoError(t, ks.produceKEK())

	priv, _ := ecdh.X25519().GenerateKey(rand.Reader)
	kn := keyName(cert)
	ks.AddMember(kn.String(), priv.PublicKey())

	am := ks.AccessManager()
	nacSigner := ks.signer

	// producer encrypts + publishes to a store
	plain := []byte("secret sensor data")
	encC, encCK, err := NewEncryptor("/test/s", "/test", "docs", am.KEK()).Encrypt("/test/s/t", plain)
	require.NoError(t, err)

	store := storage.NewMemoryStore()
	cN := tu.NoErr(enc.NameFromStr("/test/s/t"))
	ckN := tu.NoErr(enc.NameFromStr("/test/s/t/CK"))

	d1 := tu.NoErr(spec.Spec{}.MakeData(cN, &ndn.DataConfig{Freshness: optional.Some(time.Hour)},
		enc.Wire{encC.EncryptedPayload}, nacSigner))
	store.Put(cN, d1.Wire.Join())

	d2 := tu.NoErr(spec.Spec{}.MakeData(ckN, &ndn.DataConfig{Freshness: optional.Some(time.Hour)},
		enc.Wire{encCK.EncryptedPayload}, nacSigner))
	store.Put(ckN, d2.Wire.Join())

	// consumer fetches and decrypts
	rawC, _ := store.Get(cN, false)
	pC, _, _ := spec.Spec{}.ReadData(enc.NewBufferView(rawC))

	rawCK, _ := store.Get(ckN, false)
	pCK, _, _ := spec.Spec{}.ReadData(enc.NewBufferView(rawCK))

	kdkN := tu.NoErr(enc.NameFromStr(EncryptedKDKName("/test", "docs", am.KDKID(), kn.String())))
	rawKDK, _ := ks.store.Get(kdkN, false)
	pKDK, _, _ := spec.Spec{}.ReadData(enc.NewBufferView(rawKDK))

	got, err := NewDecryptor(kn.String(), priv).Decrypt(
		&EncryptedContent{EncryptedPayload: pC.Content().Join()},
		&EncryptedCK{EncryptedPayload: pCK.Content().Join()},
		pKDK.Content().Join(),
	)
	require.NoError(t, err)
	require.Equal(t, plain, got)
}
