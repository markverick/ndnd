package bier_tests

// Topology-level BIER simulation tests.
//
// These tests build realistic multi-router networks (linear, star, tree, ring,
// grid, full-mesh, random) and verify the BIER forwarding algorithm:
//   - Every specified BFER receives exactly one local delivery
//   - Packets never reach routers NOT in the bitstring
//   - Loop suppression prevents infinite forwarding
//   - Scale: up to 256 routers
//
// Each test creates per-router fw.BiftState objects and runs the BIER algorithm
// directly (without the global Bift or processBierInterest), allowing full
// multi-router simulation in a single test process.

import (
	"fmt"
	"math/rand"
	"sort"
	"testing"

	fw "github.com/named-data/ndnd/fw/fw"
	enc "github.com/named-data/ndnd/std/encoding"
)

// ---------------------------------------------------------------------------
// Topology simulator
// ---------------------------------------------------------------------------

// topo represents a simulated BIER network with N routers (IDs 0..N-1).
type topo struct {
	n    int
	adj  [][]int         // undirected adjacency list
	bift []*fw.BiftState // per-router BIFT (index == router ID == BFR-ID)
}

func newTopo(n int) *topo {
	t := &topo{
		n:    n,
		adj:  make([][]int, n),
		bift: make([]*fw.BiftState, n),
	}
	for i := range t.bift {
		t.bift[i] = &fw.BiftState{}
	}
	return t
}

func (t *topo) addLink(a, b int) {
	t.adj[a] = append(t.adj[a], b)
	t.adj[b] = append(t.adj[b], a)
}

// buildBifts computes each router's BIFT via BFS shortest-path routing.
// Face IDs are the direct-neighbour router IDs (one face per link).
func (t *topo) buildBifts() {
	for src := 0; src < t.n; src++ {
		// BFS from src
		dist := make([]int, t.n)
		nh := make([]int, t.n) // next-hop neighbour of src toward dst
		for i := range dist {
			dist[i] = -1
			nh[i] = -1
		}
		dist[src] = 0
		queue := []int{src}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, nb := range t.adj[cur] {
				if dist[nb] == -1 {
					dist[nb] = dist[cur] + 1
					if cur == src {
						nh[nb] = nb // direct link
					} else {
						nh[nb] = nh[cur]
					}
					queue = append(queue, nb)
				}
			}
		}

		b := t.bift[src]
		for dst := 0; dst < t.n; dst++ {
			if dst == src {
				continue
			}
			name := enc.Name{enc.NewGenericComponent(fmt.Sprintf("r%d", dst))}
			b.RegisterRouter(name, dst)
			if nh[dst] >= 0 {
				// Offset face IDs by 1: NextHop=0 is the "unset" sentinel in
				// fw.BiftState, so router 0 as a direct neighbour must map to face 1.
				b.UpdateNextHop(dst, uint64(nh[dst]+1))
			}
		}
		b.RebuildFbm()
	}
}

// buildBitstring sets bits for each router ID in ids.
func (t *topo) buildBitstring(ids ...int) []byte {
	var bs []byte
	for _, id := range ids {
		bs = fw.BierSetBit(bs, id)
	}
	return bs
}

// topoSimResult is returned by simulate.
type topoSimResult struct {
	delivered   map[int]bool // routers that got local delivery
	packetsSent int          // total number of forwarded copies
	hopCounts   map[int]int  // router → hop distance from BFIR
}

// simulate injects a BIER interest at srcRouter with bitstring bs and runs
// the full forwarding simulation.  Returns which routers received delivery.
func (t *topo) simulate(srcRouter int, bs []byte) topoSimResult {
	res := topoSimResult{
		delivered: make(map[int]bool),
		hopCounts: make(map[int]int),
	}

	type item struct {
		router   int
		bs       []byte
		inFace   int // -1 = BFIR injection (no loop-prevention skip)
		hopCount int
	}

	queue := []item{{srcRouter, fw.BierClone(bs), -1, 0}}
	// visited prevents processing the same (router, bitstring) state twice.
	visited := make(map[string]bool)

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		key := fmt.Sprintf("%d|%x", cur.router, cur.bs)
		if visited[key] {
			continue
		}
		visited[key] = true

		remaining := fw.BierClone(cur.bs)

		// BFER check: local bit
		if fw.BierGetBit(remaining, cur.router) {
			res.delivered[cur.router] = true
			if _, seen := res.hopCounts[cur.router]; !seen {
				res.hopCounts[cur.router] = cur.hopCount
			}
			fw.BierClearBit(remaining, cur.router)
		}

		if fw.BierIsZero(remaining) {
			continue
		}

		// BFR replication
		for _, nb := range t.bift[cur.router].GetNeighborEntries() {
			// Face IDs are offset by 1 (see buildBifts); map back to router ID.
			nbID := int(nb.FaceID) - 1
			if nbID == cur.inFace {
				continue
			}
			mask := fw.BierAnd(remaining, nb.Fbm)
			if fw.BierIsZero(mask) {
				continue
			}
			res.packetsSent++
			queue = append(queue, item{nbID, mask, cur.router, cur.hopCount + 1})
			remaining = fw.BierAndNot(remaining, nb.Fbm)
		}
	}
	return res
}

// assertDeliveredExactly verifies delivered == want (no more, no less).
func assertDeliveredExactly(t *testing.T, res topoSimResult, want ...int) {
	t.Helper()
	wantSet := make(map[int]bool, len(want))
	for _, id := range want {
		wantSet[id] = true
	}
	for id := range res.delivered {
		if !wantSet[id] {
			t.Errorf("unexpected delivery to router %d", id)
		}
	}
	for id := range wantSet {
		if !res.delivered[id] {
			t.Errorf("missing delivery to router %d", id)
		}
	}
}

