package engine

import (
	"bufio"
	"os"
	"runtime"
	"strings"
)

type ClientRoutingMode uint8

const (
	ClientRoutingModeLocal ClientRoutingMode = iota
	ClientRoutingModeGateway
)

type ClientConfig struct {
	TransportUri string

	// Prefix expose routing mode: local (default) or gateway.
	RoutingMode ClientRoutingMode
}

// (AI GENERATED DESCRIPTION): Retrieves the NDN client configuration, starting with a default transport URI and overriding it with values from `client.conf` files in prioritized directories and the `NDN_CLIENT_TRANSPORT` environment variable.
func GetClientConfig() ClientConfig {
	// Default configuration
	transportUri := "unix:///run/nfd/nfd.sock"
	if runtime.GOOS == "darwin" {
		transportUri = "unix:///var/run/nfd/nfd.sock"
	}
	config := ClientConfig{
		TransportUri: transportUri,
		RoutingMode:  ClientRoutingModeLocal,
	}

	// Order of increasing priority
	configDirs := []string{
		"/etc/ndn",
		"/usr/local/etc/ndn",
		os.Getenv("HOME") + "/.ndn",
	}

	// Read each config file that we can find
	for _, dir := range configDirs {
		filename := dir + "/client.conf"

		file, err := os.OpenFile(filename, os.O_RDONLY, 0)
		if err != nil {
			continue
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") { // comment
				continue
			}

			key, val, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			val = strings.TrimSpace(val)

			switch key {
			case "transport":
				config.TransportUri = val
			case "routing_mode":
				switch strings.ToLower(strings.TrimSpace(val)) {
				case "gateway":
					config.RoutingMode = ClientRoutingModeGateway
				default:
					config.RoutingMode = ClientRoutingModeLocal
				}
			}
		}
	}

	// Environment variable overrides config file
	transportEnv := os.Getenv("NDN_CLIENT_TRANSPORT")
	if transportEnv != "" {
		config.TransportUri = transportEnv
	}
	if routingModeEnv := os.Getenv("NDN_CLIENT_ROUTING_MODE"); routingModeEnv != "" {
		switch strings.ToLower(strings.TrimSpace(routingModeEnv)) {
		case "gateway":
			config.RoutingMode = ClientRoutingModeGateway
		default:
			config.RoutingMode = ClientRoutingModeLocal
		}
	}

	return config
}
