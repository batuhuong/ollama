package llama

/*
#cgo CPPFLAGS: -O3 -DNDEBUG=1
#cgo CXXFLAGS: -std=c++11
#cgo darwin CPPFLAGS: -DGGML_USE_METAL=1 -DGGML_METAL_NDEBUG=1
#cgo darwin LDFLAGS: -framework Accelerate -framework Foundation -framework Metal -framework MetalKit -framework MetalPerformanceShaders
#include <stdlib.h>
#include "llama.h"

struct llama_sample_options
{
	float repeat_penalty;
	float frequency_penalty;
	float presence_penalty;
	float temperature;
	int32_t top_k;
	float top_p;
	float tfs_z;
	float typical_p;
	int mirostat;
	float mirostat_tau;
	float mirostat_eta;
};

llama_token llama_sample(
		struct llama_context *ctx,
		struct llama_token_data *candidates,
		size_t n_candidates,
		const llama_token *last_tokens,
		size_t n_last_tokens,
		struct llama_sample_options *opts)
{
	llama_token_data_array candidates_p = {
		candidates,
		n_candidates,
		false,
	};

	llama_sample_repetition_penalty(
		ctx, &candidates_p,
		last_tokens, n_last_tokens,
		opts->repeat_penalty);

	llama_sample_frequency_and_presence_penalties(
		ctx, &candidates_p,
		last_tokens, n_last_tokens,
		opts->frequency_penalty, opts->presence_penalty);

	if (opts->temperature <= 0) {
		return llama_sample_token_greedy(ctx, &candidates_p);
	}

	if (opts->mirostat == 1) {
		int mirostat_m = 100;
		float mirostat_mu = 2.0f * opts->mirostat_tau;
		llama_sample_temperature(ctx, &candidates_p, opts->temperature);
		return llama_sample_token_mirostat(
			ctx, &candidates_p,
			opts->mirostat_tau, opts->mirostat_eta,
			mirostat_m, &mirostat_mu);
	} else if (opts->mirostat == 2) {
		float mirostat_mu = 2.0f * opts->mirostat_tau;
		llama_sample_temperature(ctx, &candidates_p, opts->temperature);
		return llama_sample_token_mirostat_v2(
			ctx, &candidates_p,
			opts->mirostat_tau, opts->mirostat_eta,
			&mirostat_mu);
	} else {
		llama_sample_top_k(ctx, &candidates_p, opts->top_k, 1);
		llama_sample_tail_free(ctx, &candidates_p, opts->tfs_z, 1);
		llama_sample_typical(ctx, &candidates_p, opts->typical_p, 1);
		llama_sample_top_p(ctx, &candidates_p, opts->top_p, 1);
		llama_sample_temperature(ctx, &candidates_p, opts->temperature);
		return llama_sample_token(ctx, &candidates_p);
	}
}
*/
import "C"
import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"unicode/utf8"
	"unsafe"

	"github.com/jmorganca/ollama/api"
)

type LLM struct {
	params *C.struct_llama_context_params
	model  *C.struct_llama_model
	ctx    *C.struct_llama_context

	lastN  deque[C.llama_token]
	embd   []C.llama_token
	cursor int

	mu sync.Mutex
	gc bool

	api.Options
}

func New(model string, opts api.Options) (*LLM, error) {
	if _, err := os.Stat(model); err != nil {
		return nil, err
	}

	llm := LLM{Options: opts}

	C.llama_backend_init(C.bool(llm.UseNUMA))

	params := C.llama_context_default_params()
	params.seed = C.uint(llm.Seed)
	params.n_ctx = C.int(llm.NumCtx)
	params.n_batch = C.int(llm.NumBatch)
	params.n_gpu_layers = C.int(llm.NumGPU)
	params.main_gpu = C.int(llm.MainGPU)
	params.low_vram = C.bool(llm.LowVRAM)
	params.f16_kv = C.bool(llm.F16KV)
	params.logits_all = C.bool(llm.LogitsAll)
	params.vocab_only = C.bool(llm.VocabOnly)
	params.use_mmap = C.bool(llm.UseMMap)
	params.use_mlock = C.bool(llm.UseMLock)
	params.embedding = C.bool(llm.EmbeddingOnly)
	llm.params = &params

	cModel := C.CString(model)
	defer C.free(unsafe.Pointer(cModel))

	llm.model = C.llama_load_model_from_file(cModel, params)
	if llm.model == nil {
		return nil, errors.New("failed to load model")
	}

	llm.ctx = C.llama_new_context_with_model(llm.model, params)
	if llm.ctx == nil {
		return nil, errors.New("failed to create context")
	}

	llm.lastN = deque[C.llama_token]{capacity: llm.NumCtx}

	// warm up the model
	bos := []C.llama_token{C.llama_token_bos()}
	C.llama_eval(llm.ctx, unsafe.SliceData(bos), C.int(len(bos)), 0, C.int(opts.NumThread))
	C.llama_reset_timings(llm.ctx)

	return &llm, nil
}

