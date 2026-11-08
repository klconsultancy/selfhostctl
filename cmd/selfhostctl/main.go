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
	"path/filepath"
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
	// phases returns bootstrap's phases for a config; first is `--first`.
	phases func(cfg *config.ManagementCluster, first bool) ([]phase.Phase, func(), error)
	// locate says where the Management cluster's objects live, and why when unknown.
	locate func(ctx context.Context, cfg *config.ManagementCluster) (string, string, error)
	// exportBundle reads the Recovery bundle from wherever the cluster's objects are.
	exportBundle func(ctx context.Context, cfg *config.ManagementCluster) ([]byte, error)
	// adopt stamps the Management cluster's providers with the config's fingerprint.
	adopt func(ctx context.Context, cfg *config.ManagementCluster) error
}

// liveDeps are bootstrap's dependencies on the real world.
func liveDeps(cfg *config.ManagementCluster, first bool) (bootstrap.Deps, func(), error) {
	docker, err := dockerhost.New(dockerhost.OS)
	if err != nil {
		return bootstrap.Deps{}, nil, err
	}
	// The token only lifts clusterctl's GitHub rate limit; nothing else from the
	// environment reaches clusterctl.
	cc := providers.Clusterctl{GitHubToken: os.Getenv("GITHUB_TOKEN"), WaitTimeout: 10 * time.Minute}
	return bootstrap.Deps{
		Docker:        docker,
		NodesReady:    bootstrap.LiveNodesReady,
		Providers:     cc,
		Mover:         cc,
		Prerequisites: beforeapply.Applier{Config: cfg, Lookup: os.LookupEnv},
		First:         first,
	}, func() { _ = docker.Close() }, nil
}

