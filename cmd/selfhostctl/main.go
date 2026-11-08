// Command selfhostctl manages the lifecycle of one self-managed Cluster API
// management cluster. See docs/cli.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/klconsultancy/selfhostctl/internal/beforeapply"
	"github.com/klconsultancy/selfhostctl/internal/bootstrap"
	"github.com/klconsultancy/selfhostctl/internal/config"
	"github.com/klconsultancy/selfhostctl/internal/dockerhost"
	"github.com/klconsultancy/selfhostctl/internal/phase"
	"github.com/klconsultancy/selfhostctl/internal/providers"
	"github.com/klconsultancy/selfhostctl/internal/recovery"
)

// version is set at release time with -ldflags "-X main.version=<tag>".
var version = "dev"

// Exit codes (docs/cli.md).
const (
	exitOK         = 0
	exitFailure    = 1
	exitUsage      = 2
	exitBlocked    = 3
	exitCheckpoint = 4
)

func main() {
	// controller-runtime's client logs nothing the operator needs, and warns with a
	// stack trace when no logger is set.
	ctrllog.SetLogger(logr.Discard())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, liveApp()))
}

// app is what the commands need from the outside world; tests replace it.
type app struct {
	lookupEnv func(string) (string, bool)
	// phases returns bootstrap's phases for a config.
	phases func(cfg *config.ManagementCluster) ([]phase.Phase, func(), error)
	// exportBundle reads the Recovery bundle from wherever the cluster's objects are.
	exportBundle func(ctx context.Context, cfg *config.ManagementCluster) ([]byte, error)
}

func liveApp() app {
	return app{
		lookupEnv: os.LookupEnv,
		phases: func(cfg *config.ManagementCluster) ([]phase.Phase, func(), error) {
			docker, err := dockerhost.New(dockerhost.OS)
			if err != nil {
				return nil, nil, err
			}
			deps := bootstrap.Deps{
				Docker:     docker,
				NodesReady: bootstrap.LiveNodesReady,
				// The token only lifts clusterctl's GitHub rate limit; nothing else from
				// the environment reaches clusterctl.
				Providers:     providers.Clusterctl{GitHubToken: os.Getenv("GITHUB_TOKEN"), WaitTimeout: 10 * time.Minute},
				Prerequisites: beforeapply.Applier{Config: cfg, Lookup: os.LookupEnv},
			}
			return bootstrap.Phases(cfg, deps), func() { _ = docker.Close() }, nil
		},
		exportBundle: func(ctx context.Context, cfg *config.ManagementCluster) ([]byte, error) {
			docker, err := dockerhost.New(dockerhost.OS)
			if err != nil {
				return nil, err
			}
			defer func() { _ = docker.Close() }()
			kc, err := bootstrap.BootstrapCluster(cfg, docker).Kubeconfig(ctx)
			if err != nil {
				return nil, fmt.Errorf("the Bootstrap cluster holds the cluster's objects until the pivot, and isn't reachable: %w", err)
			}
			c, err := bootstrap.LiveClients(kc)
			if err != nil {
				return nil, err
			}
			b, err := recovery.Export(ctx, c, cfg.Cluster.Namespace, cfg.ClusterName())
			if err != nil {
				return nil, err
			}
			return b.JSON()
		},
	}
}

// exitError carries a command's exit code; any other error from cobra is a usage
// error.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }

func classify(err error) error {
	var b *phase.BlockedError
	var c *phase.CheckpointError
	var u *phase.UnknownPhaseError
	var e *exitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &e):
		return err
	case errors.As(err, &c):
		return &exitError{exitCheckpoint, err}
	case errors.As(err, &b):
		return &exitError{exitBlocked, err}
	case errors.As(err, &u), config.IsConfigError(err):
		return &exitError{exitUsage, err}
	}
	return &exitError{exitFailure, err}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, a app) int {
	root := newRoot(a, stdout)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}
	_, _ = fmt.Fprintln(stderr, "selfhostctl:", err)
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	return exitUsage
}

func newRoot(a app, stdout io.Writer) *cobra.Command {
	var configFile string
	root := &cobra.Command{
		Use:           "selfhostctl",
		Short:         "The lifecycle of one self-managed Cluster API management cluster",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_ = cmd.Help()
			return errors.New("a command is required")
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.PersistentFlags().StringVarP(&configFile, "config", "f", "", "the Management cluster's config file (required)")
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			_, _ = fmt.Fprintln(stdout, version)
		},
	})

	load := func() (*config.ManagementCluster, error) {
		if configFile == "" {
			return nil, &exitError{exitUsage, errors.New("--config/-f is required")}
		}
		cfg, err := config.Load(configFile)
		if err != nil {
			return nil, err
		}
		if err := cfg.CheckEnv(a.lookupEnv); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	var from string
	bootstrapCmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bring up the Management cluster through a Bootstrap cluster, phase by phase",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return classify(err)
			}
			phases, done, err := a.phases(cfg)
			if err != nil {
				return classify(err)
			}
			defer done()
			return classify(phase.Run(cmd.Context(), phases, from, stdout))
		},
	}
	bootstrapCmd.Flags().StringVar(&from, "from", "", "start at this phase, skipping the ones before it")
	root.AddCommand(bootstrapCmd)

	recoveryCmd := &cobra.Command{Use: "recovery", Short: "The Recovery bundle", Args: cobra.NoArgs}
	recoveryCmd.AddCommand(&cobra.Command{
		Use:   "export",
		Short: "Write the Recovery bundle to stdout, for your secret store",
		Long: "Writes the Recovery bundle (the cluster's CA, service-account key, front-proxy CA and\n" +
			"control-plane endpoint) to stdout as JSON. It is secret: pipe it into your secret store,\n" +
			"never into a file in git.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return classify(err)
			}
			data, err := a.exportBundle(cmd.Context(), cfg)
			if err != nil {
				return classify(err)
			}
			_, err = stdout.Write(data)
			return classify(err)
		},
	})
	root.AddCommand(recoveryCmd)

	root.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "List every phase as done, next, blocked or manual, and why",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return classify(err)
			}
			phases, done, err := a.phases(cfg)
			if err != nil {
				return classify(err)
			}
			defer done()
			lines, err := phase.Report(cmd.Context(), phases)
			for _, l := range lines {
				reason := ""
				if l.Status.Reason != "" {
					reason = ": " + l.Status.Reason
				}
				_, _ = fmt.Fprintf(stdout, "%-20s %s%s\n", l.Phase, strings.ToUpper(l.Status.State.String()), reason)
			}
			return classify(err)
		},
	})
	return root
}
