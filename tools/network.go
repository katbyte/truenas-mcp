package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/katbyte/truenas-mcp/sdk/truenas"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// How the NAS is reached: its network interfaces, and the name, gateway and name servers it goes by.

func registerNetworkTools(r *registry) {
	type interfaceRow struct {
		Name        string   `json:"name"                  jsonschema:"the interface, as the system names it: enp1s0, br0, bond0, vlan10"`
		Type        string   `json:"type"                  jsonschema:"PHYSICAL, BRIDGE, LINK_AGGREGATION or VLAN"`
		Description string   `json:"description,omitempty" jsonschema:"what it is for, in its owner's words"`
		LinkState   string   `json:"link_state,omitempty"  jsonschema:"LINK_STATE_UP when a cable or link is live, LINK_STATE_DOWN when not"`
		Addresses   []string `json:"addresses"             jsonschema:"the addresses it has now, as address/prefix, those DHCP gave included"`
		DHCP        bool     `json:"dhcp"                  jsonschema:"whether it asks DHCP for its IPv4 address"`
		IPv6Auto    bool     `json:"ipv6_auto"             jsonschema:"whether it configures its IPv6 address by itself"`
		MTU         int      `json:"mtu"                   jsonschema:"the largest packet it sends, in bytes"`
		MAC         string   `json:"mac,omitempty"         jsonschema:"its hardware address"`
		Members     []string `json:"members,omitempty"     jsonschema:"the interfaces a bridge or a link aggregation is made of"`
		VLANParent  string   `json:"vlan_parent,omitempty" jsonschema:"the interface a VLAN rides on"`
		VLANTag     int      `json:"vlan_tag,omitempty"    jsonschema:"a VLAN's number"`
	}
	type interfaceListIn struct {
		pageIn
	}
	type interfaceListOut struct {
		Interfaces []interfaceRow `json:"interfaces"`
		pageOut
	}
	add(r, readTool, &mcp.Tool{
		Name:        "interface_list",
		Description: "List the NAS's network interfaces: each one's type, whether its link is up, the addresses it has now and whether DHCP gave them, and what a bridge, link aggregation or VLAN is made of.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in interfaceListIn) (*mcp.CallToolResult, interfaceListOut, error) {
		found, err := r.typed.Interface.Query(ctx, nil, truenas.InterfaceQueryOptions{OrderBy: []string{"name"}})
		if err != nil {
			return nil, interfaceListOut{}, fmt.Errorf("listing the network interfaces: %w", err)
		}

		rows := make([]interfaceRow, 0, len(found.Answer))
		for i := range found.Answer {
			n := &found.Answer[i]
			row := interfaceRow{Name: n.Name, Type: n.Type, Description: n.Description, DHCP: n.Ipv4Dhcp, IPv6Auto: n.Ipv6Auto, MTU: n.Mtu, Members: append(n.BridgeMembers, n.LagPorts...), VLANParent: n.VlanParentInterface, VLANTag: n.VlanTag, Addresses: []string{}}
			// what it has now, which the state says: an address DHCP gave is in no setting
			if s := n.State; s != nil {
				row.LinkState, row.MAC = s.LinkState, s.LinkAddress
				if s.Mtu > 0 {
					row.MTU = s.Mtu
				}
				for _, a := range s.Aliases {
					if a.Type == "INET" || a.Type == "INET6" {
						row.Addresses = append(row.Addresses, fmt.Sprintf("%s/%v", a.Address, a.Netmask))
					}
				}
			}
			rows = append(rows, row)
		}
		out := interfaceListOut{}
		out.Interfaces, out.pageOut = pageRows(rows, in.pageIn)

		return nil, out, nil
	})

	type networkConfigurationOut struct {
		Hostname    string   `json:"hostname"               jsonschema:"the name the NAS goes by on the network"`
		Domain      string   `json:"domain,omitempty"       jsonschema:"the domain it is in"`
		IPv4Gateway string   `json:"ipv4_gateway,omitempty" jsonschema:"the router it sends IPv4 beyond its own networks to, as set or as DHCP gave it"`
		IPv6Gateway string   `json:"ipv6_gateway,omitempty" jsonschema:"the router it sends IPv6 beyond its own networks to"`
		Nameservers []string `json:"nameservers"            jsonschema:"the name servers it asks, as set or as DHCP gave them, in the order it asks them"`
		Hosts       []string `json:"hosts"                  jsonschema:"names it knows without asking, as lines of a hosts file"`
	}
	add(r, readTool, &mcp.Tool{
		Name:        "network_configuration_get",
		Description: "Say how the NAS is named and routed on the network: its hostname and domain, its gateways, and the name servers it asks, those DHCP gave included.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, networkConfigurationOut, error) {
		found, err := r.typed.NetworkConfiguration.Config(ctx)
		if err != nil {
			return nil, networkConfigurationOut{}, fmt.Errorf("reading the network settings: %w", err)
		}
		c := found.Answer
		if c == nil {
			return nil, networkConfigurationOut{}, errors.New("the NAS answered no network settings")
		}

		out := networkConfigurationOut{Hostname: c.Hostname, Domain: c.Domain, IPv4Gateway: c.Ipv4gateway, IPv6Gateway: c.Ipv6gateway, Nameservers: []string{}, Hosts: orEmpty(c.Hosts)}
		servers := []string{c.Nameserver1, c.Nameserver2, c.Nameserver3}
		// what is in force, which the state says: a gateway or name server DHCP gave is in no setting
		if s := c.State; s != nil {
			out.IPv4Gateway, out.IPv6Gateway = orNothing(s.Ipv4gateway, out.IPv4Gateway), orNothing(s.Ipv6gateway, out.IPv6Gateway)
			if s.Nameserver1+s.Nameserver2+s.Nameserver3 != "" {
				servers = []string{s.Nameserver1, s.Nameserver2, s.Nameserver3}
			}
		}
		for _, server := range servers {
			if server != "" {
				out.Nameservers = append(out.Nameservers, server)
			}
		}

		return nil, out, nil
	})
}
