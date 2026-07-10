package mockapi_test

// Contract tests for the VM debug sub-routes of the mock:
//   - GET .../vms/{vid}/status  -> VmStatus (phase/reason/message/
//     platform_error), destroyed/unknown -> 404, configurable via
//     VmStatusOverride, pure read (does not advance the status machine)
//   - GET .../vms/{vid}/console-log -> text/plain tail, destroyed/unknown
//     -> 404, never spawned -> 409, invalid tail -> 422
//   - Vm payloads serialize vm_ip/console_url as JSON null until they
//     exist (server to_contract semantics)
//
// The tests speak raw HTTP on purpose: the Terraform client has no methods
// for the debug endpoints, and the mock must stay contract-true for any
// consumer, not just for our client.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/focusnetcloud/terraform-provider-fcs/internal/mockapi"
)

const token = "fcs_mock_test_token"

// doJSON sends an authorized request and decodes the JSON response into out
// (when out != nil). It returns the HTTP status code.
func doJSON(t *testing.T, method, url string, payload, out any) int {
	t.Helper()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s %s response: %v", method, url, err)
		}
	}
	return resp.StatusCode
}

// newEnvAndVm creates an environment plus a VM and returns both ids.
func newEnvAndVm(t *testing.T, srv *mockapi.Server, envName string) (envID, vmID string) {
	t.Helper()
	var env struct {
		ID string `json:"id"`
	}
	if code := doJSON(t, http.MethodPost, srv.URL+"/v1/environments",
		map[string]any{"name": envName}, &env); code != http.StatusAccepted {
		t.Fatalf("create environment: HTTP %d", code)
	}
	var vm struct {
		ID string `json:"id"`
	}
	if code := doJSON(t, http.MethodPost, srv.URL+"/v1/environments/"+env.ID+"/vms",
		map[string]any{"image": "ubuntu-22.04", "name": "dbg"}, &vm); code != http.StatusAccepted {
		t.Fatalf("create vm: HTTP %d", code)
	}
	return env.ID, vm.ID
}

// getVmField GETs the VM item and returns its status (advances the mock
// status machine like a provider poll would).
func getVmStatusField(t *testing.T, srv *mockapi.Server, envID, vmID string) string {
	t.Helper()
	var vm struct {
		Status string `json:"status"`
	}
	if code := doJSON(t, http.MethodGet,
		srv.URL+"/v1/environments/"+envID+"/vms/"+vmID, nil, &vm); code != http.StatusOK {
		t.Fatalf("GET vm: HTTP %d", code)
	}
	return vm.Status
}

func statusURL(srv *mockapi.Server, envID, vmID string) string {
	return srv.URL + "/v1/environments/" + envID + "/vms/" + vmID + "/status"
}

func consoleLogURL(srv *mockapi.Server, envID, vmID, query string) string {
	return srv.URL + "/v1/environments/" + envID + "/vms/" + vmID + "/console-log" + query
}

func getText(t *testing.T, url string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(raw), resp.Header.Get("Content-Type")
}

func TestVmStatusProvisioningIsPureRead(t *testing.T) {
	srv := mockapi.New(token)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	envID, vmID := newEnvAndVm(t, srv, "dbg-status-prov")

	var st mockapi.VmStatus
	for i := 0; i < 2; i++ { // repeated /status reads must not flip the VM
		if code := doJSON(t, http.MethodGet, statusURL(srv, envID, vmID), nil, &st); code != http.StatusOK {
			t.Fatalf("GET status: HTTP %d", code)
		}
		if st.Phase != "Pending" || st.Reason == nil || *st.Reason != "Provisioning" || st.PlatformError {
			t.Fatalf("expected Pending/Provisioning/platform_error=false, got %+v", st)
		}
	}
	// The first item GET is still GET #1 of the status machine: /status is
	// a pure read (the server reads DB+VMI without mutating state).
	if got := getVmStatusField(t, srv, envID, vmID); got != "active" {
		t.Fatalf("status GETs must not advance the machine; first item GET expected active, got %q", got)
	}
}

