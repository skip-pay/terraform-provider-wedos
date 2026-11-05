package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/skip-pay/terraform-provider-wedos/internal/client"
)

var (
	_ resource.Resource                   = (*recordSetResource)(nil)
	_ resource.ResourceWithConfigure      = (*recordSetResource)(nil)
	_ resource.ResourceWithImportState    = (*recordSetResource)(nil)
	_ resource.ResourceWithValidateConfig = (*recordSetResource)(nil)
)

func (r *recordSetResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg recordSetModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() || cfg.Name.IsUnknown() || cfg.Type.IsUnknown() || cfg.Records.IsUnknown() || cfg.Records.IsNull() {
		return
	}
	var values []string
	resp.Diagnostics.Append(cfg.Records.ElementsAs(ctx, &values, true)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if msg := validateRecordSet(cfg.Name.ValueString(), cfg.Type.ValueString(), values); msg != "" {
		resp.Diagnostics.AddAttributeError(path.Root("records"), "Invalid record set", msg)
	}
}

func NewRecordSetResource() resource.Resource { return &recordSetResource{} }

// recordSetResource manages all rows of one (zone, name, type) — the Route 53
// record set shape. WEDOS stores one row per value with a server-assigned ID;
// the resource never stores those IDs, it re-resolves them from the zone's row
// list on every operation, so the identity is the natural key like in
// aws_route53_record.
type recordSetResource struct{ data *providerData }

type recordSetModel struct {
	ID             types.String `tfsdk:"id"`
	Zone           types.String `tfsdk:"zone"`
	Name           types.String `tfsdk:"name"`
	Type           types.String `tfsdk:"type"`
	TTL            types.Int64  `tfsdk:"ttl"`
	Records        types.Set    `tfsdk:"records"`
	AuthorComment  types.String `tfsdk:"author_comment"`
	AllowOverwrite types.Bool   `tfsdk:"allow_overwrite"`
	FQDN           types.String `tfsdk:"fqdn"`
}

func (r *recordSetResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_record_set"
}

func (r *recordSetResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "All values of one name and type in a WEDOS DNS zone. Each value is stored by WEDOS as a separate row.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "zone/name/type, with @ for the apex.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"zone": schema.StringAttribute{
				Description:   "Name of the zone the record set belongs to.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{domainNameValidator{}},
			},
			"name": schema.StringAttribute{
				Required:      true,
				Description:   `Name relative to the zone; "" for the apex.`,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{recordNameValidator{}},
			},
			"type": schema.StringAttribute{
				Description:   "Record type.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.OneOf(client.RecordTypes...)},
			},
			"ttl": schema.Int64Attribute{
				Required:    true,
				Description: "Seconds, 300–172800 (the range WEDOS accepts).",
				Validators:  []validator.Int64{int64validator.Between(minTTL, maxTTL)},
			},
			"records": schema.SetAttribute{
				Required:    true,
				ElementType: types.StringType,
				Description: "Record data in WEDOS rdata format, one element per value.",
				Validators:  []validator.Set{setvalidator.SizeAtLeast(1), setvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1))},
			},
			"author_comment": schema.StringAttribute{
				Optional:    true,
				Description: "Comment stored on every row. ASCII letters, digits and spaces only.",
				Validators:  []validator.String{stringvalidator.LengthAtLeast(1), authorCommentValidator{}},
			},
			"allow_overwrite": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Adopt rows that already exist for this name and type on create, instead of failing.",
			},
			"fqdn": schema.StringAttribute{
				Description:   "Fully qualified name of the record set.",
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *recordSetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func recordSetID(zone, name, typ string) string {
	if name == "" {
		name = "@"
	}
	return zone + "/" + name + "/" + typ
}

func fqdn(zone, name string) string {
	if name == "" {
		return zone
	}
	return name + "." + zone
}

func desiredFrom(ctx context.Context, m recordSetModel) (desiredSet, diag.Diagnostics) {
	d := desiredSet{
		name:    m.Name.ValueString(),
		typ:     m.Type.ValueString(),
		ttl:     m.TTL.ValueInt64(),
		comment: m.AuthorComment.ValueString(),
	}
	diags := m.Records.ElementsAs(ctx, &d.values, false)
	sort.Strings(d.values)
	return d, diags
}

func flatten(m *recordSetModel, zone, name, typ string, rows []client.Row) {
	m.ID = types.StringValue(recordSetID(zone, name, typ))
	m.Zone = types.StringValue(zone)
	m.Name = types.StringValue(name)
	m.Type = types.StringValue(typ)
	m.FQDN = types.StringValue(fqdn(zone, name))
	var preferred []string
	if !m.Records.IsNull() && !m.Records.IsUnknown() {
		_ = m.Records.ElementsAs(context.Background(), &preferred, false)
	}
	o := observe(rows, preferred)
	m.Records, m.TTL, m.AuthorComment = o.values, o.ttl, o.comment
}

// apply converges the rows and fills plan from what WEDOS holds afterwards.
// written reports that plan reflects existing rows even when diags carries an
// error, so a failed create still records them in state (the resource is then
// tainted and replaced) instead of leaving them orphaned.
func (r *recordSetResource) apply(ctx context.Context, plan *recordSetModel, creating bool) (diags diag.Diagnostics, written bool) {
	d, ds := desiredFrom(ctx, *plan)
	diags.Append(ds...)
	if diags.HasError() {
		return diags, false
	}

	zone := plan.Zone.ValueString()
	tx := r.data.zones.Lock(zone)
	defer tx.Unlock()

	existing, err := tx.Find(ctx, d.name, d.typ)
	if err != nil {
		diags.AddError("Listing zone rows failed", err.Error())
		return diags, false
	}
	if creating && len(existing) > 0 && !plan.AllowOverwrite.ValueBool() {
		diags.AddError("Record set already exists",
			fmt.Sprintf("%s already has %d %s row(s) in WEDOS. Import the record set with: terraform import <address> %s, or set allow_overwrite = true.",
				fqdn(zone, d.name), len(existing), d.typ, recordSetID(zone, d.name, d.typ)))
		return diags, false
	}

	sp := syncPlan{zone: zone}
	sp.addSet(existing, d)
	convergeErr := sp.execute(ctx, tx)
	// Commit even after a partial failure: the rows that were written will be
	// published by the WEDOS autocommit anyway, and committing now keeps the
	// published zone in step with what the next refresh will report.
	addCommitDiag(&diags, tx.Commit(ctx))
	if convergeErr != nil {
		diags.AddError("Writing record set failed", convergeErr.Error())
	}

	rows, err := tx.Find(ctx, d.name, d.typ)
	if err != nil {
		diags.AddError("Reading record set failed", err.Error())
		return diags, false
	}
	if len(rows) == 0 {
		if convergeErr == nil {
			diags.AddError("Record set not found after write", recordSetID(zone, d.name, d.typ))
		}
		return diags, false
	}
	flatten(plan, zone, d.name, d.typ, rows)
	return diags, true
}

func (r *recordSetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan recordSetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	diags, written := r.apply(ctx, &plan, true)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() && !written {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *recordSetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state recordSetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	zone, name, typ := state.Zone.ValueString(), state.Name.ValueString(), state.Type.ValueString()

	tx := r.data.zones.Lock(zone)
	rows, err := tx.Find(ctx, name, typ)
	tx.Unlock()
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Reading record set failed", err.Error())
		return
	}
	if len(rows) == 0 {
		resp.State.RemoveResource(ctx)
		return
	}
	flatten(&state, zone, name, typ, rows)
	if state.AllowOverwrite.IsNull() {
		state.AllowOverwrite = types.BoolValue(false)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *recordSetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan recordSetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	diags, written := r.apply(ctx, &plan, false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() && !written {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, plan)...)
}

func (r *recordSetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state recordSetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	zone, name, typ := state.Zone.ValueString(), state.Name.ValueString(), state.Type.ValueString()

	tx := r.data.zones.Lock(zone)
	defer tx.Unlock()
	rows, err := tx.Find(ctx, name, typ)
	if err != nil {
		if client.IsNotFound(err) {
			return
		}
		resp.Diagnostics.AddError("Listing zone rows failed", err.Error())
		return
	}
	var deleteErr error
	for _, row := range rows {
		if deleteErr = tx.Delete(ctx, int64(row.ID)); deleteErr != nil {
			break
		}
	}
	addCommitDiag(&resp.Diagnostics, tx.Commit(ctx))
	if deleteErr != nil {
		resp.Diagnostics.AddError("Deleting record set failed", deleteErr.Error())
	}
}

// ImportState accepts zone/name/type with @ for the apex, e.g.
// example.cz/www/A or example.cz/@/MX.
func (r *recordSetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		resp.Diagnostics.AddError("Invalid import ID", `expected zone/name/type with "@" for the apex, e.g. example.cz/www/A`)
		return
	}
	zone, name, typ := strings.ToLower(parts[0]), strings.ToLower(parts[1]), strings.ToUpper(parts[2])
	if name == "@" {
		name = ""
	}
	if !domainNameRe.MatchString(zone) || !recordNameRe.MatchString(name) {
		resp.Diagnostics.AddError("Invalid import ID", `expected zone/name/type with "@" for the apex, e.g. example.cz/www/A`)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), recordSetID(zone, name, typ))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("zone"), zone)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("type"), typ)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("allow_overwrite"), false)...)
}
