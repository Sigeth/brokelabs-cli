package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"

	"github.com/Sigeth/opslab-core-go/pkg/challenge"
	"github.com/Sigeth/opslab-core-go/pkg/ocipkg"
	"github.com/Sigeth/opslab-core-go/pkg/project"
)

// SolutionDir holds the reference solution of a challenge. It is published as
// a separate artifact, never in the package.
const SolutionDir = "solution"

// registryFlags configures access to OCI registries.
type registryFlags struct {
	plainHTTP []string
}

func (f *registryFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringSliceVar(&f.plainHTTP, "plain-http", nil,
		"registries reached over plain HTTP, e.g. localhost:5000 (local test registries only)")
}

func (f *registryFlags) remote() (ocipkg.Remote, error) {
	cred, err := registryCredentials()
	if err != nil {
		return ocipkg.Remote{}, err
	}
	return ocipkg.Remote{
		Credential: cred,
		PlainHTTP:  func(registry string) bool { return slices.Contains(f.plainHTTP, registry) },
	}, nil
}

// registryCredentials reads the credentials saved by "docker login" or
// "podman login": $REGISTRY_AUTH_FILE, then ~/.docker/config.json, then
// podman's $XDG_RUNTIME_DIR/containers/auth.json.
func registryCredentials() (auth.CredentialFunc, error) {
	paths := []string{os.Getenv("REGISTRY_AUTH_FILE")}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".docker", "config.json"))
	}
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		paths = append(paths, filepath.Join(runtime, "containers", "auth.json"))
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			continue
		}
		store, err := credentials.NewStore(path, credentials.StoreOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading registry credentials from %s: %w", path, err)
		}
		return credentials.Credential(store), nil
	}
	return nil, nil // anonymous
}

func newChallengeBuildCommand() *cobra.Command {
	var output string
	var registries registryFlags
	cmd := &cobra.Command{
		Use:   "build [DIR]",
		Short: "Build a challenge package as an OCI image layout",
		Long: "Validates the challenge, pins every image it uses by digest and writes the package\n" +
			"(and its solution, as a referrer) to an OCI image layout, ready for '" + project.Name + " challenge push'.\n" +
			"Images must already be pushed: a tag that cannot be resolved fails the build.\n" +
			"The default output is dist/<name>-<version> in the challenge directory.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			remote, err := registries.remote()
			if err != nil {
				return err
			}
			built, err := buildPackage(cmd.Context(), dir, output, remote.Resolver())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s %s: %s\n", built.name, built.version, built.desc.Digest)
			for _, image := range built.images {
				fmt.Fprintf(out, "  image %s\n", image)
			}
			if built.solution != nil {
				fmt.Fprintf(out, "  solution %s\n", built.solution.Digest)
			}
			fmt.Fprintf(out, "written to %s\n", built.layout)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "OCI image layout directory to write")
	registries.register(cmd)
	return cmd
}

type builtPackage struct {
	name, version string
	layout        string
	desc          ocispec.Descriptor
	solution      *ocispec.Descriptor
	images        []string
}

// buildPackage validates the challenge in dir, pins its images with resolve
// and writes it to the OCI image layout output, tagged with its version.
func buildPackage(ctx context.Context, dir, output string, resolve ocipkg.Resolver) (*builtPackage, error) {
	files := os.DirFS(dir)
	pkg, err := challenge.Load(files)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", dir, err)
	}
	if errs := challenge.Validate(pkg, files); len(errs) > 0 {
		return nil, fmt.Errorf("%w: %s: %v (run '%s challenge lint')", errInvalid, dir, errs.ToAggregate(), project.Name)
	}
	bundle, err := challenge.Bundle(pkg, files)
	if err != nil {
		return nil, err
	}
	solution, err := readSolution(files)
	if err != nil {
		return nil, err
	}
	if err := ocipkg.PinImages(ctx, pkg, resolve); err != nil {
		return nil, err
	}

	if output == "" {
		output = filepath.Join(dir, "dist", pkg.Metadata.Name+"-"+pkg.Metadata.Version)
	}
	store, err := oci.New(output)
	if err != nil {
		return nil, err
	}
	built := &builtPackage{name: pkg.Metadata.Name, version: pkg.Metadata.Version, layout: output, images: ocipkg.Images(pkg)}
	built.desc, err = ocipkg.Pack(ctx, store, pkg, bundle)
	if err != nil {
		return nil, err
	}
	if err := store.Tag(ctx, built.desc, pkg.Metadata.Version); err != nil {
		return nil, err
	}
	if solution != nil {
		desc, err := ocipkg.PackSolution(ctx, store, built.desc, solution)
		if err != nil {
			return nil, err
		}
		built.solution = &desc
	}
	return built, nil
}

