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

	// Cached node credentials for cert pre-population.
	// In real NDN deployments certificates are distributed out-of-band;
	// pre-populating them in simulation avoids timing-dependent cert
	// fetch failures that cause non-deterministic DV convergence.
	mu        sync.Mutex
	nodeCerts map[string][]byte      // router name → wire-encoded certificate
	nodeKeys  map[string]ndn.Signer  // router name → Ed25519 signer
	keychains []registeredKeychain
}

type registeredKeychain struct {
	router string
	kc     ndn.KeyChain
}

var (
	globalTrustMu   sync.Mutex
	globalTrust     *SimTrust
	globalTrustInit bool
	globalTrustErr  error
)

// GetSimTrust returns the global trust root, creating it on first call.
func GetSimTrust(network string) (*SimTrust, error) {
	globalTrustMu.Lock()
	defer globalTrustMu.Unlock()
	if !globalTrustInit {
		globalTrust, globalTrustErr = newSimTrust(network)
		globalTrustInit = true
	}
	return globalTrust, globalTrustErr
}

// ResetSimTrust clears the global trust root (for testing).
// Unlike assigning a new sync.Once (which is a data race on the value),
// this uses the same mutex that guards GetSimTrust.
func ResetSimTrust() {
	globalTrustMu.Lock()
	defer globalTrustMu.Unlock()
	globalTrust = nil
	globalTrustErr = nil
	globalTrustInit = false
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
		nodeCerts:  make(map[string][]byte),
		nodeKeys:   make(map[string]ndn.Signer),
		keychains:  make([]registeredKeychain, 0),
	}, nil
}

func (st *SimTrust) generateNodeCreds(router string) (ndn.Signer, []byte, error) {
	routerName, err := enc.NameFromStr(router)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid router name %q: %w", router, err)
	}

	nodeIdentity := routerName.Append(enc.NewKeywordComponent("DV"))
	nodeKeyName := sec.MakeKeyName(nodeIdentity)
	nodeSigner, err := sig.KeygenEd25519(nodeKeyName)
	if err != nil {
		return nil, nil, fmt.Errorf("keygen for %s: %w", router, err)
	}

	nodeKeyData, err := sig.MarshalSecretToData(nodeSigner)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal key for %s: %w", router, err)
	}

	now := time.Now()
	nodeCertWire, err := sec.SignCert(sec.SignCertArgs{
		Signer:    st.rootSigner,
		Data:      nodeKeyData,
		IssuerId:  enc.NewGenericComponent("NA"),
		NotBefore: now.Add(-time.Hour),
		NotAfter:  now.Add(10 * 365 * 24 * time.Hour),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("sign cert for %s: %w", router, err)
	}

	return nodeSigner, nodeCertWire.Join(), nil
}

func (st *SimTrust) distributePeerCert(router string, cert []byte) error {
	for _, reg := range st.keychains {
		if reg.router == router {
			continue
		}
		if err := reg.kc.InsertCert(cert); err != nil {
			return fmt.Errorf("failed to insert peer cert for %s into %s: %w", router, reg.router, err)
		}
	}
	return nil
}

// PreGenerateCerts generates and caches Ed25519 keys and certificates for
// the given routers. Subsequent calls to NodeKeychain for these routers
// will reuse the cached credentials and include all peer certificates,
// eliminating the need for network certificate fetches.
func (st *SimTrust) PreGenerateCerts(routers []string) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	for _, router := range routers {
		if _, ok := st.nodeCerts[router]; ok {
			continue // already generated
		}

		nodeSigner, nodeCertBytes, err := st.generateNodeCreds(router)
		if err != nil {
			return err
		}

		st.nodeKeys[router] = nodeSigner
		st.nodeCerts[router] = nodeCertBytes
		if err := st.distributePeerCert(router, nodeCertBytes); err != nil {
			return err
		}
	}
	return nil
}

// NodeKeychain builds a per-node keychain with an Ed25519 key signed by the root.
// If PreGenerateCerts was called, the cached key is reused and all peer
// certificates are included in the keychain (eliminating network cert fetches).
// Returns (keychain, store, trustAnchorNames) ready for config injection.
func (st *SimTrust) NodeKeychain(router string) (ndn.KeyChain, ndn.Store, []string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	var nodeSigner ndn.Signer
	var nodeCertBytes []byte
	var err error

	if signer, ok := st.nodeKeys[router]; ok {
		// Use pre-generated credentials
		nodeSigner = signer
		nodeCertBytes = st.nodeCerts[router]
	} else {
		// Generate on-the-fly when PreGenerateCerts was not called.
		nodeSigner, nodeCertBytes, err = st.generateNodeCreds(router)
		if err != nil {
			return nil, nil, nil, err
		}
		st.nodeKeys[router] = nodeSigner
		st.nodeCerts[router] = nodeCertBytes
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
	if err := kc.InsertCert(nodeCertBytes); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to insert node cert: %w", err)
	}

	// Insert all peer certificates (pre-populated trust).
	// This mirrors real NDN deployments where certificates are distributed
	// out-of-band, avoiding timing-dependent network cert fetch failures.
	for peerRouter, peerCert := range st.nodeCerts {
		if peerRouter == router {
			continue
		}
		if err := kc.InsertCert(peerCert); err != nil {
			return nil, nil, nil, fmt.Errorf("failed to insert peer cert for %s: %w", peerRouter, err)
		}
	}

	st.keychains = append(st.keychains, registeredKeychain{router: router, kc: kc})
	if err := st.distributePeerCert(router, nodeCertBytes); err != nil {
		return nil, nil, nil, err
	}

	return kc, store, []string{st.rootName}, nil
}
