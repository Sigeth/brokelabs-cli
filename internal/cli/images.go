package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Sigeth/brokelabs-core-go/pkg/challenge"
)

// ImagesDir holds the recipe of each machine image a challenge builds:
// images/<machine>/Dockerfile for pod machines, images/<machine>/image.yaml
// (a VM image recipe, built by brokelabs-ci's vm-image) for vm machines.
const ImagesDir = "images"

func newChallengeImagesCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "images [DIR]",
		Short: "List the machine images of a challenge and the ones it builds",
		Long: "Prints one line per machine with an image: the machine, its image reference, and the\n" +
			"build context (" + ImagesDir + "/<machine>, with a Dockerfile or a VM image recipe, image.yaml)\n" +
			"when the challenge builds that image, or '-'.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			pkg, err := challenge.Load(os.DirFS(dir))
			if err != nil {
				return fmt.Errorf("%s: %w", dir, err)
			}
			for _, m := range pkg.Spec.Machines {
				if m.Image == "" {
					continue
				}
				context := filepath.Join(dir, ImagesDir, m.Name)
				if !exists(filepath.Join(context, "Dockerfile")) && !exists(filepath.Join(context, "image.yaml")) {
					context = "-"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", m.Name, m.Image, context)
			}
			return nil
		},
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
