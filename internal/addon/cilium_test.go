package addon

import (
	"strings"
	"testing"

	"github.com/Sigeth/brokelabs-cli/internal/kube"
)

func TestRenderCilium(t *testing.T) {
	manifest, err := RenderCilium(37)
	if err != nil {
		t.Fatal(err)
	}
	objs, err := kube.Objects(manifest)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, o := range objs {
		kinds[o.GetKind()+"/"+o.GetName()] = true
		if o.GetAnnotations()["helm.sh/hook"] != "" {
			t.Errorf("%s %s is a Helm hook: nothing would run it", o.GetKind(), o.GetName())
		}
	}
	for _, want := range []string{"DaemonSet/cilium", "Deployment/cilium-operator", "ConfigMap/cilium-config"} {
		if !kinds[want] {
			t.Errorf("no %s", want)
		}
	}
	text := string(manifest)
	for _, want := range []string{`ipam: "kubernetes"`, `policy-cidr-match-mode: "nodes"`, "quay.io/cilium/cilium:v" + CiliumVersion} {
		if !strings.Contains(text, want) {
			t.Errorf("manifest lacks %s", want)
		}
	}
	if strings.Contains(text, "hubble-server-certs") {
		t.Error("Hubble certificates would change at every install")
	}
}