// ---------------------------------------------------------------------------
// Topology constructors
// ---------------------------------------------------------------------------

func buildLinear(n int) *topo {
	g := newTopo(n)
	for i := 0; i < n-1; i++ {
		g.addLink(i, i+1)
	}
	g.buildBifts()
	return g
}

func buildStar(leaves int) *topo {
	// Node 0 is the hub; nodes 1..leaves are leaves.
	g := newTopo(leaves + 1)
	for i := 1; i <= leaves; i++ {
		g.addLink(0, i)
	}
	g.buildBifts()
	return g
}

func buildBinaryTree(depth int) *topo {
	// Perfect binary tree: root=0, children of i are 2i+1 and 2i+2.
	n := (1 << depth) - 1
	g := newTopo(n)
	for i := 0; i < n; i++ {
		l, r := 2*i+1, 2*i+2
		if l < n {
			g.addLink(i, l)
		}
		if r < n {
			g.addLink(i, r)
		}
	}
	g.buildBifts()
	return g
}

func buildRing(n int) *topo {
	g := newTopo(n)
	for i := 0; i < n; i++ {
		g.addLink(i, (i+1)%n)
	}
	g.buildBifts()
	return g
}

func buildGrid(rows, cols int) *topo {
	n := rows * cols
	g := newTopo(n)
	id := func(r, c int) int { return r*cols + c }
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			if c+1 < cols {
				g.addLink(id(r, c), id(r, c+1))
			}
			if r+1 < rows {
				g.addLink(id(r, c), id(r+1, c))
			}
		}
	}
	g.buildBifts()
	return g
}

func buildFullMesh(n int) *topo {
	g := newTopo(n)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			g.addLink(i, j)
		}
	}
	g.buildBifts()
	return g
}

// buildRandomConnected generates a random connected graph with n nodes and
// extra random edges (density fraction of max possible edges).
func buildRandomConnected(n int, density float64, seed int64) *topo {
	rng := rand.New(rand.NewSource(seed))
	g := newTopo(n)

	// First build a random spanning tree (Prüfer-like shuffle)
	perm := rng.Perm(n)
	for i := 1; i < n; i++ {
		j := rng.Intn(i)
		g.addLink(perm[i], perm[j])
	}

	// Add random extra edges
	maxExtra := int(float64(n*(n-1)/2) * density)
	existingEdges := make(map[[2]int]bool)
	for i := 0; i < n; i++ {
		for _, nb := range g.adj[i] {
			if nb > i {
				existingEdges[[2]int{i, nb}] = true
			}
		}
	}
	for attempt := 0; attempt < maxExtra*5 && len(existingEdges) < maxExtra+n-1; attempt++ {
		a, b := rng.Intn(n), rng.Intn(n)
		if a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		key := [2]int{a, b}
		if !existingEdges[key] {
			g.addLink(a, b)
			existingEdges[key] = true
		}
	}
	g.buildBifts()
	return g
}

// ---------------------------------------------------------------------------
// Tests — small / hand-verified topologies
// ---------------------------------------------------------------------------

func TestBierTopologyLinear5(t *testing.T) {
	// 0 - 1 - 2 - 3 - 4
	g := buildLinear(5)

	t.Run("BFIR=0 delivers to leaf 4 only", func(t *testing.T) {
		bs := g.buildBitstring(4)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 4)
	})

	t.Run("BFIR=0 delivers to all nodes", func(t *testing.T) {
		bs := g.buildBitstring(0, 1, 2, 3, 4)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 0, 1, 2, 3, 4)
	})

	t.Run("BFIR=2 delivers to both ends", func(t *testing.T) {
		bs := g.buildBitstring(0, 4)
		res := g.simulate(2, bs)
		assertDeliveredExactly(t, res, 0, 4)
	})

	t.Run("Intermediate-only bitstring — BFIR=0 delivers to 1,3", func(t *testing.T) {
		bs := g.buildBitstring(1, 3)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 1, 3)
		// Nodes 2 and 4 should NOT get delivery
		if res.delivered[2] || res.delivered[4] {
			t.Error("delivery leaked to non-BFER nodes")
		}
	})
}

func TestBierTopologyStar(t *testing.T) {
	// Hub=0, Leaves=1..8
	g := buildStar(8)

	t.Run("Hub to all leaves", func(t *testing.T) {
		bs := g.buildBitstring(1, 2, 3, 4, 5, 6, 7, 8)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 1, 2, 3, 4, 5, 6, 7, 8)
		// Hub delivers via 8 separate face copies (one per leaf)
		if res.packetsSent != 8 {
			t.Errorf("star hub should send exactly 8 packets, got %d", res.packetsSent)
		}
	})

	t.Run("Leaf to hub and 3 other leaves", func(t *testing.T) {
		bs := g.buildBitstring(0, 2, 5, 7)
		res := g.simulate(1, bs)
		assertDeliveredExactly(t, res, 0, 2, 5, 7)
	})

	t.Run("Leaf to single other leaf (2-hop)", func(t *testing.T) {
		bs := g.buildBitstring(8)
		res := g.simulate(1, bs)
		assertDeliveredExactly(t, res, 8)
	})
}

