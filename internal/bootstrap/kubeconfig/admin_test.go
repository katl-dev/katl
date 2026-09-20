package kubeconfig

import "testing"

func TestCredentialsSelectCurrentContext(t *testing.T) {
	data := []byte(`current-context: chosen
contexts:
- name: chosen
  context: {cluster: second, user: admin}
clusters:
- name: first
  cluster: {certificate-authority-data: WRONG}
- name: second
  cluster: {certificate-authority-data: CA}
users:
- name: other
  user: {client-certificate-data: WRONG, client-key-data: WRONG}
- name: admin
  user: {client-certificate-data: CERT, client-key-data: KEY}
`)
	got, err := ParseCredentials(data)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Credentials{CertificateAuthorityData: "CA", ClientCertificateData: "CERT", ClientKeyData: "KEY"}) {
		t.Fatalf("%#v", got)
	}
}

func TestCredentialsRequireEmbeddedData(t *testing.T) {
	for _, data := range []string{
		"clusters: [{cluster: {certificate-authority: /etc/secret}}]\nusers: [{user: {exec: {command: unsafe}}}]",
		"current-context: missing\nclusters: [{cluster: {certificate-authority-data: CA}}]\nusers: [{user: {client-certificate-data: CERT, client-key-data: KEY}}]",
	} {
		if _, err := ParseCredentials([]byte(data)); err == nil {
			t.Fatal("accepted unavailable credentials")
		}
	}
}
