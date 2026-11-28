package persistedrecord

// The crash model is shared with this package's external tests, which drive
// callers such as the generation records through it without widening the API.

type CrashFS = crashFS

var CrashEachStep = crashEachStep

func (fs *crashFS) SeedVolatile(path string, data []byte) { fs.seedVolatile(path, data) }

func (fs *crashFS) SeedVolatileDir(path string) { fs.seedVolatileDir(path) }

func (fs *crashFS) Read(path string) ([]byte, bool) { return fs.read(path) }
