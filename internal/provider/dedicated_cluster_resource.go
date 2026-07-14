package provider

// fcs_dedicated_cluster (kind=dedicated) creates a real RKE2 cluster sized via
// explicit control-plane and worker node pools.
//
// It reuses the generic clusterResource implementation. Node-pool sizing is
// updated in place; changing node CPU/RAM causes a controlled rolling
// replacement of the affected pool, while the cluster identity is preserved.

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

const (
	dedicatedMinCPVcpu               int64 = 2
	dedicatedMinSingleCombinedCPVcpu int64 = 4
	dedicatedMinCPRamGB              int64 = 8
	dedicatedMinPVCStorageGB         int64 = 50
	dedicatedMinWorkerVcpu           int64 = 2
	dedicatedMinWorkerRamGB          int64 = 4
)

// NewDedicatedClusterResource returns fcs_dedicated_cluster (kind=dedicated).
func NewDedicatedClusterResource() resource.Resource {
	return newClusterResource(clusterKind{
		kind:       "dedicated",
		typeSuffix: "_dedicated_cluster",
		description: "An FCS Dedicated cluster (a real RKE2 cluster on dedicated nodes) inside " +
			"an environment. It is sized with explicit control-plane and worker node pools " +
			"(cp_nodes/cp_vcpu/cp_ram_gb and worker_nodes/worker_vcpu/worker_ram_gb) rather " +
			"than a t-shirt size. CIDRs, IPs and VLANs are allocated server-side.",
		dedicated:            true,
		defaultCreateTimeout: 30 * time.Minute,
		defaultDeleteTimeout: 20 * time.Minute,
	})
}

// dedicatedSizingAttributes returns the in-place node-pool sizing attributes.
// Only rke2_version remains replacement-only.
func dedicatedSizingAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"cp_nodes": schema.Int64Attribute{
			Optional: true,
			Computed: true,
			Default:  int64default.StaticInt64(3),
			Description: "Number of control-plane nodes: 1 (single-node, non-HA) or 3 (HA). " +
				"Defaults to 3. Changes resize the existing pool in place.",
			Validators: []validator.Int64{
				int64validator.OneOf(1, 3),
			},
		},
		"cp_vcpu": schema.Int64Attribute{
			Required: true,
			Description: "vCPUs per control-plane node (required, at least 2; at least 4 for " +
				"a single-node cluster without workers). Changes roll the control-plane pool.",
			Validators: []validator.Int64{
				int64validator.AtLeast(dedicatedMinCPVcpu),
			},
		},
		"cp_ram_gb": schema.Int64Attribute{
			Required:    true,
			Description: "RAM in GB per control-plane node (required, at least 8). Changes roll the control-plane pool.",
			Validators: []validator.Int64{
				int64validator.AtLeast(dedicatedMinCPRamGB),
			},
		},
		"worker_nodes": schema.Int64Attribute{
			Optional: true,
			Computed: true,
			Default:  int64default.StaticInt64(0),
			Description: "Number of worker nodes (default 0: a control-plane-only cluster with " +
				"workloads scheduled on the control plane). Changes scale the existing pool in place.",
			Validators: []validator.Int64{
				int64validator.AtLeast(0),
			},
		},
		"worker_vcpu": schema.Int64Attribute{
			Optional:    true,
			Computed:    true,
			Default:     int64default.StaticInt64(0),
			Description: "vCPUs per worker node (default 0). Changes roll the worker pool.",
			Validators: []validator.Int64{
				int64validator.AtLeast(0),
			},
		},
		"worker_ram_gb": schema.Int64Attribute{
			Optional:    true,
			Computed:    true,
			Default:     int64default.StaticInt64(0),
			Description: "RAM in GB per worker node (default 0). Changes roll the worker pool.",
			Validators: []validator.Int64{
				int64validator.AtLeast(0),
			},
		},
		"pvc_storage_gb": schema.Int64Attribute{
			Optional: true,
			Computed: true,
			Default:  int64default.StaticInt64(100),
			Description: "Persistent-volume storage in GB available to the cluster (default 100, " +
				"at least 50, in 50 GB steps). Storage can only grow in place.",
			Validators: []validator.Int64{
				int64validator.AtLeast(dedicatedMinPVCStorageGB),
			},
		},
		"rke2_version": schema.StringAttribute{
			// Optional-only (not Computed): the cluster read endpoint does not
			// echo the node-pool spec, so a Computed value would stay unknown
			// after apply ("value remained unknown"). The server applies its
			// own default when this is omitted; mirrors k8s_version on the
			// business/flex clusters.
			Optional: true,
			Description: "RKE2 version, e.g. v1.31.5+rke2r1 (server default when unset). " +
				"Changing it forces a new cluster.",
			PlanModifiers: []planmodifier.String{
				stringplanmodifier.RequiresReplace(),
			},
		},
	}
}

// readDedicatedSizing copies the planned node-pool attributes into the
// ClusterSpec. Defaults are already materialised by the schema (Optional +
// Computed + Default), so the plan always carries concrete values here.
func readDedicatedSizing(ctx context.Context, plan *tfsdk.Plan, spec *client.ClusterSpec, diags *diag.Diagnostics) {
	var cpNodes, cpVcpu, cpRamGB, workerNodes, workerVcpu, workerRamGB, pvcStorageGB types.Int64
	var rke2Version types.String
	diags.Append(plan.GetAttribute(ctx, path.Root("cp_nodes"), &cpNodes)...)
	diags.Append(plan.GetAttribute(ctx, path.Root("cp_vcpu"), &cpVcpu)...)
	diags.Append(plan.GetAttribute(ctx, path.Root("cp_ram_gb"), &cpRamGB)...)
	diags.Append(plan.GetAttribute(ctx, path.Root("worker_nodes"), &workerNodes)...)
	diags.Append(plan.GetAttribute(ctx, path.Root("worker_vcpu"), &workerVcpu)...)
	diags.Append(plan.GetAttribute(ctx, path.Root("worker_ram_gb"), &workerRamGB)...)
	diags.Append(plan.GetAttribute(ctx, path.Root("pvc_storage_gb"), &pvcStorageGB)...)
	diags.Append(plan.GetAttribute(ctx, path.Root("rke2_version"), &rke2Version)...)
	if diags.HasError() {
		return
	}
	spec.CPNodes = cpNodes.ValueInt64()
	spec.CPVcpu = cpVcpu.ValueInt64()
	spec.CPRamGB = cpRamGB.ValueInt64()
	spec.WorkerNodes = workerNodes.ValueInt64()
	spec.WorkerVcpu = workerVcpu.ValueInt64()
	spec.WorkerRamGB = workerRamGB.ValueInt64()
	spec.PVCStorageGB = pvcStorageGB.ValueInt64()
	spec.RKE2Version = rke2Version.ValueString()
}
