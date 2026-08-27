//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/provider"
)

// e2eDeploymentModel mirrors the (unexported) provider.deploymentModel tfsdk shape
// so the e2e package — which lives outside internal/provider — can write and read
// state through the REAL resource schema by tfsdk tag. The framework maps by tag,
// not by Go type identity, so this is a faithful stand-in for driving Schema/state
// round-trips against the impl (DESIGN §5.2).
type e2eDeploymentModel struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	Spec            types.String `tfsdk:"spec"`
	SpecFile        types.String `tfsdk:"spec_file"`
	Variables       types.Map    `tfsdk:"variables"`
	VersionOverride types.String `tfsdk:"version_override"`
	DestroyMode     types.String `tfsdk:"destroy_mode"`
	SpecHash        types.String `tfsdk:"spec_hash"`
	ResolvedSpec    types.String `tfsdk:"resolved_spec"`
	DeployedVersion types.String `tfsdk:"deployed_version"`
	PreviousVersion types.String `tfsdk:"previous_version"`
	Hosts           types.List   `tfsdk:"hosts"`
	ReleasePath     types.String `tfsdk:"release_path"`
	ServiceStatus   types.String `tfsdk:"service_status"`
}

// schemaState holds per-scenario inputs and outputs.
type schemaState struct {
	spec     types.String
	specFile types.String

	validationErr bool
	errSummaries  []string

	schema   schema.Schema
	original *e2eDeploymentModel
	got      *e2eDeploymentModel
}

func newResourceSchema() schema.Schema {
	sr := resource.SchemaResponse{}
	(&provider.DeploymentResource{}).Schema(context.Background(), resource.SchemaRequest{}, &sr)
	return sr.Schema
}

// buildConfig materialises a tfsdk.Config from the REAL resource schema with the
// scenario's spec/spec_file values, exactly as the framework hands it to the
// ConfigValidator during `terraform validate`.
func buildConfig(sch schema.Schema, specVal, specFileVal types.String) (tfsdk.Config, error) {
	m := &e2eDeploymentModel{
		Spec:      specVal,
		SpecFile:  specFileVal,
		Variables: types.MapNull(types.StringType),
		Hosts:     types.ListNull(types.StringType),
	}
	st := tfsdk.State{Schema: sch}
	if diags := st.Set(context.Background(), m); diags.HasError() {
		return tfsdk.Config{}, fmt.Errorf("build config: %v", diags)
	}
	return tfsdk.Config{Schema: sch, Raw: st.Raw}, nil
}

// --- One-of validation steps ----------------------------------------------

func (s *schemaState) givenBothSet() error {
	s.spec = types.StringValue("kind: Deployment")
	s.specFile = types.StringValue("/x/spec.yaml")
	return nil
}

func (s *schemaState) givenBothEmpty() error {
	s.spec = types.StringValue("")
	s.specFile = types.StringValue("")
	return nil
}

func (s *schemaState) givenNeitherSet() error {
	s.spec = types.StringNull()
	s.specFile = types.StringNull()
	return nil
}

func (s *schemaState) givenOnlySpec() error {
	s.spec = types.StringValue("kind: Deployment")
	s.specFile = types.StringNull()
	return nil
}

func (s *schemaState) givenOnlySpecFile() error {
	s.spec = types.StringNull()
	s.specFile = types.StringValue("/x/spec.yaml")
	return nil
}

func (s *schemaState) whenValidatorsRun() error {
	res := &provider.DeploymentResource{}
	sch := newResourceSchema()
	cfg, err := buildConfig(sch, s.spec, s.specFile)
	if err != nil {
		return err
	}
	req := resource.ValidateConfigRequest{Config: cfg}
	resp := &resource.ValidateConfigResponse{}

	validators := res.ConfigValidators(context.Background())
	if len(validators) == 0 {
		return fmt.Errorf("resource advertises no ConfigValidators; VAL-06 exactly-one-of enforcement is not wired in")
	}
	for _, v := range validators {
		v.ValidateResource(context.Background(), req, resp)
	}

	s.validationErr = resp.Diagnostics.HasError()
	s.errSummaries = nil
	for _, d := range resp.Diagnostics.Errors() {
		s.errSummaries = append(s.errSummaries, d.Summary())
	}
	return nil
}

func (s *schemaState) thenValidationFailsWith(substr string) error {
	if !s.validationErr {
		return fmt.Errorf("expected a validation error containing %q, got none", substr)
	}
	for _, sum := range s.errSummaries {
		if strings.Contains(sum, substr) {
			return nil
		}
	}
	return fmt.Errorf("no validation error mentions %q; got %v", substr, s.errSummaries)
}

