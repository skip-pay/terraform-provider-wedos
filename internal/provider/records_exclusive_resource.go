package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/skip-pay/terraform-provider-wedos/internal/client"
)

var (
	_ resource.Resource                   = (*recordsExclusiveResource)(nil)
	_ resource.ResourceWithConfigure      = (*recordsExclusiveResource)(nil)
	_ resource.ResourceWithImportState    = (*recordsExclusiveResource)(nil)
	_ resource.ResourceWithValidateConfig = (*recordsExclusiveResource)(nil)
)

func NewRecordsExclusiveResource() resource.Resource { return &recordsExclusiveResource{} }

// recordsExclusiveResource owns every record in a zone. One apply lists the
// zone once, plans all row writes, runs them under one zone lock and commits
// once — the shape that fits the WEDOS staged-commit model, and the WEDOS
// counterpart of aws_route53_records_exclusive.
//
// Apex NS rows are never managed: they belong to the zone's delegation and are
// left untouched, like Route 53 apex NS/SOA.
type recordsExclusiveResource struct{ data *providerData }

type recordsExclusiveModel struct {
	ID         types.String `tfsdk:"id"`
	Zone       types.String `tfsdk:"zone"`
	RecordSets types.Set    `tfsdk:"record_sets"`
}

type exclusiveSetModel struct {
	Name          types.String `tfsdk:"name"`
	Type          types.String `tfsdk:"type"`
	TTL           types.Int64  `tfsdk:"ttl"`
	Records       types.Set    `tfsdk:"records"`
	AuthorComment types.String `tfsdk:"author_comment"`
}

var exclusiveSetType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"name":           types.StringType,
	"type":           types.StringType,
	"ttl":            types.Int64Type,
	"records":        types.SetType{ElemType: types.StringType},
	"author_comment": types.StringType,
}}

func (r *recordsExclusiveResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_records_exclusive"
}

func (r *recordsExclusiveResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Exclusive management of every record in a WEDOS DNS zone. Records not in record_sets are deleted. " +
			"Apex NS records are ignored. Destroying the resource stops management and leaves the records in place. " +
			"Do not combine with wedos_dns_record_set on the same zone.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"zone": schema.StringAttribute{
				Description:   "Name of the zone whose records this resource owns.",
				Required:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{domainNameValidator{}},
			},
			"record_sets": schema.SetNestedAttribute{
				Required:    true,
				Description: "Every record set of the zone. An empty set deletes all records except apex NS.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Required:    true,
							Description: `Name relative to the zone; "" for the apex.`,
							Validators:  []validator.String{recordNameValidator{}},
						},
						"type": schema.StringAttribute{
							Description: "Record type.",
							Required:    true,
							Validators:  []validator.String{stringvalidator.OneOf(client.RecordTypes...)},
						},
						"ttl": schema.Int64Attribute{
							Description: "TTL in seconds, 300 to 172800, applied to every value.",
							Required:    true,
							Validators:  []validator.Int64{int64validator.Between(minTTL, maxTTL)},
						},
						"records": schema.SetAttribute{
							Description: "Record values in WEDOS rdata format, one element per value.",
							Required:    true,
							ElementType: types.StringType,
							Validators:  []validator.Set{setvalidator.SizeAtLeast(1), setvalidator.ValueStringsAre(stringvalidator.LengthAtLeast(1))},
						},
						"author_comment": schema.StringAttribute{
							Description: "Comment stored on every value. ASCII letters, digits and spaces only.",
							Optional:    true,
							Validators:  []validator.String{stringvalidator.LengthAtLeast(1), authorCommentValidator{}},
						},
					},
				},
			},
		},
	}
}

