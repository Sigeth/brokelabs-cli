// Package addon installs the cluster add-ons the runtimes need. They have
// their own lifecycles, so the platform chart does not ship them.
package addon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/Sigeth/brokelabs-core-go/pkg/project"

	"github.com/Sigeth/brokelabs-cli/internal/kube"
)

// The KubeVirt release installed, pinned with the checksum of its operator
// manifest.
const (
	KubeVirtVersion        = "v1.9.1"
	kubeVirtOperatorSHA256 = "9ac45a436c49d32e6fc21d542bcb7600b970705297b9e6752f3ef9b0a1d292da"
	kubeVirtNamespace      = "kubevirt"
)

// KubeVirtOperatorURL is where the operator manifest of KubeVirtVersion is
// downloaded from.
var KubeVirtOperatorURL = "https://github.com/kubevirt/kubevirt/releases/download/" + KubeVirtVersion + "/kubevirt-operator.yaml"

// kubeVirtCR configures KubeVirt for BrokeLabs: the RuntimeDefault seccomp
// profile makes the VMs' launcher pods pass the "restricted" Pod Security
// Standard of instance namespaces (D33).
const kubeVirtCR = `apiVersion: kubevirt.io/v1
kind: KubeVirt
metadata: {name: kubevirt, namespace: kubevirt}
spec:
  certificateRotateStrategy: {}
  customizeComponents: {}
  workloadUpdateStrategy: {}
  configuration:
    seccompConfiguration:
      virtualMachineInstanceProfile:
        customProfile: {runtimeDefaultProfile: true}
`

// KVMResource is the node resource KubeVirt exposes on nodes with /dev/kvm.
const KVMResource corev1.ResourceName = "devices.kubevirt.io/kvm"

var kubeVirtGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}

// KubeVirtOptions tune the installation.
type KubeVirtOptions struct {
	// NoImageVolume makes KubeVirt run containerDisks as containers instead
	// of Kubernetes image volumes. For dev clusters loading rebuilt VM images
	// under the same tag: k3s' containerd then fails to mount the new image
	// as a volume (ImageVolumeMountFailed) until it restarts. Image volumes
	// are the safer default (no bind mounts by virt-handler).
	NoImageVolume bool
	// Timeout of the wait for KubeVirt to be available.
	Timeout time.Duration
}

// kubeVirtCRFor returns the KubeVirt CR for the options.
func kubeVirtCRFor(opts KubeVirtOptions) string {
	if !opts.NoImageVolume {
		return kubeVirtCR
	}
	return kubeVirtCR + "    developerConfiguration:\n      disabledFeatureGates: [ImageVolume]\n"
}

// InstallKubeVirt installs or upgrades KubeVirt and waits until it is
// available. It then restarts the BrokeLabs operator, which detects KubeVirt
// at startup.
func InstallKubeVirt(ctx context.Context, c *kube.Cluster, out io.Writer, opts KubeVirtOptions) error {
	timeout := opts.Timeout
	fmt.Fprintf(out, "downloading KubeVirt %s\n", KubeVirtVersion)
	manifest, err := download(ctx, KubeVirtOperatorURL, kubeVirtOperatorSHA256)
	if err != nil {
		return err
	}
	objs, err := kube.Objects(manifest)
	if err != nil {
		return fmt.Errorf("decoding the KubeVirt manifest: %w", err)
	}
	cr, err := kube.Objects([]byte(kubeVirtCRFor(opts)))
	if err != nil {
		return err
	}
	if err := c.Apply(ctx, append(objs, cr...)); err != nil {
		return err
	}

	fmt.Fprintln(out, "waiting for KubeVirt to be available")
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		kv, err := c.Dynamic.Resource(kubeVirtGVR).Namespace(kubeVirtNamespace).Get(wctx, "kubevirt", metav1.GetOptions{})
		if err == nil && conditionTrue(kv, "Available") {
			break
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("KubeVirt was not available after %s (kubectl -n %s get pods)", timeout, kubeVirtNamespace)
		case <-time.After(5 * time.Second):
		}
	}

	restarted, err := restartOperators(ctx, c.Client)
	if err != nil {
		return err
	}
	for _, d := range restarted {
		fmt.Fprintf(out, "restarted %s so it detects KubeVirt\n", d)
	}
	return reportKVM(ctx, c.Client, out)
}

