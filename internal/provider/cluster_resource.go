package provider

// Generic cluster resource backing fcs_business_cluster (kind=business),
// fcs_flex_cluster (kind=flex) and fcs_namespace (kind=namespace). All three
// map onto the same /v1/environments/{id}/clusters sub-API; the kind is
// derived from the resource type, never user-configured.
//
// Sizing changes are applied in place through PATCH. Environment and version
// changes still require replacement.

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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

// defaultDeleteTimeout applies to all cluster kinds: DELETE returns 202 and
// the provider polls until 404/destroyed so dependent resources (the
// environment) are only destroyed after the cluster is really gone.
const defaultDeleteTimeout = 15 * time.Minute

var (
	_ resource.Resource                   = (*clusterResource)(nil)
	_ resource.ResourceWithConfigure      = (*clusterResource)(nil)
	_ resource.ResourceWithImportState    = (*clusterResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*clusterResource)(nil)
	_ resource.ResourceWithValidateConfig = (*clusterResource)(nil)
)

// clusterKind parametrizes the shared implementation per resource type.
type clusterKind struct {
	kind                 string // ClusterSpec.kind value sent to the API
	typeSuffix           string // appended to the provider type name
	description          string
	sizing               bool // size / vcpu / ram_gb / storage_gb / k8s_version
	dedicated            bool // cp_*/worker_*/pvc_storage_gb/rke2_version node-pool sizing
	defaultCreateTimeout time.Duration
	defaultDeleteTimeout time.Duration // 0 falls back to the package defaultDeleteTimeout
}

// deleteTimeout returns the kind-specific delete timeout, defaulting to the
// shared 15m when the kind does not override it.
func (k clusterKind) deleteTimeout() time.Duration {
	if k.defaultDeleteTimeout > 0 {
		return k.defaultDeleteTimeout
	}
	return defaultDeleteTimeout
}

// NewBusinessClusterResource returns fcs_business_cluster (kind=business).
func NewBusinessClusterResource() resource.Resource {
	return newClusterResource(clusterKind{
		kind:       "business",
		typeSuffix: "_business_cluster",
		description: "An FCS Business cluster (dedicated virtual cluster) inside an " +
			"environment. CIDRs, IPs and VLANs are allocated server-side.",
		sizing:               true,
		defaultCreateTimeout: 20 * time.Minute,
	})
}

// NewFlexClusterResource returns fcs_flex_cluster (kind=flex).
func NewFlexClusterResource() resource.Resource {
	return newClusterResource(clusterKind{
		kind:       "flex",
		typeSuffix: "_flex_cluster",
		description: "An FCS Flex cluster (shared virtual cluster) inside an environment. " +
			"CIDRs and quotas are allocated server-side.",
		sizing:               true,
		defaultCreateTimeout: 25 * time.Minute,
	})
}

// NewNamespaceResource returns fcs_namespace (kind=namespace, Free tier).
func NewNamespaceResource() resource.Resource {
	return newClusterResource(clusterKind{
		kind:       "namespace",
		typeSuffix: "_namespace",
		description: "An FCS Free-tier namespace inside an environment (a real namespace " +
			"on a shared cluster). Sizing is fixed by the product tier.",
		sizing:               false,
		defaultCreateTimeout: 10 * time.Minute,
	})
}

func newClusterResource(cfg clusterKind) resource.Resource {
	return &clusterResource{cfg: cfg, pollInterval: defaultPollInterval}
}

type clusterResource struct {
	client       *client.Client
	cfg          clusterKind
	pollInterval time.Duration // overridable in tests
}

func (r *clusterResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + r.cfg.typeSuffix
}

