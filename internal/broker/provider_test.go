package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fullCloud implements enough of the smol cloud contract to exercise the whole
// masterKeyProvider surface (artifacts, create+start, list, stop).
func fullCloud(master string) *httptest.Server {
	var machines []map[string]any
	seq := 0
	mux := http.NewServeMux()
	auth := func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+master }
	mux.HandleFunc("/v1/artifacts", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"reference": "tenants/T/" + r.Header.Get("X-Smol-Owner") + ":v1"})
	})
	mux.HandleFunc("/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(machines)
			return
		}
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		seq++
		m["id"] = "mach-x"
		m["state"] = "stopped"
		m["resources"] = map[string]any{"cpus": 2.0, "memoryMb": 1024.0, "diskGb": 5.0}
		machines = append(machines, m)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/v1/machines/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(401)
			return
		}
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/machines/"), "/")[0]
		action := strings.TrimPrefix(r.URL.Path, "/v1/machines/"+id+"/")
		for _, m := range machines {
			if m["id"] == id {
				if action == "start" {
					m["state"] = "started"
				} else if action == "stop" {
					m["state"] = "stopped"
				}
				_ = json.NewEncoder(w).Encode(m)
				return
			}
		}
		w.WriteHeader(404)
	})
	return httptest.NewServer(mux)
}

func TestMasterKeyProvider_FullSurface(t *testing.T) {
	srv := fullCloud("smk_test")
	defer srv.Close()
	p := newMasterKeyProvider(srv.URL, "smk_test", "PILOT_OWNER")
	ctx := context.Background()

	// Push from an image → creates + starts (state started, owner-tagged).
	resp, err := p.Push(ctx, "alice", PushSpec{Name: "web", Image: "alpine:3.20", Net: true}, nil)
	if err != nil || resp.Status/100 != 2 {
		t.Fatalf("image push: %d %v %s", resp.Status, err, resp.Body)
	}
	var m map[string]any
	_ = json.Unmarshal(resp.Body, &m)
	if m["state"] != "started" {
		t.Fatalf("push should start the machine, got %v", m["state"])
	}
	if env, _ := m["env"].(map[string]any); env["PILOT_OWNER"] != "alice" {
		t.Fatalf("owner tag missing: %v", m["env"])
	}

	// Push from an artifact → uploads then creates.
	if r2, err := p.Push(ctx, "bob", PushSpec{Name: "job"}, []byte("artifactbytes")); err != nil || r2.Status/100 != 2 {
		t.Fatalf("artifact push: %d %v", r2.Status, err)
	}

	// List filters to the owner.
	lr, err := p.List(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	var mine []map[string]any
	_ = json.Unmarshal(lr.Body, &mine)
	if len(mine) != 1 {
		t.Fatalf("alice should see only her machine, got %d", len(mine))
	}

	// AllOwned returns everything with resources for the meter.
	all, err := p.AllOwned(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("AllOwned: %d %v", len(all), err)
	}
	if !all[0].Running() || all[0].Cpus != 2 {
		t.Fatalf("AllOwned resources wrong: %+v", all[0])
	}

	// Stop works.
	if err := p.Stop(ctx, all[0].ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestMasterKeyProvider_PushNeedsSourceAndName(t *testing.T) {
	srv := fullCloud("k")
	defer srv.Close()
	p := newMasterKeyProvider(srv.URL, "k", "PILOT_OWNER")
	if _, err := p.Push(context.Background(), "x", PushSpec{}, nil); err == nil {
		t.Fatal("push with neither artifact nor image must error")
	}
	if got := ownerName("x", ""); got != "smol-x-vm" {
		t.Fatalf("default name: %q", got)
	}
}
