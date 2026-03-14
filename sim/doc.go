// Package sim provides an ns-3 discrete-event simulation adapter for NDNd.
//
// It bridges NDNd's Go forwarder with ns-3's C++ simulation engine via CGo,
// replacing wall-clock time with simulation time and replacing OS networking
// with ns-3 NetDevice packet delivery.
//
// The key mechanism is core.NowFunc: this package overrides it so that all
// of NDNd's forwarder code uses ns-3 simulation time instead of wall-clock
// time. Each simulated node gets its own fw.Thread with a per-node FIB, and
// faces are registered in the global dispatch table with unique IDs.
package sim
