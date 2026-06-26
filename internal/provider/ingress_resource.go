package provider

// fcs_environment_ingress: makes a business cluster's app externally reachable
// through the central NAT-GW. Maps onto
// /v1/environments/{id}/ingress. It binds NO additional public IP — it reuses
// the EIP the business cluster already allocated at create time.
//
// Every spec attribute is immutable on the API (RequiresReplace); there is no
// Update path. Create is NOT polled to "active": a live ingress is always
// status="provisioning" (the contract defines no active state, the NAT-GW
// reconcile is asynchronous). Destroy polls until the rule is gone (404 /
// destroyed) so dependent destroys only start afterwards.

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
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

// defaultIngressCreateTimeout covers POST + (no poll); generous so a slow
// idempotency lookup never trips it.
const defaultIngressCreateTimeout = 5 * time.Minute

var (
	_ resource.Resource              = (*ingressResource)(nil)
	_ resource.ResourceWithConfigure = (*ingressResource)(nil)
)

// NewIngressResource returns the fcs_environment_ingress resource.
func NewIngressResource() resource.Resource {
	return &ingressResource{pollInterval: defaultPollInterval}
}

type ingressResource struct {
	client       *client.Client
	pollInterval time.Duration // overridable in tests
}

func (r *ingressResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment_ingress"
}

func (r *ingressResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Exposes a business cluster's service externally through the central FCS " +
			"NAT-GW (DNAT on the cluster's existing public IP — no extra public IP is bound). " +
			"All attributes force replacement; there is no in-place update. Destroy removes the " +
			"DNAT rule and waits until it is reconciled away.",
		Attributes: map[string]schema.Attribute{
			"environment_id": schema.StringAttribute{
				Required: true,
				Description: "ID of the fcs_environment hosting the cluster. " +
					"Changing it forces a new ingress.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cluster_id": schema.StringAttribute{
				Required: true,
				Description: "ID of the business cluster to expose (must be active and have a " +
					"public IP). Changing it forces a new ingress.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"service": schema.StringAttribute{
				Required: true,
				Description: "Target service name inside the cluster (informational for the rule " +
					"description). Changing it forces a new ingress.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 253),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"port": schema.Int64Attribute{
				Required:    true,
				Description: "Target port (1–65535). Changing it forces a new ingress.",
				Validators: []validator.Int64{
					int64validator.Between(1, 65535),
				},
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
				},
			},
			"tls": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("auto"),
				Description: "TLS handling: \"auto\" (the cluster ingress terminates TLS). " +
					"Plain L7 host routing is not currently supported. Changing this value forces a new ingress.",
				Validators: []validator.String{
					stringvalidator.OneOf("auto"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"hostname_prefix": schema.StringAttribute{
				Required: true,
				Description: "DNS label prefix; idempotency key per (environment, cluster). The " +
					"reachable endpoint is the L4 public_url https://<public_ip>[:<port>]. Must match " +
					"^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ (max. 63 chars; no leading or " +
					"trailing hyphen). Changing it forces a new ingress.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 63),
					stringvalidator.RegexMatches(hostnamePrefixPattern,
						"must be lowercase letters, digits and hyphens with no leading or "+
							"trailing hyphen (^[a-z0-9]([a-z0-9-]*[a-z0-9])?$)"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Server-assigned ingress ID (UUID).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Lifecycle status: provisioning | offboarding | destroyed.",
			},
			"public_url": schema.StringAttribute{
				Computed: true,
				Description: "Reachable L4 endpoint https://<public_ip>[:<port>] (null until known). " +
					"Hostname-based L7 routing is planned for a future release.",
			},
			"timeouts": timeouts.Attributes(ctx, timeouts.Opts{
				Create: true,
				CreateDescription: fmt.Sprintf("How long to wait for the create call to return "+
					"(default %s). Accepts a duration string such as \"10m\".", defaultIngressCreateTimeout),
				Delete: true,
				DeleteDescription: fmt.Sprintf("How long to wait for the teardown to finish (GET returns 404 "+
					"or status=destroyed; default %s).", defaultDeleteTimeout),
			}),
		},
	}
}

func (r *ingressResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *ingressResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var envID string
	var clusterID, service, tls, hostnamePrefix types.String
	var port types.Int64
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("cluster_id"), &clusterID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("service"), &service)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("port"), &port)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("tls"), &tls)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("hostname_prefix"), &hostnamePrefix)...)
	if resp.Diagnostics.HasError() {
		return
	}

	spec := client.IngressSpec{
		ClusterID:      clusterID.ValueString(),
		Service:        service.ValueString(),
		Port:           port.ValueInt64(),
		TLS:            tls.ValueString(),
		HostnamePrefix: hostnamePrefix.ValueString(),
	}

	// 202 (newly created) and 200 (idempotent re-apply over (environment,
	// cluster_id, hostname_prefix)) are both success per contract. The
	// response already carries the stable fields; there is no poll-to-active
	// (the record stays status=provisioning).
	created, err := r.client.CreateIngress(ctx, envID, spec)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not create fcs_environment_ingress",
			fmt.Sprintf("POST /v1/environments/%s/ingress (cluster=%s, prefix=%s) failed: %s",
				envID, spec.ClusterID, spec.HostnamePrefix, err),
		)
		return
	}

	resp.State.Raw = req.Plan.Raw
	setIngressState(ctx, &resp.State, created, &resp.Diagnostics)
}

