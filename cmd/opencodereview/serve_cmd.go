// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/alibaba/open-code-review/internal/config/serveconfig"
	"github.com/alibaba/open-code-review/internal/serve/checkout"
	"github.com/alibaba/open-code-review/internal/serve/forge"
	"github.com/alibaba/open-code-review/internal/serve/github"
	"github.com/alibaba/open-code-review/internal/serve/gitlab"
	"github.com/alibaba/open-code-review/internal/serve/server"
	"github.com/alibaba/open-code-review/internal/serve/state"
	"github.com/alibaba/open-code-review/internal/stdout"
	"github.com/spf13/cobra"
)

const (
	serveLocale         = "vi"
	serveStartupTimeout = 30 * time.Second
	serveDrainTimeout   = 60 * time.Second
	// servePathPrefix is where a reverse proxy may mount the service without
	// stripping the prefix; the bare paths are served as well.
	servePathPrefix = "/ocr"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the PR review webhook service",
	Long: `Run the PR review webhook service.

The service receives GitHub App and GitLab webhooks, reviews pull and merge
requests in bounded rounds, and posts every finding as a resolvable thread.
It is configured through OCR_SERVE_* environment variables; see
deploy/serve/README.md.`,
	Args: cobra.NoArgs,
	RunE: runServe,
}

var serveHooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Manage forge webhooks",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var serveHooksGitLabCmd = &cobra.Command{
	Use:   "gitlab",
	Short: "Register the service webhooks on a GitLab instance",
	Long: `Register the service webhooks on a GitLab instance.

Without --project the command manages the system hook, a project hook on every
project, the bot membership in every top-level group, and the "all threads
must be resolved" merge setting. With --project it manages one project hook
only. The default is a dry run.

Needs OCR_SERVE_GITLAB_URL, OCR_SERVE_GITLAB_ADMIN_TOKEN (api and admin_mode
scopes; pass it for this run only), OCR_SERVE_GITLAB_HOOK_URL, and for --apply
OCR_SERVE_GITLAB_WEBHOOK_SECRET. OCR_SERVE_GITLAB_BOT_USERNAME is needed
without --project.`,
	Example: `  ocr serve hooks gitlab --project group/app
  ocr serve hooks gitlab --apply
  ocr serve hooks gitlab --remove`,
	Args: cobra.NoArgs,
	RunE: runServeHooksGitLab,
}

var serveHooksFlags struct {
	dryRun, apply, remove bool
	project               string
}

func init() {
	f := serveHooksGitLabCmd.Flags()
	f.BoolVar(&serveHooksFlags.dryRun, "dry-run", false, "list the changes without making them (default)")
	f.BoolVar(&serveHooksFlags.apply, "apply", false, "create or update the hooks and settings")
	f.BoolVar(&serveHooksFlags.remove, "remove", false, "delete the hooks; bot membership and the merge setting stay")
	f.StringVar(&serveHooksFlags.project, "project", "", "limit the run to one project (path with namespace)")
	serveHooksGitLabCmd.MarkFlagsMutuallyExclusive("dry-run", "apply", "remove")

	serveHooksCmd.AddCommand(serveHooksGitLabCmd)
	serveCmd.AddCommand(serveHooksCmd)
}

// serveEnv is what the service reads from the process. Tests replace it.
type serveEnv struct {
	getenv   func(string) string
	readFile func(string) ([]byte, error)
	logOut   io.Writer
	httpc    *http.Client
	// ready, when set, receives the bound address once the listener is open.
	ready func(addr string)
}

func processServeEnv() serveEnv {
	return serveEnv{getenv: os.Getenv, readFile: os.ReadFile, logOut: os.Stdout}
}

func runServe(cmd *cobra.Command, _ []string) error {
	ctx, stop := interruptContextWithForcedExit(cmd.Context())
	defer stop()
	// Agent progress lines would interleave with the JSON log on stdout.
	// Concurrent reviews must not quiet stdout themselves, so it is done
	// once here, before any review starts.
	defer stdout.Quiet()()
	return serveMain(ctx, processServeEnv())
}

// serveMain runs the service until ctx ends, then drains in-flight work.
func serveMain(ctx context.Context, env serveEnv) error {
	cfg, err := serveconfig.Load(env.getenv, env.readFile)
	if err != nil {
		return fmt.Errorf("serve config: %w", err)
	}
	if err := cfg.RequireServe(); err != nil {
		return fmt.Errorf("serve config: %w", err)
	}
	if err := checkServeAppConfig(); err != nil {
		return err
	}
	log := server.NewLogger(env.logOut)

	forges, err := buildServeForges(ctx, cfg, env.httpc)
	if err != nil {
		return err
	}
	workspaces, err := checkout.NewManager(cfg.WorkDir, cfg.Workers)
	if err != nil {
		return err
	}
	loc, err := state.LoadLocale(serveLocale)
	if err != nil {
		return err
	}
	srv, err := server.New(server.Deps{
		Config: cfg, Forges: forges, Reviewer: newServeReviewer(cfg), Workspaces: workspaces,
		Locale: loc, Logger: log, Version: Version,
	})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Addr, err)
	}
	httpSrv := &http.Server{
		Handler:           serveRoutes(srv.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	if err := srv.Start(ctx); err != nil {
		_ = ln.Close()
		return err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String(), "version", Version, "platforms", platformNames(forges))
	if env.ready != nil {
		env.ready(ln.Addr().String())
	}

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("http server: %w", err)
		}
	}
	log.Info("shutting down", "drain_timeout", serveDrainTimeout.String())
	// The parent context is already cancelled; the drain needs its own budget.
	drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveDrainTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(drainCtx); err != nil {
		log.Error("http shutdown", "error", err.Error())
	}
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Error("review drain cut short; unfinished reviews consumed no round", "error", err.Error())
	}
	log.Info("stopped")
	return runErr
}