// KubeVirtStatus prints whether KubeVirt runs and which nodes can run VMs.
func KubeVirtStatus(ctx context.Context, c *kube.Cluster, out io.Writer) error {
	kv, err := c.Dynamic.Resource(kubeVirtGVR).Namespace(kubeVirtNamespace).Get(ctx, "kubevirt", metav1.GetOptions{})
	if err != nil {
		fmt.Fprintf(out, "KubeVirt is not installed (%v): `%s addon kubevirt install`\n", err, project.Name)
		return nil
	}
	phase, _, _ := unstructured.NestedString(kv.Object, "status", "phase")
	version, _, _ := unstructured.NestedString(kv.Object, "status", "observedKubeVirtVersion")
	fmt.Fprintf(out, "KubeVirt %s: %s, available: %v\n", version, phase, conditionTrue(kv, "Available"))
	if seccomp, _, _ := unstructured.NestedBool(kv.Object, "spec", "configuration", "seccompConfiguration",
		"virtualMachineInstanceProfile", "customProfile", "runtimeDefaultProfile"); !seccomp {
		fmt.Fprintf(out, "warning: VMs do not use the RuntimeDefault seccomp profile: instance namespaces will refuse them (reinstall with `%s addon kubevirt install`)\n", project.Name)
	}
	return reportKVM(ctx, c.Client, out)
}

// reportKVM lists the nodes able to run VMs, with advice when there is none.
func reportKVM(ctx context.Context, client kubernetes.Interface, out io.Writer) error {
	nodes, err := KVMNodes(ctx, client)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		fmt.Fprintln(out, "no node exposes /dev/kvm: vm challenges cannot run on this cluster.")
		fmt.Fprintf(out, "Run `%s host check` on a node to see why (nested virtualization when the nodes are VMs).\n", project.Name)
		return nil
	}
	fmt.Fprintf(out, "nodes able to run VMs: %s\n", strings.Join(nodes, ", "))
	return nil
}

// KVMNodes returns the nodes where KubeVirt exposes /dev/kvm.
func KVMNodes(ctx context.Context, client kubernetes.Interface) ([]string, error) {
	list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range list.Items {
		if q, ok := n.Status.Allocatable[KVMResource]; ok && q.Cmp(resource.MustParse("0")) > 0 {
			names = append(names, n.Name)
		}
	}
	return names, nil
}

// restartOperators restarts the BrokeLabs operator deployments of the cluster.
func restartOperators(ctx context.Context, client kubernetes.Interface) ([]string, error) {
	list, err := client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/part-of=" + project.Name + ",app.kubernetes.io/name=operator",
	})
	if err != nil {
		return nil, err
	}
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		project.LabelPrefix+"restarted-at", time.Now().UTC().Format(time.RFC3339))
	var restarted []string
	for _, d := range list.Items {
		if _, err := client.AppsV1().Deployments(d.Namespace).Patch(ctx, d.Name, types.StrategicMergePatchType,
			[]byte(patch), metav1.PatchOptions{FieldManager: kube.FieldManager}); err != nil {
			return restarted, fmt.Errorf("restarting %s/%s: %w", d.Namespace, d.Name, err)
		}
		restarted = append(restarted, d.Namespace+"/"+d.Name)
	}
	return restarted, nil
}

func conditionTrue(obj *unstructured.Unstructured, condition string) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if ok && m["type"] == condition && m["status"] == "True" {
			return true
		}
	}
	return false
}

// download fetches url and checks its SHA-256.
func download(ctx context.Context, url, sha string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if err := verify(data, sha); err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	return data, nil
}

func verify(data []byte, want string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum %s, expected %s", got, want)
	}
	return nil
}
