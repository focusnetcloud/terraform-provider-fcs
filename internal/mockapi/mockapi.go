// Package mockapi provides an httptest-based mock of the FCS API for tests
// without a real API.
//
// Implemented contract semantics:
//   - Bearer auth: wrong/missing token -> 401 with Error body
//   - POST /v1/environments: idempotent over name (non-terminal records
//     only) -> 200 with the existing Environment; quota exhausted -> 409
//     (reason QuotaExceeded); otherwise 202 with the new Environment —
//     environments are provisioned synchronously (status=active, like the
//     server)
//   - GET /v1/environments: tenant-scoped list (live environments only)
//   - GET /v1/environments/{id}: 404 when unknown; after DELETE the row
//     stays readable as status=destroyed for EnvGoneAfterGETs GETs, then
//     -> 404 (both gone signals the provider must handle)
//   - DELETE /v1/environments/{id}: 202 (idempotent), cascades clusters/VMs
//
// Cluster sub-API:
//   - POST .../clusters: 202 + Cluster (status=provisioning, kind echoed);
//     idempotent re-POST over (environment, kind) -> 200 + the existing
//     non-terminal cluster, even when the spec differs (server semantics);
//     409 is reserved for real quota/capacity cases
//   - GET .../clusters/{cid}: provisioning -> active after
//     ClusterReadyAfterGETs GETs (or -> error with FailClusterProvisioning)
//   - DELETE .../clusters/{cid}: 202 -> status destroyed; the row stays
//     readable for ClusterGoneAfterGETs GETs, then -> 404
//   - POST .../clusters/{cid}/kubeconfig: 201 + short-lived credentials for
//     flex/business/dedicated; shared namespaces have no independent API
//     server and return 409 (matching the live contract)
//
// VM sub-API:
//   - POST .../vms: image must be in the catalog -> else 422; idempotent
//     re-POST (same non-empty name) -> 200 + existing; otherwise 202 +
//     Vm (status=provisioning, server-generated name when empty)
//   - GET .../vms/{vid}: provisioning -> active (running) or stopped
//     (running=false) after VmReadyAfterGETs GETs (or -> error with
//     FailVmProvisioning); active VMs carry a vm_ip; vm_ip/console_url
//     are JSON null until they exist (server to_contract semantics)
//   - GET .../vms/{vid}/status: VmStatus debug contract derived from the
//     current mock state (or VmStatusOverride); destroyed/unknown -> 404
//     immediately (server filters destroyed in SQL, no gone window)
//   - GET .../vms/{vid}/console-log?tail=N: text/plain tail of ConsoleLog;
//     destroyed/unknown -> 404; not yet spawned (provisioning/error) ->
//     409 (server _require_live_vm); invalid tail -> 422
//   - POST .../vms/{vid}/power: stop/start flip status stopped/active,
//     restart keeps the status; invalid verbs -> 422
//   - DELETE .../vms/{vid}: 202 -> status destroyed; after
//     VmGoneAfterGETs further GETs the VM row disappears (404)
//
// Ingress/Egress sub-API (status machine: a live record is always
// status=provisioning — there is no "active" state):
//   - POST .../ingress: idempotent over (environment, cluster_id,
//     hostname_prefix) -> 200 + existing; otherwise the cluster must belong to
//     the environment (else 404) and be business+active+public_ip (else 409);
//     202 + Ingress (public_url https://<public_ip>[:<port>], the real L4 endpoint)
//   - GET .../ingress/{id}: provisioning while live; after DELETE the row
//     stays readable as destroyed for IngressGoneAfterGETs GETs, then -> 404
//   - DELETE .../ingress/{id}: 202 -> status destroyed (idempotent)
//   - POST .../egress: idempotent over (environment, cluster_id,
//     destination_cidr, protocol, port_range) -> 200 + existing; unknown
//     cluster -> 404; invalid/empty destination_cidr -> 422; otherwise 202 +
//     Egress
//   - GET/DELETE .../egress/{id}: analogous to ingress (EgressGoneAfterGETs)
//   - environment DELETE cascades ingress/egress (server CHILD_TEARDOWN)
//
// Published App sub-API:
//   - POST /v1/k8s/namespaces/{namespace_id}/published-apps publishes a
//     Service from a free namespace or flex cluster. The namespace_id is the
//     cluster ID returned by fcs_namespace or fcs_flex_cluster.
//   - Idempotent over the generated hostname and identical target -> 200 +
//     existing; same hostname with a different target -> 409.
//   - GET/DELETE item endpoints are tenant-scoped through the namespace ID.
//   - cluster/environment DELETE cascades published apps.
//
// IaaS-vDC sub-API:
//   - POST .../iaas-vdcs: idempotent over (environment, name) -> 200 +
//     existing; otherwise 202 + vDC with service-gateway contract fields
//   - POST .../iaas-vdcs/{id}/networks: idempotent over (vDC, name) when
//     CIDR matches; different CIDR or duplicate CIDR -> 409; otherwise 202
//   - GET .../iaas-vdcs/{id}/networks/{nid}: planned/provisioning -> active
//     after IaasNetworkReadyAfterGETs GETs (or -> error with
//     FailIaasNetworkProvisioning)
//   - GET/DELETE item endpoints; environment DELETE cascades vDCs/networks
//
// Top-level reads:
//   - GET /v1/quota: configurable limits (QuotaMax*), usage computed from
//     live environments/VMs/clusters
//   - GET /v1/images: the configurable Images catalog
package mockapi

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is a mock FCS API.
type Server struct {
	*httptest.Server

	Token   string // expected bearer token
	MaxEnvs int    // 409 QuotaExceeded once reached; 0 = unlimited

	// Environment status machine knobs.
	EnvGoneAfterGETs int // GETs returning destroyed after DELETE before 404 (default 1)

	// Cluster status machine knobs.
	ClusterReadyAfterGETs   int  // GETs until provisioning -> active (default 2)
	ClusterGoneAfterGETs    int  // GETs returning offboarding after DELETE before 404 (default 1)
	FailClusterProvisioning bool // provisioning -> error instead of active
	OmitSAToken             bool // kubeconfig endpoint returns no sa_token
	// ClusterProvisioningDiagnostics, when set, is returned on every cluster
	// object and is useful for testing Terraform diagnostics on async failures.
	ClusterProvisioningDiagnostics string

	// DedicatedClusterReadyAfterGETs overrides ClusterReadyAfterGETs for
	// kind=dedicated clusters only (a real RKE2 cluster provisions slower than
	// a virtual cluster). 0 falls back to ClusterReadyAfterGETs.
	DedicatedClusterReadyAfterGETs int

	// VM status machine knobs.
	VmReadyAfterGETs   int  // GETs until provisioning -> active|stopped (default 2)
	VmGoneAfterGETs    int  // GETs returning destroyed after DELETE before 404 (default 1)
	FailVmProvisioning bool // provisioning -> error instead of active

	// Ingress/Egress status machine knobs. A live record is always
	// status=provisioning (no active state); after DELETE the row stays
	// readable as status=destroyed for IngressGoneAfterGETs / EgressGoneAfterGETs
	// GETs before turning into a 404.
	IngressGoneAfterGETs int // default 1
	EgressGoneAfterGETs  int // default 1

	// IaaS network status machine knobs.
	IaasNetworkReadyAfterGETs   int  // GETs until planned/provisioning -> active (default 2)
	FailIaasNetworkProvisioning bool // planned/provisioning -> error instead of active

	// VmStatusOverride, when set, is returned verbatim by GET
	// .../vms/{vid}/status for every live VM (e.g. to simulate
	// ImagePullFailed or platform_error cases); destroyed/unknown VMs
	// still 404. When nil the VmStatus is derived from the mock state.
	VmStatusOverride *VmStatus

	// ConsoleLog is the guest console log served by GET
	// .../vms/{vid}/console-log (tail applies per line; default: a small
	// cloud-init style boot log).
	ConsoleLog string

	// Quota limits returned by GET /v1/quota; usage is computed from live
	// mock state.
	QuotaMaxEnvironments int
	QuotaMaxVMs          int
	QuotaMaxVCPU         int
	QuotaMaxRAMGB        int
	QuotaMaxPublicIPs    int

	// Images is the catalog served by GET /v1/images and validated on VM
	// create (unknown image -> 422, like the server).
	Images []Image

	mu              sync.Mutex
	envs            map[string]*environment // by id
	byName          map[string]string       // name -> id
	clusters        map[string]*cluster     // by id
	vms             map[string]*vm          // by id
	ingresses       map[string]*ingress     // by id
	egresses        map[string]*egress      // by id
	publishedApps   map[int64]*publishedApp // by id
	iaasVdcs        map[string]*iaasVdc     // by id
	iaasNetworks    map[string]*iaasNetwork // by id
	kubeconfigMints int                     // successful kubeconfig mints
	nextPublishedID int64
	nextIaasVdcSeq  int64
}

