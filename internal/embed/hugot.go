package embed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

const (
	// MiniLMHFRepo is the HuggingFace repo we download. KnightsAnalytics
	// hosts a MiniLM derivative pre-converted to ONNX with the right
	// tokenizer files for Hugot's loader. Springdrift uses the same
	// convention.
	MiniLMHFRepo = "KnightsAnalytics/all-MiniLM-L6-v2"

	// MiniLMDimensions is the vector size produced by the chosen
	// MiniLM checkpoint. Pinned here so callers can pre-allocate
	// without round-tripping through Dimensions().
	MiniLMDimensions = 384

	// HugotMiniLMID is stamped on stored vectors so a future model
	// change is detectable as a known migration. Bump the trailing
	// "v1" only if the model identity itself changes.
	HugotMiniLMID = "hugot/all-minilm-l6-v2/v1"

	// pipelineName is Hugot's internal handle for the loaded
	// pipeline. Hugot keys pipelines by name and locks creation per
	// name; only one pipeline of this name can live in a session.
	pipelineName = "retainer-cbr-embed"

	// hugotModelDirSlug is what Hugot's DownloadModel produces from
	// MiniLMHFRepo: it slugifies "/" → "_". Computing this once at
	// the top so cache-presence checks stay readable.
	hugotModelDirSlug = "KnightsAnalytics_all-MiniLM-L6-v2"
)

// HugotConfig configures the Hugot-backed embedder.
type HugotConfig struct {
	// ModelCacheDir is the directory the model lives in. Conventional
	// path is `$WORKSPACE/data/models/`. On first construction, if
	// the model isn't present, it's downloaded here from HuggingFace.
	// Subsequent constructions load from cache.
	ModelCacheDir string

	// Logger receives lifecycle messages — first-run download notice,
	// pipeline-loaded confirmation, close-on-shutdown. Defaults to
	// slog.Default.
	Logger *slog.Logger
}

// HugotEmbedder embeds text via Hugot's pure-Go GoMLX backend. No cgo,
// no system ONNX runtime. First Embed-call cost dominated by
// pipeline construction (already paid in NewHugot); per-call cost is
// model inference only.
//
// Concurrency: serialised by an internal mutex. Hugot's pipeline
// state is not documented as concurrency-safe and the typical
// Retainer workload (one case per cycle) doesn't benefit from
// parallelism.
type HugotEmbedder struct {
	cfg HugotConfig

	mu       sync.Mutex
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
	closed   bool
}

// NewHugot constructs the embedder, downloading the MiniLM model on
// first use into ModelCacheDir. Boot cost: one HF round-trip on first
// run (~90MB), then GoMLX session init + ONNX graph load (one-time,
// seconds). Returns the constructed embedder when both succeed.
//
// Errors come from three sources: cache dir creation, model download
// (network-bound, retried by Hugot's downloader), and pipeline load
// (corrupt cache or malformed model files). On any failure the
// caller receives a wrapped error with no leaked resources — the
// session is destroyed before the error returns.
//
// Callers MUST defer e.Close() at shutdown so the GoMLX arena
// releases. Permanent supervisor stop-handlers are the right place.
func NewHugot(ctx context.Context, cfg HugotConfig) (*HugotEmbedder, error) {
	if cfg.ModelCacheDir == "" {
		return nil, errors.New("embed: HugotConfig.ModelCacheDir is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	if err := os.MkdirAll(cfg.ModelCacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("embed: mkdir model cache: %w", err)
	}

	modelDir, err := ensureMiniLM(ctx, cfg.ModelCacheDir, cfg.Logger)
	if err != nil {
		return nil, err
	}

	session, err := hugot.NewGoSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("embed: hugot go session: %w", err)
	}

	pipelineCfg := hugot.FeatureExtractionConfig{
		ModelPath:    modelDir,
		Name:         pipelineName,
		OnnxFilename: "model.onnx",
	}
	pipeline, err := hugot.NewPipeline(session, pipelineCfg)
	if err != nil {
		_ = session.Destroy()
		return nil, fmt.Errorf("embed: hugot pipeline: %w", err)
	}

	cfg.Logger.Info("embed: hugot pipeline ready",
		"id", HugotMiniLMID,
		"dimensions", MiniLMDimensions,
		"model_dir", modelDir,
	)

	return &HugotEmbedder{
		cfg:      cfg,
		session:  session,
		pipeline: pipeline,
	}, nil
}

// Embed returns the 384-dim vector for one input. Errors include
// pipeline-internal failures (rare — model corruption or context
// cancellation). Returns no vector + error in that case; CBR retrieval
// auto-renormalises weights when this happens for a single query, so
// transient failures don't take out the whole retrieval.
func (e *HugotEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, errors.New("embed: embedder closed")
	}
	out, err := e.pipeline.RunPipeline(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("embed: run pipeline: %w", err)
	}
	if len(out.Embeddings) != 1 {
		return nil, fmt.Errorf("embed: expected 1 embedding, got %d", len(out.Embeddings))
	}
	return out.Embeddings[0], nil
}

// Dimensions is the constant 384 for MiniLM-L6-v2.
func (e *HugotEmbedder) Dimensions() int { return MiniLMDimensions }

// ID stamps stored vectors so model swaps are migration-safe.
func (e *HugotEmbedder) ID() string { return HugotMiniLMID }

// Close releases the GoMLX session. Idempotent.
func (e *HugotEmbedder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.session == nil {
		return nil
	}
	return e.session.Destroy()
}

// ensureMiniLM checks the cache for a complete model (model.onnx +
// tokenizer.json). When missing or incomplete, calls Hugot's
// downloader to fetch from HuggingFace. Returns the absolute path to
// the model directory Hugot expects to load from.
//
// We deliberately don't validate the ONNX/tokenizer contents —
// Hugot's `NewPipeline` will catch a corrupt model, and a partial
// download leaves the directory in a state that triggers a re-fetch
// next start (the file checks below short-circuit on missing pieces).
func ensureMiniLM(ctx context.Context, cacheDir string, logger *slog.Logger) (string, error) {
	expectedDir := filepath.Join(cacheDir, hugotModelDirSlug)
	onnxPath := filepath.Join(expectedDir, "model.onnx")
	tokenizerPath := filepath.Join(expectedDir, "tokenizer.json")

	_, onnxErr := os.Stat(onnxPath)
	_, tokenizerErr := os.Stat(tokenizerPath)
	if onnxErr == nil && tokenizerErr == nil {
		return expectedDir, nil
	}

	logger.Info("embed: downloading model on first use",
		"repo", MiniLMHFRepo,
		"destination", cacheDir,
		"approx_size", "~90MB",
	)
	options := hugot.NewDownloadOptions()
	resultPath, err := hugot.DownloadModel(ctx, MiniLMHFRepo, cacheDir, options)
	if err != nil {
		return "", fmt.Errorf("embed: download MiniLM: %w", err)
	}
	logger.Info("embed: model downloaded", "path", resultPath)
	return resultPath, nil
}