func (llm *LLM) Close() {
	llm.gc = true

	llm.mu.Lock()
	defer llm.mu.Unlock()

	defer C.llama_free_model(llm.model)
	defer C.llama_free(llm.ctx)

	C.llama_print_timings(llm.ctx)
}

func (llm *LLM) Predict(ctx []int, prompt string, fn func(api.GenerateResponse)) error {
	llm.mu.Lock()
	defer llm.mu.Unlock()

	C.llama_reset_timings(llm.ctx)

	tokens := make([]C.llama_token, len(ctx))
	for i := range tokens {
		tokens[i] = C.llama_token(ctx[i])
	}

	llm.marshalPrompt(tokens, prompt)

	var b bytes.Buffer
	for {
		token, err := llm.next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		} else if llm.gc {
			return io.EOF
		}

		b.WriteString(llm.detokenize(token))
		if utf8.Valid(b.Bytes()) || b.Len() >= utf8.UTFMax {
			fn(api.GenerateResponse{Response: b.String()})
			b.Reset()
		}
	}

	lastN := make([]int, 0, llm.lastN.Len())
	llm.lastN.Range(func(t C.llama_token) bool {
		if t != 0 {
			lastN = append(lastN, int(t))
		}

		return true
	})

	timings := C.llama_get_timings(llm.ctx)
	fn(api.GenerateResponse{
		Done:               true,
		Context:            lastN,
		PromptEvalCount:    int(timings.n_p_eval),
		PromptEvalDuration: parseDurationMs(float64(timings.t_p_eval_ms)),
		EvalCount:          int(timings.n_eval),
		EvalDuration:       parseDurationMs(float64(timings.t_eval_ms)),
	})

	return nil
}

func (llm *LLM) marshalPrompt(ctx []C.llama_token, prompt string) []C.llama_token {
	prompt = " " + prompt

	// prepare prompt
	tokens := append(ctx, llm.tokenize(prompt)...)
	if llm.NumKeep < 0 {
		llm.NumKeep = len(tokens)
	}

	// min(llm.NumCtx - 4, llm.NumKeep)
	if llm.NumCtx-4 < llm.NumKeep {
		llm.NumKeep = llm.NumCtx - 4
	}

	if len(tokens) >= llm.NumCtx {
		// truncate input
		numLeft := (llm.NumCtx - llm.NumKeep) / 2
		truncated := tokens[:llm.NumKeep]
		erasedBlocks := (len(tokens) - llm.NumKeep - numLeft - 1) / numLeft
		truncated = append(truncated, tokens[llm.NumKeep+erasedBlocks*numLeft:]...)
		llm.lastN.PushLeft(tokens[len(tokens)-llm.NumCtx:]...)

		log.Printf("input truncated: num_ctx=%d num_keep=%d num_left=%d new_tokens=%v", llm.NumCtx, llm.NumKeep, numLeft, truncated)
		tokens = truncated
	} else {
		for i := 0; i < (llm.lastN.Cap() - len(tokens)); i++ {
			llm.lastN.PushLeft(0)
		}

		llm.lastN.PushLeft(tokens...)
	}

	var i int
	for i = 0; i < len(llm.embd) && i < len(tokens) && llm.embd[i] == tokens[i]; i++ {
		// noop
	}

	llm.embd = tokens
	if i == len(tokens) {
		// evaluate at least one token to generate logits
		i--
	}

	llm.cursor = i

	log.Printf("prompt: num_past=%d cached=%v eval=%v", i, llm.detokenize(llm.embd[:i]...), llm.detokenize(llm.embd[i:]...))
	return tokens
}

