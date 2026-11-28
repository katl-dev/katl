package main

import (
	"bytes"
	"testing"
)

func TestExtensionBundles(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	var out bytes.Buffer
	if err := run([]string{"extension-bundles", commit}, &out, &out, nil); err != nil {
		t.Fatal(err)
	}
	want := `{"pattern":"{katl-extension-bundles-standard-0123456789abcdef0123456789abcdef01234567,katl-extension-bundles-lts-0123456789abcdef0123456789abcdef01234567}","bundles":[{"flavour":"standard","name":"katl-extension-bundles-standard-0123456789abcdef0123456789abcdef01234567"},{"flavour":"lts","name":"katl-extension-bundles-lts-0123456789abcdef0123456789abcdef01234567"}]}`
	if got := out.String(); got != want+"\n" {
		t.Errorf("inventory = %s, want %s", got, want)
	}
}

func TestExtensionBundlesRejectsShortCommit(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"extension-bundles", "0123456789ab"}, &out, &out, nil); err == nil {
		t.Fatal("extension-bundles accepted an abbreviated commit")
	}
}
