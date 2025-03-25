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

// Package kubeconfig provides a Kubernetes cluster provider that watches secrets
// containing kubeconfig data and creates controller-runtime clusters for each.
package kubeconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

const (
	// DefaultKubeconfigSecretLabel is the default label key to identify kubeconfig secrets
	DefaultKubeconfigSecretLabel = "sigs.k8s.io/multicluster-runtime-kubeconfig"

	// DefaultKubeconfigSecretKey is the default key in the secret data that contains the kubeconfig
	DefaultKubeconfigSecretKey = "kubeconfig"
)

var _ multicluster.Provider = &Provider{}

// New creates a new Kubeconfig Provider.
func New(mgr mcmanager.Manager, opts Options) *Provider {
	// Set defaults
	if opts.KubeconfigLabel == "" {
		opts.KubeconfigLabel = DefaultKubeconfigSecretLabel
	}
	if opts.KubeconfigKey == "" {
		opts.KubeconfigKey = DefaultKubeconfigSecretKey
	}
	if opts.ConnectionTimeout == 0 {
		opts.ConnectionTimeout = 10 * time.Second
	}
	if opts.CacheSyncTimeout == 0 {
		opts.CacheSyncTimeout = 30 * time.Second
	}

	var client client.Client
	if mgr != nil {
		client = mgr.GetLocalManager().GetClient()
	}

	return &Provider{
		opts:        opts,
		log:         log.Log.WithName("kubeconfig-provider"),
		client:      client,
		clusters:    map[string]cluster.Cluster{},
		cancelFns:   map[string]context.CancelFunc{},
		seenHashes:  map[string]string{},
		readySignal: make(chan struct{}),
	}
}

// Options are the options for the Kubeconfig Provider.
type Options struct {
	// Namespace to watch for kubeconfig secrets
	Namespace string

	// Label key to identify kubeconfig secrets
	KubeconfigLabel string

	// Key in the secret data that contains the kubeconfig
	KubeconfigKey string

	// ConnectionTimeout is the timeout for connecting to a cluster
	ConnectionTimeout time.Duration

	// CacheSyncTimeout is the timeout for waiting for the cache to sync
	CacheSyncTimeout time.Duration
}

type index struct {
	object       client.Object
	field        string
	extractValue client.IndexerFunc
}

// Provider is a cluster provider that watches for secrets containing kubeconfig data
// and engages clusters based on those kubeconfigs.
type Provider struct {
	opts        Options
	log         logr.Logger
	client      client.Client
	lock        sync.RWMutex
	clusters    map[string]cluster.Cluster
	cancelFns   map[string]context.CancelFunc
	indexers    []index
	seenHashes  map[string]string // tracks resource versions
	readySignal chan struct{}     // Signal when provider is ready to start
	readyOnce   sync.Once         // Ensure we only signal once
}

// IsReady returns a channel that will be closed when the provider is ready to start
func (p *Provider) IsReady() <-chan struct{} {
	return p.readySignal
}

// Get returns the cluster with the given name, if it is known.
func (p *Provider) Get(ctx context.Context, clusterName string) (cluster.Cluster, error) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	if cl, ok := p.clusters[clusterName]; ok {
		return cl, nil
	}

	return nil, fmt.Errorf("cluster %s not found", clusterName)
}

// Run starts the provider and blocks, watching for kubeconfig secrets.
func (p *Provider) Run(ctx context.Context, mgr mcmanager.Manager) error {
	p.log.Info("Starting kubeconfig provider", "namespace", p.opts.Namespace, "label", p.opts.KubeconfigLabel)

	// If client isn't set yet, get it from the manager
	if p.client == nil && mgr != nil {
		p.client = mgr.GetLocalManager().GetClient()
	}

	// Signal that we're starting up
	p.readyOnce.Do(func() {
		p.log.Info("Signaling that KubeconfigProvider is ready to start")
		close(p.readySignal)
	})

	// Wait for the controller-runtime cache to be ready before using it
	if mgr != nil && mgr.GetLocalManager().GetCache() != nil {
		p.log.Info("Waiting for controller-runtime cache to be ready")
		if !mgr.GetLocalManager().GetCache().WaitForCacheSync(ctx) {
			return fmt.Errorf("timed out waiting for cache to sync")
		}
		p.log.Info("Controller-runtime cache is synced")
	} else {
		p.log.Info("No manager or cache available, skipping cache sync")
	}

	// Do initial sync using direct API call
	if err := p.syncSecretsInternal(ctx); err != nil {
		p.log.Error(err, "initial secret sync failed")
		// Continue anyway - don't exit on sync failure
	} else {
		p.log.Info("Initial secret sync successful")
	}

	// Create a Kubernetes clientset for watching
	var config *rest.Config
	var err error

	// Try to get in-cluster config
	config, err = rest.InClusterConfig()
	if err != nil {
		p.log.Info("not running in-cluster, using kubeconfig for local development")

		// Look for kubeconfig in default locations
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
		config, err = clientConfig.ClientConfig()
		if err != nil {
			return fmt.Errorf("failed to create config: %w", err)
		}
	}

	p.log.Info("Successfully connected to kubernetes api", "host", config.Host)

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create clientset: %w", err)
	}

	// Set up label selector for our kubeconfig label
	labelSelector := fmt.Sprintf("%s=true", p.opts.KubeconfigLabel)
	p.log.Info("Watching for kubeconfig secrets", "selector", labelSelector)

	// Watch for secret changes
	return p.watchSecrets(ctx, clientset, labelSelector, mgr)
}

