package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"
)

const accountsHelp = `Manage the account pool.

Usage:
  claude-balancer accounts add                 Sign in and add the account
  claude-balancer accounts list                Show pooled accounts
  claude-balancer accounts mode <account> <mode> Set routing to normal, priority, or paused
  claude-balancer accounts rm <email>          Drop an account

Flags:
  -state string      state database (default %s)
  -json              machine-readable output, list only
`

func printAccountsHelp(w io.Writer) {
	fmt.Fprintf(w, accountsHelp, defaultStatePath())
}

func accountsCmd(args []string) error {
	if len(args) == 0 {
		printAccountsHelp(os.Stderr)
		return errors.New("no subcommand given")
	}

	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	fs.Usage = func() { printAccountsHelp(os.Stderr) }
	path := fs.String("state", defaultStatePath(), "state database")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	store, err := openStateStore(*path)
	if err != nil {
		return err
	}
	defer store.Close()
	pool, err := loadPool(store)
	if err != nil {
		return err
	}

	switch args[0] {
	case "add":
		return addAccount(pool)
	case "list":
		return listAccounts(pool, *asJSON)
	case "mode":
		if fs.NArg() != 2 {
			return errors.New("accounts mode needs an account and one of: normal, priority, paused")
		}
		account, err := pool.resolve(fs.Arg(0))
		if err != nil {
			return err
		}
		mode := routingMode(fs.Arg(1))
		if err := pool.setRoutingMode(account, mode); err != nil {
			return err
		}
		fmt.Printf("set %s routing mode to %s\n", describe(account), mode)
		return nil
	case "rm":
		who := fs.Arg(0)
		if who == "" {
			return errors.New("accounts rm needs an email")
		}
		account, err := pool.resolve(who)
		if err != nil {
			return err
		}
		if err := pool.remove(account); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", describe(account))
		return nil
	case "help", "-h", "--help":
		printAccountsHelp(os.Stdout)
		return nil
	}
	printAccountsHelp(os.Stderr)
	return fmt.Errorf("unknown subcommand %q", args[0])
}

func addAccount(pool *Pool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	credentials, err := claudeLogin(ctx, http.DefaultClient, claudeOAuthPort, true)
	if err != nil {
		return err
	}
	account := accountFromClaudeCredentials(credentials)
	if err := pool.add(account); err != nil {
		return err
	}
	fmt.Printf("added %s\n", describe(account))
	return nil
}

func describe(a *Account) string {
	if a.email() == "" {
		return a.id()
	}
	return fmt.Sprintf("%s (%s)", a.email(), a.plan())
}

func listAccounts(pool *Pool, asJSON bool) error {
	accounts := pool.sorted()
	if asJSON {
		out := make([]map[string]any, 0, len(accounts))
		for _, a := range accounts {
			candidate := a.routingCandidate()
			out = append(out, map[string]any{
				"id":           a.id(),
				"email":        a.email(),
				"plan":         a.plan(),
				"paused":       candidate.paused,
				"routing_mode": candidate.mode,
				"token":        tokenStatus(a),
				"expiry":       a.expires(),
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(accounts) == 0 {
		fmt.Println("no accounts; add one with: claude-balancer accounts add")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EMAIL\tPLAN\tROUTING\tTOKEN")
	for _, a := range accounts {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", label(a), a.plan(), routing(a), tokenStatus(a))
	}
	return w.Flush()
}

func routing(a *Account) string {
	candidate := a.routingCandidate()
	if candidate.paused {
		return "paused"
	}
	if candidate.mode == routingModePriority {
		return string(routingModePriority)
	}
	return string(routingModeNormal)
}

func tokenStatus(a *Account) string {
	expiry := a.expires()
	switch {
	case expiry.IsZero():
		return "unknown"
	case time.Now().After(expiry):
		return "expired, refreshes on first use"
	default:
		return "valid for " + time.Until(expiry).Round(time.Minute).String()
	}
}

func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return home
}
