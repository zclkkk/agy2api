package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
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
		if err := printQuotaSummary(w, summary, loc, time.Now()); err != nil {
			return err
		}
	}
	if failed > 0 {
		return fmt.Errorf("quota unavailable for %d of %d accounts", failed, len(accounts))
	}
	return nil
}

// printQuotaSummary preserves the upstream group and window order, including
// groups for models that this bridge does not serve.
func printQuotaSummary(w io.Writer, summary ag.QuotaSummary, loc *time.Location, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "GROUP\tWINDOW\tREMAINING\tSTATUS\tRESETS")
	for _, group := range summary.Groups {
		name := group.DisplayName
		if name == "" {
			name = "-"
		}
		for _, bucket := range group.Buckets {
			window := bucket.Window
			if window == "" {
				window = bucket.DisplayName
			}
			if window == "" {
				window = bucket.ID
			}
			if window == "" {
				window = "-"
			}
			remaining, status, reset := quotaDetails(bucket, loc, now)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, window, remaining, status, reset)
		}
	}
	return tw.Flush()
}

func quotaDetails(bucket ag.QuotaBucket, loc *time.Location, now time.Time) (remaining, status, reset string) {
	if bucket.Disabled {
		return "-", "disabled", "-"
	}
	remaining, status, reset = "-", "unknown", "-"
	switch {
	case bucket.RemainingFraction != nil && bucket.RemainingAmount == nil:
		fraction := *bucket.RemainingFraction
		if math.IsNaN(fraction) || math.IsInf(fraction, 0) || fraction < 0 || fraction > 1 {
			return
		}
		remaining, status = fmt.Sprintf("%.2f%%", fraction*100), "available"
		switch {
		case fraction == 1:
			// Full buckets may return a moving now+window reset time. Like the
			// official panel, show availability rather than a pending reset.
			return
		case fraction == 0:
			status = "exhausted"
		case fraction < 0.0001:
			remaining = "<0.01%"
		case fraction > 0.9999:
			remaining = ">99.99%"
		}
	case bucket.RemainingAmount != nil && bucket.RemainingFraction == nil:
		amount, err := bucket.RemainingAmount.Int64()
		if err != nil || amount < 0 {
			return
		}
		remaining, status = strconv.FormatInt(amount, 10), "available"
		if amount == 0 {
			status = "exhausted"
		}
	default:
		return
	}
	if t, err := time.Parse(time.RFC3339, bucket.ResetTime); err == nil {
		reset = t.In(loc).Format("2006-01-02 15:04 MST")
		if !t.After(now) {
			// Do not invent a fresh quota when the upstream reset has passed.
			status = "reset due"
		}
	}
	return
}
