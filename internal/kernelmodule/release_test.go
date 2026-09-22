package kernelmodule

import (
	"testing"
)

func TestExtensionReleaseCompatibility(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		accept  bool
	}{
		{
			name:    "interface",
			content: "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\nARCHITECTURE=x86-64\n",
			accept:  true,
		},
		{
			name:    "quoted identifiers",
			content: "ID=\"katlos\"\nSYSEXT_LEVEL='katl-runtime-1'\nSYSEXT_SCOPE=\"system portable\"\n",
			accept:  true,
		},
		{
			name:    "version fallback",
			content: "ID=katlos\nVERSION_ID=2026.9.1\n",
			accept:  true,
		},
		{
			name:    "universal metadata",
			content: "ID=_any\nARCHITECTURE=_any\n",
			accept:  true,
		},
		{
			name:    "wrong level",
			content: "ID=katlos\nSYSEXT_LEVEL=katl-runtime-2\nVERSION_ID=2026.9.1\n",
		},
		{
			name:    "wrong architecture",
			content: "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\nARCHITECTURE=arm64\n",
		},
		{
			name:    "initrd only",
			content: "ID=katlos\nSYSEXT_LEVEL=katl-runtime-1\nSYSEXT_SCOPE=initrd\n",
		},
		{
			name:    "wrong OS",
			content: "ID=other\nSYSEXT_LEVEL=katl-runtime-1\n",
		},
		{
			name:    "wrong version",
			content: "ID=katlos\nVERSION_ID=2026.8.1\n",
		},
		{
			name:    "unsupported expansion",
			content: "ID=katlos\nSYSEXT_LEVEL=$RUNTIME_INTERFACE\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeModuleFile(t, root, "usr/lib/extension-release.d/extension-release.driver", test.content)
			err := validateExtensionRelease(root, "driver.raw", "2026.9.1", "katl-runtime-1", "x86-64")
			if (err == nil) != test.accept {
				t.Fatalf("accepted = %v, error = %v", test.accept, err)
			}
			if test.accept {
				if err := validateExtensionRelease(root, "another.raw", "2026.9.1", "katl-runtime-1", "x86-64"); err == nil {
					t.Fatal("accepted metadata for a different activation filename")
				}
			}
		})
	}
}
