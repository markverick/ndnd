package table

import (
	"sync"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
)

// PrefixEventKind represents the type of prefix event.
type PrefixEventKind int

const (
	PrefixEventGlobalAnnounce  PrefixEventKind = iota
	PrefixEventAddRemotePrefix
)

// PrefixEvent is fired when prefix state changes in the DV system.
type PrefixEvent struct {
	Kind   PrefixEventKind
	At     time.Time
	Name   enc.Name
	Router enc.Name
}

var (
	prefixEventObserverMu sync.RWMutex
	prefixEventObserver   func(PrefixEvent)
)

// SetPrefixEventObserver registers a callback invoked on prefix events.
func SetPrefixEventObserver(observer func(PrefixEvent)) {
	prefixEventObserverMu.Lock()
	prefixEventObserver = observer
	prefixEventObserverMu.Unlock()
}

// NotifyPrefixEvent fires the registered observer with the given event.
func NotifyPrefixEvent(ev PrefixEvent) {
	prefixEventObserverMu.RLock()
	obs := prefixEventObserver
	prefixEventObserverMu.RUnlock()
	if obs != nil {
		obs(ev)
	}
}
