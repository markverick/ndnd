package bier_tests

import (
	"testing"

	fw "github.com/named-data/ndnd/fw/fw"
	enc "github.com/named-data/ndnd/std/encoding"
)

// TestBierBitManipulation tests the BIER bit manipulation helper functions
func TestBierBitManipulation(t *testing.T) {
	t.Run("SetBit and GetBit", func(t *testing.T) {
		var bs []byte

		// Set bits 0, 5, 15
		bs = fw.BierSetBit(bs, 0)
		bs = fw.BierSetBit(bs, 5)
		bs = fw.BierSetBit(bs, 15)

		// Check that bits are set correctly
		if !fw.BierGetBit(bs, 0) {
			t.Error("Bit 0 should be set")
		}
		if !fw.BierGetBit(bs, 5) {
			t.Error("Bit 5 should be set")
		}
		if !fw.BierGetBit(bs, 15) {
			t.Error("Bit 15 should be set")
		}

		// Check that other bits are not set
		if fw.BierGetBit(bs, 1) {
			t.Error("Bit 1 should not be set")
		}
		if fw.BierGetBit(bs, 7) {
			t.Error("Bit 7 should not be set")
		}
	})

	t.Run("ClearBit", func(t *testing.T) {
		bs := []byte{0xFF, 0xFF} // All bits set in first two bytes

		fw.BierClearBit(bs, 3)
		fw.BierClearBit(bs, 10)

		if fw.BierGetBit(bs, 3) {
			t.Error("Bit 3 should be cleared")
		}
		if fw.BierGetBit(bs, 10) {
			t.Error("Bit 10 should be cleared")
		}
		if !fw.BierGetBit(bs, 2) {
			t.Error("Bit 2 should still be set")
		}
		if !fw.BierGetBit(bs, 9) {
			t.Error("Bit 9 should still be set")
		}
	})

	t.Run("fw.BierAnd", func(t *testing.T) {
		a := []byte{0b11110000, 0b10101010}
		b := []byte{0b11001100, 0b11110000}

		result := fw.BierAnd(a, b)
		expected := []byte{0b11000000, 0b10100000}

		if len(result) != len(expected) {
			t.Errorf("Result length mismatch: got %d, want %d", len(result), len(expected))
		}
		for i := range expected {
			if result[i] != expected[i] {
				t.Errorf("Byte %d: got %08b, want %08b", i, result[i], expected[i])
			}
		}
	})

	t.Run("fw.BierAndNot", func(t *testing.T) {
		a := []byte{0b11111111, 0b11111111}
		b := []byte{0b00001111, 0b11110000}

		result := fw.BierAndNot(a, b)
		expected := []byte{0b11110000, 0b00001111}

		if len(result) != len(expected) {
			t.Errorf("Result length mismatch: got %d, want %d", len(result), len(expected))
		}
		for i := range expected {
			if result[i] != expected[i] {
				t.Errorf("Byte %d: got %08b, want %08b", i, result[i], expected[i])
			}
		}
	})

	t.Run("fw.BierIsZero", func(t *testing.T) {
		zero := []byte{0, 0, 0}
		nonZero := []byte{0, 0, 1}

		if !fw.BierIsZero(zero) {
			t.Error("Zero bitstring should return true")
		}
		if fw.BierIsZero(nonZero) {
			t.Error("Non-zero bitstring should return false")
		}
	})

	t.Run("fw.BierClone", func(t *testing.T) {
		original := []byte{1, 2, 3, 4}
		cloned := fw.BierClone(original)

		if len(cloned) != len(original) {
			t.Errorf("Clone length mismatch: got %d, want %d", len(cloned), len(original))
		}

		// Verify contents are identical
		for i := range original {
			if cloned[i] != original[i] {
				t.Errorf("Byte %d: got %d, want %d", i, cloned[i], original[i])
			}
		}

		// Verify it's a deep copy
		cloned[0] = 99
		if original[0] == 99 {
			t.Error("Modifying clone should not affect original")
		}
	})
}