func TestVmStatusActiveThenStopped(t *testing.T) {
	srv := mockapi.New(token)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	envID, vmID := newEnvAndVm(t, srv, "dbg-status-act")

	if got := getVmStatusField(t, srv, envID, vmID); got != "active" {
		t.Fatalf("expected active, got %q", got)
	}
	var st mockapi.VmStatus
	if code := doJSON(t, http.MethodGet, statusURL(srv, envID, vmID), nil, &st); code != http.StatusOK {
		t.Fatalf("GET status: HTTP %d", code)
	}
	if st.Phase != "Running" || st.Reason != nil || st.PlatformError {
		t.Fatalf("expected Running with null reason, got %+v", st)
	}
	if !strings.Contains(st.Message, "10.0.0.") {
		t.Fatalf("expected the vm ip in the message, got %q", st.Message)
	}

	if code := doJSON(t, http.MethodPost,
		srv.URL+"/v1/environments/"+envID+"/vms/"+vmID+"/power",
		map[string]string{"action": "stop"}, nil); code != http.StatusAccepted {
		t.Fatalf("power stop: HTTP %d", code)
	}
	if code := doJSON(t, http.MethodGet, statusURL(srv, envID, vmID), nil, &st); code != http.StatusOK {
		t.Fatalf("GET status after stop: HTTP %d", code)
	}
	if st.Phase != "Stopped" || st.Reason != nil || st.PlatformError {
		t.Fatalf("expected Stopped with null reason, got %+v", st)
	}
}

func TestVmStatusErrorIsPlatformError(t *testing.T) {
	srv := mockapi.New(token)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	srv.FailVmProvisioning = true
	envID, vmID := newEnvAndVm(t, srv, "dbg-status-err")

	if got := getVmStatusField(t, srv, envID, vmID); got != "error" {
		t.Fatalf("expected error, got %q", got)
	}
	var st mockapi.VmStatus
	if code := doJSON(t, http.MethodGet, statusURL(srv, envID, vmID), nil, &st); code != http.StatusOK {
		t.Fatalf("GET status: HTTP %d", code)
	}
	if st.Phase != "Failed" || st.Reason == nil || *st.Reason != "platform_error" || !st.PlatformError {
		t.Fatalf("expected fail-loud Failed/platform_error=true, got %+v", st)
	}
}

func TestVmStatusOverride(t *testing.T) {
	srv := mockapi.New(token)
	defer srv.Close()
	reason := "ImagePullFailed"
	srv.VmStatusOverride = &mockapi.VmStatus{
		Phase:         "Pending",
		Reason:        &reason,
		Message:       "registry unreachable",
		PlatformError: true,
	}
	envID, vmID := newEnvAndVm(t, srv, "dbg-status-ovr")

	var st mockapi.VmStatus
	if code := doJSON(t, http.MethodGet, statusURL(srv, envID, vmID), nil, &st); code != http.StatusOK {
		t.Fatalf("GET status: HTTP %d", code)
	}
	if st.Reason == nil || *st.Reason != "ImagePullFailed" || !st.PlatformError || st.Message != "registry unreachable" {
		t.Fatalf("expected the configured override, got %+v", st)
	}
}

func TestVmStatusDestroyedAndUnknown404(t *testing.T) {
	srv := mockapi.New(token)
	defer srv.Close()
	srv.VmGoneAfterGETs = 5 // item GET would still serve destroyed rows
	envID, vmID := newEnvAndVm(t, srv, "dbg-status-404")

	if code := doJSON(t, http.MethodDelete,
		srv.URL+"/v1/environments/"+envID+"/vms/"+vmID, nil, nil); code != http.StatusAccepted {
		t.Fatalf("DELETE vm: HTTP %d", code)
	}
	// Unlike the item GET (transitional readable destroyed), /status is an
	// immediate 404: the server filters destroyed rows in the get_vm SQL.
	if code := doJSON(t, http.MethodGet, statusURL(srv, envID, vmID), nil, nil); code != http.StatusNotFound {
		t.Fatalf("expected 404 for destroyed vm status, got %d", code)
	}
	if code := doJSON(t, http.MethodGet,
		statusURL(srv, envID, "00000000-0000-4000-8000-000000000000"), nil, nil); code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown vm status, got %d", code)
	}
}

