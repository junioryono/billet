package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	storecontract "github.com/junioryono/billet/internal/store"
	"github.com/junioryono/billet/internal/store/ebss3"
)

// The answers `billet acceptance matrix published` gives beside success: a store that
// answered a miss for some architecture to the end, and one that could not be asked.
const (
	publishedMissing   = 1
	publishedCouldNot  = 2
	publishedPollEvery = 15 * time.Second
)

// matrixGuestArch is the name a guest gives the image store for its architecture, which
// is the last element of its key: the guest's docker-cache.sh maps uname's x86_64 to
// amd64 and aarch64 to arm64, and a test holds this to that script.
var matrixGuestArch = map[config.Arch]string{config.ArchX64: "amd64", config.ArchARM64: "arm64"}

// cmdMatrixPublished waits until each named architecture's Docker image store has a
// generation in the run's site store, which is the cold image runs' publication: the
// deployment is fresh each run, so any generation under its namespace is this run's.
//
// THE WARM IMAGE RUNS WAIT FOR THIS, NOT FOR A TIME. On per-job EC2 the node publishes a
// job's image store only once the guest is proved gone, and on 2026-11-14 (run
// 43590137207) arm64's was first attached 19 minutes after its cold job ended, so a warm
// run sent after a fixed 150 s found it cold.
func cmdMatrixPublished(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet acceptance matrix published", env.Stdout)
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")
	archs := fs.String("archs", "", "the architectures whose image store must be published, comma-separated")
	wait := fs.Duration("wait", 30*time.Minute, "how long to wait for every one")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	covered, err := parseMatrixArchs(*archs)
	if err != nil {
		return err
	}
	if len(covered) == 0 {
		return errors.New("--archs: name the architectures whose image store must be published")
	}
	if *wait <= 0 {
		return fmt.Errorf("--wait must be positive, got %s", *wait)
	}
	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}
	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		return err
	}
	if cfg.Node == nil || cfg.Node.EBSS3 == nil {
		return errors.New("the workspace's node has no ebs-s3 store, so there is no publication to wait for")
	}
	if ws.DeploymentID == "" {
		return errors.New("the workspace records no deployment id, so its store's namespace is unknown")
	}
	namespace := app.CacheNamespace(ws.DeploymentID, cfg.Node.Site)
	store, err := ebss3.New(*cfg.Node.EBSS3, namespace, awscreds.Default())
	if err != nil {
		return fmt.Errorf("the node's ebs-s3 store: %w", err)
	}

	return waitPublished(ctx, env.Stdout, store.Current, namespace, covered, *wait, publishedPollEvery)
}

// waitPublished asks current for each architecture's image store until every one has a
// generation or wait passes. It answers in three ways: nil once every one is published;
// publishedMissing when the store answered a miss for one to the end; publishedCouldNot
// when the last answer for one was not an answer, which is never read as a miss.
func waitPublished(
	ctx context.Context, out io.Writer, current func(context.Context, string) (string, error),
	namespace string, archs []config.Arch, wait, poll time.Duration,
) error {
	deadline := time.Now().Add(wait)
	pending := map[config.Arch]error{}
	for _, a := range archs {
		pending[a] = storecontract.ErrMiss
	}
	for {
		for _, a := range archs {
			if _, ok := pending[a]; !ok {
				continue
			}
			key := node.DockerStoreKey(namespace, matrixGuestArch[a])
			askCtx, cancel := context.WithTimeout(ctx, poll)
			generation, err := current(askCtx, key)
			cancel()
			if err == nil && generation != "" {
				fmt.Fprintf(out, "%s: the image store is published as %s\n", a, generation)
				delete(pending, a)

				continue
			}
			if err == nil {
				err = storecontract.ErrMiss
			}
			pending[a] = err
		}
		if len(pending) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}

	code, msg := publishedMissing, ""
	for _, a := range archs {
		err, ok := pending[a]
		if !ok {
			continue
		}
		if errors.Is(err, storecontract.ErrMiss) {
			msg += fmt.Sprintf("%s: no image store is published after %s; ", a, wait)

			continue
		}
		code = publishedCouldNot
		msg += fmt.Sprintf("%s: the store could not be asked: %v; ", a, err)
	}

	return &cli.ExitError{Code: code, Msg: msg[:len(msg)-2]}
}
