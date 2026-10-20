package nac

import (
	"fmt"
	"os"
	"strings"

	"github.com/dgraph-io/badger/v4"
	"github.com/named-data/ndnd/std/ndn"
	"github.com/named-data/ndnd/std/object/storage"
)

const (
	keyPairPrefix = "nac/keypair"
	memberPrefix  = "nac/member/"
)

type BadgerNacStore struct {
	db     *badger.DB
	pktStr ndn.Store
}

func NewBadgerNacStore(dir string) (*BadgerNacStore, error) {
	metaDir := dir + "/meta"
	pktDir := dir + "/packets"
	for _, d := range []string{metaDir, pktDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	opts := badger.DefaultOptions(metaDir).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open meta db: %w", err)
	}

	pktStore, err := storage.NewBadgerStore(pktDir)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open packet store: %w", err)
	}

	return &BadgerNacStore{db: db, pktStr: pktStore}, nil
}

func (s *BadgerNacStore) Close() error {
	s.pktStr.(*storage.BadgerStore).Close()
	return s.db.Close()
}

func (s *BadgerNacStore) PacketStore() ndn.Store {
	return s.pktStr
}

func (s *BadgerNacStore) HasKeyPair() bool {
	var found bool
	s.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get([]byte(keyPairPrefix))
		found = err == nil
		return nil
	})
	return found
}

// looks like [16B id][32B private key]
func (s *BadgerNacStore) SaveKeyPair(kek *KeyEncryptionKey, kdk *KeyDecryptionKey) error {
	val := make([]byte, 0, 48)
	val = append(val, kdk.ID...)
	val = append(val, kdk.PrivateKey.Bytes()...)
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(keyPairPrefix), val)
	})
}

func (s *BadgerNacStore) LoadKeyPair() (*KeyEncryptionKey, *KeyDecryptionKey, error) {
	var val []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get([]byte(keyPairPrefix))
		if err != nil {
			return err
		}
		val, err = item.ValueCopy(nil)
		return err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("load keypair: %w", err)
	}
	if len(val) != 48 {
		return nil, nil, fmt.Errorf("corrupt keypair: expected 48 bytes, got %d", len(val))
	}

	id := val[:16]
	priv, err := DeserializePrivateKey(val[16:48])
	if err != nil {
		return nil, nil, fmt.Errorf("deserialize private key: %w", err)
	}

	kek := &KeyEncryptionKey{ID: id, PublicKey: priv.PublicKey()}
	kdk := &KeyDecryptionKey{ID: id, PrivateKey: priv}
	return kek, kdk, nil
}

func (s *BadgerNacStore) SaveMember(consumerKeyName string, encKDK []byte) error {
	key := memberPrefix + consumerKeyName
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(key), encKDK)
	})
}

func (s *BadgerNacStore) LoadMembers() (map[string][]byte, error) {
	members := make(map[string][]byte)
	err := s.db.View(func(txn *badger.Txn) error {
		prefix := []byte(memberPrefix)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.Valid(); it.Next() {
			item := it.Item()
			name := strings.TrimPrefix(string(item.Key()), memberPrefix)
			val, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			members[name] = val
		}
		return nil
	})
	return members, err
}
