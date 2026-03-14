package sim

import (
	"fmt"
	"sync"
)

// Runtime manages all simulation nodes and provides the global simulation
// state. There is exactly one Runtime per simulation run.
type Runtime struct {
	mu    sync.Mutex
	clock Clock
	nodes map[uint32]*Node
}

// NewRuntime creates a new simulation runtime with the given clock.
func NewRuntime(clock Clock) *Runtime {
	return &Runtime{
		clock: clock,
		nodes: make(map[uint32]*Node),
	}
}

// CreateNode creates a new simulation node with the given ID.
func (r *Runtime) CreateNode(id uint32) (*Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.nodes[id]; exists {
		return nil, fmt.Errorf("node %d already exists", id)
	}

	node := NewNode(id, r.clock)
	r.nodes[id] = node
	return node, nil
}

// GetNode returns the node with the given ID.
func (r *Runtime) GetNode(id uint32) *Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodes[id]
}

// DestroyNode stops and removes the node with the given ID.
func (r *Runtime) DestroyNode(id uint32) {
	r.mu.Lock()
	node, ok := r.nodes[id]
	if ok {
		delete(r.nodes, id)
	}
	r.mu.Unlock()

	if ok {
		node.Stop()
	}
}

// DestroyAll stops and removes all nodes.
func (r *Runtime) DestroyAll() {
	r.mu.Lock()
	nodes := make(map[uint32]*Node, len(r.nodes))
	for k, v := range r.nodes {
		nodes[k] = v
	}
	r.nodes = make(map[uint32]*Node)
	r.mu.Unlock()

	for _, node := range nodes {
		node.Stop()
	}
}

// NodeCount returns the number of active nodes.
func (r *Runtime) NodeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.nodes)
}

// Clock returns the simulation clock.
func (r *Runtime) Clock() Clock {
	return r.clock
}
