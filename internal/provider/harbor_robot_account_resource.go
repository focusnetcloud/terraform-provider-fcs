package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

var (
	_ resource.Resource                = (*harborRobotAccountResource)(nil)
	_ resource.ResourceWithConfigure   = (*harborRobotAccountResource)(nil)
	_ resource.ResourceWithImportState = (*harborRobotAccountResource)(nil)
)

var (
	harborRobotNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	uuidPattern            = regexp.MustCompile(
		`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`,
	)
)

// NewHarborRobotAccountResource returns fcs_harbor_robot_account.
func NewHarborRobotAccountResource() resource.Resource {
	return &harborRobotAccountResource{}
}

type harborRobotAccountResource struct {
	client *client.Client
}

type harborRobotAccountResourceModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	Description       types.String `tfsdk:"description"`
	PermissionProfile types.String `tfsdk:"permission_profile"`
	ExpiresInDays     types.Int64  `tfsdk:"expires_in_days"`
	Disabled          types.Bool   `tfsdk:"disabled"`
	RotationTrigger   types.String `tfsdk:"rotation_trigger"`
	Project           types.String `tfsdk:"project"`
	Registry          types.String `tfsdk:"registry"`
	Username          types.String `tfsdk:"username"`
	Secret            types.String `tfsdk:"secret"`
	Status            types.String `tfsdk:"status"`
	ExpiresAt         types.String `tfsdk:"expires_at"`
	CreatedAt         types.String `tfsdk:"created_at"`
	UpdatedAt         types.String `tfsdk:"updated_at"`
}

func (r *harborRobotAccountResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_harbor_robot_account"
}

func (r *harborRobotAccountResource) Schema(
	_ context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	resp.Schema = schema.Schema{
		Description: "A tenant project-scoped Harbor Robot Account. Permission profiles are fixed to " +
			"pull or push_pull. The secret is returned only after create or rotation and is stored " +
			"as sensitive Terraform state; protect the state backend accordingly.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "FCS registry resource ID (UUID).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Tenant-local Robot Account name. Changing it creates a new account.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 63),
					stringvalidator.RegexMatches(
						harborRobotNamePattern,
						"must start with a lowercase letter or digit and contain only lowercase letters, digits, '.', '_' or '-'",
					),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"description": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
				Description: "Human-readable purpose of the account.",
			},
			"permission_profile": schema.StringAttribute{
				Required:    true,
				Description: "Fixed least-privilege profile: pull or push_pull.",
				Validators: []validator.String{
					stringvalidator.OneOf("pull", "push_pull"),
				},
			},
			"expires_in_days": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Default:     int64default.StaticInt64(90),
				Description: "Credential lifetime in days (1–365).",
				Validators: []validator.Int64{
					int64validator.Between(1, 365),
				},
			},
			"disabled": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Disable the account without deleting it.",
			},
			"rotation_trigger": schema.StringAttribute{
				Optional: true,
				Description: "Arbitrary version marker. Changing this value rotates the secret in place. " +
					"The marker is provider-local and is never sent to Harbor.",
			},
			"project": schema.StringAttribute{
				Computed:    true,
				Description: "Tenant Harbor project derived by the API, e.g. t-8012.",
			},
			"registry": schema.StringAttribute{
				Computed:    true,
				Description: "Registry hostname.",
			},
			"username": schema.StringAttribute{
				Computed:    true,
				Description: "Full Harbor Robot Account username.",
			},
			"secret": schema.StringAttribute{
				Computed:    true,
				Sensitive:   true,
				Description: "One-time Robot Account secret. Null after import until rotation_trigger changes.",
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "FCS lifecycle status.",
			},
			"expires_at": schema.StringAttribute{
				Computed:    true,
				Description: "Server-reported expiry timestamp.",
			},
			"created_at": schema.StringAttribute{
				Computed:    true,
				Description: "Creation timestamp.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"updated_at": schema.StringAttribute{
				Computed:    true,
				Description: "Last update timestamp.",
			},
		},
	}
}

func (r *harborRobotAccountResource) Configure(
	_ context.Context,
	req resource.ConfigureRequest,
	resp *resource.ConfigureResponse,
) {
	if req.ProviderData == nil {
		return
	}
	apiClient, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected resource configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	r.client = apiClient
}

