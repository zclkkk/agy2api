package antigravity

import (
	"context"
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
	Window string `json:"window"`
	// RemainingFraction is nil when the upstream gives no value, which is not
	// the same as an explicit zero.
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
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
