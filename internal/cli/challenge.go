package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Sigeth/brokelabs-core-go/api/v1alpha1"
	"github.com/Sigeth/brokelabs-core-go/pkg/challenge"
)

func newChallengeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "challenge",
		Short: "Write, test and publish challenges",
	}
	cmd.AddCommand(newChallengeLintCommand(), newChallengeManifestCommand(), newChallengeImagesCommand(),
		newChallengeBuildCommand(), newChallengePushCommand(), newChallengeSignCommand(), newChallengeTestCommand())
	return cmd
}

func newChallengeLintCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "lint [DIR]",
		Short: "Validate a challenge package (default: current directory)",
		Long: "Validates " + challenge.ManifestFile + " and checks that every file it references exists.\n" +
			"It runs the same validation as the operator when the challenge is installed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return lint(cmd, dir)
		},
	}
}

var errInvalid = errors.New("invalid challenge package")

func lint(cmd *cobra.Command, dir string) error {
	files := os.DirFS(dir)
	pkg, err := challenge.Load(files)
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	errs := challenge.Validate(pkg, files)
	out := cmd.OutOrStdout()
	if len(errs) == 0 {
		fmt.Fprintf(out, "%s %s: valid (%d machines, %d tasks)\n",
			pkg.Metadata.Name, pkg.Metadata.Version, len(pkg.Spec.Machines), len(pkg.Spec.Tasks))
		bundle, err := challenge.Bundle(pkg, files)
		if err != nil {
			return err
		}
		perms, err := challenge.Permissions(pkg, bundle)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", errInvalid, dir, err)
		}
		printPermissions(out, perms)
		return nil
	}
	for _, e := range errs {
		fmt.Fprintf(out, "  %s\n", e.Error())
	}
	return fmt.Errorf("%w: %d problem(s) in %s", errInvalid, len(errs), dir)
}

// printPermissions shows what a package needs from the cluster, as admins
// see it before installing (D41).
func printPermissions(out io.Writer, perms *v1alpha1.RequiredPermissions) {
	runtimes := make([]string, 0, len(perms.Runtimes))
	for _, r := range perms.Runtimes {
		runtimes = append(runtimes, string(r))
	}
	fmt.Fprintf(out, "  permissions: runtimes %s; %s CPU and %s of memory per instance; egress %s\n",
		strings.Join(runtimes, ", "), perms.CPU.String(), perms.Memory.String(), perms.Egress)
	if len(perms.EgressAllowlist) > 0 {
		fmt.Fprintf(out, "  egress to: %s\n", strings.Join(perms.EgressAllowlist, ", "))
	}
	if len(perms.ExtraManifestKinds) > 0 {
		fmt.Fprintf(out, "  extra manifests: %s\n", strings.Join(perms.ExtraManifestKinds, ", "))
	}
	if perms.CheckerImage != "" {
		fmt.Fprintf(out, "  custom checker: %s\n", perms.CheckerImage)
	}
}
