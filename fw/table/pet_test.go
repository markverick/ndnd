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

	pet.AddEgressEnc(name, egress)
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
