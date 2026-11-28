package redact

import "testing"

func TestPrivateKeys(t *testing.T) {
	const certificate = "-----BEGIN CERTIFICATE-----\npublic\n-----END CERTIFICATE-----"
	for _, test := range []struct {
		name, input, want string
	}{
		{
			name:  "terminated blocks",
			input: "before\n-----BEGIN RSA PRIVATE KEY-----\nsecret-one\n-----END RSA PRIVATE KEY-----\nbetween\n-----BEGIN OPENSSH PRIVATE KEY-----\nsecret-two\n-----END OPENSSH PRIVATE KEY-----\nafter\n",
			want:  "before\n[REDACTED PRIVATE KEY]\nbetween\n[REDACTED PRIVATE KEY]\nafter\n",
		},
		{
			name:  "unterminated block",
			input: "before\n-----BEGIN PRIVATE KEY-----\nsecret\ntruncated log line",
			want:  "before\n[REDACTED PRIVATE KEY]",
		},
		{
			name:  "JSON escaped",
			input: `{"output":"before\n-----BEGIN EC PRIVATE KEY-----\nsecret\n-----END EC PRIVATE KEY-----\nafter","certificate":"-----BEGIN CERTIFICATE-----\npublic\n-----END CERTIFICATE-----"}`,
			want:  `{"output":"before\n[REDACTED PRIVATE KEY]\nafter","certificate":"-----BEGIN CERTIFICATE-----\npublic\n-----END CERTIFICATE-----"}`,
		},
		{
			name:  "certificate",
			input: certificate,
			want:  certificate,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := PrivateKeys(test.input); got != test.want {
				t.Fatalf("PrivateKeys() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPrivateKeyFields(t *testing.T) {
	input := `{"management":{"serverPrivateKey" : "escaped \"quote\" and \\ slash","serverPrivateKeyPath":"/path/to/key"},"nodes":[{"privateKey":"raw","clientPrivateKey":"raw","server_private_key":"raw","client-key-data":"raw","name":"node"}]}`
	want := `{"management":{"serverPrivateKey" : "[REDACTED PRIVATE KEY]","serverPrivateKeyPath":"/path/to/key"},"nodes":[{"privateKey":"[REDACTED PRIVATE KEY]","clientPrivateKey":"[REDACTED PRIVATE KEY]","server_private_key":"[REDACTED PRIVATE KEY]","client-key-data":"[REDACTED PRIVATE KEY]","name":"node"}]}`
	if got := PrivateKeyFields(input); got != want {
		t.Fatalf("PrivateKeyFields() = %q, want %q", got, want)
	}
}

func TestKubeconfigData(t *testing.T) {
	input := "clusters:\n- cluster:\n    certificate-authority-data: Q0E=\n    server: https://10.0.0.1:6443\nusers:\n- name: admin\n  user:\n    client-certificate-data: Q0VSVA==\n    client-key-data: LS0tLS1CRUdJTiBQUklWQVRFIEtFWS0tLS0t\n"
	want := "clusters:\n- cluster:\n    certificate-authority-data: Q0E=\n    server: https://10.0.0.1:6443\nusers:\n- name: admin\n  user:\n    client-certificate-data: [REDACTED]\n    client-key-data: [REDACTED]\n"
	if got := KubeconfigData(input); got != want {
		t.Fatalf("KubeconfigData() = %q, want %q", got, want)
	}
}