func (r *clusterResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := map[string]schema.Attribute{
		"environment_id": schema.StringAttribute{
			Required: true,
			Description: "ID of the fcs_environment hosting this cluster. " +
				"Changing it forces a new cluster.",
			Validators: []validator.String{
				stringvalidator.LengthAtLeast(1),
			},
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.RequiresReplace(),
			},
		},
		"id": schema.StringAttribute{
			Computed:    true,
			Description: "Server-assigned cluster ID (UUID).",
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.UseStateForUnknown(),
			},
		},
		"status": schema.StringAttribute{
			Computed:    true,
			Description: "Lifecycle status: provisioning | active | resizing | error | offboarding | destroyed.",
		},
		"api_server_url": schema.StringAttribute{
			Computed:    true,
			Description: "Kube-API endpoint of the cluster (server-allocated).",
		},
		"cluster_cidr": schema.StringAttribute{
			Computed:    true,
			Description: "Pod CIDR (server-allocated).",
		},
		"service_cidr": schema.StringAttribute{
			Computed:    true,
			Description: "Service CIDR (server-allocated).",
		},
		"provisioning_diagnostics": schema.StringAttribute{
			Computed: true,
			Description: "Server-provided provisioning diagnostics for asynchronous waits. " +
				"Dedicated clusters include service gateway scope/status, tenant-networking " +
				"pipeline, Apstra commit handoff and Rancher handoff details. This value is " +
				"informational and must not be used as desired configuration.",
		},
		"timeouts": timeouts.Attributes(ctx, timeouts.Opts{
			Create: true,
			CreateDescription: fmt.Sprintf("How long to wait for the cluster to reach status=active "+
				"(default %s). This is an object attribute: configure it as "+
				"timeouts = { create = \"30m\" }; a timeouts { ... } block is invalid.", r.cfg.defaultCreateTimeout),
			Delete: true,
			DeleteDescription: fmt.Sprintf("How long to wait for the teardown to finish (GET returns 404 "+
				"or status=destroyed; default %s). Configure it inside the same timeouts = { ... } object.", r.cfg.deleteTimeout()),
			Update: true,
			UpdateDescription: fmt.Sprintf("How long to wait for an in-place resize to reach the requested size "+
				"(default %s). Configure it inside the same timeouts = { ... } object.", r.cfg.defaultCreateTimeout),
		}),
	}

	if r.cfg.sizing {
		customSizingPaths := []path.Expression{
			path.MatchRoot("vcpu"),
			path.MatchRoot("ram_gb"),
			path.MatchRoot("storage_gb"),
		}
		attrs["size"] = schema.StringAttribute{
			Optional: true,
			Description: "T-shirt size S | M | L (server default: S when neither size nor custom " +
				"sizing is set). Mutually exclusive with vcpu/ram_gb/storage_gb. Changes resize " +
				"the existing cluster in place. Removing size later keeps the observed current " +
				"sizing; set size explicitly to request another preset.",
			Validators: []validator.String{
				stringvalidator.OneOf("S", "M", "L"),
				stringvalidator.ConflictsWith(customSizingPaths...),
			},
		}
		attrs["vcpu"] = schema.Int64Attribute{
			Optional:    true,
			Computed:    true,
			Description: "Custom sizing: vCPUs (alternative to size). Changes resize the existing cluster in place.",
			Validators: []validator.Int64{
				int64validator.AtLeast(1),
			},
			PlanModifiers: []planmodifier.Int64{unknownWhenSizeConfigured{}},
		}
		attrs["ram_gb"] = schema.Int64Attribute{
			Optional:    true,
			Computed:    true,
			Description: "Custom sizing: RAM in GB (alternative to size). Changes resize the existing cluster in place.",
			Validators: []validator.Int64{
				int64validator.AtLeast(1),
			},
			PlanModifiers: []planmodifier.Int64{unknownWhenSizeConfigured{}},
		}
		attrs["storage_gb"] = schema.Int64Attribute{
			Optional: true,
			Computed: true,
			Description: "Custom sizing: storage in GB (alternative to size). Storage is grow-only; " +
				"changes resize the existing cluster in place.",
			Validators: []validator.Int64{
				int64validator.AtLeast(1),
			},
			PlanModifiers: []planmodifier.Int64{unknownWhenSizeConfigured{}},
		}
		attrs["k8s_version"] = schema.StringAttribute{
			Optional: true,
			Description: "Kubernetes version, e.g. v1.35.3-k3s1 (server default when unset). " +
				"Changing it forces a new cluster.",
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.RequiresReplace(),
			},
		}
	}

	if r.cfg.dedicated {
		for name, at := range dedicatedSizingAttributes() {
			attrs[name] = at
		}
	}

	description := r.cfg.description + " Create is asynchronous: the provider polls until " +
		"status=active. Destroy polls until the cluster is gone so the environment is " +
		"only torn down afterwards. Import uses <environment_id>/<cluster_id>."
	if r.cfg.sizing || r.cfg.dedicated {
		description += " Sizing is read back from the API and changes are applied in place; " +
			"the cluster ID and Kubernetes API remain unchanged."
	}
	resp.Schema = schema.Schema{
		Description: description,
		Attributes:  attrs,
	}
}

