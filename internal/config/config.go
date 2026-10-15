package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	ModelDir      string
	LLMModel      string
	LLMModelURL   string
	EmbedModel    string
	EmbedModelURL string
	GitHubToken   string
}

func Default() Config {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cache", "ai-pr-review", "models")
	return Config{
		ModelDir:      dir,
		LLMModel:      filepath.Join(dir, "gemma-3-4b-it-Q4_K_M.gguf"),
		LLMModelURL:   "https://huggingface.co/ggml-org/gemma-3-4b-it-GGUF/resolve/main/gemma-3-4b-it-Q4_K_M.gguf",
		EmbedModel:    filepath.Join(dir, "embeddinggemma-300m-qat-Q8_0.gguf"),
		EmbedModelURL: "https://huggingface.co/ggml-org/embeddinggemma-300m-qat-q8_0-GGUF/resolve/main/embeddinggemma-300m-qat-Q8_0.gguf",
		GitHubToken:   os.Getenv("GITHUB_TOKEN"),
	}
}

func EnsureDir(c Config) error {
	return os.MkdirAll(c.ModelDir, 0755)
}
