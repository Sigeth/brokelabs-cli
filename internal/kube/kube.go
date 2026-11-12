// Package kube connects to the cluster of a kubeconfig context and applies
// manifests with server-side apply, like `kubectl apply --server-side`.
package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Sigeth/opslab-core-go/pkg/project"
)

// FieldManager owns the fields the CLI applies.
const FieldManager = project.Name + "-cli"

// Cluster is a connection to one cluster.
type Cluster struct {
	Config  *rest.Config
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
	mapper  *restmapper.DeferredDiscoveryRESTMapper
}

// Connect uses the kubeconfig rules of kubectl ($KUBECONFIG, ~/.kube/config)
// and the given context (the current one when empty).
func Connect(kubeconfig, kubeContext string) (*Cluster, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading the kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	return &Cluster{
		Config: config, Client: client, Dynamic: dyn,
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
	}, nil
}

// Objects decodes a multi-document YAML manifest.
func Objects(manifest []byte) ([]*unstructured.Unstructured, error) {
	decoder := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	var objs []*unstructured.Unstructured
	for {
		var obj map[string]any
		if err := decoder.Decode(&obj); errors.Is(err, io.EOF) {
			return objs, nil
		} else if err != nil {
			return nil, err
		}
		if len(obj) > 0 {
			objs = append(objs, &unstructured.Unstructured{Object: obj})
		}
	}
}

// Apply server-side applies objects in order. CustomResourceDefinitions are
// discovered again before the objects that use them.
func (c *Cluster) Apply(ctx context.Context, objs []*unstructured.Unstructured) error {
	for _, obj := range objs {
		gvk := obj.GroupVersionKind()
		mapping, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if meta.IsNoMatchError(err) {
			c.mapper.Reset()
			mapping, err = c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", gvk.Kind, obj.GetName(), err)
		}
		var resource dynamic.ResourceInterface = c.Dynamic.Resource(mapping.Resource)
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resource = c.Dynamic.Resource(mapping.Resource).Namespace(obj.GetNamespace())
		}
		data, err := obj.MarshalJSON()
		if err != nil {
			return err
		}
		force := true
		if _, err := resource.Patch(ctx, obj.GetName(), types.ApplyPatchType, data,
			metav1.PatchOptions{FieldManager: FieldManager, Force: &force}); err != nil {
			return fmt.Errorf("applying %s %s: %w", gvk.Kind, obj.GetName(), err)
		}
		if gvk.Kind == "CustomResourceDefinition" {
			c.mapper.Reset()
		}
	}
	return nil
}
