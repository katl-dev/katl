package systemdunit

import "embed"

// RuntimeUnits is the shared source for units installed into the image and
// rendered into an installed system's state tree.
//
//go:embed units/*
var RuntimeUnits embed.FS
