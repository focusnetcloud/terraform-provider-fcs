package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

var (
	_ resource.Resource              = (*environmentResource)(nil)
	_ resource.ResourceWithConfigure = (*environmentResource)(nil)
)

// NewEnvironmentResource returns the fcs_environment resource.
func NewEnvironmentResource() resource.Resource {
	return &environmentResource{}
}

// environmentResource manages the environment TTL envelope.
// All configurable attributes are immutable on the API, hence RequiresReplace
// everywhere and no Update path.
type environmentResource struct {
	client *client.Client
}

type environmentResourceModel struct {
	ID           types.String `tfsdk:"id"`
	Name         types.String `tfsdk:"name"`
	TTLSeconds   types.Int64  `tfsdk:"ttl_seconds"`
	Labels       types.Map    `tfsdk:"labels"`
	Status       types.String `tfsdk:"status"`
	TTLExpiresAt types.String `tfsdk:"ttl_expires_at"`
	CreatedAt    types.String `tfsdk:"created_at"`
}

func (r *environmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment"
}

func (r *environmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An FCS environment: the TTL/quota envelope for ephemeral resources. " +
			"The server-side TTL reaper is authoritative for teardown; " +
			"`terraform destroy` is advisory. Create is idempotent over (tenant, name).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Server-assigned environment ID (UUID).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "Environment name; idempotency key per tenant (max. 63 characters). " +
					"Changing it forces a new environment.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 63),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"ttl_seconds": schema.Int64Attribute{
				Optional: true,
				Description: "Optional time-to-live in seconds (300–604800). If OMITTED the " +
					"environment is PERSISTENT (no expiry, never reaped) — this is the default. " +
					"Set it to make the environment ephemeral: after expiry the server-side reaper " +
					"tears it down. Changing it forces a new environment.",
				Validators: []validator.Int64{
					int64validator.Between(300, 604800),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"labels": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Free-form labels. Changing them forces a new environment.",
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.RequiresReplace(),
				},
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Lifecycle status: provisioning | active | error | offboarding | destroyed.",
			},
			"ttl_expires_at": schema.StringAttribute{
				Computed:    true,
				Description: "Server-computed expiry timestamp (RFC 3339); null for persistent environments (no ttl_seconds).",
			},
			"created_at": schema.StringAttribute{
				Computed:    true,
				Description: "Creation timestamp (RFC 3339).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *environmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *environmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan environmentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	spec := client.EnvironmentSpec{
		Name: plan.Name.ValueString(),
	}
	// Only send ttl_seconds when the caller set it. A null/unknown ttl_seconds
	// means PERSISTENT: the field is dropped from the POST body (json omitempty),
	// so the server inserts ttl_expires_at = NULL and never reaps it. The explicit
	// IsNull guard avoids relying on the implicit null->ValueInt64()==0->omitempty
	// chain (so a future omitempty change can't silently send ttl_seconds:0).
	if !plan.TTLSeconds.IsNull() && !plan.TTLSeconds.IsUnknown() {
		spec.TTLSeconds = plan.TTLSeconds.ValueInt64()
	}
	if !plan.Labels.IsNull() && !plan.Labels.IsUnknown() {
		labels := map[string]string{}
		resp.Diagnostics.Append(plan.Labels.ElementsAs(ctx, &labels, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		spec.Labels = labels
	}

	// 202 (newly created) and 200 (idempotent re-apply over name) are both
	// success per contract; the client normalizes them.
	created, err := r.client.CreateEnvironment(ctx, spec)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not create fcs_environment",
			fmt.Sprintf("POST /v1/environments for name %q failed: %s", spec.Name, err),
		)
		return
	}

	// Read back the authoritative state.
	env, err := r.client.GetEnvironment(ctx, created.ID)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not read fcs_environment after create",
			fmt.Sprintf("GET /v1/environments/%s failed: %s", created.ID, err),
		)
		return
	}

	applyEnvironment(env, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *environmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state environmentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	env, err := r.client.GetEnvironment(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			// Drift-safe: gone server-side (TTL reaper or manual teardown).
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Could not read fcs_environment",
			fmt.Sprintf("GET /v1/environments/%s failed: %s", state.ID.ValueString(), err),
		)
		return
	}
	if env.Status == "destroyed" {
		// The server may keep destroyed rows readable for a while; treat
		// them like a 404 so Terraform plans a re-create instead of
		// drifting forever (same handling as fcs_vm).
		resp.State.RemoveResource(ctx)
		return
	}

	applyEnvironment(env, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *environmentResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Unreachable: every configurable attribute carries RequiresReplace.
	resp.Diagnostics.AddError(
		"fcs_environment does not support in-place updates",
		"All attributes require replacement; Terraform should never call Update. This is a bug in the provider.",
	)
}

func (r *environmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state environmentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// 202 and 404 (TTL reaper was faster — destroy is advisory) are both
	// success; the client tolerates the 404.
	if err := r.client.DeleteEnvironment(ctx, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError(
			"Could not delete fcs_environment",
			fmt.Sprintf("DELETE /v1/environments/%s failed: %s", state.ID.ValueString(), err),
		)
	}
}

// applyEnvironment copies the stable contract fields of an API Environment
// into the model. ttl_seconds and labels are config-only (the read endpoint
// does not return them) and stay untouched.
func applyEnvironment(env *client.Environment, model *environmentResourceModel) {
	model.ID = types.StringValue(env.ID)
	model.Name = types.StringValue(env.Name)
	model.Status = types.StringValue(env.Status)
	// A persistent environment has no expiry; the API returns null/empty -> map to
	// StringNull (not StringValue("")) so state reads null. Create and Read both
	// call this, so the mapping is identical => no "inconsistent result after apply".
	if env.TTLExpiresAt == "" {
		model.TTLExpiresAt = types.StringNull()
	} else {
		model.TTLExpiresAt = types.StringValue(env.TTLExpiresAt)
	}
	model.CreatedAt = types.StringValue(env.CreatedAt)
}
