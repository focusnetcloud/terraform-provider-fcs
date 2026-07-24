package provider

// fcs_vm: a standalone VM in the persistent tenant network, brokered by the
// FCS API. Maps onto /v1/environments/{id}/vms.
//
// Every spec attribute is immutable on the API (RequiresReplace) except
// `running`, the desired power state: toggling it is an in-place update via
// POST .../vms/{vmId}/power (stop|start).

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

// defaultVmCreateTimeout covers POST + poll-to-ready (status=active for
// running VMs, stopped for running=false): image import + first boot.
const defaultVmCreateTimeout = 15 * time.Minute
const defaultVmUpdateTimeout = 5 * time.Minute

var (
	_ resource.Resource                   = (*vmResource)(nil)
	_ resource.ResourceWithConfigure      = (*vmResource)(nil)
	_ resource.ResourceWithImportState    = (*vmResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*vmResource)(nil)
	_ resource.ResourceWithValidateConfig = (*vmResource)(nil)
)

// NewVmResource returns the fcs_vm resource.
func NewVmResource() resource.Resource {
	return &vmResource{pollInterval: defaultPollInterval}
}

type vmResource struct {
	client       *client.Client
	pollInterval time.Duration // overridable in tests
}

func (r *vmResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vm"
}

func (r *vmResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A standalone VM in the persistent tenant network, brokered by the FCS " +
			"API. Create is asynchronous: the provider polls " +
			"until the VM reaches its desired power state. All attributes except `running` " +
			"force replacement. After a create timeout, verify the VM through the API: " +
			"untaint a healthy tracked VM and import only when it is absent from state. " +
			"Because the API never returns cloud-init payloads, imported VMs that originally " +
			"used cloud-init must ignore changes to those sensitive attributes.",
		Attributes: map[string]schema.Attribute{
			"environment_id": schema.StringAttribute{
				Required: true,
				Description: "ID of the fcs_environment hosting this VM. " +
					"Changing it forces a new VM.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"image": schema.StringAttribute{
				Optional: true,
				Description: "Catalog image name (see the fcs_images data source). " +
					"Exactly one of image or harbor_artifact_id must be set. Changing it " +
					"forces a new VM.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"harbor_artifact_id": schema.StringAttribute{
				Optional: true,
				Description: "ID of an active fcs_harbor_artifact with kind vm_disk. The API " +
					"imports its digest-pinned OCI artifact through CDI. Exactly one of image " +
					"or harbor_artifact_id must be set. Changing it forces a new VM.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(uuidPattern, "must be a canonical UUID"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "VM name; idempotency key per environment. Server-generated when " +
					"unset. Changing it forces a new VM.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 63),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cpu_cores": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(2),
				Description: "vCPU cores (default 2). Changing it forces a new VM.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"memory_gb": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(4),
				Description: "Guest memory in GiB (default 4). Changing it forces a new VM.",
				Validators: []validator.Int64{
					int64validator.AtLeast(1),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"disk_gb": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(20),
				Description: "Root disk size in GiB (default 20). Changing it forces a new VM.",
				Validators: []validator.Int64{
					int64validator.AtLeast(10),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"nic_network": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("tenant"),
				Description: "Legacy tenant network attachment (default \"tenant\"); the server " +
					"picks NAD and IP. For IaaS-vDC networks set vdc_id/network_id and " +
					"either set nic_network to \"iaas\" or \"iaas-vdc\", or leave the " +
					"default \"tenant\" for backward-compatible API payloads. " +
					"Changing it forces a new VM.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"vdc_id": schema.StringAttribute{
				Optional: true,
				Description: "Optional IaaS-vDC ID for standalone IaaS VM network binding. " +
					"Must be set together with network_id. Changing it forces a new VM.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"network_id": schema.StringAttribute{
				Optional: true,
				Description: "Optional IaaS-vDC network ID for standalone IaaS VM network " +
					"binding. The network must be active server-side. Must be set together " +
					"with vdc_id. Changing it forces a new VM.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cloud_init_userdata": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "cloud-init user-data (sensitive). Changing it forces a new VM.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cloud_init_networkdata": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "cloud-init network-data (sensitive). Changing it forces a new VM.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"running": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				Description: "Desired power state (default true). The only in-place updatable " +
					"attribute: toggling it calls the power endpoint (stop/start).",
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Server-assigned VM ID (UUID).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Lifecycle status: provisioning | active | stopped | error | destroyed.",
			},
			"vm_ip": schema.StringAttribute{
				Computed:    true,
				Description: "IP of the VM in the tenant network (server-allocated, known once active).",
			},
			"console_url": schema.StringAttribute{
				Computed:    true,
				Sensitive:   true,
				Description: "Time-limited signed console URL (null until the console broker exists).",
			},
			"timeouts": timeouts.Attributes(ctx, timeouts.Opts{
				Create: true,
				CreateDescription: fmt.Sprintf("How long to wait for the VM to reach its desired power state "+
					"(default %s). Accepts a duration string such as \"30m\".", defaultVmCreateTimeout),
				Update: true,
				UpdateDescription: fmt.Sprintf("How long to wait for an in-place stop/start to reach its "+
					"observed KubeVirt power state (default %s). If an update times out, verify the live "+
					"VM status before retrying because the accepted operation may still converge.",
					defaultVmUpdateTimeout),
				Delete: true,
				DeleteDescription: fmt.Sprintf("How long to wait for the teardown to finish (GET returns 404 "+
					"or status=destroyed; default %s).", defaultDeleteTimeout),
			}),
		},
	}
}

// ValidateConfig mirrors the API's exact-one source invariant at plan time.
// Unknown values are deferred because Terraform may resolve them from another
// resource during apply.
func (r *vmResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var image, harborArtifactID types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("image"), &image)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("harbor_artifact_id"), &harborArtifactID)...)
	if resp.Diagnostics.HasError() || image.IsUnknown() || harborArtifactID.IsUnknown() {
		return
	}
	imageSet := !image.IsNull()
	artifactSet := !harborArtifactID.IsNull()
	if imageSet == artifactSet {
		resp.Diagnostics.AddAttributeError(
			path.Root("image"),
			"Invalid VM image source",
			"Exactly one of image or harbor_artifact_id must be configured.",
		)
	}
}