// ModifyPlan makes volatile server fields unknown whenever a real sizing
// update is planned. A resize changes both the status and diagnostics, and
// known old plan values would otherwise violate Terraform's apply consistency
// contract after a successful in-place resize or timeout recovery.
func (r *clusterResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	sizingChanged := r.sizingChanged(ctx, &req.Plan, &req.State, &resp.Diagnostics)
	if sizingChanged || resizeStatusInProgress(ctx, &req.State, &resp.Diagnostics) {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("status"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("provisioning_diagnostics"), types.StringUnknown())...)
	}
}

func (r *clusterResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	if !r.cfg.dedicated {
		return
	}

	var cpNodes, cpVcpu, workerNodes, workerVcpu, workerRamGB, pvcStorageGB types.Int64
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("cp_nodes"), &cpNodes)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("cp_vcpu"), &cpVcpu)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("worker_nodes"), &workerNodes)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("worker_vcpu"), &workerVcpu)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("worker_ram_gb"), &workerRamGB)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("pvc_storage_gb"), &pvcStorageGB)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !pvcStorageGB.IsNull() && !pvcStorageGB.IsUnknown() && pvcStorageGB.ValueInt64()%50 != 0 {
		resp.Diagnostics.AddAttributeError(
			path.Root("pvc_storage_gb"),
			"Invalid dedicated cluster PVC storage size",
			"pvc_storage_gb must be a multiple of 50 GB.",
		)
	}

	if cpNodes.IsUnknown() || cpVcpu.IsUnknown() || workerNodes.IsUnknown() {
		return
	}
	cpNodeCount := int64(3)
	if !cpNodes.IsNull() {
		cpNodeCount = cpNodes.ValueInt64()
	}
	workerNodeCount := int64(0)
	if !workerNodes.IsNull() {
		workerNodeCount = workerNodes.ValueInt64()
	}
	if cpNodeCount == 1 &&
		workerNodeCount == 0 &&
		!cpVcpu.IsNull() &&
		cpVcpu.ValueInt64() < dedicatedMinSingleCombinedCPVcpu {
		resp.Diagnostics.AddAttributeError(
			path.Root("cp_vcpu"),
			"Dedicated cluster control plane is too small",
			fmt.Sprintf(
				"cp_vcpu must be at least %d when cp_nodes=1 and worker_nodes=0. "+
					"A single combined RKE2 node runs the control plane, CNI, Rancher agent and workloads.",
				dedicatedMinSingleCombinedCPVcpu,
			),
		)
	}

	if workerNodeCount <= 0 || workerVcpu.IsUnknown() || workerRamGB.IsUnknown() {
		return
	}
	if !workerVcpu.IsNull() && workerVcpu.ValueInt64() < dedicatedMinWorkerVcpu {
		resp.Diagnostics.AddAttributeError(
			path.Root("worker_vcpu"),
			"Dedicated worker vCPU is too small",
			fmt.Sprintf("worker_vcpu must be at least %d when worker_nodes is greater than 0.", dedicatedMinWorkerVcpu),
		)
	}
	if !workerRamGB.IsNull() && workerRamGB.ValueInt64() < dedicatedMinWorkerRamGB {
		resp.Diagnostics.AddAttributeError(
			path.Root("worker_ram_gb"),
			"Dedicated worker RAM is too small",
			fmt.Sprintf("worker_ram_gb must be at least %d when worker_nodes is greater than 0.", dedicatedMinWorkerRamGB),
		)
	}
}

