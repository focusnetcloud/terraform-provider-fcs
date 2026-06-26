package provider

// fcs_images: the VM image catalog approved for environments via
// GET /v1/images. Every listed name is a valid fcs_vm image value.

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
)

var (
	_ datasource.DataSource              = (*imagesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*imagesDataSource)(nil)
)

// NewImagesDataSource returns the fcs_images data source.
func NewImagesDataSource() datasource.DataSource {
	return &imagesDataSource{}
}

type imagesDataSource struct {
	client *client.Client
}

type imagesDataSourceModel struct {
	Images []imageModel `tfsdk:"images"`
}

type imageModel struct {
	Name        types.String `tfsdk:"name"`
	DisplayName types.String `tfsdk:"display_name"`
	Source      types.String `tfsdk:"source"`
}

func (d *imagesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_images"
}

func (d *imagesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The VM image catalog approved for environments (GET /v1/images). " +
			"Every listed name is a valid `image` for fcs_vm.",
		Attributes: map[string]schema.Attribute{
			"images": schema.ListNestedAttribute{
				Computed:    true,
				Description: "Available images.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Computed:    true,
							Description: "Catalog name; valid as fcs_vm image.",
						},
						"display_name": schema.StringAttribute{
							Computed:    true,
							Description: "Human-readable name (may be null).",
						},
						"source": schema.StringAttribute{
							Computed:    true,
							Description: "catalog (standard image catalog) | env (operator allowlist).",
						},
					},
				},
			},
		},
	}
}

func (d *imagesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return // provider not configured yet (e.g. during validate)
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected data source configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a bug in the provider.", req.ProviderData),
		)
		return
	}
	d.client = c
}

func (d *imagesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	images, err := d.client.ListImages(ctx)
	if err != nil {
		resp.Diagnostics.AddError(
			"Could not read fcs_images",
			fmt.Sprintf("GET /v1/images failed: %s", err),
		)
		return
	}

	model := imagesDataSourceModel{Images: make([]imageModel, 0, len(images))}
	for _, img := range images {
		displayName := types.StringNull()
		if img.DisplayName != "" {
			displayName = types.StringValue(img.DisplayName)
		}
		model.Images = append(model.Images, imageModel{
			Name:        types.StringValue(img.Name),
			DisplayName: displayName,
			Source:      types.StringValue(img.Source),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &model)...)
}