func (r *ingressResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var envID, id string
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ing, err := r.client.GetIngress(ctx, envID, id)
	if err != nil {
		if client.IsNotFound(err) {
			// Drift-safe: gone server-side (env teardown / TTL reaper).
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Could not read fcs_environment_ingress",
			fmt.Sprintf("GET /v1/environments/%s/ingress/%s failed: %s", envID, id, err),
		)
		return
	}
	if ing.Status == "destroyed" {
		// Treat a readable destroyed row like a 404 so Terraform plans a
		// re-create instead of drifting forever.
		resp.State.RemoveResource(ctx)
		return
	}

	setIngressState(ctx, &resp.State, ing, &resp.Diagnostics)
}

func (r *ingressResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Unreachable: every configurable attribute carries RequiresReplace.
	resp.Diagnostics.AddError(
		"fcs_environment_ingress does not support in-place updates",
		"All attributes require replacement; Terraform should never call Update. This is a bug in the provider.",
	)
}

func (r *ingressResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
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

	// 202 and 404 (already gone) both count as success; the client tolerates
	// the 404.
	if err := r.client.DeleteIngress(ctx, envID, id); err != nil {
		resp.Diagnostics.AddError(
			"Could not delete fcs_environment_ingress",
			fmt.Sprintf("DELETE /v1/environments/%s/ingress/%s failed: %s", envID, id, err),
		)
		return
	}

	// Poll until the DNAT rule is really gone so dependent destroys (the
	// cluster, then the environment) only start afterwards.
	if err := waitForIngressGone(ctx, r.client, envID, id, r.pollInterval, deleteTimeout); err != nil {
		resp.Diagnostics.AddError(
			"fcs_environment_ingress teardown did not finish",
			fmt.Sprintf("Ingress %s in environment %s: %s", id, envID, err),
		)
	}
}

// setIngressState writes the computed contract fields of an Ingress into the
// state. Configurable attributes (environment_id, cluster_id, service, port,
// tls, hostname_prefix, timeouts) stay at their planned/stated values — the
// read endpoint does not return them. An empty public_url becomes null.
func setIngressState(ctx context.Context, state *tfsdk.State, ing *client.Ingress, diags *diag.Diagnostics) {
	diags.Append(state.SetAttribute(ctx, path.Root("id"), ing.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("status"), ing.Status)...)
	publicURL := types.StringNull()
	if ing.PublicURL != "" {
		publicURL = types.StringValue(ing.PublicURL)
	}
	diags.Append(state.SetAttribute(ctx, path.Root("public_url"), publicURL)...)
}
