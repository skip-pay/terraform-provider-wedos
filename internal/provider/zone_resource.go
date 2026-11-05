package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/skip-pay/terraform-provider-wedos/internal/client"
)

var (
	_ resource.Resource                = (*zoneResource)(nil)
	_ resource.ResourceWithConfigure   = (*zoneResource)(nil)
	_ resource.ResourceWithImportState = (*zoneResource)(nil)
)

func NewZoneResource() resource.Resource { return &zoneResource{} }

type zoneResource struct{ data *providerData }

type zoneModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	AXFRIPs     types.Set    `tfsdk:"axfr_ips"`
	Status      types.String `tfsdk:"status"`
	NameServers types.List   `tfsdk:"name_servers"`
}

func (r *zoneResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_zone"
}

func (r *zoneResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A primary DNS zone in WEDOS DNS. Deleting the resource deletes the zone with all its records.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   "Zone name, e.g. example.cz.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{domainNameValidator{}},
			},
			"axfr_ips": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "IP addresses allowed to transfer the zone (outgoing AXFR). Unset disables AXFR.",
				Validators:  []validator.Set{setvalidator.SizeAtLeast(1)},
			},
			"status": schema.StringAttribute{
				Description:   "Zone status reported by WEDOS, e.g. active.",
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name_servers": schema.ListAttribute{
				Computed:      true,
				ElementType:   types.StringType,
				Description:   "Authoritative name servers assigned by WEDOS; use them for the delegation at the registrar.",
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *zoneResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *providerData, got %T", req.ProviderData))
		return
	}
	r.data = data
}

func axfrSettings(ctx context.Context, set types.Set) (*int, []string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if set.IsNull() {
		zero := 0
		return &zero, nil, diags
	}
	var ips []string
	diags.Append(set.ElementsAs(ctx, &ips, false)...)
	sort.Strings(ips)
	one := 1
	return &one, ips, diags
}

func (r *zoneResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan zoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	enabled, ips, diags := axfrSettings(ctx, plan.AXFRIPs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	in := client.DomainCreate{Name: plan.Name.ValueString(), Type: "primary"}
	if *enabled == 1 {
		in.AXFREnabled, in.AXFRIPs = enabled, ips
	}
	if err := r.data.client.AddDomain(ctx, in); err != nil {
		if client.HasCode(err, 3303) {
			resp.Diagnostics.AddError("Zone already exists",
				fmt.Sprintf("Zone %s already exists in WEDOS DNS. Import it with: terraform import <address> %s", in.Name, in.Name))
			return
		}
		resp.Diagnostics.AddError("Creating zone failed", err.Error())
		return
	}
	// The zone exists from here on: read it before committing, so that a failed
	// commit still records it in state (tainted) instead of orphaning it.
	resp.Diagnostics.Append(r.read(ctx, in.Name, &plan)...)
	if resp.Diagnostics.HasError() || plan.ID.IsNull() {
		return
	}
	// A new zone is staged like a row change (changed=1, no commit_date) and
	// is not served until it is committed.
	tx := r.data.zones.Lock(in.Name)
	tx.MarkDirty()
	addCommitDiag(&resp.Diagnostics, tx.Commit(ctx))
	tx.Unlock()
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

// read refreshes m from the API. It returns found=false through m.ID being null.
func (r *zoneResource) read(ctx context.Context, name string, m *zoneModel) diag.Diagnostics {
	var diags diag.Diagnostics
	d, err := r.data.client.GetDomain(ctx, name)
	if err != nil {
		if client.IsNotFound(err) {
			m.ID = types.StringNull()
			return diags
		}
		diags.AddError("Reading zone failed", err.Error())
		return diags
	}
	m.ID = types.StringValue(d.Name)
	m.Name = types.StringValue(d.Name)
	m.Status = types.StringValue(d.Status)

	ns := make([]attr.Value, 0, len(d.NS))
	for _, n := range d.NS {
		ns = append(ns, types.StringValue(n))
	}
	m.NameServers = types.ListValueMust(types.StringType, ns)

	if d.AXFREnabled == 1 {
		var ips []attr.Value
		for _, ip := range d.AXFRIPs {
			ips = append(ips, types.StringValue(ip))
		}
		m.AXFRIPs = types.SetValueMust(types.StringType, ips)
	} else {
		m.AXFRIPs = types.SetNull(types.StringType)
	}
	return diags
}

func (r *zoneResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state zoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.read(ctx, state.ID.ValueString(), &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if state.ID.IsNull() {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *zoneResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan zoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	enabled, ips, diags := axfrSettings(ctx, plan.AXFRIPs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.data.client.UpdateDomain(ctx, plan.Name.ValueString(), client.DomainUpdate{AXFREnabled: enabled, AXFRIPs: ips}); err != nil {
		resp.Diagnostics.AddError("Updating zone failed", err.Error())
		return
	}
	resp.Diagnostics.Append(r.read(ctx, plan.Name.ValueString(), &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *zoneResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state zoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.ID.ValueString()
	tx := r.data.zones.Lock(name)
	defer tx.Unlock()
	if err := r.data.client.DeleteDomain(ctx, name); err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Deleting zone failed", err.Error())
		return
	}
	tx.Forget()
}

func (r *zoneResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	zone := strings.ToLower(req.ID)
	if !domainNameRe.MatchString(zone) {
		resp.Diagnostics.AddError("Invalid import ID", "expected a zone name, e.g. example.cz")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), zone)...)
}
