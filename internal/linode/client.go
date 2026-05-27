/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package linode provides a thin wrapper around the linodego SDK that exposes
// only the account-maintenance endpoint used by this controller.
package linode

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/linode/linodego"
	"golang.org/x/oauth2"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Maintenance is the subset of linodego.AccountMaintenance that this controller
// needs to make scheduling decisions.
type Maintenance struct {
	// LinodeID is the numeric ID of the Linode instance.
	LinodeID int64

	// Label is the human-readable label of the Linode instance.
	Label string

	// EntityURL is the API URL of the entity (informational).
	EntityURL string

	// MaintenanceType is the type of maintenance (e.g. "reboot").
	MaintenanceType string

	// ScheduledAt is the time the maintenance window begins.
	// NotBefore is preferred when available; When is used as a fallback.
	ScheduledAt time.Time
}

// hostOverrideTransport rewrites the scheme and host of every outbound request
// so that Linode API calls are routed to a non-default endpoint (e.g.
// api.devcloud.linode.com) without touching linodego's internal resty state.
type hostOverrideTransport struct {
	base   http.RoundTripper
	scheme string
	host   string
}

func (t *hostOverrideTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL.Scheme = t.scheme
	r.URL.Host = t.host
	r.Host = t.host
	return t.base.RoundTrip(r)
}

// Client wraps a linodego.Client and exposes maintenance-related operations.
type Client struct {
	inner   linodego.Client
	baseURL string // effective base URL used for logging
}

// NewClient creates a Linode API client authenticated with token.
// If baseURL is non-empty it overrides the default API endpoint
// (e.g. "https://api.devcloud.linode.com"). The override is applied at the
// HTTP transport layer to avoid calling linodego's SetBaseURL on a value copy.
func NewClient(token, baseURL string) *Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	var transport http.RoundTripper = &oauth2.Transport{Source: ts}

	effective := "https://api.linode.com"
	if baseURL != "" {
		if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
			transport = &hostOverrideTransport{
				base:   transport,
				scheme: u.Scheme,
				host:   u.Host,
			}
			effective = baseURL
		}
	}

	c := linodego.NewClient(&http.Client{Transport: transport})
	return &Client{inner: c, baseURL: effective}
}

// ListMaintenances fetches all upcoming account maintenances and returns only
// those belonging to Linode instances (entity.type == "linode") that have a
// usable scheduled time.
func (c *Client) ListMaintenances(ctx context.Context) ([]Maintenance, error) {
	log := logf.FromContext(ctx).WithName("linode-client")
	apiURL := c.baseURL + "/v4/account/maintenance"
	log.Info("Calling Linode API", "url", apiURL)

	raw, err := c.inner.ListMaintenances(ctx, nil)
	if err != nil {
		log.Error(err, "Linode API call failed", "url", apiURL)
		return nil, fmt.Errorf("listing Linode maintenances: %w", err)
	}
	log.Info("Linode API call succeeded", "url", apiURL, "rawCount", len(raw))

	result := make([]Maintenance, 0, len(raw))
	for _, m := range raw {
		if m.Entity == nil || m.Entity.Type != "linode" {
			continue
		}

		// Skip maintenances that have already completed.
		// The Linode API continues returning them with complete_time set after they finish,
		// which would otherwise keep nodes tagged indefinitely via ScheduledAt.Before(now).
		if m.CompleteTime != nil && m.CompleteTime.Before(time.Now()) {
			continue
		}

		// Prefer NotBefore (authoritative window start), fall back to the
		// deprecated When field which some older API responses still populate.
		var scheduledAt time.Time
		switch {
		case m.NotBefore != nil:
			scheduledAt = *m.NotBefore
		case m.When != nil: //nolint:staticcheck // intentional fallback for older API responses
			scheduledAt = *m.When //nolint:staticcheck // intentional fallback for older API responses
		default:
			// No usable time; skip.
			continue
		}

		result = append(result, Maintenance{
			LinodeID:        int64(m.Entity.ID),
			Label:           m.Entity.Label,
			EntityURL:       m.Entity.URL,
			MaintenanceType: m.Type,
			ScheduledAt:     scheduledAt,
		})
	}

	return result, nil
}
