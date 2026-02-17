// Copyright 2026 Matheus Pimenta.
// SPDX-License-Identifier: AGPL-3.0

package cloudflare_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	cfgo "github.com/cloudflare/cloudflare-go/v6"

	"github.com/matheuscscp/cloudflare-gateway-controller/internal/cloudflare"
)

const testAccountID = "test-account-id"

func newTestClient(t *testing.T, handler http.Handler) cloudflare.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := cloudflare.NewClient(cloudflare.ClientConfig{
		APIToken:  "test-token",
		AccountID: testAccountID,
		BaseURL:   server.URL,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// envelope is the standard Cloudflare API response wrapper.
func envelope(result any) map[string]any {
	return map[string]any{
		"success":  true,
		"errors":   []any{},
		"messages": []any{},
		"result":   result,
	}
}

// paginatedEnvelope wraps a list result with pagination info.
func paginatedEnvelope(result any) map[string]any {
	return map[string]any{
		"success":  true,
		"errors":   []any{},
		"messages": []any{},
		"result":   result,
		"result_info": map[string]any{
			"page":     1,
			"per_page": 20,
		},
	}
}

func emptyPage() map[string]any {
	return paginatedEnvelope([]any{})
}

// TestIsConflict tests the IsConflict error helper.
func TestIsConflict(t *testing.T) {
	t.Run("nil error", func(t *testing.T) {
		if cloudflare.IsConflict(nil) {
			t.Error("IsConflict(nil) = true, want false")
		}
	})

	t.Run("plain error", func(t *testing.T) {
		if cloudflare.IsConflict(errors.New("something failed")) {
			t.Error("IsConflict(plain error) = true, want false")
		}
	})

	t.Run("conflict error", func(t *testing.T) {
		var apiErr cfgo.Error
		apiErr.StatusCode = http.StatusConflict
		if !cloudflare.IsConflict(&apiErr) {
			t.Error("IsConflict(409) = false, want true")
		}
	})

	t.Run("wrapped conflict error", func(t *testing.T) {
		var apiErr cfgo.Error
		apiErr.StatusCode = http.StatusConflict
		wrapped := fmt.Errorf("outer: %w", &apiErr)
		if !cloudflare.IsConflict(wrapped) {
			t.Error("IsConflict(wrapped 409) = false, want true")
		}
	})

	t.Run("non-conflict API error", func(t *testing.T) {
		var apiErr cfgo.Error
		apiErr.StatusCode = http.StatusNotFound
		if cloudflare.IsConflict(&apiErr) {
			t.Error("IsConflict(404) = true, want false")
		}
	})
}

func TestNewClient(t *testing.T) {
	c, err := cloudflare.NewClient(cloudflare.ClientConfig{
		APIToken:  "test-token",
		AccountID: "test-account",
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if c == nil {
		t.Fatal("NewClient() returned nil")
	}
}

func TestCreateTunnel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /accounts/{accountID}/cfd_tunnel", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var params map[string]any
		json.Unmarshal(body, &params)
		if params["name"] != "my-tunnel" {
			t.Errorf("CreateTunnel name = %v, want my-tunnel", params["name"])
		}
		writeJSON(w, http.StatusOK, envelope(map[string]any{
			"id":   "tunnel-123",
			"name": "my-tunnel",
		}))
	})
	c := newTestClient(t, mux)

	id, err := c.CreateTunnel(context.Background(), "my-tunnel")
	if err != nil {
		t.Fatalf("CreateTunnel() error = %v", err)
	}
	if id != "tunnel-123" {
		t.Errorf("CreateTunnel() = %q, want %q", id, "tunnel-123")
	}
}

func TestGetTunnelIDByName(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /accounts/{accountID}/cfd_tunnel", func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if page == "2" {
				writeJSON(w, http.StatusOK, emptyPage())
				return
			}
			writeJSON(w, http.StatusOK, paginatedEnvelope([]map[string]any{
				{"id": "tunnel-abc", "name": "my-tunnel"},
			}))
		})
		c := newTestClient(t, mux)

		id, err := c.GetTunnelIDByName(context.Background(), "my-tunnel")
		if err != nil {
			t.Fatalf("GetTunnelIDByName() error = %v", err)
		}
		if id != "tunnel-abc" {
			t.Errorf("GetTunnelIDByName() = %q, want %q", id, "tunnel-abc")
		}
	})

	t.Run("not found", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /accounts/{accountID}/cfd_tunnel", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, emptyPage())
		})
		c := newTestClient(t, mux)

		id, err := c.GetTunnelIDByName(context.Background(), "missing")
		if err != nil {
			t.Fatalf("GetTunnelIDByName() error = %v", err)
		}
		if id != "" {
			t.Errorf("GetTunnelIDByName() = %q, want empty", id)
		}
	})
}

