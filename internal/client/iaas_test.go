package client_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

func newEnvAndIaasVdc(t *testing.T, c *client.Client, envName string) (string, string) {
	t.Helper()
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: envName})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}
	vdc, err := c.CreateIaasVdc(context.Background(), env.ID, client.IaasVdcSpec{Name: "prod"})
	if err != nil {
		t.Fatalf("CreateIaasVdc: %v", err)
	}
	return env.ID, vdc.ID
}

func TestCreateIaasVdcNew(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-iaas-vdc-new"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}

	vdc, err := c.CreateIaasVdc(context.Background(), env.ID, client.IaasVdcSpec{
		Name:   "prod",
		Labels: map[string]string{"service": "iaas"},
	})
	if err != nil {
		t.Fatalf("CreateIaasVdc: %v", err)
	}
	if vdc.ID == "" {
		t.Fatal("expected non-empty vdc id")
	}
	if vdc.EnvironmentID != env.ID {
		t.Fatalf("expected environment_id %q, got %q", env.ID, vdc.EnvironmentID)
	}
	if vdc.Status != "planned" {
		t.Fatalf("expected planned vdc, got %q", vdc.Status)
	}
	if vdc.IaasVdcSeq != 42 || vdc.ScopeKey != "42" {
		t.Fatalf("expected sequence/scope_key 42, got seq=%d scope=%q", vdc.IaasVdcSeq, vdc.ScopeKey)
	}
	if vdc.GatewayScope != "iaas_vdc" || vdc.GatewayVPCName != "iaas-42" || vdc.GatewayName != "gw-iaas-42" {
		t.Fatalf("unexpected gateway metadata: %+v", vdc)
	}
	if srv.IaasVdcCount() != 1 {
		t.Fatalf("expected 1 IaaS-vDC, got %d", srv.IaasVdcCount())
	}
}

func TestCreateIaasVdcIdempotentReapply(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	env, err := c.CreateEnvironment(context.Background(), client.EnvironmentSpec{Name: "lab-iaas-vdc-idem"})
	if err != nil {
		t.Fatalf("create environment: %v", err)
	}

	first, err := c.CreateIaasVdc(context.Background(), env.ID, client.IaasVdcSpec{Name: "prod"})
	if err != nil {
		t.Fatalf("first CreateIaasVdc: %v", err)
	}
	second, err := c.CreateIaasVdc(context.Background(), env.ID, client.IaasVdcSpec{Name: "prod"})
	if err != nil {
		t.Fatalf("idempotent CreateIaasVdc: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected existing vdc on reapply, got %s != %s", first.ID, second.ID)
	}
	if srv.IaasVdcCount() != 1 {
		t.Fatalf("expected 1 IaaS-vDC after reapply, got %d", srv.IaasVdcCount())
	}
}

func TestCreateIaasNetworkNewNormalizesCIDR(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, vdcID := newEnvAndIaasVdc(t, c, "lab-iaas-net-new")

	network, err := c.CreateIaasNetwork(context.Background(), envID, vdcID, client.IaasNetworkSpec{
		Name: "web",
		CIDR: "10.80.1.7/24",
	})
	if err != nil {
		t.Fatalf("CreateIaasNetwork: %v", err)
	}
	if network.ID == "" {
		t.Fatal("expected non-empty network id")
	}
	if network.IaasVdcID != vdcID || network.EnvironmentID != envID {
		t.Fatalf("unexpected scoping: %+v", network)
	}
	if network.CIDR != "10.80.1.0/24" {
		t.Fatalf("expected normalized CIDR, got %q", network.CIDR)
	}
	if network.PolicyDefault != "private" || network.Status != "planned" {
		t.Fatalf("unexpected network status/defaults: %+v", network)
	}
	if network.VLANID != nil {
		t.Fatalf("vlan_id must be null until realized, got %d", *network.VLANID)
	}
	if srv.IaasNetworkCount() != 1 {
		t.Fatalf("expected 1 IaaS network, got %d", srv.IaasNetworkCount())
	}
}

