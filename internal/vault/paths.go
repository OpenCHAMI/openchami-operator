// Copyright © 2026 OpenCHAMI a Series of LF Projects, LLC
//
// SPDX-License-Identifier: MIT

package vault

import "fmt"

// VaultPaths holds all Vault path strings for a single cluster.
// Construct via Paths(clusterName) — never build path strings manually.
type VaultPaths struct {
	// KVMount is the KV v2 mount point shared across all clusters.
	KVMount string

	// SecretPrefix is the per-cluster KV path prefix.
	// All cluster secrets live under this prefix.
	SecretPrefix string

	// DBCredentials is the path for database credentials.
	DBCredentials string

	// S3Credentials is the path for VersityGW boot-image bucket credentials.
	S3Credentials string

	// LogCredentials is the path for VersityGW log bucket credentials.
	LogCredentials string

	// TokensmithOIDC is the path for the tokensmith OIDC client secret.
	TokensmithOIDC string

	// PolicyServices is the name of the Vault policy granting read access
	// to this cluster's secrets.
	PolicyServices string

	// AppRoleServices is the AppRole name for this cluster's services.
	AppRoleServices string

	// K8sRoleServices is the Kubernetes auth role name for this cluster.
	K8sRoleServices string
}

// Paths returns the canonical VaultPaths for clusterName.
// All Vault interactions in the operator use these paths.
// The naming scheme ensures two clusters never share a path.
func Paths(clusterName string) VaultPaths {
	prefix := "openchami/" + clusterName
	role := "openchami-" + clusterName + "-services"
	return VaultPaths{
		KVMount:         "openchami",
		SecretPrefix:    prefix,
		DBCredentials:   prefix + "/db/credentials",
		S3Credentials:   prefix + "/s3/versitygw",
		LogCredentials:  prefix + "/s3/logs",
		TokensmithOIDC:  prefix + "/oidc/tokensmith-client",
		PolicyServices:  role,
		AppRoleServices: role,
		K8sRoleServices: role,
	}
}

// SecretRef returns the full KV v2 data path for a given secret path.
// Used when reading secrets via the Vault API (which adds /data/ into the path).
func (p VaultPaths) SecretRef(path string) string {
	return fmt.Sprintf("%s/data/%s", p.KVMount, path)
}