func TestDeleteTunnel(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /accounts/{accountID}/cfd_tunnel/{tunnelID}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, envelope(map[string]any{
				"id": r.PathValue("tunnelID"),
			}))
		})
		c := newTestClient(t, mux)

		if err := c.DeleteTunnel(context.Background(), "tunnel-123"); err != nil {
			t.Fatalf("DeleteTunnel() error = %v", err)
		}
	})

	t.Run("not found returns nil", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /accounts/{accountID}/cfd_tunnel/{tunnelID}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"success":  false,
				"errors":   []map[string]any{{"code": 1000, "message": "not found"}},
				"messages": []any{},
				"result":   nil,
			})
		})
		c := newTestClient(t, mux)

		if err := c.DeleteTunnel(context.Background(), "missing"); err != nil {
			t.Fatalf("DeleteTunnel(not found) error = %v, want nil", err)
		}
	})
}

func TestGetTunnelToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /accounts/{accountID}/cfd_tunnel/{tunnelID}/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, envelope("my-secret-token"))
	})
	c := newTestClient(t, mux)

	token, err := c.GetTunnelToken(context.Background(), "tunnel-123")
	if err != nil {
		t.Fatalf("GetTunnelToken() error = %v", err)
	}
	if token != "my-secret-token" {
		t.Errorf("GetTunnelToken() = %q, want %q", token, "my-secret-token")
	}
}

func TestUpdateTunnelConfiguration(t *testing.T) {
	t.Run("with path", func(t *testing.T) {
		var captured map[string]any
		mux := http.NewServeMux()
		mux.HandleFunc("PUT /accounts/{accountID}/cfd_tunnel/{tunnelID}/configurations", func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &captured)
			writeJSON(w, http.StatusOK, envelope(map[string]any{
				"tunnel_id": r.PathValue("tunnelID"),
			}))
		})
		c := newTestClient(t, mux)

		err := c.UpdateTunnelConfiguration(context.Background(), "tunnel-123", []cloudflare.IngressRule{
			{Hostname: "app.example.com", Service: "http://localhost:8080", Path: "/api"},
			{Hostname: "", Service: "http_status:404"},
		})
		if err != nil {
			t.Fatalf("UpdateTunnelConfiguration() error = %v", err)
		}

		config, _ := captured["config"].(map[string]any)
		ingress, _ := config["ingress"].([]any)
		if len(ingress) != 2 {
			t.Fatalf("ingress length = %d, want 2", len(ingress))
		}
		first, _ := ingress[0].(map[string]any)
		if first["hostname"] != "app.example.com" {
			t.Errorf("ingress[0].hostname = %v, want app.example.com", first["hostname"])
		}
		if first["path"] != "/api" {
			t.Errorf("ingress[0].path = %v, want /api", first["path"])
		}
	})

	t.Run("without path", func(t *testing.T) {
		var captured map[string]any
		mux := http.NewServeMux()
		mux.HandleFunc("PUT /accounts/{accountID}/cfd_tunnel/{tunnelID}/configurations", func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &captured)
			writeJSON(w, http.StatusOK, envelope(map[string]any{
				"tunnel_id": r.PathValue("tunnelID"),
			}))
		})
		c := newTestClient(t, mux)

		err := c.UpdateTunnelConfiguration(context.Background(), "tunnel-123", []cloudflare.IngressRule{
			{Hostname: "app.example.com", Service: "http://localhost:8080"},
		})
		if err != nil {
			t.Fatalf("UpdateTunnelConfiguration() error = %v", err)
		}

		config, _ := captured["config"].(map[string]any)
		ingress, _ := config["ingress"].([]any)
		first, _ := ingress[0].(map[string]any)
		if _, ok := first["path"]; ok {
			t.Errorf("ingress[0].path should be absent, got %v", first["path"])
		}
	})
}