// readSolution returns the files of the solution directory, or nil when the
// challenge has none.
func readSolution(files fs.FS) (map[string][]byte, error) {
	if _, err := fs.Stat(files, SolutionDir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	solution := map[string][]byte{}
	size := 0
	err := fs.WalkDir(files, SolutionDir, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: only regular files can be shipped", name)
		}
		data, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		size += len(data)
		solution[name] = data
		return nil
	})
	if err != nil {
		return nil, err
	}
	if size > challenge.MaxBundleSize {
		return nil, fmt.Errorf("%s: %d bytes, more than the %d allowed", SolutionDir, size, challenge.MaxBundleSize)
	}
	if len(solution) == 0 {
		return nil, nil
	}
	return solution, nil
}

func newChallengePushCommand() *cobra.Command {
	var registries registryFlags
	cmd := &cobra.Command{
		Use:   "push LAYOUT REPOSITORY",
		Short: "Push a built challenge package to a registry",
		Long: "Pushes the package of an OCI image layout written by '" + project.Name + " challenge build',\n" +
			"with its solution, to REPOSITORY, tagged with the package version:\n\n" +
			"  " + project.Name + " challenge push dist/ssh-hardening-1.0.0 ghcr.io/sigeth/opslab-challenges/ssh-hardening\n\n" +
			"It prints the pushed reference, pinned by digest. Credentials come from 'docker login' or 'podman login'.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			remote, err := registries.remote()
			if err != nil {
				return err
			}
			repo, _, err := remote.Repository(args[1])
			if err != nil {
				return err
			}
			ref, err := pushPackage(cmd.Context(), args[0], repo, args[1])
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), ref)
			return nil
		},
	}
	registries.register(cmd)
	return cmd
}

// pushPackage copies the package of layout (and its referrers) to dst, tagged
// with its version, and returns repository@digest.
func pushPackage(ctx context.Context, layout string, dst oras.Target, repository string) (string, error) {
	if _, err := os.Stat(filepath.Join(layout, ocispec.ImageLayoutFile)); err != nil {
		return "", fmt.Errorf("%s is not an OCI image layout (run '%s challenge build'): %w", layout, project.Name, err)
	}
	store, err := oci.NewWithContext(ctx, layout)
	if err != nil {
		return "", err
	}
	desc, version, err := layoutPackage(ctx, store)
	if err != nil {
		return "", fmt.Errorf("%s: %w", layout, err)
	}
	// Fetch validates the package before anything leaves the machine.
	if _, err := ocipkg.Fetch(ctx, store, desc); err != nil {
		return "", fmt.Errorf("%s: %w", layout, err)
	}
	if err := oras.ExtendedCopyGraph(ctx, store, dst, desc, oras.DefaultExtendedCopyGraphOptions); err != nil {
		return "", err
	}
	if err := dst.Tag(ctx, desc, version); err != nil {
		return "", err
	}
	return repository + "@" + desc.Digest.String(), nil
}

// layoutPackage returns the only package tagged in a layout and its version.
func layoutPackage(ctx context.Context, store *oci.Store) (ocispec.Descriptor, string, error) {
	var found []string
	err := store.Tags(ctx, "", func(tags []string) error {
		found = append(found, tags...)
		return nil
	})
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}
	if len(found) != 1 {
		return ocispec.Descriptor{}, "", fmt.Errorf("want exactly one tagged package, found %d (%v)", len(found), found)
	}
	desc, err := store.Resolve(ctx, found[0])
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}
	return desc, found[0], nil
}
