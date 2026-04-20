package table

import (
	"testing"

	enc "github.com/named-data/ndnd/std/encoding"
	"github.com/stretchr/testify/assert"
)

// TestNdnCleanUpFace_Tree verifies that CleanUpFace removes only the nexthop
// for the given face across all FIB entries and prunes now-empty tree nodes.
func TestNdnCleanUpFace_Tree(t *testing.T) {
	fib := NewFibStrategyTree()

	a, _ := enc.NameFromStr("/a")
	b, _ := enc.NameFromStr("/a/b")

	fib.InsertNextHopEnc(a, 10, 1)
	fib.InsertNextHopEnc(a, 20, 2)
	fib.InsertNextHopEnc(b, 10, 1)

	fib.CleanUpFace(10)

	// Face 10 removed from /a; face 20 still there.
	hopsA := fib.FindNextHopsEnc(a)
	assert.Equal(t, 1, len(hopsA))
	assert.Equal(t, uint64(20), hopsA[0].Nexthop)

	// /a/b had only face 10 — entry must be pruned.
	assert.Equal(t, 1, len(fib.GetAllFIBEntries()), "/a/b should be pruned; only /a remains")

	// Removing a non-existent face is a no-op.
	fib.CleanUpFace(99)
	assert.Equal(t, 1, len(fib.GetAllFIBEntries()))
}

// TestNdnCleanUpFace_Tree_AllRemoved verifies that when every nexthop of every
// entry belongs to the removed face, the tree is left empty.
func TestNdnCleanUpFace_Tree_AllRemoved(t *testing.T) {
	fib := NewFibStrategyTree()

	for _, s := range []string{"/x", "/x/y", "/x/y/z"} {
		name, _ := enc.NameFromStr(s)
		fib.InsertNextHopEnc(name, 5, 0)
	}
	assert.Equal(t, 3, len(fib.GetAllFIBEntries()))

	fib.CleanUpFace(5)
	assert.Equal(t, 0, len(fib.GetAllFIBEntries()), "all entries must be pruned after last face removed")
}

// TestNdnClearNextHops_PrunesNode verifies that ClearNextHopsEnc prunes the
// tree node when it becomes empty (no strategy, no children).  This is a
// regression test for the bug where ClearNextHopsEnc did not call pruneIfEmpty,
// leaking zombie tree nodes.
func TestNdnClearNextHops_PrunesNode(t *testing.T) {
	fib := NewFibStrategyTree()

	name, _ := enc.NameFromStr("/prune/me")
	fib.InsertNextHopEnc(name, 1, 0)
	assert.Equal(t, 1, len(fib.GetAllFIBEntries()))

	fib.ClearNextHopsEnc(name)
	assert.Equal(t, 0, len(fib.GetAllFIBEntries()), "tree node must be pruned after ClearNextHopsEnc empties it")
}

// TestNdnClearNextHops_KeepsNodeWithStrategy verifies that ClearNextHopsEnc
// does NOT prune a node that still has a strategy set.
func TestNdnClearNextHops_KeepsNodeWithStrategy(t *testing.T) {
	fib := NewFibStrategyTree()

	name, _ := enc.NameFromStr("/keep/strategy")
	strat, _ := enc.NameFromStr("/localhost/nfd/strategy/best-route/v=1")
	fib.InsertNextHopEnc(name, 1, 0)
	fib.SetStrategyEnc(name, strat)

	fib.ClearNextHopsEnc(name)

	// Strategy still there — node must not be pruned.
	assert.Equal(t, 0, len(fib.FindNextHopsEnc(name)))
	assert.NotNil(t, fib.FindStrategyEnc(name))
	strats := fib.GetAllForwardingStrategies()
	found := false
	for _, e := range strats {
		if e.Name().Equal(name) {
			found = true
		}
	}
	assert.True(t, found, "strategy entry must survive ClearNextHopsEnc")
}