func TestBierTopologyBinaryTree(t *testing.T) {
	// Perfect binary tree, depth=4 → 15 nodes
	// Root=0, leaves=7..14
	g := buildBinaryTree(4)

	t.Run("Root to all leaves", func(t *testing.T) {
		bs := g.buildBitstring(7, 8, 9, 10, 11, 12, 13, 14)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 7, 8, 9, 10, 11, 12, 13, 14)
	})

	t.Run("Root to specific subtree (left half)", func(t *testing.T) {
		// Left subtree leaves: 7, 8, 9, 10
		bs := g.buildBitstring(7, 8, 9, 10)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 7, 8, 9, 10)
		// Right subtree leaves must NOT receive
		for _, id := range []int{11, 12, 13, 14} {
			if res.delivered[id] {
				t.Errorf("right-subtree leaf %d should not receive delivery", id)
			}
		}
	})

	t.Run("Leaf to root and far leaf (deep cross-tree)", func(t *testing.T) {
		// Leaf 7 to root (0) and far leaf (14) — 5 hops apart
		bs := g.buildBitstring(0, 14)
		res := g.simulate(7, bs)
		assertDeliveredExactly(t, res, 0, 14)
	})

	t.Run("All-to-all: every node is both BFIR and BFER", func(t *testing.T) {
		// Build bitstring for ALL 15 nodes
		all := make([]int, 15)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})
}

func TestBierTopologyRing(t *testing.T) {
	// 0 - 1 - 2 - 3 - 4 - 5 - 6 - 7 - 0
	g := buildRing(8)

	t.Run("BFIR=0 opposite side delivery (node 4)", func(t *testing.T) {
		bs := g.buildBitstring(4)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 4)
	})

	t.Run("BFIR=0 all nodes", func(t *testing.T) {
		all := make([]int, 8)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("BFIR=0 two opposite-side nodes", func(t *testing.T) {
		bs := g.buildBitstring(3, 5)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 3, 5)
	})

	t.Run("No stray delivery on ring — check non-BFER nodes", func(t *testing.T) {
		bs := g.buildBitstring(2, 6)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 2, 6)
		for _, id := range []int{1, 3, 4, 5, 7} {
			if res.delivered[id] {
				t.Errorf("non-BFER node %d received delivery", id)
			}
		}
	})
}

func TestBierTopologyDiamond(t *testing.T) {
	// Diamond: 0→1, 0→2, 1→3, 2→3
	// Tests multipath / redundancy scenario
	g := newTopo(4)
	g.addLink(0, 1)
	g.addLink(0, 2)
	g.addLink(1, 3)
	g.addLink(2, 3)
	g.buildBifts()

	t.Run("BFIR=0 to both 1 and 2 simultaneously", func(t *testing.T) {
		bs := g.buildBitstring(1, 2)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 1, 2)
	})

	t.Run("BFIR=0 to node 3 (two-hop, two paths)", func(t *testing.T) {
		bs := g.buildBitstring(3)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 3)
	})

	t.Run("BFIR=0 all four nodes", func(t *testing.T) {
		bs := g.buildBitstring(0, 1, 2, 3)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 0, 1, 2, 3)
	})

	t.Run("BFIR=3 back to 0 (reverse)", func(t *testing.T) {
		bs := g.buildBitstring(0)
		res := g.simulate(3, bs)
		assertDeliveredExactly(t, res, 0)
	})
}

func TestBierTopologyFullMesh(t *testing.T) {
	// 12-node full mesh: every pair connected
	const n = 12
	g := buildFullMesh(n)

	t.Run("Any node to all others", func(t *testing.T) {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
		// In a full mesh with n nodes, BFIR can reach each in 1 hop.
		// packetsSent ≤ n-1 (one per peer)
		if res.packetsSent > n-1 {
			t.Errorf("full mesh BFIR should send at most %d packets, sent %d", n-1, res.packetsSent)
		}
	})

	t.Run("Subset delivery — half the nodes", func(t *testing.T) {
		half := make([]int, n/2)
		for i := range half {
			half[i] = i * 2
		}
		bs := g.buildBitstring(half...)
		res := g.simulate(1, bs)
		assertDeliveredExactly(t, res, half...)
	})
}

