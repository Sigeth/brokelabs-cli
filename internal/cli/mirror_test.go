package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/Sigeth/opslab-cli/internal/kube"
)

func TestRegistriesConfig(t *testing.T) {
	var out bytes.Buffer
	if err := writeRegistriesConfig(&out, "10.43.0.50:5000", []string{"ghcr.io", "registry.school:5000"}); err != nil {
		t.Fatal(err)
	}
	var cfg registriesConfig
	if err := yaml.Unmarshal(out.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	ghcr := cfg.Mirrors["ghcr.io"]
	if len(ghcr.Endpoint) != 1 || ghcr.Endpoint[0] != "http://10.43.0.50:5000" || ghcr.Rewrite["^(.*)$"] != "ghcr.io/$1" {
		t.Fatalf("ghcr.io = %+v", ghcr)
	}
	if got := cfg.Mirrors["registry.school:5000"].Rewrite["^(.*)$"]; got != "registry.school_5000/$1" {
		t.Fatalf("rewrite with a port = %q", got)
	}
	for _, bad := range [][]string{{"ghcr.io/x"}, {""}} {
		if err := writeRegistriesConfig(&out, "10.43.0.50:5000", bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
	if err := writeRegistriesConfig(&out, "10.43.0.50", []string{"ghcr.io"}); err == nil || !strings.Contains(err.Error(), "host:port") {
		t.Errorf("error = %v", err)
	}
}

func TestMirrorEndpointFromTheService(t *testing.T) {
	c := &kube.Cluster{Client: fake.NewSimpleClientset(&corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: mirrorService, Namespace: "opslab-system"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.43.12.34"},
	})}
	got, err := mirrorEndpoint(context.Background(), c, "opslab-system")
	if err != nil || got != "10.43.12.34:5000" {
		t.Fatalf("endpoint = %q, %v", got, err)
	}
	if _, err := mirrorEndpoint(context.Background(), c, "elsewhere"); err == nil || !strings.Contains(err.Error(), "mirror.enabled") {
		t.Fatalf("error = %v", err)
	}
}
