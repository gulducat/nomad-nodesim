// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"

	internalConfig "github.com/hashicorp-forge/nomad-nodesim/internal/config"
	"github.com/hashicorp-forge/nomad-nodesim/internal/nodefactory"
	internalSimnode "github.com/hashicorp-forge/nomad-nodesim/internal/simnode"
	"github.com/hashicorp-forge/nomad-nodesim/nodegroup"
	"github.com/hashicorp-forge/nomad-nodesim/simnode"
	"github.com/hashicorp/go-hclog"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	flagConfig := internalConfig.Config{
		Log: &internalConfig.Log{},
		Node: &internalConfig.Node{
			Resources: &internalConfig.NodeResource{},
		},
	}

	flag.StringVar(&flagConfig.WorkDir, "work-dir", "", "working directory")
	flag.Var(&flagConfig.ServerAddr, "server-addr", "address of server's rpc port; can be specified multiple times")
	flag.StringVar(&flagConfig.NodeNamePrefix, "node-name-prefix", "", "nodes will be named [prefix]-[i]")
	flag.IntVar(&flagConfig.NodeNum, "node-num", 0, "number of client nodes")
	flag.StringVar(&flagConfig.AllocRunnerType, "alloc-runner-type", "", "the type of Nomad client alloc runner to use")

	// The CLI flags for the HCL Logger.
	flag.StringVar(&flagConfig.Log.Level, "log-level", "", "the verbosity level of logs")
	flag.BoolVar(&flagConfig.Log.JSON, "log-json", false, "output logs in a JSON format")
	flag.BoolVar(&flagConfig.Log.IncludeLocation, "log-include-location", false, "include file and line information in each log line")

	var configFile string
	flag.StringVar(&configFile, "config", "", "path to a config file to load")

	flag.Parse()

	// Instantiate our initial default config. This will be used to overlay all
	// other configs, starting with any supplied config file, then the CLI
	// flags.
	mergedConfig := internalConfig.Default()

	if configFile != "" {
		parsedConfigFile, err := internalConfig.ParseFile(configFile)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "failed parse config file: %s", err)
			os.Exit(2)
		}
		mergedConfig = mergedConfig.Merge(parsedConfigFile)
	}

	mergedConfig = mergedConfig.Merge(&flagConfig)

	// Default to a single node when no node_num or groups are configured.
	if mergedConfig.NodeNum == 0 && len(mergedConfig.Groups) == 0 {
		mergedConfig.NodeNum = 1
	}

	// Build the logger used by the nodesim application.
	logger := hclog.NewInterceptLogger(&hclog.LoggerOptions{
		Name:            "nomad-nodesim",
		Level:           hclog.LevelFromString(mergedConfig.Log.Level),
		JSONFormat:      mergedConfig.Log.JSON,
		IncludeLocation: mergedConfig.Log.IncludeLocation,
	})

	logger.Info("config",
		"dir", mergedConfig.WorkDir, "num", mergedConfig.NodeNum, "server", mergedConfig.ServerAddr,
		"id", mergedConfig.NodeNamePrefix)

	if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "canceled before clients created")
		os.Exit(2)
	}

	buildInfo, err := internalSimnode.GenerateBuildInfo(logger)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "failed to generate build info: %w", err)
		os.Exit(2)
	}

	handles := make([]*simnode.Node, mergedConfig.NodeNum)

	for i := 0; i < mergedConfig.NodeNum; i++ {

		nodeName := fmt.Sprintf("%s-%v", mergedConfig.NodeNamePrefix, i)

		// Start the simulate client; any error starting any client is
		// considered fatal to the nodesim application.
		if handles[i], err = nodefactory.Build(logger, buildInfo, mergedConfig, nodeName); err != nil {
			err = fmt.Errorf("error creating client %s: %w", nodeName, err)
			break
		}

		logger.Info("started client",
			"node_id", handles[i].Client.NodeID(), "node_name", nodeName,
			"index", i+1, "total", mergedConfig.NodeNum)

		if err = ctx.Err(); err != nil {
			break
		}
	}

	if err != nil {
		logger.Error("error creating clients", "error", err)
		shutdownNodes(logger, handles)
		logger.Info("done cleaning up client nodes")
		os.Exit(10)
	}

	logger.Info("clients started", "total", mergedConfig.NodeNum)

	// Initialize node groups and start the node group HTTP API server.
	manager := nodegroup.NewManager(mergedConfig, buildInfo, logger)
	if err := manager.InitFromConfig(); err != nil {
		logger.Error("error initializing node groups", "error", err)
		shutdownNodes(logger, handles)
		os.Exit(10)
	}

	groupServer := nodegroup.NewServer(manager)
	go func() {
		logger.Info("node group API listening", "addr", groupServer.Addr)
		if err := groupServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("node group API server error", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("interrupted; shutting down")
	_ = groupServer.Close()
	manager.Shutdown()
	shutdownNodes(logger, handles)
	logger.Info("done")
}

func shutdownNodes(logger hclog.Logger, handles []*simnode.Node) {
	wg := &sync.WaitGroup{}
	for _, h := range handles {
		if h == nil {
			continue
		}
		wg.Add(1)
		go func(n *simnode.Node) {
			defer wg.Done()
			if err := n.Shutdown(); err != nil {
				logger.Warn("error shutting down client node", "error", err, "node_id", n.Client.NodeID())
			}
		}(h)
	}
	wg.Wait()
}
