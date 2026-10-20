package nac

import (
	"crypto/ecdh"
	"encoding/hex"
	"fmt"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
	spec "github.com/named-data/ndnd/std/ndn/spec_2022"
	"github.com/named-data/ndnd/std/object/storage"
	sec "github.com/named-data/ndnd/std/security"
	"github.com/named-data/ndnd/std/types/optional"
)

// key server for NAC - pre-produces signed KEK/KDK data packets AND serve them from db
//
//	KEK at <access-prefix>/NAC/<dataset>/KEK/<key-id>
//	KDK at <access-prefix>/NAC/<dataset>/KDK/<key-id>/ENCRYPTED-BY/<member>
//	ENROLL at <access-prefix>/NAC/<dataset>/ENROLL
type KeyServer struct {
	engine ndn.Engine
	signer ndn.Signer
	am     *AccessManager
	store  ndn.Store

	nacPrefix enc.Name // <access-prefix>/NAC/<dataset>

	// NOTE: when nil all KPs are ephemeral
	persist Store
	// trust config for validating enrollment certificates
	trust *sec.TrustConfig
}

// creates a NAC key server for <accessPrefix>/NAC/<dataset>
func NewKeyServer(engine ndn.Engine, signer ndn.Signer, accessPrefix, dataset string, trust *sec.TrustConfig) (*KeyServer, error) {
	am, err := NewAccessManager(accessPrefix, dataset)
	if err != nil {
		return nil, fmt.Errorf("failed to create access manager: %w", err)
	}

	nacPrefixStr := accessPrefix + "/NAC/" + dataset
	prefix, err := enc.NameFromStr(nacPrefixStr)
	if err != nil {
		return nil, fmt.Errorf("invalid NAC prefix: %w", err)
	}

	return &KeyServer{
		engine:    engine,
		signer:    signer,
		am:        am,
		store:     storage.NewMemoryStore(),
		nacPrefix: prefix,
		trust:     trust,
	}, nil
}

func NewPersistentKeyServer(engine ndn.Engine, signer ndn.Signer, accessPrefix, dataset string, store Store, trust *sec.TrustConfig) (*KeyServer, error) {
	nacPrefixStr := accessPrefix + "/NAC/" + dataset
	prefix, err := enc.NameFromStr(nacPrefixStr)
	if err != nil {
		return nil, fmt.Errorf("invalid NAC prefix: %w", err)
	}

	var am *AccessManager
	if store.HasKeyPair() {
		kek, kdk, err := store.LoadKeyPair()
		if err != nil {
			return nil, fmt.Errorf("failed to load key pair: %w", err)
		}
		members, err := store.LoadMembers()
		if err != nil {
			return nil, fmt.Errorf("failed to load members: %w", err)
		}
		am = NewAccessManagerFromState(accessPrefix, dataset, kek, kdk, members)
		fmt.Printf("NAC KeyServer: restored %d enrolled members\n", len(members))
	} else {
		am, err = NewAccessManager(accessPrefix, dataset)
		if err != nil {
			return nil, fmt.Errorf("failed to create access manager: %w", err)
		}
		if err := store.SaveKeyPair(am.KEK(), am.kdk); err != nil {
			return nil, fmt.Errorf("failed to persist key pair: %w", err)
		}
		fmt.Println("NAC KeyServer: generated and persisted new key pair")
	}

	return &KeyServer{
		engine:    engine,
		signer:    signer,
		am:        am,
		store:     store.PacketStore(),
		nacPrefix: prefix,
		persist:   store,
		trust:     trust,
	}, nil
}

// returns the underlying access manager
func (ks *KeyServer) AccessManager() *AccessManager {
	return ks.am
}

