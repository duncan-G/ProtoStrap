package docker

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

func TestManageSwarmInitsTheSwarmAndOverlay(t *testing.T) {
	d := newDaemon(t)
	inited := false
	var initBody, netBody map[string]any
	d.on("GET /swarm", func(w http.ResponseWriter, r *http.Request) {
		if !inited {
			w.WriteHeader(503)
			w.Write([]byte(`{"message":"This node is not a swarm manager."}`))
			return
		}
		writeJSON(w, map[string]any{"ID": "swarm-1", "JoinTokens": map[string]string{"Worker": "SWMTKN-w", "Manager": "SWMTKN-m"}})
	})
	d.on("POST /swarm/init", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&initBody)
		inited = true
		writeJSON(w, "node-1")
	})
	d.on("GET /networks/protostrap", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"network protostrap not found"}`))
	})
	d.on("POST /networks/create", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&netBody)
		writeJSON(w, map[string]string{"Id": "net-1"})
	})
	d.on("DELETE /networks/ingress", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })

	c := newClient(t, d, nil)
	j, err := c.ManageSwarm(t.Context(), SwarmSettings{Advertise: "10.20.1.10", Network: "protostrap", Subnet: "10.200.0.0/22", AddressPool: "10.201.0.0/16"})
	if err != nil {
		t.Fatal(err)
	}
	if j.SwarmID != "swarm-1" || j.Token != "SWMTKN-w" || j.Manager != "10.20.1.10:2377" {
		t.Errorf("join %+v", j)
	}
	if initBody["AdvertiseAddr"] != "10.20.1.10" || initBody["DefaultAddrPool"].([]any)[0] != "10.201.0.0/16" {
		t.Errorf("init %v", initBody)
	}
	ipam := netBody["IPAM"].(map[string]any)["Config"].([]any)[0].(map[string]any)
	if netBody["Name"] != "protostrap" || netBody["Driver"] != "overlay" || netBody["Attachable"] != true || ipam["Subnet"] != "10.200.0.0/22" {
		t.Errorf("network %v", netBody)
	}
	if !slices.Contains(d.requests, "DELETE /networks/ingress") {
		t.Errorf("Swarm's own ingress network wasn't removed: %v", d.requests)
	}
}

func TestManageSwarmLeavesAnExistingSwarmAlone(t *testing.T) {
	d := newDaemon(t)
	d.on("GET /swarm", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ID": "swarm-1", "JoinTokens": map[string]string{"Worker": "SWMTKN-w"}})
	})
	d.on("GET /networks/protostrap", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]string{"Id": "net-1"}) })
	d.on("DELETE /networks/ingress", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"network ingress not found"}`))
	})
	c := newClient(t, d, nil)
	if _, err := c.ManageSwarm(t.Context(), SwarmSettings{Advertise: "10.20.1.10", Network: "protostrap", Subnet: "10.200.0.0/22"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range d.requests {
		if r == "POST /swarm/init" || r == "POST /networks/create" {
			t.Errorf("%s on an existing swarm", r)
		}
	}
}

func TestNodes(t *testing.T) {
	d := newDaemon(t)
	d.on("GET /nodes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, []map[string]any{
			{"ID": "n1", "Status": map[string]string{"State": "ready", "Addr": "10.20.1.10"}},
			{"ID": "n2", "Status": map[string]string{"State": "down", "Addr": "10.20.16.5"}},
		})
	})
	removed := ""
	d.on("DELETE /nodes/*", func(w http.ResponseWriter, r *http.Request) {
		removed = r.URL.Path + "?" + r.URL.RawQuery
		w.WriteHeader(200)
	})
	c := newClient(t, d, nil)
	nodes, err := c.Nodes(t.Context())
	if err != nil || len(nodes) != 2 || nodes[1].ID != "n2" || nodes[1].State != "down" || nodes[1].Addr != "10.20.16.5" {
		t.Errorf("nodes %+v %v", nodes, err)
	}
	if err := c.RemoveNode(t.Context(), "n2"); err != nil {
		t.Fatal(err)
	}
	if removed != "/v1.47/nodes/n2?force=true" {
		t.Errorf("removed %q", removed)
	}
}
