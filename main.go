// Command gh-workflow-exporter exports GitHub Actions workflow health as
// Prometheus metrics. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jedwards1230/gh-workflow-exporter/internal/collector"
	"github.com/jedwards1230/gh-workflow-exporter/internal/config"
	"github.com/jedwards1230/gh-workflow-exporter/internal/github"
	"github.com/jedwards1230/gh-workflow-exporter/internal/version"
)

const (
	defaultConfigPath = "/etc/gh-workflow-exporter/config.yaml"
	defaultSecretDir  = "/etc/gh-app" //nolint:gosec // G101: a directory path, not a credential
	defaultListen     = ":9877"
	defaultLogLevel   = "info"
)

// options are the resolved command-line settings.
type options struct {
	configPath     string
	configExplicit bool // set by flag or env, so it must exist
	secretDir      string
	listen         string
	logLevel       slog.Level
	apiURL         string
	showVersion    bool
}

// parseOptions resolves settings with precedence flags > env > defaults.
func parseOptions(args []string, getenv func(string) string, stderr io.Writer) (*options, error) {
	envOr := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	fset := flag.NewFlagSet("gh-workflow-exporter", flag.ContinueOnError)
	fset.SetOutput(stderr)
	o := &options{}
	var level string
	fset.StringVar(&o.configPath, "config", envOr("GHWE_CONFIG", defaultConfigPath), "config file `path` (env GHWE_CONFIG)")
	fset.StringVar(&o.secretDir, "secret-dir", envOr("GHWE_SECRET_DIR", defaultSecretDir), "`dir` holding app_id, installation_id and private_key (env GHWE_SECRET_DIR)")
	fset.StringVar(&o.listen, "listen", envOr("GHWE_LISTEN", defaultListen), "HTTP listen `address` (env GHWE_LISTEN)")
	fset.StringVar(&level, "log-level", envOr("GHWE_LOG_LEVEL", defaultLogLevel), "log `level`: debug, info, warn or error (env GHWE_LOG_LEVEL)")
	fset.StringVar(&o.apiURL, "github-api-url", envOr("GHWE_GITHUB_API_URL", github.DefaultBaseURL), "GitHub REST API base `URL` (env GHWE_GITHUB_API_URL)")
	fset.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	if err := fset.Parse(args); err != nil {
		return nil, err
	}
	if fset.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fset.Args(), " "))
	}
	o.configExplicit = getenv("GHWE_CONFIG") != ""
	fset.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			o.configExplicit = true
		}
	})
	if err := o.logLevel.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q", level)
	}
	return o, nil
}

// loadConfig reads the config file. A missing file at the default path means
// "all defaults"; a path the operator named must exist.
func loadConfig(o *options, log *slog.Logger) (*config.Config, error) {
	cfg, err := config.Load(o.configPath)
	if err != nil && !o.configExplicit && errors.Is(err, fs.ErrNotExist) {
		log.Warn("config file not found; using defaults", "path", o.configPath)
		return config.Default(), nil
	}
	return cfg, err
}

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "gh-workflow-exporter:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string) error {
	o, err := parseOptions(args, getenv, os.Stderr)
	if err != nil {
		return err
	}
	if o.showVersion {
		fmt.Println("gh-workflow-exporter", version.Info())
		return nil
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: o.logLevel}))

	cfg, err := loadConfig(o, log)
	if err != nil {
		return err
	}
	creds, err := config.LoadCredentials(o.secretDir, getenv)
	if err != nil {
		return fmt.Errorf("loading GitHub App credentials: %w", err)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// The callback is only invoked by requests made from col.Run, which
	// starts after col is assigned.
	var col *collector.Collector
	api, err := github.New(creds.AppID, creds.InstallationID, creds.PrivateKeyPEM, github.Options{
		BaseURL:     o.apiURL,
		UserAgent:   "gh-workflow-exporter/" + version.Short(),
		OnRateLimit: func(v float64) { col.SetRateLimitRemaining(v) },
	})
	if err != nil {
		return fmt.Errorf("GitHub client: %w", err)
	}
	col = collector.New(api, cfg, log, reg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", o.listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", o.listen, err)
	}
	srv := &http.Server{Handler: newMux(reg, col.Ready), ReadHeaderTimeout: 10 * time.Second}

	log.Info("starting", "version", version.Info(), "listen", ln.Addr().String(),
		"poll_interval", cfg.Interval().String(), "app_id", creds.AppID, "installation_id", creds.InstallationID)

	var wg sync.WaitGroup
	wg.Go(func() { col.Run(ctx) })
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
	case err = <-serveErr:
		stop()
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if serr := srv.Shutdown(shutCtx); serr != nil {
		log.Warn("HTTP shutdown", "err", serr)
	}
	wg.Wait()
	log.Info("stopped")
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP server: %w", err)
	}
	return nil
}

func newMux(reg *prometheus.Registry, ready func() bool) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "waiting for the first successful poll", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}
