package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/spf13/cobra"
	"oras.land/oras-go/v2/content"

	"github.com/Sigeth/brokelabs-core-go/pkg/ocipkg"
	"github.com/Sigeth/brokelabs-core-go/pkg/project"
	"github.com/Sigeth/brokelabs-core-go/pkg/signing"
)

// IdentityTokenEnv holds an OIDC token for keyless signing.
const IdentityTokenEnv = "SIGSTORE_ID_TOKEN"

// signable reads an artifact to sign and returns its OCI manifest bytes,
// failing when the artifact is not of the expected kind.
type signable func(ctx context.Context, src content.Fetcher, desc ocispec.Descriptor) ([]byte, error)

func signablePackage(ctx context.Context, src content.Fetcher, desc ocispec.Descriptor) ([]byte, error) {
	pkg, err := ocipkg.Fetch(ctx, src, desc)
	if err != nil {
		return nil, err
	}
	return pkg.OCIManifest, nil
}

func newChallengeSignCommand() *cobra.Command {
	return newSignCommand("challenge package", project.Name+" challenge push", signablePackage)
}

// newSignCommand signs artifacts of one kind (packages, catalog indexes).
func newSignCommand(kind, pushedBy string, check signable) *cobra.Command {
	var (
		keyFile    string
		keyless    bool
		registries registryFlags
	)
	cmd := &cobra.Command{
		Use:   "sign REF",
		Short: "Sign a pushed " + kind,
		Long: "Signs the " + kind + " at REF (pinned by digest, as printed by '" + pushedBy + "') and attaches\n" +
			"the signature to it in the registry, as a Sigstore bundle referrer.\n\n" +
			"With --key, signs with an ECDSA P-256 private key (unencrypted PEM), e.g. created with:\n" +
			"  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out signing.key\n" +
			"  openssl pkey -in signing.key -pubout -out signing.pub   # goes in the trust policy\n\n" +
			"With --keyless, signs with a short-lived Sigstore certificate for an OIDC identity: the token comes\n" +
			"from $" + IdentityTokenEnv + ", or from GitHub Actions (permission id-token: write).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (keyFile == "") == !keyless {
				return errors.New("use exactly one of --key and --keyless")
			}
			remote, err := registries.remote()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			var signer packageSigner
			if keyless {
				signer, err = keylessSigner(ctx)
			} else {
				signer, err = keySigner(keyFile)
			}
			if err != nil {
				return err
			}
			repo, parsed, err := remote.Repository(args[0])
			if err != nil {
				return err
			}
			if _, err := parsed.Digest(); err != nil {
				return fmt.Errorf("%s: sign a reference pinned by digest (repository@sha256:...), as printed by push", args[0])
			}
			desc, err := repo.Resolve(ctx, parsed.Reference)
			if err != nil {
				return err
			}
			sig, err := signArtifact(ctx, repo, desc, check, signer)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "signed %s: signature %s\n", args[0], sig.Digest)
			return nil
		},
	}
	cmd.Flags().StringVar(&keyFile, "key", "", "PEM private key (ECDSA P-256) to sign with")
	cmd.Flags().BoolVar(&keyless, "keyless", false, "sign with a Sigstore certificate for an OIDC identity")
	registries.register(cmd)
	return cmd
}

// packageSigner turns an OCI manifest into a Sigstore bundle.
type packageSigner func(manifest []byte) ([]byte, error)

// signArtifact checks the artifact described by desc in target, signs it and
// attaches the signature to it.
func signArtifact(ctx context.Context, target interface {
	content.Fetcher
	content.Pusher
}, desc ocispec.Descriptor, check signable, signer packageSigner) (ocispec.Descriptor, error) {
	manifest, err := check(ctx, target, desc)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	bundle, err := signer(manifest)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("signing: %w", err)
	}
	return signing.Attach(ctx, target, desc, bundle)
}

func keySigner(path string) (packageSigner, error) {
	pemKey, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kp, err := signing.LoadKeyPair(pemKey)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return func(manifest []byte) ([]byte, error) {
		return signing.Sign(manifest, kp, sign.BundleOptions{})
	}, nil
}

// keylessSigner signs with the Sigstore public-good instance: a Fulcio
// certificate for the OIDC identity, a Rekor entry and a timestamp.
func keylessSigner(ctx context.Context) (packageSigner, error) {
	token, err := identityToken(ctx)
	if err != nil {
		return nil, err
	}
	client, err := tuf.New(tuf.DefaultOptions().WithDisableLocalCache())
	if err != nil {
		return nil, fmt.Errorf("reaching the Sigstore TUF repository: %w", err)
	}
	trustedRoot, err := root.GetTrustedRoot(client)
	if err != nil {
		return nil, err
	}
	config, err := root.GetSigningConfig(client)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	fulcio, err := root.SelectService(config.FulcioCertificateAuthorityURLs(), sign.FulcioAPIVersions, now)
	if err != nil {
		return nil, err
	}
	opts := sign.BundleOptions{
		Context:                    ctx,
		TrustedRoot:                trustedRoot, // the new bundle is verified before use
		CertificateProvider:        sign.NewFulcio(&sign.FulcioOptions{BaseURL: fulcio.URL, Retries: 2, UserAgent: project.Name}),
		CertificateProviderOptions: &sign.CertificateProviderOptions{IDToken: token},
	}
	rekors, err := root.SelectServices(config.RekorLogURLs(), config.RekorLogURLsConfig(), sign.RekorAPIVersions, now)
	if err != nil {
		return nil, err
	}
	for _, s := range rekors {
		opts.TransparencyLogs = append(opts.TransparencyLogs,
			sign.NewRekor(&sign.RekorOptions{BaseURL: s.URL, Version: s.MajorAPIVersion, Retries: 2, UserAgent: project.Name}))
	}
	// Rekor v2 entries carry no time: a timestamp authority proves when the
	// short-lived certificate was used.
	tsas, err := root.SelectServices(config.TimestampAuthorityURLs(), config.TimestampAuthorityURLsConfig(), sign.TimestampAuthorityAPIVersions, now)
	if err != nil {
		return nil, err
	}
	for _, s := range tsas {
		opts.TimestampAuthorities = append(opts.TimestampAuthorities,
			sign.NewTimestampAuthority(&sign.TimestampAuthorityOptions{URL: s.URL, Retries: 2}))
	}
	return func(manifest []byte) ([]byte, error) {
		kp, err := sign.NewEphemeralKeypair(nil)
		if err != nil {
			return nil, err
		}
		return signing.Sign(manifest, kp, opts)
	}, nil
}

// identityToken returns the OIDC token for keyless signing: $SIGSTORE_ID_TOKEN,
// or a token requested from GitHub Actions for the "sigstore" audience.
func identityToken(ctx context.Context) (string, error) {
	if token := os.Getenv(IdentityTokenEnv); token != "" {
		return token, nil
	}
	reqURL, reqToken := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if reqURL == "" || reqToken == "" {
		return "", fmt.Errorf("no OIDC token: set $%s, or run in GitHub Actions with the id-token: write permission", IdentityTokenEnv)
	}
	u, err := url.Parse(reqURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("audience", "sigstore")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+reqToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting a GitHub Actions OIDC token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("requesting a GitHub Actions OIDC token: %s", resp.Status)
	}
	var token struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &token); err != nil || token.Value == "" {
		return "", errors.New("the GitHub Actions OIDC token response has no token")
	}
	return token.Value, nil
}
