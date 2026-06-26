package provider

// fcs_environment_egress: an egress-allow rule for a cluster, applied through
// the central NAT-GW egress ACLs. Maps onto
// /v1/environments/{id}/egress.
//
// Every spec attribute is immutable on the API (RequiresReplace); there is no
// Update path. Create is NOT polled to "active" (a live egress is always
// status="provisioning"; the reconcile is asynchronous and the contract
// defines no active state). Destroy polls until the rule is gone.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

// hostnamePrefixPattern enforces the contract's DNS-label charset for
// fcs_environment_ingress.hostname_prefix. It mirrors server-side validation:
// ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ - lowercase letters/digits/hyphens, but no
// leading or trailing hyphen. Using the laxer ^[a-z0-9-]+$ here let the plan
// accept prefixes (e.g. "-a", "a-") the API then rejects at apply. Shared so
// the ingress resource can reference it.
var hostnamePrefixPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// defaultEgressCreateTimeout covers POST + (no poll).
const defaultEgressCreateTimeout = 5 * time.Minute

var (
	_ resource.Resource                   = (*egressResource)(nil)
	_ resource.ResourceWithConfigure      = (*egressResource)(nil)
	_ resource.ResourceWithValidateConfig = (*egressResource)(nil)
)

// NewEgressResource returns the fcs_environment_egress resource.
func NewEgressResource() resource.Resource {
	return &egressResource{pollInterval: defaultPollInterval}
}

type egressResource struct {
	client       *client.Client
	pollInterval time.Duration // overridable in tests
}

func (r *egressResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment_egress"
}

func (r *egressResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An egress-allow rule for a cluster, applied through the central FCS NAT-GW " +
			"egress ACLs (kube-ovn). All attributes force replacement; there is no in-place " +
			"update. Destroy removes the rule and waits until it is reconciled away.",
		Attributes: map[string]schema.Attribute{
			"environment_id": schema.StringAttribute{
				Required: true,
				Description: "ID of the fcs_environment hosting the cluster. " +
					"Changing it forces a new egress rule.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cluster_id": schema.StringAttribute{
				Required: true,
				Description: "ID of the cluster the rule is scoped to. " +
					"Changing it forces a new egress rule.",
				Validators: []validator.String{
					stringvalidator.LengthAtLeast(1),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"destination_cidr": schema.StringAttribute{
				Required: true,
				Description: "Destination CIDR (IPv4/IPv6) or /32 host the cluster may reach " +
					"(IPs and CIDRs only; no FQDNs). Changing it forces a new egress rule.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 253),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"protocol": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("any"),
				Description: "Protocol: tcp | udp | icmp | any (default \"any\"). " +
					"Changing it forces a new egress rule.",
				Validators: []validator.String{
					stringvalidator.OneOf("tcp", "udp", "icmp", "any"),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"port_range": schema.StringAttribute{
				Optional: true,
				Description: "Port or port range for tcp/udp, e.g. \"443\" or \"8000-9000\". " +
					"Changing it forces a new egress rule.",
				Validators: []validator.String{
					stringvalidator.LengthBetween(1, 20),
				},
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Server-assigned egress rule ID (UUID).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Lifecycle status: provisioning | offboarding | destroyed.",
			},
			"timeouts": timeouts.Attributes(ctx, timeouts.Opts{
				Create: true,
				CreateDescription: fmt.Sprintf("How long to wait for the create call to return "+
					"(default %s). Accepts a duration string such as \"10m\".", defaultEgressCreateTimeout),
				Delete: true,
				DeleteDescription: fmt.Sprintf("How long to wait for the teardown to finish (GET returns 404 "+
					"or status=destroyed; default %s).", defaultDeleteTimeout),
			}),
		},
	}
}

func (r *egressResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// parsePortRange parses a "single" or "low-high" port spec. ok=false on any
// non-numeric or wrong-arity input. Mirrors the server's split('-') parse in
// EgressCreateRequest._validate_port_range_format.
func parsePortRange(s string) (low, high int, ok bool) {
	parts := strings.Split(s, "-")
	switch len(parts) {
	case 1:
		n, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, 0, false
		}
		return n, n, true
	case 2:
		lo, err1 := strconv.Atoi(parts[0])
		hi, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		return lo, hi, true
	default:
		return 0, 0, false
	}
}

// ValidateConfig rejects at plan time the same port_range inputs the server
// rejects with 422. Two rules, mirroring EgressCreateRequest:
//   - port_range only valid for tcp/udp. For any/icmp the reconciler drops the
//     port filter and would apply a broader rule than asked — reject the combo
//     (an unset protocol defaults to "any" server-side, so treat Null as "any").
//   - port_range must be a single port or ascending "low-high", all in 1..65535.
func (r *egressResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var protocol, portRange types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("protocol"), &protocol)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("port_range"), &portRange)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Unknown (e.g. interpolated from another resource) -> can't validate at
	// plan time; the server still enforces 422 at apply.
	if portRange.IsNull() || portRange.IsUnknown() || portRange.ValueString() == "" {
		return
	}
	pr := portRange.ValueString()

	// Cross-field tcp/udp check ONLY when protocol is known. An unknown protocol
	// (interpolated from another resource/var) might resolve to tcp/udp at
	// apply, so pre-rejecting port_range here would fail valid plans; the server
	// still enforces the combination with 422 at apply. A Null protocol IS known
	// — it resolves to the schema default "any".
	if !protocol.IsUnknown() {
		proto := protocol.ValueString()
		if protocol.IsNull() || proto == "" {
			proto = "any" // schema default applied at apply time
		}
		if proto == "any" || proto == "icmp" {
			resp.Diagnostics.AddAttributeError(
				path.Root("port_range"),
				"port_range not allowed for this protocol",
				fmt.Sprintf("port_range is only valid for protocol \"tcp\" or \"udp\"; got protocol=%q. "+
					"Drop port_range or set protocol to tcp/udp.", proto),
			)
			return
		}
	}

	// Syntax/bounds always run (port_range is known at this point), independent
	// of whether protocol is known — a malformed range is wrong for any proto.
	low, high, ok := parsePortRange(pr)
	if !ok {
		resp.Diagnostics.AddAttributeError(
			path.Root("port_range"),
			"Invalid port_range",
			fmt.Sprintf("port_range %q must be a single port (\"443\") or an ascending "+
				"range (\"8000-9000\").", pr),
		)
		return
	}
	if low < 1 || low > 65535 || high < 1 || high > 65535 {
		resp.Diagnostics.AddAttributeError(
			path.Root("port_range"),
			"port_range out of range",
			fmt.Sprintf("port_range %q has a port outside 1..65535.", pr),
		)
		return
	}
	if strings.Contains(pr, "-") && low >= high {
		resp.Diagnostics.AddAttributeError(
			path.Root("port_range"),
			"port_range must be ascending",
			fmt.Sprintf("port_range %q must be ascending (low < high); use a single port "+
				"for one port.", pr),
		)
	}
}

func (r *egressResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var envID string
	var clusterID, destinationCIDR, protocol, portRange types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("cluster_id"), &clusterID)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("destination_cidr"), &destinationCIDR)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("protocol"), &protocol)...)
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("port_range"), &portRange)...)
	if resp.Diagnostics.HasError() {
		return
	}

	spec := client.EgressSpec{
		ClusterID:       clusterID.ValueString(),
		DestinationCIDR: destinationCIDR.ValueString(),
		Protocol:        protocol.ValueString(),
	}
	if !portRange.IsNull() && !portRange.IsUnknown() {
		pr := portRange.ValueString()
		spec.PortRange = &pr
	}

	// 202 (newly created) and 200 (idempotent re-apply over (environment,
	// cluster_id, destination_cidr, protocol, port_range)) are both success
	// per contract. There is no poll-to-active (the record stays
	// status=provisioning).
	created, err := r.client.CreateEgress(ctx, envID, spec)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not create fcs_environment_egress",
			fmt.Sprintf("POST /v1/environments/%s/egress (cluster=%s, cidr=%s) failed: %s",
				envID, spec.ClusterID, spec.DestinationCIDR, err),
		)
		return
	}

	resp.State.Raw = req.Plan.Raw
	setEgressState(ctx, &resp.State, created, &resp.Diagnostics)
}

