package client_test

// Tests for the top-level read endpoints GET /v1/quota and GET /v1/images
// against the mock (limits configurable, usage computed from live state).

import (
	"context"
	"testing"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/client"
	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

func TestGetQuotaLimitsAndUsage(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.QuotaMaxVMs = 5
	srv.QuotaMaxVCPU = 16
	c := newTestClient(t, srv.URL, testToken)

	q, err := c.GetQuota(context.Background())
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.MaxVMs != 5 || q.MaxVCPU != 16 {
		t.Fatalf("expected configured limits 5/16, got %d/%d", q.MaxVMs, q.MaxVCPU)
	}
	if q.UsedEnvironments != 0 || q.UsedVMs != 0 {
		t.Fatalf("expected zero usage on a fresh mock, got %+v", q)
	}

	envID := newEnvForVms(t, c, "lab-quota")
	if _, err := c.CreateVm(context.Background(), envID,
		client.VmSpec{Image: "ubuntu-22.04", CPUCores: 4, MemoryGB: 8}); err != nil {
		t.Fatalf("CreateVm: %v", err)
	}

	q, err = c.GetQuota(context.Background())
	if err != nil {
		t.Fatalf("GetQuota after create: %v", err)
	}
	if q.UsedEnvironments != 1 || q.UsedVMs != 1 {
		t.Fatalf("expected 1 env / 1 vm used, got %d/%d", q.UsedEnvironments, q.UsedVMs)
	}
	if q.UsedVCPU != 4 || q.UsedRAMGB != 8 {
		t.Fatalf("expected 4 vCPU / 8 GiB used, got %d/%d", q.UsedVCPU, q.UsedRAMGB)
	}
}

func TestGetQuotaCountsVmSpecDefaults(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, testToken)
	envID := newEnvForVms(t, c, "lab-quota-defaults")

	// Spec without sizing: the server defaults (2 vCPU / 4 GiB) count.
	if _, err := c.CreateVm(context.Background(), envID, client.VmSpec{Image: "ubuntu-22.04"}); err != nil {
		t.Fatalf("CreateVm: %v", err)
	}
	q, err := c.GetQuota(context.Background())
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.UsedVCPU != 2 || q.UsedRAMGB != 4 {
		t.Fatalf("expected default sizing 2/4 counted, got %d/%d", q.UsedVCPU, q.UsedRAMGB)
	}
}

func TestGetQuotaUnauthorized(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, "fcs_wrong")

	_, err := c.GetQuota(context.Background())
	apiErr, ok := err.(*client.APIError)
	if !ok || apiErr.StatusCode != 401 {
		t.Fatalf("expected APIError 401, got %v", err)
	}
}

func TestListImagesCatalog(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	srv.Images = []mockapi.Image{
		{Name: "ubuntu-22.04", DisplayName: "Ubuntu 22.04 LTS", Source: "catalog"},
		{Name: "lab-base", Source: "env"},
	}
	c := newTestClient(t, srv.URL, testToken)

	images, err := c.ListImages(context.Background())
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(images))
	}
	if images[0].Name != "ubuntu-22.04" || images[0].DisplayName != "Ubuntu 22.04 LTS" || images[0].Source != "catalog" {
		t.Fatalf("unexpected first image: %+v", images[0])
	}
	if images[1].Name != "lab-base" || images[1].DisplayName != "" || images[1].Source != "env" {
		t.Fatalf("unexpected second image: %+v", images[1])
	}
}

func TestListImagesUnauthorized(t *testing.T) {
	srv := mockapi.New(testToken)
	defer srv.Close()
	c := newTestClient(t, srv.URL, "fcs_wrong")

	_, err := c.ListImages(context.Background())
	apiErr, ok := err.(*client.APIError)
	if !ok || apiErr.StatusCode != 401 {
		t.Fatalf("expected APIError 401, got %v", err)
	}
}
