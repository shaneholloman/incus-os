package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxc/incus/v6/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/api"
)

// networkdConfigFile represents a given filename and its contents.
type networkdConfigFile struct {
	Name     string
	Contents string
}

// ApplyNetworkConfiguration instructs systemd-networkd to apply the supplied network configuration.
func ApplyNetworkConfiguration(ctx context.Context, networkCfg *api.SystemNetworkConfig, timeout time.Duration) error {
	err := ValidateNetworkConfiguration(networkCfg)
	if err != nil {
		return err
	}

	// Get hostname and domain from network config, if defined.
	hostname := ""
	if networkCfg.DNS != nil && networkCfg.DNS.Hostname != "" {
		hostname = networkCfg.DNS.Hostname
		if networkCfg.DNS.Domain != "" {
			hostname += "." + networkCfg.DNS.Domain
		}
	}

	// Apply the configured hostname, or reset back to default if not set.
	err = SetHostname(ctx, hostname)
	if err != nil {
		return err
	}

	// Set proxy environment variables, or clear existing ones if none are defined.
	err = UpdateEnvironment(networkCfg.Proxy)
	if err != nil {
		return err
	}

	err = generateNetworkConfiguration(ctx, networkCfg)
	if err != nil {
		return err
	}

	err = waitForUdevInterfaceRename(ctx, 5*time.Second)
	if err != nil {
		return err
	}

	// Restart networking after new config files have been generated.
	err = RestartUnit(ctx, "systemd-networkd")
	if err != nil {
		return err
	}

	// (Re)start NTP time synchronization. Since we might be overriding the default fallback NTP servers,
	// the service is disabled by default and only started once we have performed the network (re)configuration.
	err = RestartUnit(ctx, "systemd-timesyncd")
	if err != nil {
		return err
	}

	// Wait for the network to apply.
	return waitForNetworkOnline(ctx, networkCfg, timeout)
}

// ValidateNetworkConfiguration performs some basic validation checks on the supplied network configuration.
func ValidateNetworkConfiguration(networkCfg *api.SystemNetworkConfig) error {
	if networkCfg == nil {
		return errors.New("no network configuration provided")
	}

	err := validateInterfaces(networkCfg.Interfaces, networkCfg.VLANs)
	if err != nil {
		return err
	}

	err = validateBonds(networkCfg.Bonds, networkCfg.VLANs)
	if err != nil {
		return err
	}

	err = validateVLANs(networkCfg)
	if err != nil {
		return err
	}

	return nil
}

// generateNetworkConfiguration clears any existing configuration from /run/systemd/network/ and generates
// new config files from the supplied NetworkConfig struct.
func generateNetworkConfiguration(_ context.Context, networkCfg *api.SystemNetworkConfig) error {
	// Remove any existing configuration.
	err := os.RemoveAll(SystemdNetworkConfigPath)
	if err != nil {
		return err
	}

	err = os.Mkdir(SystemdNetworkConfigPath, 0o755)
	if err != nil {
		return err
	}

	// Generate .link files.
	for _, cfg := range generateLinkFileContents(*networkCfg) {
		err := os.WriteFile(filepath.Join(SystemdNetworkConfigPath, cfg.Name), []byte(cfg.Contents), 0o644)
		if err != nil {
			return err
		}
	}

	// Generate .netdev files.
	for _, cfg := range generateNetdevFileContents(*networkCfg) {
		err := os.WriteFile(filepath.Join(SystemdNetworkConfigPath, cfg.Name), []byte(cfg.Contents), 0o644)
		if err != nil {
			return err
		}
	}

	// Generate .network files.
	for _, cfg := range generateNetworkFileContents(*networkCfg) {
		err := os.WriteFile(filepath.Join(SystemdNetworkConfigPath, cfg.Name), []byte(cfg.Contents), 0o644)
		if err != nil {
			return err
		}
	}

	// Generate systemd-timesyncd configuration if any timeservers are defined.
	ntpCfg := ""
	if networkCfg.NTP != nil {
		ntpCfg = generateTimesyncContents(*networkCfg.NTP)

		if ntpCfg != "" {
			err := os.WriteFile(SystemdTimesyncConfigFile, []byte(ntpCfg), 0o644)
			if err != nil {
				return err
			}
		}
	}

	// If there's no NTP configuration, remove the old config file that might exist.
	if networkCfg.NTP == nil || ntpCfg == "" {
		_ = os.Remove(SystemdTimesyncConfigFile)
	}

	return nil
}

