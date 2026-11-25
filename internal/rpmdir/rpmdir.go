// Package rpmdir prepares RPM directories that mkosi reads as package
// directories.
package rpmdir

import (
	"os"
	"time"
)

// Epoch is the modification time of every prepared RPM. mkosi records the
// modification time of each package-directory file in its image cache
// manifest, so copies of identical RPMs must carry identical times for a
// cached image to match across checkouts and runners.
var Epoch = time.Unix(1, 0)

// Pin sets the modification time of the RPM at path to Epoch.
func Pin(path string) error {
	return os.Chtimes(path, Epoch, Epoch)
}
