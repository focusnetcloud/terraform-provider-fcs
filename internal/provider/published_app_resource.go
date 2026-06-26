package provider

// fcs_published_app publishes a Service from a Free namespace or Flex cluster
// through the public tenant app gateway. It maps onto:
//
//	/v1/k8s/namespaces/{namespace_id}/published-apps
//
// The API contract is immutable: changing target service, port, namespace or
// slug creates a replacement.

import (
	"context"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

var (
	appSlugPattern    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	kubernetesNameRE  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	pathPrefixPattern = regexp.MustCompile(`^/.*`)
	reservedAppSlugs  = []string{
		"admin",
		"business",
		"dedicated",
		"default",
		"enterprise",
		"flex",
		"free",
		"internal",
		"kube",
		"kubernetes",
		"public",
		"root",
		"system",
		"test",
	}
)

var (
	_ resource.Resource              = (*publishedAppResource)(nil)
	_ resource.ResourceWithConfigure = (*publishedAppResource)(nil)
)

// NewPublishedAppResource returns the fcs_published_app resource.
func NewPublishedAppResource() resource.Resource {
	return &publishedAppResource{}
}

type publishedAppResource struct {
	client *client.Client
}

func (r *publishedAppResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_published_app"
}

func (r *publishedAppResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Publishes a Kubernetes Service from an fcs_namespace or fcs_flex_cluster " +
			"through the public app gateway (*.free.k8s.focusnet.de or *.flex.k8s.focusnet.de). " +
			"All configurable attributes force replacement.",
		Attributes: map[string]schema.Attribute{
			"namespace_id": schema.StringAttribute{
				Required: true,
				Description: "ID of the fcs_namespace or fcs_flex_cluster whose in-cluster " +
					"Service should be published. Changing it forces a new published app.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"app_slug": schema.StringAttribute{
				Required: true,
				Description: "DNS label used for the public hostname. Must be lowercase letters, " +
					"digits and single hyphen-separated segments (3-63 chars, no leading, trailing " +
					"or repeated hyphen) and must not be a reserved platform slug. The API appends " +
					"the tenant marker before the tier domain. Changing it forces a new published app.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(3, 63),
					stringvalidator.NoneOf(reservedAppSlugs...),
					stringvalidator.RegexMatches(appSlugPattern,
						"must match ^[a-z0-9]+(-[a-z0-9]+)*$"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"service_name": schema.StringAttribute{
				Required: true,
				Description: "Kubernetes Service name inside the vcluster namespace. Changing it " +
					"forces a new published app.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 63),
					stringvalidator.RegexMatches(kubernetesNameRE,
						"must be a lowercase Kubernetes DNS label"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"service_port": schema.Int64Attribute{
				Required:    true,
				Description: "Service port to publish (1-65535). Changing it forces a new published app.",
				Validators: []validator.Int64{
					int64validator.Between(1, 65535),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"vcluster_namespace": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("default"),
				Description: "Namespace inside the vcluster that contains the Service. Defaults " +
					"to \"default\". Changing it forces a new published app.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 63),
					stringvalidator.RegexMatches(kubernetesNameRE,
						"must be a lowercase Kubernetes DNS label"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"path_prefix": schema.StringAttribute{
				Optional: true,
				Description: "Optional HTTP path prefix to route to the Service, e.g. \"/\" or " +
					"\"/api\". Changing it forces a new published app.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 255),
					stringvalidator.RegexMatches(pathPrefixPattern, "must start with /"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"id": schema.Int64Attribute{
				Computed:    true,
				Description: "Server-assigned published app ID. The public API contract returns this as an integer.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"hostname": schema.StringAttribute{
				Computed:    true,
				Description: "Public hostname assigned by the app gateway, including the tenant marker.",
			},
			"tls_mode": schema.StringAttribute{
				Computed:    true,
				Description: "TLS mode applied by the app gateway. Currently auto.",
			},
		},
	}
}

func (r *publishedAppResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
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

func (r *publishedAppResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var namespaceID string
	var appSlug, serviceName, vclusterNamespace, pathPrefix types.String
	var servicePort types.Int64
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("namespace_id"), &namespaceID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("app_slug"), &appSlug)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("service_name"), &serviceName)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("service_port"), &servicePort)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("vcluster_namespace"), &vclusterNamespace)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("path_prefix"), &pathPrefix)...)
	if resp.Diagnostics.HasError() {
		return
	}

	spec := client.PublishedAppSpec{
		AppSlug:           appSlug.ValueString(),
		ServiceName:       serviceName.ValueString(),
		ServicePort:       servicePort.ValueInt64(),
		VclusterNamespace: vclusterNamespace.ValueString(),
	}
	if !pathPrefix.IsNull() && !pathPrefix.IsUnknown() {
		p := pathPrefix.ValueString()
		spec.PathPrefix = &p
	}

	created, err := r.client.CreatePublishedApp(ctx, namespaceID, spec)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not create fcs_published_app",
			fmt.Sprintf("POST /v1/k8s/namespaces/%s/published-apps (app_slug=%s, service=%s:%d) failed: %s",
				namespaceID, spec.AppSlug, spec.ServiceName, spec.ServicePort, err),
		)
		return
	}

	resp.State.Raw = req.Plan.Raw
	setPublishedAppState(ctx, &resp.State, created, &resp.Diagnostics)
}

func (r *publishedAppResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var namespaceID string
	var id int64
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("namespace_id"), &namespaceID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() {
		return
	}

	app, err := r.client.GetPublishedApp(ctx, namespaceID, id)
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Could not read fcs_published_app",
			fmt.Sprintf("GET /v1/k8s/namespaces/%s/published-apps/%d failed: %s", namespaceID, id, err),
		)
		return
	}

	setPublishedAppState(ctx, &resp.State, app, &resp.Diagnostics)
}

func (r *publishedAppResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"fcs_published_app does not support in-place updates",
		"All attributes require replacement; Terraform should never call Update. This is a bug in the provider.",
	)
}

func (r *publishedAppResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var namespaceID string
	var id int64
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("namespace_id"), &namespaceID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.DeletePublishedApp(ctx, namespaceID, id); err != nil {
		resp.Diagnostics.AddError(
			"Could not delete fcs_published_app",
			fmt.Sprintf("DELETE /v1/k8s/namespaces/%s/published-apps/%d failed: %s", namespaceID, id, err),
		)
	}
}

func setPublishedAppState(ctx context.Context, state *tfsdk.State, app *client.PublishedApp, diags *diag.Diagnostics) {
	diags.Append(state.SetAttribute(ctx, path.Root("id"), app.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("hostname"), app.Hostname)...)
	diags.Append(state.SetAttribute(ctx, path.Root("app_slug"), app.AppSlug)...)
	diags.Append(state.SetAttribute(ctx, path.Root("service_name"), app.ServiceName)...)
	diags.Append(state.SetAttribute(ctx, path.Root("service_port"), app.ServicePort)...)
	diags.Append(state.SetAttribute(ctx, path.Root("vcluster_namespace"), app.VclusterNamespace)...)
	pathPrefix := types.StringNull()
	if app.PathPrefix != "" {
		pathPrefix = types.StringValue(app.PathPrefix)
	}
	diags.Append(state.SetAttribute(ctx, path.Root("path_prefix"), pathPrefix)...)
	diags.Append(state.SetAttribute(ctx, path.Root("tls_mode"), app.TLSMode)...)
}
