package tart

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// manifestLimit bounds a manifest read. A VM image's manifest names a few hundred disk
// chunks at most; a longer answer is not one.
const manifestLimit = 4 << 20

// maxRegistryRedirects bounds the redirects one exchange follows.
const maxRegistryRedirects = 5

// tokenLimit bounds a token answer.
const tokenLimit = 1 << 20

// registryTimeout bounds one exchange with a registry.
const registryTimeout = 30 * time.Second

// ociManifest is the only media type asked for, the one tart's own registry client
// asks for, so the digest is of the manifest tart would pull.
const ociManifest = "application/vnd.oci.image.manifest.v1+json"

// errRegistryNeedsCredentials is a registry that refuses an anonymous reader, which is
// a could-not-tell about the tag rather than an answer.
var errRegistryNeedsCredentials = errors.New("the registry does not answer anonymously")

// registryClient follows redirects over TLS alone: Go keeps a bearer across a redirect to
// the same host, a downgrade included. Which hosts a registry sends a reader to is its own
// to say, a token service on another host being the ordinary case, and the operator named
// the registry.
func registryClient() *http.Client {
	return &http.Client{
		Timeout: registryTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("the registry redirected to %s, which is not TLS", req.URL.Host)
			}

			if len(via) >= maxRegistryRedirects {
				return errors.New("the registry redirected too many times")
			}

			return nil
		},
	}
}

// remoteRef is a remote reference split the way the distribution API addresses it.
type remoteRef struct {
	host, repository, tag string
}

// parseRemoteRef splits a normalised key. A digest in it is not a tag, and its caller
// asks no registry for one.
func parseRemoteRef(key string) (remoteRef, error) {
	host, rest, ok := strings.Cut(key, "/")
	if !ok || host == "" || rest == "" {
		return remoteRef{}, fmt.Errorf("%q does not name a registry and a repository", key)
	}

	if strings.Contains(rest, "@") {
		return remoteRef{}, fmt.Errorf("%q already names a digest, not a tag", key)
	}

	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return remoteRef{}, fmt.Errorf("%q names no tag", key)
	}

	return remoteRef{host: host, repository: rest[:i], tag: rest[i+1:]}, nil
}

// pullReference is the digest form of a key: its repository, without the tag.
func pullReference(key, digest string) (string, error) {
	ref, err := parseRemoteRef(key)
	if err != nil {
		return "", err
	}

	return ref.host + "/" + ref.repository + "@" + digest, nil
}

// registryDigest asks a registry what a tag names now, anonymously, over TLS.
//
// THE DIGEST IS OF THE BYTES RECEIVED, and a header naming another refuses: the pull
// that follows asks for this digest by name and then holds tart's stored manifest to
// it, so a wrong one here is a refused verification and never a wrong record.
func registryDigest(ctx context.Context, client *http.Client, key string) (string, error) {
	ref, err := parseRemoteRef(key)
	if err != nil {
		return "", err
	}

	target := url.URL{
		Scheme: "https",
		Host:   ref.host,
		Path:   "/v2/" + ref.repository + "/manifests/" + ref.tag,
	}

	res, err := manifestGet(ctx, client, target.String(), "")
	if err != nil {
		return "", err
	}

	if res.StatusCode == http.StatusUnauthorized {
		challenge := res.Header.Get("WWW-Authenticate")
		_ = res.Body.Close()

		token, err := anonymousToken(ctx, client, challenge)
		if err != nil {
			return "", err
		}

		if res, err = manifestGet(ctx, client, target.String(), token); err != nil {
			return "", err
		}
	}

	defer func() { _ = res.Body.Close() }()

	switch res.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", errRegistryNeedsCredentials
	default:
		return "", fmt.Errorf("the registry answered %d for %s", res.StatusCode, key)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, manifestLimit+1))
	if err != nil {
		return "", fmt.Errorf("read %s's manifest: %w", key, err)
	}

	if len(body) > manifestLimit {
		return "", fmt.Errorf("%s's manifest is larger than %d bytes", key, manifestLimit)
	}

	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	if said := res.Header.Get("Docker-Content-Digest"); said != "" && said != digest {
		return "", fmt.Errorf("the registry names %s as %s and served bytes that hash to %s",
			key, said, digest)
	}

	return digest, nil
}

func manifestGet(ctx context.Context, client *http.Client, target, token string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, registryTimeout)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		cancel()

		return nil, err
	}

	req.Header.Set("Accept", ociManifest)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	res, err := client.Do(req)
	if err != nil {
		cancel()

		// *url.Error names the URL and never a header, so the token is not in it.
		return nil, fmt.Errorf("ask the registry: %w", err)
	}

	res.Body = cancelOnClose{ReadCloser: res.Body, cancel: cancel}

	return res, nil
}

// cancelOnClose ends a request's deadline with its body.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	defer c.cancel()

	return c.ReadCloser.Close()
}

// anonymousToken performs the Bearer exchange a registry's 401 asks for, with no
// credential of any kind.
func anonymousToken(ctx context.Context, client *http.Client, challenge string) (string, error) {
	scheme, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", errRegistryNeedsCredentials
	}

	fields := parseChallenge(params)

	realm, err := url.Parse(fields["realm"])
	if err != nil || realm.Scheme != "https" || realm.Host == "" {
		return "", fmt.Errorf("the registry's token realm is not an https URL: %w", errRegistryNeedsCredentials)
	}

	query := realm.Query()

	for _, key := range []string{"service", "scope"} {
		if fields[key] != "" {
			query.Set(key, fields[key])
		}
	}

	realm.RawQuery = query.Encode()

	ctx, cancel := context.WithTimeout(ctx, registryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), http.NoBody)
	if err != nil {
		return "", err
	}

	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ask the registry for an anonymous token: %w", err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return "", errRegistryNeedsCredentials
	}

	var answer struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}

	if err := json.NewDecoder(io.LimitReader(res.Body, tokenLimit)).Decode(&answer); err != nil {
		return "", fmt.Errorf("read the registry's token: %w", err)
	}

	if answer.Token != "" {
		return answer.Token, nil
	}

	if answer.AccessToken != "" {
		return answer.AccessToken, nil
	}

	return "", errRegistryNeedsCredentials
}

// parseChallenge reads `key="value",key="value"`, whose values may hold commas (a
// scope's `pull,push`).
func parseChallenge(s string) map[string]string {
	fields := map[string]string{}

	for s != "" {
		key, rest, ok := strings.Cut(strings.TrimLeft(s, " ,"), "=")
		if !ok {
			break
		}

		if !strings.HasPrefix(rest, `"`) {
			value, after, _ := strings.Cut(rest, ",")
			fields[key] = value
			s = after

			continue
		}

		value, after, ok := strings.Cut(rest[1:], `"`)
		if !ok {
			break
		}

		fields[key] = value
		s = after
	}

	return fields
}