// waitForUdevInterfaceRename waits up to a provided timeout for udev to pickup and process
// the renaming of interfaces. At system startup there's a small race between udev being fully
// started and our reconfiguring of the network, so we poll in a loop until we see the kernel
// has been notified of the rename.
func waitForUdevInterfaceRename(ctx context.Context, timeout time.Duration) error {
	endTime := time.Now().Add(timeout)

	for {
		if time.Now().After(endTime) {
			return errors.New("timed out waiting for udev to rename interface(s)")
		}

		// Trigger udev rule update to pickup device names.
		_, err := subprocess.RunCommandContext(ctx, "udevadm", "trigger", "--action=add")
		if err != nil {
			return err
		}

		// Wait for udev to be done processing the events.
		_, err = subprocess.RunCommandContext(ctx, "udevadm", "settle")
		if err != nil {
			return err
		}

		// Check if the kernel has noticed the renaming of (at least) one interface to
		// the expected "en<MAC address>" format.
		_, err = subprocess.RunCommandContext(ctx, "journalctl", "-b", "-t", "kernel", "-g", "en[[:xdigit:]]{12}: renamed from ")
		if err == nil {
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// waitForNetworkOnline waits up to a provided timeout for configured network interfaces,
// bonds, and vlans to configure their IP address(es) and come online.
func waitForNetworkOnline(ctx context.Context, networkCfg *api.SystemNetworkConfig, timeout time.Duration) error {
	isOnline := func(name string) (bool, bool) {
		output, err := subprocess.RunCommandContext(ctx, "networkctl", "status", name)
		if err != nil {
			return false, true
		}

		return strings.Contains(output, "Online state: online"), strings.Contains(output, "Required For Online: yes")
	}

	hasAtLeastOneConfiguredIP := func(name string) bool {
		ipAddressRegex := regexp.MustCompile(`inet6? (.+)/\d+ `)

		output, err := subprocess.RunCommandContext(ctx, "ip", "address", "show", name)
		if err != nil {
			return false
		}

		numIPs := 0
		matches := ipAddressRegex.FindAllStringSubmatch(output, -1)

		for _, addr := range matches {
			// Don't count link-local addresses.
			if strings.HasPrefix(addr[1], "169.254.") || strings.HasPrefix(addr[1], "fe80:") {
				continue
			}

			numIPs++
		}

		return numIPs > 0
	}

	endTime := time.Now().Add(timeout)

	devicesToCheck := []string{}

	for _, i := range networkCfg.Interfaces {
		if len(i.Addresses) == 0 {
			continue
		}

		devicesToCheck = append(devicesToCheck, i.Name)
	}

	for _, b := range networkCfg.Bonds {
		if len(b.Addresses) == 0 {
			continue
		}

		devicesToCheck = append(devicesToCheck, b.Name)
	}

	for _, v := range networkCfg.VLANs {
		if len(v.Addresses) == 0 {
			continue
		}

		devicesToCheck = append(devicesToCheck, v.Name)
	}

	for {
		if time.Now().After(endTime) {
			return errors.New("timed out waiting for network to come online")
		}

		allDevicesOnline := true
		for _, name := range devicesToCheck {
			online, requiredOnline := isOnline(name)
			if !requiredOnline {
				continue
			}

			if !online || !hasAtLeastOneConfiguredIP(name) {
				allDevicesOnline = false

				break
			}
		}

		if allDevicesOnline {
			return nil
		}

		time.Sleep(500 * time.Millisecond)
	}
}

// generateLinkFileContents generates the contents of systemd.link files. Returns an array of ConfigFile structs.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.link.html
func generateLinkFileContents(networkCfg api.SystemNetworkConfig) []networkdConfigFile {
	ret := []networkdConfigFile{}

	for _, i := range networkCfg.Interfaces {
		strippedHwaddr := strings.ToLower(strings.ReplaceAll(i.Hwaddr, ":", ""))
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("00-en%s.link", strippedHwaddr),
			Contents: fmt.Sprintf(`[Match]
PermanentMACAddress=%s

[Link]
MACAddressPolicy=random
NamePolicy=
Name=en%s
`, i.Hwaddr, strippedHwaddr),
		})
	}

	for _, b := range networkCfg.Bonds {
		for _, member := range b.Members {
			strippedHwaddr := strings.ToLower(strings.ReplaceAll(member, ":", ""))
			ret = append(ret, networkdConfigFile{
				Name: fmt.Sprintf("01-en%s.link", strippedHwaddr),
				Contents: fmt.Sprintf(`[Match]
PermanentMACAddress=%s

[Link]
NamePolicy=
Name=en%s
`, member, strippedHwaddr),
			})
		}
	}

	return ret
}

// generateNetdevFileContents generates the contents of systemd.netdev files. Returns an array of networkdConfigFile structs.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.netdev.html
func generateNetdevFileContents(networkCfg api.SystemNetworkConfig) []networkdConfigFile {
	ret := []networkdConfigFile{}

	// Create bridge and veth devices for each interface.
	for _, i := range networkCfg.Interfaces {
		mtuString := ""
		if i.MTU != 0 {
			mtuString = fmt.Sprintf("MTUBytes=%d", i.MTU)
		}

		// Bridge.
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("10-br%s.netdev", i.Name),
			Contents: fmt.Sprintf(`[NetDev]
Name=br%s
Kind=bridge
%s

[Bridge]
VLANFiltering=true
`, i.Name, mtuString),
		})

		// veth.
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("10-%s.netdev", i.Name),
			Contents: fmt.Sprintf(`[NetDev]
Name=%s
Kind=veth
MACAddress=%s
%s

[Peer]
Name=vt%s
`, i.Name, i.Hwaddr, mtuString, i.Name),
		})
	}

	// Create bond, bridge, and veth devices for each bond.
	for _, b := range networkCfg.Bonds {
		mtuString := ""
		if b.MTU != 0 {
			mtuString = fmt.Sprintf("MTUBytes=%d", b.MTU)
		}

		// Bond.
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("11-bn%s.netdev", b.Name),
			Contents: fmt.Sprintf(`[NetDev]
Name=bn%s
Kind=bond
%s

[Bond]
Mode=%s
`, b.Name, mtuString, b.Mode),
		})

		// Bridge.
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("11-br%s.netdev", b.Name),
			Contents: fmt.Sprintf(`[NetDev]
Name=br%s
Kind=bridge
%s

[Bridge]
VLANFiltering=true
`, b.Name, mtuString),
		})

		// veth.
		bondMacAddr := b.Hwaddr
		if bondMacAddr == "" {
			bondMacAddr = b.Members[0]
		}

		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("11-%s.netdev", b.Name),
			Contents: fmt.Sprintf(`[NetDev]
Name=%s
Kind=veth
MACAddress=%s
%s

[Peer]
Name=vt%s
`, b.Name, bondMacAddr, mtuString, b.Name),
		})
	}

	// Create vlans.
	for _, v := range networkCfg.VLANs {
		mtuString := ""
		if v.MTU != 0 {
			mtuString = fmt.Sprintf("MTUBytes=%d", v.MTU)
		}

		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("12-%s.netdev", v.Name),
			Contents: fmt.Sprintf(`[NetDev]
Name=%s
Kind=vlan
%s

[VLAN]
Id=%d
`, v.Name, mtuString, v.ID),
		})
	}

	return ret
}

