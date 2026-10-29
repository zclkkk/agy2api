package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/zclkkk/agy2api/internal/account"
	ag "github.com/zclkkk/agy2api/internal/antigravity"
	"github.com/zclkkk/agy2api/internal/config"
)

// label names an account for people: its email, then its display name.
func label(a account.Account) string {
	if a.Name == "" {
		return a.Email
	}
	return fmt.Sprintf("%s (%s)", a.Email, a.Name)
}

// printAccounts lists the accounts with their API keys.
func printAccounts(w io.Writer, accounts []account.Account, loc *time.Location) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tEMAIL\tNAME\tAPI KEY\tCREATED")
	for _, a := range accounts {
		name := a.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.Email, name, a.APIKey, a.CreatedAt.In(loc).Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

func quota(ctx context.Context, cfg config.Config, client *http.Client, agent *ag.UserAgent) error {
	accounts, err := account.List(cfg.DataDir)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Fprintln(os.Stderr, "agy2api:", noAccounts)
		return nil
	}
	if _, err := agent.Refresh(ctx, client); err != nil {
		fmt.Fprintln(os.Stderr, "agy2api: using", agent.String()+":", err)
	}
	return printQuota(os.Stdout, accounts, func(a account.Account) (ag.Catalog, error) {
		upstream, err := upstreamFor(a, client, agent)
		if err != nil {
			return ag.Catalog{}, err
		}
		return upstream.Models(ctx)
	}, time.Local)
}

// printQuota lists each account's models with their remaining quota. An
// account that cannot be queried shows its error and the rest still print.
func printQuota(w io.Writer, accounts []account.Account, catalog func(account.Account) (ag.Catalog, error), loc *time.Location) error {
	failed := 0
	for i, a := range accounts {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s  %s\n", a.ID, label(a))
		c, err := catalog(a)
		if err != nil {
			failed++
			fmt.Fprintln(w, "error:", err)
			continue
		}
		if err := printModels(w, c, loc); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("quota unavailable for %d of %d accounts", failed, len(accounts))
	}
	return nil
}

// printModels lists the models /v1/models offers, with the account's remaining
// quota for each.
func printModels(w io.Writer, catalog ag.Catalog, loc *time.Location) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tQUOTA\tRESETS")
	for _, id := range catalog.ListedModelIDs() {
		info, _ := catalog.Info(id)
		quota, reset := "-", "-"
		if q := info.Quota; q != nil {
			quota = fmt.Sprintf("%.1f%%", q.Remaining*100)
			if t, err := time.Parse(time.RFC3339, q.Reset); err == nil {
				reset = t.In(loc).Format("2006-01-02 15:04")
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", id, quota, reset)
	}
	return tw.Flush()
}
