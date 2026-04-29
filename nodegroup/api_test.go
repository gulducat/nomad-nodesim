// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package nodegroup

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	internalConfig "github.com/hashicorp-forge/nomad-nodesim/internal/config"
)

// fakeManager satisfies managerFacade without starting real Nomad nodes.
type fakeManager struct {
	groups map[string]*Status
}

func newFakeManager(groups ...*Status) *fakeManager {
	m := &fakeManager{groups: make(map[string]*Status)}
	for _, g := range groups {
		m.groups[g.Name] = g
	}
	return m
}

func (f *fakeManager) List() []*Status {
	out := make([]*Status, 0, len(f.groups))
	for _, s := range f.groups {
		cp := *s
		out = append(out, &cp)
	}
	return out
}

func (f *fakeManager) Get(name string) (*Status, bool) {
	s, ok := f.groups[name]
	if !ok {
		return nil, false
	}
	cp := *s
	return &cp, true
}

func (f *fakeManager) Create(name string, startCount int, _ *internalConfig.Node) (*Status, error) {
	if _, exists := f.groups[name]; exists {
		return nil, ErrAlreadyExists
	}
	s := &Status{
		Name:         name,
		DesiredCount: startCount,
		CurrentCount: startCount,
		Ready:        true,
	}
	f.groups[name] = s
	cp := *s
	return &cp, nil
}

func (f *fakeManager) Delete(name string) error {
	if _, ok := f.groups[name]; !ok {
		return ErrNotFound
	}
	delete(f.groups, name)
	return nil
}

func (f *fakeManager) Scale(name string, count int) (*Status, error) {
	s, ok := f.groups[name]
	if !ok {
		return nil, ErrNotFound
	}
	s.DesiredCount = count
	s.CurrentCount = count
	s.Ready = true
	cp := *s
	return &cp, nil
}

// --- Tests ---

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(buildMux(newFakeManager()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body map[string]string
	mustDecode(t, resp, &body)
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %q", body["status"])
	}
}

func TestListGroups_Empty(t *testing.T) {
	srv := httptest.NewServer(buildMux(newFakeManager()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/groups")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body []*Status
	mustDecode(t, resp, &body)
	if len(body) != 0 {
		t.Fatalf("expected empty list, got %d", len(body))
	}
}

func TestListGroups_WithGroups(t *testing.T) {
	fm := newFakeManager(
		&Status{Name: "web", NodePool: "web-pool", DesiredCount: 2, CurrentCount: 2, Ready: true},
		&Status{Name: "api", NodePool: "api-pool", DesiredCount: 1, CurrentCount: 1, Ready: true},
	)
	srv := httptest.NewServer(buildMux(fm))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/v1/groups")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var body []*Status
	mustDecode(t, resp, &body)
	if len(body) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(body))
	}
}

func TestCreateGroup(t *testing.T) {
	srv := httptest.NewServer(buildMux(newFakeManager()))
	defer srv.Close()

	resp := mustPost(t, srv.URL+"/v1/groups", CreateRequest{
		Name:       "web",
		StartCount: 3,
		Node:       &NodeConfig{NodePool: "web-pool"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var s Status
	mustDecode(t, resp, &s)
	if s.Name != "web" || s.DesiredCount != 3 || s.CurrentCount != 3 {
		t.Fatalf("unexpected status: %+v", s)
	}
}

func TestCreateGroup_MissingName(t *testing.T) {
	srv := httptest.NewServer(buildMux(newFakeManager()))
	defer srv.Close()

	resp := mustPost(t, srv.URL+"/v1/groups", CreateRequest{StartCount: 1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCreateGroup_AlreadyExists(t *testing.T) {
	fm := newFakeManager(&Status{Name: "web"})
	srv := httptest.NewServer(buildMux(fm))
	defer srv.Close()

	resp := mustPost(t, srv.URL+"/v1/groups", CreateRequest{Name: "web"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}

func TestCreateGroup_NegativeStartCount(t *testing.T) {
	srv := httptest.NewServer(buildMux(newFakeManager()))
	defer srv.Close()

	resp := mustPost(t, srv.URL+"/v1/groups", CreateRequest{Name: "web", StartCount: -1})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestGetGroup_Found(t *testing.T) {
	fm := newFakeManager(&Status{Name: "web", NodePool: "web-pool", DesiredCount: 3, CurrentCount: 3, Ready: true})
	srv := httptest.NewServer(buildMux(fm))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/v1/groups/web")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var s Status
	mustDecode(t, resp, &s)
	if s.Name != "web" || s.DesiredCount != 3 || !s.Ready {
		t.Fatalf("unexpected status: %+v", s)
	}
}

func TestGetGroup_NotFound(t *testing.T) {
	srv := httptest.NewServer(buildMux(newFakeManager()))
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/v1/groups/missing")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestDeleteGroup(t *testing.T) {
	fm := newFakeManager(&Status{Name: "web"})
	srv := httptest.NewServer(buildMux(fm))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/groups/web", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
	// Confirm it's gone
	get, _ := http.Get(srv.URL + "/v1/groups/web")
	if get.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", get.StatusCode)
	}
}

func TestDeleteGroup_NotFound(t *testing.T) {
	srv := httptest.NewServer(buildMux(newFakeManager()))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/groups/missing", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestScaleGroup_Up(t *testing.T) {
	fm := newFakeManager(&Status{Name: "web", NodePool: "web-pool", DesiredCount: 1, CurrentCount: 1, Ready: true})
	srv := httptest.NewServer(buildMux(fm))
	defer srv.Close()

	resp := mustPost(t, srv.URL+"/v1/groups/web/scale", map[string]int{"count": 5})
	var s Status
	mustDecode(t, resp, &s)
	if s.DesiredCount != 5 || s.CurrentCount != 5 || !s.Ready {
		t.Fatalf("unexpected status after scale: %+v", s)
	}
}

func TestScaleGroup_Down(t *testing.T) {
	fm := newFakeManager(&Status{Name: "web", NodePool: "web-pool", DesiredCount: 5, CurrentCount: 5, Ready: true})
	srv := httptest.NewServer(buildMux(fm))
	defer srv.Close()

	resp := mustPost(t, srv.URL+"/v1/groups/web/scale", map[string]int{"count": 2})
	var s Status
	mustDecode(t, resp, &s)
	if s.DesiredCount != 2 || s.CurrentCount != 2 {
		t.Fatalf("unexpected status after scale down: %+v", s)
	}
}

func TestScaleGroup_NotFound(t *testing.T) {
	srv := httptest.NewServer(buildMux(newFakeManager()))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/groups/missing/scale",
		"application/json", bytes.NewBufferString(`{"count":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestScaleGroup_NegativeCount(t *testing.T) {
	fm := newFakeManager(&Status{Name: "web"})
	srv := httptest.NewServer(buildMux(fm))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/groups/web/scale",
		"application/json", bytes.NewBufferString(`{"count":-1}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestScaleGroup_InvalidBody(t *testing.T) {
	fm := newFakeManager(&Status{Name: "web"})
	srv := httptest.NewServer(buildMux(fm))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/groups/web/scale",
		"application/json", bytes.NewBufferString(`not json`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// --- Helpers ---

func mustDecode(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func mustPost(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