func TestVmConsoleLogTailAndContentType(t *testing.T) {
	srv := mockapi.New(token)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	srv.ConsoleLog = "l1\nl2\nl3\nl4\nl5"
	envID, vmID := newEnvAndVm(t, srv, "dbg-log-tail")
	if got := getVmStatusField(t, srv, envID, vmID); got != "active" {
		t.Fatalf("expected active, got %q", got)
	}

	code, body, contentType := getText(t, consoleLogURL(srv, envID, vmID, "?tail=2"))
	if code != http.StatusOK {
		t.Fatalf("GET console-log: HTTP %d", code)
	}
	if body != "l4\nl5" {
		t.Fatalf("expected the last 2 lines, got %q", body)
	}
	if !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("expected text/plain, got %q", contentType)
	}

	// Default tail (500) covers the whole log.
	code, body, _ = getText(t, consoleLogURL(srv, envID, vmID, ""))
	if code != http.StatusOK || body != "l1\nl2\nl3\nl4\nl5" {
		t.Fatalf("expected the full log by default, got HTTP %d %q", code, body)
	}

	// A stopped VM was spawned: its (completed) virt-launcher pod log stays
	// readable.
	if code := doJSON(t, http.MethodPost,
		srv.URL+"/v1/environments/"+envID+"/vms/"+vmID+"/power",
		map[string]string{"action": "stop"}, nil); code != http.StatusAccepted {
		t.Fatalf("power stop: HTTP %d", code)
	}
	if code, _, _ = getText(t, consoleLogURL(srv, envID, vmID, "?tail=1")); code != http.StatusOK {
		t.Fatalf("expected console-log for a stopped vm, got HTTP %d", code)
	}
}

func TestVmConsoleLogNotSpawned409(t *testing.T) {
	srv := mockapi.New(token)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	envID, vmID := newEnvAndVm(t, srv, "dbg-log-409")

	// provisioning: no virt-launcher pod yet -> 409 (server semantics).
	if code, _, _ := getText(t, consoleLogURL(srv, envID, vmID, "")); code != http.StatusConflict {
		t.Fatalf("expected 409 while provisioning, got %d", code)
	}

	srv.FailVmProvisioning = true
	if got := getVmStatusField(t, srv, envID, vmID); got != "error" {
		t.Fatalf("expected error, got %q", got)
	}
	if code, _, _ := getText(t, consoleLogURL(srv, envID, vmID, "")); code != http.StatusConflict {
		t.Fatalf("expected 409 after a failed spawn, got %d", code)
	}
}

func TestVmConsoleLogDestroyedAndUnknown404(t *testing.T) {
	srv := mockapi.New(token)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	srv.VmGoneAfterGETs = 5
	envID, vmID := newEnvAndVm(t, srv, "dbg-log-404")
	if got := getVmStatusField(t, srv, envID, vmID); got != "active" {
		t.Fatalf("expected active, got %q", got)
	}

	if code := doJSON(t, http.MethodDelete,
		srv.URL+"/v1/environments/"+envID+"/vms/"+vmID, nil, nil); code != http.StatusAccepted {
		t.Fatalf("DELETE vm: HTTP %d", code)
	}
	if code, _, _ := getText(t, consoleLogURL(srv, envID, vmID, "")); code != http.StatusNotFound {
		t.Fatalf("expected 404 for destroyed vm console-log, got %d", code)
	}
	if code, _, _ := getText(t,
		consoleLogURL(srv, envID, "00000000-0000-4000-8000-000000000000", "")); code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown vm console-log, got %d", code)
	}
}

func TestVmConsoleLogInvalidTail422(t *testing.T) {
	srv := mockapi.New(token)
	defer srv.Close()
	srv.VmReadyAfterGETs = 1
	envID, vmID := newEnvAndVm(t, srv, "dbg-log-422")
	if got := getVmStatusField(t, srv, envID, vmID); got != "active" {
		t.Fatalf("expected active, got %q", got)
	}

	for _, tail := range []string{"0", "-3", "abc", "100001"} {
		if code, _, _ := getText(t, consoleLogURL(srv, envID, vmID, "?tail="+tail)); code != http.StatusUnprocessableEntity {
			t.Fatalf("tail=%s: expected 422, got %d", tail, code)
		}
	}
}

// TestVmNullContractFieldsSerializedAsNull pins the wire format: like the
// server (to_contract), vm_ip and console_url are JSON null — not "" — until
// they exist.
func TestVmNullContractFieldsSerializedAsNull(t *testing.T) {
	srv := mockapi.New(token)
	defer srv.Close()
	envID, vmID := newEnvAndVm(t, srv, "dbg-null-fields")

	var raw map[string]json.RawMessage
	if code := doJSON(t, http.MethodGet,
		srv.URL+"/v1/environments/"+envID+"/vms/"+vmID, nil, &raw); code != http.StatusOK {
		t.Fatalf("GET vm: HTTP %d", code)
	}
	for _, field := range []string{"vm_ip", "console_url"} {
		val, ok := raw[field]
		if !ok {
			t.Fatalf("expected %s to be present in the provisioning payload", field)
		}
		if string(val) != "null" {
			t.Fatalf("expected %s to be JSON null while provisioning, got %s", field, val)
		}
	}
}
