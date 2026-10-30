package docker

import (
	"context"
	"net/http"
	"net/url"

	"github.com/duncan-G/protostrap/internal/api"
)

// SwarmSettings are how data-1 makes the swarm (docs/network.md#dockers-ranges).
type SwarmSettings struct {
	// Advertise is data-1's fixed address.
	Advertise string
	// Network is the overlay's name, protostrap.
	Network string
	// Subnet is the overlay's range, 10.200.0.0/22.
	Subnet string
	// AddressPool is the range for any other swarm network, 10.201.0.0/16.
	AddressPool string
}

// SwarmNode is a VM in the swarm.
type SwarmNode struct {
	ID    string
	Addr  string
	State string
}

// ManageSwarm makes this daemon the swarm's manager, if it isn't one, with the overlay
// network every replica joins, and removes Swarm's own ingress network, so no port can be
// published on every VM by mistake. It returns what workers need to join.
func (c *Client) ManageSwarm(ctx context.Context, s SwarmSettings) (api.Join, error) {
	var info struct {
		ID         string
		JoinTokens struct{ Worker string }
	}
	err := c.call(ctx, "swarm inspect", http.MethodGet, "/swarm", nil, nil, &info)
	if statusIs(err, http.StatusServiceUnavailable, http.StatusNotAcceptable) {
		body := map[string]any{"ListenAddr": "0.0.0.0:2377", "AdvertiseAddr": s.Advertise}
		if s.AddressPool != "" {
			body["DefaultAddrPool"] = []string{s.AddressPool}
			body["SubnetSize"] = 24
		}
		if err := c.call(ctx, "swarm init", http.MethodPost, "/swarm/init", nil, body, nil); err != nil {
			return api.Join{}, err
		}
		err = c.call(ctx, "swarm inspect", http.MethodGet, "/swarm", nil, nil, &info)
	}
	if err != nil {
		return api.Join{}, err
	}
	err = c.call(ctx, "network inspect", http.MethodGet, "/networks/"+url.PathEscape(s.Network), nil, nil, nil)
	if statusIs(err, http.StatusNotFound) {
		body := map[string]any{
			"Name": s.Network, "Driver": "overlay", "Attachable": true, "Scope": "swarm", "CheckDuplicate": true,
			"IPAM": map[string]any{"Driver": "default", "Config": []map[string]string{{"Subnet": s.Subnet}}},
		}
		err = c.call(ctx, "network create", http.MethodPost, "/networks/create", nil, body, nil)
	}
	if err != nil {
		return api.Join{}, err
	}
	if err := c.call(ctx, "network remove", http.MethodDelete, "/networks/ingress", nil, nil, nil, http.StatusNotFound); err != nil {
		return api.Join{}, err
	}
	return api.Join{SwarmID: info.ID, Token: info.JoinTokens.Worker, Manager: s.Advertise + ":2377"}, nil
}

// Nodes lists the swarm's nodes.
func (c *Client) Nodes(ctx context.Context) ([]SwarmNode, error) {
	var raw []struct {
		ID     string
		Status struct{ State, Addr string }
	}
	if err := c.call(ctx, "node list", http.MethodGet, "/nodes", nil, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]SwarmNode, 0, len(raw))
	for _, n := range raw {
		out = append(out, SwarmNode{ID: n.ID, Addr: n.Status.Addr, State: n.Status.State})
	}
	return out, nil
}

// RemoveNode removes a node that is gone.
func (c *Client) RemoveNode(ctx context.Context, id string) error {
	return c.call(ctx, "node remove", http.MethodDelete, "/nodes/"+url.PathEscape(id), url.Values{"force": {"true"}}, nil, nil, http.StatusNotFound)
}