func (r *clusterResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *clusterResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var envID string
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("environment_id"), &envID)...)

	var timeoutsVal timeouts.Value
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("timeouts"), &timeoutsVal)...)
	if resp.Diagnostics.HasError() {
		return
	}
	createTimeout, diags := timeoutsVal.Create(ctx, r.cfg.defaultCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	spec := client.ClusterSpec{Kind: r.cfg.kind}
	if r.cfg.sizing {
		var size, k8sVersion types.String
		var vcpu, ramGB, storageGB types.Int64
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("size"), &size)...)
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("vcpu"), &vcpu)...)
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("ram_gb"), &ramGB)...)
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("storage_gb"), &storageGB)...)
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("k8s_version"), &k8sVersion)...)
		if resp.Diagnostics.HasError() {
			return
		}
		spec.Size = size.ValueString()
		spec.VCPU = vcpu.ValueInt64()
		spec.RAMGB = ramGB.ValueInt64()
		spec.StorageGB = storageGB.ValueInt64()
		spec.K8sVersion = k8sVersion.ValueString()
	}
	if r.cfg.dedicated {
		readDedicatedSizing(ctx, &req.Plan, &spec, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	// 202 (newly created) and 200 (idempotent re-apply: the server is
	// idempotent over (environment, kind) and returns the existing
	// non-terminal cluster) are both success per contract.
	created, err := r.client.CreateCluster(ctx, envID, spec)
	if err != nil {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Could not create fcs%s", r.cfg.typeSuffix),
			fmt.Sprintf("POST /v1/environments/%s/clusters (kind=%s) failed: %s", envID, r.cfg.kind, err),
		)
		return
	}
	if created.Kind != "" && created.Kind != r.cfg.kind {
		resp.Diagnostics.AddError(
			fmt.Sprintf("API returned wrong cluster kind for fcs%s", r.cfg.typeSuffix),
			fmt.Sprintf("Expected kind %q, the API returned %q for cluster %s. This is a server bug.",
				r.cfg.kind, created.Kind, created.ID),
		)
		return
	}

	// Persist the cluster in state before polling: if the wait below fails
	// (timeout, status=error, cancellation), Terraform still tracks the
	// resource (tainted) instead of orphaning it server-side.
	resp.State.Raw = req.Plan.Raw
	r.setClusterState(ctx, &resp.State, created, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	final, err := waitForClusterActive(ctx, r.client, envID, created.ID, r.pollInterval, createTimeout)
	if err != nil {
		if final != nil {
			r.setClusterState(ctx, &resp.State, final, &resp.Diagnostics)
		}
		resp.Diagnostics.AddError(
			fmt.Sprintf("fcs%s did not become active", r.cfg.typeSuffix),
			fmt.Sprintf("Cluster %s in environment %s: %s", created.ID, envID, err),
		)
		return
	}

	r.setClusterState(ctx, &resp.State, final, &resp.Diagnostics)
}

func (r *clusterResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var envID, id string
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cl, err := r.client.GetCluster(ctx, envID, id)
	if err != nil {
		if client.IsNotFound(err) {
			// Drift-safe: gone server-side (TTL reaper or manual teardown).
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError(
			fmt.Sprintf("Could not read fcs%s", r.cfg.typeSuffix),
			fmt.Sprintf("GET /v1/environments/%s/clusters/%s failed: %s", envID, id, err),
		)
		return
	}
	if cl.Status == "destroyed" {
		// The server may keep destroyed rows readable for a while; treat
		// them like a 404 so Terraform plans a re-create instead of
		// drifting forever (same handling as fcs_vm).
		resp.State.RemoveResource(ctx)
		return
	}
	if cl.Kind != "" && cl.Kind != r.cfg.kind {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Could not read fcs%s", r.cfg.typeSuffix),
			fmt.Sprintf("Cluster %s in environment %s has kind %q, but this resource requires kind %q. "+
				"Use the matching Terraform resource type.", id, envID, cl.Kind, r.cfg.kind),
		)
		return
	}

	r.setClusterState(ctx, &resp.State, cl, &resp.Diagnostics)
}

func (r *clusterResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var envID, id string
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	var timeoutsVal timeouts.Value
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("timeouts"), &timeoutsVal)...)
	if resp.Diagnostics.HasError() {
		return
	}
	updateTimeout, diags := timeoutsVal.Update(ctx, r.cfg.defaultCreateTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	sizingChanged := r.sizingChanged(ctx, &req.Plan, &req.State, &resp.Diagnostics)
	resizeInProgress := resizeStatusInProgress(ctx, &req.State, &resp.Diagnostics)
	if !sizingChanged && !resizeInProgress {
		resp.State.Raw = req.State.Raw
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("timeouts"), timeoutsVal)...)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if !sizingChanged {
		r.recoverClusterResize(ctx, envID, id, updateTimeout, &req.Plan, resp)
		return
	}

	spec := r.resizeSpecFromPlan(ctx, &req.Plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if clusterResizeSpecEmpty(spec) {
		// A plan can contain framework-injected unknown Optional+Computed
		// sizing values (not user intent), for example against an older API.
		// Never turn that into an empty mutating PATCH. Refresh the observed
		// cluster into the planned state and retain only the timeout change.
		current, err := r.client.GetCluster(ctx, envID, id)
		if err != nil {
			resp.Diagnostics.AddError(
				fmt.Sprintf("Could not refresh fcs%s", r.cfg.typeSuffix),
				fmt.Sprintf("GET /v1/environments/%s/clusters/%s failed: %s", envID, id, err),
			)
			return
		}
		resp.State.Raw = req.Plan.Raw
		r.setClusterState(ctx, &resp.State, current, &resp.Diagnostics)
		return
	}
	accepted, err := r.client.ResizeCluster(ctx, envID, id, spec)
	if err != nil {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Could not resize fcs%s", r.cfg.typeSuffix),
			fmt.Sprintf("PATCH /v1/environments/%s/clusters/%s failed: %s", envID, id, err),
		)
		return
	}
	if accepted.ID != id {
		resp.Diagnostics.AddError(
			fmt.Sprintf("API changed identity while resizing fcs%s", r.cfg.typeSuffix),
			fmt.Sprintf("Expected cluster ID %s, API returned %s. The existing cluster was not removed from state.", id, accepted.ID),
		)
		return
	}
	if accepted.Kind != "" && accepted.Kind != r.cfg.kind {
		resp.Diagnostics.AddError(
			fmt.Sprintf("API returned wrong cluster kind while resizing fcs%s", r.cfg.typeSuffix),
			fmt.Sprintf("Expected kind %q, API returned %q for cluster %s.", r.cfg.kind, accepted.Kind, id),
		)
		return
	}
	if accepted.DesiredSpec == nil {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Could not verify fcs%s resize", r.cfg.typeSuffix),
			"The cluster resize response omitted desired_spec. The provider cannot safely verify convergence without the API-resolved target sizing.",
		)
		return
	}
	if err := validateResizeTarget(r.cfg.dedicated, r.cfg.kind, *accepted.DesiredSpec); err != nil {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Could not verify fcs%s resize", r.cfg.typeSuffix),
			fmt.Sprintf("The cluster resize response contains an invalid desired_spec: %s", err),
		)
		return
	}

	resp.State.Raw = req.Plan.Raw
	r.setClusterState(ctx, &resp.State, accepted, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	final, err := waitForClusterResized(ctx, r.client, envID, id, *accepted.DesiredSpec, r.cfg.dedicated, r.pollInterval, updateTimeout)
	if err != nil {
		if final != nil {
			r.setClusterState(ctx, &resp.State, final, &resp.Diagnostics)
		}
		resp.Diagnostics.AddError(
			fmt.Sprintf("fcs%s resize did not finish", r.cfg.typeSuffix),
			fmt.Sprintf("Cluster %s in environment %s: %s", id, envID, err),
		)
		return
	}
	r.setClusterState(ctx, &resp.State, final, &resp.Diagnostics)
}

