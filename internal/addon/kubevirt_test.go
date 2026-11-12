package addon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Sigeth/opslab-cli/internal/kube"
)

func node(name string, kvm string) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{}}}
	if kvm != "" {
		n.Status.Allocatable[KVMResource] = resource.MustParse(kvm)
	}
	return n
}

func TestKVMNodes(t *testing.T) {
	client := fake.NewClientset(node("a", "1k"), node("b", ""), node("c", "0"))
	got, err := KVMNodes(context.Background(), client)
	if err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("KVMNodes = %v, %v", got, err)
	}
}

func TestRestartOperators(t *testing.T) {
	labels := map[string]string{"app.kubernetes.io/part-of": "opslab", "app.kubernetes.io/name": "operator"}
	client := fake.NewClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "opslab-operator", Namespace: "opslab-system", Labels: labels}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "opslab-system"}},
	)
	got, err := restartOperators(context.Background(), client)
	if err != nil || !slices.Equal(got, []string{"opslab-system/opslab-operator"}) {
		t.Fatalf("restarted = %v, %v", got, err)
	}
}

func TestKubeVirtCRUsesRuntimeDefaultSeccomp(t *testing.T) {
	objs, err := kube.Objects([]byte(kubeVirtCR))
	if err != nil || len(objs) != 1 {
		t.Fatalf("objects = %v, %v", objs, err)
	}
	on, _, _ := unstructured.NestedBool(objs[0].Object, "spec", "configuration", "seccompConfiguration",
		"virtualMachineInstanceProfile", "customProfile", "runtimeDefaultProfile")
	if !on || objs[0].GetNamespace() != kubeVirtNamespace {
		t.Fatalf("CR = %v", objs[0].Object)
	}
}

func TestDownloadVerifiesTheChecksum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("manifest")) }))
	defer srv.Close()
	sum := sha256.Sum256([]byte("manifest"))
	if data, err := download(context.Background(), srv.URL, hex.EncodeToString(sum[:])); err != nil || string(data) != "manifest" {
		t.Fatalf("download = %q, %v", data, err)
	}
	if _, err := download(context.Background(), srv.URL, "00"); err == nil {
		t.Fatal("a wrong checksum must be refused")
	}
}

func TestObjectsSkipsEmptyDocuments(t *testing.T) {
	objs, err := kube.Objects([]byte("---\napiVersion: v1\nkind: Namespace\nmetadata: {name: a}\n---\n---\napiVersion: v1\nkind: Namespace\nmetadata: {name: b}\n"))
	if err != nil || len(objs) != 2 || objs[1].GetName() != "b" {
		t.Fatalf("objects = %v, %v", objs, err)
	}
}
