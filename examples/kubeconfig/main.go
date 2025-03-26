/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	// Import your controllers here <--------------------------------
	"sigs.k8s.io/multicluster-runtime/examples/kubeconfig/controllers"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	kubeconfigprovider "sigs.k8s.io/multicluster-runtime/providers/kubeconfig"
)

func main() {
	var namespace string
	var kubeconfigLabel string
	var connectionTimeout time.Duration
	var cacheSyncTimeout time.Duration
	var kubeconfigPath string
	var providerReadyTimeout time.Duration

	flag.StringVar(&namespace, "namespace", "default", "Namespace where kubeconfig secrets are stored")
	flag.StringVar(&kubeconfigLabel, "kubeconfig-label", "sigs.k8s.io/multicluster-runtime-kubeconfig",
		"Label used to identify secrets containing kubeconfig data")
	flag.DurationVar(&connectionTimeout, "connection-timeout", 15*time.Second,
		"Timeout for connecting to a cluster")
	flag.DurationVar(&cacheSyncTimeout, "cache-sync-timeout", 60*time.Second,
		"Timeout for waiting for the cache to sync")
	flag.StringVar(&kubeconfigPath, "kubeconfig-path", "",
		"Path to kubeconfig file for test secrets (defaults to ~/.kube/config if not set)")
	flag.DurationVar(&providerReadyTimeout, "provider-ready-timeout", 120*time.Second,
		"Timeout for waiting for the provider to be ready")	

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrllog.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	entryLog := ctrllog.Log.WithName("entrypoint")
	ctx := ctrl.SetupSignalHandler()

	entryLog.Info("Starting application", "namespace", namespace, "kubeconfigLabel", kubeconfigLabel)

	// Create the kubeconfig provider with options
	providerOpts := kubeconfigprovider.Options{
		Namespace:         namespace,
		KubeconfigLabel:   kubeconfigLabel,
		ConnectionTimeout: connectionTimeout,
		CacheSyncTimeout:  cacheSyncTimeout,
		KubeconfigPath:    kubeconfigPath,
	}

	// Create the provider first, then the manager with the provider
	entryLog.Info("Creating provider")
	provider := kubeconfigprovider.New(providerOpts)

	// Create the multicluster manager with the provider
	entryLog.Info("Creating manager")

	// Modify manager options to avoid waiting for cache sync
	managerOpts := manager.Options{
		// Don't block main thread on leader election
		LeaderElection:          false,
		LeaderElectionNamespace: namespace,
		LeaderElectionID:        "multicluster-runtime-kubeconfig-leader",
	}

	mgr, err := mcmanager.New(ctrl.GetConfigOrDie(), provider, managerOpts)
	if err != nil {
		entryLog.Error(err, "Unable to create manager")
		os.Exit(1)
	}

	// Add our controllers
	entryLog.Info("Adding controllers")

	// TODO: Run your controllers here <--------------------------------
	podWatcher := controllers.NewPodWatcher(mgr)
	if err := mgr.Add(podWatcher); err != nil {
		entryLog.Error(err, "Unable to add pod watcher")
		os.Exit(1)
	}

	// Start provider in a goroutine
	entryLog.Info("Starting provider")
	go func() {
		err := provider.Run(ctx, mgr)
		if err != nil && ctx.Err() == nil {
			entryLog.Error(err, "Provider exited with error")
		}
	}()

	// Wait for the provider to be ready with a short timeout
	entryLog.Info("Waiting for provider to be ready")
	readyCtx, cancel := context.WithTimeout(ctx, providerReadyTimeout)
	defer cancel()

	select {
	case <-provider.IsReady():
		entryLog.Info("Provider is ready")
	case <-readyCtx.Done():
		entryLog.Error(readyCtx.Err(), "Timeout waiting for provider to be ready, continuing anyway")
	}

	// Start the manager
	entryLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		entryLog.Error(err, "Error running manager")
		os.Exit(1)
	}
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
