// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

// Package nodegroup implements named, scalable sets of simulated Nomad nodes.
// Groups are pre-declared in config and scaled by external callers (e.g. the
// nodesim-target autoscaler plugin) via the HTTP API.
package nodegroup

import (
	"errors"
	"fmt"
	"sync"

	internalConfig "github.com/hashicorp-forge/nomad-nodesim/internal/config"
	"github.com/hashicorp-forge/nomad-nodesim/internal/nodefactory"
	internalSimnode "github.com/hashicorp-forge/nomad-nodesim/internal/simnode"
	"github.com/hashicorp-forge/nomad-nodesim/simnode"
	"github.com/hashicorp/go-hclog"
)

// ErrNotFound is returned when the named group does not exist.
var ErrNotFound = errors.New("group not found")

// ErrAlreadyExists is returned when trying to create a group that already exists.
var ErrAlreadyExists = errors.New("group already exists")

// NodeGroup is a named, scalable set of simulated Nomad nodes. Nodes in the
// group are named <group>-<index> (e.g. web-0, web-1). The same index always
// produces the same Nomad node ID because Nomad derives node ID from the
// client state directory path, which is based on the node name.
type NodeGroup struct {
	name    string
	desired int
	nodes   map[int]*simnode.Node
	mu      sync.Mutex

	// effectiveCfg is the merged config used to build nodes for this group.
	// It is the base config with this group's node{} block merged on top.
	effectiveCfg *internalConfig.Config
}

// Status is a point-in-time snapshot of a NodeGroup's state, safe to marshal
// to JSON.
type Status struct {
	Name     string `json:"name"`
	NodePool string `json:"node_pool"`
	Count    int    `json:"count"`
	Nodes    int    `json:"nodes"`
	Ready    bool   `json:"ready"`
}

func (ng *NodeGroup) status() *Status {
	current := len(ng.nodes)
	return &Status{
		Name:     ng.name,
		NodePool: ng.effectiveCfg.Node.NodePool,
		Count:    ng.desired,
		Nodes:    current,
		Ready:    current == ng.desired,
	}
}

// Manager owns all NodeGroups and provides Scale/Get/List operations.
type Manager struct {
	groups    map[string]*NodeGroup
	mu        sync.RWMutex
	baseCfg   *internalConfig.Config
	buildInfo *internalSimnode.BuildInfo
	logger    hclog.Logger
}

// NewManager creates a Manager. Call InitFromConfig to populate groups from
// the parsed config.
func NewManager(cfg *internalConfig.Config, buildInfo *internalSimnode.BuildInfo, logger hclog.Logger) *Manager {
	return &Manager{
		groups:    make(map[string]*NodeGroup),
		baseCfg:   cfg,
		buildInfo: buildInfo,
		logger:    logger.Named("nodegroup"),
	}
}

// InitFromConfig pre-creates all groups from config and starts count
// nodes for each. Returns the first error encountered.
func (m *Manager) InitFromConfig() error {
	m.logger.Info("initializing groups from config", "count", len(m.baseCfg.Groups))
	for _, gcfg := range m.baseCfg.Groups {
		ng := m.newNodeGroup(gcfg)

		m.mu.Lock()
		m.groups[gcfg.Name] = ng
		m.mu.Unlock()

		if gcfg.Count > 0 {
			if err := m.scaleGroup(ng, gcfg.Count); err != nil {
				return fmt.Errorf("group %q: %w", gcfg.Name, err)
			}
		}
	}
	return nil
}

// Scale sets the desired node count for the named group, starting or stopping
// nodes as needed. It is synchronous — it returns after reconciliation is
// complete.
func (m *Manager) Scale(name string, count int) (*Status, error) {
	m.mu.RLock()
	ng, ok := m.groups[name]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("group %q not found", name)
	}

	m.logger.Info("scaling group", "group", name, "count", count)
	if err := m.scaleGroup(ng, count); err != nil {
		return nil, err
	}

	ng.mu.Lock()
	s := ng.status()
	ng.mu.Unlock()
	return s, nil
}

// Get returns the current status of a named group.
func (m *Manager) Get(name string) (*Status, bool) {
	m.mu.RLock()
	ng, ok := m.groups[name]
	m.mu.RUnlock()
	if !ok {
		return nil, false
	}

	ng.mu.Lock()
	defer ng.mu.Unlock()
	return ng.status(), true
}

// List returns the current status of all groups, sorted by name.
func (m *Manager) List() []*Status {
	m.mu.RLock()
	groups := make([]*NodeGroup, 0, len(m.groups))
	for _, ng := range m.groups {
		groups = append(groups, ng)
	}
	m.mu.RUnlock()

	out := make([]*Status, 0, len(groups))
	for _, ng := range groups {
		ng.mu.Lock()
		out = append(out, ng.status())
		ng.mu.Unlock()
	}
	return out
}