func TestListZoneIDs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "2" {
			writeJSON(w, http.StatusOK, emptyPage())
			return
		}
		writeJSON(w, http.StatusOK, paginatedEnvelope([]map[string]any{
			{"id": "zone-1", "name": "example.com"},
			{"id": "zone-2", "name": "other.com"},
		}))
	})
	c := newTestClient(t, mux)

	ids, err := c.ListZoneIDs(context.Background())
	if err != nil {
		t.Fatalf("ListZoneIDs() error = %v", err)
	}
	if len(ids) != 2 || ids[0] != "zone-1" || ids[1] != "zone-2" {
		t.Errorf("ListZoneIDs() = %v, want [zone-1, zone-2]", ids)
	}
}

func TestFindZoneIDByHostname(t *testing.T) {
	t.Run("exact match", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
			name := r.URL.Query().Get("name")
			page := r.URL.Query().Get("page")
			if page == "2" {
				writeJSON(w, http.StatusOK, emptyPage())
				return
			}
			if name == "example.com" {
				writeJSON(w, http.StatusOK, paginatedEnvelope([]map[string]any{
					{"id": "zone-abc", "name": "example.com"},
				}))
				return
			}
			writeJSON(w, http.StatusOK, emptyPage())
		})
		c := newTestClient(t, mux)

		id, err := c.FindZoneIDByHostname(context.Background(), "example.com")
		if err != nil {
			t.Fatalf("FindZoneIDByHostname() error = %v", err)
		}
		if id != "zone-abc" {
			t.Errorf("FindZoneIDByHostname() = %q, want %q", id, "zone-abc")
		}
	})

	t.Run("subdomain strips to parent zone", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
			name := r.URL.Query().Get("name")
			page := r.URL.Query().Get("page")
			if page == "2" {
				writeJSON(w, http.StatusOK, emptyPage())
				return
			}
			if name == "example.com" {
				writeJSON(w, http.StatusOK, paginatedEnvelope([]map[string]any{
					{"id": "zone-parent", "name": "example.com"},
				}))
				return
			}
			writeJSON(w, http.StatusOK, emptyPage())
		})
		c := newTestClient(t, mux)

		id, err := c.FindZoneIDByHostname(context.Background(), "sub.example.com")
		if err != nil {
			t.Fatalf("FindZoneIDByHostname() error = %v", err)
		}
		if id != "zone-parent" {
			t.Errorf("FindZoneIDByHostname() = %q, want %q", id, "zone-parent")
		}
	})

	t.Run("not found", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, emptyPage())
		})
		c := newTestClient(t, mux)

		_, err := c.FindZoneIDByHostname(context.Background(), "unknown.example.com")
		if err == nil {
			t.Fatal("FindZoneIDByHostname() error = nil, want error")
		}
	})
}

func TestEnsureDNSCNAME(t *testing.T) {
	t.Run("creates when not found", func(t *testing.T) {
		var createdBody map[string]any
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, emptyPage())
		})
		mux.HandleFunc("POST /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &createdBody)
			writeJSON(w, http.StatusOK, envelope(map[string]any{
				"id":      "record-new",
				"name":    createdBody["name"],
				"content": createdBody["content"],
				"type":    "CNAME",
			}))
		})
		c := newTestClient(t, mux)

		err := c.EnsureDNSCNAME(context.Background(), "zone-1", "app.example.com", "tunnel.cfargotunnel.com")
		if err != nil {
			t.Fatalf("EnsureDNSCNAME() error = %v", err)
		}
		if createdBody["name"] != "app.example.com" {
			t.Errorf("created name = %v, want app.example.com", createdBody["name"])
		}
		if createdBody["content"] != "tunnel.cfargotunnel.com" {
			t.Errorf("created content = %v, want tunnel.cfargotunnel.com", createdBody["content"])
		}
	})

	t.Run("updates when target differs", func(t *testing.T) {
		var updatedBody map[string]any
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if page == "2" {
				writeJSON(w, http.StatusOK, emptyPage())
				return
			}
			writeJSON(w, http.StatusOK, paginatedEnvelope([]map[string]any{
				{"id": "record-existing", "name": "app.example.com", "content": "old-target.cfargotunnel.com", "type": "CNAME"},
			}))
		})
		mux.HandleFunc("PUT /zones/{zoneID}/dns_records/{recordID}", func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &updatedBody)
			writeJSON(w, http.StatusOK, envelope(map[string]any{
				"id":      r.PathValue("recordID"),
				"name":    updatedBody["name"],
				"content": updatedBody["content"],
				"type":    "CNAME",
			}))
		})
		c := newTestClient(t, mux)

		err := c.EnsureDNSCNAME(context.Background(), "zone-1", "app.example.com", "new-target.cfargotunnel.com")
		if err != nil {
			t.Fatalf("EnsureDNSCNAME() error = %v", err)
		}
		if updatedBody["content"] != "new-target.cfargotunnel.com" {
			t.Errorf("updated content = %v, want new-target.cfargotunnel.com", updatedBody["content"])
		}
	})

	t.Run("no-op when target matches", func(t *testing.T) {
		updateCalled := false
		createCalled := false
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if page == "2" {
				writeJSON(w, http.StatusOK, emptyPage())
				return
			}
			writeJSON(w, http.StatusOK, paginatedEnvelope([]map[string]any{
				{"id": "record-existing", "name": "app.example.com", "content": "tunnel.cfargotunnel.com", "type": "CNAME"},
			}))
		})
		mux.HandleFunc("PUT /zones/{zoneID}/dns_records/{recordID}", func(w http.ResponseWriter, r *http.Request) {
			updateCalled = true
		})
		mux.HandleFunc("POST /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			createCalled = true
		})
		c := newTestClient(t, mux)

		err := c.EnsureDNSCNAME(context.Background(), "zone-1", "app.example.com", "tunnel.cfargotunnel.com")
		if err != nil {
			t.Fatalf("EnsureDNSCNAME() error = %v", err)
		}
		if updateCalled {
			t.Error("EnsureDNSCNAME() called update when target already matches")
		}
		if createCalled {
			t.Error("EnsureDNSCNAME() called create when target already matches")
		}
	})
}

