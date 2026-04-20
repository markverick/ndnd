package core

import "time"

// NowFunc returns the current time. Defaults to time.Now.
// Override this for simulation to return ns-3 simulation time.
var NowFunc = time.Now

// Now returns the current time using NowFunc.
func Now() time.Time {
	return NowFunc()
}
