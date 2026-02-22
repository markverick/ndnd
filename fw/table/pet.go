/* YaNFD - Yet another NDN Forwarding Daemon
 *
 * Copyright (C) 2020-2025 Eric Newberry.
 *
 * This file is licensed under the terms of the MIT License, as found in LICENSE.md.
 */

package table

import (
	"container/list"
	"sort"
	"sync"

	enc "github.com/named-data/ndnd/std/encoding"
)

// PetNextHop represents a next hop in the PET.
type PetNextHop struct {
	FaceID uint64
	Cost   uint64
}

// PetEntry represents a snapshot of a PET entry.
type PetEntry struct {
	Name          enc.Name
	EgressRouters []enc.Name
	NextHops      []PetNextHop
}

type petEntryState struct {
	name     enc.Name
	egress   map[uint64]enc.Name
	nextHops map[uint64]PetNextHop
}

func (e *petEntryState) snapshot() PetEntry {
	entry := PetEntry{
		Name:          e.name.Clone(),
		EgressRouters: make([]enc.Name, 0, len(e.egress)),
		NextHops:      make([]PetNextHop, 0, len(e.nextHops)),
	}

	for _, egress := range e.egress {
		entry.EgressRouters = append(entry.EgressRouters, egress.Clone())
	}
	for _, nh := range e.nextHops {
		entry.NextHops = append(entry.NextHops, nh)
	}

	sort.Slice(entry.EgressRouters, func(i, j int) bool {
		return entry.EgressRouters[i].String() < entry.EgressRouters[j].String()
	})
	sort.Slice(entry.NextHops, func(i, j int) bool {
		return entry.NextHops[i].FaceID < entry.NextHops[j].FaceID
	})

	return entry
}

type petNode struct {
	name      enc.Name
	component enc.Component
	depth     int

	parent   *petNode
	children map[uint64]*petNode

	entry *petEntryState
}

func (n *petNode) cleanUpFace(faceID uint64) bool {
	for hash, child := range n.children {
		if child.cleanUpFace(faceID) {
			delete(n.children, hash)
			child.parent = nil
		}
	}

	if n.entry != nil {
		delete(n.entry.nextHops, faceID)
		if len(n.entry.egress) == 0 && len(n.entry.nextHops) == 0 {
			n.entry = nil
		}
	}

	return n.parent != nil && n.entry == nil && len(n.children) == 0
}

// PrefixEgressTable represents the Prefix Egress Table (PET).
type PrefixEgressTable struct {
	root  petNode
	mutex sync.RWMutex
}

// Pet is the global Prefix Egress Table.
var Pet = PrefixEgressTable{
	root: petNode{
		children: make(map[uint64]*petNode),
	},
}

// (AI GENERATED DESCRIPTION): Returns the literal string "pet" to identify the PET table in logs.
func (p *PrefixEgressTable) String() string {
	return "pet"
}

func (n *petNode) findExactMatchEntryEnc(name enc.Name) *petNode {
	match := n.findLongestPrefixEntryEnc(name)
	if len(name) == len(match.name) {
		return match
	}
	return nil
}

func (n *petNode) findLongestPrefixEntryEnc(name enc.Name) *petNode {
	if len(name) > n.depth {
		if child, ok := n.children[At(name, n.depth).Hash()]; ok {
			return child.findLongestPrefixEntryEnc(name)
		}
	}
	return n
}

func (n *petNode) fillTreeToPrefixEnc(name enc.Name) *petNode {
	entry := n.findLongestPrefixEntryEnc(name)

	for depth := entry.depth; depth < len(name); depth++ {
		component := At(name, depth).Clone()
		child := &petNode{
			name:      entry.name.Append(component),
			component: component,
			depth:     depth + 1,
			parent:    entry,
			children:  make(map[uint64]*petNode),
		}
		entry.children[component.Hash()] = child
		entry = child
	}
	return entry
}

func (n *petNode) pruneIfEmpty() {
	for entry := n; entry != nil && entry.parent != nil && entry.entry == nil && len(entry.children) == 0; {
		parent := entry.parent
		delete(parent.children, entry.component.Hash())
		entry.parent = nil
		entry = parent
	}
}