func (r *recordsExclusiveResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

type setKey struct{ name, typ string }

func keyOf(name, typ string) setKey { return setKey{strings.ToLower(name), strings.ToUpper(typ)} }

func isApexNS(k setKey) bool { return k.name == "" && k.typ == "NS" }

func (r *recordsExclusiveResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg recordsExclusiveModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() || cfg.RecordSets.IsNull() || cfg.RecordSets.IsUnknown() {
		return
	}
	var sets []exclusiveSetModel
	resp.Diagnostics.Append(cfg.RecordSets.ElementsAs(ctx, &sets, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	seen := map[setKey]bool{}
	typesByName := map[string][]string{}
	for _, s := range sets {
		if s.Name.IsUnknown() || s.Type.IsUnknown() {
			continue
		}
		k := keyOf(s.Name.ValueString(), s.Type.ValueString())
		label := fqdnLabel(s.Name.ValueString(), k.typ)
		if isApexNS(k) {
			resp.Diagnostics.AddAttributeError(path.Root("record_sets"), "Apex NS is not managed",
				"The zone's apex NS records belong to WEDOS and are ignored by this resource; remove "+label+" from record_sets.")
		}
		if seen[k] {
			resp.Diagnostics.AddAttributeError(path.Root("record_sets"), "Duplicate record set",
				label+" is defined more than once; put all its values into one record set.")
		}
		seen[k] = true
		for _, other := range typesByName[k.name] {
			if typesConflict(other, k.typ) {
				resp.Diagnostics.AddAttributeError(path.Root("record_sets"), "Conflicting record sets",
					fmt.Sprintf("%s cannot coexist with %s at the same name (a CNAME excludes every other type; an ALIAS excludes A and AAAA).", label, fqdnLabel(s.Name.ValueString(), other)))
			}
		}
		typesByName[k.name] = append(typesByName[k.name], k.typ)
		if s.Records.IsUnknown() || s.Records.IsNull() {
			continue
		}
		var values []string
		resp.Diagnostics.Append(s.Records.ElementsAs(ctx, &values, true)...)
		if msg := validateRecordSet(s.Name.ValueString(), k.typ, values); msg != "" {
			resp.Diagnostics.AddAttributeError(path.Root("record_sets"), "Invalid record set", label+": "+msg)
		}
	}
}

func fqdnLabel(name, typ string) string {
	if name == "" {
		name = "@"
	}
	return name + " " + typ
}

func desiredSets(ctx context.Context, m recordsExclusiveModel) ([]desiredSet, diag.Diagnostics) {
	var sets []exclusiveSetModel
	diags := m.RecordSets.ElementsAs(ctx, &sets, false)
	out := make([]desiredSet, 0, len(sets))
	for _, s := range sets {
		d := desiredSet{
			name:    strings.ToLower(s.Name.ValueString()),
			typ:     strings.ToUpper(s.Type.ValueString()),
			ttl:     s.TTL.ValueInt64(),
			comment: s.AuthorComment.ValueString(),
		}
		diags.Append(s.Records.ElementsAs(ctx, &d.values, false)...)
		sort.Strings(d.values)
		out = append(out, d)
	}
	return out, diags
}

// groupRows groups the zone's rows by (name, type), dropping apex NS.
func groupRows(rows []client.Row) (map[setKey][]client.Row, []setKey) {
	groups := map[setKey][]client.Row{}
	for _, row := range rows {
		k := keyOf(row.Name, row.Type)
		if isApexNS(k) {
			continue
		}
		groups[k] = append(groups[k], row)
	}
	keys := make([]setKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name != keys[j].name {
			return keys[i].name < keys[j].name
		}
		return keys[i].typ < keys[j].typ
	})
	return groups, keys
}

// preferredValues returns the configured spelling of every record set's
// values in m (plan or prior state), for observe().
func preferredValues(ctx context.Context, m recordsExclusiveModel) map[setKey][]string {
	out := map[setKey][]string{}
	if m.RecordSets.IsNull() || m.RecordSets.IsUnknown() {
		return out
	}
	var sets []exclusiveSetModel
	if diags := m.RecordSets.ElementsAs(ctx, &sets, false); diags.HasError() {
		return out
	}
	for _, s := range sets {
		var values []string
		_ = s.Records.ElementsAs(ctx, &values, false)
		k := keyOf(s.Name.ValueString(), s.Type.ValueString())
		out[k] = append(out[k], values...)
	}
	return out
}

func (r *recordsExclusiveResource) flatten(ctx context.Context, zone string, rows []client.Row, preferred map[setKey][]string) (recordsExclusiveModel, diag.Diagnostics) {
	groups, keys := groupRows(rows)
	sets := make([]exclusiveSetModel, 0, len(keys))
	for _, k := range keys {
		o := observe(groups[k], preferred[k])
		sets = append(sets, exclusiveSetModel{
			Name: types.StringValue(k.name), Type: types.StringValue(k.typ),
			TTL: o.ttl, Records: o.values, AuthorComment: o.comment,
		})
	}
	set, diags := types.SetValueFrom(ctx, exclusiveSetType, sets)
	return recordsExclusiveModel{ID: types.StringValue(zone), Zone: types.StringValue(zone), RecordSets: set}, diags
}

// sync converges the whole zone to the plan. On create, rows outside the
// configuration are an error rather than silently deleted, so every deletion
// this resource performs has been visible in a plan.
func (r *recordsExclusiveResource) sync(ctx context.Context, plan recordsExclusiveModel, creating bool) (recordsExclusiveModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	zone := plan.Zone.ValueString()
	desired, ds := desiredSets(ctx, plan)
	diags.Append(ds...)
	if diags.HasError() {
		return plan, diags
	}

	tx := r.data.zones.Lock(zone)
	defer tx.Unlock()
	rows, err := tx.All(ctx)
	if err != nil {
		diags.AddError("Listing zone rows failed", err.Error())
		return plan, diags
	}
	groups, keys := groupRows(rows)

	wanted := map[setKey]bool{}
	for _, d := range desired {
		wanted[keyOf(d.name, d.typ)] = true
	}

	if creating {
		var unmanaged []string
		for _, k := range keys {
			if !wanted[k] {
				unmanaged = append(unmanaged, fqdnLabel(k.name, k.typ))
			}
		}
		if len(unmanaged) > 0 {
			diags.AddError("Zone has records outside the configuration",
				fmt.Sprintf("%s has %d record set(s) not in record_sets: %s.\nImport the zone first so the next plan shows what would be deleted: terraform import <address> %s",
					zone, len(unmanaged), strings.Join(unmanaged, ", "), zone))
			return plan, diags
		}
	}

	sp := syncPlan{zone: zone}
	for _, d := range desired {
		sp.addSet(groups[keyOf(d.name, d.typ)], d)
	}
	for _, k := range keys {
		if !wanted[k] {
			sp.removeRows(groups[k])
		}
	}

	syncErr := sp.execute(ctx, tx)
	// See recordSetResource.apply: commit whatever was written, even on failure.
	addCommitDiag(&diags, tx.Commit(ctx))
	if syncErr != nil {
		diags.AddError("Writing zone records failed", syncErr.Error())
		return plan, diags
	}

	rows, err = tx.All(ctx)
	if err != nil {
		diags.AddError("Reading zone rows failed", err.Error())
		return plan, diags
	}
	state, ds := r.flatten(ctx, zone, rows, preferredValues(ctx, plan))
	diags.Append(ds...)
	return state, diags
}

func (r *recordsExclusiveResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan recordsExclusiveModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state, diags := r.sync(ctx, plan, true)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *recordsExclusiveResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var prior recordsExclusiveModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	zone := prior.ID.ValueString()

	tx := r.data.zones.Lock(zone)
	rows, err := tx.All(ctx)
	tx.Unlock()
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Reading zone rows failed", err.Error())
		return
	}
	state, diags := r.flatten(ctx, zone, rows, preferredValues(ctx, prior))
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

func (r *recordsExclusiveResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan recordsExclusiveModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state, diags := r.sync(ctx, plan, false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// Delete only stops management; records stay in the zone. Deleting the zone
// itself is wedos_dns_zone's job.
func (r *recordsExclusiveResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}

func (r *recordsExclusiveResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	zone := strings.ToLower(req.ID)
	if !domainNameRe.MatchString(zone) {
		resp.Diagnostics.AddError("Invalid import ID", "expected a zone name, e.g. example.cz")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), zone)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("zone"), zone)...)
}
