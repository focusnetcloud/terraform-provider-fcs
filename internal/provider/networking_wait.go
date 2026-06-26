package provider

// Poll-to-gone helpers for the Phase-B networking resources
// (fcs_environment_ingress / fcs_environment_egress).
//
// Unlike clusters/VMs there is NO poll-to-ready: a live ingress/egress record
// is always status="provisioning" (the row is persisted immediately; the
// NAT-GW reconcile runs asynchronously and the contract defines no "active"
// terminal state — Ingress/Egress.status enum is provisioning|offboarding|
// destroyed). Create therefore persists the POST response directly. Destroy
// polls GET until the API returns 404 (or status=destroyed) so the dependent
// destroys (the cluster, then the environment) only start once the NAT-GW rule
// is really gone.

import (
	"context"
	"fmt"
	"time"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

// waitForIngressGone polls GET .../ingress/{id} after DELETE until the API
// returns 404 or status=destroyed.
func waitForIngressGone(ctx context.Context, c *client.Client, envID, ingressID string, interval, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := "unknown"
	for {
		ing, err := c.GetIngress(ctx, envID, ingressID)
		if err != nil {
			if client.IsNotFound(err) {
				return nil
			}
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out after %s waiting for ingress %s teardown (last status %q)", timeout, ingressID, lastStatus)
			}
			return err
		}
		if ing.Status == "destroyed" {
			return nil
		}
		lastStatus = ing.Status

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out after %s waiting for ingress %s teardown (last status %q)", timeout, ingressID, lastStatus)
			}
			return ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

// waitForEgressGone polls GET .../egress/{id} after DELETE until the API
// returns 404 or status=destroyed.
func waitForEgressGone(ctx context.Context, c *client.Client, envID, egressID string, interval, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := "unknown"
	for {
		eg, err := c.GetEgress(ctx, envID, egressID)
		if err != nil {
			if client.IsNotFound(err) {
				return nil
			}
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out after %s waiting for egress %s teardown (last status %q)", timeout, egressID, lastStatus)
			}
			return err
		}
		if eg.Status == "destroyed" {
			return nil
		}
		lastStatus = eg.Status

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out after %s waiting for egress %s teardown (last status %q)", timeout, egressID, lastStatus)
			}
			return ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}