// checkServeAppConfig refuses settings that are unsafe or incomplete for a
// service: MCP setup commands would run inside the checkout of an untrusted
// PR, and review comments must come out in the configured language.
func checkServeAppConfig() error {
	path, err := defaultConfigPath()
	if err != nil {
		return err
	}
	cfg, err := LoadAppConfig(path)
	if err != nil {
		return err
	}
	if cfg != nil && len(cfg.MCPServers) > 0 {
		return fmt.Errorf("mcp_servers is set in %s; ocr serve does not run MCP servers, remove the key", path)
	}
	if cfg == nil || strings.TrimSpace(cfg.Language) == "" {
		return fmt.Errorf("language is not set in %s; set it, for example: ocr config set language Vietnamese", path)
	}
	return nil
}

func buildServeForges(ctx context.Context, cfg *serveconfig.Config, httpc *http.Client) (map[forge.Platform]forge.Forge, error) {
	forges := map[forge.Platform]forge.Forge{}
	if cfg.GitHub != nil {
		gh, err := github.New(*cfg.GitHub, httpc, nil, nil)
		if err != nil {
			return nil, err
		}
		forges[forge.GitHub] = gh
	}
	if cfg.GitLab != nil {
		gl, err := gitlab.New(*cfg.GitLab, httpc, nil)
		if err != nil {
			return nil, err
		}
		gl.SetBotIndicators(cfg.BotIndicators)
		// The bot identity filters the service's own events. Reading it now
		// also proves the token works before any webhook is accepted.
		idCtx, cancel := context.WithTimeout(ctx, serveStartupTimeout)
		defer cancel()
		if _, err := gl.BotUserID(idCtx); err != nil {
			return nil, fmt.Errorf("gitlab: read the bot user with %s: %w", serveconfig.EnvGitLabToken, err)
		}
		forges[forge.GitLab] = gl
	}
	return forges, nil
}

// serveRoutes serves the handler at the root and under servePathPrefix, so
// the webhook URLs work whether or not the proxy strips the prefix.
func serveRoutes(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", h)
	mux.Handle(servePathPrefix+"/", http.StripPrefix(servePathPrefix, h))
	return mux
}

func platformNames(forges map[forge.Platform]forge.Forge) []string {
	var out []string
	for _, p := range []forge.Platform{forge.GitHub, forge.GitLab} {
		if forges[p] != nil {
			out = append(out, string(p))
		}
	}
	return out
}

func runServeHooksGitLab(cmd *cobra.Command, _ []string) error {
	mode := gitlab.HookDryRun
	switch {
	case serveHooksFlags.apply:
		mode = gitlab.HookApply
	case serveHooksFlags.remove:
		mode = gitlab.HookRemove
	}
	return serveHooksGitLab(cmd.Context(), processServeEnv(), mode, serveHooksFlags.project, cmd.OutOrStdout())
}

// serveHooksGitLab reads only the GitLab settings, so a GitHub setting that
// is missing from the shell running the command does not block it.
func serveHooksGitLab(ctx context.Context, env serveEnv, mode gitlab.HookMode, project string, out io.Writer) error {
	gitlabOnly := func(key string) string {
		if strings.HasPrefix(key, "OCR_SERVE_GITHUB_") {
			return ""
		}
		return env.getenv(key)
	}
	cfg, err := serveconfig.Load(gitlabOnly, env.readFile)
	if err != nil {
		return fmt.Errorf("serve config: %w", err)
	}
	if cfg.GitLab == nil {
		return fmt.Errorf("%s is required", serveconfig.EnvGitLabURL)
	}
	if cfg.GitLab.AdminToken == "" {
		return fmt.Errorf("%s is required; pass it for this run only, never keep it in the service environment", serveconfig.EnvGitLabAdminToken)
	}
	reg := gitlab.NewHookRegistrar(cfg.GitLab.URL, cfg.GitLab.AdminToken, cfg.GitLab.HookURL,
		cfg.GitLab.WebhookSecret, cfg.GitLab.BotUsername, env.httpc, nil)
	if _, err := reg.Run(ctx, gitlab.HookOptions{Mode: mode, Project: project}, out); err != nil {
		return err
	}
	return nil
}
