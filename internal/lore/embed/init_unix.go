// Unix constructor seam for PrepareAndProbe. BGEEmbedder itself is unix-
// only (runtime_unix.go + bge_unix.go); this file wraps it so init.go
// (which is platform-agnostic) can call newBGEEmbedderFromExt without
// build-tagging the whole init flow.

//go:build unix

package embed

import "fmt"

func newBGEEmbedderFromExt(ext *ExtractResult) (Embedder, func(), error) {
	if ext == nil {
		return nil, func() {}, fmt.Errorf("embed: newBGEEmbedderFromExt: nil ExtractResult")
	}
	emb, err := NewBGEEmbedder(RuntimeConfig{
		LibraryPath: ext.LibraryPath,
		ModelPath:   ext.ModelPath,
		VocabPath:   ext.VocabPath,
		NumThreads:  0,
	})
	if err != nil {
		return nil, func() {}, err
	}
	return emb, emb.Close, nil
}