// generateNetworkFileContents generates the contents of systemd.network files. Returns an array of networkdConfigFile structs.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.network.html
func generateNetworkFileContents(networkCfg api.SystemNetworkConfig) []networkdConfigFile {
	ret := []networkdConfigFile{}

	// Create networks for each interface and its bridge.
	for _, i := range networkCfg.Interfaces {
		// User side of veth device.
		cfgString := fmt.Sprintf(`[Match]
Name=%s

[Link]
%s

[DHCP]
ClientIdentifier=mac
RouteMetric=100
UseMTU=true

[Network]
%s`, i.Name, generateLinkSectionContents(i.Addresses, i.RequiredForOnline), generateNetworkSectionContents(i.Name, networkCfg.VLANs, networkCfg.DNS, networkCfg.NTP))

		cfgString += processAddresses(i.Addresses)

		if len(i.Routes) > 0 {
			cfgString += processRoutes(i.Routes)
		}

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("20-%s.network", i.Name),
			Contents: cfgString,
		})

		// Bridge side of veth device.
		cfgString = fmt.Sprintf(`[Match]
Name=vt%s

[Network]
Bridge=br%s
`, i.Name, i.Name)

		cfgString += generateBridgeVLANContents(i.Name, i.VLAN, i.VLANTags, networkCfg.VLANs)

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("20-vt%s.network", i.Name),
			Contents: cfgString,
		})

		// Add underlying interface to bridge.
		strippedHwaddr := strings.ToLower(strings.ReplaceAll(i.Hwaddr, ":", ""))
		cfgString = fmt.Sprintf(`[Match]
Name=en%s

[Network]
LLDP=%s
EmitLLDP=%s
Bridge=br%s
`, strippedHwaddr, strconv.FormatBool(i.LLDP), strconv.FormatBool(i.LLDP), i.Name)

		cfgString += generateBridgeVLANContents(i.Name, i.VLAN, i.VLANTags, networkCfg.VLANs)

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("20-en%s.network", strippedHwaddr),
			Contents: cfgString,
		})

		// Bridge.
		cfgString = fmt.Sprintf(`[Match]
Name=br%s

[Network]
LinkLocalAddressing=no
ConfigureWithoutCarrier=yes
`, i.Name)

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("20-br%s.network", i.Name),
			Contents: cfgString,
		})
	}

	// Create networks for each bond, its member(s), and its bridge.
	for _, b := range networkCfg.Bonds {
		// User side of veth device.
		cfgString := fmt.Sprintf(`[Match]
Name=%s

[Link]
%s

[DHCP]
ClientIdentifier=mac
RouteMetric=100
UseMTU=true

[Network]
%s`, b.Name, generateLinkSectionContents(b.Addresses, b.RequiredForOnline), generateNetworkSectionContents(b.Name, networkCfg.VLANs, networkCfg.DNS, networkCfg.NTP))

		cfgString += processAddresses(b.Addresses)

		if len(b.Routes) > 0 {
			cfgString += processRoutes(b.Routes)
		}

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("21-%s.network", b.Name),
			Contents: cfgString,
		})

		// Bridge side of veth device.
		cfgString = fmt.Sprintf(`[Match]
Name=vt%s

[Network]
Bridge=br%s
`, b.Name, b.Name)

		cfgString += generateBridgeVLANContents(b.Name, b.VLAN, b.VLANTags, networkCfg.VLANs)

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("21-vt%s.network", b.Name),
			Contents: cfgString,
		})

		// Add bond to bridge.
		cfgString = fmt.Sprintf(`[Match]
Name=bn%s

[Network]
LinkLocalAddressing=no
ConfigureWithoutCarrier=yes
Bridge=br%s
`, b.Name, b.Name)

		cfgString += generateBridgeVLANContents(b.Name, b.VLAN, b.VLANTags, networkCfg.VLANs)

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("21-bn%s.network", b.Name),
			Contents: cfgString,
		})

		// Bridge.
		cfgString = fmt.Sprintf(`[Match]
Name=br%s

[Network]
LinkLocalAddressing=no
ConfigureWithoutCarrier=yes
`, b.Name)

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("21-br%s.network", b.Name),
			Contents: cfgString,
		})

		// Bond members.
		for index, member := range b.Members {
			memberStrippedHwaddr := strings.ToLower(strings.ReplaceAll(member, ":", ""))

			ret = append(ret, networkdConfigFile{
				Name: fmt.Sprintf("21-bn%s-dev%d.network", b.Name, index),
				Contents: fmt.Sprintf(`[Match]
Name=en%s

[Network]
LLDP=%s
EmitLLDP=%s
Bond=bn%s
`, memberStrippedHwaddr, strconv.FormatBool(b.LLDP), strconv.FormatBool(b.LLDP), b.Name),
			})
		}
	}

	// Create network for each VLAN.
	for _, v := range networkCfg.VLANs {
		cfgString := fmt.Sprintf(`[Match]
Name=%s

[Link]
%s

[DHCP]
ClientIdentifier=mac
RouteMetric=100
UseMTU=true

[Network]
%s`, v.Name, generateLinkSectionContents(v.Addresses, v.RequiredForOnline), generateNetworkSectionContents(v.Name, nil, networkCfg.DNS, networkCfg.NTP))

		cfgString += processAddresses(v.Addresses)

		if len(v.Routes) > 0 {
			cfgString += processRoutes(v.Routes)
		}

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("22-%s.network", v.Name),
			Contents: cfgString,
		})
	}

	return ret
}

