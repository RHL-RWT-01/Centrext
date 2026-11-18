package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"centrext/internal/connect"
	"centrext/internal/infrastructure/keystore"
)

// keychainTokens adapts the keystore to connect.TokenStore.
type keychainTokens struct{ home string }

func (k keychainTokens) Store(id, tok string) error     { return keystore.StoreAgentToken(k.home, id, tok) }
func (k keychainTokens) Load(id string) (string, error) { return keystore.StoredAgentToken(k.home, id) }
func (k keychainTokens) Delete(id string) error         { return keystore.DeleteAgentToken(k.home, id) }

// binaryPath is the installed centrext that client configs will run.
func binaryPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if strings.Contains(p, string(filepath.Separator)+"go-build") {
		return "", errors.New("this is a temporary `go run` binary; build or install centrext first (e.g. `go install ./cmd/centrext`) and run connect from it")
	}
	return p, nil
}

func connectEnv() (connect.Env, error) {
	bin, err := binaryPath()
	if err != nil {
		return connect.Env{}, err
	}
	return connectEnvAt(bin)
}

// connectEnvAt builds the env for a given binary ("" = this process's path as is).
func connectEnvAt(bin string) (connect.Env, error) {
	if bin == "" {
		p, err := os.Executable()
		if err != nil {
			return connect.Env{}, err
		}
		bin = p
	}
	h, err := home()
	if err != nil {
		return connect.Env{}, err
	}
	uh, err := os.UserHomeDir()
	if err != nil {
		return connect.Env{}, err
	}
	cd, _ := os.UserConfigDir()
	host, _ := os.Hostname()
	if host == "" {
		host = "local"
	}
	return connect.Env{UserHome: uh, ConfigDir: cd, CentrextHome: h, Binary: bin, Hostname: host, Tokens: keychainTokens{h}}, nil
}

func (a *app) cmdConnect(ctx context.Context, args []string, remove bool) error {
	name := "connect"
	if remove {
		name = "disconnect"
	}
	fs := newFlags(name)
	dir := fs.String("dir", "", "project folder for cursor (default: this repo)")
	var nss multi
	fs.Var(&nss, "namespace", "restrict a new agent to this namespace (repeatable; default: all)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "CLIENT ("+strings.Join(connect.Clients, ", ")+")"); err != nil {
		return err
	}
	env, err := connectEnv()
	if err != nil {
		return err
	}
	opts := connect.Options{Client: pos[0], Dir: *dir}
	for _, n := range nss {
		ns, err := a.svc.FindNamespace(ctx, a.user, n)
		if err != nil {
			return fmt.Errorf("namespace %s: %w", n, err)
		}
		opts.Namespaces = append(opts.Namespaces, ns.ID)
	}
	var rep connect.Report
	if remove {
		rep, err = connect.Disconnect(ctx, a.svc, a.user, env, opts)
	} else {
		rep, err = connect.Connect(ctx, a.svc, a.user, env, opts)
	}
	printReport(rep, remove)
	return err
}

func printReport(rep connect.Report, removed bool) {
	switch {
	case removed:
		fmt.Printf("Disconnected %s.\n", rep.Client)
	case rep.NewAgent:
		fmt.Printf("Connected %s as new agent %s (token in the OS keychain).\n", rep.Client, rep.AgentID)
	case rep.AgentID != "":
		fmt.Printf("Connected %s with existing agent %s.\n", rep.Client, rep.AgentID)
	}
	if rep.Project != "" {
		fmt.Printf("Project: %s\n", rep.Project)
	}
	if len(rep.Changed) > 0 {
		fmt.Println("Updated:")
		for _, c := range rep.Changed {
			fmt.Println("  " + c)
		}
	} else if rep.AgentID != "" || removed {
		fmt.Println("Client config was already up to date.")
	}
	for _, m := range rep.Manual {
		fmt.Println("To do: " + m)
	}
	for _, n := range rep.Notes {
		fmt.Println(n)
	}
}
