package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// TestParsePortRange covers the helper backing ValidateConfig's plan-time
// port_range validation. Bounds/ascending/cross-field are asserted in
// TestEgressValidateConfig; this pins the parse arity/numeric rules.
func TestParsePortRange(t *testing.T) {
	cases := []struct {
		in        string
		low, high int
		ok        bool
	}{
		{"443", 443, 443, true},
		{"8000-9000", 8000, 9000, true},
		{"1-65535", 1, 65535, true},
		{"100-50", 100, 50, true}, // parses; ascending rejected in ValidateConfig
		{"0", 0, 0, true},         // parses; out-of-range rejected in ValidateConfig
		{"foo", 0, 0, false},      // non-numeric
		{"443-", 0, 0, false},     // missing high
		{"-443", 0, 0, false},     // missing low
		{"1-2-3", 0, 0, false},    // wrong arity
		{"", 0, 0, false},         // empty splits to one non-numeric part
	}
	for _, c := range cases {
		low, high, ok := parsePortRange(c.in)
		if ok != c.ok || (ok && (low != c.low || high != c.high)) {
			t.Errorf("parsePortRange(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.in, low, high, ok, c.low, c.high, c.ok)
		}
	}
}

// egressConfig builds a tfsdk.Config from the real egress schema with all
// attributes Null except protocol/port_range (which the caller controls). This
// lets ValidateConfig be exercised directly, including the protocol=Unknown
// case that acc tests can't easily express.
func egressConfig(t *testing.T, protocol, portRange tftypes.Value) tfsdk.Config {
	t.Helper()
	ctx := context.Background()
	r := &egressResource{}
	sr := resource.SchemaResponse{}
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	s := sr.Schema
	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	vals := map[string]tftypes.Value{}
	for name, at := range objType.AttributeTypes {
		vals[name] = tftypes.NewValue(at, nil) // Null
	}
	vals["environment_id"] = tftypes.NewValue(tftypes.String, "env")
	vals["cluster_id"] = tftypes.NewValue(tftypes.String, "cl")
	vals["destination_cidr"] = tftypes.NewValue(tftypes.String, "8.8.8.8/32")
	vals["protocol"] = protocol
	vals["port_range"] = portRange
	return tfsdk.Config{Schema: s, Raw: tftypes.NewValue(objType, vals)}
}

func validateEgress(t *testing.T, protocol, portRange tftypes.Value) resource.ValidateConfigResponse {
	t.Helper()
	r := &egressResource{}
	resp := resource.ValidateConfigResponse{}
	r.ValidateConfig(context.Background(),
		resource.ValidateConfigRequest{Config: egressConfig(t, protocol, portRange)}, &resp)
	return resp
}

func strVal(s string) tftypes.Value { return tftypes.NewValue(tftypes.String, s) }
func nullStr() tftypes.Value        { return tftypes.NewValue(tftypes.String, nil) }
func unknownStr() tftypes.Value     { return tftypes.NewValue(tftypes.String, tftypes.UnknownValue) }

// TestEgressValidateConfig exercises the plan-time validation: an UNKNOWN
// protocol must NOT trip the tcp/udp cross-field check. It may resolve to
// tcp/udp at apply; the server still enforces 422.
func TestEgressValidateConfig(t *testing.T) {
	cases := []struct {
		name      string
		protocol  tftypes.Value
		portRange tftypes.Value
		wantError bool
	}{
		// R6 P2: unknown protocol + valid/known port_range -> only syntax is
		// checked, no spurious cross-field rejection.
		{"unknown_proto_valid_port", unknownStr(), strVal("443"), false},
		{"unknown_proto_valid_range", unknownStr(), strVal("8000-9000"), false},
		// ...but a malformed range is still wrong regardless of protocol.
		{"unknown_proto_bad_syntax", unknownStr(), strVal("foo"), true},
		{"unknown_proto_descending", unknownStr(), strVal("100-50"), true},
		{"unknown_proto_out_of_range", unknownStr(), strVal("70000"), true},
		// Known protocol cross-field rules (also covered by acc tests).
		{"tcp_valid", strVal("tcp"), strVal("443"), false},
		{"udp_range", strVal("udp"), strVal("8000-9000"), false},
		{"any_with_port", strVal("any"), strVal("443"), true},
		{"icmp_with_port", strVal("icmp"), strVal("443"), true},
		{"null_proto_with_port", nullStr(), strVal("443"), true}, // Null == default "any"
		// No port_range -> always fine, even for any/icmp.
		{"any_no_port", strVal("any"), nullStr(), false},
		{"unknown_proto_no_port", unknownStr(), nullStr(), false},
		// Unknown port_range -> can't validate yet, skip.
		{"tcp_unknown_port", strVal("tcp"), unknownStr(), false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			resp := validateEgress(t, c.protocol, c.portRange)
			if got := resp.Diagnostics.HasError(); got != c.wantError {
				t.Fatalf("HasError=%v want %v (diags: %v)", got, c.wantError, resp.Diagnostics)
			}
		})
	}
}