// pre-produce KEK, register routes, attach handlers
func (ks *KeyServer) Start() error {
	if err := ks.produceKEK(); err != nil {
		return fmt.Errorf("failed to produce KEK: %w", err)
	}

	// TODO: should we be re-signing KDK packets for any previously enrolled members??
	for name := range ks.am.members {
		if err := ks.produceKDK(name); err != nil {
			return fmt.Errorf("failed to re-produce KDK for %s: %w", name, err)
		}
	}
	if err := ks.engine.RegisterRoute(ks.nacPrefix); err != nil {
		return fmt.Errorf("failed to register route: %w", err)
	}

	// one handler serves everything from the store
	if err := ks.engine.AttachHandler(ks.nacPrefix, ks.onFetch); err != nil {
		return fmt.Errorf("failed to attach fetch handler: %w", err)
	}

	// enroll is dynamic - produces new KDK packets on demand
	enrollPrefix := ks.nacPrefix.Append(enc.NewGenericComponent("ENROLL"))
	if err := ks.engine.AttachHandler(enrollPrefix, ks.onEnrollInterest); err != nil {
		return fmt.Errorf("failed to attach ENROLL handler: %w", err)
	}

	fmt.Printf("NAC KeyServer started at %s\n", ks.nacPrefix)
	return nil
}

func (ks *KeyServer) Stop() error {
	ks.engine.DetachHandler(ks.nacPrefix)
	ks.engine.DetachHandler(ks.nacPrefix.Append(enc.NewGenericComponent("ENROLL")))
	return nil
}

// sign and store the KEK data packet
func (ks *KeyServer) produceKEK() error {
	kek := ks.am.KEK()
	kekNameStr := KEKName(ks.am.AccessPrefix(), ks.am.Dataset(), kek.ID)
	kekName, err := enc.NameFromStr(kekNameStr)
	if err != nil {
		return fmt.Errorf("invalid KEK name: %w", err)
	}

	data, err := ks.engine.Spec().MakeData(kekName, &ndn.DataConfig{
		ContentType: optional.Some(ndn.ContentTypeKey),
		Freshness:   optional.Some(time.Hour),
	}, enc.Wire{kek.PublicKey.Bytes()}, ks.signer)
	if err != nil {
		return fmt.Errorf("failed to sign KEK: %w", err)
	}

	return ks.store.Put(kekName, data.Wire.Join())
}

func (ks *KeyServer) produceKDK(consumerKeyName string) error {
	encKDK, ok := ks.am.GetEncryptedKDK(consumerKeyName)
	if !ok {
		return fmt.Errorf("consumer not authorized: %s", consumerKeyName)
	}

	kdkNameStr := EncryptedKDKName(
		ks.am.AccessPrefix(), ks.am.Dataset(), ks.am.KDKID(), consumerKeyName,
	)
	kdkName, err := enc.NameFromStr(kdkNameStr)
	if err != nil {
		return fmt.Errorf("invalid KDK name: %w", err)
	}

	data, err := ks.engine.Spec().MakeData(kdkName, &ndn.DataConfig{
		ContentType: optional.Some(ndn.ContentTypeKey),
		Freshness:   optional.Some(time.Hour),
	}, enc.Wire{encKDK}, ks.signer)
	if err != nil {
		return fmt.Errorf("failed to sign KDK: %w", err)
	}

	return ks.store.Put(kdkName, data.Wire.Join())
}

// AddMember authorizes a consumer and produces their signed KDK Data packet.
func (ks *KeyServer) AddMember(consumerKeyName string, consumerPubKey *ecdh.PublicKey) error {
	if err := ks.am.AddMember(consumerKeyName, consumerPubKey); err != nil {
		return err
	}
	normalized := normalizeKeyName(consumerKeyName)
	if ks.persist != nil {
		encKDK, _ := ks.am.GetEncryptedKDK(normalized)
		if err := ks.persist.SaveMember(normalized, encKDK); err != nil {
			return fmt.Errorf("failed to persist member %s: %w", normalized, err)
		}
	}
	return ks.produceKDK(normalized)
}

// serve pre-produced packets from the store
func (ks *KeyServer) onFetch(args ndn.InterestHandlerArgs) {
	data, err := ks.store.Get(args.Interest.Name(), args.Interest.CanBePrefix())
	if err != nil || data == nil {
		return
	}
	args.Reply(enc.Wire{data})
}

