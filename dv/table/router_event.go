package table

import (
	"sync"
	"time"

	enc "github.com/named-data/ndnd/std/encoding"
)

// RouterReachableEvent is fired when a DV node first learns a route to
// another router (the router becomes reachable in the RIB).
type RouterReachableEvent struct {
	// At is the timestamp when the router became reachable (sim or wall clock).
	At time.Time
	// NodeRouter is the router name of the observing node.
	NodeRouter enc.Name
	// ReachableRouter is the router name that just became reachable.
	ReachableRouter enc.Name
}

var (
	routerReachableObserverMu sync.RWMutex
	routerReachableObserver   func(RouterReachableEvent)
)

// SetRouterReachableObserver registers a callback invoked each time a DV
// router discovers that another router has become reachable.
func SetRouterReachableObserver(observer func(RouterReachableEvent)) {
	routerReachableObserverMu.Lock()
	routerReachableObserver = observer
	routerReachableObserverMu.Unlock()
}

// NotifyRouterReachable fires the registered observer with the given event.
// Safe to call from any goroutine. No-op if no observer is registered.
func NotifyRouterReachable(ev RouterReachableEvent) {
	routerReachableObserverMu.RLock()
	obs := routerReachableObserver
	routerReachableObserverMu.RUnlock()
	if obs != nil {
		obs(ev)
	}
}
