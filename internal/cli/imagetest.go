package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing/fstest"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/Sigeth/opslab-core-go/api/v1alpha1"
	"github.com/Sigeth/opslab-core-go/pkg/challenge"
	"github.com/Sigeth/opslab-core-go/pkg/project"

	"github.com/Sigeth/opslab-cli/internal/kube"
)

// defaultInstancesNamespace holds ChallengeInstances and CheckRuns.
const defaultInstancesNamespace = project.Name + "-instances"

var errContract = errors.New("the image does not meet the base image contract")

// contractPackage is a challenge whose only machine boots the image under
// test without network access. Provisioning checks most of the contract
// (cloud-init done, SSH as the checker with the pinned host key, sudo for
// setup); its tasks check the rest.
const contractPackage = `apiVersion: opslab.io/v1alpha1
kind: ChallengePackage
metadata: {name: NAME, version: 0.0.0}
spec:
  title: Base image contract test
  summary: Boots a VM image and checks the base image contract.
  category: linux-hardening
  difficulty: easy
  license: MIT
  permissions: {egress: none}
  lifetime: {default: 30m, max: 30m}
  machines:
    - {name: vm, runtime: vm, image: IMAGE, resources: {cpu: "1", memory: 1Gi}, setup: [setup.sh]}
  access:
    terminals: [{machine: vm}]
  tasks:
    - id: student-ssh
      title: The student logs in over SSH with the instance key
      points: 1
      check: {machine: vm, sshLogin: {user: student, expect: allowed}}
    - id: student-uid
      title: The student is UID 1000
      points: 1
      check: {machine: vm, shell: {run: "id -u student", expectOutput: "^1000\\s*$"}}
    - id: checker-root
      title: Checks run as root through sudo
      points: 1
      check: {machine: vm, shell: {run: "id -u", expectOutput: "^0\\s*$"}}
    - id: cloud-init
      title: cloud-init finished without error
      points: 1
      check: {machine: vm, shell: {run: "cloud-init status", expectOutput: "status: done"}}
`

var (
	challengeInstanceGVR = schema.GroupVersionResource{Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Resource: "challengeinstances"}
	checkRunGVR          = schema.GroupVersionResource{Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Resource: "checkruns"}
	challengeGVR         = schema.GroupVersionResource{Group: v1alpha1.GroupVersion.Group, Version: v1alpha1.GroupVersion.Version, Resource: "challenges"}
)

func newImageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Work with VM images",
	}
	var (
		kubeconfig, kubeContext, systemNS, instancesNS string
		timeout                                        time.Duration
		keep                                           bool
	)
	test := &cobra.Command{
		Use:   "test IMAGE",
		Short: "Boot a VM image on the cluster and check the base image contract",
		Long: "Installs a throwaway challenge whose only machine boots IMAGE (a containerDisk) without\n" +
			"network access, waits until the platform has set it up, runs the contract checks, and\n" +
			"removes everything. Needs opslab with KubeVirt on the cluster of the current context.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := kube.Connect(kubeconfig, kubeContext)
			if err != nil {
				return err
			}
			t := imageTest{cluster: c, out: cmd.OutOrStdout(), image: args[0], systemNS: systemNS, instancesNS: instancesNS, timeout: timeout}
			return t.run(cmd.Context(), keep)
		},
	}
	test.Flags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	test.Flags().StringVar(&kubeContext, "context", "", "kubeconfig context (default: the current one)")
	test.Flags().StringVar(&systemNS, "system-namespace", defaultSystemNamespace, "namespace of the opslab operator")
	test.Flags().StringVar(&instancesNS, "instances-namespace", defaultInstancesNamespace, "namespace of the instances")
	test.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait for the machine")
	test.Flags().BoolVar(&keep, "keep", false, "keep the instance and challenge (to investigate)")
	cmd.AddCommand(test)
	return cmd
}

type imageTest struct {
	cluster               *kube.Cluster
	out                   io.Writer
	image                 string
	systemNS, instancesNS string
	timeout               time.Duration
}

// testName names the throwaway objects of one run: unique, since the
// namespace of a previous run may still be terminating.
func testName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "image-test-" + hex.EncodeToString(b)
}

