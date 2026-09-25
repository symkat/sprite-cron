package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/symkat/sprite-cron/internal/app"
	"golang.org/x/term"
)

var version = "dev"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func output(v any) { e := json.NewEncoder(os.Stdout); e.SetIndent("", "  "); _ = e.Encode(v) }
func duration(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, e := strconv.Atoi(strings.TrimSuffix(s, "d"))
		return time.Duration(n) * 24 * time.Hour, e
	}
	return time.ParseDuration(s)
}

// Accept options on either side of positional arguments.
func parse(f *flag.FlagSet, args []string) error {
	options, positions := []string{}, []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			options = append(options, arg)
			name := strings.TrimLeft(strings.SplitN(arg, "=", 2)[0], "-")
			option := f.Lookup(name)
			if option == nil {
				return fmt.Errorf("unknown option: %s", name)
			}
			isBool := false
			if b, ok := option.Value.(interface{ IsBoolFlag() bool }); ok {
				isBool = b.IsBoolFlag()
			}
			if !strings.Contains(arg, "=") && !isBool {
				if i+1 >= len(args) {
					return fmt.Errorf("missing value for %s", arg)
				}
				i++
				options = append(options, args[i])
			}
		} else {
			positions = append(positions, arg)
		}
	}
	return f.Parse(append(options, positions...))
}
func password(stdin bool) (string, error) {
	if stdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 1026))
		return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("password requires a terminal; use --password-stdin for a secure pipe")
	}
	fmt.Fprint(os.Stderr, "Password: ")
	b, e := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if e != nil {
		return "", e
	}
	fmt.Fprint(os.Stderr, "Confirm password: ")
	confirmation, e := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if e != nil {
		return "", e
	}
	if string(b) != string(confirmation) {
		return "", errors.New("passwords do not match")
	}
	return string(b), nil
}

// keygen works before configuration or database initialization. Keep stdout
// machine-readable so an SSH caller can securely redirect it into secrets import.
func keygen(args []string, out io.Writer) error {
	f := flag.NewFlagSet("keygen", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	flySecret := f.Bool("fly-secret", false, "print a Fly secrets import line for key v1")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("keygen takes no positional arguments")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	value := base64.StdEncoding.EncodeToString(b)
	if *flySecret {
		keyring, err := json.Marshal(map[string]string{"v1": value})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "SPRITE_CRON_KEYS=%s\n", keyring)
		return err
	}
	_, err := fmt.Fprintln(out, value)
	return err
}