// Image mirrors the Image schema.
type Image struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Source      string `json:"source"`
}

type environment struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	TTLExpiresAt *string `json:"ttl_expires_at"` // nil -> JSON null (persistent env)
	CreatedAt    string  `json:"created_at"`

	goneGETs int // GETs since DELETE
	deleted  bool
}

type environmentSpec struct {
	Name       string            `json:"name"`
	TTLSeconds int64             `json:"ttl_seconds"`
	Labels     map[string]string `json:"labels"`
}

type cluster struct {
	ID                      string `json:"id"`
	Kind                    string `json:"kind"`
	Status                  string `json:"status"`
	APIServerURL            string `json:"api_server_url"`
	ClusterCIDR             string `json:"cluster_cidr"`
	ServiceCIDR             string `json:"service_cidr"`
	ProvisioningDiagnostics string `json:"provisioning_diagnostics"`

	envID    string
	spec     clusterSpec
	getCount int    // GETs while provisioning
	goneGETs int    // GETs since DELETE
	publicIP string // business clusters get an EIP once active (ingress precondition)
	deleted  bool
}

type clusterSpec struct {
	Kind       string `json:"kind"`
	Size       string `json:"size"`
	VCPU       int64  `json:"vcpu"`
	RAMGB      int64  `json:"ram_gb"`
	StorageGB  int64  `json:"storage_gb"`
	K8sVersion string `json:"k8s_version"`

	// Dedicated (kind="dedicated") node-pool sizing — echoed/round-tripped so
	// the mock can validate the additive contract fields the provider sends.
	CPNodes      int64  `json:"cp_nodes"`
	CPVcpu       int64  `json:"cp_vcpu"`
	CPRamGB      int64  `json:"cp_ram_gb"`
	WorkerNodes  int64  `json:"worker_nodes"`
	WorkerVcpu   int64  `json:"worker_vcpu"`
	WorkerRamGB  int64  `json:"worker_ram_gb"`
	PVCStorageGB int64  `json:"pvc_storage_gb"`
	RKE2Version  string `json:"rke2_version"`
}

// hostnamePrefixPattern mirrors the server-side hostname_prefix validation:
// ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ - no leading/trailing hyphen. The mock
// enforces it so provider tests catch the stricter server rule.
var hostnamePrefixPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

var validClusterKinds = map[string]bool{"namespace": true, "flex": true, "business": true, "dedicated": true}

type vm struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// VMIP/ConsoleURL are pointers so the mock serializes JSON null until
	// the values exist — exactly like the server (to_contract: vm_ip null
	// until Ready+IP, console_url null until the console broker is available).
	VMIP       *string `json:"vm_ip"`
	ConsoleURL *string `json:"console_url"`

	envID    string
	spec     vmSpec
	getCount int // GETs while provisioning
	goneGETs int // GETs since DELETE
	deleted  bool
}

// VmStatus mirrors the VmStatus contract schema (debug endpoint): plain
// reason codes plus the fail-loud platform_error flag. Reason is a pointer
// because the server returns null when there is nothing to report.
type VmStatus struct {
	Phase         string  `json:"phase"`
	Reason        *string `json:"reason"`
	Message       string  `json:"message"`
	PlatformError bool    `json:"platform_error"`
}

type vmSpec struct {
	Image                string `json:"image"`
	Name                 string `json:"name"`
	CPUCores             int64  `json:"cpu_cores"`
	MemoryGB             int64  `json:"memory_gb"`
	DiskGB               int64  `json:"disk_gb"`
	NICNetwork           string `json:"nic_network"`
	CloudInitUserdata    string `json:"cloud_init_userdata"`
	CloudInitNetworkdata string `json:"cloud_init_networkdata"`
	Running              *bool  `json:"running"`
	VdcID                string `json:"vdc_id"`
	NetworkID            string `json:"network_id"`
}

func (s vmSpec) running() bool {
	return s.Running == nil || *s.Running
}

// ingress mirrors the Ingress contract schema plus the mock bookkeeping. A
// live record is always status=provisioning; PublicURL is a pointer so it
// serializes as JSON null when unknown (the contract allows null).
type ingress struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	PublicURL *string `json:"public_url"`

	envID          string
	clusterID      string
	hostnamePrefix string
	goneGETs       int // GETs since DELETE
	deleted        bool
}

type ingressSpec struct {
	ClusterID      string `json:"cluster_id"`
	Service        string `json:"service"`
	Port           int    `json:"port"`
	TLS            string `json:"tls"`
	HostnamePrefix string `json:"hostname_prefix"`
}

// mockPublicURL mirrors the server's _public_url: the real L4
// endpoint https://<public_ip>[:<port>], IPv6-bracketed, :443 omitted.
func mockPublicURL(publicIP string, port int) string {
	host := publicIP
	if strings.Contains(publicIP, ":") {
		host = "[" + publicIP + "]"
	}
	if port == 443 {
		return "https://" + host
	}
	return fmt.Sprintf("https://%s:%d", host, port)
}

// egress mirrors the Egress contract schema plus the mock bookkeeping.
type egress struct {
	ID     string `json:"id"`
	Status string `json:"status"`

	envID           string
	clusterID       string
	destinationCIDR string
	protocol        string
	portRange       string
	goneGETs        int // GETs since DELETE
	deleted         bool
}

type egressSpec struct {
	ClusterID       string  `json:"cluster_id"`
	DestinationCIDR string  `json:"destination_cidr"`
	Protocol        string  `json:"protocol"`
	PortRange       *string `json:"port_range"`
}

// publishedApp mirrors the tenant public app publishing contract plus mock
// bookkeeping. There is no lifecycle status in the public contract; create and
// delete are synchronous at the intent layer.
type publishedApp struct {
	ID                int64   `json:"id"`
	Hostname          string  `json:"hostname"`
	AppSlug           string  `json:"app_slug"`
	ServiceName       string  `json:"service_name"`
	ServicePort       int64   `json:"service_port"`
	VclusterNamespace string  `json:"vcluster_namespace"`
	PathPrefix        *string `json:"path_prefix"`
	TLSMode           string  `json:"tls_mode"`

	namespaceID string
}

type publishedAppSpec struct {
	AppSlug           string  `json:"app_slug"`
	ServiceName       string  `json:"service_name"`
	ServicePort       int64   `json:"service_port"`
	VclusterNamespace string  `json:"vcluster_namespace"`
	PathPrefix        *string `json:"path_prefix"`
}

const mockPublishedAppTenantID = 8646

var publishedAppSlugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

var reservedPublishedAppSlugs = map[string]bool{
	"admin":      true,
	"business":   true,
	"dedicated":  true,
	"default":    true,
	"enterprise": true,
	"flex":       true,
	"free":       true,
	"internal":   true,
	"kube":       true,
	"kubernetes": true,
	"public":     true,
	"root":       true,
	"system":     true,
	"test":       true,
}

