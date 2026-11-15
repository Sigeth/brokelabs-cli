package cli

import (
	"context"
	"fmt"
	"io/fs"
	"testing/fstest"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"

	"github.com/Sigeth/opslab-core-go/api/v1alpha1"
	"github.com/Sigeth/opslab-core-go/pkg/catalog"
	"github.com/Sigeth/opslab-core-go/pkg/challenge"
	"github.com/Sigeth/opslab-core-go/pkg/ocipkg"
	"github.com/Sigeth/opslab-core-go/pkg/project"
)

func newCatalogCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Publish catalogs of challenges",
	}
	cmd.AddCommand(newCatalogIndexCommand(),
		newSignCommand("catalog index", project.Name+" catalog index", signableIndex))
	return cmd
}

func signableIndex(ctx context.Context, src content.Fetcher, desc ocispec.Descriptor) ([]byte, error) {
	idx, err := catalog.Fetch(ctx, src, desc)
	if err != nil {
		return nil, err
	}
	return idx.OCIManifest, nil
}

func newCatalogIndexCommand() *cobra.Command {
	var (
		push       string
		base       string
		registries registryFlags
	)
	cmd := &cobra.Command{
		Use:   "index --push REPOSITORY:TAG PACKAGE_REF...",
		Short: "Build and push a catalog index listing published packages",
		Long: "Reads every package (pinned by digest, as printed by '" + project.Name + " challenge push'), validates it\n" +
			"and lists it in a catalog index pushed to --push. With --base, the packages are added to an\n" +
			"existing index, so a catalog grows one release at a time:\n\n" +
			"  " + project.Name + " catalog index --base ghcr.io/sigeth/opslab-catalog:latest \\\n" +
			"    --push ghcr.io/sigeth/opslab-catalog:latest ghcr.io/sigeth/opslab-challenges/ssh-hardening@sha256:...\n\n" +
			"Sign the index afterwards with '" + project.Name + " catalog sign'.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if push == "" {
				return fmt.Errorf("--push is required")
			}
			remote, err := registries.remote()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			idx := catalog.New()
			if base != "" {
				if idx, err = fetchIndex(ctx, remote, base); err != nil {
					return fmt.Errorf("--base %s: %w", base, err)
				}
			}
			for _, ref := range args {
				if err := addPackage(ctx, remote, idx, ref); err != nil {
					return err
				}
			}
			repo, parsed, err := remote.Repository(push)
			if err != nil {
				return err
			}
			if parsed.Reference == "" {
				return fmt.Errorf("--push %s: name a tag", push)
			}
			pushed, err := pushIndex(ctx, repo, idx, parsed.Reference)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s@%s (%d packages)\n", parsed.Registry+"/"+parsed.Repository, pushed.Digest, len(idx.Packages))
			return nil
		},
	}
	cmd.Flags().StringVar(&push, "push", "", "repository and tag of the index to push")
	cmd.Flags().StringVar(&base, "base", "", "existing index to add the packages to")
	registries.register(cmd)
	return cmd
}

func fetchIndex(ctx context.Context, remote ocipkg.Remote, ref string) (*v1alpha1.CatalogIndex, error) {
	repo, _, err := remote.Repository(ref)
	if err != nil {
		return nil, err
	}
	desc, err := remote.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	idx, err := catalog.Fetch(ctx, repo, desc)
	if err != nil {
		return nil, err
	}
	return idx.Index, nil
}

// addPackage reads and validates the package at ref and lists it in idx.
func addPackage(ctx context.Context, remote ocipkg.Remote, idx *v1alpha1.CatalogIndex, ref string) error {
	if !ocipkg.Pinned(ref) {
		return fmt.Errorf("%s: list packages pinned by digest, as printed by '%s challenge push'", ref, project.Name)
	}
	repo, _, err := remote.Repository(ref)
	if err != nil {
		return err
	}
	desc, err := remote.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	pkg, err := ocipkg.Fetch(ctx, repo, desc)
	if err != nil {
		return fmt.Errorf("%s: %w", ref, err)
	}
	if errs := challenge.Validate(pkg.Manifest, mapFiles(pkg.Files)); len(errs) > 0 {
		return fmt.Errorf("%s: %w: %v", ref, errInvalid, errs.ToAggregate())
	}
	return catalog.Add(idx, pkg.Manifest, ref)
}

// pushIndex packs idx into dst and tags it.
func pushIndex(ctx context.Context, dst oras.Target, idx *v1alpha1.CatalogIndex, tag string) (ocispec.Descriptor, error) {
	desc, err := catalog.Pack(ctx, dst, idx)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	return desc, dst.Tag(ctx, desc, tag)
}

// mapFiles is a file system of package files, for validation.
func mapFiles(files map[string][]byte) fs.FS {
	m := fstest.MapFS{}
	for name, data := range files {
		m[name] = &fstest.MapFile{Data: data, Mode: 0o644}
	}
	return m
}