// watchSecrets sets up a watch for Secret resources with the given label selector
func (p *Provider) watchSecrets(ctx context.Context, clientset kubernetes.Interface, labelSelector string, mgr mcmanager.Manager) error {
	watcher, err := clientset.CoreV1().Secrets(p.opts.Namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return fmt.Errorf("failed to watch secrets: %w", err)
	}
	defer watcher.Stop()

	p.log.Info("Started watching for kubeconfig secrets")

	for {
		select {
		case <-ctx.Done():
			p.log.Info("Context cancelled, stopping watch")
			return ctx.Err()
		case event, ok := <-watcher.ResultChan():
			if !ok {
				p.log.Info("Watch channel closed, restarting watch")
				// Recreate the watcher
				newWatcher, err := clientset.CoreV1().Secrets(p.opts.Namespace).Watch(ctx, metav1.ListOptions{
					LabelSelector: labelSelector,
				})
				if err != nil {
					p.log.Error(err, "Failed to restart watch, waiting before retry")
					time.Sleep(5 * time.Second)
					continue
				}
				watcher = newWatcher
				continue
			}

			// Process the event
			switch event.Type {
			case watch.Added, watch.Modified:
				secret, ok := event.Object.(*corev1.Secret)
				if !ok {
					p.log.Info("Unexpected object type", "type", fmt.Sprintf("%T", event.Object))
					continue
				}
				p.log.Info("Processing secret event", "name", secret.Name, "event", event.Type)
				if err := p.handleSecret(ctx, secret, mgr); err != nil {
					p.log.Error(err, "Failed to handle secret", "name", secret.Name)
				}
			case watch.Deleted:
				secret, ok := event.Object.(*corev1.Secret)
				if !ok {
					p.log.Info("Unexpected object type", "type", fmt.Sprintf("%T", event.Object))
					continue
				}
				p.log.Info("Secret deleted", "name", secret.Name)
				p.handleSecretDelete(secret)
			case watch.Error:
				p.log.Error(fmt.Errorf("watch error"), "Error event received")
			}
		}
	}
}

// handleSecret processes a secret containing kubeconfig data
func (p *Provider) handleSecret(ctx context.Context, secret *corev1.Secret, mgr mcmanager.Manager) error {
	if secret == nil {
		return fmt.Errorf("received nil secret")
	}

	// Extract name to use as cluster name
	clusterName := secret.Name
	log := p.log.WithValues("cluster", clusterName, "secret", fmt.Sprintf("%s/%s", secret.Namespace, secret.Name))

	// Check if this secret has kubeconfig data
	kubeconfigData, ok := secret.Data[p.opts.KubeconfigKey]
	if !ok {
		log.Info("Secret does not contain kubeconfig data", "key", p.opts.KubeconfigKey)
		return nil
	}

	// Hash the kubeconfig to detect changes
	dataHash := hashBytes(kubeconfigData)

	// Check if we've seen this version before
	p.lock.RLock()
	existingHash, exists := p.seenHashes[clusterName]
	p.lock.RUnlock()

	if exists && existingHash == dataHash {
		log.Info("Kubeconfig unchanged, skipping")
		return nil
	}

	// Parse the kubeconfig
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return fmt.Errorf("failed to parse kubeconfig: %w", err)
	}

	// Set reasonable defaults for the client
	restConfig.Timeout = p.opts.ConnectionTimeout

	// Check if we already have this cluster
	p.lock.RLock()
	_, clusterExists := p.clusters[clusterName]
	p.lock.RUnlock()

	// If the cluster already exists, remove it first
	if clusterExists {
		log.Info("Cluster already exists, updating it")
		if err := p.removeCluster(ctx, clusterName); err != nil {
			return fmt.Errorf("failed to remove existing cluster: %w", err)
		}
	}

	// Create a new cluster
	log.Info("Creating new cluster from kubeconfig")
	cl, err := cluster.New(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create cluster: %w", err)
	}

	// Apply any field indexers
	for _, idx := range p.indexers {
		if err := cl.GetFieldIndexer().IndexField(ctx, idx.object, idx.field, idx.extractValue); err != nil {
			return fmt.Errorf("failed to index field %q: %w", idx.field, err)
		}
	}

	// Create a context that will be canceled when this cluster is removed
	clusterCtx, cancel := context.WithCancel(ctx)

	// Start the cluster
	go func() {
		if err := cl.Start(clusterCtx); err != nil {
			log.Error(err, "Failed to start cluster")
		}
	}()

	// Wait for cache to sync
	log.Info("Waiting for cluster cache to sync", "timeout", p.opts.CacheSyncTimeout)
	syncCtx, syncCancel := context.WithTimeout(ctx, p.opts.CacheSyncTimeout)
	defer syncCancel()

	if !cl.GetCache().WaitForCacheSync(syncCtx) {
		cancel() // Cancel the cluster context
		return fmt.Errorf("timeout waiting for cache to sync")
	}

	// Store the cluster
	p.lock.Lock()
	p.clusters[clusterName] = cl
	p.cancelFns[clusterName] = cancel
	p.seenHashes[clusterName] = dataHash
	p.lock.Unlock()

	log.Info("Successfully added cluster")

	// Engage the manager if provided
	if mgr != nil {
		if err := mgr.Engage(clusterCtx, clusterName, cl); err != nil {
			log.Error(err, "Failed to engage manager, removing cluster")
			p.lock.Lock()
			delete(p.clusters, clusterName)
			delete(p.cancelFns, clusterName)
			delete(p.seenHashes, clusterName)
			p.lock.Unlock()
			cancel() // Cancel the cluster context
			return fmt.Errorf("failed to engage manager: %w", err)
		}
		log.Info("Successfully engaged manager")
	}

	return nil
}