func (s *schemaState) thenValidationSucceeds() error {
	if s.validationErr {
		return fmt.Errorf("expected validation to succeed, got errors %v", s.errSummaries)
	}
	return nil
}

// --- Schema round-trip steps ----------------------------------------------

func (s *schemaState) givenFullyPopulatedState() error {
	s.original = &e2eDeploymentModel{
		ID:              types.StringValue("dep-123"),
		Name:            types.StringValue("payments-svc"),
		Spec:            types.StringValue("kind: Deployment"),
		SpecFile:        types.StringNull(),
		Variables:       types.MapValueMust(types.StringType, map[string]attr.Value{"ENV": types.StringValue("prod")}),
		VersionOverride: types.StringValue("2.0.0"),
		DestroyMode:     types.StringValue("purge"),
		SpecHash:        types.StringValue("abc123"),
		ResolvedSpec:    types.StringValue("kind: Deployment\nresolved: true"),
		DeployedVersion: types.StringValue("2.0.0"),
		PreviousVersion: types.StringValue("1.0.0"),
		Hosts:           types.ListValueMust(types.StringType, []attr.Value{types.StringValue("lab-01"), types.StringValue("lab-02")}),
		ReleasePath:     types.StringValue(`C:\releases\dep-123`),
		ServiceStatus:   types.StringValue("running"),
	}
	return nil
}

func (s *schemaState) whenStateWrittenThenRead() error {
	sch := newResourceSchema()
	s.schema = sch

	st := tfsdk.State{Schema: sch}
	if diags := st.Set(context.Background(), s.original); diags.HasError() {
		return fmt.Errorf("write state: %v", diags)
	}
	var got e2eDeploymentModel
	if diags := st.Get(context.Background(), &got); diags.HasError() {
		return fmt.Errorf("read state: %v", diags)
	}
	s.got = &got
	return nil
}

func (s *schemaState) thenRoundTripsWithoutLoss() error {
	if s.original == nil || s.got == nil {
		return fmt.Errorf("round-trip did not run")
	}
	if !reflect.DeepEqual(*s.original, *s.got) {
		return fmt.Errorf("round-trip lost attributes:\n original=%#v\n got=%#v", *s.original, *s.got)
	}
	return nil
}

func (s *schemaState) thenSchemaExposesDesignAttributes() error {
	wantPublic := []string{
		"spec", "spec_file", "variables", "version_override", "destroy_mode",
		"id", "name", "deployed_version", "previous_version", "hosts",
		"release_path", "service_status", "spec_hash",
	}
	var missing []string
	for _, n := range wantPublic {
		if _, ok := s.schema.Attributes[n]; !ok {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("schema drops DESIGN §5.2 attribute(s) %v", missing)
	}
	return nil
}

func InitializeScenario_terraform_provider_surface_provider_and_deployment_resource_schema(ctx *godog.ScenarioContext) {
	st := &schemaState{}
	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		*st = schemaState{}
		return ctx, nil
	})

	ctx.Step(`^a deployment resource config with both spec and spec_file set$`, st.givenBothSet)
	ctx.Step(`^a deployment resource config with both spec and spec_file empty$`, st.givenBothEmpty)
	ctx.Step(`^a deployment resource config with neither spec nor spec_file set$`, st.givenNeitherSet)
	ctx.Step(`^a deployment resource config with only spec set$`, st.givenOnlySpec)
	ctx.Step(`^a deployment resource config with only spec_file set$`, st.givenOnlySpecFile)
	ctx.Step(`^the deployment resource config validators run$`, st.whenValidatorsRun)
	ctx.Step(`^validation fails with an "([^"]*)" error$`, st.thenValidationFailsWith)
	ctx.Step(`^validation succeeds$`, st.thenValidationSucceeds)

	ctx.Step(`^a fully-populated deployment state$`, st.givenFullyPopulatedState)
	ctx.Step(`^the state is written then read back through the schema$`, st.whenStateWrittenThenRead)
	ctx.Step(`^the state round-trips without attribute loss$`, st.thenRoundTripsWithoutLoss)
	ctx.Step(`^the schema exposes every DESIGN §5.2 attribute$`, st.thenSchemaExposesDesignAttributes)
}

func TestE2E_terraform_provider_surface_provider_and_deployment_resource_schema(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_terraform_provider_surface_provider_and_deployment_resource_schema,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"terraform_provider_surface_provider_and_deployment_resource_schema.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run provider_and_deployment_resource_schema e2e feature tests")
	}
}
