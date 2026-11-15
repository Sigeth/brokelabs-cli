package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"oras.land/oras-go/v2/content/oci"

	"github.com/Sigeth/opslab-core-go/api/v1alpha1"
	"github.com/Sigeth/opslab-core-go/pkg/catalog"
	"github.com/Sigeth/opslab-core-go/pkg/ocipkg"
	"github.com/Sigeth/opslab-core-go/pkg/signing"
)

func TestPushAndSignAnIndex(t *testing.T) {
	ctx := context.Background()
	built, err := buildPackage(ctx, packageWithSolution(t), "", fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	store, err := oci.NewWithContext(ctx, built.layout)
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := ocipkg.Fetch(ctx, store, built.desc)
	if err != nil {
		t.Fatal(err)
	}
	idx := catalog.New()
	if err := catalog.Add(idx, pkg.Manifest, pkg.Files, "registry.example/hello@"+built.desc.Digest.String()); err != nil {
		t.Fatal(err)
	}
	dst, err := oci.NewWithContext(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := pushIndex(ctx, dst, idx, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if tagged, err := dst.Resolve(ctx, "latest"); err != nil || tagged.Digest != desc.Digest {
		t.Fatalf("latest = %v, %v", tagged.Digest, err)
	}

	kp, _ := signing.GenerateKeyPair()
	pemKey, _ := kp.PrivateKeyPEM()
	keyFile := filepath.Join(t.TempDir(), "k")
	_ = os.WriteFile(keyFile, pemKey, 0o600)
	signer, err := keySigner(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signArtifact(ctx, dst, desc, signableIndex, signer); err != nil {
		t.Fatal(err)
	}
	fetched, err := catalog.Fetch(ctx, dst, desc)
	if err != nil {
		t.Fatal(err)
	}
	bundles, _ := signing.Bundles(ctx, dst, desc)
	pub, _ := kp.GetPublicKeyPem()
	if _, err := (signing.Verifier{}).Verify(v1alpha1.TrustPolicy{Keys: []string{pub}}, fetched.OCIManifest, bundles); err != nil {
		t.Fatal(err)
	}

	// A package is not an index, and an index is not a package.
	if _, err := signArtifact(ctx, store, built.desc, signableIndex, signer); err == nil {
		t.Error("a package must not be signed as an index")
	}
	if _, err := signArtifact(ctx, dst, desc, signablePackage, signer); err == nil {
		t.Error("an index must not be signed as a package")
	}
}

func TestIndexRequiresPushAndPinnedRefs(t *testing.T) {
	if _, err := run(t, "catalog", "index", "registry.example/x@sha256:0000"); err == nil {
		t.Error("--push must be required")
	}
	if err := addPackage(context.Background(), ocipkg.Remote{}, catalog.New(), "registry.example/x:1.0.0"); err == nil {
		t.Error("unpinned refs must be refused")
	}
}