// handleSecretDelete handles the deletion of a secret
func (p *Provider) handleSecretDelete(secret *corev1.Secret) {
	if secret == nil {
		return
	}

	clusterName := secret.Name
	log := p.log.WithValues("cluster", clusterName)

	log.Info("Handling deleted secret")

	// Remove the cluster
	if err := p.removeCluster(context.Background(), clusterName); err != nil {
		log.Error(err, "Failed to remove cluster")
	}
}

// removeCluster removes a cluster by name
func (p *Provider) removeCluster(ctx context.Context, clusterName string) error {
	log := p.log.WithValues("cluster", clusterName)
	log.Info("Removing cluster")

	// Find the cluster and cancel function
	p.lock.RLock()
	_, exists := p.clusters[clusterName]
	if !exists {
		p.lock.RUnlock()
		return fmt.Errorf("cluster %s not found", clusterName)
	}

	// Get the cancel function
	cancelFn, exists := p.cancelFns[clusterName]
	if !exists {
		p.lock.RUnlock()
		return fmt.Errorf("cancel function for cluster %s not found", clusterName)
	}
	p.lock.RUnlock()

	// Cancel the context to trigger cleanup for this cluster
	cancelFn()
	log.Info("Cancelled cluster context")

	// Clean up our maps
	p.lock.Lock()
	delete(p.clusters, clusterName)
	delete(p.cancelFns, clusterName)
	delete(p.seenHashes, clusterName)
	p.lock.Unlock()

	log.Info("Successfully removed cluster")
	return nil
}

// syncSecretsInternal performs an initial sync of all secrets with the kubeconfig label
func (p *Provider) syncSecretsInternal(ctx context.Context) error {
	if p.client == nil {
		return fmt.Errorf("client not initialized")
	}

	// List all secrets with our label
	var secrets corev1.SecretList
	if err := p.client.List(ctx, &secrets, client.InNamespace(p.opts.Namespace), client.MatchingLabels{
		p.opts.KubeconfigLabel: "true",
	}); err != nil {
		return fmt.Errorf("failed to list secrets: %w", err)
	}

	p.log.Info("Found secrets with kubeconfig label", "count", len(secrets.Items))

	// Process each secret
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		p.log.Info("Processing secret during initial sync", "name", secret.Name)
		if err := p.handleSecret(ctx, secret, nil); err != nil {
			p.log.Error(err, "Failed to handle secret during initial sync", "name", secret.Name)
			// Continue with other secrets
		}
	}

	return nil
}

// IndexField indexes a field on all clusters, existing and future.
func (p *Provider) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	// Save for future clusters
	p.indexers = append(p.indexers, index{
		object:       obj,
		field:        field,
		extractValue: extractValue,
	})

	// Apply to existing clusters
	for name, cl := range p.clusters {
		if err := cl.GetFieldIndexer().IndexField(ctx, obj, field, extractValue); err != nil {
			return fmt.Errorf("failed to index field %q on cluster %q: %w", field, name, err)
		}
	}

	return nil
}

// ListClusters returns a list of all discovered clusters.
func (p *Provider) ListClusters() map[string]cluster.Cluster {
	p.lock.RLock()
	defer p.lock.RUnlock()

	// Return a copy of the map to avoid race conditions
	result := make(map[string]cluster.Cluster, len(p.clusters))
	for k, v := range p.clusters {
		result[k] = v
	}
	return result
}

// hashBytes returns a hex-encoded SHA256 hash of the given bytes
func hashBytes(data []byte) string {
	h := sha256.New()
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
