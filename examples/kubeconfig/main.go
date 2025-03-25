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

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	kubeconfigprovider "sigs.k8s.io/multicluster-runtime/providers/kubeconfig"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = zap.New(zap.UseDevMode(true))
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	// TODO: add your CRDs here after you import them above
	// utilruntime.Must(crdv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var namespace string
	var kubeconfigLabel string
	var connectionTimeout time.Duration
	var cacheSyncTimeout time.Duration

	flag.StringVar(&namespace, "namespace", "default", "Namespace where kubeconfig secrets are stored")
	flag.StringVar(&kubeconfigLabel, "kubeconfig-label", "sigs.k8s.io/multicluster-runtime-kubeconfig",
		"Label used to identify secrets containing kubeconfig data")
	flag.DurationVar(&connectionTimeout, "connection-timeout", 15*time.Second,
		"Timeout for connecting to a cluster")
	flag.DurationVar(&cacheSyncTimeout, "cache-sync-timeout", 60*time.Second,
		"Timeout for waiting for the cache to sync")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Create a simple in-cluster config
	config, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to get kubernetes configuration")
		os.Exit(1)
	}

	// Get the root context for the whole application
	ctx := ctrl.SetupSignalHandler()

	setupLog.Info("initializing multicluster support", "namespace", namespace)

	// Create standard manager options
	mgmtOpts := manager.Options{
		Scheme: scheme,
	}

	// First, create a standard controller-runtime manager
	// This is for the conventional controller approach
	mgr, err := ctrl.NewManager(config, mgmtOpts)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Now let's set up the multicluster part
	// Create a MulticlusterReconciler that will be used by controllers and the provider
	mcReconciler := kubeconfigprovider.reconciler(mgr.GetClient(), mgr.GetScheme())

	// Create a KubeconfigClusterManager that will manage multiple clusters
	clusterManager := kubeconfigprovider.manager(mgr, mcReconciler)

	// Create and configure the kubeconfig provider with the cluster manager
	kubeconfigProvider := kubeconfigprovider.New(
		clusterManager,
		kubeconfigprovider.Options{
			Namespace:         namespace,
			KubeconfigLabel:   kubeconfigLabel,
			ConnectionTimeout: connectionTimeout,
			CacheSyncTimeout:  cacheSyncTimeout,
		},
	)

	// Start the provider in a background goroutine and wait for initial discovery
	go func() {
		setupLog.Info("starting kubeconfig provider")

		// Run the provider - it will handle initial sync internally
		err := kubeconfigProvider.Run(ctx, clusterManager)
		if err != nil && !errors.Is(err, context.Canceled) {
			setupLog.Error(err, "Error running provider")
			os.Exit(1)
		}
	}()

	// Wait for the provider to signal it's ready to start
	select {
	case <-kubeconfigProvider.IsReady():
		setupLog.Info("Kubeconfig provider starting...")
	case <-time.After(5 * time.Second):
		setupLog.Info("Timeout waiting for provider to start, continuing anyway")
	}

	// Now let's add a delay to allow provider to discover and connect to clusters
	setupLog.Info("Waiting for provider to discover clusters...")

	// TODO: set up your controllers for CRDs/CRs.
	//   Below are examples for 'FailoverGroup' and 'Failover' CRs.

	// Example of how controllers would be set up with scheme handling
	// if err := (&controller.FailoverGroupReconciler{
	// 	Client:       mgr.GetClient(),
	// 	Scheme:       mgr.GetScheme(), // Get scheme from manager
	// 	MCReconciler: mcReconciler,
	// }).SetupWithManager(mgr); err != nil {
	// 	setupLog.Error(err, "unable to create failovergroup controller", "controller", "FailoverGroup")
	// 	os.Exit(1)
	// }

	// if err := (&controller.FailoverReconciler{
	// 	Client:       mgr.GetClient(),
	// 	Scheme:       mgr.GetScheme(), // Get scheme from manager
	// 	MCReconciler: mcReconciler,
	// }).SetupWithManager(mgr); err != nil {
	// 	setupLog.Error(err, "unable to create failover controller", "controller", "Failover")
	// 	os.Exit(1)
	// }

	// Setup healthz/readyz checks
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