func processAddresses(addresses []string) string {
	ret := ""
	if len(addresses) != 0 {
		ret += "LinkLocalAddressing=ipv6\n"
	} else {
		ret += "LinkLocalAddressing=no\n"
		ret += "ConfigureWithoutCarrier=yes\n"
	}

	hasDHCP4 := false
	hasDHCP6 := false
	acceptIPv6RA := false
	for _, addr := range addresses {
		switch addr {
		case "dhcp4": //nolint:goconst
			hasDHCP4 = true
		case "dhcp6":
			hasDHCP6 = true
		case "slaac": //nolint:goconst
			acceptIPv6RA = true

		default:
			ret += fmt.Sprintf("Address=%s\n", addr)
		}
	}

	if acceptIPv6RA {
		ret += "IPv6AcceptRA=true\n"
	} else {
		ret += "IPv6AcceptRA=false\n"
	}

	if hasDHCP4 && hasDHCP6 { //nolint:gocritic
		ret += "DHCP=yes\n"
	} else if hasDHCP4 {
		ret += "DHCP=ipv4\n"
	} else if hasDHCP6 {
		ret += "DHCP=ipv6\n"
	}

	return ret
}

func processRoutes(routes []api.SystemNetworkRoute) string {
	ret := ""

	for _, route := range routes {
		ret += "\n[Route]\n"

		switch route.Via {
		case "dhcp4":
			ret += "Gateway=_dhcp4\n"
		case "slaac":
			ret += "Gateway=_ipv6ra\n"
		default:
			ret += fmt.Sprintf("Gateway=%s\n", route.Via)
		}

		ret += fmt.Sprintf("Destination=%s\n", route.To)
	}

	return ret
}

