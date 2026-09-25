package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const outputFormatsAnnotation = "katl-output-formats"

var optionEnv = map[string]string{
	"config":   "KATLCTL_CLUSTER_CONFIG",
	"context":  "KATLCTL_CONTEXT",
	"node":     "KATLCTL_NODE",
	"endpoint": "KATLCTL_ENDPOINT",
}

// Bind defaults before parsing so explicit flags win and required flags see them.
func applyOptionEnv(command *cobra.Command) {
	for name, env := range optionEnv {
		flag := command.Flags().Lookup(name)
		if flag == nil {
			continue
		}
		// A repeated --node appends values, so an environment default cannot be overridden.
		if _, repeated := flag.Value.(*stringList); repeated {
			continue
		}
		if value := os.Getenv(env); value != "" {
			_ = command.Flags().Set(name, value)
		}
	}
	for _, child := range command.Commands() {
		applyOptionEnv(child)
	}
}

func addOutputFlag(command *cobra.Command, value *string, fallback string, formats ...string) {
	command.Flags().StringVarP(value, "output", "o", fallback, "output format: "+strings.Join(formats, ", "))
	command.Flags().Lookup("output").Annotations = map[string][]string{outputFormatsAnnotation: formats}
	_ = command.RegisterFlagCompletionFunc("output", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return formats, cobra.ShellCompDirectiveNoFileComp
	})
}

// Validate operator options before commands can contact nodes or write files.
func validateCommandFlags(command *cobra.Command, _ []string) error {
	var invalid error
	command.Flags().VisitAll(func(flag *pflag.Flag) {
		if invalid != nil {
			return
		}
		if formats := flag.Annotations[outputFormatsAnnotation]; len(formats) > 0 && !slices.Contains(formats, flag.Value.String()) {
			invalid = fmt.Errorf("--%s = %q, want %s", flag.Name, flag.Value.String(), strings.Join(formats, " or "))
		}
		if flag.Value.Type() == "duration" {
			value, err := time.ParseDuration(flag.Value.String())
			if err != nil || value <= 0 {
				invalid = fmt.Errorf("--%s must be a positive duration", flag.Name)
			}
		}
	})
	return invalid
}