func (llm *LLM) tokenize(prompt string) []C.llama_token {
	cPrompt := C.CString(prompt)
	defer C.free(unsafe.Pointer(cPrompt))

	tokens := make([]C.llama_token, len(prompt)+1)
	if n := C.llama_tokenize(llm.ctx, cPrompt, unsafe.SliceData(tokens), C.int(len(tokens)), true); n > 0 {
		return tokens[:n]
	}

	return nil
}

func (llm *LLM) detokenize(tokens ...C.llama_token) string {
	var sb strings.Builder
	for _, token := range tokens {
		sb.WriteString(C.GoString(C.llama_token_to_str(llm.ctx, token)))
	}

	return sb.String()
}

func (llm *LLM) next() (C.llama_token, error) {
	if len(llm.embd) >= llm.NumCtx {
		numLeft := (llm.NumCtx - llm.NumKeep) / 2
		truncated := llm.embd[:llm.NumKeep]
		truncated = append(truncated, llm.embd[numLeft:]...)

		log.Printf("input truncated: num_ctx=%d num_keep=%d num_left=%d new_tokens=%v", llm.NumCtx, llm.NumKeep, numLeft, truncated)
		llm.embd = truncated
		// reset cursor to the most recent token
		llm.cursor = len(truncated) - 1
	}

	for {
		if llm.cursor >= len(llm.embd) {
			break
		}

		numEval := len(llm.embd) - llm.cursor
		if numEval > llm.NumBatch {
			numEval = llm.NumBatch
		}

		if retval := C.llama_eval(llm.ctx, unsafe.SliceData(llm.embd[llm.cursor:]), C.int(numEval), C.int(llm.cursor), C.int(llm.NumThread)); retval != 0 {
			return 0, fmt.Errorf("llama_eval: %d", retval)
		}

		llm.cursor += numEval
	}

	var sampleOpts C.struct_llama_sample_options
	sampleOpts.repeat_penalty = C.float(llm.RepeatPenalty)
	sampleOpts.frequency_penalty = C.float(llm.FrequencyPenalty)
	sampleOpts.presence_penalty = C.float(llm.PresencePenalty)
	sampleOpts.temperature = C.float(llm.Temperature)
	sampleOpts.top_k = C.int(llm.TopK)
	sampleOpts.top_p = C.float(llm.TopP)
	sampleOpts.tfs_z = C.float(llm.TFSZ)
	sampleOpts.typical_p = C.float(llm.TypicalP)
	sampleOpts.mirostat = C.int(llm.Mirostat)
	sampleOpts.mirostat_tau = C.float(llm.MirostatTau)
	sampleOpts.mirostat_eta = C.float(llm.MirostatEta)

	numVocab := C.llama_n_vocab(llm.ctx)
	logits := unsafe.Slice(C.llama_get_logits(llm.ctx), numVocab)

	// TODO: logit bias

	candidates := make([]C.llama_token_data, numVocab)
	for i := range logits {
		candidates[i] = C.llama_token_data{
			id:    C.int(i),
			logit: logits[i],
			p:     0,
		}
	}

	token := C.llama_sample(
		llm.ctx,
		unsafe.SliceData(candidates), C.size_t(len(candidates)),
		unsafe.SliceData(llm.lastN.Data()), C.size_t(llm.lastN.Len()),
		&sampleOpts,
	)

	llm.lastN.PushLeft(token)
	llm.embd = append(llm.embd, token)

	if token == C.llama_token_eos() {
		return 0, io.EOF
	}

	return token, nil
}

func (llm *LLM) sample(output deque[C.llama_token], opts *C.struct_llama_sample_options) (C.llama_token, error) {
	numVocab := int(C.llama_n_vocab(llm.ctx))
	logits := unsafe.Slice(C.llama_get_logits(llm.ctx), numVocab)

	candidates := deque[C.struct_llama_token_data]{capacity: numVocab}
	for i := 0; i < candidates.Cap(); i++ {
		candidates.PushLeft(C.struct_llama_token_data{
			id:    C.int(i),
			logit: logits[i],
			p:     0,
		})
	}

	token := C.llama_sample(
		llm.ctx,
		unsafe.SliceData(candidates.Data()), C.size_t(candidates.Len()),
		unsafe.SliceData(output.Data()), C.size_t(output.Len()),
		opts)
	if token != C.llama_token_eos() {
		return token, nil
	}

	return 0, io.EOF
}
