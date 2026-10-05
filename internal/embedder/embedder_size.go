package embedder

// resolveEmbedContextSize returns an explicit embedding context budget.
// fromModel is an operator/model-supplied GGUF context size; 0 means "use the
// model native window" (llama-go NewContext without WithContext).
func resolveEmbedContextSize(fromModel int) int {
	if fromModel > 0 {
		return fromModel
	}
	return 0
}