func liveApp() app {
	return app{
		lookupEnv: os.LookupEnv,
		phases: func(cfg *config.ManagementCluster, first bool) ([]phase.Phase, func(), error) {
			deps, done, err := liveDeps(cfg, first)
			if err != nil {
				return nil, nil, err
			}
			return bootstrap.Phases(cfg, deps), done, nil
		},
		locate: func(ctx context.Context, cfg *config.ManagementCluster) (string, string, error) {
			deps, done, err := liveDeps(cfg, false)
			if err != nil {
				return "", "", err
			}
			defer done()
			s, err := bootstrap.Locate(ctx, cfg, deps)
			return s.Location.String(), s.Reason, err
		},
		exportBundle: func(ctx context.Context, cfg *config.ManagementCluster) ([]byte, error) {
			deps, done, err := liveDeps(cfg, false)
			if err != nil {
				return nil, err
			}
			defer done()
			s, err := bootstrap.Locate(ctx, cfg, deps)
			if err != nil {
				return nil, err
			}
			kc := s.BootstrapKC
			if s.Location == bootstrap.LocationManagement {
				kc = s.ManagementKC
			}
			if kc == nil {
				return nil, fmt.Errorf("no cluster holds the Management cluster's objects (location %s)", s.Location)
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
		adopt: func(ctx context.Context, cfg *config.ManagementCluster) error {
			path, err := cfg.KubeconfigPath()
			if err != nil {
				return err
			}
			kc, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("the Management cluster's kubeconfig: %w", err)
			}
			deps, done, err := liveDeps(cfg, false)
			if err != nil {
				return err
			}
			defer done()
			return deps.Providers.(providers.Clusterctl).Adopt(ctx, kc, providers.FromConfig(&cfg.Providers))
		},
	}
}

// renewValidity is how long a renewed kubeconfig lasts: a bridge until bootstrap
// refreshes it from the Management cluster's own Secret.
const renewValidity = 30 * 24 * time.Hour

func renewKubeconfig(cfg *config.ManagementCluster, lookup func(string) (string, bool), now time.Time) (string, error) {
	if cfg.Recovery == nil {
		return "", &exitError{exitUsage, errors.New("kubeconfig renew needs recovery in the config, for the Recovery bundle")}
	}
	value, _ := lookup(cfg.Recovery.BundleFromEnv)
	if value == "" {
		return "", &exitError{exitUsage, fmt.Errorf("no Recovery bundle in %s: load it for this command, e.g. op run -- selfhostctl kubeconfig renew", cfg.Recovery.BundleFromEnv)}
	}
	b, err := recovery.Parse(value)
	if err != nil {
		return "", &exitError{exitUsage, fmt.Errorf("%s: %w", cfg.Recovery.BundleFromEnv, err)}
	}
	kc, err := b.AdminKubeconfig(cfg.ClusterName(), renewValidity, now)
	if err != nil {
		return "", err
	}
	path, err := cfg.KubeconfigPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path+".tmp", kc, 0o600); err != nil {
		return "", err
	}
	return path, os.Rename(path+".tmp", path)
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

	runPhases := func(cmd *cobra.Command, from string, first bool) error {
		cfg, err := load()
		if err != nil {
			return classify(err)
		}
		phases, done, err := a.phases(cfg, first)
		if err != nil {
			return classify(err)
		}
		defer done()
		return classify(phase.Run(cmd.Context(), phases, from, stdout))
	}

	var from string
	var first bool
	bootstrapCmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bring up the Management cluster through a Bootstrap cluster until it manages itself, phase by phase",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPhases(cmd, from, first)
		},
	}
	bootstrapCmd.Flags().StringVar(&from, "from", "", "start at this phase, skipping the ones before it")
	bootstrapCmd.Flags().BoolVar(&first, "first", false, "this is the cluster's first bootstrap: no Management cluster exists yet (only needed without its kubeconfig)")
	root.AddCommand(bootstrapCmd)

	root.AddCommand(&cobra.Command{
		Use:   "pivot",
		Short: "bootstrap's phases from " + bootstrap.PivotFrom + ": the Management cluster takes over from the Bootstrap cluster",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPhases(cmd, bootstrap.PivotFrom, false)
		},
	})

	providersCmd := &cobra.Command{Use: "providers", Short: "The Management cluster's Cluster API providers", Args: cobra.NoArgs}
	providersCmd.AddCommand(&cobra.Command{
		Use:   "adopt",
		Short: "Stamp providers selfhostctl didn't install with the config's fingerprint",
		Long: "For a Management cluster whose providers were installed some other way. Checks that\n" +
			"their versions are the config's, then records the fingerprint of providers.variables on\n" +
			"them. Run it only if they run with exactly those variables (feature gates).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return classify(err)
			}
			if err := a.adopt(cmd.Context(), cfg); err != nil {
				return classify(err)
			}
			_, _ = fmt.Fprintln(stdout, "providers adopted")
			return nil
		},
	})
	root.AddCommand(providersCmd)

	kubeconfigCmd := &cobra.Command{Use: "kubeconfig", Short: "The Management cluster's admin kubeconfig", Args: cobra.NoArgs}
	kubeconfigCmd.AddCommand(&cobra.Command{
		Use:   "renew",
		Short: "Write a new admin kubeconfig signed with the Recovery bundle's CA",
		Long: "For when cluster.kubeconfig has expired or is lost. Reads the Recovery bundle from the\n" +
			"variable recovery.bundleFromEnv names: load it for this one command only, e.g.\n" +
			"op run -- selfhostctl kubeconfig renew -f <config>. The certificate is valid for 30\n" +
			"days; the next bootstrap run refreshes the file from the Management cluster.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := load()
			if err != nil {
				return classify(err)
			}
			path, err := renewKubeconfig(cfg, a.lookupEnv, time.Now())
			if err != nil {
				return classify(err)
			}
			_, _ = fmt.Fprintln(stdout, "wrote", path)
			return nil
		},
	})
	root.AddCommand(kubeconfigCmd)

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
			location, why, err := a.locate(cmd.Context(), cfg)
			if err != nil {
				return classify(err)
			}
			if why != "" {
				location += " (" + why + ")"
			}
			_, _ = fmt.Fprintf(stdout, "location: %s\n", location)
			phases, done, err := a.phases(cfg, false)
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
				_, _ = fmt.Fprintf(stdout, "%-25s %s%s\n", l.Phase, strings.ToUpper(l.Status.State.String()), reason)
			}
			return classify(err)
		},
	})
	return root
}
