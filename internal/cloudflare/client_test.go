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
	. "github.com/onsi/gomega"

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

func apiError(status int) map[string]any {
	return map[string]any{
		"success":  false,
		"errors":   []map[string]any{{"code": 1000, "message": http.StatusText(status)}},
		"messages": []any{},
		"result":   nil,
	}
}

func TestIsConflict(t *testing.T) {
	t.Run("nil error", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(cloudflare.IsConflict(nil)).To(BeFalse())
	})

	t.Run("plain error", func(t *testing.T) {
		g := NewWithT(t)
		g.Expect(cloudflare.IsConflict(errors.New("something failed"))).To(BeFalse())
	})

	t.Run("conflict error", func(t *testing.T) {
		g := NewWithT(t)
		var apiErr cfgo.Error
		apiErr.StatusCode = http.StatusConflict
		g.Expect(cloudflare.IsConflict(&apiErr)).To(BeTrue())
	})

	t.Run("wrapped conflict error", func(t *testing.T) {
		g := NewWithT(t)
		var apiErr cfgo.Error
		apiErr.StatusCode = http.StatusConflict
		wrapped := fmt.Errorf("outer: %w", &apiErr)
		g.Expect(cloudflare.IsConflict(wrapped)).To(BeTrue())
	})

	t.Run("non-conflict API error", func(t *testing.T) {
		g := NewWithT(t)
		var apiErr cfgo.Error
		apiErr.StatusCode = http.StatusNotFound
		g.Expect(cloudflare.IsConflict(&apiErr)).To(BeFalse())
	})
}

func TestTunnelTarget(t *testing.T) {
	g := NewWithT(t)
	g.Expect(cloudflare.TunnelTarget("abc-123")).To(Equal("abc-123.cfargotunnel.com"))
}

func TestNewClient(t *testing.T) {
	g := NewWithT(t)
	c, err := cloudflare.NewClient(cloudflare.ClientConfig{
		APIToken:  "test-token",
		AccountID: "test-account",
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c).NotTo(BeNil())
}

func TestCreateTunnel(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("POST /accounts/{accountID}/cfd_tunnel", func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var params map[string]any
			json.Unmarshal(body, &params)
			g.Expect(params["name"]).To(Equal("my-tunnel"))
			writeJSON(w, http.StatusOK, envelope(map[string]any{
				"id":   "tunnel-123",
				"name": "my-tunnel",
			}))
		})
		c := newTestClient(t, mux)

		id, err := c.CreateTunnel(context.Background(), "my-tunnel")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(id).To(Equal("tunnel-123"))
	})

	t.Run("API error", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("POST /accounts/{accountID}/cfd_tunnel", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		_, err := c.CreateTunnel(context.Background(), "my-tunnel")
		g.Expect(err).To(HaveOccurred())
	})
}

func TestGetTunnelIDByName(t *testing.T) {
	t.Run("found", func(t *testing.T) {
		g := NewWithT(t)
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
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(id).To(Equal("tunnel-abc"))
	})

	t.Run("not found", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /accounts/{accountID}/cfd_tunnel", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, emptyPage())
		})
		c := newTestClient(t, mux)

		id, err := c.GetTunnelIDByName(context.Background(), "missing")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(id).To(BeEmpty())
	})

	t.Run("API error", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /accounts/{accountID}/cfd_tunnel", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		_, err := c.GetTunnelIDByName(context.Background(), "my-tunnel")
		g.Expect(err).To(HaveOccurred())
	})
}

func TestDeleteTunnel(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /accounts/{accountID}/cfd_tunnel/{tunnelID}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, envelope(map[string]any{
				"id": r.PathValue("tunnelID"),
			}))
		})
		c := newTestClient(t, mux)

		g.Expect(c.DeleteTunnel(context.Background(), "tunnel-123")).To(Succeed())
	})

	t.Run("not found returns nil", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /accounts/{accountID}/cfd_tunnel/{tunnelID}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, apiError(http.StatusNotFound))
		})
		c := newTestClient(t, mux)

		g.Expect(c.DeleteTunnel(context.Background(), "missing")).To(Succeed())
	})

	t.Run("non-404 error is returned", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("DELETE /accounts/{accountID}/cfd_tunnel/{tunnelID}", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		g.Expect(c.DeleteTunnel(context.Background(), "tunnel-123")).To(HaveOccurred())
	})
}

func TestGetTunnelToken(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /accounts/{accountID}/cfd_tunnel/{tunnelID}/token", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, envelope("my-secret-token"))
		})
		c := newTestClient(t, mux)

		token, err := c.GetTunnelToken(context.Background(), "tunnel-123")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(token).To(Equal("my-secret-token"))
	})

	t.Run("API error", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /accounts/{accountID}/cfd_tunnel/{tunnelID}/token", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		_, err := c.GetTunnelToken(context.Background(), "tunnel-123")
		g.Expect(err).To(HaveOccurred())
	})
}

func TestUpdateTunnelConfiguration(t *testing.T) {
	t.Run("with path", func(t *testing.T) {
		g := NewWithT(t)
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
		g.Expect(err).NotTo(HaveOccurred())

		config, _ := captured["config"].(map[string]any)
		ingress, _ := config["ingress"].([]any)
		g.Expect(ingress).To(HaveLen(2))
		first, _ := ingress[0].(map[string]any)
		g.Expect(first["hostname"]).To(Equal("app.example.com"))
		g.Expect(first["path"]).To(Equal("/api"))
	})

	t.Run("without path", func(t *testing.T) {
		g := NewWithT(t)
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
		g.Expect(err).NotTo(HaveOccurred())

		config, _ := captured["config"].(map[string]any)
		ingress, _ := config["ingress"].([]any)
		first, _ := ingress[0].(map[string]any)
		g.Expect(first).NotTo(HaveKey("path"))
	})

	t.Run("API error", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("PUT /accounts/{accountID}/cfd_tunnel/{tunnelID}/configurations", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		err := c.UpdateTunnelConfiguration(context.Background(), "tunnel-123", []cloudflare.IngressRule{
			{Hostname: "app.example.com", Service: "http://localhost:8080"},
		})
		g.Expect(err).To(HaveOccurred())
	})
}

