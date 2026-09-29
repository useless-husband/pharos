// Command pharos is a self-hosted uptime monitor and status page.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"

	"github.com/useless-husband/pharos/internal/config"
	"github.com/useless-husband/pharos/internal/demo"
	"github.com/useless-husband/pharos/internal/engine"
	"github.com/useless-husband/pharos/internal/notify"
	"github.com/useless-husband/pharos/internal/probe"
	"github.com/useless-husband/pharos/internal/store"
	"github.com/useless-husband/pharos/internal/web"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = ""

//go:embed example.yaml
var exampleConfig []byte

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

const usage = `Pharos — uptime monitoring and status pages in a single binary.

Usage:
  pharos <command> [flags]

Commands:
  run            Start monitoring and serve the status page and dashboard
  validate       Check a configuration file and report every problem
  init           Write an example configuration file
  hash-password  Create a password hash for server.admin.password_hash
  notify-test    Send a test notification through one notifier
  export         Write the status page as static files
  demo           Run with simulated data (no network, nothing to configure)
  healthcheck    Exit 0 if a running instance is healthy (for containers)
  version        Print the version

Run "pharos <command> -h" for the flags of a command.
The configuration file defaults to $PHAROS_CONFIG, then ./pharos.yaml.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "run":
		err = cmdRun(args)
	case "validate", "check":
		err = cmdValidate(args)
	case "init":
		err = cmdInit(args)
	case "hash-password":
		err = cmdHashPassword(args)
	case "notify-test":
		err = cmdNotifyTest(args)
	case "export":
		err = cmdExport(args)
	case "demo":
		err = cmdDemo(args)
	case "healthcheck":
		err = cmdHealthcheck(args)
	case "version", "--version", "-v":
		fmt.Println("pharos", buildVersion())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "pharos: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		var ce *config.Error
		if errors.As(err, &ce) {
			fmt.Fprintln(os.Stderr, ce.Error())
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, "pharos:", err)
		os.Exit(1)
	}
}

func defaultConfigPath() string {
	if p := os.Getenv("PHAROS_CONFIG"); p != "" {
		return p
	}
	return "pharos.yaml"
}

func newLogger(format, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	}
	return nil, fmt.Errorf("invalid log format %q (text or json)", format)
}

func setUserAgent() {
	ua := "Pharos/" + buildVersion() + " (+https://github.com/useless-husband/pharos)"
	probe.UserAgent = ua
	notify.UserAgent = ua
}

// app is a running instance.
type app struct {
	cfgPath string
	log     *slog.Logger
	store   *store.Store
	engine  *engine.Engine
	notify  *notify.Dispatcher
	mu      sync.Mutex // serializes reloads
}

func (a *app) reload() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg, err := config.Load(a.cfgPath)
	if err != nil {
		a.log.Error("reload rejected", "err", err)
		return err
	}
	old := a.engine.Config()
	if cfg.Storage.Path != old.Storage.Path || cfg.Server.Listen != old.Server.Listen {
		a.log.Warn("storage.path and server.listen take effect after a restart")
		cfg.Storage.Path, cfg.Server.Listen = old.Storage.Path, old.Server.Listen
	}
	if err := a.notify.Reload(cfg); err != nil {
		return err
	}
	return a.engine.Reload(cfg)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	fs.StringVar(cfgPath, "c", defaultConfigPath(), "configuration file (shorthand)")
	logFormat := fs.String("log-format", "text", "log format: text or json")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn or error")
	_ = fs.Parse(args)

	log, err := newLogger(*logFormat, *logLevel)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	setUserAgent()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, cfg, *cfgPath, log, nil, engine.Options{}, notify.Options{})
}

// serve runs the engine and HTTP server until ctx ends. st may be an
// already opened store (the demo's in-memory database); otherwise the
// configured database is opened.
func serve(ctx context.Context, cfg *config.Config, cfgPath string, log *slog.Logger, st *store.Store, eopts engine.Options, nopts notify.Options) error {
	log.Info("starting pharos", "version", buildVersion(), "config", cfgPath, "database", cfg.Storage.Path, "monitors", len(cfg.Monitors))
	if st == nil {
		var err error
		st, err = store.Open(ctx, cfg.Storage.Path)
		if err != nil {
			return err
		}
	}
	defer st.Close()
	if err := st.FailStaleNotifications(ctx); err != nil {
		return err
	}
	var err error
	nopts.Log, nopts.Logger = st, log
	disp, err := notify.NewDispatcher(cfg, nopts)
	if err != nil {
		return err
	}
	eopts.Store, eopts.Notifier, eopts.Logger = st, disp, log
	eng := engine.New(eopts)
	engineCtx, stopEngine := context.WithCancel(context.Background())
	defer stopEngine()
	if err := eng.Start(engineCtx, cfg); err != nil {
		return err
	}
	a := &app{cfgPath: cfgPath, log: log, store: st, engine: eng, notify: disp}
	var reload func() error
	if cfgPath != "" {
		reload = a.reload
	}
	srv, err := web.New(ctx, web.Options{Engine: eng, Store: st, Notify: disp, Logger: log, Version: buildVersion(), Reload: reload})
	if err != nil {
		return err
	}
	if cfg.Server.Admin.PasswordHash == "" {
		log.Warn("no admin password set: the dashboard only accepts connections from this machine")
	}

	if reload != nil {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		defer signal.Stop(hup)
		go func() {
			for range hup {
				if err := a.reload(); err == nil {
					log.Info("configuration reloaded on SIGHUP")
				}
			}
		}()
	}

	err = web.Run(ctx, cfg.Server.Listen, srv.Handler(), log)
	log.Info("shutting down")
	stopEngine()
	eng.Wait()
	drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	disp.Close(drain)
	cancel()
	log.Info("stopped")
	return err
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	fs.StringVar(cfgPath, "c", defaultConfigPath(), "configuration file (shorthand)")
	_ = fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	public := 0
	for _, g := range cfg.StatusPage.Groups {
		public += len(g.Monitors)
	}
	if len(cfg.StatusPage.Groups) == 0 {
		public = len(cfg.Monitors)
	}
	fmt.Printf("%s is valid: %s (%d on the status page), %s, %s.\n", *cfgPath,
		count(len(cfg.Monitors), "monitor"), public, count(len(cfg.Notifiers), "notifier"), count(len(cfg.Maintenance), "maintenance window"))
	if cfg.Server.Admin.PasswordHash == "" {
		fmt.Println("Note: no admin password is set; the dashboard will only accept local connections.")
	}
	return nil
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	out := fs.String("o", "pharos.yaml", `file to write, or "-" for standard output`)
	force := fs.Bool("force", false, "overwrite an existing file")
	_ = fs.Parse(args)
	if *out == "-" {
		_, err := os.Stdout.Write(exampleConfig)
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(*out, flags, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s already exists (use -force to overwrite)", *out)
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(exampleConfig); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("Wrote %s. Edit the monitors, then run:\n  pharos validate -c %s\n  pharos run -c %s\n", *out, *out, *out)
	return nil
}

func cmdHashPassword(args []string) error {
	fs := flag.NewFlagSet("hash-password", flag.ExitOnError)
	cost := fs.Int("cost", 12, "bcrypt cost")
	_ = fs.Parse(args)
	var pw []byte
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Password: ")
		a, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, "Repeat password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		if string(a) != string(b) {
			return errors.New("passwords do not match")
		}
		pw = a
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		pw = []byte(strings.TrimRight(line, "\r\n"))
	}
	if len(pw) < 10 {
		return errors.New("use a password of at least 10 characters")
	}
	hash, err := bcrypt.GenerateFromPassword(pw, *cost)
	if err != nil {
		return err
	}
	fmt.Println(string(hash))
	if term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintln(os.Stderr, "\nPut it in your configuration (quote it, it contains $):\n  server:\n    admin:\n      password_hash: \""+string(hash)+"\"")
	}
	return nil
}

func cmdNotifyTest(args []string) error {
	fs := flag.NewFlagSet("notify-test", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	fs.StringVar(cfgPath, "c", defaultConfigPath(), "configuration file (shorthand)")
	_ = fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		var names []string
		for _, n := range cfg.Notifiers {
			names = append(names, n.Name)
		}
		return fmt.Errorf("usage: pharos notify-test [-c file] <notifier>   (configured: %s)", strings.Join(names, ", "))
	}
	setUserAgent()
	d, err := notify.NewDispatcher(cfg, notify.Options{})
	if err != nil {
		return err
	}
	name := fs.Arg(0)
	if err := d.Test(context.Background(), name); err != nil {
		return fmt.Errorf("notifier %s: %w", name, err)
	}
	fmt.Printf("Test notification sent through %s.\n", name)
	return nil
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	fs.StringVar(cfgPath, "c", defaultConfigPath(), "configuration file (shorthand)")
	out := fs.String("o", "site", "output directory")
	useDemo := fs.Bool("demo", false, "export the demo data instead of a real database")
	lang := fs.String("lang", "en", "language of the demo (en or zh-TW)")
	_ = fs.Parse(args)
	ctx := context.Background()

	var cfg *config.Config
	var st *store.Store
	var err error
	if *useDemo {
		cfg, st, err = demoData(ctx, *lang, ":0")
		if err != nil {
			return err
		}
	} else {
		cfg, err = config.Load(*cfgPath)
		if err != nil {
			return err
		}
		if _, err := os.Stat(cfg.Storage.Path); err != nil {
			return fmt.Errorf("database %s: %w (run pharos first)", cfg.Storage.Path, err)
		}
		st, err = store.Open(ctx, cfg.Storage.Path)
		if err != nil {
			return err
		}
	}
	defer st.Close()
	eng := engine.New(engine.Options{Store: st, ReadOnly: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err := eng.Start(ctx, cfg); err != nil {
		return err
	}
	srv, err := web.New(ctx, web.Options{Engine: eng, Store: st, Version: buildVersion()})
	if err != nil {
		return err
	}
	files, err := srv.Export(ctx, *out)
	if err != nil {
		return err
	}
	fmt.Printf("Wrote %d files to %s/\n", len(files), strings.TrimSuffix(*out, "/"))
	return nil
}

// demoData creates a seeded, in-memory demo database: nothing touches the
// disk or the network.
func demoData(ctx context.Context, lang, listen string) (*config.Config, *store.Store, error) {
	cfg, err := demo.Config(":memory:", lang, listen)
	if err != nil {
		return nil, nil, err
	}
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		return nil, nil, err
	}
	if err := demo.Seed(ctx, st, cfg, time.Now()); err != nil {
		st.Close()
		return nil, nil, err
	}
	return cfg, st, nil
}

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "address to serve on")
	lang := fs.String("lang", "en", "language: en or zh-TW")
	_ = fs.Parse(args)
	log, _ := newLogger("text", "info")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, st, err := demoData(ctx, *lang, *listen)
	if err != nil {
		return err
	}
	host := *listen
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	fmt.Fprintf(os.Stderr, "\nPharos demo with 90 days of simulated history (kept in memory, nothing is sent anywhere).\n  Status page: http://%s/\n  Dashboard:   http://%s/admin\nPress Ctrl+C to stop.\n\n", host, host)
	return serve(ctx, cfg, "", log, st, engine.Options{NewProber: demo.NewProber}, notify.Options{NewSender: demo.NewSender})
}

func cmdHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	target := fs.String("url", "", "health endpoint (default: derived from server.listen in the configuration)")
	cfgPath := fs.String("config", defaultConfigPath(), "configuration file")
	_ = fs.Parse(args)
	if *target == "" {
		*target = "http://127.0.0.1:8080/healthz"
		if cfg, err := config.Load(*cfgPath); err == nil {
			host, port, _ := net.SplitHostPort(cfg.Server.Listen)
			if host == "" || host == "0.0.0.0" || host == "::" {
				host = "127.0.0.1"
			}
			*target = "http://" + net.JoinHostPort(host, port) + "/healthz"
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(*target)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}
