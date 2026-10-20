package nac

import "github.com/named-data/ndnd/std/ndn"

// Store persists NAC key server state across restarts.
type Store interface {
	HasKeyPair() bool
	LoadKeyPair() (*KeyEncryptionKey, *KeyDecryptionKey, error)
	SaveKeyPair(kek *KeyEncryptionKey, kdk *KeyDecryptionKey) error

	LoadMembers() (map[string][]byte, error)
	SaveMember(consumerKeyName string, encKDK []byte) error

	// packet store for pre-signed KEK/KDK data packets
	PacketStore() ndn.Store

	Close() error
}
