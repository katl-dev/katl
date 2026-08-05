package configbundle

import (
	"strings"
	"testing"
)

func TestSourceSchemaAcceptsConfigsAcceptedByKatl(t *testing.T) {
	partitionBacked := strings.Replace(validSourceConfig(), `            selector:
              disk:
                byID: /dev/disk/by-id/ata-cp-data
`, `            selector:
              partition:
                byVolumeName: true
`, 1)
	zeroDefaults := strings.Replace(validSourceConfig(), "    port: 6443\n", "    port: 0\n", 1)
	zeroDefaults = strings.Replace(zeroDefaults, "          files:\n", `          state: ""
          files:
`, 1)
	zeroDefaults = strings.Replace(zeroDefaults, "              content: |\n", `              mode: 0
              content: |
`, 1)
	clearedIdentity := strings.Replace(validSourceConfig(), "                byID: /dev/disk/by-id/ata-cp-data\n", `                byID: ""
                serial: data-for-cp-1
`, 1)

	schema := newSourceSchemaValidator(t)
	for _, test := range []struct {
		name   string
		source string
	}{
		{name: "whole disk", source: validSourceConfig()},
		{name: "convention partition", source: partitionBacked},
		{name: "explicit default values", source: zeroDefaults},
		{name: "clear inherited identity", source: clearedIdentity},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := schema.Validate(sourceSchemaValue(t, test.source)); err != nil {
				t.Fatalf("schema rejected valid ClusterConfig: %v", err)
			}
			if _, _, err := BuildArchive(BuildRequest{SourcePath: writeSource(t, test.source)}); err != nil {
				t.Fatalf("BuildArchive() rejected schema-valid ClusterConfig: %v", err)
			}
		})
	}
}

func TestSourceSchemaRejectsSemanticErrors(t *testing.T) {
	schema := newSourceSchemaValidator(t)
	tests := []struct {
		name   string
		source string
	}{
		{
			name:   "missing Kubernetes block",
			source: strings.Replace(validSourceConfig(), "  kubernetes:\n    version: v1.36.1\n", "", 1),
		},
		{
			name:   "missing Kubernetes version",
			source: strings.Replace(validSourceConfig(), "    version: v1.36.1\n", "", 1),
		},
		{
			name:   "empty Kubernetes version",
			source: strings.Replace(validSourceConfig(), "    version: v1.36.1", `    version: ""`, 1),
		},
		{
			name: "disk and partition",
			source: strings.Replace(validSourceConfig(), `            selector:
              disk:
                byID: /dev/disk/by-id/ata-cp-data
`, `            selector:
              disk:
                byID: /dev/disk/by-id/ata-cp-data
              partition:
                byVolumeName: true
`, 1),
		},
		{
			name: "empty partition selector",
			source: strings.Replace(validSourceConfig(), `            selector:
              disk:
                byID: /dev/disk/by-id/ata-cp-data
`, `            selector:
              partition: {}
`, 1),
		},
		{
			name: "multiple disk identities",
			source: strings.Replace(validSourceConfig(), "          byID: /dev/disk/by-id/ata-cp-root\n", `          byID: /dev/disk/by-id/ata-cp-root
          serial: duplicate-identity
`, 1),
		},
		{
			name:   "unsupported filesystem",
			source: strings.Replace(validSourceConfig(), "          filesystem: xfs\n", "          filesystem: zfs\n", 1),
		},
		{
			name:   "port out of range",
			source: strings.Replace(validSourceConfig(), "    port: 6443\n", "    port: 65536\n", 1),
		},
		{
			name:   "missing node name",
			source: strings.Replace(validSourceConfig(), "    - name: worker-1\n", "    - controlPlane: false\n", 1),
		},
		{
			name: "missing nested sysfs value",
			source: strings.Replace(validSourceConfig(), "    hostConfiguration:\n", `    hostConfiguration:
      sysfs:
        - path: /sys/module/printk/parameters/time
`, 1),
		},
		{
			name: "file content and source",
			source: strings.Replace(validSourceConfig(), `            - path: /etc/systemd/network/10-common.network
              content: |
                [Match]
                Name=enp1s0

                [Network]
                DHCP=yes
`, `            - path: /etc/example
              content: inline
              source: example.conf
`, 1),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := schema.Validate(sourceSchemaValue(t, test.source)); err == nil {
				t.Fatal("schema accepted semantically invalid ClusterConfig")
			}
		})
	}
}
