package provider

// Poll-to-ready / poll-to-gone helpers for the cluster resources. Create is
// async: 202 + status=provisioning; the client polls GET until
// status=active|error; teardown polls until 404/destroyed so terraform's
// dependency ordering stays correct on environment destroy).

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

// defaultPollInterval is the base GET interval; each wait applies jitter.
const defaultPollInterval = 5 * time.Second

// jitter spreads the base interval by ±20% so many parallel resources
// (for_each over attendees) do not poll in lockstep.
func jitter(interval time.Duration) time.Duration {
	return time.Duration(float64(interval) * (0.8 + 0.4*rand.Float64()))
}

// waitForClusterActive polls GET .../clusters/{id} until status=active.
// It returns an error when the cluster enters status=error (or another
// terminal teardown status), the timeout elapses, or ctx is cancelled.
// The last seen cluster is returned alongside the error when available.
func waitForClusterActive(ctx context.Context, c *client.Client, envID, clusterID string, interval, timeout time.Duration) (*client.Cluster, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := "unknown"
	lastDiagnostics := ""
	for {
		cl, err := c.GetCluster(ctx, envID, clusterID)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return nil, timeoutErr(timeout, clusterID, lastStatus, lastDiagnostics)
			}
			return nil, err
		}
		switch cl.Status {
		case "active":
			return cl, nil
		case "error":
			return cl, fmt.Errorf("cluster %s entered status \"error\" during provisioning%s; "+
				"inspect the environment via the FCS API and retry or contact support",
				clusterID, diagnosticSuffix(cl.ProvisioningDiagnostics))
		case "offboarding", "destroyed":
			return cl, fmt.Errorf("cluster %s unexpectedly entered teardown status %q while waiting for active", clusterID, cl.Status)
		}
		lastStatus = cl.Status
		lastDiagnostics = cl.ProvisioningDiagnostics

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return nil, timeoutErr(timeout, clusterID, lastStatus, lastDiagnostics)
			}
			return nil, ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

// waitForClusterGone polls GET .../clusters/{id} after DELETE until the API
// returns 404 or status=destroyed.
func waitForClusterGone(ctx context.Context, c *client.Client, envID, clusterID string, interval, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := "unknown"
	for {
		cl, err := c.GetCluster(ctx, envID, clusterID)
		if err != nil {
			if client.IsNotFound(err) {
				return nil
			}
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out after %s waiting for cluster %s teardown (last status %q)", timeout, clusterID, lastStatus)
			}
			return err
		}
		if cl.Status == "destroyed" {
			return nil
		}
		lastStatus = cl.Status

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out after %s waiting for cluster %s teardown (last status %q)", timeout, clusterID, lastStatus)
			}
			return ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

func timeoutErr(timeout time.Duration, clusterID, lastStatus, diagnostics string) error {
	return fmt.Errorf("timed out after %s waiting for cluster %s to become active (last status %q); "+
		"the cluster keeps provisioning server-side — re-run terraform apply or raise the create timeout%s",
		timeout, clusterID, lastStatus, diagnosticSuffix(diagnostics))
}

func diagnosticSuffix(diagnostics string) string {
	if diagnostics == "" {
		return ""
	}
	return fmt.Sprintf("; provisioning diagnostics: %s", diagnostics)
}