// Shutdown stops all nodes in all groups.
func (m *Manager) Shutdown() {
	m.mu.RLock()
	groups := make([]*NodeGroup, 0, len(m.groups))
	for _, ng := range m.groups {
		groups = append(groups, ng)
	}
	m.mu.RUnlock()

	m.logger.Info("shutting down all groups", "count", len(groups))
	wg := &sync.WaitGroup{}
	for _, ng := range groups {
		ng.mu.Lock()
		for idx, node := range ng.nodes {
			wg.Add(1)
			go func(name string, n *simnode.Node) {
				defer wg.Done()
				if err := n.Shutdown(); err != nil {
					m.logger.Warn("error shutting down group node", "error", err, "node_id", n.Client.NodeID())
				}
			}(fmt.Sprintf("%s-%d", ng.name, idx), node)
		}
		ng.mu.Unlock()
	}
	wg.Wait()
}

// Create registers a new named group, starts count nodes, and returns
// its initial status. Returns ErrAlreadyExists if the name is taken.
func (m *Manager) Create(name string, startCount int, nodeOverride *internalConfig.Node) (*Status, error) {
	m.logger.Info("creating group", "group", name, "count", startCount)
	gcfg := &internalConfig.NodeGroup{
		Name:  name,
		Count: startCount,
		Node:  nodeOverride,
	}
	ng := m.newNodeGroup(gcfg)

	m.mu.Lock()
	if _, exists := m.groups[name]; exists {
		m.mu.Unlock()
		return nil, ErrAlreadyExists
	}
	m.groups[name] = ng
	m.mu.Unlock()

	if startCount > 0 {
		if err := m.scaleGroup(ng, startCount); err != nil {
			m.mu.Lock()
			delete(m.groups, name)
			m.mu.Unlock()
			return nil, fmt.Errorf("starting nodes: %w", err)
		}
	}

	ng.mu.Lock()
	s := ng.status()
	ng.mu.Unlock()
	return s, nil
}

// Delete shuts down all nodes in the named group and removes it. Returns
// ErrNotFound if the group does not exist.
func (m *Manager) Delete(name string) error {
	m.logger.Info("deleting group", "group", name)
	m.mu.Lock()
	ng, ok := m.groups[name]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.groups, name)
	m.mu.Unlock()

	ng.mu.Lock()
	defer ng.mu.Unlock()
	for idx, node := range ng.nodes {
		nodeName := fmt.Sprintf("%s-%d", ng.name, idx)
		if err := node.Shutdown(); err != nil {
			m.logger.Warn("error stopping node during delete", "node_name", nodeName, "error", err)
		}
		delete(ng.nodes, idx)
	}
	return nil
}

// newNodeGroup constructs a NodeGroup from its config, merging the group's
// node{} block over the base config.
func (m *Manager) newNodeGroup(gcfg *internalConfig.NodeGroup) *NodeGroup {
	effectiveCfg := *m.baseCfg
	if gcfg.Node != nil {
		effectiveCfg.Node = m.baseCfg.Node.Merge(gcfg.Node)
	}
	return &NodeGroup{
		name:         gcfg.Name,
		nodes:        make(map[int]*simnode.Node),
		effectiveCfg: &effectiveCfg,
	}
}

// scaleGroup reconciles a group to the desired count. Must not be called with
// ng.mu held.
func (m *Manager) scaleGroup(ng *NodeGroup, count int) error {
	ng.mu.Lock()
	defer ng.mu.Unlock()

	ng.desired = count
	current := len(ng.nodes)

	// Start nodes for new indices in parallel.
	if count > current {
		type result struct {
			idx  int
			node *simnode.Node
			err  error
		}
		ch := make(chan result, count-current)
		for i := current; i < count; i++ {
			i := i
			nodeName := fmt.Sprintf("%s-%d", ng.name, i)
			go func() {
				node, err := nodefactory.Build(m.logger, m.buildInfo, ng.effectiveCfg, nodeName)
				ch <- result{idx: i, node: node, err: err}
			}()
		}
		var errs []error
		for i := current; i < count; i++ {
			r := <-ch
			if r.err != nil {
				errs = append(errs, fmt.Errorf("starting node %s-%d: %w", ng.name, r.idx, r.err))
				continue
			}
			ng.nodes[r.idx] = r.node
			m.logger.Info("started group node", "group", ng.name,
				"node_name", fmt.Sprintf("%s-%d", ng.name, r.idx),
				"node_id", r.node.Client.NodeID())
		}
		if len(errs) > 0 {
			return errors.Join(errs...)
		}
	}

	// Stop nodes for removed indices in parallel.
	if count < current {
		wg := &sync.WaitGroup{}
		for i := current - 1; i >= count; i-- {
			i := i
			node := ng.nodes[i]
			nodeName := fmt.Sprintf("%s-%d", ng.name, i)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := node.Shutdown(); err != nil {
					m.logger.Warn("error stopping group node", "group", ng.name,
						"node_name", nodeName, "error", err)
				} else {
					m.logger.Info("stopped group node", "group", ng.name, "node_name", nodeName)
				}
			}()
			delete(ng.nodes, i)
		}
		wg.Wait()
	}

	return nil
}
