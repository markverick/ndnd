package sim

import (
	enc "github.com/named-data/ndnd/std/encoding"
)

// parseNameFromString parses an NDN name from its URI string representation.
func parseNameFromString(s string) (enc.Name, error) {
	return enc.NameFromStr(s)
}
