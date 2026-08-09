package vmtest

import (
	"encoding/binary"
	"fmt"
	mathrand "math/rand"
	"time"

	"github.com/katl-dev/katl/internal/bootstrap/cluster"
	installmanifest "github.com/katl-dev/katl/internal/installer/manifest"
	"github.com/katl-dev/katl/internal/managementidentity"
)

var vmtestManagementIdentityTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const VMTestManagementClusterName = "katl-smoke"

// VMTestManagementIdentity deterministically recreates test-only management
// trust. It is used only by libvirt fixtures; release artifacts never contain
// this authority or its operator key.
func VMTestManagementIdentity(clusterName string) (managementidentity.Bundle, error) {
	seedBytes := []byte("katl-vmtest-management-identity:" + clusterName)
	var seed uint64 = 1469598103934665603
	for _, value := range seedBytes {
		seed ^= uint64(value)
		seed *= 1099511628211
	}
	random := mathrand.New(mathrand.NewSource(int64(binary.LittleEndian.Uint64([]byte{
		byte(seed), byte(seed >> 8), byte(seed >> 16), byte(seed >> 24),
		byte(seed >> 32), byte(seed >> 40), byte(seed >> 48), byte(seed >> 56),
	}))))
	return managementidentity.Generate(managementidentity.GenerateOptions{ClusterName: clusterName, Now: vmtestManagementIdentityTime, Random: random})
}

func VMTestManagementPlanning(clusterName string, nodeNames []string) (map[string]installmanifest.ManagementIdentity, error) {
	bundle, err := VMTestManagementIdentity(clusterName)
	if err != nil {
		return nil, err
	}
	seed := int64(1)
	for _, name := range nodeNames {
		for _, value := range []byte(name) {
			seed = seed*31 + int64(value)
		}
	}
	random := mathrand.New(mathrand.NewSource(seed))
	result := make(map[string]installmanifest.ManagementIdentity, len(nodeNames))
	for _, nodeName := range nodeNames {
		credentials, _, err := managementidentity.EnsureNode(&bundle, nodeName, vmtestManagementIdentityTime, random)
		if err != nil {
			return nil, fmt.Errorf("issue vmtest management identity for %s: %w", nodeName, err)
		}
		result[nodeName] = installmanifest.ManagementIdentity{
			CACertificate: credentials.CACertificate, ServerCertificate: credentials.ServerCertificate, ServerPrivateKey: credentials.ServerPrivateKey,
		}
	}
	return result, nil
}

func VMTestManagementClient(clusterName string) (managementidentity.ClientCredentials, error) {
	bundle, err := VMTestManagementIdentity(clusterName)
	if err != nil {
		return managementidentity.ClientCredentials{}, err
	}
	return managementidentity.Client(bundle, time.Now().UTC())
}

func VMTestAgentConnector(clusterName string) (cluster.TCPAgentConnector, error) {
	client, err := VMTestManagementClient(clusterName)
	if err != nil {
		return cluster.TCPAgentConnector{}, err
	}
	return cluster.TCPAgentConnector{Credentials: &client}, nil
}