func (r *vmResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return // provider not configured yet (e.g. during validate)
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected resource configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	r.client = c
}

// ModifyPlan marks status and vm_ip unknown when the desired power state
// changes: the in-place power update flips them server-side, so carrying
// the prior state values into the plan would make the apply result
// inconsistent with the plan.
func (r *vmResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // create or destroy: nothing to adjust
	}
	var planRunning, stateRunning types.Bool
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("running"), &planRunning)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("running"), &stateRunning)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !planRunning.IsUnknown() && planRunning.Equal(stateRunning) {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("vm_ip"), types.StringUnknown())...)
}

func (r *vmResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var envID string
	var name, image, harborArtifactID, nicNetwork, vdcID, networkID, userdata, networkdata types.String
	var cpuCores, memoryGB, diskGB types.Int64
	var running types.Bool
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("name"), &name)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("image"), &image)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("harbor_artifact_id"), &harborArtifactID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("cpu_cores"), &cpuCores)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("memory_gb"), &memoryGB)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("disk_gb"), &diskGB)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("nic_network"), &nicNetwork)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("vdc_id"), &vdcID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("network_id"), &networkID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("cloud_init_userdata"), &userdata)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("cloud_init_networkdata"), &networkdata)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("running"), &running)...)

	var timeoutsVal timeouts.Value
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("timeouts"), &timeoutsVal)...)
	if resp.Diagnostics.HasError() {
		return
	}
	vdcUnset := vdcID.IsNull() || vdcID.IsUnknown()
	networkUnset := networkID.IsNull() || networkID.IsUnknown()
	if vdcUnset != networkUnset {
		resp.Diagnostics.AddError(
			"Invalid IaaS-vDC network binding",
			"vdc_id and network_id must be set together for fcs_vm IaaS network binding.",
		)
		return
	}
	createTimeout, diags := timeoutsVal.Create(ctx, defaultVmCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	runVal := running.ValueBool()
	spec := client.VmSpec{
		Image:                image.ValueString(),
		HarborArtifactID:     harborArtifactID.ValueString(),
		Name:                 name.ValueString(), // empty: server generates one
		CPUCores:             cpuCores.ValueInt64(),
		MemoryGB:             memoryGB.ValueInt64(),
		DiskGB:               diskGB.ValueInt64(),
		NICNetwork:           nicNetwork.ValueString(),
		CloudInitUserdata:    userdata.ValueString(),
		CloudInitNetworkdata: networkdata.ValueString(),
		Running:              &runVal,
		VdcID:                vdcID.ValueString(),
		NetworkID:            networkID.ValueString(),
	}

	// 202 (newly created) and 200 (idempotent re-POST over name) are both
	// success per contract.
	created, err := r.client.CreateVm(ctx, envID, spec)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not create fcs_vm",
			fmt.Sprintf("POST /v1/environments/%s/vms failed: %s", envID, err),
		)
		return
	}

	// Persist the VM in state before polling: if the wait below fails
	// (timeout, status=error, cancellation), Terraform still tracks the
	// resource (tainted) instead of orphaning it server-side.
	resp.State.Raw = req.Plan.Raw
	if name.IsUnknown() || name.IsNull() {
		// Server-generated name: adopt it as the computed value (null when
		// the server response omits the additive name field).
		nameVal := types.StringNull()
		if created.Name != "" {
			nameVal = types.StringValue(created.Name)
		}
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), nameVal)...)
	}
	setVmState(ctx, &resp.State, created, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	// Poll until the VM reaches its desired power state.
	target := "active"
	if !runVal {
		target = "stopped"
	}
	final, err := waitForVmStatus(ctx, r.client, envID, created.ID, target, r.pollInterval, createTimeout)
	if err != nil {
		if final != nil {
			setVmState(ctx, &resp.State, final, &resp.Diagnostics)
		}
		resp.Diagnostics.AddError(
			fmt.Sprintf("fcs_vm did not become %s", target),
			fmt.Sprintf("VM %s in environment %s: %s", created.ID, envID, err),
		)
		return
	}

	setVmState(ctx, &resp.State, final, &resp.Diagnostics)
}

