package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// unknownWhenSizeConfigured prevents Optional+Computed custom sizing values
// from carrying their old state into a plan that switches to a t-shirt size.
// The API-resolved size is read back after PATCH and these attributes become
// null, keeping the two configuration modes mutually exclusive in state.
type unknownWhenSizeConfigured struct{}

func (unknownWhenSizeConfigured) Description(context.Context) string {
	return "Marks custom sizing unknown when a t-shirt size is configured."
}

func (m unknownWhenSizeConfigured) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (unknownWhenSizeConfigured) PlanModifyInt64(ctx context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	var size types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("size"), &size)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !size.IsNull() && !size.IsUnknown() && size.ValueString() != "" {
		if req.StateValue.IsNull() {
			resp.PlanValue = types.Int64Null()
		} else {
			resp.PlanValue = types.Int64Unknown()
		}
	}
}
