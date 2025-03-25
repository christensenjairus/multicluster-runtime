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

	"golang.org/x/sync/errgroup"

	corev1 "k8s.io/api/core/v1"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
	kubeconfigprovider "sigs.k8s.io/multicluster-runtime/providers/kubeconfig"
)

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

	ctrllog.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	entryLog := ctrllog.Log.WithName("entrypoint")
	ctx := ctrl.SetupSignalHandler()

	// Create the kubeconfig provider
	provider := kubeconfigprovider.New(
		nil, // Will be set by mcmanager.New
		kubeconfigprovider.Options{
			Namespace:         namespace,
			KubeconfigLabel:   kubeconfigLabel,
			ConnectionTimeout: connectionTimeout,
			CacheSyncTimeout:  cacheSyncTimeout,
		},
	)

	// Create the multicluster manager with the provider
	mgr, err := mcmanager.New(ctrl.GetConfigOrDie(), provider, manager.Options{})
	if err != nil {
		entryLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Set up a controller to list pods in each cluster
	err = mgr.Add(mcreconcile.Func(
		func(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
			log := ctrllog.Log.WithValues("cluster", req.ClusterName)
			log.Info("Reconciling Pod")

			cl, err := mgr.GetCluster(ctx, req.ClusterName)
			if err != nil {
				return ctrl.Result{}, err
			}

			// List pods in the default namespace
			var pods corev1.PodList
			if err := cl.GetClient().List(ctx, &pods, &client.ListOptions{
				Namespace: "default",
			}); err != nil {
				return ctrl.Result{}, err
			}

			log.Info("Listing pods in default namespace", "count", len(pods.Items))
			for _, pod := range pods.Items {
				log.Info("Found pod",
					"namespace", pod.Namespace,
					"name", pod.Name,
					"status", pod.Status.Phase)
			}

			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		},
	).ToController("pod-lister", mgr))

	if err != nil {
		entryLog.Error(err, "unable to create pod lister controller")
		os.Exit(1)
	}

	// Starting everything
	g, ctx := errgroup.WithContext(ctx)

	// Wait for the provider to be ready before starting the manager
	select {
	case <-provider.IsReady():
		entryLog.Info("Provider is ready")
	case <-ctx.Done():
		entryLog.Error(ctx.Err(), "context cancelled before provider was ready")
		os.Exit(1)
	}

	g.Go(func() error {
		entryLog.Info("Starting provider")
		return ignoreCanceled(provider.Run(ctx, mgr))
	})

	g.Go(func() error {
		entryLog.Info("Starting manager")
		return ignoreCanceled(mgr.Start(ctx))
	})

	if err := g.Wait(); err != nil {
		entryLog.Error(err, "error running manager or provider")
		os.Exit(1)
	}
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
