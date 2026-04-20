// This file is the CGo build entry point. When built with
// -buildmode=c-shared or c-archive, it produces a shared library
// or static archive that ns-3 can link against.
//
// The exported symbols from sim/cgo_export.go are available
// to C/C++ code through the generated header file.
package main

import "C"

// Import the sim package to ensure all CGo exports are registered.
import _ "github.com/named-data/ndnd/sim"

func main() {}
