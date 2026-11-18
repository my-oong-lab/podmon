// podmon is a small, dependency-free dashboard for a home server: CPU, memory,
// disk, network and the containers running under Podman.
package main

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

const minPasswordLen = 12

type config struct {
	listen       string
	user         string
	password     string
	noAuth       bool
	tlsCert      string
	tlsKey       string
	allowedHosts []string
	podmanSocket string
	podmanBin    string
	interval     time.Duration
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func readPasswordFile(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s must not be readable by group or others (chmod 600)", path)
	}
	b, err := readSmall(path, 4096)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func loadConfig(args []string, getenv func(string) string) (*config, error) {
	cfg := &config{}
	var hosts string
	flags := flag.NewFlagSet("podmon", flag.ContinueOnError)
	flags.StringVar(&cfg.listen, "listen", "127.0.0.1:8080", "address to listen on, host:port")
	flags.StringVar(&cfg.user, "user", firstNonEmpty(getenv("PODMON_USER"), "admin"), "login name (env PODMON_USER)")
	flags.BoolVar(&cfg.noAuth, "no-auth", false, "disable login (only allowed on a loopback listen address)")
	flags.StringVar(&cfg.tlsCert, "tls-cert", "", "TLS certificate file, PEM")
	flags.StringVar(&cfg.tlsKey, "tls-key", "", "TLS private key file, PEM")
	flags.StringVar(&hosts, "allowed-hosts", "", "comma-separated Host header allowlist, names without port")
	flags.StringVar(&cfg.podmanSocket, "podman-socket", "", "Podman API socket path (default: auto-detect)")
	flags.StringVar(&cfg.podmanBin, "podman-bin", "", "absolute path of the podman binary for the CLI fallback (default: look up in PATH)")
	flags.DurationVar(&cfg.interval, "interval", 2*time.Second, "system sampling interval, 1s to 60s")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}

	pwFile, pwEnv := getenv("PODMON_PASSWORD_FILE"), getenv("PODMON_PASSWORD")
	switch {
	case pwFile != "" && pwEnv != "":
		return nil, errors.New("set only one of PODMON_PASSWORD_FILE and PODMON_PASSWORD")
	case pwFile != "":
		pw, err := readPasswordFile(pwFile)
		if err != nil {
			return nil, fmt.Errorf("PODMON_PASSWORD_FILE: %w", err)
		}
		cfg.password = pw
	default:
		cfg.password = pwEnv
	}

	host, _, err := net.SplitHostPort(cfg.listen)
	if err != nil {
		return nil, fmt.Errorf("-listen: %w", err)
	}
	if cfg.noAuth {
		if !isLoopbackHost(host) {
			return nil, errors.New("-no-auth is only allowed with a loopback -listen address such as 127.0.0.1:8080")
		}
		if cfg.password != "" {
			return nil, errors.New("-no-auth conflicts with a configured password")
		}
	} else {
		if cfg.user == "" || len(cfg.user) > 64 || strings.ContainsAny(cfg.user, ":\r\n") {
			return nil, errors.New("login name must be 1-64 characters and must not contain ':'")
		}
		if len(cfg.password) < minPasswordLen {
			return nil, fmt.Errorf("a password of at least %d characters is required: set PODMON_PASSWORD_FILE (preferred) or PODMON_PASSWORD, or use -no-auth on loopback", minPasswordLen)
		}
	}
	if (cfg.tlsCert == "") != (cfg.tlsKey == "") {
		return nil, errors.New("-tls-cert and -tls-key must be given together")
	}
	if cfg.interval < time.Second || cfg.interval > time.Minute {
		return nil, errors.New("-interval must be between 1s and 60s")
	}
	if cfg.podmanBin != "" && !filepath.IsAbs(cfg.podmanBin) {
		return nil, errors.New("-podman-bin must be an absolute path")
	}
	for _, h := range strings.Split(hosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			cfg.allowedHosts = append(cfg.allowedHosts, hostOnly(h))
		}
	}
	if cfg.noAuth && len(cfg.allowedHosts) == 0 {
		cfg.allowedHosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	return cfg, nil
}

func run(cfg *config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	assets, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	s := newSampler(cfg.interval)
	pm := newPodman(cfg.podmanSocket, cfg.podmanBin)
	a, err := newApp(cfg, s, pm, assets)
	if err != nil {
		return err
	}
	go s.run(ctx)
	go pm.run(ctx)

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           a.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	if cfg.tlsCert != "" {
		srv.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	host, _, _ := net.SplitHostPort(cfg.listen)
	scheme := "http"
	if cfg.tlsCert != "" {
		scheme = "https"
	} else if !isLoopbackHost(host) {
		log.Printf("warning: serving plain HTTP on a non-loopback address; the login is sent unencrypted. Use -tls-cert/-tls-key or put a TLS proxy in front.")
	}
	if cfg.noAuth {
		log.Printf("warning: authentication is disabled")
	}
	log.Printf("podmon listening on %s://%s", scheme, cfg.listen)

	errc := make(chan error, 1)
	go func() {
		if cfg.tlsCert != "" {
			errc <- srv.ListenAndServeTLS(cfg.tlsCert, cfg.tlsKey)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("podmon: ")
	cfg, err := loadConfig(os.Args[1:], os.Getenv)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		fmt.Fprintln(os.Stderr, "podmon:", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		log.Fatalf("%v", err)
	}
}
