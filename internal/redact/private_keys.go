package redact

import "regexp"

const PrivateKeyMarker = "[REDACTED PRIVATE KEY]"

var (
	// A block without an END marker runs to the end of the input, which
	// covers logs truncated mid-key.
	privateKeyBlock = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?:.*?-----END [A-Z0-9 ]*PRIVATE KEY-----|.*)`)

	// The manifest and identity schemas and the management API use these names
	// for string fields. Match the JSON string grammar to preserve escaped quotes.
	privateKeyField = regexp.MustCompile(`("(?:privateKey|serverPrivateKey|clientPrivateKey|server_private_key|client-key-data)"\s*:\s*)"(?:[^"\\]|\\.)*"`)

	kubeconfigData = regexp.MustCompile(`(?im)^(\s*(?:client-certificate-data|client-key-data):\s*)\S+`)
)

// PrivateKeys replaces PEM private key blocks in text, including JSON strings
// whose line breaks have been escaped.
func PrivateKeys(text string) string {
	return privateKeyBlock.ReplaceAllString(text, PrivateKeyMarker)
}

// PrivateKeyFields replaces the values of JSON string fields that hold
// private keys.
func PrivateKeyFields(text string) string {
	return privateKeyField.ReplaceAllString(text, `${1}"`+PrivateKeyMarker+`"`)
}

// KubeconfigData replaces the client credential data of block-style YAML
// kubeconfig users.
func KubeconfigData(text string) string {
	return kubeconfigData.ReplaceAllString(text, "${1}[REDACTED]")
}
