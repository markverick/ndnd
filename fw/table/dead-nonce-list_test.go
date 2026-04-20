package table

import (
	"testing"
	"time"

	"github.com/named-data/ndnd/fw/core"
	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/stretchr/testify/assert"
)

// TestDeadNonceList_InsertAndFind verifies basic insert and lookup.
func TestDeadNonceList_InsertAndFind(t *testing.T) {
	d := NewDeadNonceList()
	defer d.Ticker.Stop()
	name, _ := enc.NameFromStr("/test/name")

	assert.False(t, d.Find(name, 1234), "nonce not yet inserted")
	alreadyPresent := d.Insert(name, 1234)
	assert.False(t, alreadyPresent, "first insert returns false")
	assert.True(t, d.Find(name, 1234), "nonce must be found after insert")
}

// TestDeadNonceList_InsertDuplicate verifies that inserting the same nonce
// twice returns true on the second call and does not change state.
func TestDeadNonceList_InsertDuplicate(t *testing.T) {
	d := NewDeadNonceList()
	defer d.Ticker.Stop()
	name, _ := enc.NameFromStr("/dup/test")

	d.Insert(name, 42)
	alreadyPresent := d.Insert(name, 42)
	assert.True(t, alreadyPresent, "duplicate insert must return true")
	assert.True(t, d.Find(name, 42))
}

// TestDeadNonceList_DifferentNonces verifies that two different nonces for the
// same name are stored and retrieved independently.
func TestDeadNonceList_DifferentNonces(t *testing.T) {
	d := NewDeadNonceList()
	defer d.Ticker.Stop()
	name, _ := enc.NameFromStr("/nonce/isolation")

	d.Insert(name, 1)
	d.Insert(name, 2)

	assert.True(t, d.Find(name, 1))
	assert.True(t, d.Find(name, 2))
	assert.False(t, d.Find(name, 3))
}

// TestDeadNonceList_DifferentNames verifies that the same nonce value for two
// different names produces independent entries.
func TestDeadNonceList_DifferentNames(t *testing.T) {
	d := NewDeadNonceList()
	defer d.Ticker.Stop()
	a, _ := enc.NameFromStr("/name/a")
	b, _ := enc.NameFromStr("/name/b")

	d.Insert(a, 7)
	assert.True(t, d.Find(a, 7))
	assert.False(t, d.Find(b, 7), "nonce for /name/a must not match /name/b")
}

// TestDeadNonceList_RemoveExpiredEntries verifies that entries whose lifetime
// has elapsed are removed by RemoveExpiredEntries, while fresh entries survive.
func TestDeadNonceList_RemoveExpiredEntries(t *testing.T) {
	// Freeze time so we control expiry.
	now := time.Unix(1_000_000, 0)
	core.NowFunc = func() time.Time { return now }
	defer func() { core.NowFunc = time.Now }()

	d := NewDeadNonceList()
	defer d.Ticker.Stop()
	name, _ := enc.NameFromStr("/expiry/test")

	// Insert at t=0 (relative).  Default lifetime is 6 000 ms.
	d.Insert(name, 99)
	assert.True(t, d.Find(name, 99))

	// Advance time by 7 seconds — entry is now expired.
	now = now.Add(7 * time.Second)
	d.RemoveExpiredEntries()
	assert.False(t, d.Find(name, 99), "expired entry must be removed")
}

// TestDeadNonceList_FreshEntryNotExpired verifies that an entry inserted just
// before expiry is NOT removed when only partially elapsed.
func TestDeadNonceList_FreshEntryNotExpired(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	core.NowFunc = func() time.Time { return now }
	defer func() { core.NowFunc = time.Now }()

	d := NewDeadNonceList()
	defer d.Ticker.Stop()
	name, _ := enc.NameFromStr("/fresh/test")

	d.Insert(name, 55)

	// Only 1 second has passed — still within the 6 s lifetime.
	now = now.Add(1 * time.Second)
	d.RemoveExpiredEntries()
	assert.True(t, d.Find(name, 55), "fresh entry must not be evicted early")
}

// TestDeadNonceList_EvictionCap verifies that RemoveExpiredEntries removes at
// most 100 entries per call, leaving the remaining expired entries in place
// until the next call.
func TestDeadNonceList_EvictionCap(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	core.NowFunc = func() time.Time { return now }
	defer func() { core.NowFunc = time.Now }()

	d := NewDeadNonceList()
	defer d.Ticker.Stop()
	base, _ := enc.NameFromStr("/cap/test")

	// Insert 150 entries, all with distinct names to avoid hash collisions.
	const total = 150
	for i := uint32(0); i < total; i++ {
		d.Insert(base, i)
	}

	// Advance past the 6 s lifetime so all entries expire.
	now = now.Add(7 * time.Second)

	// First call: should remove exactly 100, leaving 50.
	d.RemoveExpiredEntries()

	remaining := 0
	for i := uint32(0); i < total; i++ {
		if d.Find(base, i) {
			remaining++
		}
	}
	assert.Equal(t, total-100, remaining, "first RemoveExpiredEntries call must evict at most 100 entries")

	// Second call: should drain the rest.
	d.RemoveExpiredEntries()
	remaining = 0
	for i := uint32(0); i < total; i++ {
		if d.Find(base, i) {
			remaining++
		}
	}
	assert.Equal(t, 0, remaining, "second call must remove the remaining entries")
}
