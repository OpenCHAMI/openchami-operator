// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

// TopologySpec is the canonical schema for the openchami-{cluster}-topology
// ConfigMap. This schema is owned by the operator. Services consume it.
package reconcilers

// TopologySpec is the top-level structure serialized into the topology ConfigMap.
type TopologySpec struct {
	ClusterName string           `json:"clusterName"`
	Version     string           `json:"version"`
	GeneratedAt string           `json:"generatedAt"`
	Domain      string           `json:"domain"`
	Services    TopologyServices `json:"services"`
	Platform    TopologyPlatform `json:"platform"`
	Database    TopologyDatabase `json:"database"`
}

// TopologyServiceEntry describes a single service's connectivity information.
type TopologyServiceEntry struct {
	Endpoint     string `json:"endpoint"`
	ExternalPath string `json:"externalPath,omitempty"`
	Ready        bool   `json:"ready"`
	// JWKSURL is set only for tokensmith.
	JWKSURL string `json:"jwksURL,omitempty"`
	// S3Endpoint and S3Bucket are set only for boot-service.
	S3Endpoint string `json:"s3Endpoint,omitempty"`
	S3Bucket   string `json:"s3Bucket,omitempty"`
}

// TopologyServices lists all operator-managed service entries.
type TopologyServices struct {
	SMD             TopologyServiceEntry `json:"smd"`
	Tokensmith      TopologyServiceEntry `json:"tokensmith"`
	BootService     TopologyServiceEntry `json:"bootService"`
	MetadataService TopologyServiceEntry `json:"metadataService"`
}

// TopologyVault holds Vault connectivity for services.
type TopologyVault struct {
	Address    string `json:"address"`
	KVMount    string `json:"kvMount"`
	PathPrefix string `json:"pathPrefix"`
}

// TopologyObjectStorage holds object storage connectivity.
type TopologyObjectStorage struct {
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
}

// TopologyLogging holds log bucket connectivity.
type TopologyLogging struct {
	Endpoint      string `json:"endpoint"`
	Bucket        string `json:"bucket"`
	ParquetPrefix string `json:"parquetPrefix"`
}

// TopologyPlatform holds all platform infrastructure endpoints.
type TopologyPlatform struct {
	Vault         TopologyVault         `json:"vault"`
	ObjectStorage TopologyObjectStorage `json:"objectStorage"`
	Logging       TopologyLogging       `json:"logging"`
}

// TopologyDatabase holds PostgreSQL endpoint information.
type TopologyDatabase struct {
	ReadWriteEndpoint string `json:"readWriteEndpoint"`
	ReadOnlyEndpoint  string `json:"readOnlyEndpoint"`
}
