package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/tokenbroker"
)

// NewBrokerClient is this host's connection to the deployment's token broker: the key
// that proves it, and the roots that prove the broker.
func NewBrokerClient(cfg *config.Config) (*tokenbroker.Client, error) {
	broker := cfg.Server.TokenBroker

	key, err := tokenbroker.ReadClientKey(broker.ClientKeyPath)
	if err != nil {
		return nil, err
	}

	cert, err := tokenbroker.ClientCertificate(key)
	if err != nil {
		return nil, err
	}

	roots, err := tokenbroker.LoadRoots(broker.CAFile)
	if err != nil {
		return nil, err
	}

	// PEEKED, NEVER MINTED: a first start reaches this before the ledger's open has
	// made the identity, and the identity's minting is not this code's to do.
	dir := cfg.Server.IdentityDir

	return tokenbroker.NewClient(tokenbroker.ClientOptions{
		URL: broker.URL, Certificate: cert, Roots: roots,
		Identity: func() (string, bool, error) { return state.PeekDeploymentID(dir) },
	}), nil
}

// TargetTokens is where a deployment taking its tokens from a broker gets one
// target's, on the client it was handed.
func TargetTokens(client *tokenbroker.Client, target config.GitHubTarget) github.TokenSource {
	return client.Tokens(GitHubTargetOf(target), target.AppID, target.InstallationID)
}

// Broker connection limits, generous for one host's deployments: a control plane asks
// about once an hour, per target.
const (
	brokerConnectionLimit = 4096
	brokerStopGrace       = 10 * time.Second
)

// ServeBroker runs the token broker until ctx ends: the process holding the App keys,
// and nothing else.
func ServeBroker(ctx context.Context, cfg *config.BrokerConfig, log *slog.Logger) error {
	server, err := tokenbroker.NewServer(cfg, log)
	if err != nil {
		return err
	}

	tlsConf, err := tokenbroker.ServerTLS(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return err
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen for control planes on %s: %w", cfg.Listen, err)
	}

	// THE SAME ACCEPTOR THE NODE WIRE USES: a budget charged only after a handshake
	// succeeds, so a stranger who can open a socket cannot occupy a control plane's
	// place, and a saturated port refuses at once.
	bounded := newHandshakingListener(ctx, ln, tlsConf, brokerConnectionLimit, handshakeBounds{}, log)

	srv := &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    8 << 10,
	}

	served := make(chan error, 1)

	go func() { served <- srv.Serve(bounded) }()

	log.Info("serving installation tokens", "listen", ln.Addr().String(),
		"apps", len(cfg.Apps), "deployments", len(cfg.Deployments))

	select {
	case err := <-served:
		return fmt.Errorf("serve installation tokens: %w", err)
	case <-ctx.Done():
	}

	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), brokerStopGrace)
	defer cancel()

	if err := srv.Shutdown(stop); err != nil {
		return fmt.Errorf("stop serving installation tokens: %w", err)
	}

	<-served

	return nil
}
