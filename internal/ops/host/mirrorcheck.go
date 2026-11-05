package host

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
)

// mirrorProbeLimit bounds asking one registry mirror whether it answers.
const mirrorProbeLimit = 5 * time.Second

// judgeRegistryMirrors says what this node's guests pull through, and refuses a
// configuration naming an authority it cannot hand them.
//
// A SITE WITH NO DOCKER HUB MIRROR IS REPORTED, NOT REFUSED: its jobs run, until
// enough of them behind one address meet Docker Hub's limit. Only a Firecracker
// node's guests consume a mirror, so nothing is said of any other.
//
// A MIRROR THAT DOES NOT ANSWER IS REPORTED TOO. A guest falls back to the
// registry on the same failure, so this is a saving lost and no job's; and the
// refusal is kept for the one thing an operator got wrong in this file.
//
// SKIPPED PROBES SAY SO, because "skipped" must never read as "answers".
func judgeRegistryMirrors(ctx context.Context, cfg *config.Config, skipProbes bool) ([]string, error) {
	if cfg.Node == nil || cfg.Node.Provider != config.ProviderFirecracker {
		return nil, nil
	}

	if cfg.Node.RegistryMirrors == nil || cfg.Node.RegistryMirrors.Empty() {
		site := cfg.Node.Site
		if site == "" {
			site = "local (implicit)"
		}

		return []string{
			"NO DOCKER HUB MIRROR at site " + site + ": this node's guests pull from Docker Hub over the " +
				"site's own address, and Docker limits an unauthenticated address: 100 pulls an hour by its " +
				"own header on 2026-11-05, shared by everything behind it",
			"the host role runs one unless billet_registry_mirrors is false; node.registry_mirrors names " +
				"your own, and a pull past the limit fails with toomanyrequests",
		}, nil
	}

	authority, err := app.NodeRegistryMirrorsCA(cfg.Node)
	if err != nil {
		return []string{"could not read the mirrors' authority, so a guest cannot verify a mirror it signs " +
			"and pulls from the registry"}, fmt.Errorf("registry mirrors: %w", err)
	}

	mirrors := cfg.Node.RegistryMirrors
	var lines []string
	for _, mirror := range []struct{ upstream, origin string }{
		{"docker.io", mirrors.DockerIO}, {"ghcr.io", mirrors.GHCRIO}, {"quay.io", mirrors.QuayIO},
	} {
		switch {
		case skipProbes:
			lines = append(lines, mirror.upstream+" through "+mirror.origin+" (configured, not probed)")
		default:
			if err := probeRegistryMirror(ctx, mirror.origin, authority); err != nil {
				lines = append(lines, "NOT ANSWERING: "+mirror.upstream+" through "+mirror.origin+
					", so its pulls go to the registry: "+err.Error())

				continue
			}
			lines = append(lines, mirror.upstream+" through "+mirror.origin+" answers")
		}
	}

	return lines, nil
}

// probeRegistryMirror asks a mirror's registry API whether it is there, trusting
// what a guest would: the public authorities and the mirrors' own. A registry
// answers 200 or, when it wants a credential, 401; a redirect is not followed,
// because a guest's own client would be following it somewhere else.
func probeRegistryMirror(ctx context.Context, origin, authority string) error {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if authority != "" && !roots.AppendCertsFromPEM([]byte(authority)) {
		return errors.New("its authority holds no certificate")
	}

	client := &http.Client{
		Timeout: mirrorProbeLimit,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/v2/", http.NoBody)
	if err != nil {
		return err
	}

	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("it answered %d", response.StatusCode)
	}

	return nil
}
