// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package nodegroup

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"

	internalConfig "github.com/hashicorp-forge/nomad-nodesim/internal/config"
	"github.com/hashicorp/go-hclog"
)

const defaultAddr = "[::]:4649"

// CreateRequest is the request body for POST /v1/groups.
type CreateRequest struct {
	Name  string      `json:"name"`
	Count int         `json:"count"`
	Node  *NodeConfig `json:"node,omitempty"`
}

// NodeConfig mirrors the HCL node{} block for use in API requests.
// All fields are optional; omitted fields inherit from the base node config.
type NodeConfig struct {
	Region     string            `json:"region,omitempty"`
	Datacenter string            `json:"datacenter,omitempty"`
	NodePool   string            `json:"node_pool,omitempty"`
	NodeClass  string            `json:"node_class,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	Resources  *ResourceConfig   `json:"resources,omitempty"`
}

// ResourceConfig mirrors the HCL resources{} block.
type ResourceConfig struct {
	CPUCompute uint64 `json:"cpu_compute,omitempty"`
	MemoryMB   uint64 `json:"memory_mb,omitempty"`
}

func (nc *NodeConfig) toInternalNode() *internalConfig.Node {
	if nc == nil {
		return nil
	}
	n := &internalConfig.Node{
		Region:     nc.Region,
		Datacenter: nc.Datacenter,
		NodePool:   nc.NodePool,
		NodeClass:  nc.NodeClass,
		Options:    nc.Options,
	}
	if nc.Resources != nil {
		n.Resources = &internalConfig.NodeResource{
			CPUCompute: nc.Resources.CPUCompute,
			MemoryMB:   nc.Resources.MemoryMB,
		}
	}
	return n
}

// managerFacade is the interface the HTTP handler uses. Keeping it here (not
// just in tests) documents the API surface and allows test injection.
type managerFacade interface {
	List() []*Status
	Get(name string) (*Status, bool)
	ListNodes(name string) ([]*NodeInfo, bool)
	Create(name string, startCount int, nodeOverride *internalConfig.Node) (*Status, error)
	Delete(name string) error
	Scale(name string, count int) (*Status, error)
}

// NewServer returns an HTTP server that exposes the node group API.
// Address defaults to [::]:4649; override with NODESIM_GROUPS_ADDR.
func NewServer(m *Manager) *http.Server {
	addr := os.Getenv("NODESIM_GROUPS_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	return &http.Server{
		Addr:    addr,
		Handler: buildMux(m, m.logger.Named("api")),
	}
}

// buildMux constructs the API routes against a managerFacade, making it
// testable with a fake manager.
func buildMux(m managerFacade, logger hclog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /v1/groups", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.List())
	})

	mux.HandleFunc("POST /v1/groups", func(w http.ResponseWriter, r *http.Request) {
		var req CreateRequest
		if err := decodeStrict(r, &req); err != nil {
			logger.Debug("bad request", "method", r.Method, "path", r.URL.Path, "error", err)
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Name == "" {
			logger.Debug("bad request", "method", r.Method, "path", r.URL.Path, "error", "name is required")
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		if req.Count < 0 {
			logger.Debug("bad request", "method", r.Method, "path", r.URL.Path, "error", "count must be >= 0")
			writeError(w, http.StatusBadRequest, "count must be >= 0")
			return
		}

		s, err := m.Create(req.Name, req.Count, req.Node.toInternalNode())
		if err != nil {
			if errors.Is(err, ErrAlreadyExists) {
				logger.Debug("conflict creating group", "group", req.Name, "error", err)
				writeError(w, http.StatusConflict, err.Error())
			} else {
				logger.Error("failed to create group", "group", req.Name, "error", err)
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		logger.Info("created group via API", "group", s.Name, "count", s.Count, "node_pool", s.NodePool)
		writeJSON(w, http.StatusCreated, s)
	})

	mux.HandleFunc("GET /v1/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		s, ok := m.Get(name)
		if !ok {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		writeJSON(w, http.StatusOK, s)
	})

	mux.HandleFunc("GET /v1/groups/{name}/nodes", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		nodes, ok := m.ListNodes(name)
		if !ok {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		writeJSON(w, http.StatusOK, nodes)
	})

	mux.HandleFunc("DELETE /v1/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := m.Delete(name); err != nil {
			if errors.Is(err, ErrNotFound) {
				logger.Debug("group not found for delete", "group", name)
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				logger.Error("failed to delete group", "group", name, "error", err)
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		logger.Info("deleted group via API", "group", name)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /v1/groups/{name}/scale", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")

		var req struct {
			Count int `json:"count"`
		}
		if err := decodeStrict(r, &req); err != nil {
			logger.Debug("bad request", "method", r.Method, "path", r.URL.Path, "error", err)
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Count < 0 {
			logger.Debug("bad request", "method", r.Method, "path", r.URL.Path, "error", "count must be >= 0")
			writeError(w, http.StatusBadRequest, "count must be >= 0")
			return
		}

		s, err := m.Scale(name, req.Count)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				logger.Debug("group not found for scale", "group", name)
				writeError(w, http.StatusNotFound, err.Error())
			} else {
				logger.Error("failed to scale group", "group", name, "count", req.Count, "error", err)
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		logger.Info("scaled group via API", "group", s.Name, "count", s.Count, "node_pool", s.NodePool)
		writeJSON(w, http.StatusOK, s)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// decodeStrict decodes JSON from r.Body into v, returning an error for unknown
// fields or malformed input. This gives callers an informative error message
// (e.g. "json: unknown field \"nmae\"") instead of silently ignoring typos.
func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
