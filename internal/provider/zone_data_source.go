package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSourceWithConfigure = (*zoneDataSource)(nil)

func NewZoneDataSource() datasource.DataSource { return &zoneDataSource{} }

type zoneDataSource struct{ data *providerData }

type zoneDataModel struct {
	Name        types.String `tfsdk:"name"`
	Status      types.String `tfsdk:"status"`
	Type        types.String `tfsdk:"type"`
	Serial      types.Int64  `tfsdk:"serial"`
	RecordCount types.Int64  `tfsdk:"record_count"`
	NameServers types.List   `tfsdk:"name_servers"`
}

func (d *zoneDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_zone"
}

func (d *zoneDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Looks up an existing WEDOS DNS zone.",
		Attributes: map[string]schema.Attribute{
			"name":         schema.StringAttribute{Required: true, Description: "Zone name, e.g. example.cz.", Validators: []validator.String{domainNameValidator{}}},
			"status":       schema.StringAttribute{Computed: true, Description: "Zone status reported by WEDOS, e.g. active."},
			"type":         schema.StringAttribute{Computed: true, Description: "Zone type: primary or secondary."},
			"serial":       schema.Int64Attribute{Computed: true, Description: "SOA serial of the zone; 0 before the first commit."},
			"record_count": schema.Int64Attribute{Computed: true, Description: "Number of records in the zone as reported by WEDOS."},
			"name_servers": schema.ListAttribute{Computed: true, ElementType: types.StringType, Description: "Authoritative name servers assigned by WEDOS."},
		},
	}
}

func (d *zoneDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *providerData, got %T", req.ProviderData))
		return
	}
	d.data = data
}

func (d *zoneDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m zoneDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	z, err := d.data.client.GetDomain(ctx, m.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Reading zone failed", err.Error())
		return
	}
	m.Status = types.StringValue(z.Status)
	m.Type = types.StringValue(z.Type)
	m.Serial = types.Int64Value(int64(z.Serial))
	m.RecordCount = types.Int64Value(int64(z.RRCount))
	ns := make([]attr.Value, 0, len(z.NS))
	for _, n := range z.NS {
		ns = append(ns, types.StringValue(n))
	}
	m.NameServers = types.ListValueMust(types.StringType, ns)
	resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
}
