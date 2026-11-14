package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry"

	"github.com/Sigeth/opslab-core-go/pkg/ocipkg"
)

var imageDigest = digest.FromString("debian")

func fakeResolver(_ context.Context, _ string) (digest.Digest, error) { return imageDigest, nil }

func packageWithSolution(t *testing.T) string {
	t.Helper()
	dir := writePackage(t, validManifest, "setup.sh")
	if err := os.MkdirAll(filepath.Join(dir, SolutionDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SolutionDir, "solve.sh"), []byte("touch /tmp/hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBuildPinsImagesAndAttachesTheSolution(t *testing.T) {
	ctx := context.Background()
	dir := packageWithSolution(t)
	built, err := buildPackage(ctx, dir, "", fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "dist", "hello-0.1.0"); built.layout != want {
		t.Errorf("layout = %s, want %s", built.layout, want)
	}
	store, err := oci.NewWithContext(ctx, built.layout)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := store.Resolve(ctx, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := ocipkg.Fetch(ctx, store, desc)
	if err != nil {
		t.Fatal(err)
	}
	if want := "docker.io/library/debian:13-slim@" + imageDigest.String(); pkg.Manifest.Spec.Machines[0].Image != want {
		t.Errorf("image = %s, want %s", pkg.Manifest.Spec.Machines[0].Image, want)
	}
	if _, ok := pkg.Files["setup.sh"]; !ok {
		t.Errorf("setup.sh missing from %v", pkg.Files)
	}
	if _, ok := pkg.Files["solution/solve.sh"]; ok {
		t.Error("the solution must not ship in the package")
	}
	referrers, err := registry.Referrers(ctx, store, desc, ocipkg.SolutionArtifactType)
	if err != nil || len(referrers) != 1 {
		t.Fatalf("solution referrers = %v, %v", referrers, err)
	}
}

func TestBuildIsReproducible(t *testing.T) {
	dir := packageWithSolution(t)
	first, err := buildPackage(context.Background(), dir, filepath.Join(t.TempDir(), "a"), fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildPackage(context.Background(), dir, filepath.Join(t.TempDir(), "b"), fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	if first.desc.Digest != second.desc.Digest {
		t.Fatalf("digests differ: %s, %s", first.desc.Digest, second.desc.Digest)
	}
}

func TestBuildFailsOnUnresolvableImages(t *testing.T) {
	dir := writePackage(t, validManifest, "setup.sh")
	_, err := buildPackage(context.Background(), dir, "", func(context.Context, string) (digest.Digest, error) {
		return "", errors.New("not found")
	})
	if err == nil || !strings.Contains(err.Error(), "debian:13-slim") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "dist")); !os.IsNotExist(statErr) {
		t.Error("nothing must be written when the build fails")
	}
}

func TestBuildRefusesInvalidPackages(t *testing.T) {
	dir := writePackage(t, validManifest) // setup.sh is missing
	if _, err := buildPackage(context.Background(), dir, "", fakeResolver); !errors.Is(err, errInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestPushCopiesThePackageAndItsSolution(t *testing.T) {
	ctx := context.Background()
	built, err := buildPackage(ctx, packageWithSolution(t), "", fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := oci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := pushPackage(ctx, built.layout, dst, "registry.example/hello")
	if err != nil {
		t.Fatal(err)
	}
	if want := "registry.example/hello@" + built.desc.Digest.String(); ref != want {
		t.Errorf("ref = %s, want %s", ref, want)
	}
	desc, err := dst.Resolve(ctx, "0.1.0")
	if err != nil || desc.Digest != built.desc.Digest {
		t.Fatalf("tag 0.1.0 = %v, %v", desc.Digest, err)
	}
	referrers, err := registry.Referrers(ctx, dst, desc, ocipkg.SolutionArtifactType)
	if err != nil || len(referrers) != 1 || referrers[0].Digest != built.solution.Digest {
		t.Fatalf("solution referrers = %v, %v", referrers, err)
	}
}

func TestPushRefusesOtherDirectories(t *testing.T) {
	dst, _ := oci.New(t.TempDir())
	if _, err := pushPackage(context.Background(), t.TempDir(), dst, "registry.example/x"); err == nil ||
		!strings.Contains(err.Error(), "not an OCI image layout") {
		t.Fatalf("error = %v", err)
	}
}