func resizeStatusInProgress(ctx context.Context, state *tfsdk.State, diags *diag.Diagnostics) bool {
	var status types.String
	diags.Append(state.GetAttribute(ctx, path.Root("status"), &status)...)
	return !status.IsNull() && !status.IsUnknown() && status.ValueString() == "resizing"
}

func (r *clusterResource) recoverClusterResize(
	ctx context.Context,
	envID string,
	id string,
	updateTimeout time.Duration,
	plan *tfsdk.Plan,
	resp *resource.UpdateResponse,
) {
	current, err := r.client.GetCluster(ctx, envID, id)
	if err != nil {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Could not recover fcs%s resize", r.cfg.typeSuffix),
			fmt.Sprintf("GET /v1/environments/%s/clusters/%s failed: %s", envID, id, err),
		)
		return
	}
	if current.ID != id {
		resp.Diagnostics.AddError(
			fmt.Sprintf("API changed identity while recovering fcs%s resize", r.cfg.typeSuffix),
			fmt.Sprintf("Expected cluster ID %s, API returned %s. The existing cluster was not removed from state.", id, current.ID),
		)
		return
	}
	if current.Kind != "" && current.Kind != r.cfg.kind {
		resp.Diagnostics.AddError(
			fmt.Sprintf("API returned wrong cluster kind while recovering fcs%s resize", r.cfg.typeSuffix),
			fmt.Sprintf("Expected kind %q, API returned %q for cluster %s.", r.cfg.kind, current.Kind, id),
		)
		return
	}
	if current.DesiredSpec == nil {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Could not recover fcs%s resize", r.cfg.typeSuffix),
			"The in-progress cluster resize omitted desired_spec. The provider cannot safely verify convergence without the API-resolved target sizing.",
		)
		return
	}
	if err := validateResizeTarget(r.cfg.dedicated, r.cfg.kind, *current.DesiredSpec); err != nil {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Could not recover fcs%s resize", r.cfg.typeSuffix),
			fmt.Sprintf("The in-progress cluster resize contains an invalid desired_spec: %s", err),
		)
		return
	}
	resp.State.Raw = plan.Raw
	r.setClusterState(ctx, &resp.State, current, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	final, err := waitForClusterResized(
		ctx, r.client, envID, id, *current.DesiredSpec,
		r.cfg.dedicated, r.pollInterval, updateTimeout,
	)
	if err != nil {
		if final != nil {
			r.setClusterState(ctx, &resp.State, final, &resp.Diagnostics)
		}
		resp.Diagnostics.AddError(
			fmt.Sprintf("fcs%s resize recovery did not finish", r.cfg.typeSuffix),
			fmt.Sprintf("Cluster %s in environment %s: %s", id, envID, err),
		)
		return
	}
	r.setClusterState(ctx, &resp.State, final, &resp.Diagnostics)
}

