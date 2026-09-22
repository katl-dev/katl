package handoff

import "io/fs"

// SetExtensionLayout supplies the immutable media artifacts before Handler is
// constructed. Only public release blobs are exposed, never the media root.
func (s *HandoffServer) SetExtensionLayout(layout fs.FS) {
	s.extensionLayout = layout
}
