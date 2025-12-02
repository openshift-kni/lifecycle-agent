package utils

import (
	"fmt"
	"net"
	"strings"

	"github.com/openshift-kni/lifecycle-agent/internal/common"
)

// ValidateIPFamilyConfig performs family-specific validation for addr, CIDR, gateway and DNS.
// The addr argument may be provided either as "IP" or "IP/prefix". All fields may optionally
// be wrapped in square brackets, which will be ignored during parsing.
func ValidateIPFamilyConfig(
	family string,
	addr string,
	networkCIDR string,
	gateway string,
	dnsServer string,
) error {
	trim := func(s string) string {
		return strings.Trim(s, "[]")
	}

	addr = trim(addr)
	networkCIDR = trim(networkCIDR)
	gateway = trim(gateway)
	dnsServer = trim(dnsServer)

	var ip net.IP
	if strings.Contains(addr, "/") {
		parsedIP, _, err := net.ParseCIDR(addr)
		if err != nil {
			return fmt.Errorf("invalid %s address: %s", strings.ToUpper(family), addr)
		}
		ip = parsedIP
	} else {
		ip = net.ParseIP(addr)
		if ip == nil {
			return fmt.Errorf("invalid %s address: %s", strings.ToUpper(family), addr)
		}
	}

	isIPv4 := family == common.IPv4FamilyName
	if isIPv4 && ip.To4() == nil {
		return fmt.Errorf("invalid %s address: %s", strings.ToUpper(family), addr)
	}
	if !isIPv4 && ip.To4() != nil {
		return fmt.Errorf("invalid %s address: %s", strings.ToUpper(family), addr)
	}

	_, ipNet, err := net.ParseCIDR(networkCIDR)
	if err != nil {
		return fmt.Errorf("invalid %s machine network CIDR: %s", strings.ToLower(family), networkCIDR)
	}
	if isIPv4 && ipNet.IP.To4() == nil {
		return fmt.Errorf("%s-machine-network must be an %s CIDR: %s", family, strings.ToUpper(family), networkCIDR)
	}
	if !isIPv4 && ipNet.IP.To4() != nil {
		return fmt.Errorf("%s-machine-network must be an %s CIDR: %s", family, strings.ToUpper(family), networkCIDR)
	}
	if !ipNet.Contains(ip) {
		return fmt.Errorf("%s address %s is not within machine network %s", strings.ToUpper(family), addr, networkCIDR)
	}

	if gateway != "" {
		gw := net.ParseIP(gateway)
		if gw == nil {
			return fmt.Errorf("invalid %s gateway: %s", strings.ToUpper(family), gateway)
		}
		if isIPv4 && gw.To4() == nil {
			return fmt.Errorf("invalid %s gateway: %s", strings.ToUpper(family), gateway)
		}
		if !isIPv4 && gw.To4() != nil {
			return fmt.Errorf("invalid %s gateway: %s", strings.ToUpper(family), gateway)
		}
		if !ipNet.Contains(gw) {
			return fmt.Errorf("%s gateway %s is not within machine network %s", strings.ToUpper(family), gateway, networkCIDR)
		}
	}

	if dnsServer != "" {
		dns := net.ParseIP(dnsServer)
		if dns == nil {
			return fmt.Errorf("invalid %s DNS server: %s", strings.ToUpper(family), dnsServer)
		}
		if isIPv4 && dns.To4() == nil {
			return fmt.Errorf("invalid %s DNS server: %s", strings.ToUpper(family), dnsServer)
		}
		if !isIPv4 && dns.To4() != nil {
			return fmt.Errorf("invalid %s DNS server: %s", strings.ToUpper(family), dnsServer)
		}
	}

	return nil
}