func (r *vmResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var envID, id string
	var stateImage, stateHarborArtifactID types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("image"), &stateImage)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("harbor_artifact_id"), &stateHarborArtifactID)...)
	if resp.Diagnostics.HasError() {
		return
	}
	importRead := stateImage.IsNull() && stateHarborArtifactID.IsNull()

	vm, err := r.client.GetVm(ctx, envID, id)
	if err != nil {
		if client.IsNotFound(err) {
			// Drift-safe: gone server-side (TTL reaper or manual teardown).
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Could not read fcs_vm",
			fmt.Sprintf("GET /v1/environments/%s/vms/%s failed: %s", envID, id, err),
		)
		return
	}
	if vm.Status == "destroyed" {
		// The server keeps destroyed rows readable; treat them like a 404
		// so Terraform plans a re-create instead of drifting forever.
		resp.State.RemoveResource(ctx)
		return
	}

	if importRead && !vmHasSpecReadback(vm) {
		resp.Diagnostics.AddError(
			"FCS API does not support lossless VM import",
			"The VM exists, but this FCS API response does not include the non-secret VM "+
				"specification required to reconstruct Terraform state. Upgrade the FCS API "+
				"before importing this VM; the provider refuses to create a replacement-prone "+
				"partial state.",
		)
		return
	}

	if vmHasSpecReadback(vm) {
		setVmReadState(ctx, &resp.State, vm, &resp.Diagnostics)
		return
	}
	// Backward compatibility for already-managed resources against an older
	// API: refresh only observed fields and preserve the existing config.
	setVmState(ctx, &resp.State, vm, &resp.Diagnostics)
}

// Update handles the single mutable attribute `running` by calling the
// power endpoint (stop|start); everything else carries RequiresReplace.
func (r *vmResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var envID, id string
	var planRunning, stateRunning types.Bool
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("running"), &planRunning)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("running"), &stateRunning)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !planRunning.Equal(stateRunning) {
		var timeoutsVal timeouts.Value
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("timeouts"), &timeoutsVal)...)
		if resp.Diagnostics.HasError() {
			return
		}
		updateTimeout, diags := timeoutsVal.Update(ctx, defaultVmUpdateTimeout)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}

		action := "stop"
		if planRunning.ValueBool() {
			action = "start"
		}
		if err := r.client.PowerVm(ctx, envID, id, action); err != nil {
			resp.Diagnostics.AddError(
				"Could not update fcs_vm power state",
				fmt.Sprintf("POST /v1/environments/%s/vms/%s/power (action=%s) failed: %s", envID, id, action, err),
			)
			return
		}
		if _, err := waitForVmPowerState(
			ctx,
			r.client,
			envID,
			id,
			planRunning.ValueBool(),
			r.pollInterval,
			updateTimeout,
		); err != nil {
			resp.Diagnostics.AddError(
				"fcs_vm power update did not settle",
				fmt.Sprintf("VM %s in environment %s: %s", id, envID, err),
			)
			return
		}
	}

	// Refresh the computed contract fields after the power verb (the
	// server mirrors stop/start into the status synchronously).
	vm, err := r.client.GetVm(ctx, envID, id)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not read fcs_vm after power update",
			fmt.Sprintf("GET /v1/environments/%s/vms/%s failed: %s", envID, id, err),
		)
		return
	}

	resp.State.Raw = req.Plan.Raw
	setVmState(ctx, &resp.State, vm, &resp.Diagnostics)
}

func (r *vmResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var envID, id string
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)

	var timeoutsVal timeouts.Value
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("timeouts"), &timeoutsVal)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteTimeout, diags := timeoutsVal.Delete(ctx, defaultDeleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// 202 and 404 (already gone) both count as success; the client
	// tolerates the 404.
	if err := r.client.DeleteVm(ctx, envID, id); err != nil {
		resp.Diagnostics.AddError(
			"Could not delete fcs_vm",
			fmt.Sprintf("DELETE /v1/environments/%s/vms/%s failed: %s", envID, id, err),
		)
		return
	}

	// Poll until the VM is really gone (404 or status=destroyed) so
	// dependent destroys (the environment) only start afterwards.
	if err := waitForVmGone(ctx, r.client, envID, id, r.pollInterval, deleteTimeout); err != nil {
		resp.Diagnostics.AddError(
			"fcs_vm teardown did not finish",
			fmt.Sprintf("VM %s in environment %s: %s", id, envID, err),
		)
	}
}

