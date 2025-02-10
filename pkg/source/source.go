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

package source

import (
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	mcreconcile "github.com/multicluster-runtime/multicluster-runtime/pkg/reconcile"
)

// Source is a source of events (e.g. Create, Update, Delete operations on Kubernetes Objects, Webhook callbacks, etc)
// which should be processed by event.EventHandlers to enqueue reconcile.Requests.
//
// * Use Kind for events originating in the cluster (e.g. Pod Create, Pod Update, Deployment Update).
//
// * Use Channel for events originating outside the cluster (e.g. GitHub Webhook callback, Polling external urls).
//
// Users may build their own Source implementations.
type Source = TypedSource[client.Object, mcreconcile.Request]

// TypedSource is a generic source of events (e.g. Create, Update, Delete operations on Kubernetes Objects, Webhook callbacks, etc)
// which should be processed by event.EventHandlers to enqueue a request.
//
// * Use Kind for events originating in the cluster (e.g. Pod Create, Pod Update, Deployment Update).
//
// * Use Channel for events originating outside the cluster (e.g. GitHub Webhook callback, Polling external urls).
//
// Users may build their own Source implementations.
type TypedSource[object client.Object, request comparable] interface {
	ForCluster(cluster.Cluster) (source.TypedSource[request], error)
}

// SyncingSource is a source that needs syncing prior to being usable. The controller
// will call its WaitForSync prior to starting workers.
type SyncingSource[object client.Object] TypedSyncingSource[object, mcreconcile.Request]

// TypedSyncingSource is a source that needs syncing prior to being usable. The controller
// will call its WaitForSync prior to starting workers.
type TypedSyncingSource[object client.Object, request comparable] interface {
	TypedSource[object, request]
	SyncingForCluster(cluster.Cluster) (source.TypedSyncingSource[request], error)
	WithProjection(func(cluster.Cluster, object) (object, error)) TypedSyncingSource[object, request]
}

// Kind creates a KindSource with the given cache provider.
func Kind[object client.Object](
	obj object,
	handler func(cluster.Cluster) handler.TypedEventHandler[object, mcreconcile.Request],
	predicates ...predicate.TypedPredicate[object],
) SyncingSource[object] {
	return TypedKind[object, mcreconcile.Request](obj, handler, predicates...)
}

// TypedKind creates a KindSource with the given cache provider.
func TypedKind[object client.Object, request comparable](
	obj object,
	handler func(cluster.Cluster) handler.TypedEventHandler[object, request],
	predicates ...predicate.TypedPredicate[object],
) TypedSyncingSource[object, request] {
	return &kind[object, request]{
		obj:        obj,
		handler:    handler,
		predicates: predicates,
		project:    func(_ cluster.Cluster, obj object) (object, error) { return obj, nil },
	}
}

type kind[object client.Object, request comparable] struct {
	obj        object
	handler    func(cl cluster.Cluster) handler.TypedEventHandler[object, request]
	predicates []predicate.TypedPredicate[object]
	project    func(cluster.Cluster, object) (object, error)
}

type clusterKind[object client.Object, request comparable] struct {
	source.TypedSyncingSource[request]
}

// WithProjection sets the projection function for the KindSource.
func (k *kind[object, request]) WithProjection(project func(cluster.Cluster, object) (object, error)) TypedSyncingSource[object, request] {
	k.project = project
	return k
}

func (k *kind[object, request]) ForCluster(cl cluster.Cluster) (source.TypedSource[request], error) {
	obj, err := k.project(cl, k.obj)
	if err != nil {
		return nil, err
	}
	return &clusterKind[object, request]{
		TypedSyncingSource: source.TypedKind(cl.GetCache(), obj, k.handler(cl), k.predicates...),
	}, nil
}

func (k *kind[object, request]) SyncingForCluster(cl cluster.Cluster) (source.TypedSyncingSource[request], error) {
	obj, err := k.project(cl, k.obj)
	if err != nil {
		return nil, err
	}
	return &clusterKind[object, request]{
		TypedSyncingSource: source.TypedKind(cl.GetCache(), obj, k.handler(cl), k.predicates...),
	}, nil
}
