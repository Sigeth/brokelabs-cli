package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing/fstest"
	"time"

	"github.com/spf13/cobra"

	"github.com/Sigeth/opslab-core-go/api/v1alpha1"
	"github.com/Sigeth/opslab-core-go/pkg/challenge"

	"github.com/Sigeth/opslab-cli/internal/kube"
)

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
			t := imageTest{platform: platform{cluster: c, out: cmd.OutOrStdout(), systemNS: systemNS, instancesNS: instancesNS, timeout: timeout}, image: args[0]}
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
	platform
	image string
}

func (t imageTest) run(ctx context.Context, keep bool) error {
	name := "image-test-" + randomSuffix()
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
	if !keep {
		defer t.deleteChallenge(name, cm.Name)
		defer t.deleteInstance(name)
	}
	// Wait for the challenge to be installed before starting an instance.
	if err := t.waitChallenge(ctx, name); err != nil {
		return fmt.Errorf("%w: %v", errContract, err)
	}
	if err := t.startInstance(ctx, name, name, "image test"); err != nil {
		return err
	}

	fmt.Fprintf(t.out, "booting %s\n", t.image)
	start := time.Now()
	if _, err := t.waitInstance(ctx, name); err != nil {
		return fmt.Errorf("%w: %v", errContract, err)
	}
	fmt.Fprintf(t.out, "set up after %s: cloud-init done, SSH as %s with the pinned host key, sudo works\n",
		time.Since(start).Round(time.Second), v1alpha1.CheckerUser)

	results, err := t.check(ctx, name, 1)
	if err != nil {
		return fmt.Errorf("%w: %v", errContract, err)
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