func validateResizeTarget(dedicated bool, kind string, target client.ClusterSizing) error {
	if dedicated {
		if target.CPNodes < 1 || target.CPVcpu < 1 || target.CPRamGB < 1 || target.PVCStorageGB < 1 {
			return fmt.Errorf("dedicated sizing is incomplete")
		}
		if target.WorkerNodes < 0 || target.WorkerVcpu < 0 || target.WorkerRamGB < 0 {
			return fmt.Errorf("dedicated worker sizing contains negative values")
		}
		return nil
	}
	if target.VCPU < 1 || target.RAMGB < 1 || target.StorageGB < 1 {
		return fmt.Errorf("%s sizing is incomplete", kind)
	}
	return nil
}

func clusterResizeSpecEmpty(spec client.ClusterResizeSpec) bool {
	return spec.Size == "" && spec.VCPU == 0 && spec.RAMGB == 0 && spec.StorageGB == 0 &&
		spec.CPNodes == 0 && spec.CPVcpu == 0 && spec.CPRamGB == 0 &&
		spec.WorkerNodes == nil && spec.WorkerVcpu == nil && spec.WorkerRamGB == nil && spec.PVCStorageGB == 0
}

func (r *clusterResource) sizingChanged(ctx context.Context, plan *tfsdk.Plan, state *tfsdk.State, diags *diag.Diagnostics) bool {
	if !r.cfg.sizing && !r.cfg.dedicated {
		return false
	}
	if r.cfg.sizing {
		var planSize, stateSize types.String
		diags.Append(plan.GetAttribute(ctx, path.Root("size"), &planSize)...)
		diags.Append(state.GetAttribute(ctx, path.Root("size"), &stateSize)...)
		if !planSize.Equal(stateSize) {
			return true
		}
		if !planSize.IsNull() && !planSize.IsUnknown() && planSize.ValueString() != "" {
			return false
		}
		if int64AttributesChanged(ctx, plan, state, diags, "vcpu", "ram_gb", "storage_gb") {
			return true
		}
	}
	if r.cfg.dedicated {
		return int64AttributesChanged(ctx, plan, state, diags,
			"cp_nodes", "cp_vcpu", "cp_ram_gb", "worker_nodes", "worker_vcpu", "worker_ram_gb", "pvc_storage_gb")
	}
	return false
}

