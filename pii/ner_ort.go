//go:build ner

package pii

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
)

// ner backend: rampart minilm token classifier via runtime-loaded onnxruntime
// (purego, no cgo). built with -tags ner only; default build loads nothing.

var nerLabels = []string{
	"O",
	"B-GIVEN_NAME", "I-GIVEN_NAME", "B-SURNAME", "I-SURNAME",
	"B-EMAIL", "I-EMAIL", "B-PHONE", "I-PHONE", "B-URL", "I-URL",
	"B-TAX_ID", "I-TAX_ID", "B-BANK_ACCOUNT", "I-BANK_ACCOUNT",
	"B-ROUTING_NUMBER", "I-ROUTING_NUMBER", "B-GOVERNMENT_ID", "I-GOVERNMENT_ID",
	"B-PASSPORT", "I-PASSPORT", "B-DRIVERS_LICENSE", "I-DRIVERS_LICENSE",
	"B-BUILDING_NUMBER", "I-BUILDING_NUMBER", "B-STREET_NAME", "I-STREET_NAME",
	"B-SECONDARY_ADDRESS", "I-SECONDARY_ADDRESS",
	"B-CITY", "I-CITY", "B-STATE", "I-STATE", "B-ZIP_CODE", "I-ZIP_CODE",
}

var nerLabelToCat = map[string]Label{
	"GIVEN_NAME": LGivenName, "SURNAME": LSurname, "EMAIL": LEmail,
	"PHONE": LPhone, "URL": LURL, "TAX_ID": LTaxID,
	"BANK_ACCOUNT": LBankAccount, "ROUTING_NUMBER": LRoutingNumber,
	"GOVERNMENT_ID": LGovernmentID, "PASSPORT": LPassport,
	"DRIVERS_LICENSE": LDriversLicense, "BUILDING_NUMBER": LBuildingNumber,
	"STREET_NAME": LStreetName, "SECONDARY_ADDRESS": LSecondaryAddress,
	"CITY": LCity, "STATE": LState, "ZIP_CODE": LZipCode,
}

type nerEngine struct {
	mu       sync.Mutex
	rt       *ort.Runtime
	env      *ort.Env
	sess     *ort.Session
	tok      *WordPiece
	clsID    int
	sepID    int
	minutes  float64
	timeout  time.Duration
	minScore float64
}

var (
	nerMu       sync.Mutex
	nerEng      *nerEngine
	nerKey      string
	nerErr      error
	nerWarnOnce sync.Once
	// nerSem bounds in-flight inference to one (matching e.mu's serialization).
	nerSem = make(chan struct{}, 1)
)

func nerSingleton(modelPath, ortLib string, timeoutMs int, minScore float64) (*nerEngine, error) {
	// keyed on (model, lib): the old sync.Once pinned the first caller's
	// config and silently ignored every later one.
	key := modelPath + "\x00" + ortLib
	nerMu.Lock()
	defer nerMu.Unlock()
	if nerEng != nil && nerKey == key {
		return nerEng, nerErr
	}
	if nerEng != nil {
		// different key: a config hot-reload moved the model or the lib.
		// release the old native session/runtime instead of leaking one
		// loaded model per reload.
		nerEng.close()
		nerEng, nerKey = nil, ""
	}
	e := &nerEngine{timeout: time.Duration(timeoutMs) * time.Millisecond, minScore: minScore}
	if e.timeout <= 0 {
		e.timeout = 500 * time.Millisecond
	}
	if e.minScore <= 0 {
		e.minScore = 0.4
	}
	vocabPath := os.Getenv("RAMPART_VOCAB")
	if vocabPath == "" {
		vocabPath = modelDir(modelPath) + "/vocab.txt"
	}
	tok, err := LoadWordPiece(vocabPath)
	if err != nil {
		nerErr = err
		return nil, err
	}
	e.tok = tok
	e.clsID = tok.ID("[CLS]")
	e.sepID = tok.ID("[SEP]")
	lib := ortLib
	if lib == "" {
		lib = os.Getenv("ORT_LIB")
	}
	if lib == "" {
		nerErr = fmt.Errorf("pii ner: no onnxruntime lib (set ort_lib or ORT_LIB)")
		return nil, nerErr
	}
	rt, err := ort.NewRuntime(lib, 23)
	if err != nil {
		nerErr = err
		return nil, err
	}
	e.rt = rt
	env, err := rt.NewEnv("rampart", ort.LoggingLevelWarning)
	if err != nil {
		nerErr = err
		e.close()
		return nil, err
	}
	e.env = env
	sess, err := rt.NewSession(env, modelPath, nil)
	if err != nil {
		nerErr = err
		e.close()
		return nil, err
	}
	e.sess = sess
	nerEng, nerKey, nerErr = e, key, nil
	return nerEng, nerErr
}

