package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Sigeth/brokelabs-cli/internal/addon"
	"github.com/Sigeth/brokelabs-cli/internal/host"
	"github.com/Sigeth/brokelabs-cli/internal/kube"
)

var errNoKVM = errors.New("this machine cannot run the vm runtime")

func newHostCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Inspect the machine the CLI runs on",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Check that this machine can run virtual machines (vm runtime)",
		Long: "Checks /dev/kvm, the CPU virtualization flags and whether this machine is itself a VM,\n" +
			"and explains what to change when VMs cannot run. Run it on a cluster node.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := host.Inspect(os.DirFS("/"))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			yes := map[bool]string{true: "yes", false: "no"}
			fmt.Fprintf(out, "/dev/kvm:        %s\n", yes[r.KVM])
			fmt.Fprintf(out, "CPU extensions:  %s\n", orNone(r.CPUFlag))
			fmt.Fprintf(out, "hypervisor:      %s\n", orNone(r.Hypervisor))
			if r.Nested != "" {
				fmt.Fprintf(out, "nested for VMs:  %s\n", r.Nested)
			}
			if r.VMReady() {
				fmt.Fprintln(out, "this machine can run the vm runtime")
				return nil
			}
			for _, line := range r.Advice() {
				fmt.Fprintln(out, line)
			}
			return errNoKVM
		},
	})
	return cmd
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func newAddonCommand() *cobra.Command {
	var kubeconfig, kubeContext string
	cmd := &cobra.Command{
		Use:   "addon",
		Short: "Install the cluster add-ons of the runtimes",
	}
	cmd.PersistentFlags().StringVar(&kubeconfig, "kubeconfig", "", "kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	cmd.PersistentFlags().StringVar(&kubeContext, "context", "", "kubeconfig context (default: the current one)")

	kubevirt := &cobra.Command{
		Use:   "kubevirt",
		Short: "KubeVirt, for the vm runtime (" + addon.KubeVirtVersion + ")",
	}
	var opts addon.KubeVirtOptions
	install := &cobra.Command{
		Use:   "install",
		Short: "Install or upgrade KubeVirt, configured for brokelabs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := kube.Connect(kubeconfig, kubeContext)
			if err != nil {
				return err
			}
			return addon.InstallKubeVirt(cmd.Context(), c, cmd.OutOrStdout(), opts)
		},
	}
	install.Flags().DurationVar(&opts.Timeout, "timeout", 10*time.Minute, "how long to wait for KubeVirt to be available")
	install.Flags().BoolVar(&opts.NoImageVolume, "no-image-volume", false,
		"run containerDisks as containers, not image volumes (dev clusters reloading VM images under the same tag)")
	status := &cobra.Command{
		Use:   "status",
		Short: "Show KubeVirt and the nodes able to run VMs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := kube.Connect(kubeconfig, kubeContext)
			if err != nil {
				return err
			}
			return addon.KubeVirtStatus(cmd.Context(), c, cmd.OutOrStdout())
		},
	}
	kubevirt.AddCommand(install, status)
	cmd.AddCommand(kubevirt)
	return cmd
}
