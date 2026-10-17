package main

import (
	"fmt"
	"regexp"
)

var unsupportedSharedKubeletBase = regexp.MustCompile(`(?m)^[ \t]*line [0-9]+: field sharedKubeletBase not found in type configapply\.inlineKubeadmConfig[ \t]*(?:$|;)`)

func configCompatibilityDiagnostic(message string) string {
	if !unsupportedSharedKubeletBase.MatchString(message) {
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
