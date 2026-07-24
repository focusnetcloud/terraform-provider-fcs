package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

var (
	_ resource.Resource                = (*harborArtifactResource)(nil)
	_ resource.ResourceWithConfigure   = (*harborArtifactResource)(nil)
	_ resource.ResourceWithImportState = (*harborArtifactResource)(nil)
)

var (
	harborRepositoryPattern = regexp.MustCompile(
		`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`,
	)
	harborReferencePattern = regexp.MustCompile(
		`^(?:[A-Za-z0-9_][A-Za-z0-9._-]{0,127}|sha256:[0-9a-f]{64})$`,
	)
)

// NewHarborArtifactResource returns fcs_harbor_artifact.
func NewHarborArtifactResource() resource.Resource {
	return &harborArtifactResource{}
}

type harborArtifactResource struct {
	client *client.Client
}

type harborArtifactResourceModel struct {
	ID                 types.String `tfsdk:"id"`
	Repository         types.String `tfsdk:"repository"`
	Reference          types.String `tfsdk:"reference"`
	Kind               types.String `tfsdk:"kind"`
	Project            types.String `tfsdk:"project"`
	Registry           types.String `tfsdk:"registry"`
	Digest             types.String `tfsdk:"digest"`
	CanonicalReference types.String `tfsdk:"canonical_reference"`
	MediaType          types.String `tfsdk:"media_type"`
	SizeBytes          types.Int64  `tfsdk:"size_bytes"`
	Status             types.String `tfsdk:"status"`
	CreatedAt          types.String `tfsdk:"created_at"`
	UpdatedAt          types.String `tfsdk:"updated_at"`
}

func (r *harborArtifactResource) Metadata(
	_ context.Context,
	req resource.MetadataRequest,
	resp *resource.MetadataResponse,
) {
	resp.TypeName = req.ProviderTypeName + "_harbor_artifact"
}

func (r *harborArtifactResource) Schema(
	_ context.Context,
	_ resource.SchemaRequest,
	resp *resource.SchemaResponse,
) {
	requiresReplace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "Registers an existing container or VM disk artifact from the tenant's Harbor " +
			"project and pins it to an immutable digest. Destroy removes only the FCS registration; " +
			"it never deletes Harbor manifests or layers.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "FCS registry resource ID (UUID).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"repository": schema.StringAttribute{
				Required:    true,
				Description: "Repository path relative to the server-derived tenant project.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 255),
					stringvalidator.RegexMatches(
						harborRepositoryPattern,
						"must be a lowercase OCI repository path without empty, relative or absolute segments",
					),
				},
				PlanModifiers: requiresReplace,
			},
			"reference": schema.StringAttribute{
				Required:    true,
				Description: "Existing OCI tag or sha256 digest. Tags are resolved and pinned during create.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 128),
					stringvalidator.RegexMatches(
						harborReferencePattern,
						"must be a valid OCI tag or lowercase sha256 digest",
					),
				},
				PlanModifiers: requiresReplace,
			},
			"kind": schema.StringAttribute{
				Required:    true,
				Description: "Artifact use: container or vm_disk.",
				Validators: []validator.String{
					stringvalidator.OneOf("container", "vm_disk"),
				},
				PlanModifiers: requiresReplace,
			},
			"project": schema.StringAttribute{
				Computed:    true,
				Description: "Tenant Harbor project derived by the API.",
			},
			"registry": schema.StringAttribute{
				Computed:    true,
				Description: "Registry hostname.",
			},
			"digest": schema.StringAttribute{
				Computed:    true,
				Description: "Immutable sha256 digest resolved by the API.",
			},
			"canonical_reference": schema.StringAttribute{
				Computed:    true,
				Description: "Digest-pinned registry/project/repository@sha256 reference.",
			},
			"media_type": schema.StringAttribute{
				Computed:    true,
				Description: "OCI manifest media type reported by Harbor.",
			},
			"size_bytes": schema.Int64Attribute{
				Computed:    true,
				Description: "Artifact size reported by Harbor, in bytes.",
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "FCS registration lifecycle status.",
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
				Description: "Last verification timestamp.",
			},
		},
	}
}

func (r *harborArtifactResource) Configure(
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

func (r *harborArtifactResource) Create(
	ctx context.Context,
	req resource.CreateRequest,
	resp *resource.CreateResponse,
) {
	var plan harborArtifactResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	artifact, err := r.client.CreateHarborArtifact(ctx, client.HarborArtifactSpec{
		Repository: plan.Repository.ValueString(),
		Reference:  plan.Reference.ValueString(),
		Kind:       plan.Kind.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not create fcs_harbor_artifact",
			fmt.Sprintf(
				"POST /v1/registry/artifacts for %q:%q failed: %s",
				plan.Repository.ValueString(),
				plan.Reference.ValueString(),
				err,
			),
		)
		return
	}
	applyHarborArtifact(artifact, &plan)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *harborArtifactResource) Read(
	ctx context.Context,
	req resource.ReadRequest,
	resp *resource.ReadResponse,
) {
	var state harborArtifactResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	artifact, err := r.client.GetHarborArtifact(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Could not read fcs_harbor_artifact",
			fmt.Sprintf("GET /v1/registry/artifacts/%s failed: %s", state.ID.ValueString(), err),
		)
		return
	}
	applyHarborArtifact(artifact, &state)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *harborArtifactResource) Update(
	_ context.Context,
	_ resource.UpdateRequest,
	resp *resource.UpdateResponse,
) {
	resp.Diagnostics.AddError(
		"Unexpected fcs_harbor_artifact update",
		"repository, reference and kind all require replacement; no in-place update exists.",
	)
}

func (r *harborArtifactResource) Delete(
	ctx context.Context,
	req resource.DeleteRequest,
	resp *resource.DeleteResponse,
) {
	var state harborArtifactResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteHarborArtifact(ctx, state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError(
			"Could not delete fcs_harbor_artifact",
			fmt.Sprintf("DELETE /v1/registry/artifacts/%s failed: %s", state.ID.ValueString(), err),
		)
	}
}

func (r *harborArtifactResource) ImportState(
	ctx context.Context,
	req resource.ImportStateRequest,
	resp *resource.ImportStateResponse,
) {
	id := strings.TrimSpace(req.ID)
	if !uuidPattern.MatchString(id) {
		resp.Diagnostics.AddError(
			"Invalid fcs_harbor_artifact import ID",
			"Expected a canonical UUID, for example 66666666-7777-4888-8999-aaaaaaaaaaaa.",
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
}

func applyHarborArtifact(
	artifact *client.HarborArtifact,
	model *harborArtifactResourceModel,
) {
	model.ID = types.StringValue(artifact.ID)
	model.Repository = types.StringValue(artifact.Repository)
	model.Reference = types.StringValue(artifact.Reference)
	model.Kind = types.StringValue(artifact.Kind)
	model.Project = types.StringValue(artifact.Project)
	model.Registry = types.StringValue(artifact.Registry)
	model.Digest = types.StringValue(artifact.Digest)
	model.CanonicalReference = types.StringValue(artifact.CanonicalReference)
	model.MediaType = nullableString(artifact.MediaType)
	model.SizeBytes = types.Int64Value(artifact.SizeBytes)
	model.Status = types.StringValue(artifact.Status)
	model.CreatedAt = nullableString(artifact.CreatedAt)
	model.UpdatedAt = nullableString(artifact.UpdatedAt)
}
