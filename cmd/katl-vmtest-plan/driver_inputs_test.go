package main

import "testing"

// TestDriverDigestsGolden pins the published driver digests for the fixture
// tree built by newDriverRepo. Qualification records are looked up by digest,
// so any change to these values orphans every existing record; change them
// only together with driverDigestVersion.
func TestDriverDigestsGolden(t *testing.T) {
	newDriverRepo(t)

	digests, err := committedDriverDigests()
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"standard": "v5:d0c6b98b9c10140b8ce5420eb201deb9443236b5c21affa4a80d98d73dbdf7c6",
		"lts":      "v5:f9518ba08b0b78b5f58eb25437b9b5729ba55018c22789f9bbeea2f25af9ce98",
	}
	if len(digests) != len(want) {
		t.Errorf("digests = %v, want flavours %v", digests, want)
	}
	for flavour, digest := range want {
		if digests[flavour] != digest {
			t.Errorf("%s digest = %q, want %q", flavour, digests[flavour], digest)
		}
	}
}