func (r *egressResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var envID, id string
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() {
		return
	}

	eg, err := r.client.GetEgress(ctx, envID, id)
	if err != nil {
		if client.IsNotFound(err) {
			// Drift-safe: gone server-side (env teardown / TTL reaper).
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			"Could not read fcs_environment_egress",
			fmt.Sprintf("GET /v1/environments/%s/egress/%s failed: %s", envID, id, err),
		)
		return
	}
	if eg.Status == "destroyed" {
		// Treat a readable destroyed row like a 404 so Terraform plans a
		// re-create instead of drifting forever.
		resp.State.RemoveResource(ctx)
		return
	}

	setEgressState(ctx, &resp.State, eg, &resp.Diagnostics)
}

func (r *egressResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Unreachable: every configurable attribute carries RequiresReplace.
	resp.Diagnostics.AddError(
		"fcs_environment_egress does not support in-place updates",
		"All attributes require replacement; Terraform should never call Update. This is a bug in the provider.",
	)
}

func (r *egressResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
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
	if err := r.client.DeleteEgress(ctx, envID, id); err != nil {
		resp.Diagnostics.AddError(
			"Could not delete fcs_environment_egress",
			fmt.Sprintf("DELETE /v1/environments/%s/egress/%s failed: %s", envID, id, err),
		)
		return
	}

	// Poll until the egress rule is really gone so dependent destroys (the
	// cluster, then the environment) only start afterwards.
	if err := waitForEgressGone(ctx, r.client, envID, id, r.pollInterval, deleteTimeout); err != nil {
		resp.Diagnostics.AddError(
			"fcs_environment_egress teardown did not finish",
			fmt.Sprintf("Egress %s in environment %s: %s", id, envID, err),
		)
	}
}

// setEgressState writes the computed contract fields of an Egress into the
// state. Configurable attributes (environment_id, cluster_id,
// destination_cidr, protocol, port_range, timeouts) stay at their
// planned/stated values — the read endpoint does not return them.
func setEgressState(ctx context.Context, state *tfsdk.State, eg *client.Egress, diags *diag.Diagnostics) {
	diags.Append(state.SetAttribute(ctx, path.Root("id"), eg.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("status"), eg.Status)...)
}
