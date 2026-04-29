// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultNodeNumIsZero(t *testing.T) {
	cfg := Default()
	if cfg.NodeNum != 0 {
		t.Fatalf("expected default NodeNum=0, got %d", cfg.NodeNum)
	}
}

func TestMergeNodeNum(t *testing.T) {
	base := Default() // NodeNum=0
	overlay := &Config{NodeNum: 5}
	result := base.Merge(overlay)
	if result.NodeNum != 5 {
		t.Fatalf("expected NodeNum=5 after merge, got %d", result.NodeNum)
	}
}

func TestMergeNodeNum_ZeroDoesNotOverride(t *testing.T) {
	// NodeNum=0 in overlay is treated as "not set" — keeps base value.
	// This is acceptable since node_num=0 is equivalent to omitting it.
	base := Default()
	base.NodeNum = 3
	overlay := &Config{NodeNum: 0}
	result := base.Merge(overlay)
	if result.NodeNum != 3 {
		t.Fatalf("expected NodeNum=3 unchanged, got %d", result.NodeNum)
	}
}

func TestMergeGroups(t *testing.T) {
	base := Default()
	overlay := &Config{
		Groups: []*NodeGroup{
			{Name: "web", StartCount: 2},
		},
	}
	result := base.Merge(overlay)
	if len(result.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(result.Groups))
	}
	if result.Groups[0].Name != "web" || result.Groups[0].StartCount != 2 {
		t.Fatalf("unexpected group: %+v", result.Groups[0])
	}
}

func TestNodeMerge_NodePoolOverride(t *testing.T) {
	base := &Node{NodePool: "default", Region: "global"}
	override := &Node{NodePool: "web-pool"}
	result := base.Merge(override)
	if result.NodePool != "web-pool" {
		t.Fatalf("expected NodePool=web-pool, got %q", result.NodePool)
	}
	if result.Region != "global" {
		t.Fatalf("expected Region=global (inherited), got %q", result.Region)
	}
}

func TestParseFile_WithNodeGroup(t *testing.T) {
	hcl := `
work_dir = "/tmp/test"
node_num = 0

group "web" {
  start_count = 3
  node {
    node_pool = "web-pool"
    resources {
      cpu_compute = 4000
      memory_mb   = 8000
    }
  }
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "nodesim.hcl")
	if err := os.WriteFile(path, []byte(hcl), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if len(cfg.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(cfg.Groups))
	}
	g := cfg.Groups[0]
	if g.Name != "web" {
		t.Fatalf("expected group name=web, got %q", g.Name)
	}
	if g.StartCount != 3 {
		t.Fatalf("expected start_count=3, got %d", g.StartCount)
	}
	if g.Node == nil || g.Node.NodePool != "web-pool" {
		t.Fatalf("expected node_pool=web-pool, got %v", g.Node)
	}
	if g.Node.Resources == nil || g.Node.Resources.CPUCompute != 4000 {
		t.Fatalf("expected cpu_compute=4000, got %v", g.Node.Resources)
	}
}
