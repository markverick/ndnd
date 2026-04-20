package sim

import (
	"sync"
)

// Runtime manages all simulation nodes and provides the global simulation
// state. There is exactly one Runtime per simulation run.
type Runtime struct {
	mu    sync.Mutex
	nodes map[uint32]*Node
}

// NewRuntime creates a new simulation runtime.
func NewRuntime() *Runtime {
	return &Runtime{
		nodes: make(map[uint32]*Node),
	}
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

// AddNode registers a node under the given ID.
func (r *Runtime) AddNode(id uint32, node *Node) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[id] = node
}

// NodeCount returns the number of active nodes.
func (r *Runtime) NodeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.nodes)
}
