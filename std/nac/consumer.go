package nac

import (
	"crypto/ecdh"
	"fmt"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/named-data/ndnd/std/ndn"
	sec "github.com/named-data/ndnd/std/security"
	"github.com/named-data/ndnd/std/types/optional"
	"github.com/named-data/ndnd/std/utils"
)

// Consumer fetches NAC keys over NDN and decrypts content.
type Consumer struct {
	engine          ndn.Engine
	consumerKeyName string
	privateKey      *ecdh.PrivateKey
	decryptor       *Decryptor
	trust           *sec.TrustConfig
}

// NewConsumer creates a NAC consumer that can fetch keys and decrypt content.
// trust may be nil to skip validation (not recommended).
func NewConsumer(engine ndn.Engine, consumerKeyName string, privateKey *ecdh.PrivateKey, trust *sec.TrustConfig) *Consumer {
	return &Consumer{
		engine:          engine,
		consumerKeyName: consumerKeyName,
		privateKey:      privateKey,
		decryptor:       NewDecryptor(consumerKeyName, privateKey),
		trust:           trust,
	}
}

// validates received data packet against the trust config
func (c *Consumer) validateData(data ndn.Data, sigCovered enc.Wire) error {
	if c.trust == nil {
		return nil
	}
	ch := make(chan error, 1)
	c.trust.Validate(sec.TrustConfigValidateArgs{
		Data:       data,
		DataSigCov: sigCovered,
		Fetch: func(name enc.Name, cfg *ndn.InterestConfig, cb ndn.ExpressCallbackFunc) {
			interest, err := c.engine.Spec().MakeInterest(name, cfg, nil, nil)
			if err != nil {
				cb(ndn.ExpressCallbackArgs{Result: ndn.InterestResultTimeout})
				return
			}
			c.engine.Express(interest, cb)
		},
		Callback: func(valid bool, err error) {
			if !valid {
				if err != nil {
					ch <- fmt.Errorf("validation failed: %w", err)
				} else {
					ch <- fmt.Errorf("validation failed")
				}
			} else {
				ch <- nil
			}
		},
	})
	return <-ch
}

// FetchKEK fetches the public Key Encryption Key from the network.
// kekName: <access-prefix>/NAC/<dataset>/KEK/<key-id>
func (c *Consumer) FetchKEK(kekName string) (*KeyEncryptionKey, error) {
	name, err := enc.NameFromStr(kekName)
	if err != nil {
		return nil, fmt.Errorf("invalid KEK name: %w", err)
	}

	ch := make(chan ndn.ExpressCallbackArgs, 1)
	interest, err := c.engine.Spec().MakeInterest(name, &ndn.InterestConfig{
		Lifetime: optional.Some(4 * time.Second),
		Nonce:    utils.ConvertNonce(c.engine.Timer().Nonce()),
	}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to make KEK interest: %w", err)
	}

	c.engine.Express(interest, func(args ndn.ExpressCallbackArgs) { ch <- args })
	args := <-ch

	if args.Result != ndn.InterestResultData {
		return nil, fmt.Errorf("failed to fetch KEK: %s", args.Result)
	}

	if err := c.validateData(args.Data, args.SigCovered); err != nil {
		return nil, fmt.Errorf("KEK %s: %w", kekName, err)
	}

	pubKeyBytes := args.Data.Content().Join()
	pubKey, err := DeserializePublicKey(pubKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid KEK public key: %w", err)
	}

	return &KeyEncryptionKey{
		PublicKey: pubKey,
	}, nil
}

// FetchEncryptedKDK fetches the encrypted KDK for this consumer from the network.
// kdkForName: <access-prefix>/NAC/<dataset>/KDK/<key-id>/ENCRYPTED-BY/<consumer-key-name>
func (c *Consumer) FetchEncryptedKDK(kdkForName string) ([]byte, error) {
	name, err := enc.NameFromStr(kdkForName)
	if err != nil {
		return nil, fmt.Errorf("invalid KDK name: %w", err)
	}

	ch := make(chan ndn.ExpressCallbackArgs, 1)
	interest, err := c.engine.Spec().MakeInterest(name, &ndn.InterestConfig{
		Lifetime: optional.Some(4 * time.Second),
		Nonce:    utils.ConvertNonce(c.engine.Timer().Nonce()),
	}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to make KDK interest: %w", err)
	}

	c.engine.Express(interest, func(args ndn.ExpressCallbackArgs) { ch <- args })
	args := <-ch

	if args.Result != ndn.InterestResultData {
		return nil, fmt.Errorf("failed to fetch encrypted KDK: %s", args.Result)
	}

	if err := c.validateData(args.Data, args.SigCovered); err != nil {
		return nil, fmt.Errorf("KDK %s: %w", kdkForName, err)
	}

	return args.Data.Content().Join(), nil
}

// Decrypt decrypts content using the full NAC chain, fetching keys from network as needed.
func (c *Consumer) Decrypt(
	encContent *EncryptedContent,
	encCK *EncryptedCK,
	encKDKBlob []byte,
) ([]byte, error) {
	return c.decryptor.Decrypt(encContent, encCK, encKDKBlob)
}
