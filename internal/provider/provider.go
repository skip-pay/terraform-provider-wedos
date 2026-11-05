package provider

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/skip-pay/terraform-provider-wedos/internal/client"
)

var _ provider.Provider = (*wedosProvider)(nil)

type wedosProvider struct {
	version string
}

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &wedosProvider{version: version} }
}

type providerModel struct {
	BaseURL       types.String `tfsdk:"base_url"`
	ClientID      types.String `tfsdk:"client_id"`
	ClientSecret  types.String `tfsdk:"client_secret"`
	AccessToken   types.String `tfsdk:"access_token"`
	Commit        types.Bool   `tfsdk:"commit"`
	WaitForReload types.Bool   `tfsdk:"wait_for_reload"`
	ReloadTimeout types.String `tfsdk:"reload_timeout"`
}

const (
	defaultReloadTimeout = 2 * time.Minute
	defaultPollInterval  = 3 * time.Second
)

func (p *wedosProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "wedos"
	resp.Version = p.version
}

func (p *wedosProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages WEDOS DNS (WDNS) zones and records through the WEDOS HTTP REST API v2.",
		Attributes: map[string]schema.Attribute{
			"base_url": schema.StringAttribute{
				Optional:    true,
				Description: "API base URL. Defaults to WEDOS_BASE_URL or https://api.wedos.com.",
			},
			"client_id": schema.StringAttribute{
				Optional:    true,
				Description: "OAuth2 client ID of the API client. Defaults to WEDOS_CLIENT_ID.",
			},
			"client_secret": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "OAuth2 client secret. Defaults to WEDOS_CLIENT_SECRET.",
			},
			"access_token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Static bearer token generated in the WEDOS admin, used instead of client credentials. Defaults to WEDOS_ACCESS_TOKEN.",
			},
			"commit": schema.BoolAttribute{
				Optional:    true,
				Description: "Commit the zone after every resource write (default true). With false, changes are published by the WEDOS autocommit about five minutes after the last change.",
			},
			"wait_for_reload": schema.BoolAttribute{
				Optional: true,
				Description: "After each commit, wait until the WEDOS API reports the commit processed (default false). " +
					"Typically 7–10 s and 3–5 extra API requests per commit. This does not mean the name servers already " +
					"serve the change: WEDOS publishes in a 5-minute cycle, measured 1–7.5 minutes until all name servers have it. Ignored when commit = false.",
			},
			"reload_timeout": schema.StringAttribute{
				Optional:    true,
				Description: "How long to wait for the reload, as a Go duration (default 2m). A timeout is reported as a warning: the change is committed and WEDOS reloads it later.",
			},
		},
	}
}

func stringOrEnv(v types.String, env string) string {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueString()
	}
	return os.Getenv(env)
}

func (p *wedosProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	c, err := client.New(client.Config{
		BaseURL:      stringOrEnv(cfg.BaseURL, "WEDOS_BASE_URL"),
		ClientID:     stringOrEnv(cfg.ClientID, "WEDOS_CLIENT_ID"),
		ClientSecret: stringOrEnv(cfg.ClientSecret, "WEDOS_CLIENT_SECRET"),
		AccessToken:  stringOrEnv(cfg.AccessToken, "WEDOS_ACCESS_TOKEN"),
		UserAgent:    "terraform-provider-wedos/" + p.version,
	})
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("client_id"), "Missing WEDOS credentials", err.Error())
		return
	}

	policy, diags := commitPolicyFrom(cfg)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data := &providerData{client: c, zones: newZoneStore(c, policy)}
	resp.ResourceData = data
	resp.DataSourceData = data
}

func (p *wedosProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewZoneResource,
		NewRecordSetResource,
		NewRecordsExclusiveResource,
	}
}

func (p *wedosProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewZoneDataSource,
	}
}

// providerData is shared by every resource and data source of one provider
// instance, i.e. one Terraform operation.
type providerData struct {
	client *client.Client
	zones  *zoneStore
}

func commitPolicyFrom(cfg providerModel) (commitPolicy, diag.Diagnostics) {
	var diags diag.Diagnostics
	p := commitPolicy{commit: true, reloadTimeout: defaultReloadTimeout, pollInterval: defaultPollInterval}
	if !cfg.Commit.IsNull() && !cfg.Commit.IsUnknown() {
		p.commit = cfg.Commit.ValueBool()
	}
	if !cfg.WaitForReload.IsNull() && !cfg.WaitForReload.IsUnknown() {
		p.waitReload = cfg.WaitForReload.ValueBool() && p.commit
	}
	if !cfg.ReloadTimeout.IsNull() && !cfg.ReloadTimeout.IsUnknown() {
		d, err := time.ParseDuration(cfg.ReloadTimeout.ValueString())
		if err != nil || d <= 0 {
			diags.AddAttributeError(path.Root("reload_timeout"), "Invalid reload_timeout", fmt.Sprintf("%q is not a positive Go duration such as 90s or 2m", cfg.ReloadTimeout.ValueString()))
			return p, diags
		}
		p.reloadTimeout = d
	}
	// Test knob: the acceptance tests run against a fake that reloads after a
	// few polls and should not sleep 3 s per poll.
	if v := os.Getenv("WEDOS_RELOAD_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			p.pollInterval = d
		}
	}
	return p, diags
}

// addCommitDiag turns the result of zoneTx.Commit into diagnostics: a reload
// timeout is a warning, anything else an error.
func addCommitDiag(diags *diag.Diagnostics, err error) {
	switch {
	case err == nil:
	case IsReloadTimeout(err):
		diags.AddWarning("Zone reload not confirmed", err.Error())
	default:
		diags.AddError("Committing zone failed", err.Error())
	}
}
