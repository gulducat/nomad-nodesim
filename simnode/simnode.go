// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package simnode

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client"
)

type Node struct {
	Client *client.Client
	logger hclog.Logger
}

func New(c *client.Client, logger hclog.Logger) *Node {
	return &Node{
		Client: c,
		logger: logger.With("node", c.NodeID()),
	}
}

// Shutdown gracefully removes the node from the cluster and stops the client.
//
// Leave is called first, which drains any running allocations and marks the
// node ineligible for scheduling. This uses the node's own auth token, so it
// works correctly on ACL-enabled clusters. Once Leave returns (or the drain
// deadline is reached), Shutdown stops the client process. The server will
// mark the node "down" naturally once heartbeats cease.
func (n *Node) Shutdown() error {
	if err := n.Client.Leave(); err != nil {
		n.logger.Warn("error leaving cluster", "error", err)
	}
	return n.Client.Shutdown()
}