func (t imageTest) run(ctx context.Context, keep bool) error {
	name := testName()
	pkg := strings.NewReplacer("NAME", name, "IMAGE", t.image).Replace(contractPackage)
	files := fstest.MapFS{
		challenge.ManifestFile: {Data: []byte(pkg)},
		"instructions/en.md":   {Data: []byte("# Base image contract test\n")},
		"setup.sh":             {Data: []byte("#!/bin/sh\n# Runs as root through the checker's sudo.\n[ \"$(id -u)\" = 0 ]\n")},
	}
	cm, ch, err := inlineChallenge(files, "image test", t.systemNS)
	if err != nil {
		return err
	}
	objs, err := toUnstructured(cm, ch)
	if err != nil {
		return err
	}
	if err := t.cluster.Apply(ctx, objs); err != nil {
		return err
	}
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "ChallengeInstance",
		"metadata": map[string]any{"name": name, "namespace": t.instancesNS},
		"spec":     map[string]any{"challenge": name, "owner": map[string]any{"userID": project.Name + "-cli", "displayName": "image test"}},
	}}
	if !keep {
		defer t.cleanup(name, cm.Name)
	}
	// Wait for the challenge to be installed before starting an instance.
	if err := t.waitChallenge(ctx, name); err != nil {
		return err
	}
	if err := t.cluster.Apply(ctx, []*unstructured.Unstructured{instance}); err != nil {
		return err
	}

	fmt.Fprintf(t.out, "booting %s\n", t.image)
	start := time.Now()
	if err := t.waitInstance(ctx, name); err != nil {
		return err
	}
	fmt.Fprintf(t.out, "set up after %s: cloud-init done, SSH as %s with the pinned host key, sudo works\n",
		time.Since(start).Round(time.Second), v1alpha1.CheckerUser)

	run := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(), "kind": "CheckRun",
		"metadata": map[string]any{"name": name + "-1", "namespace": t.instancesNS},
		"spec":     map[string]any{"instance": name},
	}}
	if err := t.cluster.Apply(ctx, []*unstructured.Unstructured{run}); err != nil {
		return err
	}
	results, err := t.waitCheckRun(ctx, name+"-1")
	if err != nil {
		return err
	}
	failed := 0
	for _, r := range results {
		mark := "ok  "
		if !r.Passed {
			mark, failed = "FAIL", failed+1
		}
		fmt.Fprintf(t.out, "%s %s: %s\n", mark, r.Task, r.Message)
	}
	if failed > 0 {
		return fmt.Errorf("%w: %d check(s) failed", errContract, failed)
	}
	fmt.Fprintf(t.out, "%s meets the base image contract\n", t.image)
	return nil
}

func (t imageTest) waitChallenge(ctx context.Context, name string) error {
	return t.poll(ctx, func() (bool, error) {
		obj, err := t.cluster.Dynamic.Resource(challengeGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		var ch v1alpha1.Challenge
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &ch); err != nil {
			return false, err
		}
		if ch.Status.Package != nil && !ch.Status.Runnable {
			return false, fmt.Errorf("the cluster cannot run vm machines: %s", ch.Status.UnrunnableReason)
		}
		return ch.Status.Runnable, nil
	})
}

func (t imageTest) waitInstance(ctx context.Context, name string) error {
	last := ""
	return t.poll(ctx, func() (bool, error) {
		obj, err := t.cluster.Dynamic.Resource(challengeInstanceGVR).Namespace(t.instancesNS).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		var inst v1alpha1.ChallengeInstance
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &inst); err != nil {
			return false, err
		}
		if msg := inst.Status.Message; msg != "" && msg != last {
			fmt.Fprintf(t.out, "  %s\n", msg)
			last = msg
		}
		switch inst.Status.Phase {
		case v1alpha1.InstanceReady:
			return true, nil
		case v1alpha1.InstanceFailed, v1alpha1.InstanceExpired:
			return false, fmt.Errorf("%w: %s", errContract, inst.Status.Message)
		}
		return false, nil
	})
}

func (t imageTest) waitCheckRun(ctx context.Context, name string) ([]v1alpha1.TaskResult, error) {
	var results []v1alpha1.TaskResult
	err := t.poll(ctx, func() (bool, error) {
		obj, err := t.cluster.Dynamic.Resource(checkRunGVR).Namespace(t.instancesNS).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		var run v1alpha1.CheckRun
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &run); err != nil {
			return false, err
		}
		switch run.Status.Phase {
		case v1alpha1.CheckRunCompleted:
			results = run.Status.Results
			return true, nil
		case v1alpha1.CheckRunError:
			return false, fmt.Errorf("the checks could not run: %s", run.Status.Message)
		}
		return false, nil
	})
	return results, err
}

// poll calls done every 3 seconds until it returns true or an error, or the
// timeout.
func (t imageTest) poll(ctx context.Context, done func() (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	for {
		ok, err := done()
		if ok || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: timed out after %s", errContract, t.timeout)
		case <-time.After(3 * time.Second):
		}
	}
}

// cleanup removes the throwaway objects (best effort).
func (t imageTest) cleanup(name, configMap string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = t.cluster.Dynamic.Resource(challengeInstanceGVR).Namespace(t.instancesNS).Delete(ctx, name, metav1.DeleteOptions{})
	_ = t.cluster.Dynamic.Resource(challengeGVR).Delete(ctx, name, metav1.DeleteOptions{})
	_ = t.cluster.Client.CoreV1().ConfigMaps(t.systemNS).Delete(ctx, configMap, metav1.DeleteOptions{})
}

func toUnstructured(objs ...runtime.Object) ([]*unstructured.Unstructured, error) {
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
		if err != nil {
			return nil, err
		}
		u := &unstructured.Unstructured{Object: m}
		unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
		unstructured.RemoveNestedField(u.Object, "status")
		out = append(out, u)
	}
	return out, nil
}