func TestDeleteDNSCNAME(t *testing.T) {
	t.Run("deletes when found", func(t *testing.T) {
		deleteCalled := false
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if page == "2" {
				writeJSON(w, http.StatusOK, emptyPage())
				return
			}
			writeJSON(w, http.StatusOK, paginatedEnvelope([]map[string]any{
				{"id": "record-del", "name": "app.example.com", "content": "tunnel.cfargotunnel.com", "type": "CNAME"},
			}))
		})
		mux.HandleFunc("DELETE /zones/{zoneID}/dns_records/{recordID}", func(w http.ResponseWriter, r *http.Request) {
			deleteCalled = true
			writeJSON(w, http.StatusOK, envelope(map[string]any{
				"id": r.PathValue("recordID"),
			}))
		})
		c := newTestClient(t, mux)

		err := c.DeleteDNSCNAME(context.Background(), "zone-1", "app.example.com")
		if err != nil {
			t.Fatalf("DeleteDNSCNAME() error = %v", err)
		}
		if !deleteCalled {
			t.Error("DeleteDNSCNAME() did not call delete")
		}
	})

	t.Run("no-op when not found", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, emptyPage())
		})
		c := newTestClient(t, mux)

		err := c.DeleteDNSCNAME(context.Background(), "zone-1", "missing.example.com")
		if err != nil {
			t.Fatalf("DeleteDNSCNAME() error = %v, want nil", err)
		}
	})
}

func TestListDNSCNAMEsByTarget(t *testing.T) {
	t.Run("returns matching hostnames", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			page := r.URL.Query().Get("page")
			if page == "2" {
				writeJSON(w, http.StatusOK, emptyPage())
				return
			}
			writeJSON(w, http.StatusOK, paginatedEnvelope([]map[string]any{
				{"id": "r1", "name": "app1.example.com", "content": "tunnel.cfargotunnel.com", "type": "CNAME"},
				{"id": "r2", "name": "app2.example.com", "content": "tunnel.cfargotunnel.com", "type": "CNAME"},
			}))
		})
		c := newTestClient(t, mux)

		hostnames, err := c.ListDNSCNAMEsByTarget(context.Background(), "zone-1", "tunnel.cfargotunnel.com")
		if err != nil {
			t.Fatalf("ListDNSCNAMEsByTarget() error = %v", err)
		}
		if len(hostnames) != 2 || hostnames[0] != "app1.example.com" || hostnames[1] != "app2.example.com" {
			t.Errorf("ListDNSCNAMEsByTarget() = %v, want [app1.example.com, app2.example.com]", hostnames)
		}
	})

	t.Run("returns nil when empty", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, emptyPage())
		})
		c := newTestClient(t, mux)

		hostnames, err := c.ListDNSCNAMEsByTarget(context.Background(), "zone-1", "tunnel.cfargotunnel.com")
		if err != nil {
			t.Fatalf("ListDNSCNAMEsByTarget() error = %v", err)
		}
		if hostnames != nil {
			t.Errorf("ListDNSCNAMEsByTarget() = %v, want nil", hostnames)
		}
	})
}