func generateNetworkSectionContents(name string, vlans []api.SystemNetworkVLAN, dns *api.SystemNetworkDNS, ntp *api.SystemNetworkNTP) string {
	ret := ""

	// Add any matching VLANs to the config.
	for _, v := range vlans {
		if v.Parent == name {
			ret += fmt.Sprintf("VLAN=%s\n", v.Name)
		}
	}

	// If there are search domains or name servers, add those to the config.
	if dns != nil {
		if len(dns.SearchDomains) > 0 {
			ret += fmt.Sprintf("Domains=%s\n", strings.Join(dns.SearchDomains, " "))
		}

		for _, ns := range dns.Nameservers {
			ret += fmt.Sprintf("DNS=%s\n", ns)
		}
	}

	// If there are time servers defined, add them to the config.
	if ntp != nil {
		for _, ts := range ntp.Timeservers {
			ret += fmt.Sprintf("NTP=%s\n", ts)
		}
	}

	return ret
}

func generateTimesyncContents(ntp api.SystemNetworkNTP) string {
	if len(ntp.Timeservers) == 0 {
		return ""
	}

	return "[Time]\nFallbackNTP=" + strings.Join(ntp.Timeservers, " ") + "\n"
}

func generateBridgeVLANContents(bridgeName string, specificVLAN int, additionalVLANTags []int, vlans []api.SystemNetworkVLAN) string {
	vlanTags := []int{}

	// Add specific VLAN tag, if configured.
	if specificVLAN != 0 {
		vlanTags = append(vlanTags, specificVLAN)
	}

	// Add any additional VLAN tags.
	vlanTags = append(vlanTags, additionalVLANTags...)

	// Grab any relevant tags for this bridge from VLAN definitions.
	for _, vlan := range vlans {
		if vlan.Parent == bridgeName {
			vlanTags = append(vlanTags, vlan.ID)
		}
	}

	// Sort and remove any duplicate tags.
	slices.Sort(vlanTags)
	vlanTags = slices.Compact(vlanTags)

	ret := ""

	if len(vlanTags) > 0 {
		if specificVLAN != 0 {
			ret += "\n[BridgeVLAN]\n"
			ret += fmt.Sprintf("PVID=%d\n", specificVLAN)
			ret += fmt.Sprintf("EgressUntagged=%d\n", specificVLAN)
		}

		for _, tag := range vlanTags {
			ret += "\n[BridgeVLAN]\n"
			ret += fmt.Sprintf("VLAN=%d\n", tag)
		}
	}

	return ret
}

func generateLinkSectionContents(addresses []string, requiredForOnline string) string {
	if len(addresses) == 0 || requiredForOnline == "no" {
		return "RequiredForOnline=no"
	}

	if requiredForOnline == "" {
		requiredForOnline = "any"
	}

	return "RequiredForOnline=yes\nRequiredFamilyForOnline=" + requiredForOnline
}
