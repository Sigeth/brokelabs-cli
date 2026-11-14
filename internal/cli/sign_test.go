package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oras.land/oras-go/v2/content/oci"

	"github.com/Sigeth/opslab-core-go/api/v1alpha1"
	"github.com/Sigeth/opslab-core-go/pkg/ocipkg"
	"github.com/Sigeth/opslab-core-go/pkg/signing"
)

func TestSignWithAKey(t *testing.T) {
	ctx := context.Background()
	built, err := buildPackage(ctx, packageWithSolution(t), "", fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	store, err := oci.NewWithContext(ctx, built.layout)
	if err != nil {
		t.Fatal(err)
	}
	kp, _ := signing.GenerateKeyPair()
	pemKey, _ := kp.PrivateKeyPEM()
	keyFile := filepath.Join(t.TempDir(), "signing.key")
	if err := os.WriteFile(keyFile, pemKey, 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := keySigner(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signPackage(ctx, store, built.desc, signer); err != nil {
		t.Fatal(err)
	}

	pkg, err := ocipkg.Fetch(ctx, store, built.desc)
	if err != nil {
		t.Fatal(err)
	}
	bundles, err := signing.Bundles(ctx, store, built.desc)
	if err != nil || len(bundles) != 1 {
		t.Fatalf("bundles = %d, %v", len(bundles), err)
	}
	pub, _ := kp.GetPublicKeyPem()
	signerID, err := signing.Verifier{}.Verify(v1alpha1.TrustPolicy{Keys: []string{pub}}, pkg.OCIManifest, bundles)
	if err != nil || !strings.HasPrefix(signerID, "key sha256:") {
		t.Fatalf("signer = %q, %v", signerID, err)
	}

	// The solution is not a package: it cannot be signed as one.
	if _, err := signPackage(ctx, store, *built.solution, signer); err == nil {
		t.Fatal("signing a solution must fail")
	}
}

func TestSignRequiresOneMode(t *testing.T) {
	for _, args := range [][]string{
		{"challenge", "sign", "registry.example/x@sha256:" + strings.Repeat("0", 64)},
		{"challenge", "sign", "--key", "k", "--keyless", "registry.example/x@sha256:" + strings.Repeat("0", 64)},
	} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("%v: error = %v", args, err)
		}
	}
}

func TestIdentityTokenFromGitHubActions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer request-token" || r.URL.Query().Get("audience") != "sigstore" {
			http.Error(w, "denied", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"value":"oidc-token"}`))
	}))
	defer srv.Close()
	t.Setenv(IdentityTokenEnv, "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", srv.URL+"/token?api-version=2.0")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "request-token")
	if token, err := identityToken(context.Background()); err != nil || token != "oidc-token" {
		t.Fatalf("token = %q, %v", token, err)
	}
	t.Setenv(IdentityTokenEnv, "explicit")
	if token, _ := identityToken(context.Background()); token != "explicit" {
		t.Fatalf("token = %q, want the explicit one", token)
	}
	t.Setenv(IdentityTokenEnv, "")
	t.Setenv("ACTIONS_ID_TOKEN_REQUEST_URL", "")
	if _, err := identityToken(context.Background()); err == nil {
		t.Fatal("expected an error without any token source")
	}
}