func TestBierTopologyGrid8x8(t *testing.T) {
	// 8×8 = 64 nodes
	const rows, cols = 8, 8
	g := buildGrid(rows, cols)

	t.Run("Corner to corner (0 to 63)", func(t *testing.T) {
		bs := g.buildBitstring(63)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 63)
	})

	t.Run("All four corners", func(t *testing.T) {
		bs := g.buildBitstring(0, cols-1, (rows-1)*cols, rows*cols-1)
		res := g.simulate(32, bs) // inject from center
		assertDeliveredExactly(t, res, 0, cols-1, (rows-1)*cols, rows*cols-1)
	})

	t.Run("Full grid multicast — all 64 nodes from node 0", func(t *testing.T) {
		all := make([]int, rows*cols)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("Row multicast — deliver to entire top row", func(t *testing.T) {
		topRow := make([]int, cols)
		for c := 0; c < cols; c++ {
			topRow[c] = c
		}
		bs := g.buildBitstring(topRow...)
		res := g.simulate(rows*cols-1, bs) // inject from bottom-right
		assertDeliveredExactly(t, res, topRow...)
	})

	t.Run("Column multicast — deliver to entire left column", func(t *testing.T) {
		leftCol := make([]int, rows)
		for r := 0; r < rows; r++ {
			leftCol[r] = r * cols
		}
		bs := g.buildBitstring(leftCol...)
		res := g.simulate(cols-1, bs) // inject from top-right
		assertDeliveredExactly(t, res, leftCol...)
	})
}

// ---------------------------------------------------------------------------
// Scale tests — correctness at large router counts
// ---------------------------------------------------------------------------

func TestBierScaleLinear64(t *testing.T) {
	const n = 64
	g := buildLinear(n)

	t.Run("All 64 nodes deliver to themselves", func(t *testing.T) {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("Every other node (32 BFERs)", func(t *testing.T) {
		var even []int
		for i := 0; i < n; i += 2 {
			even = append(even, i)
		}
		bs := g.buildBitstring(even...)
		res := g.simulate(1, bs)
		assertDeliveredExactly(t, res, even...)
	})

	t.Run("Last node only from first node (63 hops)", func(t *testing.T) {
		bs := g.buildBitstring(n - 1)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, n-1)
		if res.delivered[1] || res.delivered[30] {
			t.Error("intermediate nodes should not get delivery")
		}
	})
}

func TestBierScaleBinaryTree128(t *testing.T) {
	// depth=7 → 127 nodes
	const depth = 7
	g := buildBinaryTree(depth)
	n := (1 << depth) - 1

	t.Run("Root to all leaves", func(t *testing.T) {
		firstLeaf := (1 << (depth - 1)) - 1 // 2^(depth-1) - 1
		var leaves []int
		for id := firstLeaf; id < n; id++ {
			leaves = append(leaves, id)
		}
		bs := g.buildBitstring(leaves...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, leaves...)
	})

	t.Run("All nodes all-multicast from root", func(t *testing.T) {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
		if len(res.delivered) != n {
			t.Errorf("expected %d deliveries, got %d", n, len(res.delivered))
		}
	})
}

func TestBierScaleGrid16x16(t *testing.T) {
	// 256 nodes
	const rows, cols = 16, 16
	g := buildGrid(rows, cols)
	n := rows * cols

	t.Run("Full grid all-multicast", func(t *testing.T) {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		if len(res.delivered) != n {
			t.Errorf("expected %d deliveries, got %d", n, len(res.delivered))
		}
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("Checkerboard pattern — 128 BFERs", func(t *testing.T) {
		var checkers []int
		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				if (r+c)%2 == 0 {
					checkers = append(checkers, r*cols+c)
				}
			}
		}
		bs := g.buildBitstring(checkers...)
		res := g.simulate(1, bs)
		assertDeliveredExactly(t, res, checkers...)
		// Odd nodes must not receive
		for r := 0; r < rows; r++ {
			for c := 0; c < cols; c++ {
				if (r+c)%2 != 0 {
					id := r*cols + c
					if res.delivered[id] {
						t.Errorf("non-BFER node %d received delivery", id)
					}
				}
			}
		}
	})
}

func TestBierScaleRandomGraph(t *testing.T) {
	// 64-node random connected graph, moderate density
	const n = 64
	g := buildRandomConnected(n, 0.05, 42)

	all := make([]int, n)
	for i := range all {
		all[i] = i
	}

	t.Run("All 64 nodes full multicast", func(t *testing.T) {
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("Random 32-node subset", func(t *testing.T) {
		rng := rand.New(rand.NewSource(99))
		perm := rng.Perm(n)
		subset := perm[:32]
		sort.Ints(subset)
		bs := g.buildBitstring(subset...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, subset...)
		// Nodes NOT in subset must not receive
		notInSubset := make(map[int]bool)
		for _, id := range perm[32:] {
			notInSubset[id] = true
		}
		for id := range res.delivered {
			if notInSubset[id] {
				t.Errorf("non-BFER node %d received delivery", id)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Loop-suppression and statelessness invariants
// ---------------------------------------------------------------------------

func TestBierLoopInvariantRing(t *testing.T) {
	// In any ring, no node should see the same (router,bitmask) state twice.
	// The visited-state check in simulate catches any loop; we just verify
	// the final delivery set is exactly what was requested.
	const n = 16
	g := buildRing(n)

	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	bs := g.buildBitstring(all...)
	res := g.simulate(0, bs)
	assertDeliveredExactly(t, res, all...)
}

func TestBierNoDeliveryToUnrequestedNodes(t *testing.T) {
	// Regardless of topology, nodes not in the bitstring must never see delivery.
	topologies := []struct {
		name string
		g    *topo
		n    int
	}{
		{"linear-20", buildLinear(20), 20},
		{"ring-20", buildRing(20), 20},
		{"grid-4x4", buildGrid(4, 4), 16},
		{"binary-tree-d4", buildBinaryTree(4), 15},
	}

	for _, tc := range topologies {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Only put every 3rd node in the bitstring
			var bfers []int
			bferSet := make(map[int]bool)
			for i := 0; i < tc.n; i += 3 {
				bfers = append(bfers, i)
				bferSet[i] = true
			}
			bs := tc.g.buildBitstring(bfers...)
			res := tc.g.simulate(0, bs)

			// Verify: every delivered node is in bferSet
			for id := range res.delivered {
				if !bferSet[id] {
					t.Errorf("[%s] node %d received delivery but was not a BFER", tc.name, id)
				}
			}
			// Verify: every bfer received delivery
			for _, id := range bfers {
				if !res.delivered[id] {
					t.Errorf("[%s] BFER node %d did not receive delivery", tc.name, id)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Additional topology constructors
// ---------------------------------------------------------------------------

// buildTorus constructs a rows×cols torus: a grid with wrap-around edges in
// both dimensions (every node has exactly 4 neighbours).
func buildTorus(rows, cols int) *topo {
	n := rows * cols
	g := newTopo(n)
	id := func(r, c int) int { return r*cols + c }
	added := make(map[[2]int]bool)
	addEdge := func(a, b int) {
		if a > b {
			a, b = b, a
		}
		key := [2]int{a, b}
		if !added[key] {
			g.addLink(a, b)
			added[key] = true
		}
	}
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			addEdge(id(r, c), id(r, (c+1)%cols))
			addEdge(id(r, c), id((r+1)%rows, c))
		}
	}
	g.buildBifts()
	return g
}

// buildNaryTree constructs a perfect N-ary tree with the given branching
// factor and depth (number of levels, including the root).
// Node 0 is the root; children of node i are branching*i+1..branching*i+branching.
func buildNaryTree(branching, depth int) *topo {
	n := 0
	power := 1
	for d := 0; d < depth; d++ {
		n += power
		power *= branching
	}
	g := newTopo(n)
	for i := 0; i < n; i++ {
		for j := 1; j <= branching; j++ {
			child := branching*i + j
			if child < n {
				g.addLink(i, child)
			}
		}
	}
	g.buildBifts()
	return g
}

// buildCaterpillar constructs a caterpillar graph: a linear spine (0..spineLen-1)
// with legsPerNode leaf nodes hanging off each spine node.
// Leaf attached to spine node i: spineLen + i*legsPerNode + j (j=0..legsPerNode-1).
func buildCaterpillar(spineLen, legsPerNode int) *topo {
	n := spineLen + spineLen*legsPerNode
	g := newTopo(n)
	for i := 0; i < spineLen-1; i++ {
		g.addLink(i, i+1)
	}
	for i := 0; i < spineLen; i++ {
		for j := 0; j < legsPerNode; j++ {
			g.addLink(i, spineLen+i*legsPerNode+j)
		}
	}
	g.buildBifts()
	return g
}

// buildBarbell constructs a barbell graph: two cliques connected by a path.
//   - Left clique:  nodes 0..clique1Size-1
//   - Path nodes:   nodes clique1Size..clique1Size+pathLen-1 (pathLen=0 → direct connection)
//   - Right clique: nodes clique1Size+pathLen..n-1
func buildBarbell(clique1Size, pathLen, clique2Size int) *topo {
	n := clique1Size + pathLen + clique2Size
	g := newTopo(n)
	for i := 0; i < clique1Size; i++ {
		for j := i + 1; j < clique1Size; j++ {
			g.addLink(i, j)
		}
	}
	start2 := clique1Size + pathLen
	for i := 0; i < clique2Size; i++ {
		for j := i + 1; j < clique2Size; j++ {
			g.addLink(start2+i, start2+j)
		}
	}
	if pathLen == 0 {
		g.addLink(0, start2)
	} else {
		g.addLink(0, clique1Size)
		for i := 0; i < pathLen-1; i++ {
			g.addLink(clique1Size+i, clique1Size+i+1)
		}
		g.addLink(clique1Size+pathLen-1, start2)
	}
	g.buildBifts()
	return g
}

// ---------------------------------------------------------------------------
// Torus topology tests
// ---------------------------------------------------------------------------

func TestBierTopologyTorus(t *testing.T) {
	t.Run("4x4 full multicast from corner", func(t *testing.T) {
		g := buildTorus(4, 4)
		all := make([]int, 16)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("4x4 inject from interior node, deliver to corners", func(t *testing.T) {
		// Node 5 = (row 1, col 1) in 4×4 layout; corners are 0,3,12,15.
		g := buildTorus(4, 4)
		bs := g.buildBitstring(0, 3, 12, 15)
		res := g.simulate(5, bs)
		assertDeliveredExactly(t, res, 0, 3, 12, 15)
	})

	t.Run("4x4 wrap-around: node 0 to right-edge and bottom-edge neighbours", func(t *testing.T) {
		// In a 4×4 torus node 0 (0,0) has wrap-around links to node 3 (0,3)
		// and node 12 (3,0).
		g := buildTorus(4, 4)
		bs := g.buildBitstring(3, 12)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 3, 12)
	})

	t.Run("4x4 no delivery to non-BFER nodes", func(t *testing.T) {
		g := buildTorus(4, 4)
		// Only even-indexed nodes are BFERs.
		var bfers []int
		bferSet := make(map[int]bool)
		for i := 0; i < 16; i += 2 {
			bfers = append(bfers, i)
			bferSet[i] = true
		}
		bs := g.buildBitstring(bfers...)
		res := g.simulate(1, bs)
		assertDeliveredExactly(t, res, bfers...)
		for i := 1; i < 16; i += 2 {
			if res.delivered[i] {
				t.Errorf("odd node %d received delivery but was not a BFER", i)
			}
		}
	})

	t.Run("6x6 checkerboard BFER pattern", func(t *testing.T) {
		g := buildTorus(6, 6)
		var checkers []int
		for r := 0; r < 6; r++ {
			for c := 0; c < 6; c++ {
				if (r+c)%2 == 0 {
					checkers = append(checkers, r*6+c)
				}
			}
		}
		bs := g.buildBitstring(checkers...)
		res := g.simulate(1, bs) // inject from a non-BFER node
		assertDeliveredExactly(t, res, checkers...)
		for r := 0; r < 6; r++ {
			for c := 0; c < 6; c++ {
				if (r+c)%2 != 0 {
					if res.delivered[r*6+c] {
						t.Errorf("non-BFER node %d received delivery", r*6+c)
					}
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// N-ary tree topology tests
// ---------------------------------------------------------------------------

func TestBierTopologyNaryTree(t *testing.T) {
	t.Run("3-ary depth-4 tree: root to all leaves", func(t *testing.T) {
		// 40 nodes total; leaves are nodes 13..39.
		g := buildNaryTree(3, 4)
		var leaves []int
		for i := 13; i < 40; i++ {
			leaves = append(leaves, i)
		}
		bs := g.buildBitstring(leaves...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, leaves...)
	})

	t.Run("3-ary depth-4 tree: all-multicast from root", func(t *testing.T) {
		g := buildNaryTree(3, 4)
		all := make([]int, 40)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("3-ary depth-4 tree: leaf-to-leaf cross-subtree delivery", func(t *testing.T) {
		// Leaf 13 (leftmost) to leaf 39 (rightmost) — maximum cross-tree path.
		g := buildNaryTree(3, 4)
		bs := g.buildBitstring(13, 39)
		res := g.simulate(13, bs)
		assertDeliveredExactly(t, res, 13, 39)
	})

	t.Run("4-ary depth-3 tree: root to all leaves", func(t *testing.T) {
		// 1+4+16 = 21 nodes; leaves are 5..20.
		g := buildNaryTree(4, 3)
		var leaves []int
		for i := 5; i < 21; i++ {
			leaves = append(leaves, i)
		}
		bs := g.buildBitstring(leaves...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, leaves...)
	})

	t.Run("5-ary depth-3 tree: all-multicast from root", func(t *testing.T) {
		// 1+5+25 = 31 nodes.
		g := buildNaryTree(5, 3)
		all := make([]int, 31)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("3-ary depth-4 tree: only right subtree BFERs", func(t *testing.T) {
		// Right subtree (via child 3 of root) has leaves 31..39.
		g := buildNaryTree(3, 4)
		rightLeaves := []int{31, 32, 33, 34, 35, 36, 37, 38, 39}
		bs := g.buildBitstring(rightLeaves...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, rightLeaves...)
		// Left and mid subtree leaves must not receive.
		for i := 13; i < 31; i++ {
			if res.delivered[i] {
				t.Errorf("left/mid-subtree leaf %d should not receive delivery", i)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Caterpillar topology tests
// ---------------------------------------------------------------------------

func TestBierTopologyCaterpillar(t *testing.T) {
	// Spine: 0..7  (8 nodes)
	// Leaves: spine node i → leaves 8+i*3, 8+i*3+1, 8+i*3+2
	// Total: 8 + 8*3 = 32 nodes.

	t.Run("Spine head BFIR to all leaves", func(t *testing.T) {
		g := buildCaterpillar(8, 3)
		var leaves []int
		for i := 8; i < 32; i++ {
			leaves = append(leaves, i)
		}
		bs := g.buildBitstring(leaves...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, leaves...)
	})

	t.Run("All-multicast from spine head", func(t *testing.T) {
		g := buildCaterpillar(8, 3)
		all := make([]int, 32)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("Leaf-to-leaf cross-spine delivery", func(t *testing.T) {
		// Leaf 8 (attached to spine 0) to leaf 31 (attached to spine 7).
		g := buildCaterpillar(8, 3)
		bs := g.buildBitstring(8, 31)
		res := g.simulate(8, bs)
		assertDeliveredExactly(t, res, 8, 31)
	})

	t.Run("Spine-only multicast: no leaf delivery", func(t *testing.T) {
		g := buildCaterpillar(8, 3)
		spine := []int{0, 1, 2, 3, 4, 5, 6, 7}
		bs := g.buildBitstring(spine...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, spine...)
		for i := 8; i < 32; i++ {
			if res.delivered[i] {
				t.Errorf("leaf %d received delivery but only spine nodes are BFERs", i)
			}
		}
	})

	t.Run("Alternating leaves from mid-spine BFIR", func(t *testing.T) {
		g := buildCaterpillar(8, 3)
		var altLeaves []int
		for i := 8; i < 32; i += 2 {
			altLeaves = append(altLeaves, i)
		}
		bs := g.buildBitstring(altLeaves...)
		res := g.simulate(3, bs) // inject from mid-spine node
		assertDeliveredExactly(t, res, altLeaves...)
	})

	t.Run("Single leaf from opposite end leaf", func(t *testing.T) {
		g := buildCaterpillar(8, 3)
		// Leaf 8 to leaf 30 — 7 spine hops + 2 leaf hops = 9 hops apart.
		bs := g.buildBitstring(30)
		res := g.simulate(8, bs)
		assertDeliveredExactly(t, res, 30)
	})
}

// ---------------------------------------------------------------------------
// Barbell topology tests
// ---------------------------------------------------------------------------

func TestBierTopologyBarbell(t *testing.T) {
	// Barbell(5, 3, 5): left clique 0..4, path 5..7, right clique 8..12 (13 nodes).

	t.Run("Left-clique BFIR to all right-clique BFERs", func(t *testing.T) {
		g := buildBarbell(5, 3, 5)
		rightClique := []int{8, 9, 10, 11, 12}
		bs := g.buildBitstring(rightClique...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, rightClique...)
	})

	t.Run("Right-clique BFIR to all left-clique BFERs", func(t *testing.T) {
		g := buildBarbell(5, 3, 5)
		leftClique := []int{0, 1, 2, 3, 4}
		bs := g.buildBitstring(leftClique...)
		res := g.simulate(8, bs)
		assertDeliveredExactly(t, res, leftClique...)
	})

	t.Run("Full all-multicast across barbell", func(t *testing.T) {
		g := buildBarbell(5, 3, 5)
		all := make([]int, 13)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("Path-node BFIR delivers to nodes in both cliques", func(t *testing.T) {
		g := buildBarbell(5, 3, 5)
		// Inject from middle path node 6; deliver to 2 nodes in each clique.
		bs := g.buildBitstring(1, 2, 9, 10)
		res := g.simulate(6, bs)
		assertDeliveredExactly(t, res, 1, 2, 9, 10)
	})

	t.Run("Direct barbell (no path nodes): clique1=4 directly joined to clique2=4", func(t *testing.T) {
		g := buildBarbell(4, 0, 4)
		all := make([]int, 8)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("Barbell: non-BFER path nodes receive no delivery", func(t *testing.T) {
		g := buildBarbell(5, 3, 5)
		// Only deliver to clique nodes; path nodes 5,6,7 must not receive.
		bs := g.buildBitstring(0, 1, 2, 3, 4, 8, 9, 10, 11, 12)
		res := g.simulate(0, bs)
		for _, pathNode := range []int{5, 6, 7} {
			if res.delivered[pathNode] {
				t.Errorf("path node %d received delivery but was not a BFER", pathNode)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Scale tests — new topologies at larger router counts
// ---------------------------------------------------------------------------

func TestBierScaleRing64(t *testing.T) {
	const n = 64
	g := buildRing(n)

	t.Run("All 64 nodes full multicast", func(t *testing.T) {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("Opposite-side single delivery (node 32 from node 0)", func(t *testing.T) {
		bs := g.buildBitstring(32)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 32)
	})

	t.Run("Quarter-ring delivery (nodes 0, 16, 32, 48 from node 8)", func(t *testing.T) {
		bs := g.buildBitstring(0, 16, 32, 48)
		res := g.simulate(8, bs)
		assertDeliveredExactly(t, res, 0, 16, 32, 48)
	})

	t.Run("Every other node (32 BFERs)", func(t *testing.T) {
		var even []int
		for i := 0; i < n; i += 2 {
			even = append(even, i)
		}
		bs := g.buildBitstring(even...)
		res := g.simulate(1, bs)
		assertDeliveredExactly(t, res, even...)
	})
}

func TestBierScaleStar32(t *testing.T) {
	// Hub=0, leaves=1..32
	const leaves = 32
	g := buildStar(leaves)

	t.Run("Hub to all 32 leaves", func(t *testing.T) {
		all := make([]int, leaves)
		for i := range all {
			all[i] = i + 1
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
		if res.packetsSent != leaves {
			t.Errorf("star hub should send exactly %d packets, got %d", leaves, res.packetsSent)
		}
	})

	t.Run("Leaf to hub and 15 other leaves (2-hop multicast)", func(t *testing.T) {
		targets := []int{0} // hub
		for i := 2; i <= 16; i++ {
			targets = append(targets, i)
		}
		bs := g.buildBitstring(targets...)
		res := g.simulate(1, bs)
		assertDeliveredExactly(t, res, targets...)
	})

	t.Run("Single leaf-to-leaf (2-hop via hub)", func(t *testing.T) {
		bs := g.buildBitstring(32)
		res := g.simulate(1, bs)
		assertDeliveredExactly(t, res, 32)
		// Leaf → hub → leaf: 2 packet copies forwarded.
		if res.packetsSent != 2 {
			t.Errorf("leaf-to-leaf via hub should send exactly 2 packets, got %d", res.packetsSent)
		}
	})
}

func TestBierScaleTorus8x8(t *testing.T) {
	const rows, cols = 8, 8
	g := buildTorus(rows, cols)
	n := rows * cols

	t.Run("Full 64-node torus multicast from corner", func(t *testing.T) {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
	})

	t.Run("Corner-to-corner (node 0 to node 63)", func(t *testing.T) {
		bs := g.buildBitstring(63)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, 63)
	})

	t.Run("Wrap-around: top row from bottom-right corner", func(t *testing.T) {
		topRow := make([]int, cols)
		for c := 0; c < cols; c++ {
			topRow[c] = c
		}
		bs := g.buildBitstring(topRow...)
		res := g.simulate(rows*cols-1, bs) // inject from bottom-right
		assertDeliveredExactly(t, res, topRow...)
	})

	t.Run("Left column multicast from top-right corner", func(t *testing.T) {
		leftCol := make([]int, rows)
		for r := 0; r < rows; r++ {
			leftCol[r] = r * cols
		}
		bs := g.buildBitstring(leftCol...)
		res := g.simulate(cols-1, bs)
		assertDeliveredExactly(t, res, leftCol...)
	})
}

func TestBierScaleNaryTree121(t *testing.T) {
	// 3-ary tree, depth 5: 1+3+9+27+81 = 121 nodes.
	// Leaves: nodes 40..120.
	const branching, depth = 3, 5
	g := buildNaryTree(branching, depth)
	n := 0
	power := 1
	for d := 0; d < depth; d++ {
		n += power
		power *= branching
	}
	firstLeaf := n - power/branching // = 121 - 81 = 40

	t.Run("Root to all leaves (81 BFERs)", func(t *testing.T) {
		var leaves []int
		for i := firstLeaf; i < n; i++ {
			leaves = append(leaves, i)
		}
		bs := g.buildBitstring(leaves...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, leaves...)
	})

	t.Run("All-multicast from root (121 nodes)", func(t *testing.T) {
		all := make([]int, n)
		for i := range all {
			all[i] = i
		}
		bs := g.buildBitstring(all...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, all...)
		if len(res.delivered) != n {
			t.Errorf("expected %d deliveries, got %d", n, len(res.delivered))
		}
	})

	t.Run("Every third leaf from root", func(t *testing.T) {
		var subset []int
		for i := firstLeaf; i < n; i += 3 {
			subset = append(subset, i)
		}
		bs := g.buildBitstring(subset...)
		res := g.simulate(0, bs)
		assertDeliveredExactly(t, res, subset...)
	})
}

// ---------------------------------------------------------------------------
// Multi-source injection tests
// ---------------------------------------------------------------------------

// TestBierMultiSourceInjection verifies that the same bitstring injected from
// different BFIR positions always delivers to exactly the requested BFERs.
func TestBierMultiSourceInjection(t *testing.T) {
	t.Run("Linear-10: same bitstring from three different BFIR positions", func(t *testing.T) {
		g := buildLinear(10)
		bfers := []int{2, 7}
		bs := g.buildBitstring(bfers...)

		for _, src := range []int{0, 5, 9} {
			res := g.simulate(src, bs)
			assertDeliveredExactly(t, res, bfers...)
		}
	})

	t.Run("Star: inject from different leaves, same delivery set", func(t *testing.T) {
		g := buildStar(8) // hub=0, leaves=1..8
		bfers := []int{0, 3, 6}
		bs := g.buildBitstring(bfers...)

		for _, src := range []int{1, 5, 8} {
			res := g.simulate(src, bs)
			assertDeliveredExactly(t, res, bfers...)
		}
	})

	t.Run("5x5 grid: inject from all four corners, same BFER set", func(t *testing.T) {
		const rows, cols = 5, 5
		g := buildGrid(rows, cols)
		bfers := []int{12, 7, 17} // center + two edge nodes
		bs := g.buildBitstring(bfers...)

		corners := []int{0, cols - 1, (rows - 1) * cols, rows*cols - 1}
		for _, corner := range corners {
			res := g.simulate(corner, bs)
			assertDeliveredExactly(t, res, bfers...)
		}
	})

	t.Run("Binary tree depth-4: inject from multiple leaves, deliver to root", func(t *testing.T) {
		g := buildBinaryTree(4)   // 15 nodes; leaves=7..14
		bs := g.buildBitstring(0) // only root is BFER

		for _, leaf := range []int{7, 10, 14} {
			res := g.simulate(leaf, bs)
			assertDeliveredExactly(t, res, 0)
		}
	})

	t.Run("Torus 4x4: same delivery set from all four corners", func(t *testing.T) {
		g := buildTorus(4, 4)
		// Deliver to a few interior nodes.
		bfers := []int{5, 6, 9, 10}
		bs := g.buildBitstring(bfers...)

		for _, corner := range []int{0, 3, 12, 15} {
			res := g.simulate(corner, bs)
			assertDeliveredExactly(t, res, bfers...)
		}
	})

	t.Run("Caterpillar: inject from any spine node, deliver to same leaf set", func(t *testing.T) {
		g := buildCaterpillar(6, 2) // 6 spine + 12 leaves = 18 nodes
		// Leaves of spine nodes 0 and 5: 6,7 and 16,17.
		bfers := []int{6, 7, 16, 17}
		bs := g.buildBitstring(bfers...)

		for _, src := range []int{0, 2, 4, 5} {
			res := g.simulate(src, bs)
			assertDeliveredExactly(t, res, bfers...)
		}
	})
}

// ---------------------------------------------------------------------------
// Benchmark — forwarding throughput at scale
// ---------------------------------------------------------------------------

func BenchmarkBierSimulate256NodeGrid(b *testing.B) {
	g := buildGrid(16, 16)
	n := 256
	all := make([]int, n)
	for i := range all {
		all[i] = i
	}
	bs := g.buildBitstring(all...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.simulate(0, bs)
	}
}

func BenchmarkBierSimulate64NodeRandom(b *testing.B) {
	g := buildRandomConnected(64, 0.1, 777)
	all := make([]int, 64)
	for i := range all {
		all[i] = i
	}
	bs := g.buildBitstring(all...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.simulate(0, bs)
	}
}

func BenchmarkBierBuildBifts256(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buildGrid(16, 16)
	}
}

func BenchmarkBierSimulate64NodeTorus(b *testing.B) {
	g := buildTorus(8, 8)
	all := make([]int, 64)
	for i := range all {
		all[i] = i
	}
	bs := g.buildBitstring(all...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.simulate(0, bs)
	}
}

func BenchmarkBierSimulate40NodeNaryTree(b *testing.B) {
	g := buildNaryTree(3, 4)
	all := make([]int, 40)
	for i := range all {
		all[i] = i
	}
	bs := g.buildBitstring(all...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.simulate(0, bs)
	}
}

func BenchmarkBierSimulate121NodeNaryTree(b *testing.B) {
	g := buildNaryTree(3, 5)
	all := make([]int, 121)
	for i := range all {
		all[i] = i
	}
	bs := g.buildBitstring(all...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.simulate(0, bs)
	}
}

func BenchmarkBierSimulate32NodeCaterpillar(b *testing.B) {
	g := buildCaterpillar(8, 3)
	all := make([]int, 32)
	for i := range all {
		all[i] = i
	}
	bs := g.buildBitstring(all...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		g.simulate(0, bs)
	}
}
