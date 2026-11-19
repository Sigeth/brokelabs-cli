package addon

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/engine"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Sigeth/brokelabs-cli/internal/kube"
)

// Cilium is the cluster network (D54): it drops packets whose source is not
// the sending pod's own address, which flannel and kube-router do not. Every
// runtime where students are root (vm, userns, vcluster pods) can forge
// source addresses otherwise, past NetworkPolicies and instance isolation.
// The chart is embedded at a pinned version and rendered here (no Helm
// release), like the operator renders vcluster.
const (
	CiliumVersion   = "1.20.5"
	ciliumNamespace = "kube-system"
)

//go:embed charts/cilium-1.20.5.tgz
var ciliumChart []byte

// ciliumValues: IPs from the nodes' pod ranges (any cluster allocating them,
// k3s included), the cluster's own kube-proxy, NetworkPolicy ipBlocks that may
// select nodes (instances reach the API server by its node address, D51), one
// operator, and no Hubble (it would generate certificates at every render).
func ciliumValues() map[string]any {
	return map[string]any{
		"ipam":                 map[string]any{"mode": "kubernetes"},
		"kubeProxyReplacement": "false",
		"policyCIDRMatchMode":  []any{"nodes"},
		"operator":             map[string]any{"replicas": 1},
		"hubble":               map[string]any{"enabled": false},
	}
}

// RenderCilium renders the Cilium objects for a cluster of a Kubernetes minor version.
func RenderCilium(minor int) ([]byte, error) {
	chrt, err := loader.LoadArchive(bytes.NewReader(ciliumChart))
	if err != nil {
		return nil, fmt.Errorf("loading the Cilium chart: %w", err)
	}
	caps := common.DefaultCapabilities.Copy()
	caps.KubeVersion = common.KubeVersion{Version: fmt.Sprintf("v1.%d.0", minor), Major: "1", Minor: strconv.Itoa(minor)}
	rv, err := util.ToRenderValues(chrt, ciliumValues(), common.ReleaseOptions{Name: "cilium", Namespace: ciliumNamespace, IsInstall: true}, caps)
	if err != nil {
		return nil, err
	}
	files, err := engine.Render(chrt, rv)
	if err != nil {
		return nil, fmt.Errorf("rendering the Cilium chart: %w", err)
	}
	var out bytes.Buffer
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if strings.HasSuffix(name, ".yaml") && strings.TrimSpace(files[name]) != "" {
			fmt.Fprintf(&out, "---\n# %s\n%s\n", name, files[name])
		}
	}
	return out.Bytes(), nil
}

// CiliumOptions configure InstallCilium.
type CiliumOptions struct {
	Timeout time.Duration
}

// InstallCilium installs or upgrades Cilium and waits for its agents and
// operator. On k3s, the server runs with flannel-backend none and
// disable-network-policy.
func InstallCilium(ctx context.Context, c *kube.Cluster, out io.Writer, opts CiliumOptions) error {
	version, err := c.Client.Discovery().ServerVersion()
	if err != nil {
		return err
	}
	minor, err := strconv.Atoi(strings.TrimRight(version.Minor, "+"))
	if err != nil {
		return fmt.Errorf("reading the Kubernetes version %q: %w", version.Minor, err)
	}
	manifest, err := RenderCilium(minor)
	if err != nil {
		return err
	}
	objs, err := kube.Objects(manifest)
	if err != nil {
		return fmt.Errorf("decoding the Cilium manifest: %w", err)
	}
	fmt.Fprintf(out, "installing Cilium %s\n", CiliumVersion)
	if err := c.Apply(ctx, objs); err != nil {
		return err
	}

	fmt.Fprintln(out, "waiting for Cilium to be ready")
	wctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	for {
		ready, why := ciliumReady(wctx, c)
		if ready {
			fmt.Fprintln(out, "Cilium is ready")
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("Cilium was not ready after %s: %s (kubectl -n %s get pods -l k8s-app=cilium)", opts.Timeout, why, ciliumNamespace)
		case <-time.After(5 * time.Second):
		}
	}
}

// ciliumReady reports whether the agent runs on every node and the operator is available.
func ciliumReady(ctx context.Context, c *kube.Cluster) (bool, string) {
	ds, err := c.Client.AppsV1().DaemonSets(ciliumNamespace).Get(ctx, "cilium", metav1.GetOptions{})
	if err != nil {
		return false, err.Error()
	}
	if ds.Status.DesiredNumberScheduled == 0 || ds.Status.NumberReady < ds.Status.DesiredNumberScheduled || ds.Status.UpdatedNumberScheduled < ds.Status.DesiredNumberScheduled {
		return false, fmt.Sprintf("%d of %d agents ready", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
	}
	op, err := c.Client.AppsV1().Deployments(ciliumNamespace).Get(ctx, "cilium-operator", metav1.GetOptions{})
	if err != nil {
		return false, err.Error()
	}
	if op.Status.AvailableReplicas < 1 {
		return false, "the operator is not available"
	}
	return true, ""
}
