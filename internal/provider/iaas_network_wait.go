package provider

// Poll-to-ready / poll-to-gone helpers for fcs_iaas_network. Network
// realization can include external fabric, Harvester and KubeOVN work, so a
// POST response is only intent persistence; Terraform must wait for active.

import (
	"context"
	"fmt"
	"time"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

func waitForIaasNetworkActive(ctx context.Context, c *client.Client, envID, vdcID, networkID string, interval, timeout time.Duration) (*client.IaasNetwork, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := "unknown"
	for {
		network, err := c.GetIaasNetwork(ctx, envID, vdcID, networkID)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return nil, iaasNetworkTimeoutErr(timeout, networkID, lastStatus)
			}
			return nil, err
		}
		switch network.Status {
		case "active":
			return network, nil
		case "error":
			return network, fmt.Errorf("iaas network %s entered status \"error\" during provisioning; "+
				"inspect the environment via the FCS API for gateway, Harvester and fabric diagnostics", networkID)
		case "destroying", "destroyed":
			return network, fmt.Errorf("iaas network %s unexpectedly entered teardown status %q while waiting for active", networkID, network.Status)
		}
		lastStatus = network.Status

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return nil, iaasNetworkTimeoutErr(timeout, networkID, lastStatus)
			}
			return nil, ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

func waitForIaasNetworkGone(ctx context.Context, c *client.Client, envID, vdcID, networkID string, interval, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := "unknown"
	for {
		network, err := c.GetIaasNetwork(ctx, envID, vdcID, networkID)
		if err != nil {
			if client.IsNotFound(err) {
				return nil
			}
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out after %s waiting for iaas network %s teardown (last status %q)", timeout, networkID, lastStatus)
			}
			return err
		}
		if network.Status == "destroyed" {
			return nil
		}
		if network.Status == "error" {
			return fmt.Errorf("iaas network %s entered status \"error\" during teardown; "+
				"inspect the environment via the FCS API for gateway, Harvester and fabric cleanup diagnostics", networkID)
		}
		lastStatus = network.Status

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out after %s waiting for iaas network %s teardown (last status %q)", timeout, networkID, lastStatus)
			}
			return ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

func iaasNetworkTimeoutErr(timeout time.Duration, networkID, lastStatus string) error {
	return fmt.Errorf("timed out after %s waiting for iaas network %s to become active (last status %q); "+
		"the network keeps provisioning server-side - re-run terraform apply or raise the create timeout",
		timeout, networkID, lastStatus)
}