func TestListZoneIDs(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		g := NewWithT(t)
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
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(ids).To(Equal([]string{"zone-1", "zone-2"}))
	})

	t.Run("API error", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		_, err := c.ListZoneIDs(context.Background())
		g.Expect(err).To(HaveOccurred())
	})
}

func TestFindZoneIDByHostname(t *testing.T) {
	t.Run("exact match", func(t *testing.T) {
		g := NewWithT(t)
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
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(id).To(Equal("zone-abc"))
	})

	t.Run("subdomain strips to parent zone", func(t *testing.T) {
		g := NewWithT(t)
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
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(id).To(Equal("zone-parent"))
	})

	t.Run("not found", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, emptyPage())
		})
		c := newTestClient(t, mux)

		_, err := c.FindZoneIDByHostname(context.Background(), "unknown.example.com")
		g.Expect(err).To(HaveOccurred())
	})

	t.Run("API error", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		_, err := c.FindZoneIDByHostname(context.Background(), "app.example.com")
		g.Expect(err).To(HaveOccurred())
	})
}

func TestEnsureDNSCNAME(t *testing.T) {
	t.Run("creates when not found", func(t *testing.T) {
		g := NewWithT(t)
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

		g.Expect(c.EnsureDNSCNAME(context.Background(), "zone-1", "app.example.com", "tunnel.cfargotunnel.com")).To(Succeed())
		g.Expect(createdBody["name"]).To(Equal("app.example.com"))
		g.Expect(createdBody["content"]).To(Equal("tunnel.cfargotunnel.com"))
	})

	t.Run("updates when target differs", func(t *testing.T) {
		g := NewWithT(t)
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

		g.Expect(c.EnsureDNSCNAME(context.Background(), "zone-1", "app.example.com", "new-target.cfargotunnel.com")).To(Succeed())
		g.Expect(updatedBody["content"]).To(Equal("new-target.cfargotunnel.com"))
	})

	t.Run("list error", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		g.Expect(c.EnsureDNSCNAME(context.Background(), "zone-1", "app.example.com", "tunnel.cfargotunnel.com")).To(HaveOccurred())
	})

	t.Run("update error", func(t *testing.T) {
		g := NewWithT(t)
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
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		g.Expect(c.EnsureDNSCNAME(context.Background(), "zone-1", "app.example.com", "new-target.cfargotunnel.com")).To(HaveOccurred())
	})

	t.Run("create error", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, emptyPage())
		})
		mux.HandleFunc("POST /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		g.Expect(c.EnsureDNSCNAME(context.Background(), "zone-1", "app.example.com", "tunnel.cfargotunnel.com")).To(HaveOccurred())
	})

	t.Run("no-op when target matches", func(t *testing.T) {
		g := NewWithT(t)
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

		g.Expect(c.EnsureDNSCNAME(context.Background(), "zone-1", "app.example.com", "tunnel.cfargotunnel.com")).To(Succeed())
		g.Expect(updateCalled).To(BeFalse())
		g.Expect(createCalled).To(BeFalse())
	})
}

func TestDeleteDNSCNAME(t *testing.T) {
	t.Run("deletes when found", func(t *testing.T) {
		g := NewWithT(t)
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

		g.Expect(c.DeleteDNSCNAME(context.Background(), "zone-1", "app.example.com")).To(Succeed())
		g.Expect(deleteCalled).To(BeTrue())
	})

	t.Run("no-op when not found", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, emptyPage())
		})
		c := newTestClient(t, mux)

		g.Expect(c.DeleteDNSCNAME(context.Background(), "zone-1", "missing.example.com")).To(Succeed())
	})

	t.Run("list error", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		g.Expect(c.DeleteDNSCNAME(context.Background(), "zone-1", "app.example.com")).To(HaveOccurred())
	})

	t.Run("delete error", func(t *testing.T) {
		g := NewWithT(t)
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
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		g.Expect(c.DeleteDNSCNAME(context.Background(), "zone-1", "app.example.com")).To(HaveOccurred())
	})
}

func TestListDNSCNAMEsByTarget(t *testing.T) {
	t.Run("returns matching hostnames", func(t *testing.T) {
		g := NewWithT(t)
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
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(hostnames).To(Equal([]string{"app1.example.com", "app2.example.com"}))
	})

	t.Run("returns nil when empty", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, emptyPage())
		})
		c := newTestClient(t, mux)

		hostnames, err := c.ListDNSCNAMEsByTarget(context.Background(), "zone-1", "tunnel.cfargotunnel.com")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(hostnames).To(BeNil())
	})

	t.Run("API error", func(t *testing.T) {
		g := NewWithT(t)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /zones/{zoneID}/dns_records", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusBadRequest, apiError(http.StatusBadRequest))
		})
		c := newTestClient(t, mux)

		_, err := c.ListDNSCNAMEsByTarget(context.Background(), "zone-1", "tunnel.cfargotunnel.com")
		g.Expect(err).To(HaveOccurred())
	})
}
