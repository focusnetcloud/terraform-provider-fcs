package provider

// Generic cluster resource backing fcs_business_cluster (kind=business),
// fcs_flex_cluster (kind=flex) and fcs_namespace (kind=namespace). All three
// map onto the same /v1/environments/{id}/clusters sub-API; the kind is
// derived from the resource type, never user-configured.
//
// All configurable attributes are immutable on the API, hence RequiresReplace
// everywhere and no Update.

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
			Description: "Lifecycle status: provisioning | active | error | offboarding | destroyed.",
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
				"(default %s). Accepts a duration string such as \"30m\".", r.cfg.defaultCreateTimeout),
			Delete: true,
			DeleteDescription: fmt.Sprintf("How long to wait for the teardown to finish (GET returns 404 "+
				"or status=destroyed; default %s).", r.cfg.deleteTimeout()),
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
				"sizing is set). Mutually exclusive with vcpu/ram_gb/storage_gb. " +
				"Changing it forces a new cluster (no resize path exists).",
			Validators: []validator.String{
				stringvalidator.OneOf("S", "M", "L"),
				stringvalidator.ConflictsWith(customSizingPaths...),
			},
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.RequiresReplace(),
			},
		}
		attrs["vcpu"] = schema.Int64Attribute{
			Optional: true,
			Description: "Custom sizing: vCPUs (alternative to size). " +
				"Changing it forces a new cluster.",
			Validators: []validator.Int64{
				int64validator.AtLeast(1),
			},
			PlanModifiers: []planmodifier.Int64{
				int64planmodifier.RequiresReplace(),
			},
		}
		attrs["ram_gb"] = schema.Int64Attribute{
			Optional: true,
			Description: "Custom sizing: RAM in GB (alternative to size). " +
				"Changing it forces a new cluster.",
			Validators: []validator.Int64{
				int64validator.AtLeast(1),
			},
			PlanModifiers: []planmodifier.Int64{
				int64planmodifier.RequiresReplace(),
			},
		}
		attrs["storage_gb"] = schema.Int64Attribute{
			Optional: true,
			Description: "Custom sizing: storage in GB (alternative to size). " +
				"Changing it forces a new cluster.",
			Validators: []validator.Int64{
				int64validator.AtLeast(1),
			},
			PlanModifiers: []planmodifier.Int64{
				int64planmodifier.RequiresReplace(),
			},
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

	resp.Schema = schema.Schema{
		Description: r.cfg.description + " Create is asynchronous: the provider polls until " +
			"status=active. Destroy polls until the cluster is gone so the environment is " +
			"only torn down afterwards.",
		Attributes: attrs,
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
	setClusterState(ctx, &resp.State, created, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}

	final, err := waitForClusterActive(ctx, r.client, envID, created.ID, r.pollInterval, createTimeout)
	if err != nil {
		if final != nil {
			setClusterState(ctx, &resp.State, final, &resp.Diagnostics)
		}
		resp.Diagnostics.AddError(
			fmt.Sprintf("fcs%s did not become active", r.cfg.typeSuffix),
			fmt.Sprintf("Cluster %s in environment %s: %s", created.ID, envID, err),
		)
		return
	}

	setClusterState(ctx, &resp.State, final, &resp.Diagnostics)
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

	setClusterState(ctx, &resp.State, cl, &resp.Diagnostics)
}

func (r *clusterResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Unreachable: every configurable attribute carries RequiresReplace.
	resp.Diagnostics.AddError(
		fmt.Sprintf("fcs%s does not support in-place updates", r.cfg.typeSuffix),
		"All attributes require replacement; Terraform should never call Update. This is a bug in the provider.",
	)
}

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

// setClusterState writes the computed contract fields of a Cluster into the
// state. Configurable attributes (environment_id, sizing, timeouts) stay at
// their planned/stated values — the read endpoint does not return them.
func setClusterState(ctx context.Context, state *tfsdk.State, cl *client.Cluster, diags *diag.Diagnostics) {
	diags.Append(state.SetAttribute(ctx, path.Root("id"), cl.ID)...)
	diags.Append(state.SetAttribute(ctx, path.Root("status"), cl.Status)...)
	diags.Append(state.SetAttribute(ctx, path.Root("api_server_url"), cl.APIServerURL)...)
	diags.Append(state.SetAttribute(ctx, path.Root("cluster_cidr"), cl.ClusterCIDR)...)
	diags.Append(state.SetAttribute(ctx, path.Root("service_cidr"), cl.ServiceCIDR)...)
	diags.Append(state.SetAttribute(ctx, path.Root("provisioning_diagnostics"), cl.ProvisioningDiagnostics)...)
}
