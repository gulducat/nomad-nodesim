// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package simnode

import (
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client"
	"github.com/hashicorp/nomad/nomad/structs"
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

func (n *Node) Shutdown() error {
	n.deregister()
	return n.Client.Shutdown()
}

// deregister sends Node.Deregister to the server so the node is immediately
// removed from the cluster rather than left in a "down" state until GC.
// Errors are logged but do not block shutdown.
func (n *Node) deregister() {
	req := &structs.NodeDeregisterRequest{
		NodeID: n.Client.NodeID(),
		WriteRequest: structs.WriteRequest{
			Region:    n.Client.Region(),
			AuthToken: n.Client.GetConfig().Node.SecretID,
		},
	}
	var resp structs.NodeUpdateResponse
	if err := n.Client.RPC("Node.Deregister", req, &resp); err != nil {
		n.logger.Warn("failed to deregister node", "error", err)
	} else {
		n.logger.Debug("deregistered node")
	}
}
