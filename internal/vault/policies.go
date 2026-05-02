/*
Copyright 2026 OpenCHAMI Authors.
Licensed under the Apache License, Version 2.0.
*/

package vault

import "fmt"

// ServicesPolicy returns the HCL policy granting a cluster's services
// read access to its own KV secrets and the ability to issue PKI certs
// from the shared openchami-services PKI role.
//
// The policy is named PolicyServices(clusterName) when applied via
// EnsurePolicy.
func ServicesPolicy(clusterName string) string {
	p := Paths(clusterName)
	return fmt.Sprintf(`
path "%s/data/%s/*" {
  capabilities = ["read"]
}

path "%s/metadata/%s/*" {
  capabilities = ["read", "list"]
}

path "pki/issue/openchami-services" {
  capabilities = ["create", "update"]
}
`, p.KVMount, clusterName, p.KVMount, clusterName)
}