// TestBiftConstruction tests the BIFT construction and lookup
func TestBiftConstruction(t *testing.T) {
	bift := &fw.BiftState{}

	t.Run("RegisterRouter", func(t *testing.T) {
		r1 := enc.Name{enc.NewGenericComponent("router1")}
		r2 := enc.Name{enc.NewGenericComponent("router2")}
		r3 := enc.Name{enc.NewGenericComponent("router3")}

		bift.RegisterRouter(r1, 0)
		bift.RegisterRouter(r2, 1)
		bift.RegisterRouter(r3, 5)

		// Verify lookups
		id1, ok1 := bift.GetRouterBfrId(r1)
		if !ok1 || id1 != 0 {
			t.Errorf("Router1 BFR-ID: got %d, want 0", id1)
		}

		id2, ok2 := bift.GetRouterBfrId(r2)
		if !ok2 || id2 != 1 {
			t.Errorf("Router2 BFR-ID: got %d, want 1", id2)
		}

		id3, ok3 := bift.GetRouterBfrId(r3)
		if !ok3 || id3 != 5 {
			t.Errorf("Router3 BFR-ID: got %d, want 5", id3)
		}

		// Test non-existent router
		r4 := enc.Name{enc.NewGenericComponent("router4")}
		_, ok4 := bift.GetRouterBfrId(r4)
		if ok4 {
			t.Error("Non-existent router should return false")
		}
	})

	t.Run("BuildBierBitString", func(t *testing.T) {
		r1 := enc.Name{enc.NewGenericComponent("router1")}
		r2 := enc.Name{enc.NewGenericComponent("router2")}
		r3 := enc.Name{enc.NewGenericComponent("router3")}

		bift.RegisterRouter(r1, 0)
		bift.RegisterRouter(r2, 1)
		bift.RegisterRouter(r3, 5)

		// Build bit-string for routers 1 and 3
		egressRouters := []enc.Name{r1, r3}
		bs := bift.BuildBierBitString(egressRouters)

		// Check that bits 0 and 5 are set
		if !fw.BierGetBit(bs, 0) {
			t.Error("Bit 0 (router1) should be set")
		}
		if !fw.BierGetBit(bs, 5) {
			t.Error("Bit 5 (router3) should be set")
		}
		if fw.BierGetBit(bs, 1) {
			t.Error("Bit 1 (router2) should not be set")
		}
	})

	t.Run("UpdateNextHop and RebuildFbm", func(t *testing.T) {
		r1 := enc.Name{enc.NewGenericComponent("router1")}
		r2 := enc.Name{enc.NewGenericComponent("router2")}
		r3 := enc.Name{enc.NewGenericComponent("router3")}

		bift.RegisterRouter(r1, 0)
		bift.RegisterRouter(r2, 1)
		bift.RegisterRouter(r3, 5)

		// Set next hops - routers 1 and 2 via face 100, router 3 via face 200
		bift.UpdateNextHop(0, 100)
		bift.UpdateNextHop(1, 100)
		bift.UpdateNextHop(5, 200)

		bift.RebuildFbm()

		// Get neighbor entries
		neighbors := bift.GetNeighborEntries()

		// Should have 2 neighbor entries (face 100 and face 200)
		if len(neighbors) != 2 {
			t.Errorf("Expected 2 neighbor entries, got %d", len(neighbors))
		}

		// Find face 100 entry and verify its F-BM has bits 0 and 1 set
		var face100Fbm []byte
		var face200Fbm []byte
		for _, n := range neighbors {
			if n.FaceID == 100 {
				face100Fbm = n.Fbm
			} else if n.FaceID == 200 {
				face200Fbm = n.Fbm
			}
		}

		if face100Fbm == nil {
			t.Error("Face 100 neighbor entry not found")
		} else {
			if !fw.BierGetBit(face100Fbm, 0) {
				t.Error("Face 100 F-BM should have bit 0 set")
			}
			if !fw.BierGetBit(face100Fbm, 1) {
				t.Error("Face 100 F-BM should have bit 1 set")
			}
			if fw.BierGetBit(face100Fbm, 5) {
				t.Error("Face 100 F-BM should not have bit 5 set")
			}
		}

		if face200Fbm == nil {
			t.Error("Face 200 neighbor entry not found")
		} else {
			if !fw.BierGetBit(face200Fbm, 5) {
				t.Error("Face 200 F-BM should have bit 5 set")
			}
			if fw.BierGetBit(face200Fbm, 0) {
				t.Error("Face 200 F-BM should not have bit 0 set")
			}
		}
	})
}

// TestBierReplicationMask tests the replication mask computation
func TestBierReplicationMask(t *testing.T) {
	// Simulate a scenario where we have 3 downstream neighbors
	// Neighbor 1 can reach routers at bits 0, 1, 2
	// Neighbor 2 can reach routers at bits 3, 4
	// Neighbor 3 can reach routers at bits 5, 6

	var fbm1, fbm2, fbm3 []byte
	fbm1 = fw.BierSetBit(fbm1, 0)
	fbm1 = fw.BierSetBit(fbm1, 1)
	fbm1 = fw.BierSetBit(fbm1, 2)

	fbm2 = fw.BierSetBit(fbm2, 3)
	fbm2 = fw.BierSetBit(fbm2, 4)

	fbm3 = fw.BierSetBit(fbm3, 5)
	fbm3 = fw.BierSetBit(fbm3, 6)

	// Incoming BIER bit-string has bits 0, 3, 5 set (3 egress routers)
	var incoming []byte
	incoming = fw.BierSetBit(incoming, 0)
	incoming = fw.BierSetBit(incoming, 3)
	incoming = fw.BierSetBit(incoming, 5)

	// Compute replication masks
	rep1 := fw.BierAnd(incoming, fbm1)
	rep2 := fw.BierAnd(incoming, fbm2)
	rep3 := fw.BierAnd(incoming, fbm3)

	// Neighbor 1 should replicate for bit 0 only
	if !fw.BierGetBit(rep1, 0) {
		t.Error("Neighbor 1 should replicate for bit 0")
	}
	if fw.BierGetBit(rep1, 1) || fw.BierGetBit(rep1, 2) {
		t.Error("Neighbor 1 should not replicate for bits 1 or 2")
	}

	// Neighbor 2 should replicate for bit 3 only
	if !fw.BierGetBit(rep2, 3) {
		t.Error("Neighbor 2 should replicate for bit 3")
	}
	if fw.BierGetBit(rep2, 4) {
		t.Error("Neighbor 2 should not replicate for bit 4")
	}

	// Neighbor 3 should replicate for bit 5 only
	if !fw.BierGetBit(rep3, 5) {
		t.Error("Neighbor 3 should replicate for bit 5")
	}
	if fw.BierGetBit(rep3, 6) {
		t.Error("Neighbor 3 should not replicate for bit 6")
	}
}
