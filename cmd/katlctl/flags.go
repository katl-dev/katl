package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const outputFormatsAnnotation = "katl-output-formats"

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