type iaasVdc struct {
	ID             string `json:"id"`
	EnvironmentID  string `json:"environment_id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	IaasVdcSeq     int64  `json:"iaas_vdc_seq"`
	GatewayScope   string `json:"gateway_scope"`
	ScopeKey       string `json:"scope_key"`
	GatewayVPCName string `json:"gateway_vpc_name"`
	GatewayName    string `json:"gateway_name"`
	CreatedAt      string `json:"created_at"`

	envID    string
	deleted  bool
	goneGETs int
}

type iaasVdcSpec struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

type iaasNetwork struct {
	ID                 string `json:"id"`
	IaasVdcID          string `json:"iaas_vdc_id"`
	EnvironmentID      string `json:"environment_id"`
	Name               string `json:"name"`
	CIDR               string `json:"cidr"`
	VLANID             *int64 `json:"vlan_id"`
	HarvesterNamespace string `json:"harvester_namespace"`
	HarvesterNADName   string `json:"harvester_nad_name"`
	KubeovnSubnetName  string `json:"kubeovn_subnet_name"`
	DefaultGatewayIP   string `json:"default_gateway_ip"`
	PolicyDefault      string `json:"policy_default"`
	Status             string `json:"status"`
	CreatedAt          string `json:"created_at"`

	envID    string
	vdcID    string
	getCount int
	deleted  bool
	goneGETs int
}

type iaasNetworkSpec struct {
	Name          string `json:"name"`
	CIDR          string `json:"cidr"`
	PolicyDefault string `json:"policy_default"`
}

// New starts a mock server expecting the given bearer token.
func New(token string) *Server {
	s := &Server{
		Token:                     token,
		EnvGoneAfterGETs:          1,
		ClusterReadyAfterGETs:     2,
		ClusterGoneAfterGETs:      1,
		VmReadyAfterGETs:          2,
		VmGoneAfterGETs:           1,
		IngressGoneAfterGETs:      1,
		EgressGoneAfterGETs:       1,
		IaasNetworkReadyAfterGETs: 2,
		QuotaMaxEnvironments:      25,
		QuotaMaxVMs:               10,
		QuotaMaxVCPU:              32,
		QuotaMaxRAMGB:             64,
		QuotaMaxPublicIPs:         2,
		Images: []Image{
			{Name: "ubuntu-22.04", DisplayName: "Ubuntu 22.04 LTS", Source: "catalog"},
			{Name: "coriolis-worker-ubuntu2204-qga", DisplayName: "Ubuntu 22.04 LTS (QGA)", Source: "catalog"},
			{Name: "lab-base", Source: "env"},
		},
		ConsoleLog: "[    0.000000] Linux version 6.8.0 (mock)\n" +
			"[    1.234567] systemd[1]: Reached target Multi-User System.\n" +
			"cloud-init[812]: Cloud-init v. 24.1 running 'modules:final'\n" +
			"login: ",
		envs:            map[string]*environment{},
		byName:          map[string]string{},
		clusters:        map[string]*cluster{},
		vms:             map[string]*vm{},
		ingresses:       map[string]*ingress{},
		egresses:        map[string]*egress{},
		publishedApps:   map[int64]*publishedApp{},
		iaasVdcs:        map[string]*iaasVdc{},
		iaasNetworks:    map[string]*iaasNetwork{},
		nextPublishedID: 1,
		nextIaasVdcSeq:  42,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/environments", s.handleCollection)
	mux.HandleFunc("/v1/environments/", s.handleItem)
	mux.HandleFunc("/v1/k8s/namespaces/", s.handlePublishedAppPath)
	mux.HandleFunc("/v1/quota", s.handleQuota)
	mux.HandleFunc("/v1/images", s.handleImages)
	s.Server = httptest.NewServer(mux)
	return s
}

// EnvironmentCount returns the number of live (not yet deleted) environments.
func (s *Server) EnvironmentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveEnvCountLocked()
}

// liveEnvCountLocked counts non-deleted environments. Caller must hold s.mu.
func (s *Server) liveEnvCountLocked() int {
	n := 0
	for _, env := range s.envs {
		if !env.deleted {
			n++
		}
	}
	return n
}

// DeleteByName hard-removes an environment out-of-band (simulates a fully
// completed reap: the next GET is a 404). Returns true when something was
// removed.
func (s *Server) DeleteByName(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byName[name]
	if !ok {
		return false
	}
	delete(s.byName, name)
	delete(s.envs, id)
	s.dropClustersOfLocked(id)
	s.dropVmsOfLocked(id)
	s.dropNetworkingOfLocked(id)
	s.dropIaasOfLocked(id)
	return true
}

// DestroyByName soft-destroys an environment out-of-band (simulates the TTL
// reaper mid-teardown): the row stays readable as status=destroyed for
// EnvGoneAfterGETs GETs before turning into a 404. Children cascade.
// Returns true when something was destroyed.
func (s *Server) DestroyByName(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byName[name]
	if !ok {
		return false
	}
	env := s.envs[id]
	delete(s.byName, name) // destroyed is terminal: the name is free again
	env.deleted = true
	env.Status = "destroyed"
	env.goneGETs = 0
	s.dropClustersOfLocked(id)
	s.dropVmsOfLocked(id)
	s.dropNetworkingOfLocked(id)
	s.dropIaasOfLocked(id)
	return true
}

// DestroyClustersOfEnv soft-destroys all live clusters of the named
// environment out-of-band (simulates reaper/manual teardown): the rows stay
// readable as status=destroyed for ClusterGoneAfterGETs GETs before turning
// into 404s. Returns the number of clusters destroyed.
func (s *Server) DestroyClustersOfEnv(envName string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byName[envName]
	if !ok {
		return 0
	}
	n := 0
	for _, cl := range s.clusters {
		if cl.envID == id && !cl.deleted {
			cl.deleted = true
			cl.Status = "destroyed"
			cl.goneGETs = 0
			s.dropPublishedAppsOfClusterLocked(cl.ID)
			n++
		}
	}
	return n
}

// VmCount returns the number of live (not yet deleted) VMs.
func (s *Server) VmCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, v := range s.vms {
		if !v.deleted {
			n++
		}
	}
	return n
}

// VmSpecByName returns the create payload the mock stored for a VM.
func (s *Server) VmSpecByName(envName string, vmName string) (vmSpec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	envID, ok := s.byName[envName]
	if !ok {
		return vmSpec{}, false
	}
	for _, v := range s.vms {
		if v.envID == envID && !v.deleted && v.Name == vmName {
			return v.spec, true
		}
	}
	return vmSpec{}, false
}

// dropVmsOfLocked removes all VMs of an environment (cascade on
// environment teardown). Caller must hold s.mu.
func (s *Server) dropVmsOfLocked(envID string) {
	for id, v := range s.vms {
		if v.envID == envID {
			delete(s.vms, id)
		}
	}
}

// dropNetworkingOfLocked removes all ingress/egress records of an environment
// (cascade on environment teardown, mirroring the server CHILD_TEARDOWN that
// reaps env-linked port-forwards/egress-rules). Caller must hold s.mu.
func (s *Server) dropNetworkingOfLocked(envID string) {
	for id, ing := range s.ingresses {
		if ing.envID == envID {
			delete(s.ingresses, id)
		}
	}
	for id, eg := range s.egresses {
		if eg.envID == envID {
			delete(s.egresses, id)
		}
	}
}

// IaasVdcCount returns the number of live IaaS-vDC records.
func (s *Server) IaasVdcCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, vdc := range s.iaasVdcs {
		if !vdc.deleted {
			n++
		}
	}
	return n
}

// IaasNetworkCount returns the number of live IaaS-vDC network records.
func (s *Server) IaasNetworkCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, network := range s.iaasNetworks {
		if !network.deleted {
			n++
		}
	}
	return n
}

// dropIaasOfLocked removes all IaaS-vDCs and networks of an environment.
// Caller must hold s.mu.
func (s *Server) dropIaasOfLocked(envID string) {
	for id, vdc := range s.iaasVdcs {
		if vdc.envID == envID {
			delete(s.iaasVdcs, id)
		}
	}
	for id, network := range s.iaasNetworks {
		if network.envID == envID {
			delete(s.iaasNetworks, id)
		}
	}
}

// ClusterCount returns the number of live (not yet deleted) clusters.
func (s *Server) ClusterCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, cl := range s.clusters {
		if !cl.deleted {
			n++
		}
	}
	return n
}

// dropClustersOfLocked removes all clusters of an environment (cascade on
// environment teardown). Caller must hold s.mu.
func (s *Server) dropClustersOfLocked(envID string) {
	for id, cl := range s.clusters {
		if cl.envID == envID {
			s.dropPublishedAppsOfClusterLocked(id)
			delete(s.clusters, id)
		}
	}
}

// dropPublishedAppsOfClusterLocked removes all published apps scoped to a
// namespace/flex cluster. Caller must hold s.mu.
func (s *Server) dropPublishedAppsOfClusterLocked(clusterID string) {
	for id, app := range s.publishedApps {
		if app.namespaceID == clusterID {
			delete(s.publishedApps, id)
		}
	}
}

func (s *Server) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+s.Token
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, detail, reason string) {
	writeJSON(w, status, map[string]string{"detail": detail, "reason": reason})
}

func (s *Server) handleCollection(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid or missing bearer token", "Unauthorized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		list := make([]*environment, 0, len(s.envs))
		for _, env := range s.envs {
			if env.deleted {
				continue // destroyed rows are not listed
			}
			list = append(list, env)
		}
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		var spec environmentSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || spec.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid environment spec", "BadRequest")
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if id, ok := s.byName[spec.Name]; ok {
			// Idempotent re-apply over (tenant, name) — byName only tracks
			// non-terminal environments.
			writeJSON(w, http.StatusOK, s.envs[id])
			return
		}
		if s.MaxEnvs > 0 && s.liveEnvCountLocked() >= s.MaxEnvs {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("max_concurrent_environments (%d) reached", s.MaxEnvs), "QuotaExceeded")
			return
		}
		now := time.Now().UTC()
		// Mirror the server: ttl_seconds omitted/0 => PERSISTENT (no expiry,
		// ttl_expires_at empty/null); only an explicit ttl_seconds sets an expiry.
		var ttlExpiresAt *string // nil => JSON null for a persistent environment
		if spec.TTLSeconds > 0 {
			s := now.Add(time.Duration(spec.TTLSeconds) * time.Second).Format(time.RFC3339)
			ttlExpiresAt = &s
		}
		env := &environment{
			ID:   newUUID(),
			Name: spec.Name,
			// Environments are provisioned synchronously: active right away, no async step.
			Status:       "active",
			TTLExpiresAt: ttlExpiresAt,
			CreatedAt:    now.Format(time.RFC3339),
		}
		s.envs[env.ID] = env
		s.byName[env.Name] = env.ID
		writeJSON(w, http.StatusAccepted, env)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleItem(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid or missing bearer token", "Unauthorized")
		return
	}
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/environments/"), "/")
	if len(segs) == 0 || segs[0] == "" {
		http.NotFound(w, r)
		return
	}
	id := segs[0]

	s.mu.Lock()
	defer s.mu.Unlock()
	env, ok := s.envs[id]
	if !ok {
		writeError(w, http.StatusNotFound, "environment not found", "NotFound")
		return
	}
	if env.deleted && len(segs) > 1 {
		// Sub-resources of a destroyed environment are gone (cascade).
		writeError(w, http.StatusNotFound, "environment not found", "NotFound")
		return
	}

	switch {
	case len(segs) == 1:
		s.handleEnvironmentItemLocked(w, r, env)
	case len(segs) == 2 && segs[1] == "clusters":
		s.handleClusterCollectionLocked(w, r, env)
	case len(segs) == 3 && segs[1] == "clusters":
		s.handleClusterItemLocked(w, r, env, segs[2])
	case len(segs) == 4 && segs[1] == "clusters" && segs[3] == "kubeconfig":
		s.handleClusterKubeconfigLocked(w, r, env, segs[2])
	case len(segs) == 2 && segs[1] == "vms":
		s.handleVmCollectionLocked(w, r, env)
	case len(segs) == 3 && segs[1] == "vms":
		s.handleVmItemLocked(w, r, env, segs[2])
	case len(segs) == 4 && segs[1] == "vms" && segs[3] == "power":
		s.handleVmPowerLocked(w, r, env, segs[2])
	case len(segs) == 4 && segs[1] == "vms" && segs[3] == "status":
		s.handleVmStatusLocked(w, r, env, segs[2])
	case len(segs) == 4 && segs[1] == "vms" && segs[3] == "console-log":
		s.handleVmConsoleLogLocked(w, r, env, segs[2])
	case len(segs) == 2 && segs[1] == "ingress":
		s.handleIngressCollectionLocked(w, r, env)
	case len(segs) == 3 && segs[1] == "ingress":
		s.handleIngressItemLocked(w, r, env, segs[2])
	case len(segs) == 2 && segs[1] == "egress":
		s.handleEgressCollectionLocked(w, r, env)
	case len(segs) == 3 && segs[1] == "egress":
		s.handleEgressItemLocked(w, r, env, segs[2])
	case len(segs) == 2 && segs[1] == "iaas-vdcs":
		s.handleIaasVdcCollectionLocked(w, r, env)
	case len(segs) == 3 && segs[1] == "iaas-vdcs":
		s.handleIaasVdcItemLocked(w, r, env, segs[2])
	case len(segs) == 4 && segs[1] == "iaas-vdcs" && segs[3] == "networks":
		s.handleIaasNetworkCollectionLocked(w, r, env, segs[2])
	case len(segs) == 5 && segs[1] == "iaas-vdcs" && segs[3] == "networks":
		s.handleIaasNetworkItemLocked(w, r, env, segs[2], segs[4])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleEnvironmentItemLocked(w http.ResponseWriter, r *http.Request, env *environment) {
	switch r.Method {
	case http.MethodGet:
		if env.deleted {
			env.goneGETs++
			if env.goneGETs > s.EnvGoneAfterGETs {
				delete(s.envs, env.ID)
				writeError(w, http.StatusNotFound, "environment not found", "NotFound")
				return
			}
			writeJSON(w, http.StatusOK, env) // status=destroyed
			return
		}
		writeJSON(w, http.StatusOK, env)
	case http.MethodDelete:
		// Idempotent: repeated DELETE on a destroyed environment is still
		// a 202. The row stays readable as destroyed for EnvGoneAfterGETs
		// GETs (mirrors the server DB before the destroyed->404 mapping).
		if !env.deleted {
			delete(s.byName, env.Name) // destroyed is terminal: name is free again
			env.deleted = true
			env.Status = "destroyed"
			env.goneGETs = 0
			s.dropClustersOfLocked(env.ID) // server-side cascade (offboard_environment)
			s.dropVmsOfLocked(env.ID)
			s.dropNetworkingOfLocked(env.ID) // env-linked ingress/egress reaped
			s.dropIaasOfLocked(env.ID)
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleClusterCollectionLocked(w http.ResponseWriter, r *http.Request, env *environment) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var spec clusterSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || !validClusterKinds[spec.Kind] {
		writeError(w, http.StatusBadRequest, "invalid cluster spec: kind must be namespace|flex|business|dedicated", "BadRequest")
		return
	}
	for _, cl := range s.clusters {
		if cl.envID != env.ID || cl.deleted || cl.Kind != spec.Kind {
			continue
		}
		// Idempotent re-apply over (environment, kind): the existing
		// non-terminal cluster is returned even when the spec differs
		// (server semantics; 409 is reserved for real quota cases).
		writeJSON(w, http.StatusOK, cl)
		return
	}
	cl := &cluster{
		ID:                      newUUID(),
		Kind:                    spec.Kind,
		Status:                  "provisioning",
		ClusterCIDR:             "10.52.0.0/16",
		ServiceCIDR:             "10.53.0.0/16",
		ProvisioningDiagnostics: s.ClusterProvisioningDiagnostics,
		envID:                   env.ID,
		spec:                    spec,
	}
	cl.APIServerURL = fmt.Sprintf("https://%s.k8s.focusnet.de:6443", cl.ID[:8])
	s.clusters[cl.ID] = cl
	writeJSON(w, http.StatusAccepted, cl)
}

// clusterReadyAfterGETsFor returns the provisioning->active GET threshold for
// a cluster kind: kind=dedicated honours DedicatedClusterReadyAfterGETs when
// set (a real RKE2 cluster provisions slower), otherwise all kinds share
// ClusterReadyAfterGETs.
func (s *Server) clusterReadyAfterGETsFor(kind string) int {
	if kind == "dedicated" && s.DedicatedClusterReadyAfterGETs > 0 {
		return s.DedicatedClusterReadyAfterGETs
	}
	return s.ClusterReadyAfterGETs
}

func (s *Server) handleClusterItemLocked(w http.ResponseWriter, r *http.Request, env *environment, clusterID string) {
	cl, ok := s.clusters[clusterID]
	if !ok || cl.envID != env.ID {
		writeError(w, http.StatusNotFound, "cluster not found", "NotFound")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if cl.deleted {
			cl.goneGETs++
			if cl.goneGETs > s.ClusterGoneAfterGETs {
				delete(s.clusters, clusterID)
				writeError(w, http.StatusNotFound, "cluster not found", "NotFound")
				return
			}
			writeJSON(w, http.StatusOK, cl) // status=destroyed
			return
		}
		if cl.Status == "provisioning" {
			cl.getCount++
			if cl.getCount >= s.clusterReadyAfterGETsFor(cl.Kind) {
				if s.FailClusterProvisioning {
					cl.Status = "error"
				} else {
					cl.Status = "active"
					switch cl.Kind {
					case "business":
						// Business clusters allocate their own EIP; the
						// ingress precondition (public IP present) depends on
						// it (server: clusters carry public_ip once active).
						cl.publicIP = fmt.Sprintf("203.0.113.%d", 10+len(s.clusters))
					case "dedicated":
						// Dedicated (real RKE2) clusters get a public EIP and
						// expose the kube-API at https://<public_ip>:6443
						// (server to_contract: api_server_url once active).
						cl.publicIP = fmt.Sprintf("203.0.113.%d", 10+len(s.clusters))
						cl.APIServerURL = fmt.Sprintf("https://%s:6443", cl.publicIP)
					}
				}
			}
		}
		writeJSON(w, http.StatusOK, cl)
	case http.MethodDelete:
		// Idempotent: repeated DELETE on a destroyed cluster is still a 202.
		// The row stays readable as destroyed for ClusterGoneAfterGETs GETs.
		if !cl.deleted {
			cl.deleted = true
			cl.Status = "destroyed"
			cl.goneGETs = 0
			s.dropPublishedAppsOfClusterLocked(clusterID)
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleClusterKubeconfigLocked(w http.ResponseWriter, r *http.Request, env *environment, clusterID string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	cl, ok := s.clusters[clusterID]
	if !ok || cl.envID != env.ID || cl.deleted {
		writeError(w, http.StatusNotFound, "cluster not found", "NotFound")
		return
	}
	if cl.Kind == "namespace" {
		writeError(
			w,
			http.StatusConflict,
			"namespace clusters have no independent Kubernetes API server",
			"KubeconfigUnsupported",
		)
		return
	}
	saToken := "sa." + newUUID()
	body := map[string]any{
		"api_server_url": cl.APIServerURL,
		"kubeconfig":     renderKubeconfig(cl.APIServerURL, saToken),
		"expires_at":     time.Now().UTC().Add(15 * time.Minute).Format(time.RFC3339),
	}
	if !s.OmitSAToken {
		body["sa_token"] = saToken
	}
	s.kubeconfigMints++
	writeJSON(w, http.StatusCreated, body)
}

// KubeconfigMintCount returns how many credential sets the kubeconfig
// endpoint has successfully minted (tests use it to prove an ephemeral
// open really happened even though nothing reaches the state).
func (s *Server) KubeconfigMintCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kubeconfigMints
}

func (s *Server) imageInCatalogLocked(name string) bool {
	for _, img := range s.Images {
		if img.Name == name {
			return true
		}
	}
	return false
}

func (s *Server) handleVmCollectionLocked(w http.ResponseWriter, r *http.Request, env *environment) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var spec vmSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || spec.Image == "" {
		writeError(w, http.StatusBadRequest, "invalid vm spec: image is required", "BadRequest")
		return
	}
	if !s.imageInCatalogLocked(spec.Image) {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("image %q is not in the catalog", spec.Image), "ImageNotAllowed")
		return
	}
	if spec.Name != "" {
		for _, v := range s.vms {
			if v.envID == env.ID && !v.deleted && v.Name == spec.Name {
				// Idempotent re-POST over (environment, name).
				writeJSON(w, http.StatusOK, v)
				return
			}
		}
	}
	v := &vm{
		ID:     newUUID(),
		Name:   spec.Name,
		Status: "provisioning",
		envID:  env.ID,
		spec:   spec,
	}
	if v.Name == "" {
		v.Name = "vm-" + v.ID[:8] // server-generated name
	}
	s.vms[v.ID] = v
	writeJSON(w, http.StatusAccepted, v)
}

func (s *Server) handleVmItemLocked(w http.ResponseWriter, r *http.Request, env *environment, vmID string) {
	v, ok := s.vms[vmID]
	if !ok || v.envID != env.ID {
		writeError(w, http.StatusNotFound, "vm not found", "NotFound")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if v.deleted {
			v.goneGETs++
			if v.goneGETs > s.VmGoneAfterGETs {
				delete(s.vms, vmID)
				writeError(w, http.StatusNotFound, "vm not found", "NotFound")
				return
			}
			writeJSON(w, http.StatusOK, v) // status=destroyed
			return
		}
		if v.Status == "provisioning" {
			v.getCount++
			if v.getCount >= s.VmReadyAfterGETs {
				switch {
				case s.FailVmProvisioning:
					v.Status = "error"
				case v.spec.running():
					v.Status = "active"
					v.VMIP = strPtr("10.0.0." + fmt.Sprint(10+len(s.vms)))
				default:
					v.Status = "stopped" // running=false: spawned but powered off
				}
			}
		}
		writeJSON(w, http.StatusOK, v)
	case http.MethodDelete:
		// Idempotent: repeated DELETE on a destroyed VM is still a 202.
		if !v.deleted {
			v.deleted = true
			v.Status = "destroyed"
			v.goneGETs = 0
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleVmPowerLocked(w http.ResponseWriter, r *http.Request, env *environment, vmID string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	v, ok := s.vms[vmID]
	if !ok || v.envID != env.ID || v.deleted {
		writeError(w, http.StatusNotFound, "vm not found", "NotFound")
		return
	}
	var payload struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid power payload", "BadRequest")
		return
	}
	// Status mirroring like the server: stop/start flip the desired state,
	// restart keeps it (vms.power_environment_vm).
	switch payload.Action {
	case "stop":
		v.Status = "stopped"
	case "start":
		v.Status = "active"
		if v.VMIP == nil {
			v.VMIP = strPtr("10.0.0." + fmt.Sprint(10+len(s.vms)))
		}
	case "restart":
		// no status change
	default:
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("invalid power action %q", payload.Action), "ValidationError")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"action": payload.Action, "status": "accepted"})
}

// handleVmStatusLocked serves GET .../vms/{vid}/status (VmStatus debug
// contract). Like the server (get_vm filters destroyed in SQL), unknown and
// destroyed VMs are an immediate 404 — no transitional gone window. The
// read is pure: it does not advance the provisioning status machine.
func (s *Server) handleVmStatusLocked(w http.ResponseWriter, r *http.Request, env *environment, vmID string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	v, ok := s.vms[vmID]
	if !ok || v.envID != env.ID || v.deleted {
		writeError(w, http.StatusNotFound, "VM not found", "NotFound")
		return
	}
	if s.VmStatusOverride != nil {
		writeJSON(w, http.StatusOK, s.VmStatusOverride)
		return
	}
	writeJSON(w, http.StatusOK, s.vmStatusForLocked(v))
}

// vmStatusForLocked derives the VmStatus from the mock state for the states
// the mock can reach. Caller must hold s.mu.
func (s *Server) vmStatusForLocked(v *vm) *VmStatus {
	switch v.Status {
	case "provisioning":
		return &VmStatus{
			Phase:   "Pending",
			Reason:  strPtr("Provisioning"),
			Message: "VM is being created (VMI does not exist yet)",
		}
	case "stopped":
		return &VmStatus{
			Phase:   "Stopped",
			Message: "VM is stopped (POST .../power action=start)",
		}
	case "error":
		return &VmStatus{
			Phase:         "Failed",
			Reason:        strPtr("platform_error"),
			Message:       "spawn failed; FCS incident, not a tenant issue",
			PlatformError: true,
		}
	default: // active
		ip := ""
		if v.VMIP != nil {
			ip = *v.VMIP
		}
		return &VmStatus{
			Phase:   "Running",
			Message: fmt.Sprintf("VM running (%s)", ip),
		}
	}
}

// handleVmConsoleLogLocked serves GET .../vms/{vid}/console-log?tail=N as
// text/plain. Unknown/destroyed -> 404; VMs that never spawned
// (provisioning, or error after a failed spawn) -> 409 like the server
// (_require_live_vm / ConsoleLogUnavailable); invalid tail -> 422 (FastAPI
// query validation, ge=1 le=100000).
func (s *Server) handleVmConsoleLogLocked(w http.ResponseWriter, r *http.Request, env *environment, vmID string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	v, ok := s.vms[vmID]
	if !ok || v.envID != env.ID || v.deleted {
		writeError(w, http.StatusNotFound, "VM not found", "NotFound")
		return
	}
	tail := 500 // contract default
	if raw := r.URL.Query().Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100000 {
			writeError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("tail must be a number between 1 and 100000, not %q", raw),
				"ValidationError")
			return
		}
		tail = n
	}
	if v.Status == "provisioning" || v.Status == "error" {
		writeError(w, http.StatusConflict,
			"no virt-launcher pod found (VM not started yet?)",
			"ConsoleLogUnavailable")
		return
	}
	lines := strings.Split(s.ConsoleLog, "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(strings.Join(lines, "\n")))
}

func strPtr(s string) *string { return &s }

// IngressCount returns the number of live (not yet deleted) ingress records.
func (s *Server) IngressCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ing := range s.ingresses {
		if !ing.deleted {
			n++
		}
	}
	return n
}

// EgressCount returns the number of live (not yet deleted) egress records.
func (s *Server) EgressCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, eg := range s.egresses {
		if !eg.deleted {
			n++
		}
	}
	return n
}

// PublishedAppCount returns the number of live published app records.
func (s *Server) PublishedAppCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.publishedApps)
}

// handleIngressCollectionLocked serves POST .../ingress. Idempotent over
// (environment, cluster_id, hostname_prefix) -> 200 + existing. The ingress
// precondition (cluster known/business/active/public_ip) maps unknown clusters
// to 404 and unsuitable ones to 409.
func (s *Server) handleIngressCollectionLocked(w http.ResponseWriter, r *http.Request, env *environment) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var spec ingressSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil ||
		spec.ClusterID == "" || spec.HostnamePrefix == "" || spec.Service == "" ||
		spec.Port < 1 || spec.Port > 65535 {
		writeError(w, http.StatusUnprocessableEntity,
			"invalid ingress spec: cluster_id, service, port (1-65535) and hostname_prefix are required",
			"ValidationError")
		return
	}
	if len(spec.HostnamePrefix) > 63 || !hostnamePrefixPattern.MatchString(spec.HostnamePrefix) {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("invalid hostname_prefix %q: must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ (max 63, no leading/trailing hyphen)", spec.HostnamePrefix),
			"ValidationError")
		return
	}
	// This release supports only tls=auto (server rejects "off" with 422).
	if spec.TLS != "" && spec.TLS != "auto" {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("invalid tls %q: only \"auto\" is currently supported", spec.TLS),
			"ValidationError")
		return
	}
	// Idempotent re-apply over (environment, cluster_id, hostname_prefix).
	for _, ing := range s.ingresses {
		if ing.envID != env.ID || ing.deleted {
			continue
		}
		if ing.clusterID == spec.ClusterID && ing.hostnamePrefix == spec.HostnamePrefix {
			writeJSON(w, http.StatusOK, ing)
			return
		}
	}
	// Precondition: the cluster must belong to this environment (404), be a
	// business cluster, be active and carry a public IP (409 otherwise).
	cl, ok := s.clusters[spec.ClusterID]
	if !ok || cl.envID != env.ID || cl.deleted {
		writeError(w, http.StatusNotFound,
			"cluster not found or already torn down", "NotFound")
		return
	}
	if cl.Kind != "business" {
		writeError(w, http.StatusConflict,
			"ingress is only available for business clusters", "IngressClusterInvalid")
		return
	}
	if cl.Status != "active" {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("cluster is %q — ingress requires active", cl.Status), "IngressClusterInvalid")
		return
	}
	if cl.publicIP == "" {
		writeError(w, http.StatusConflict,
			"business cluster has no public IP yet — ingress not possible", "IngressClusterInvalid")
		return
	}
	ing := &ingress{
		ID:     newUUID(),
		Status: "provisioning",
		// public_url is the real L4 endpoint https://<public_ip>[:<port>]
		// (no L7 HTTPRoute/cert/DNS yet), mirroring the server.
		PublicURL:      strPtr(mockPublicURL(cl.publicIP, spec.Port)),
		envID:          env.ID,
		clusterID:      spec.ClusterID,
		hostnamePrefix: spec.HostnamePrefix,
	}
	s.ingresses[ing.ID] = ing
	writeJSON(w, http.StatusAccepted, ing)
}

func (s *Server) handleIngressItemLocked(w http.ResponseWriter, r *http.Request, env *environment, ingressID string) {
	ing, ok := s.ingresses[ingressID]
	if !ok || ing.envID != env.ID {
		writeError(w, http.StatusNotFound, "ingress not found", "NotFound")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if ing.deleted {
			ing.goneGETs++
			if ing.goneGETs > s.IngressGoneAfterGETs {
				delete(s.ingresses, ingressID)
				writeError(w, http.StatusNotFound, "ingress not found", "NotFound")
				return
			}
			writeJSON(w, http.StatusOK, ing) // status=destroyed
			return
		}
		writeJSON(w, http.StatusOK, ing) // always status=provisioning while live
	case http.MethodDelete:
		// Idempotent: repeated DELETE on a destroyed ingress is still a 202.
		// The row stays readable as destroyed for IngressGoneAfterGETs GETs.
		if !ing.deleted {
			ing.deleted = true
			ing.Status = "destroyed"
			ing.goneGETs = 0
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleEgressCollectionLocked serves POST .../egress. Idempotent over
// (environment, cluster_id, destination_cidr, protocol, port_range) -> 200 +
// existing. An unknown/foreign cluster -> 404; a missing/invalid CIDR -> 422.
func (s *Server) handleEgressCollectionLocked(w http.ResponseWriter, r *http.Request, env *environment) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var spec egressSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil || spec.ClusterID == "" {
		writeError(w, http.StatusBadRequest, "invalid egress spec: cluster_id is required", "BadRequest")
		return
	}
	if !validEgressCIDR(spec.DestinationCIDR) {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("destination_cidr %q is not a valid CIDR/host", spec.DestinationCIDR),
			"ValidationError")
		return
	}
	proto := spec.Protocol
	if proto == "" {
		proto = "any"
	}
	portRange := ""
	if spec.PortRange != nil {
		portRange = *spec.PortRange
	}
	// Idempotent re-apply over the natural key.
	for _, eg := range s.egresses {
		if eg.envID != env.ID || eg.deleted {
			continue
		}
		if eg.clusterID == spec.ClusterID && eg.destinationCIDR == spec.DestinationCIDR &&
			eg.protocol == proto && eg.portRange == portRange {
			writeJSON(w, http.StatusOK, eg)
			return
		}
	}
	// cluster_id must belong to this environment (server double-scoped lookup).
	cl, ok := s.clusters[spec.ClusterID]
	if !ok || cl.envID != env.ID || cl.deleted {
		writeError(w, http.StatusNotFound,
			"cluster not found or already torn down", "NotFound")
		return
	}
	// Mirror the server egress preconditions (networking.py
	// _resolve_egress_cluster / _resolve_cluster_source_cidr): egress is
	// cluster-scoped and requires an active cluster with a resolvable workload
	// CIDR. Only business clusters carry one; flex/namespace share a host VPC.
	if cl.Status != "active" {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("cluster is %q — egress requires active", cl.Status), "IngressClusterInvalid")
		return
	}
	if cl.Kind != "business" {
		writeError(w, http.StatusUnprocessableEntity,
			"egress is cluster-scoped and requires a business cluster with its own workload CIDR; "+
				"flex/namespace share a host VPC without a dedicated cluster_cidr",
			"EgressClusterCidrUnresolvable")
		return
	}
	eg := &egress{
		ID:              newUUID(),
		Status:          "provisioning",
		envID:           env.ID,
		clusterID:       spec.ClusterID,
		destinationCIDR: spec.DestinationCIDR,
		protocol:        proto,
		portRange:       portRange,
	}
	s.egresses[eg.ID] = eg
	writeJSON(w, http.StatusAccepted, eg)
}

func (s *Server) handleEgressItemLocked(w http.ResponseWriter, r *http.Request, env *environment, egressID string) {
	eg, ok := s.egresses[egressID]
	if !ok || eg.envID != env.ID {
		writeError(w, http.StatusNotFound, "egress not found", "NotFound")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if eg.deleted {
			eg.goneGETs++
			if eg.goneGETs > s.EgressGoneAfterGETs {
				delete(s.egresses, egressID)
				writeError(w, http.StatusNotFound, "egress not found", "NotFound")
				return
			}
			writeJSON(w, http.StatusOK, eg) // status=destroyed
			return
		}
		writeJSON(w, http.StatusOK, eg) // always status=provisioning while live
	case http.MethodDelete:
		if !eg.deleted {
			eg.deleted = true
			eg.Status = "destroyed"
			eg.goneGETs = 0
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePublishedAppPath(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid or missing bearer token", "Unauthorized")
		return
	}

	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/k8s/namespaces/"), "/")
	if len(segs) < 2 || len(segs) > 3 || segs[0] == "" || segs[1] != "published-apps" {
		http.NotFound(w, r)
		return
	}
	namespaceID := segs[0]

	s.mu.Lock()
	defer s.mu.Unlock()

	cl, ok := s.clusters[namespaceID]
	if !ok || cl.deleted || (cl.Kind != "namespace" && cl.Kind != "flex") {
		writeError(w, http.StatusNotFound, "namespace not found", "NotFound")
		return
	}
	if cl.Status != "active" {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("namespace is %q — published apps require active", cl.Status),
			"PublishedAppNamespaceInvalid")
		return
	}

	if len(segs) == 2 {
		s.handlePublishedAppCollectionLocked(w, r, cl)
		return
	}
	appID, err := strconv.ParseInt(segs[2], 10, 64)
	if err != nil || appID < 1 {
		writeError(w, http.StatusNotFound, "published app not found", "NotFound")
		return
	}
	s.handlePublishedAppItemLocked(w, r, namespaceID, appID)
}

func (s *Server) handlePublishedAppCollectionLocked(w http.ResponseWriter, r *http.Request, cl *cluster) {
	switch r.Method {
	case http.MethodGet:
		list := make([]*publishedApp, 0, len(s.publishedApps))
		for _, app := range s.publishedApps {
			if app.namespaceID == cl.ID {
				list = append(list, app)
			}
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		var spec publishedAppSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeError(w, http.StatusBadRequest, "invalid published app spec", "BadRequest")
			return
		}
		if spec.VclusterNamespace == "" {
			spec.VclusterNamespace = "default"
		}
		if errDetail := validatePublishedAppSpec(spec); errDetail != "" {
			writeError(w, http.StatusUnprocessableEntity, errDetail, "ValidationError")
			return
		}

		hostname := mockPublishedAppHostname(spec.AppSlug, cl.Kind)
		for _, app := range s.publishedApps {
			if app.Hostname != hostname {
				continue
			}
			if publishedAppTargetMatches(app, cl.ID, spec) {
				writeJSON(w, http.StatusOK, app)
				return
			}
			writeError(w, http.StatusConflict,
				fmt.Sprintf("published app hostname %q already exists with a different target", hostname),
				"PublishedAppHostnameConflict")
			return
		}

		app := &publishedApp{
			ID:                s.nextPublishedID,
			Hostname:          hostname,
			AppSlug:           spec.AppSlug,
			ServiceName:       spec.ServiceName,
			ServicePort:       spec.ServicePort,
			VclusterNamespace: spec.VclusterNamespace,
			PathPrefix:        spec.PathPrefix,
			TLSMode:           "auto",
			namespaceID:       cl.ID,
		}
		s.nextPublishedID++
		s.publishedApps[app.ID] = app
		writeJSON(w, http.StatusCreated, app)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePublishedAppItemLocked(w http.ResponseWriter, r *http.Request, namespaceID string, appID int64) {
	app, ok := s.publishedApps[appID]
	if !ok || app.namespaceID != namespaceID {
		writeError(w, http.StatusNotFound, "published app not found", "NotFound")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, app)
	case http.MethodDelete:
		delete(s.publishedApps, appID)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func validatePublishedAppSpec(spec publishedAppSpec) string {
	if len(spec.AppSlug) < 3 || len(spec.AppSlug) > 63 || !publishedAppSlugPattern.MatchString(spec.AppSlug) {
		return fmt.Sprintf("invalid app_slug %q: must match ^[a-z0-9]+(-[a-z0-9]+)*$ (3-63 chars)", spec.AppSlug)
	}
	if reservedPublishedAppSlugs[spec.AppSlug] {
		return fmt.Sprintf("invalid app_slug %q: reserved platform slug", spec.AppSlug)
	}
	if len(spec.ServiceName) < 1 || len(spec.ServiceName) > 63 || !hostnamePrefixPattern.MatchString(spec.ServiceName) {
		return fmt.Sprintf("invalid service_name %q: must be a Kubernetes DNS label", spec.ServiceName)
	}
	if spec.ServicePort < 1 || spec.ServicePort > 65535 {
		return fmt.Sprintf("invalid service_port %d: must be between 1 and 65535", spec.ServicePort)
	}
	if len(spec.VclusterNamespace) < 1 || len(spec.VclusterNamespace) > 63 ||
		!hostnamePrefixPattern.MatchString(spec.VclusterNamespace) {
		return fmt.Sprintf("invalid vcluster_namespace %q: must be a Kubernetes DNS label", spec.VclusterNamespace)
	}
	if spec.PathPrefix != nil {
		if len(*spec.PathPrefix) < 1 || len(*spec.PathPrefix) > 255 || !strings.HasPrefix(*spec.PathPrefix, "/") {
			return fmt.Sprintf("invalid path_prefix %q: must start with / and be at most 255 chars", *spec.PathPrefix)
		}
	}
	return ""
}

func publishedAppTier(kind string) string {
	if kind == "flex" {
		return "flex"
	}
	return "free"
}

func mockPublishedAppHostname(appSlug, kind string) string {
	return fmt.Sprintf("%s-t%d.%s.k8s.focusnet.de", appSlug, mockPublishedAppTenantID, publishedAppTier(kind))
}

func publishedAppTargetMatches(app *publishedApp, namespaceID string, spec publishedAppSpec) bool {
	return app.namespaceID == namespaceID &&
		app.AppSlug == spec.AppSlug &&
		app.ServiceName == spec.ServiceName &&
		app.ServicePort == spec.ServicePort &&
		app.VclusterNamespace == spec.VclusterNamespace &&
		publishedAppPathPrefix(app.PathPrefix) == publishedAppPathPrefix(spec.PathPrefix)
}

func publishedAppPathPrefix(prefix *string) string {
	if prefix == nil {
		return ""
	}
	return *prefix
}

// validEgressCIDR accepts an IPv4/IPv6 CIDR or a bare host IP (the server's
// repo.create_egress_rule rejects empty/garbage with a 422). Mirrors that
// shape with the standard library.
func validEgressCIDR(s string) bool {
	if s == "" {
		return false
	}
	if _, _, err := net.ParseCIDR(s); err == nil {
		return true
	}
	return net.ParseIP(s) != nil
}

func (s *Server) handleIaasVdcCollectionLocked(w http.ResponseWriter, r *http.Request, env *environment) {
	switch r.Method {
	case http.MethodGet:
		list := make([]*iaasVdc, 0, len(s.iaasVdcs))
		for _, vdc := range s.iaasVdcs {
			if vdc.envID == env.ID && !vdc.deleted {
				list = append(list, vdc)
			}
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		var spec iaasVdcSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeError(w, http.StatusBadRequest, "invalid iaas vdc spec", "BadRequest")
			return
		}
		if !validIaasName(spec.Name) {
			writeError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("invalid iaas vdc name %q: must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ (max 63, no leading/trailing hyphen)", spec.Name),
				"ValidationError")
			return
		}
		if env.Status != "active" {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("environment is not active (status=%q)", env.Status),
				"EnvironmentNotActive")
			return
		}
		for _, vdc := range s.iaasVdcs {
			if vdc.envID == env.ID && !vdc.deleted && vdc.Name == spec.Name {
				writeJSON(w, http.StatusOK, vdc)
				return
			}
		}
		seq := s.nextIaasVdcSeq
		s.nextIaasVdcSeq++
		scopeKey := strconv.FormatInt(seq, 10)
		vdc := &iaasVdc{
			ID:             newUUID(),
			EnvironmentID:  env.ID,
			Name:           spec.Name,
			Status:         "planned",
			IaasVdcSeq:     seq,
			GatewayScope:   "iaas_vdc",
			ScopeKey:       scopeKey,
			GatewayVPCName: "iaas-" + scopeKey,
			GatewayName:    "gw-iaas-" + scopeKey,
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
			envID:          env.ID,
		}
		s.iaasVdcs[vdc.ID] = vdc
		writeJSON(w, http.StatusAccepted, vdc)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleIaasVdcItemLocked(w http.ResponseWriter, r *http.Request, env *environment, vdcID string) {
	vdc, ok := s.iaasVdcs[vdcID]
	if !ok || vdc.envID != env.ID || vdc.deleted {
		writeError(w, http.StatusNotFound, "IaaS-vDC not found", "NotFound")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, vdc)
	case http.MethodDelete:
		vdc.deleted = true
		vdc.Status = "destroyed"
		vdc.goneGETs = 0
		for _, network := range s.iaasNetworks {
			if network.vdcID == vdc.ID && !network.deleted {
				network.deleted = true
				network.Status = "destroyed"
				network.goneGETs = 0
			}
		}
		writeJSON(w, http.StatusAccepted, vdc)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleIaasNetworkCollectionLocked(w http.ResponseWriter, r *http.Request, env *environment, vdcID string) {
	vdc, ok := s.iaasVdcs[vdcID]
	if !ok || vdc.envID != env.ID || vdc.deleted {
		writeError(w, http.StatusNotFound, "IaaS-vDC not found", "NotFound")
		return
	}
	switch r.Method {
	case http.MethodGet:
		list := make([]*iaasNetwork, 0, len(s.iaasNetworks))
		for _, network := range s.iaasNetworks {
			if network.envID == env.ID && network.vdcID == vdc.ID && !network.deleted {
				list = append(list, network)
			}
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		var spec iaasNetworkSpec
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			writeError(w, http.StatusBadRequest, "invalid iaas network spec", "BadRequest")
			return
		}
		if !validIaasName(spec.Name) {
			writeError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("invalid iaas network name %q: must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ (max 63, no leading/trailing hyphen)", spec.Name),
				"ValidationError")
			return
		}
		cidr, ok := normalizeCIDR(spec.CIDR)
		if !ok {
			writeError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("invalid CIDR %q", spec.CIDR),
				"ValidationError")
			return
		}
		policyDefault := spec.PolicyDefault
		if policyDefault == "" {
			policyDefault = "private"
		}
		if policyDefault != "private" && policyDefault != "allow" {
			writeError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("invalid policy_default %q: must be private or allow", spec.PolicyDefault),
				"ValidationError")
			return
		}
		if env.Status != "active" {
			writeError(w, http.StatusConflict,
				fmt.Sprintf("environment is not active (status=%q)", env.Status),
				"EnvironmentNotActive")
			return
		}
		for _, network := range s.iaasNetworks {
			if network.vdcID != vdc.ID || network.deleted {
				continue
			}
			if network.Name == spec.Name {
				if network.CIDR != cidr {
					writeError(w, http.StatusConflict,
						"IaaS-vDC network exists with a different CIDR",
						"Conflict")
					return
				}
				writeJSON(w, http.StatusOK, network)
				return
			}
			if network.CIDR == cidr {
				writeError(w, http.StatusConflict,
					"IaaS-vDC network already exists with this name or CIDR",
					"Conflict")
				return
			}
		}
		network := &iaasNetwork{
			ID:            newUUID(),
			IaasVdcID:     vdc.ID,
			EnvironmentID: env.ID,
			Name:          spec.Name,
			CIDR:          cidr,
			PolicyDefault: policyDefault,
			Status:        "planned",
			CreatedAt:     time.Now().UTC().Format(time.RFC3339),
			envID:         env.ID,
			vdcID:         vdc.ID,
		}
		s.iaasNetworks[network.ID] = network
		writeJSON(w, http.StatusAccepted, network)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleIaasNetworkItemLocked(w http.ResponseWriter, r *http.Request, env *environment, vdcID string, networkID string) {
	vdc, ok := s.iaasVdcs[vdcID]
	if !ok || vdc.envID != env.ID || vdc.deleted {
		writeError(w, http.StatusNotFound, "IaaS-vDC not found", "NotFound")
		return
	}
	network, ok := s.iaasNetworks[networkID]
	if !ok || network.envID != env.ID || network.vdcID != vdc.ID || network.deleted {
		writeError(w, http.StatusNotFound, "IaaS-vDC network not found", "NotFound")
		return
	}
	switch r.Method {
	case http.MethodGet:
		if network.Status == "planned" || network.Status == "provisioning" {
			network.Status = "provisioning"
			network.getCount++
			if network.getCount >= s.IaasNetworkReadyAfterGETs {
				if s.FailIaasNetworkProvisioning {
					network.Status = "error"
				} else {
					vlan := int64(3800 + len(s.iaasNetworks))
					network.Status = "active"
					network.VLANID = &vlan
					network.HarvesterNamespace = fmt.Sprintf("iaas-vdc-%d", vdc.IaasVdcSeq)
					network.HarvesterNADName = fmt.Sprintf("iaas-v%d", vlan)
					network.KubeovnSubnetName = fmt.Sprintf("inside-iaas-%d", vdc.IaasVdcSeq)
					network.DefaultGatewayIP = mockGatewayIP(network.CIDR)
				}
			}
		}
		writeJSON(w, http.StatusOK, network)
	case http.MethodDelete:
		network.deleted = true
		network.Status = "destroyed"
		network.goneGETs = 0
		writeJSON(w, http.StatusAccepted, network)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func mockGatewayIP(cidr string) string {
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return ""
	}
	ip = ip.To4()
	if ip == nil {
		return ""
	}
	broadcast := net.IP(make([]byte, len(ip)))
	copy(broadcast, ip)
	for i := range ip {
		broadcast[i] |= ^ipNet.Mask[i]
	}
	return net.IPv4(broadcast[0], broadcast[1], broadcast[2], broadcast[3]-5).String()
}

func validIaasName(s string) bool {
	return len(s) >= 1 && len(s) <= 63 && hostnamePrefixPattern.MatchString(s)
}

func normalizeCIDR(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	_, ipNet, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil {
		return "", false
	}
	return ipNet.String(), true
}

func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid or missing bearer token", "Unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	usedVMs, usedVCPU, usedRAM := 0, 0, 0
	for _, v := range s.vms {
		if v.deleted || v.Status == "error" {
			continue
		}
		usedVMs++
		cpu, ram := v.spec.CPUCores, v.spec.MemoryGB
		if cpu == 0 {
			cpu = 2 // server defaults (VmSpec)
		}
		if ram == 0 {
			ram = 4
		}
		usedVCPU += int(cpu)
		usedRAM += int(ram)
	}
	for _, cl := range s.clusters {
		if cl.deleted {
			continue
		}
		// Clusters and VMs share max_vcpu/max_ram_gb (server accounting);
		// only explicit custom sizing counts in the mock.
		usedVCPU += int(cl.spec.VCPU)
		usedRAM += int(cl.spec.RAMGB)
	}
	writeJSON(w, http.StatusOK, map[string]int{
		"max_concurrent_environments": s.QuotaMaxEnvironments,
		"used_environments":           s.liveEnvCountLocked(),
		"max_vms":                     s.QuotaMaxVMs,
		"used_vms":                    usedVMs,
		"max_vcpu":                    s.QuotaMaxVCPU,
		"used_vcpu":                   usedVCPU,
		"max_ram_gb":                  s.QuotaMaxRAMGB,
		"used_ram_gb":                 usedRAM,
		"max_public_ips":              s.QuotaMaxPublicIPs,
		"used_public_ips":             0,
	})
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "invalid or missing bearer token", "Unauthorized")
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	images := make([]Image, len(s.Images))
	copy(images, s.Images)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, images)
}

// renderKubeconfig builds a minimal kubeconfig embedding the short-lived
// SA token (test fixture only).
func renderKubeconfig(apiServerURL, saToken string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: sandbox
  cluster:
    server: %s
contexts:
- name: sandbox
  context:
    cluster: sandbox
    user: sandbox
current-context: sandbox
users:
- name: sandbox
  user:
    token: %s
`, apiServerURL, saToken)
}

// newUUID returns a random RFC-4122 v4 UUID without external dependencies.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable in tests
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
