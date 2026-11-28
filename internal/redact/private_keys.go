package redact

import "regexp"

const PrivateKeyMarker = "[REDACTED PRIVATE KEY]"

var privateKeyBlock = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)

// PrivateKeys replaces PEM private key blocks in text, including JSON strings
// whose line breaks have been escaped.
func PrivateKeys(text string) string {
	return privateKeyBlock.ReplaceAllString(text, PrivateKeyMarker)
}