func TestCreateIaasNetworkIdempotentReapply(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, vdcID := newEnvAndIaasVdc(t, c, "lab-iaas-net-idem")
	spec := client.IaasNetworkSpec{Name: "web", CIDR: "10.80.1.0/24", PolicyDefault: "allow"}

	first, err := c.CreateIaasNetwork(context.Background(), envID, vdcID, spec)
	if err != nil {
		t.Fatalf("first CreateIaasNetwork: %v", err)
	}
	second, err := c.CreateIaasNetwork(context.Background(), envID, vdcID, spec)
	if err != nil {
		t.Fatalf("idempotent CreateIaasNetwork: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected existing network on reapply, got %s != %s", first.ID, second.ID)
	}
	if second.PolicyDefault != "allow" {
		t.Fatalf("expected policy_default allow, got %q", second.PolicyDefault)
	}
	if srv.IaasNetworkCount() != 1 {
		t.Fatalf("expected 1 IaaS network after reapply, got %d", srv.IaasNetworkCount())
	}
}

func TestCreateIaasNetworkConflicts(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, vdcID := newEnvAndIaasVdc(t, c, "lab-iaas-net-conflict")
	if _, err := c.CreateIaasNetwork(context.Background(), envID, vdcID, client.IaasNetworkSpec{
		Name: "web",
		CIDR: "10.80.1.0/24",
	}); err != nil {
		t.Fatalf("seed CreateIaasNetwork: %v", err)
	}

	for _, tc := range []struct {
		name string
		spec client.IaasNetworkSpec
	}{
		{"same name different cidr", client.IaasNetworkSpec{Name: "web", CIDR: "10.80.2.0/24"}},
		{"same cidr different name", client.IaasNetworkSpec{Name: "db", CIDR: "10.80.1.0/24"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.CreateIaasNetwork(context.Background(), envID, vdcID, tc.spec)
			var apiErr *client.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
				t.Fatalf("expected APIError 409, got %v", err)
			}
		})
	}
	if srv.IaasNetworkCount() != 1 {
		t.Fatalf("conflicts must not create networks, got %d", srv.IaasNetworkCount())
	}
}

func TestDeleteIaasNetworkAndVdcToleratesGone(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID, vdcID := newEnvAndIaasVdc(t, c, "lab-iaas-del")
	network, err := c.CreateIaasNetwork(context.Background(), envID, vdcID, client.IaasNetworkSpec{
		Name: "web",
		CIDR: "10.80.1.0/24",
	})
	if err != nil {
		t.Fatalf("CreateIaasNetwork: %v", err)
	}

	if err := c.DeleteIaasNetwork(context.Background(), envID, vdcID, network.ID); err != nil {
		t.Fatalf("DeleteIaasNetwork: %v", err)
	}
	if _, err := c.GetIaasNetwork(context.Background(), envID, vdcID, network.ID); !client.IsNotFound(err) {
		t.Fatalf("expected network 404 after delete, got %v", err)
	}
	if err := c.DeleteIaasNetwork(context.Background(), envID, vdcID, network.ID); err != nil {
		t.Fatalf("repeated DeleteIaasNetwork must tolerate 404: %v", err)
	}

	if err := c.DeleteIaasVdc(context.Background(), envID, vdcID); err != nil {
		t.Fatalf("DeleteIaasVdc: %v", err)
	}
	if _, err := c.GetIaasVdc(context.Background(), envID, vdcID); !client.IsNotFound(err) {
		t.Fatalf("expected vdc 404 after delete, got %v", err)
	}
	if err := c.DeleteIaasVdc(context.Background(), envID, vdcID); err != nil {
		t.Fatalf("repeated DeleteIaasVdc must tolerate 404: %v", err)
	}
	if srv.IaasVdcCount() != 0 || srv.IaasNetworkCount() != 0 {
		t.Fatalf("expected no live IaaS resources, got vdcs=%d networks=%d",
			srv.IaasVdcCount(), srv.IaasNetworkCount())
	}
}
