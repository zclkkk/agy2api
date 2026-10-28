package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	ag "github.com/zclkkk/agy2api/internal/antigravity"
)

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
