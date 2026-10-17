package agent

import (
	"context"
	"errors"
)

// Each native observation owns a command budget; preceding observations must
// not consume it. The caller still bounds the complete health operation.
func boundedHealthRunner(run ToolRunner) ToolRunner {
	return func(ctx context.Context, argv []string, started func(int)) ToolResult {
		probeCtx, cancel := context.WithTimeout(ctx, kubernetesStatusProbeTimeout)
		defer cancel()
		result := run(probeCtx, argv, started)
		if err := probeCtx.Err(); err != nil {
			result.Err = errors.Join(result.Err, err)
			if result.ExitStatus == 0 {
				result.ExitStatus = 1
			}
		}
		return result
	}
}
