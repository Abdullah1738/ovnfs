package node

import (
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/stencil-hq/ovnfs/mesh"
)

// Address ranges a node ranks its own ways by.
var (
	tailscaleV4 = netip.MustParsePrefix("100.64.0.0/10")
	tailscaleV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// maxDirectWays caps the interface addresses a node advertises, leaving room
// for the relay way within a roster entry's eight.
const maxDirectWays = 7

// hostOnly are name prefixes of interfaces that exist only inside this host:
// container and VM bridges and veth pairs. No other machine reaches their
// addresses, so advertising them only delays a peer's race.
var hostOnly = []string{"docker", "br-", "veth", "virbr", "vmnet", "vboxnet", "bridge", "vmon", "vnet", "cni", "flannel", "cali", "cilium", "weave", "kube", "lxc", "lxd", "podman"}

// hostOnlyInterface reports whether name is such an interface.
func hostOnlyInterface(name string) bool {
	return slices.ContainsFunc(hostOnly, func(p string) bool { return strings.HasPrefix(name, p) })
}

// privateNetwork reports whether ip is on a private overlay network
// (Tailscale's ranges): such ways are raced first.
func privateNetwork(ip netip.Addr) bool {
	ip = ip.Unmap()
	return tailscaleV4.Contains(ip) || tailscaleV6.Contains(ip)
}

// Ways are the node's own advertised ways: its interface addresses on the
// direct port (Tailscale first, then private ranges, then the rest; no
// loopback, link-local or host-only bridges; at most maxDirectWays), the
// address the relay observed (on the direct port), then the relay itself when
// one is configured.
func (n *Node) Ways() []mesh.Way {
	var ways []mesh.Way
	if n.udp != nil {
		type ranked struct {
			rank int
			ip   netip.Addr
		}
		var ips []ranked
		seen := make(map[netip.Addr]bool)
		ifaces, _ := net.Interfaces()
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || hostOnlyInterface(iface.Name) {
				continue
			}
			addrs, _ := iface.Addrs()
			tailscale := strings.HasPrefix(iface.Name, "tailscale")
			for _, a := range addrs {
				ipn, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				ip, ok := netip.AddrFromSlice(ipn.IP)
				if !ok || !n.advertisable(ip.Unmap()) || seen[ip.Unmap()] {
					continue
				}
				ip = ip.Unmap()
				seen[ip] = true
				rank := 2
				switch {
				case tailscale || privateNetwork(ip):
					rank = 0
				case ip.IsPrivate():
					rank = 1
				}
				ips = append(ips, ranked{rank, ip})
			}
		}
		slices.SortStableFunc(ips, func(a, b ranked) int {
			if a.rank != b.rank {
				return a.rank - b.rank
			}
			if a.ip.Is4() != b.ip.Is4() {
				if a.ip.Is4() {
					return -1
				}
				return 1
			}
			return 0
		})
		for _, r := range ips {
			ways = append(ways, mesh.WayOf(netip.AddrPortFrom(r.ip, uint16(n.port))))
		}
		// The relay sees the TCP source of its link; only its address
		// helps a UDP peer, on this node's own port.
		if n.relay != nil {
			if obs, ok := n.relay.Observed(); ok && obs.Port() != 0 {
				ip := obs.Addr().Unmap()
				if n.advertisable(ip) && !seen[ip] {
					ways = append(ways, mesh.WayOf(netip.AddrPortFrom(ip, uint16(n.port))))
				}
			}
		}
		ways = ways[:min(len(ways), maxDirectWays)]
	}
	if n.relay != nil {
		ways = append(ways, mesh.Way{Relay: true})
	}
	return ways
}

// advertisable reports whether a peer could reach this node at ip.
func (n *Node) advertisable(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	return ip.Is4() || n.v6
}
