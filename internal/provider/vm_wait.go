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
	var lastPollErr error
	for {
		vm, err := c.GetVm(ctx, envID, vmID)
		if err != nil {
			if ctx.Err() != nil {
				if ctx.Err() == context.DeadlineExceeded {
					timeoutErr := lastPollErr
					if timeoutErr == nil {
						timeoutErr = err
					}
					return nil, vmTimeoutErr(timeout, vmID, target, lastStatus, timeoutErr)
				}
				return nil, ctx.Err()
			}
			if !client.IsRetryableReadError(err) {
				return nil, err
			}
			lastPollErr = err
		} else {
			lastPollErr = nil
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
		}

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return nil, vmTimeoutErr(timeout, vmID, target, lastStatus, lastPollErr)
			}
			return nil, ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

// waitForVmPowerState settles an accepted asynchronous power operation
// against the live KubeVirt status endpoint. The ordinary VM read mirrors the
// desired DB state immediately and is therefore insufficient for stop/start.
func waitForVmPowerState(
	ctx context.Context,
	c *client.Client,
	envID, vmID string,
	running bool,
	interval, timeout time.Duration,
) (*client.VmStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	target := "Stopped"
	if running {
		target = "Running"
	}
	lastPhase := "unknown"
	var lastPollErr error
	initialObserved := false
	initialFailure := false
	leftInitialFailure := false
	for {
		status, err := c.GetVmStatus(ctx, envID, vmID)
		if err != nil {
			if ctx.Err() != nil {
				if ctx.Err() == context.DeadlineExceeded {
					timeoutErr := lastPollErr
					if timeoutErr == nil {
						timeoutErr = err
					}
					return nil, vmPowerTimeoutErr(
						timeout, vmID, target, lastPhase, timeoutErr,
					)
				}
				return nil, ctx.Err()
			}
			if !client.IsRetryableReadError(err) {
				return nil, err
			}
			lastPollErr = err
		} else {
			lastPollErr = nil
			lastPhase = status.Phase
			failed := status.PlatformError || status.Phase == "Failed"
			if !initialObserved {
				initialObserved = true
				initialFailure = failed
			}
			if status.Phase == target {
				return status, nil
			}
			// A failure already present on the first observation may be the
			// state this power operation is meant to remediate. Keep polling
			// until it clears, reaches the target, or the update times out.
			if initialFailure && !failed {
				leftInitialFailure = true
			}
			if failed && (!initialFailure || leftInitialFailure) {
				reason := "platform_error"
				if status.Reason != nil && *status.Reason != "" {
					reason = *status.Reason
				}
				return status, fmt.Errorf(
					"vm %s entered live phase %q while waiting for %q "+
						"(reason %q: %s)",
					vmID,
					status.Phase,
					target,
					reason,
					status.Message,
				)
			}
		}

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return nil, vmPowerTimeoutErr(
					timeout, vmID, target, lastPhase, lastPollErr,
				)
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
	var lastPollErr error
	for {
		vm, err := c.GetVm(ctx, envID, vmID)
		if err != nil {
			if client.IsNotFound(err) {
				return nil
			}
			if ctx.Err() != nil {
				if ctx.Err() == context.DeadlineExceeded {
					timeoutErr := lastPollErr
					if timeoutErr == nil {
						timeoutErr = err
					}
					return vmTeardownTimeoutErr(timeout, vmID, lastStatus, timeoutErr)
				}
				return ctx.Err()
			}
			if !client.IsRetryableReadError(err) {
				return err
			}
			lastPollErr = err
		} else {
			lastPollErr = nil
			if vm.Status == "destroyed" {
				return nil
			}
			lastStatus = vm.Status
		}

		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return vmTeardownTimeoutErr(timeout, vmID, lastStatus, lastPollErr)
			}
			return ctx.Err()
		case <-time.After(jitter(interval)):
		}
	}
}

func vmTimeoutErr(timeout time.Duration, vmID, target, lastStatus string, lastPollErr error) error {
	return fmt.Errorf("timed out after %s waiting for vm %s to become %q (last status %q%s); "+
		"the VM may keep provisioning server-side. Verify it through the FCS API before retrying. "+
		"If Terraform tracks it as tainted and it is healthy, run terraform untaint and re-apply "+
		"instead of replacing it. Only if it is absent from state, import it as "+
		"<environment_id>/<vm_id>. You can also raise the create timeout",
		timeout, vmID, target, lastStatus, pollErrorSuffix(lastPollErr))
}

func vmTeardownTimeoutErr(timeout time.Duration, vmID, lastStatus string, lastPollErr error) error {
	return fmt.Errorf("timed out after %s waiting for vm %s teardown (last status %q%s)",
		timeout, vmID, lastStatus, pollErrorSuffix(lastPollErr))
}

func vmPowerTimeoutErr(
	timeout time.Duration,
	vmID, target, lastPhase string,
	lastPollErr error,
) error {
	return fmt.Errorf(
		"timed out after %s waiting for vm %s power state %q "+
			"(last live phase %q%s)",
		timeout,
		vmID,
		target,
		lastPhase,
		pollErrorSuffix(lastPollErr),
	)
}
