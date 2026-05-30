package speech

import (
	"path/filepath"
)

// mossNanoDefaultModelDir is the default directory for MOSS-TTS-Nano ONNX model files.
// Package-level var (not const) to allow override in tests.
var mossNanoDefaultModelDir = ".clawbench/moss-nano-models"

// MossNanoProvider implements SpeechProvider using MOSS-TTS-Nano (local, ONNX-based TTS).
//
// MOSS-TTS-Nano is a 0.1B-parameter multilingual speech generation model from MOSI.AI
// and the OpenMOSS team. It supports real-time streaming on CPU via ONNX Runtime,
// produces 48kHz stereo WAV output, and supports ~20 languages including Chinese,
// English, Japanese, Korean, and more.
//
// Installation:
//
//	git clone https://github.com/OpenMOSS/MOSS-TTS-Nano.git
//	cd MOSS-TTS-Nano && pip install -r requirements.txt && pip install -e .
//
// CLI usage:
//
//	moss-tts-nano generate --backend onnx --text "hello" --output out.wav
type MossNanoProvider struct {
	CLISpeechProvider
	// ModelDir is the directory containing MOSS-TTS-Nano ONNX model files.
	// If empty, models are auto-downloaded by the CLI on first run (to ./models/),
	// or resolved to .clawbench/moss-nano-models/.
	ModelDir string
	// PromptSpeech is the path to a reference audio file for voice cloning.
	// If empty, the model uses a built-in voice preset ("Junhao").
	PromptSpeech string
	// Backend selects the inference backend: "onnx" (default, CPU-friendly) or "pytorch" (requires GPU).
	Backend string
	// Voice is the built-in voice preset name for ONNX backend when no prompt-speech is provided.
	// Default: "Junhao". Only used with ONNX backend.
	Voice string
}

// NewMossNanoProvider creates a MossNanoProvider with sensible defaults.
func NewMossNanoProvider() *MossNanoProvider {
	p := &MossNanoProvider{
		Backend: "onnx",
		Voice:   "Junhao",
	}

	p.CLISpeechProvider = newCLISpeechProvider(SynthesizeOptions{
		BinaryName: "moss-tts-nano",
		TextSource: TextViaTempFile,
		LogName:    "moss-nano",
		ExtraArgs: func(_ string, text string, outputPath string, _ string) []string {
			args := []string{
				"generate",
				"--backend", p.Backend,
				"--text-file", text,
				"--output", outputPath,
			}
			if p.ModelDir != "" {
				args = append(args, "--onnx-model-dir", p.ModelDir)
			}
			if p.PromptSpeech != "" {
				args = append(args, "--prompt-speech", p.PromptSpeech)
			} else if p.Voice != "" {
				args = append(args, "--voice", p.Voice)
			}
			return args
		},
	})

	return p
}

// ResolveMossNanoModelDir resolves the MOSS-TTS-Nano model directory.
// If modelDir is explicitly set, it is returned as-is.
// Otherwise, it checks the default directory (.clawbench/moss-nano-models);
// if it contains model files (browser_poc_manifest.json exists in a subdirectory),
// the default is returned. Otherwise, returns "" to let the CLI auto-download models.
func ResolveMossNanoModelDir(modelDir string) string {
	if modelDir != "" {
		return modelDir
	}
	// Check if default directory has models (look for browser_poc_manifest.json)
	defaultDir := mossNanoDefaultModelDir
	matches, _ := filepath.Glob(filepath.Join(defaultDir, "*", "browser_poc_manifest.json"))
	if len(matches) > 0 {
		return defaultDir
	}
	return "" // let CLI auto-download
}
