package cli

import (
	"github.com/useteploy/teploy/internal/cloud"
)

// Machine envelopes of `teploy cloud ... --json` (capability cloud-v1,
// contracts/schema/cloud-*.schema.json). Every document carries
// machine_interface at its root like the rest of the CLI; field names are
// the CLI's snake_case, not Teploy Cloud's camelCase wire names, so the
// contract does not move when the Cloud API renames a field.

// placeholderAddress is what Teploy Cloud stores until the provider
// reports the machine's IPv4 (teploy-cloud provisioning.go).
const placeholderAddress = "0.0.0.0"

// realAddress returns addr, or "" while it is empty or the placeholder.
func realAddress(addr string) string {
	if addr == placeholderAddress || addr == "::" {
		return ""
	}
	return addr
}

// cloudServerDTO is one Teploy Cloud server. ip_address is empty until the
// provider has assigned an address.
type cloudServerDTO struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Status            string `json:"status"`
	IPAddress         string `json:"ip_address"`
	Provider          string `json:"provider"`
	Region            string `json:"region"`
	Size              string `json:"size"`
	PriceMonthlyCents int64  `json:"price_monthly_cents"`
}

func cloudServerRow(s cloud.Server) cloudServerDTO {
	return cloudServerDTO{
		ID: s.ID, Name: s.Name, Status: s.Status, IPAddress: realAddress(s.IPAddress),
		Provider: s.Provider, Region: s.ProviderRegion, Size: s.ProviderSize,
		PriceMonthlyCents: s.PriceMonthlyCents,
	}
}

// cloudStatusDTO is `teploy cloud status --json`. Under --json the command
// exits 0 whatever the key check says: key_works and error are the report.
type cloudStatusDTO struct {
	MachineInterface int    `json:"machine_interface"`
	BaseURL          string `json:"base_url"`
	LoggedIn         bool   `json:"logged_in"`
	OrgID            string `json:"org_id,omitempty"`
	KeyWorks         bool   `json:"key_works"`
	Error            string `json:"error,omitempty"`
}

// cloudServersDTO is `teploy cloud servers --json`.
type cloudServersDTO struct {
	MachineInterface int              `json:"machine_interface"`
	Servers          []cloudServerDTO `json:"servers"`
}

func cloudServersEnvelope(servers []cloud.Server) cloudServersDTO {
	rows := make([]cloudServerDTO, 0, len(servers))
	for _, s := range servers {
		rows = append(rows, cloudServerRow(s))
	}
	return cloudServersDTO{MachineInterface: MachineInterface, Servers: rows}
}

type cloudProviderDTO struct {
	Type             string `json:"type"`
	Name             string `json:"name"`
	Description      string `json:"description,omitempty"`
	ManagedAvailable bool   `json:"managed_available"`
}

// cloudProvidersDTO is `teploy cloud regions --json` without --provider.
type cloudProvidersDTO struct {
	MachineInterface int                `json:"machine_interface"`
	Providers        []cloudProviderDTO `json:"providers"`
}

func cloudProvidersEnvelope(providers []cloud.Provider) cloudProvidersDTO {
	rows := make([]cloudProviderDTO, 0, len(providers))
	for _, p := range providers {
		rows = append(rows, cloudProviderDTO{Type: p.Type, Name: p.Name, Description: p.Description, ManagedAvailable: p.ManagedAvailable})
	}
	return cloudProvidersDTO{MachineInterface: MachineInterface, Providers: rows}
}

type cloudRegionDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Country   string `json:"country,omitempty"`
	Continent string `json:"continent,omitempty"`
	Available bool   `json:"available"`
}

// cloudRegionsDTO is `teploy cloud regions --provider P --json`.
type cloudRegionsDTO struct {
	MachineInterface int              `json:"machine_interface"`
	Provider         string           `json:"provider"`
	Regions          []cloudRegionDTO `json:"regions"`
}

func cloudRegionsEnvelope(provider string, regions []cloud.Region) cloudRegionsDTO {
	rows := make([]cloudRegionDTO, 0, len(regions))
	for _, r := range regions {
		rows = append(rows, cloudRegionDTO{ID: r.ID, Name: r.Name, Country: r.Country, Continent: r.Continent, Available: r.Available})
	}
	return cloudRegionsDTO{MachineInterface: MachineInterface, Provider: provider, Regions: rows}
}

type cloudSizeDTO struct {
	ID                string `json:"id"`
	Slug              string `json:"slug,omitempty"`
	Name              string `json:"name,omitempty"`
	VCPUs             int    `json:"vcpus"`
	MemoryMB          int    `json:"memory_mb"`
	DiskGB            int    `json:"disk_gb"`
	PriceMonthlyCents int64  `json:"price_monthly_cents"`
}

// cloudSizesDTO is `teploy cloud sizes --provider P [--region R] --json`.
type cloudSizesDTO struct {
	MachineInterface int            `json:"machine_interface"`
	Provider         string         `json:"provider"`
	Region           string         `json:"region,omitempty"`
	Sizes            []cloudSizeDTO `json:"sizes"`
}

func cloudSizesEnvelope(provider, region string, sizes []cloud.Size) cloudSizesDTO {
	rows := make([]cloudSizeDTO, 0, len(sizes))
	for _, s := range sizes {
		rows = append(rows, cloudSizeDTO{ID: s.ID, Slug: s.Slug, Name: s.Name, VCPUs: s.VCPUs, MemoryMB: s.Memory, DiskGB: s.Disk, PriceMonthlyCents: s.PriceMonthlyCents})
	}
	return cloudSizesDTO{MachineInterface: MachineInterface, Provider: provider, Region: region, Sizes: rows}
}

// Setup outcomes in cloudServerResultDTO.
const (
	cloudSetupDone    = "done"
	cloudSetupSkipped = "skipped"
	cloudSetupNotRun  = "not_run"
)

// cloudServerResultDTO is `teploy cloud server create --json` and
// `teploy cloud server attach --json`. It is printed whenever a server
// exists on Teploy Cloud, including when the command then fails (ok false,
// error, and resume: the argv that finishes the job without buying again).
// When no server was bought the command prints only the error envelope.
type cloudServerResultDTO struct {
	MachineInterface int            `json:"machine_interface"`
	OK               bool           `json:"ok"`
	Server           cloudServerDTO `json:"server"`
	// Adopted is true when the create request's outcome was unknown
	// (timeout, connection error, 5xx) and the new server was found by
	// name in the organization's list afterwards.
	Adopted bool `json:"adopted"`
	// ServersYML is the servers.yml name the server was added under, or
	// "" when it was not added.
	ServersYML string   `json:"servers_yml"`
	Setup      string   `json:"setup"`
	Error      string   `json:"error,omitempty"`
	Resume     []string `json:"resume,omitempty"`
}

// cloudServerDeleteDTO is `teploy cloud server delete --json`.
type cloudServerDeleteDTO struct {
	MachineInterface  int            `json:"machine_interface"`
	OK                bool           `json:"ok"`
	Server            cloudServerDTO `json:"server"`
	ServersYMLRemoved bool           `json:"servers_yml_removed"`
}
