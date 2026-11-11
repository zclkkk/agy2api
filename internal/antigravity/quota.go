package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// QuotaSummary describes the shared limits shown by Antigravity's /usage.
// Model catalog quotaInfo does not identify the separate quota windows.
type QuotaSummary struct {
	Groups []QuotaGroup `json:"groups"`
}

type QuotaGroup struct {
	DisplayName string        `json:"displayName"`
	Buckets     []QuotaBucket `json:"buckets"`
}

type QuotaBucket struct {
	ID          string `json:"bucketId"`
	DisplayName string `json:"displayName"`
	Window      string `json:"window"`
	// The upstream uses a oneof: a fraction or an absolute amount. Pointers
	// preserve explicit zero separately from an absent value. ProtoJSON int64
	// amounts are strings; json.Number also accepts numeric JSON losslessly.
	RemainingFraction *float64     `json:"remainingFraction"`
	RemainingAmount   *json.Number `json:"remainingAmount"`
	Disabled          bool         `json:"disabled"`
	ResetTime         string       `json:"resetTime"`
}

func (c *Client) QuotaSummary(ctx context.Context) (QuotaSummary, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var summary QuotaSummary
	r, err := c.post(ctx, "retrieveUserQuotaSummary", map[string]string{"project": c.Auth.ProjectID()})
	if err != nil {
		return summary, err
	}
	if err := readJSON(r, &summary); err != nil {
		return QuotaSummary{}, err
	}
	for _, group := range summary.Groups {
		if len(group.Buckets) > 0 {
			return summary, nil
		}
	}
	return summary, errors.New("Antigravity returned no quota buckets")
}