func main() {
	syscall.Umask(0077)
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Print(`Sprite Cron — durable scheduling for Sprite exec and HTTP jobs

Commands:
  serve [--listen :8080] [--db PATH] [--origin https://HOST]
        [--insecure-local] [--workers 10] [--per-target 2] [--dispatch-disabled]
  init [--db PATH]
  keygen [--fly-secret]
  users add NAME [--role admin|operator|reader] [--service-account] [--password-stdin]
  users list
  users reset-password NAME [--password-stdin]
  users set-role NAME --role ROLE
  users disable NAME | users enable NAME
  tokens create --user NAME --name LABEL --scopes jobs:read,jobs:write --targets TARGET_ID --expires-in 90d
  tokens list [--user NAME]
  tokens revoke TOKEN_ID
  backup --out PATH
  restore --from PATH (server must be stopped; revokes sessions and API tokens)
  maintenance (wait without opening the database)
  auth reset
  credentials reencrypt
  version

All database commands accept --db PATH (or SPRITE_CRON_DB).
User/token administration requires local database permissions. There is no signup API.
See README.md for deployment, API examples, recovery, and credential management.
`)
		return nil
	}
	if args[0] == "maintenance" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		slog.Info("maintenance mode: database closed, HTTP and scheduler stopped")
		<-ctx.Done()
		return nil
	}
	if args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	if args[0] == "keygen" {
		return keygen(args[1:], os.Stdout)
	}
	command := args[0]
	sub := ""
	rest := args[1:]
	if command == "users" || command == "tokens" || command == "auth" || command == "credentials" {
		if len(rest) == 0 {
			return errors.New("subcommand required")
		}
		sub, rest = rest[0], rest[1:]
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	dbPath := f.String("db", env("SPRITE_CRON_DB", "data/sprite-cron.db"), "database path")
	listen := f.String("listen", env("SPRITE_CRON_LISTEN", ":8080"), "listen address")
	origin := f.String("origin", env("SPRITE_CRON_ORIGIN", "http://127.0.0.1:8080"), "browser origin")
	insecure := f.Bool("insecure-local", os.Getenv("SPRITE_CRON_INSECURE_LOCAL") == "true", "local HTTP cookies")
	workers := f.Int("workers", 10, "concurrent attempts")
	perTarget := f.Int("per-target", 2, "concurrent attempts per target")
	disabled := f.Bool("dispatch-disabled", os.Getenv("SPRITE_CRON_DISPATCH_DISABLED") == "true", "pause dispatch for recovery")
	role := f.String("role", "operator", "role")
	service := f.Bool("service-account", false, "no browser login")
	stdin := f.Bool("password-stdin", false, "read password from stdin")
	user := f.String("user", "", "token owner")
	name := f.String("name", "", "token name")
	scopes := f.String("scopes", "", "comma-separated scopes")
	targets := f.String("targets", "", "comma-separated target IDs (admin may use *)")
	expiry := f.String("expires-in", "90d", "token expiry")
	dest := f.String("out", "", "backup destination")
	source := f.String("from", "", "backup to restore")
	if err := parse(f, rest); err != nil {
		return err
	}
	if command == "restore" {
		if *source == "" {
			return errors.New("--from is required")
		}
		return app.Restore(*dbPath, *source)
	}
	if command == "serve" {
		if *workers < 1 || *workers > 100 || *perTarget < 1 || *perTarget > *workers {
			return errors.New("workers must be 1–100 and per-target 1–workers")
		}
		return serve(*dbPath, *listen, *origin, *insecure, *workers, *perTarget, *disabled)
	}
	s, err := app.Open(*dbPath, command == "init")
	if err != nil {
		return err
	}
	defer s.DB.Close()
	positional := f.Args()
	one := func() (string, error) {
		if len(positional) != 1 {
			return "", errors.New("exactly one name or ID required")
		}
		return positional[0], nil
	}
	switch command {
	case "init":
		fmt.Println("Initialized", s.Path)
		return nil
	case "users":
		if sub == "list" {
			users, e := s.Users()
			if e == nil {
				output(users)
			}
			return e
		}
		who, e := one()
		if e != nil {
			return e
		}
		switch sub {
		case "add":
			pw := ""
			if !*service {
				pw, e = password(*stdin)
				if e != nil {
					return e
				}
			}
			return s.AddUser(who, *role, pw, *service)
		case "reset-password":
			pw, e := password(*stdin)
			if e != nil {
				return e
			}
			return s.ChangeUser(who, "password", pw, "local-cli")
		case "set-role":
			return s.ChangeUser(who, "role", *role, "local-cli")
		case "disable", "enable":
			return s.ChangeUser(who, sub, "", "local-cli")
		}
	case "tokens":
		switch sub {
		case "create":
			ttl, e := duration(*expiry)
			if e != nil {
				return e
			}
			raw, info, e := s.NewToken(*user, *name, strings.Split(*scopes, ","), strings.Split(*targets, ","), ttl, "local-cli")
			if e == nil {
				output(map[string]any{"token": raw, "metadata": info})
			}
			return e
		case "list":
			all, e := s.Tokens()
			if e != nil {
				return e
			}
			if *user == "" {
				output(all)
				return nil
			}
			users, e := s.Users()
			if e != nil {
				return e
			}
			id := ""
			for _, u := range users {
				if u.Username == *user {
					id = u.ID
				}
			}
			if id == "" {
				return errors.New("unknown user")
			}
			filtered := []app.TokenInfo{}
			for _, t := range all {
				if t.UserID == id {
					filtered = append(filtered, t)
				}
			}
			output(filtered)
			return nil
		case "revoke":
			id, e := one()
			if e != nil {
				return e
			}
			return s.RevokeToken(id, "local-cli")
		}
	case "backup":
		if *dest == "" {
			return errors.New("--out is required")
		}
		if err = s.Backup(*dest); err != nil {
			return err
		}
		fmt.Println("Backup created:", *dest)
		return nil
	case "auth":
		if sub == "reset" {
			return s.ResetAccess()
		}
	case "credentials":
		if sub == "reencrypt" {
			v, e := app.NewVault(os.Getenv("SPRITE_CRON_KEYS"), env("SPRITE_CRON_ACTIVE_KEY", "v1"))
			if e != nil {
				return e
			}
			return s.Reencrypt(v)
		}
	}
	return errors.New("unknown command; run sprite-cron help")
}
func serve(path, listen, origin string, insecure bool, workers, perTarget int, disabled bool) error {
	u, e := url.Parse(origin)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("origin must be a URL without path, query, or credentials")
	}
	if insecure {
		host, _, err := net.SplitHostPort(listen)
		if err != nil || host != "127.0.0.1" && host != "::1" && host != "localhost" {
			return errors.New("--insecure-local requires a loopback listen address")
		}
		if u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
			return errors.New("local origin must be a loopback HTTP URL")
		}
	} else if u.Scheme != "https" {
		return errors.New("production origin must use HTTPS; use --insecure-local only on loopback")
	}
	v, e := app.NewVault(os.Getenv("SPRITE_CRON_KEYS"), env("SPRITE_CRON_ACTIVE_KEY", "v1"))
	if e != nil {
		return e
	}
	path, e = filepath.Abs(path)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	lock, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return errors.New("another scheduler already holds this database lock")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	s, e := app.Open(path, true)
	if e != nil {
		return e
	}
	defer s.DB.Close()
	executor := app.NewExecutor(s, v)
	scheduler := app.NewScheduler(s, executor, workers, perTarget)
	scheduler.Disabled = disabled
	application := app.NewApp(s, v, executor, scheduler, origin, insecure)
	server := &http.Server{Addr: listen, Handler: application.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- scheduler.Loop(ctx) }()
	httpDone := make(chan error, 1)
	go func() {
		slog.Info("Sprite Cron listening", "address", listen, "origin", origin, "dispatch_disabled", disabled)
		httpDone <- server.ListenAndServe()
	}()
	var result error
	select {
	case <-ctx.Done():
	case result = <-httpDone:
		cancel()
	case result = <-schedulerDone:
		cancel()
		schedulerDone = nil
	}
	shutdown, stop := context.WithTimeout(context.Background(), 25*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	if schedulerDone != nil {
		select {
		case err := <-schedulerDone:
			if result == nil {
				result = err
			}
		case <-shutdown.Done():
			result = errors.New("shutdown timed out; interrupted runs will be reconciled at restart")
		}
	}
	if errors.Is(result, http.ErrServerClosed) {
		return nil
	}
	return result
}
