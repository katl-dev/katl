package generation

import "github.com/katl-dev/katl/internal/installer/kernelcmdline"

// MergeKernelCommandLine preserves current boot options that are not owned by
// generation selection while keeping the candidate's controlled values.
func MergeKernelCommandLine(base []string, current []string) []string {
	return kernelcmdline.MergeCurrent(base, current, nil)
}

func controlledKernelCommandLineOption(option string) bool {
	return kernelcmdline.Protected(option)
}
