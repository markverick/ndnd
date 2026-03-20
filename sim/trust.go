package sim

import (
	"fmt"
	"sync"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
	spec "github.com/named-data/ndnd/std/ndn/spec_2022"
	"github.com/named-data/ndnd/std/object/storage"
	sec "github.com/named-data/ndnd/std/security"
	"github.com/named-data/ndnd/std/security/keychain"
	sig "github.com/named-data/ndnd/std/security/signer"
)

// SimTrust holds the shared trust root for all simulated nodes.
// One root key is generated per network prefix per simulation run.
type SimTrust struct {
	network    enc.Name
	rootSigner ndn.Signer
	rootCert   []byte // wire-encoded certificate
	rootName   string // full certificate name as string (for TrustAnchors)
}

var (
	globalTrust     *SimTrust
	globalTrustOnce sync.Once
	globalTrustErr  error
)

// GetSimTrust returns the global trust root, creating it on first call.
func GetSimTrust(network string) (*SimTrust, error) {
	globalTrustOnce.Do(func() {
		globalTrust, globalTrustErr = newSimTrust(network)
	})
	return globalTrust, globalTrustErr
}

// ResetSimTrust clears the global trust root (for testing).
func ResetSimTrust() {
	globalTrust = nil
	globalTrustErr = nil
	globalTrustOnce = sync.Once{}
}

func newSimTrust(network string) (*SimTrust, error) {
	networkName, err := enc.NameFromStr(network)
	if err != nil {
		return nil, fmt.Errorf("invalid network name: %w", err)
	}

	// Generate root Ed25519 key: /network/KEY/<random>
	rootKeyName := sec.MakeKeyName(networkName)
	rootSigner, err := sig.KeygenEd25519(rootKeyName)
	if err != nil {
		return nil, fmt.Errorf("failed to generate root key: %w", err)
	}

	// Self-sign the root certificate
	now := time.Now()
	rootCertWire, err := sec.SelfSign(sec.SignCertArgs{
		Signer:    rootSigner,
		IssuerId:  enc.NewGenericComponent("self"),
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.Add(10 * 365 * 24 * time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to self-sign root cert: %w", err)
	}

	// Parse the cert to get its name
	rootCertBytes := rootCertWire.Join()
	rootCertData, _, err := spec.Spec{}.ReadData(enc.NewWireView(rootCertWire))
	if err != nil {
		return nil, fmt.Errorf("failed to parse root cert: %w", err)
	}

	return &SimTrust{
		network:    networkName,
		rootSigner: rootSigner,
		rootCert:   rootCertBytes,
		rootName:   rootCertData.Name().String(),
	}, nil
}

// NodeKeychain builds a per-node keychain with an Ed25519 key signed by the root.
// Returns (keychain, store, trustAnchorNames) ready for config injection.
func (st *SimTrust) NodeKeychain(router string) (ndn.KeyChain, ndn.Store, []string, error) {
	routerName, err := enc.NameFromStr(router)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid router name: %w", err)
	}

	// Node identity: /network/node/32=DV (matches trust schema: #router/"32=DV"/#KEY)
	nodeIdentity := routerName.Append(enc.NewKeywordComponent("DV"))
	nodeKeyName := sec.MakeKeyName(nodeIdentity)

	nodeSigner, err := sig.KeygenEd25519(nodeKeyName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to generate node key: %w", err)
	}

	// Get the node's public key as a Data packet (for SignCert)
	nodeKeyData, err := sig.MarshalSecretToData(nodeSigner)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to marshal node key: %w", err)
	}

	// Sign node cert with root key
	now := time.Now()
	nodeCertWire, err := sec.SignCert(sec.SignCertArgs{
		Signer:    st.rootSigner,
		Data:      nodeKeyData,
		IssuerId:  enc.NewGenericComponent("NA"),
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.Add(10 * 365 * 24 * time.Hour),
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to sign node cert: %w", err)
	}

	// Build keychain
	store := storage.NewMemoryStore()
	kc := keychain.NewKeyChainMem(store)

	// Insert root cert
	if err := kc.InsertCert(st.rootCert); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to insert root cert: %w", err)
	}

	// Insert node key + cert
	if err := kc.InsertKey(nodeSigner); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to insert node key: %w", err)
	}
	if err := kc.InsertCert(nodeCertWire.Join()); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to insert node cert: %w", err)
	}

	return kc, store, []string{st.rootName}, nil
}