func (p *PrefixEgressTable) getOrCreateEntry(node *petNode, name enc.Name) *petEntryState {
	if node.entry == nil {
		node.entry = &petEntryState{
			name:     name.Clone(),
			egress:   make(map[uint64]enc.Name),
			nextHops: make(map[uint64]PetNextHop),
		}
	}
	return node.entry
}

// AddEgressEnc adds an egress router for the specified prefix.
func (p *PrefixEgressTable) AddEgressEnc(prefix enc.Name, egress enc.Name) {
	if len(egress) == 0 {
		return
	}

	p.mutex.Lock()
	defer p.mutex.Unlock()

	node := p.root.fillTreeToPrefixEnc(prefix)
	entry := p.getOrCreateEntry(node, prefix)
	entry.egress[egress.Hash()] = egress.Clone()
}

// RemoveEgressEnc removes an egress router from the specified prefix.
func (p *PrefixEgressTable) RemoveEgressEnc(prefix enc.Name, egress enc.Name) {
	if len(egress) == 0 {
		return
	}

	p.mutex.Lock()
	defer p.mutex.Unlock()

	node := p.root.findExactMatchEntryEnc(prefix)
	if node == nil || node.entry == nil {
		return
	}

	delete(node.entry.egress, egress.Hash())
	if len(node.entry.egress) == 0 && len(node.entry.nextHops) == 0 {
		node.entry = nil
		node.pruneIfEmpty()
	}
}

// AddNextHopEnc adds or updates a nexthop for the specified prefix.
func (p *PrefixEgressTable) AddNextHopEnc(prefix enc.Name, faceID uint64, cost uint64) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	node := p.root.fillTreeToPrefixEnc(prefix)
	entry := p.getOrCreateEntry(node, prefix)
	entry.nextHops[faceID] = PetNextHop{FaceID: faceID, Cost: cost}
}

// RemoveNextHopEnc removes a nexthop for the specified prefix.
func (p *PrefixEgressTable) RemoveNextHopEnc(prefix enc.Name, faceID uint64) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	node := p.root.findExactMatchEntryEnc(prefix)
	if node == nil || node.entry == nil {
		return
	}

	delete(node.entry.nextHops, faceID)
	if len(node.entry.egress) == 0 && len(node.entry.nextHops) == 0 {
		node.entry = nil
		node.pruneIfEmpty()
	}
}

// FindExactEnc returns a snapshot of the PET entry for the exact prefix.
func (p *PrefixEgressTable) FindExactEnc(prefix enc.Name) (PetEntry, bool) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	node := p.root.findExactMatchEntryEnc(prefix)
	if node == nil || node.entry == nil {
		return PetEntry{}, false
	}
	return node.entry.snapshot(), true
}

// FindLongestPrefixEnc returns a snapshot of the longest-prefix matching PET entry.
func (p *PrefixEgressTable) FindLongestPrefixEnc(name enc.Name) (PetEntry, bool) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	node := p.root.findLongestPrefixEntryEnc(name)
	for node != nil && node.entry == nil {
		node = node.parent
	}
	if node == nil || node.entry == nil {
		return PetEntry{}, false
	}
	return node.entry.snapshot(), true
}

// GetAllEntries returns snapshots of all PET entries.
func (p *PrefixEgressTable) GetAllEntries() []PetEntry {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	entries := make([]PetEntry, 0)
	queue := list.New()
	queue.PushBack(&p.root)

	for queue.Len() > 0 {
		node := queue.Front().Value.(*petNode)
		queue.Remove(queue.Front())

		for _, child := range node.children {
			queue.PushFront(child)
		}

		if node.entry != nil {
			entries = append(entries, node.entry.snapshot())
		}
	}

	return entries
}

// CleanUpFace removes all PET next hops that use the specified face.
func (p *PrefixEgressTable) CleanUpFace(faceID uint64) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.root.cleanUpFace(faceID)
}
