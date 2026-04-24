package embed

// RuntimeConfig points at the three on-disk artifacts the BGE unix path
// needs. Exposed here (not in a unix-only file) so callers on every
// platform can construct and serialize the same config object; only the
// actual loader is unix-gated.
//
//	LibraryPath: absolute path to libonnxruntime.dylib/.so
//	ModelPath:   absolute path to model.onnx
//	VocabPath:   absolute path to vocab.txt
//
// All three MUST be absolute. Relative paths defeat the extract-to-tmp
// packaging story (F3 from the spike friction log: ORT's own default
// library resolution against DYLD/LD paths is undocumented and fails in
// most environments; absolute path is the only supported contract).
type RuntimeConfig struct {
	LibraryPath string
	ModelPath   string
	VocabPath   string
	// MaxLen caps the token sequence including [CLS] and [SEP]. Zero
	// or negative picks the bge-small default of 512 at construction
	// time. Values larger than the model's position-embedding table
	// produce undefined ONNX behavior; stick to 512 unless the model
	// supports longer context.
	MaxLen int
	// NumThreads is the intra-op thread count passed to ORT's session
	// options. Zero or negative defers to ORT's default (runtime.NumCPU
	// on most builds). Set to 1 for tests or deterministic benchmarks.
	NumThreads int
}

// ORTAPIVersion is the ORT C API version this package pins to via
// shota3506/onnxruntime-purego's supportedAPIVersions check. F2 from the
// spike friction log: the library hard-codes this to 23, rejects any
// other value, and thereby ties us to ORT 1.23.x. Bumping this requires
// an explicit ADR.
const ORTAPIVersion = 23