func int64AttributesChanged(ctx context.Context, plan *tfsdk.Plan, state *tfsdk.State, diags *diag.Diagnostics, names ...string) bool {
	for _, name := range names {
		var planValue, stateValue types.Int64
		diags.Append(plan.GetAttribute(ctx, path.Root(name), &planValue)...)
		diags.Append(state.GetAttribute(ctx, path.Root(name), &stateValue)...)
		if planValue.IsUnknown() {
			continue
		}
		if !planValue.Equal(stateValue) {
			return true
		}
	}
	return false
}

func (r *clusterResource) resizeSpecFromPlan(ctx context.Context, plan *tfsdk.Plan, diags *diag.Diagnostics) client.ClusterResizeSpec {
	var spec client.ClusterResizeSpec
	if r.cfg.sizing {
		var size types.String
		var vcpu, ramGB, storageGB types.Int64
		diags.Append(plan.GetAttribute(ctx, path.Root("size"), &size)...)
		diags.Append(plan.GetAttribute(ctx, path.Root("vcpu"), &vcpu)...)
		diags.Append(plan.GetAttribute(ctx, path.Root("ram_gb"), &ramGB)...)
		diags.Append(plan.GetAttribute(ctx, path.Root("storage_gb"), &storageGB)...)
		if !size.IsNull() && !size.IsUnknown() {
			spec.Size = size.ValueString()
		} else {
			spec.VCPU = vcpu.ValueInt64()
			spec.RAMGB = ramGB.ValueInt64()
			spec.StorageGB = storageGB.ValueInt64()
		}
	}
	if r.cfg.dedicated {
		var cpNodes, cpVcpu, cpRamGB, workerNodes, workerVcpu, workerRamGB, pvcStorageGB types.Int64
		diags.Append(plan.GetAttribute(ctx, path.Root("cp_nodes"), &cpNodes)...)
		diags.Append(plan.GetAttribute(ctx, path.Root("cp_vcpu"), &cpVcpu)...)
		diags.Append(plan.GetAttribute(ctx, path.Root("cp_ram_gb"), &cpRamGB)...)
		diags.Append(plan.GetAttribute(ctx, path.Root("worker_nodes"), &workerNodes)...)
		diags.Append(plan.GetAttribute(ctx, path.Root("worker_vcpu"), &workerVcpu)...)
		diags.Append(plan.GetAttribute(ctx, path.Root("worker_ram_gb"), &workerRamGB)...)
		diags.Append(plan.GetAttribute(ctx, path.Root("pvc_storage_gb"), &pvcStorageGB)...)
		spec.CPNodes = cpNodes.ValueInt64()
		spec.CPVcpu = cpVcpu.ValueInt64()
		spec.CPRamGB = cpRamGB.ValueInt64()
		spec.WorkerNodes = int64Pointer(workerNodes.ValueInt64())
		spec.WorkerVcpu = int64Pointer(workerVcpu.ValueInt64())
		spec.WorkerRamGB = int64Pointer(workerRamGB.ValueInt64())
		spec.PVCStorageGB = pvcStorageGB.ValueInt64()
	}
	return spec
}

func int64Pointer(value int64) *int64 { return &value }

func (r *clusterResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var envID, id string
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("environment_id"), &envID)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)

	var timeoutsVal timeouts.Value
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("timeouts"), &timeoutsVal)...)
	if resp.Diagnostics.HasError() {
		return
	}
	deleteTimeout, diags := timeoutsVal.Delete(ctx, r.cfg.deleteTimeout())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// 202 and 404 (already gone) both count as success; the client
	// tolerates the 404.
	if err := r.client.DeleteCluster(ctx, envID, id); err != nil {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Could not delete fcs%s", r.cfg.typeSuffix),
			fmt.Sprintf("DELETE /v1/environments/%s/clusters/%s failed: %s", envID, id, err),
		)
		return
	}

	// Poll until the cluster is really gone so dependent destroys (the
	// environment) only start afterwards.
	if err := waitForClusterGone(ctx, r.client, envID, id, r.pollInterval, deleteTimeout); err != nil {
		resp.Diagnostics.AddError(
			fmt.Sprintf("fcs%s teardown did not finish", r.cfg.typeSuffix),
			fmt.Sprintf("Cluster %s in environment %s: %s", id, envID, err),
		)
	}
}

