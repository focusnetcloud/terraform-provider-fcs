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
	var lastPollErr error
	for {
		cl, err := c.GetCluster(ctx, envID, clusterID)
		if err != nil {
			if ctx.Err() != nil {
				if ctx.Err() == context.DeadlineExceeded {
					return nil, timeoutErr(timeout, clusterID, lastStatus, lastDiagnostics, err)
				}
				return nil, ctx.Err()
			}
			if !client.IsRetryableReadError(err) {
				return nil, err
			}
			lastPollErr = err
		} else {
			lastPollErr = nil
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
		}

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return nil, timeoutErr(timeout, clusterID, lastStatus, lastDiagnostics, lastPollErr)
			}
			return nil, ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

// waitForClusterResized waits for both status=active and the exact requested
// size. Checking only lifecycle state is unsafe because an old active response
// can race the asynchronous resize reconciler.
func waitForClusterResized(ctx context.Context, c *client.Client, envID, clusterID string, expected client.ClusterSizing, dedicated bool, interval, timeout time.Duration) (*client.Cluster, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := "unknown"
	var last *client.Cluster
	var lastPollErr error
	for {
		cl, err := c.GetCluster(ctx, envID, clusterID)
		if err != nil {
			if ctx.Err() != nil {
				if ctx.Err() == context.DeadlineExceeded {
					return last, resizeTimeoutErr(timeout, clusterID, lastStatus, lastPollErr)
				}
				return last, ctx.Err()
			}
			if !client.IsRetryableReadError(err) {
				return last, err
			}
			lastPollErr = err
		} else {
			last = cl
			lastPollErr = nil
			lastStatus = cl.Status
			switch cl.Status {
			case "active":
				if clusterMatchesResize(cl, expected, dedicated) {
					return cl, nil
				}
			case "error":
				return cl, fmt.Errorf("cluster %s entered status \"error\" during resize%s", clusterID, diagnosticSuffix(cl.ProvisioningDiagnostics))
			case "offboarding", "destroyed":
				return cl, fmt.Errorf("cluster %s unexpectedly entered teardown status %q during resize", clusterID, cl.Status)
			}
		}

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return last, resizeTimeoutErr(timeout, clusterID, lastStatus, lastPollErr)
			}
			return last, ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

func clusterMatchesResize(cl *client.Cluster, expected client.ClusterSizing, dedicated bool) bool {
	if dedicated {
		return cl.CPNodes == expected.CPNodes &&
			cl.CPVcpu == expected.CPVcpu &&
			cl.CPRamGB == expected.CPRamGB &&
			cl.WorkerNodes == expected.WorkerNodes &&
			cl.WorkerVcpu == expected.WorkerVcpu &&
			cl.WorkerRamGB == expected.WorkerRamGB &&
			cl.PVCStorageGB == expected.PVCStorageGB
	}
	return cl.VCPU == expected.VCPU && cl.RAMGB == expected.RAMGB && cl.StorageGB == expected.StorageGB
}

// waitForClusterGone polls GET .../clusters/{id} after DELETE until the API
// returns 404 or status=destroyed.
func waitForClusterGone(ctx context.Context, c *client.Client, envID, clusterID string, interval, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := "unknown"
	var lastPollErr error
	for {
		cl, err := c.GetCluster(ctx, envID, clusterID)
		if err != nil {
			if client.IsNotFound(err) {
				return nil
			}
			if ctx.Err() != nil {
				if ctx.Err() == context.DeadlineExceeded {
					return teardownTimeoutErr(timeout, clusterID, lastStatus, err)
				}
				return ctx.Err()
			}
			if !client.IsRetryableReadError(err) {
				return err
			}
			lastPollErr = err
		} else {
			lastPollErr = nil
			if cl.Status == "destroyed" {
				return nil
			}
			lastStatus = cl.Status
		}

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return teardownTimeoutErr(timeout, clusterID, lastStatus, lastPollErr)
			}
			return ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

func timeoutErr(timeout time.Duration, clusterID, lastStatus, diagnostics string, lastPollErr error) error {
	return fmt.Errorf("timed out after %s waiting for cluster %s to become active (last status %q%s); "+
		"the cluster may keep provisioning server-side. Verify it through the FCS API before retrying. "+
		"If Terraform/OpenTofu marked the tracked resource as tainted but the cluster is healthy, "+
		"use untaint instead of applying a replacement; if it is absent from state, import it as "+
		"<environment_id>/<cluster_id>. You can also raise the create timeout%s",
		timeout, clusterID, lastStatus, pollErrorSuffix(lastPollErr), diagnosticSuffix(diagnostics))
}

func teardownTimeoutErr(timeout time.Duration, clusterID, lastStatus string, lastPollErr error) error {
	return fmt.Errorf("timed out after %s waiting for cluster %s teardown (last status %q%s)",
		timeout, clusterID, lastStatus, pollErrorSuffix(lastPollErr))
}

func resizeTimeoutErr(timeout time.Duration, clusterID, lastStatus string, lastPollErr error) error {
	return fmt.Errorf("timed out after %s waiting for cluster %s to reach the requested size (last status %q%s); "+
		"the same cluster may keep resizing server-side. Re-run apply after checking its status; do not replace it",
		timeout, clusterID, lastStatus, pollErrorSuffix(lastPollErr))
}

func pollErrorSuffix(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf(", last transient GET error: %s", err)
}

func diagnosticSuffix(diagnostics string) string {
	if diagnostics == "" {
		return ""
	}
	return fmt.Sprintf("; provisioning diagnostics: %s", diagnostics)
}
