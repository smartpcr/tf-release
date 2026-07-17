package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// exactlyOneOfSpecValidator enforces DESIGN §14 VAL-06 at the Terraform
// schema/config layer: exactly one of `spec` (inline) or `spec_file` (path)
// must be set. Terraform surfaces this during plan/validate — before any spec
// parse or dial — as an [ERR_SPEC_INVALID] diagnostic, rather than relying on
// the runtime resolveSpec fallback.
type exactlyOneOfSpecValidator struct{}

func (exactlyOneOfSpecValidator) Description(_ context.Context) string {
	return "exactly one of spec or spec_file must be set"
}

func (v exactlyOneOfSpecValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (exactlyOneOfSpecValidator) ValidateResource(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var specVal, specFileVal types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("spec"), &specVal)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("spec_file"), &specFileVal)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Unknown values (interpolated from other resources/vars) can't be judged
	// yet; defer to a later plan/apply round.
	if specVal.IsUnknown() || specFileVal.IsUnknown() {
		return
	}
	hasSpec := !specVal.IsNull() && specVal.ValueString() != ""
	hasFile := !specFileVal.IsNull() && specFileVal.ValueString() != ""
	if hasSpec == hasFile {
		resp.Diagnostics.AddError(
			"[ERR_SPEC_INVALID] exactly one of spec/spec_file must be set",
			"Set exactly one of `spec` (inline YAML/JSON) or `spec_file` (path on the runner).",
		)
	}
}
