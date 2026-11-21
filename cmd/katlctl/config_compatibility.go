package main

import (
	"fmt"
	"regexp"
)

// Older node runtimes reject NodeConfigurationChange fields introduced after
// them; these are the fields katlctl renders that such runtimes cannot decode.
var unsupportedConfigurationFields = regexp.MustCompile(`(?m)(?:^[ \t]*line [0-9]+: field sharedKubeletBase not found in type configapply\.inlineKubeadmConfig[ \t]*(?:$|;)|unsupported-domain: spec\.nodeOverrides\.[a-z0-9-]+\.kubernetesMetadata: configuration domain is not supported)`)

func configCompatibilityDiagnostic(message string) string {
	if !unsupportedConfigurationFields.MatchString(message) {
		return message
	}
	return message + "; upgrade the node runtime to the KatlOS release matching this katlctl, then repeat the configuration apply"
}

func configCompatibilityError(err error) error {
	if err == nil {
		return nil
	}
	message := configCompatibilityDiagnostic(err.Error())
	if message == err.Error() {
		return err
	}
	return fmt.Errorf("%w%s", err, message[len(err.Error()):])
}