// close releases the native handles, tolerating a partially built engine.
func (e *nerEngine) close() {
	if e.sess != nil {
		e.sess.Close()
		e.sess = nil
	}
	if e.env != nil {
		e.env.Close()
		e.env = nil
	}
	if e.rt != nil {
		e.rt.Close()
		e.rt = nil
	}
}

func modelDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}

func NerAvailable() bool { return true }

// NerDetector builds the model detector; nil on any load failure so the
// pipeline degrades to heuristics. session-safe: ort sessions are driven
// serially under mutex.
func NerDetector(modelPath, ortLib string, timeoutMs int, minScore float64) Detector {
	e, err := nerSingleton(modelPath, ortLib, timeoutMs, minScore)
	if err != nil || e == nil {
		// once: the caller builds a guard per session, so this would
		// otherwise repeat on every request.
		nerWarnOnce.Do(func() {
			log.Printf("WARN: pii ner unavailable, detection is heuristics-only: %v", err)
		})
		return nil
	}
	// one global mutex serializes all inference process-wide (ort
	// sessions are not concurrent-safe): throughput ceiling ~1/timeout
	// per NER call. documented, not fixed: sharding needs N sessions.
	return func(text string) []Span {
		ctx, cancel := context.WithTimeout(context.Background(), e.timeout)
		defer cancel()
		// one inference in flight process-wide (ort sessions are not
		// concurrent-safe). a timed-out call keeps running under nerSem, so
		// the slot is the back-pressure: a caller that cannot get it inside
		// its own timeout degrades to heuristics instead of queueing an
		// unbounded pile of orphans behind the timed-out one.
		select {
		case nerSem <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		type res struct{ spans []Span }
		ch := make(chan res, 1)
		// non-blocking send: the orphan never blocks on a full channel.
		go func() {
			defer func() { <-nerSem }()
			spans := e.infer(text)
			select {
			case ch <- res{spans}:
			default:
			}
		}()
		select {
		case r := <-ch:
			return r.spans
		case <-ctx.Done():
			return nil
		}
	}
}

func (e *nerEngine) infer(text string) []Span {
	// fold the original text: foldWithMap already lowercases, and lowering
	// first would shift every later offset (İ -> "i" changes byte length),
	// making starts/ends address the lowercased copy instead of raw.
	folded, starts, ends := foldWithMap(text)
	// hyphen->space fold like rampart detectNerWindow: hyphens split
	// entities in the vocab, spaces don't. length-preserving so the
	// starts/ends map stays valid.
	foldedHyph := strings.ReplaceAll(folded, "-", " ")
	toks := e.tok.Tokenize(foldedHyph)
	const budget = 500
	var out []Span
	for w0 := 0; w0 < len(toks); w0 += budget - 64 {
		w1 := w0 + budget
		if w1 > len(toks) {
			w1 = len(toks)
		}
		win := toks[w0:w1]
		spans := e.inferWindow(win, folded, starts, ends, text)
		out = append(out, spans...)
		if w1 == len(toks) {
			break
		}
	}
	return mergeSpans(out)
}

func (e *nerEngine) inferWindow(win []Tok, folded string, starts, ends []int, raw string) []Span {
	n := len(win) + 2
	ids := make([]int64, n)
	mask := make([]int64, n)
	tt := make([]int64, n)
	ids[0] = int64(e.clsID)
	mask[0] = 1
	for i, t := range win {
		ids[i+1] = int64(t.ID)
		mask[i+1] = 1
	}
	ids[n-1] = int64(e.sepID)
	mask[n-1] = 1
	e.mu.Lock()
	defer e.mu.Unlock()
	ctx := context.Background()
	inIDs, err := ort.NewTensorValue(e.rt, ids, []int64{1, int64(n)})
	if err != nil {
		return nil
	}
	defer inIDs.Close()
	inMask, err := ort.NewTensorValue(e.rt, mask, []int64{1, int64(n)})
	if err != nil {
		return nil
	}
	defer inMask.Close()
	inTT, err := ort.NewTensorValue(e.rt, tt, []int64{1, int64(n)})
	if err != nil {
		return nil
	}
	defer inTT.Close()
	res, err := e.sess.Run(ctx, map[string]*ort.Value{
		"input_ids": inIDs, "attention_mask": inMask, "token_type_ids": inTT,
	})
	if err != nil {
		return nil
	}
	// pick the logits tensor by name + shape, not map order: go map
	// iteration is random and a second output would misread silently. close
	// every value as it is visited: a break would leak the rest.
	var logits []float32
	var shape []int64
	named := false
	for name, v := range res {
		l, sh, gerr := ort.GetTensorData[float32](v)
		v.Close()
		if gerr != nil || len(sh) != 3 || int(sh[1]) != n {
			continue
		}
		if !named && strings.Contains(strings.ToLower(name), "logit") {
			logits, shape, named = l, sh, true
			continue
		}
		if logits == nil {
			logits, shape = l, sh
		}
	}
	if len(shape) != 3 {
		return nil
	}
	nlab := int(shape[2])
	type cand struct {
		start, end int
		cat        Label
		score      float64
	}
	var cands []cand
	var cur *cand
	flush := func() {
		if cur != nil {
			cands = append(cands, *cur)
			cur = nil
		}
	}
	for i, t := range win {
		base := (1 + i) * nlab
		if base+nlab > len(logits) {
			break
		}
		best, bestV := 0, float32(math.Inf(-1))
		for l := 0; l < nlab && l < len(nerLabels); l++ {
			if logits[base+l] > bestV {
				bestV, best = logits[base+l], l
			}
		}
		// softmax for the winning class only (calibrated enough at 0.4 gate).
		sum := float32(0)
		for l := 0; l < nlab && l < len(nerLabels); l++ {
			sum += float32(math.Exp(float64(logits[base+l] - bestV)))
		}
		prob := 1 / float64(sum)
		lab := nerLabels[best]
		if lab == "O" || prob < e.minScore {
			flush()
			continue
		}
		bio := lab[:1]
		cat, ok := nerLabelToCat[lab[2:]]
		if !ok {
			flush()
			continue
		}
		// bounds-checked: folded offsets slice the raw body, and a
		// length-changing fold (U+0130 lowercases to 2 runes) would
		// otherwise misalign or panic.
		if t.Start < 0 || t.End > len(starts) || t.End > len(ends) || t.Start >= t.End {
			flush()
			continue
		}
		rs, re := starts[t.Start], ends[t.End-1]
		if rs < 0 || re > len(raw) || rs >= re {
			flush()
			continue
		}
		sub := strings.HasPrefix(t.Text, "##")
		// a B- tag always starts a new entity, even of the same
		// category: extending merges two adjacent names into one.
		if cur != nil && cur.cat == cat && (bio == "I" || (bio != "B" && sub)) {
			cur.end = re
			if prob < cur.score {
				cur.score = prob
			}
		} else {
			flush()
			cur = &cand{rs, re, cat, prob}
		}
	}
	flush()
	var out []Span
	for _, c := range cands {
		if c.end <= c.start {
			continue
		}
		out = append(out, Span{c.start, c.end, c.cat, c.score, "ner", raw[c.start:c.end]})
	}
	return out
}
