package utils

import (
	"encoding/json"
	"fmt"
	"net"
)

// NmAddr represents an IP address in nmstate output.
type NmAddr struct {
	IP           string `json:"ip"`
	PrefixLength int    `json:"prefix-length"`
}

// NmIPConf represents IPv4/IPv6 configuration for an interface.
type NmIPConf struct {
	Enabled bool     `json:"enabled"`
	Address []NmAddr `json:"address"`
}

// NmIf represents a network interface in nmstate output.
type NmIf struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	IPv4   NmIPConf `json:"ipv4"`
	IPv6   NmIPConf `json:"ipv6"`
	Bridge NmBridge `json:"bridge,omitempty"`
	VLAN   *NmVLAN  `json:"vlan,omitempty"`
}

// NmRoute represents a route entry in nmstate output.
type NmRoute struct {
	Destination      string `json:"destination"`
	NextHopAddress   string `json:"next-hop-address"`
	NextHopInterface string `json:"next-hop-interface"`
}

// NmRoutes represents running and configured routes in nmstate output.
type NmRoutes struct {
	Running []NmRoute `json:"running"`
	Config  []NmRoute `json:"config"`
}

// NmDNSList represents a list of DNS servers in nmstate output.
type NmDNSList struct {
	Server []string `json:"server"`
}

// NmDNS represents DNS resolver configuration in nmstate output.
type NmDNS struct {
	Running NmDNSList `json:"running"`
	Config  NmDNSList `json:"config"`
}

// NmState is the top-level nmstate JSON structure.
type NmState struct {
	Interfaces  []NmIf   `json:"interfaces"`
	Routes      NmRoutes `json:"routes"`
	DNSResolver NmDNS    `json:"dns-resolver"`
}

// NmBridge represents bridge configuration for an interface.
type NmBridge struct {
	Port []struct {
		Name string `json:"name"`
	} `json:"port"`
}

// NmVLAN represents VLAN configuration for an interface.
type NmVLAN struct {
	BaseIface string `json:"base-iface"`
	ID        int    `json:"id"`
}

// ParseNmstate parses nmstate JSON output into NmState.
func ParseNmstate(output string) (NmState, error) {
	var state NmState
	if err := json.Unmarshal([]byte(output), &state); err != nil {
		return state, fmt.Errorf("failed to parse nmstate JSON: %w", err)
	}
	return state, nil
}

// ExtractDNS returns the first IPv4 and IPv6 DNS servers from nmstate, preferring running config.
func ExtractDNS(state NmState) (string, string) {
	dnsServers := state.DNSResolver.Running.Server
	if len(dnsServers) == 0 {
		dnsServers = state.DNSResolver.Config.Server
	}

	var dnsV4, dnsV6 string
	for _, s := range dnsServers {
		if isIPv6String(s) {
			if dnsV6 == "" {
				dnsV6 = s
			}
		} else {
			if dnsV4 == "" {
				dnsV4 = s
			}
		}
	}

	return dnsV4, dnsV6
}

// FindDefaultGateways searches nmstate routes to find default IPv4 and IPv6 gateways
// for the given bridge name and default route destinations.
func FindDefaultGateways(state NmState, bridgeName, defaultRouteV4, defaultRouteV6 string) (string, string) {
	findGW := func(dest string) string {
		for _, rt := range state.Routes.Running {
			if rt.Destination == dest && (rt.NextHopInterface == "" || rt.NextHopInterface == bridgeName) {
				return rt.NextHopAddress
			}
		}
		for _, rt := range state.Routes.Config {
			if rt.Destination == dest && (rt.NextHopInterface == "" || rt.NextHopInterface == bridgeName) {
				return rt.NextHopAddress
			}
		}
		return ""
	}
	return findGW(defaultRouteV4), findGW(defaultRouteV6)
}

// ExtractBrExVLANID inspects the bridge uplink port; if it's a VLAN interface, returns its VLAN ID.
func ExtractBrExVLANID(state NmState, bridgeName string) (*int, error) {
	uplink, err := ExtractBrExUplinkName(state, bridgeName)
	if err != nil {
		return nil, err
	}

	for _, intf := range state.Interfaces {
		if intf.Name == uplink && intf.Type == "vlan" && intf.VLAN != nil {
			return &intf.VLAN.ID, nil
		}
	}

	return nil, nil
}

// ExtractBrExUplinkName returns the uplink port name connected to the given bridge
// (excluding the bridge internal and patch ports).
func ExtractBrExUplinkName(state NmState, bridgeName string) (string, error) {
	for _, intf := range state.Interfaces {
		if intf.Name == bridgeName && intf.Type == "ovs-bridge" {
			for _, p := range intf.Bridge.Port {
				if p.Name != "" && p.Name != bridgeName {
					return p.Name, nil
				}
			}
		}
	}
	return "", fmt.Errorf("%s uplink port not found", bridgeName)
}

// FindMatchingCIDR returns the first CIDR from the list that contains the given IP
// and matches its IP family. If none is found, returns an empty string.
func FindMatchingCIDR(ipStr string, cidrs []string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ""
	}
	isV4 := ip.To4() != nil
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil || n == nil {
			continue
		}
		if (n.IP.To4() != nil) != isV4 {
			continue
		}
		if n.Contains(ip) {
			return c
		}
	}
	return ""
}

func isIPv6String(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() == nil
}
