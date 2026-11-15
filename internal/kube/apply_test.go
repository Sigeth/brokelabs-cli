package kube

import (
	"context"
	"os"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestApplyCRDThenObject applies a new CustomResourceDefinition and an object
// of its kind in one call, on the cluster of the current context:
// OPSLAB_TEST_KUBE=1 go test ./internal/kube -run TestApplyCRDThenObject
func TestApplyCRDThenObject(t *testing.T) {
	if os.Getenv("OPSLAB_TEST_KUBE") == "" {
		t.Skip("set OPSLAB_TEST_KUBE=1 to run against the current cluster")
	}
	c, err := Connect("", "")
	if err != nil {
		t.Fatal(err)
	}
	objs, err := Objects([]byte(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: widgets.applytest.opslab.io}
spec:
  group: applytest.opslab.io
  names: {kind: Widget, plural: widgets, singular: widget}
  scope: Cluster
  versions:
    - name: v1
      served: true
      storage: true
      schema: {openAPIV3Schema: {type: object, x-kubernetes-preserve-unknown-fields: true}}
---
apiVersion: applytest.opslab.io/v1
kind: Widget
metadata: {name: first}
`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = c.Dynamic.Resource(crdGVR).Delete(context.Background(), "widgets.applytest.opslab.io", metav1.DeleteOptions{})
	}()
	if err := c.Apply(context.Background(), objs); err != nil {
		t.Fatal(err)
	}
}