// ImportState adopts a VM by its environment-scoped identity. VM IDs are not
// globally sufficient for the API path, so imports use
// <environment_id>/<vm_id> rather than a bare VM UUID.
func (r *vmResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(strings.TrimSpace(req.ID), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		resp.Diagnostics.AddError(
			"Invalid fcs_vm import ID",
			"Expected <environment_id>/<vm_id>, for example "+
				"11111111-2222-4333-8444-555555555555/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee.",
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("environment_id"), strings.TrimSpace(parts[0]))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), strings.TrimSpace(parts[1]))...)
}

// setVmState writes only server-observed fields. Configurable attributes stay
// at their planned/stated values, which is required during Create and Update:
// a server-normalized response must not produce an inconsistent apply result.
// Empty vm_ip/console_url become null.
func setVmState(ctx context.Context, state *tfsdk.State, vm *client.Vm, diags *diag.Diagnostics) {
	diags.Append(state.SetAttribute(ctx, path.Root("id"), vm.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("status"), vm.Status)...)
	vmIP := types.StringNull()
	if vm.VMIP != "" {
		vmIP = types.StringValue(vm.VMIP)
	}
	diags.Append(state.SetAttribute(ctx, path.Root("vm_ip"), vmIP)...)
	consoleURL := types.StringNull()
	if vm.ConsoleURL != "" {
		consoleURL = types.StringValue(vm.ConsoleURL)
	}
	diags.Append(state.SetAttribute(ctx, path.Root("console_url"), consoleURL)...)
}

// vmHasSpecReadback identifies the additive API contract that returns the
// non-secret VM specification. Required scalar pointers distinguish a new
// response from an older response; nullable vDC bindings may legitimately be
// absent.
func vmHasSpecReadback(vm *client.Vm) bool {
	return vm.CPUCores != nil &&
		vm.MemoryGB != nil &&
		vm.DiskGB != nil &&
		vm.NICNetwork != nil &&
		vm.Running != nil
}

// setVmReadState refreshes configurable, non-secret fields from a complete API
// readback. Cloud-init and timeouts deliberately remain state/config-only.
func setVmReadState(ctx context.Context, state *tfsdk.State, vm *client.Vm, diags *diag.Diagnostics) {
	setVmState(ctx, state, vm, diags)
	if vm.Name != "" {
		diags.Append(state.SetAttribute(ctx, path.Root("name"), vm.Name)...)
	}

	switch {
	case vm.HarborArtifactID != "" && vm.Image == "":
		diags.Append(state.SetAttribute(ctx, path.Root("image"), types.StringNull())...)
		diags.Append(state.SetAttribute(ctx, path.Root("harbor_artifact_id"), vm.HarborArtifactID)...)
	case vm.Image != "" && vm.HarborArtifactID == "":
		diags.Append(state.SetAttribute(ctx, path.Root("image"), vm.Image)...)
		diags.Append(state.SetAttribute(ctx, path.Root("harbor_artifact_id"), types.StringNull())...)
	default:
		diags.AddError(
			"Invalid VM image source returned by FCS API",
			"Expected exactly one of image or harbor_artifact_id in the VM read response.",
		)
		return
	}

	diags.Append(state.SetAttribute(ctx, path.Root("cpu_cores"), *vm.CPUCores)...)
	diags.Append(state.SetAttribute(ctx, path.Root("memory_gb"), *vm.MemoryGB)...)
	diags.Append(state.SetAttribute(ctx, path.Root("disk_gb"), *vm.DiskGB)...)
	diags.Append(state.SetAttribute(ctx, path.Root("nic_network"), *vm.NICNetwork)...)
	diags.Append(state.SetAttribute(ctx, path.Root("running"), *vm.Running)...)

	switch {
	case vm.VdcID == nil && vm.NetworkID == nil:
		diags.Append(state.SetAttribute(ctx, path.Root("vdc_id"), types.StringNull())...)
		diags.Append(state.SetAttribute(ctx, path.Root("network_id"), types.StringNull())...)
	case vm.VdcID != nil && vm.NetworkID != nil:
		diags.Append(state.SetAttribute(ctx, path.Root("vdc_id"), *vm.VdcID)...)
		diags.Append(state.SetAttribute(ctx, path.Root("network_id"), *vm.NetworkID)...)
	default:
		diags.AddError(
			"Invalid VM network binding returned by FCS API",
			"Expected vdc_id and network_id to be both null or both set in the VM read response.",
		)
	}
}