// handle enrollment - client sends [32B x25519 pubkey][cert wire] to ENROLL
// server validates cert against trust config, authorizes consumer, produces KDK packet
func (ks *KeyServer) onEnrollInterest(args ndn.InterestHandlerArgs) {
	fmt.Printf("NAC ENROLL: handler invoked for %s\n", args.Interest.Name())
	appParam := args.Interest.AppParam()
	if appParam == nil {
		fmt.Println("NAC ENROLL: missing AppParam")
		ks.replyEnrollError(args, "missing AppParam")
		return
	}

	payload := appParam.Join()

	// [32-byte x25519 pubkey][certificate wire]
	if len(payload) < 33 {
		fmt.Println("NAC ENROLL: AppParam too short")
		ks.replyEnrollError(args, "AppParam too short")
		return
	}

	x25519PubBytes := payload[:32]
	certWireBytes := payload[32:]

	// parse x25519 pubkey
	consumerPubKey, err := DeserializePublicKey(x25519PubBytes)
	if err != nil {
		fmt.Printf("NAC ENROLL: invalid X25519 key: %v\n", err)
		ks.replyEnrollError(args, "invalid X25519 public key")
		return
	}

	// parse client cert
	clientCert, sigCovered, err := spec.Spec{}.ReadData(enc.NewBufferView(certWireBytes))
	if err != nil {
		fmt.Printf("NAC ENROLL: failed to parse certificate: %v\n", err)
		ks.replyEnrollError(args, "invalid certificate")
		return
	}

	fmt.Printf("NAC ENROLL: request from %s\n", clientCert.Name())

	if ks.trust == nil {
		fmt.Println("NAC ENROLL: no trust config — rejecting")
		ks.replyEnrollError(args, "server has no trust config")
		return
	}

	// validate cert against trust config (chains to trust anchor)
	// fetch is no-op: the CA cert anchor is already in the keychain
	ks.trust.Validate(sec.TrustConfigValidateArgs{
		Data:       clientCert,
		DataSigCov: sigCovered,
		Fetch: func(name enc.Name, cfg *ndn.InterestConfig, cb ndn.ExpressCallbackFunc) {
			cb(ndn.ExpressCallbackArgs{Result: ndn.InterestResultTimeout})
		},
		Callback: func(valid bool, err error) {
			if !valid {
				fmt.Printf("NAC ENROLL: trust validation failed: %v\n", err)
				ks.replyEnrollError(args, "certificate trust validation failed")
				return
			}
			ks.completeEnrollment(args, clientCert, consumerPubKey, x25519PubBytes)
		},
	})
}

// completeEnrollment finishes enrollment after trust validation succeeds.
func (ks *KeyServer) completeEnrollment(args ndn.InterestHandlerArgs, clientCert ndn.Data, consumerPubKey *ecdh.PublicKey, x25519PubBytes []byte) {
	// derive consumer key name from cert (strip issuer + version suffix)
	consumerKeyName := clientCert.Name()
	if len(consumerKeyName) >= 2 {
		consumerKeyName = consumerKeyName[:len(consumerKeyName)-2]
	}

	// authorize and produce KDK data packet
	if err := ks.AddMember(consumerKeyName.String(), consumerPubKey); err != nil {
		fmt.Printf("NAC ENROLL: failed to add member: %v\n", err)
		ks.replyEnrollError(args, "enrollment failed")
		return
	}

	fmt.Printf("NAC ENROLL: SUCCESS - enrolled %s (X25519 pub: %s)\n",
		consumerKeyName, hex.EncodeToString(x25519PubBytes))

	// reply with KEK pub + ID so client can encrypt
	kek := ks.am.KEK()
	kekPubBytes := kek.PublicKey.Bytes()
	kekIDBytes := kek.ID
	// "OK:" + KEK pub (32 bytes) + KEK ID (16 bytes)
	response := append([]byte("OK:"), kekPubBytes...)
	response = append(response, kekIDBytes...)

	cfg := &ndn.DataConfig{
		Freshness: optional.Some(4 * time.Second),
	}
	data, err := ks.engine.Spec().MakeData(
		args.Interest.Name(),
		cfg,
		enc.Wire{response},
		ks.signer,
	)
	if err != nil {
		fmt.Printf("NAC ENROLL: failed to create response: %v\n", err)
		return
	}
	args.Reply(data.Wire)
}

func (ks *KeyServer) replyEnrollError(args ndn.InterestHandlerArgs, msg string) {
	cfg := &ndn.DataConfig{
		Freshness: optional.Some(4 * time.Second),
	}
	data, err := ks.engine.Spec().MakeData(
		args.Interest.Name(),
		cfg,
		enc.Wire{[]byte("ERR:" + msg)},
		ks.signer,
	)
	if err != nil {
		return
	}
	args.Reply(data.Wire)
}
