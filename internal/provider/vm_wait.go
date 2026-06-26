package provider

// Poll-to-ready / poll-to-gone helpers for fcs_vm. Create is async:
// 202 + status=provisioning; the provider polls GET until the VM
// reaches its desired power state; teardown polls until the VM is gone).

import (
	"context"
	"fmt"
	"time"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

// waitForVmStatus polls GET .../vms/{id} until the VM reaches the target
// status ("active" for running VMs, "stopped" for running=false). It
// returns an error when the VM enters status=error or destroyed, the
// timeout elapses, or ctx is cancelled. The last seen VM is returned
// alongside the error when available.
func waitForVmStatus(ctx context.Context, c *client.Client, envID, vmID, target string, interval, timeout time.Duration) (*client.Vm, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := "unknown"
	for {
		vm, err := c.GetVm(ctx, envID, vmID)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return nil, vmTimeoutErr(timeout, vmID, target, lastStatus)
			}
			return nil, err
		}
		switch vm.Status {
		case target:
			return vm, nil
		case "error":
			return vm, fmt.Errorf("vm %s entered status \"error\" during provisioning; "+
				"inspect GET .../vms/%s/status for the reason code (e.g. ImagePullFailed, "+
				"IPPoolExhausted) and retry or contact support", vmID, vmID)
		case "destroyed":
			return vm, fmt.Errorf("vm %s was unexpectedly destroyed while waiting for status %q "+
				"(TTL reaper or out-of-band teardown)", vmID, target)
		}
		lastStatus = vm.Status

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return nil, vmTimeoutErr(timeout, vmID, target, lastStatus)
			}
			return nil, ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

// waitForVmGone polls GET .../vms/{id} after DELETE until the API returns
// 404 or status=destroyed (the server keeps destroyed rows readable).
func waitForVmGone(ctx context.Context, c *client.Client, envID, vmID string, interval, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	lastStatus := "unknown"
	for {
		vm, err := c.GetVm(ctx, envID, vmID)
		if err != nil {
			if client.IsNotFound(err) {
				return nil
			}
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out after %s waiting for vm %s teardown (last status %q)", timeout, vmID, lastStatus)
			}
			return err
		}
		if vm.Status == "destroyed" {
			return nil
		}
		lastStatus = vm.Status

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Errorf("timed out after %s waiting for vm %s teardown (last status %q)", timeout, vmID, lastStatus)
			}
			return ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

func vmTimeoutErr(timeout time.Duration, vmID, target, lastStatus string) error {
	return fmt.Errorf("timed out after %s waiting for vm %s to become %q (last status %q); "+
		"the vm keeps provisioning server-side — re-run terraform apply or raise the create timeout",
		timeout, vmID, target, lastStatus)
}
