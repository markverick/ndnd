package table

import (
	"testing"

	enc "github.com/named-data/ndnd/std/encoding"
)

func testName(t *testing.T, s string) enc.Name {
	t.Helper()
	n, err := enc.NameFromStr(s)
	if err != nil {
		t.Fatalf("failed to parse name %s: %v", s, err)
	}
	return n
}

func newTestPet() *PrefixEgressTable {
	return &PrefixEgressTable{
		root: petNode{
			children: make(map[uint64]*petNode),
		},
	}
}

func TestPetCleanUpFaceRemovesOnlyTargetFace(t *testing.T) {
	pet := newTestPet()
	name := testName(t, "/example/a")

	pet.AddNextHopEnc(name, 11, 10)
	pet.AddNextHopEnc(name, 22, 20)

	pet.CleanUpFace(11)

	entry, ok := pet.FindExactEnc(name)
	if !ok {
		t.Fatalf("entry unexpectedly removed")
	}
	if len(entry.NextHops) != 1 || entry.NextHops[0].FaceID != 22 {
		t.Fatalf("unexpected remaining next hops: %+v", entry.NextHops)
	}
}

func TestPetCleanUpFaceRemovesEntryWhenNoNexthopOrEgressRemain(t *testing.T) {
	pet := newTestPet()
	name := testName(t, "/example/b")

	pet.AddNextHopEnc(name, 33, 5)
	pet.CleanUpFace(33)

	if _, ok := pet.FindExactEnc(name); ok {
		t.Fatalf("entry should be removed after last nexthop cleanup")
	}
}

func TestPetCleanUpFaceKeepsEntryWhenEgressExists(t *testing.T) {
	pet := newTestPet()
	name := testName(t, "/example/c")
	egress := testName(t, "/router/x")

	pet.AddEgressEnc(name, egress, false)
	pet.AddNextHopEnc(name, 44, 1)
	pet.CleanUpFace(44)

	entry, ok := pet.FindExactEnc(name)
	if !ok {
		t.Fatalf("entry unexpectedly removed")
	}
	if len(entry.NextHops) != 0 {
		t.Fatalf("expected next hops to be removed, got %+v", entry.NextHops)
	}
	if len(entry.EgressRouters) != 1 || !entry.EgressRouters[0].Equal(egress) {
		t.Fatalf("unexpected egress routers: %+v", entry.EgressRouters)
	}
}

func TestPetRootPrefixDefaultNextHop(t *testing.T) {
	pet := newTestPet()
	root := testName(t, "/")
	name := testName(t, "/example/root-default")

	pet.AddNextHopEnc(root, 77, 0)

	entry, ok := pet.FindExactEnc(root)
	if !ok {
		t.Fatalf("root PET entry missing")
	}
	if len(entry.NextHops) != 1 || entry.NextHops[0].FaceID != 77 {
		t.Fatalf("unexpected root nexthops: %+v", entry.NextHops)
	}

	longest, ok := pet.FindLongestPrefixEnc(name)
	if !ok {
		t.Fatalf("longest prefix lookup should return root entry")
	}
	if len(longest.NextHops) != 1 || longest.NextHops[0].FaceID != 77 {
		t.Fatalf("unexpected longest-prefix nexthops: %+v", longest.NextHops)
	}

	pet.RemoveNextHopEnc(root, 77)
	if _, ok := pet.FindExactEnc(root); ok {
		t.Fatalf("root PET entry should be removed after deleting last nexthop")
	}
}