func (r *harborRobotAccountResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var plan harborRobotAccountResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	created, err := r.client.CreateHarborRobotAccount(
		ctx,
		client.HarborRobotAccountSpec{
			Name:              plan.Name.ValueString(),
			Description:       plan.Description.ValueString(),
			PermissionProfile: plan.PermissionProfile.ValueString(),
			ExpiresInDays:     plan.ExpiresInDays.ValueInt64(),
			Disabled:          plan.Disabled.ValueBool(),
		},
	)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not create fcs_harbor_robot_account",
			fmt.Sprintf("POST /v1/registry/robot-accounts for name %q failed: %s", plan.Name.ValueString(), err),
		)
		return
	}

	applyHarborRobotAccount(created, &plan)
	plan.Secret = types.StringValue(created.Secret)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *harborRobotAccountResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var state harborRobotAccountResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	robot, err := r.client.GetHarborRobotAccount(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Could not read fcs_harbor_robot_account",
			fmt.Sprintf("GET /v1/registry/robot-accounts/%s failed: %s", state.ID.ValueString(), err),
		)
		return
	}

	// applyHarborRobotAccount intentionally leaves Secret and RotationTrigger
	// untouched. GET never returns a secret, so replacing it would silently
	// destroy the only usable copy in Terraform state.
	applyHarborRobotAccount(robot, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *harborRobotAccountResource) Update(
	ctx context.Context,
	req resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	var plan harborRobotAccountResourceModel
	var state harborRobotAccountResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	updated, err := r.client.UpdateHarborRobotAccount(
		ctx,
		state.ID.ValueString(),
		client.HarborRobotAccountUpdate{
			Description:       plan.Description.ValueString(),
			PermissionProfile: plan.PermissionProfile.ValueString(),
			ExpiresInDays:     plan.ExpiresInDays.ValueInt64(),
			Disabled:          plan.Disabled.ValueBool(),
		},
	)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not update fcs_harbor_robot_account",
			fmt.Sprintf("PATCH /v1/registry/robot-accounts/%s failed: %s", state.ID.ValueString(), err),
		)
		return
	}

	// Computed Secret is unknown in the plan. Preserve the old state unless
	// this update explicitly changes the rotation marker.
	plan.Secret = state.Secret
	applyHarborRobotAccount(updated, &plan)
	if !plan.RotationTrigger.IsUnknown() &&
		!plan.RotationTrigger.Equal(state.RotationTrigger) {
		rotated, rotateErr := r.client.RotateHarborRobotAccount(
			ctx,
			state.ID.ValueString(),
		)
		if rotateErr != nil {
			resp.Diagnostics.AddError(
				"Could not rotate fcs_harbor_robot_account secret",
				fmt.Sprintf(
					"POST /v1/registry/robot-accounts/%s/rotations failed: %s",
					state.ID.ValueString(),
					rotateErr,
				),
			)
			return
		}
		applyHarborRobotAccount(rotated, &plan)
		plan.Secret = types.StringValue(rotated.Secret)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *harborRobotAccountResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var state harborRobotAccountResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteHarborRobotAccount(ctx, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError(
			"Could not delete fcs_harbor_robot_account",
			fmt.Sprintf("DELETE /v1/registry/robot-accounts/%s failed: %s", state.ID.ValueString(), err),
		)
	}
}

func (r *harborRobotAccountResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	id := strings.TrimSpace(req.ID)
	if !uuidPattern.MatchString(id) {
		resp.Diagnostics.AddError(
			"Invalid fcs_harbor_robot_account import ID",
			"Expected a canonical UUID, for example 11111111-2222-4333-8444-555555555555.",
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}

// applyHarborRobotAccount copies server-observed metadata. Secret and
// RotationTrigger are deliberately provider-local and remain untouched.
func applyHarborRobotAccount(
	robot *client.HarborRobotAccount,
	model *harborRobotAccountResourceModel,
) {
	model.ID = types.StringValue(robot.ID)
	model.Name = types.StringValue(robot.Name)
	model.Description = types.StringValue(robot.Description)
	model.PermissionProfile = types.StringValue(robot.PermissionProfile)
	model.ExpiresInDays = types.Int64Value(robot.ExpiresInDays)
	model.Disabled = types.BoolValue(robot.Disabled)
	model.Project = types.StringValue(robot.Project)
	model.Registry = types.StringValue(robot.Registry)
	model.Status = types.StringValue(robot.Status)
	model.Username = nullableString(robot.Username)
	model.ExpiresAt = nullableString(robot.ExpiresAt)
	if robot.CreatedAt != "" {
		model.CreatedAt = types.StringValue(robot.CreatedAt)
	}
	model.UpdatedAt = nullableString(robot.UpdatedAt)
}

func nullableString(value string) types.String {
	if value == "" {
		return types.StringNull()
	}
	return types.StringValue(value)
}
