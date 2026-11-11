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
	return printQuota(os.Stdout, accounts, func(a account.Account) (ag.QuotaSummary, error) {
		upstream, err := upstreamFor(a, client, agent)
		if err != nil {
			return ag.QuotaSummary{}, err
		}
		return upstream.QuotaSummary(ctx)
	}, time.Local)
}

// printQuota lists each account's shared quota windows. An
// account that cannot be queried shows its error and the rest still print.
func printQuota(w io.Writer, accounts []account.Account, fetch func(account.Account) (ag.QuotaSummary, error), loc *time.Location) error {
	failed := 0
	for i, a := range accounts {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s  %s\n", a.ID, label(a))
		summary, err := fetch(a)
		if err != nil {
			failed++
			fmt.Fprintln(w, "error:", err)
			continue
		}
		if err := printQuotaSummary(w, summary, loc); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("quota unavailable for %d of %d accounts", failed, len(accounts))
	}
	return nil
}

// printQuotaSummary lists every group and window in the upstream's order,
// including groups for models that this bridge does not serve.
func printQuotaSummary(w io.Writer, summary ag.QuotaSummary, loc *time.Location) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "GROUP\tWINDOW\tREMAINING\tRESETS")
	for _, group := range summary.Groups {
		for _, bucket := range group.Buckets {
			remaining, reset, full := "-", "-", false
			if f := bucket.RemainingFraction; f != nil {
				remaining, full = percent(*f), *f == 1
			}
			// An untouched window reports now+window, which moves with every query.
			if t, err := time.Parse(time.RFC3339, bucket.ResetTime); err == nil && !full {
				reset = t.In(loc).Format("2006-01-02 15:04 MST")
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", group.DisplayName, bucket.Window, remaining, reset)
		}
	}
	return tw.Flush()
}

// percent keeps 0.00% and 100.00% for the upstream's exact zero and one, so
// rounding never shows a partly used window as exhausted or untouched.
func percent(fraction float64) string {
	switch {
	case fraction > 0 && fraction < 0.0001:
		return "<0.01%"
	case fraction > 0.9999 && fraction < 1:
		return ">99.99%"
	}
	return fmt.Sprintf("%.2f%%", fraction*100)
}