// ImportState adopts a cluster by its environment-scoped identity. Cluster IDs
// are not globally sufficient for the API path, so imports use
// <environment_id>/<cluster_id> rather than a bare cluster UUID.
func (r *clusterResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(strings.TrimSpace(req.ID), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		resp.Diagnostics.AddError(
			fmt.Sprintf("Invalid fcs%s import ID", r.cfg.typeSuffix),
			"Expected <environment_id>/<cluster_id>, for example "+
				"11111111-2222-4333-8444-555555555555/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee.",
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("environment_id"), strings.TrimSpace(parts[0]))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), strings.TrimSpace(parts[1]))...)
}

// setClusterState writes the server-observed contract fields into state. A
// create/resize acceptance response may omit sizing while the following GET
// includes it, so planned sizing is only replaced by a complete API snapshot.
func (r *clusterResource) setClusterState(ctx context.Context, state *tfsdk.State, cl *client.Cluster, diags *diag.Diagnostics) {
	diags.Append(state.SetAttribute(ctx, path.Root("id"), cl.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("status"), cl.Status)...)
	diags.Append(state.SetAttribute(ctx, path.Root("api_server_url"), cl.APIServerURL)...)
	diags.Append(state.SetAttribute(ctx, path.Root("cluster_cidr"), cl.ClusterCIDR)...)
	diags.Append(state.SetAttribute(ctx, path.Root("service_cidr"), cl.ServiceCIDR)...)
	diags.Append(state.SetAttribute(ctx, path.Root("provisioning_diagnostics"), cl.ProvisioningDiagnostics)...)
	if r.cfg.sizing {
		var size types.String
		diags.Append(state.GetAttribute(ctx, path.Root("size"), &size)...)
		if !size.IsNull() && !size.IsUnknown() && size.ValueString() != "" {
			// Optional+Computed custom sizing fields are unknown in a plan that
			// selects a t-shirt size. Resolve them to null after apply so the
			// provider never returns unknown values and the two sizing modes stay
			// mutually exclusive in state.
			diags.Append(state.SetAttribute(ctx, path.Root("vcpu"), types.Int64Null())...)
			diags.Append(state.SetAttribute(ctx, path.Root("ram_gb"), types.Int64Null())...)
			diags.Append(state.SetAttribute(ctx, path.Root("storage_gb"), types.Int64Null())...)
		} else if cl.VCPU > 0 && cl.RAMGB > 0 && cl.StorageGB > 0 {
			diags.Append(state.SetAttribute(ctx, path.Root("vcpu"), cl.VCPU)...)
			diags.Append(state.SetAttribute(ctx, path.Root("ram_gb"), cl.RAMGB)...)
			diags.Append(state.SetAttribute(ctx, path.Root("storage_gb"), cl.StorageGB)...)
		} else {
			resolveUnknownInt64Attributes(ctx, state, diags, "vcpu", "ram_gb", "storage_gb")
		}
	}
	if r.cfg.dedicated && cl.CPNodes > 0 && cl.CPVcpu > 0 && cl.CPRamGB > 0 && cl.PVCStorageGB > 0 {
		diags.Append(state.SetAttribute(ctx, path.Root("cp_nodes"), cl.CPNodes)...)
		diags.Append(state.SetAttribute(ctx, path.Root("cp_vcpu"), cl.CPVcpu)...)
		diags.Append(state.SetAttribute(ctx, path.Root("cp_ram_gb"), cl.CPRamGB)...)
		diags.Append(state.SetAttribute(ctx, path.Root("worker_nodes"), cl.WorkerNodes)...)
		diags.Append(state.SetAttribute(ctx, path.Root("worker_vcpu"), cl.WorkerVcpu)...)
		diags.Append(state.SetAttribute(ctx, path.Root("worker_ram_gb"), cl.WorkerRamGB)...)
		diags.Append(state.SetAttribute(ctx, path.Root("pvc_storage_gb"), cl.PVCStorageGB)...)
	}
}

func resolveUnknownInt64Attributes(ctx context.Context, state *tfsdk.State, diags *diag.Diagnostics, names ...string) {
	for _, name := range names {
		var value types.Int64
		diags.Append(state.GetAttribute(ctx, path.Root(name), &value)...)
		if value.IsUnknown() {
			diags.Append(state.SetAttribute(ctx, path.Root(name), types.Int64Null())...)
		}
	}
}
