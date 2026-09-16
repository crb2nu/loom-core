package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/httpclient"
)

func TestSplitSentences(t *testing.T) {
	longA := "This opening sentence is deliberately longer than forty characters."
	longB := "This closing sentence is also deliberately longer than forty characters."
	tests := []struct {
		name string
		text string
		want []string
	}{
		{"single passthrough", "  hello there  ", []string{"  hello there  "}},
		{"sentences", longA + " " + longB, []string{longA, longB}},
		{"punctuation cluster", longA + "?!  " + longB, []string{longA + "?!", longB}},
		{"no whitespace boundary", "example.com remains one sentence", []string{"example.com remains one sentence"}},
		{"newline", longA + "\n" + longB, []string{longA, longB}},
		{"short first merges forward", "Brief. " + longB, []string{"Brief. " + longB}},
		{"short last merges backward", longA + " Short.", []string{longA + " Short."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitSentences(tt.text)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("splitSentences(%q) = %#v, want %#v", tt.text, got, tt.want)
			}
		})
	}
}

func testWAV(seconds float64) []byte {
	const rate = 24000
	samples := int(seconds * rate)
	data := make([]byte, 44+samples*2)
	copy(data, "RIFF")
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[24:], rate)
	binary.LittleEndian.PutUint32(data[28:], rate*2) // 16-bit mono
	return data
}

func TestWavDurationSeconds(t *testing.T) {
	if d := wavDurationSeconds(testWAV(2.0)); d < 1.99 || d > 2.01 {
		t.Fatalf("expected ~2.0s, got %v", d)
	}
	if d := wavDurationSeconds([]byte("not a wav")); d != 0 {
		t.Fatalf("expected 0 for garbage, got %v", d)
	}
}

func newTestServer(t *testing.T, ttsBase string) *speakServer {
	t.Helper()
	return &speakServer{
		ttsBase:      ttsBase,
		player:       "true", // /usr/bin/true: starts, exits 0, plays nothing
		defaultVoice: "alan",
		client:       httpclient.NewDefault(),
	}
}

func TestHandleSpeak(t *testing.T) {
	var gotBody map[string]any
	lane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/speech" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(testWAV(1.0))
	}))
	defer lane.Close()

	s := newTestServer(t, lane.URL)
	res, err := s.handleSpeak(context.Background(), map[string]any{
		"text": "hello there", "voice": "tara", "blocking": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if gotBody["voice"] != "tara" || gotBody["input"] != "hello there" {
		t.Fatalf("lane got wrong payload: %v", gotBody)
	}
	// JSONResult renders the loom compact structured encoding, not JSON.
	text := res.Content[0].Text
	if !strings.Contains(text, "played: true") {
		t.Fatalf("expected played: true in %q", text)
	}
	if !strings.Contains(text, "duration_seconds: 1") {
		t.Fatalf("expected ~1s duration in %q", text)
	}
}

func TestHandleSpeakValidation(t *testing.T) {
	s := newTestServer(t, "http://unused.invalid")
	res, err := s.handleSpeak(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected error for missing text")
	}
	res, err = s.handleSpeak(context.Background(), map[string]any{"text": strings.Repeat("x", 2000)})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected error for oversized text")
	}
}

func TestHandleSpeakLaneError(t *testing.T) {
	lane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"orpheus lane unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer lane.Close()

	s := newTestServer(t, lane.URL)
	res, err := s.handleSpeak(context.Background(), map[string]any{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected error result for 503 lane")
	}
}

type fakeSynthesizer struct {
	mu     sync.Mutex
	calls  []string
	wavs   map[string][]byte
	errFor string
}

func (f *fakeSynthesizer) Synthesize(_ context.Context, text, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, text)
	if text == f.errFor {
		return nil, errors.New("synthesis failed")
	}
	return f.wavs[text], nil
}

type fakePlayer struct {
	mu      sync.Mutex
	started []int
	waits   []chan struct{}
}

func (f *fakePlayer) Start(wav []byte) (playback, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan struct{})
	f.started = append(f.started, len(wav))
	f.waits = append(f.waits, ch)
	return fakePlayback{done: ch}, nil
}

type fakePlayback struct{ done <-chan struct{} }

func (p fakePlayback) Wait() error { <-p.done; return nil }

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for pipeline state")
}

func TestSpeakChunksOrderedOneAheadAndAggregated(t *testing.T) {
	chunks := []string{"first", "second", "third"}
	synth := &fakeSynthesizer{wavs: map[string][]byte{
		"first": testWAV(1), "second": testWAV(2), "third": testWAV(3),
	}}
	player := &fakePlayer{}
	s := &speakServer{synthesizer: synth, playback: player}
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		res, _ := s.speakChunks(context.Background(), strings.Join(chunks, " "), "tara", true, chunks)
		done <- res
	}()

	waitFor(t, func() bool {
		synth.mu.Lock()
		defer synth.mu.Unlock()
		player.mu.Lock()
		defer player.mu.Unlock()
		return len(synth.calls) == 2 && len(player.started) == 1
	})
	player.mu.Lock()
	close(player.waits[0])
	player.mu.Unlock()
	waitFor(t, func() bool {
		synth.mu.Lock()
		defer synth.mu.Unlock()
		player.mu.Lock()
		defer player.mu.Unlock()
		return len(synth.calls) == 3 && len(player.started) == 2
	})
	player.mu.Lock()
	close(player.waits[1])
	player.mu.Unlock()
	waitFor(t, func() bool { player.mu.Lock(); defer player.mu.Unlock(); return len(player.started) == 3 })
	select {
	case <-done:
		t.Fatal("blocking call returned before final playback")
	default:
	}
	player.mu.Lock()
	close(player.waits[2])
	player.mu.Unlock()
	res := <-done
	if got := res.Content[0].Text; !strings.Contains(got, "duration_seconds: 6") || !strings.Contains(got, "played: true") {
		t.Fatalf("unexpected aggregate result: %q", got)
	}
}

func TestSpeakChunksSynthesisErrorWaitsAndStops(t *testing.T) {
	chunks := []string{"first", "second", "third"}
	synth := &fakeSynthesizer{wavs: map[string][]byte{"first": testWAV(1)}, errFor: "second"}
	player := &fakePlayer{}
	s := &speakServer{synthesizer: synth, playback: player}
	done := make(chan *mcp.CallToolResult, 1)
	go func() { res, _ := s.speakChunks(context.Background(), "text", "alan", true, chunks); done <- res }()
	waitFor(t, func() bool { synth.mu.Lock(); defer synth.mu.Unlock(); return len(synth.calls) == 2 })
	select {
	case <-done:
		t.Fatal("returned before active playback cleanup")
	default:
	}
	player.mu.Lock()
	close(player.waits[0])
	player.mu.Unlock()
	res := <-done
	if !res.IsError {
		t.Fatal("expected synthesis error result")
	}
	if len(synth.calls) != 2 {
		t.Fatalf("synthesized after error: %v", synth.calls)
	}
}

func TestSpeakChunksNonBlockingReturnsAfterFirstChunkStarts(t *testing.T) {
	chunks := []string{"first", "second"}
	wav1 := testWAV(1)
	synth := &fakeSynthesizer{wavs: map[string][]byte{"first": wav1, "second": testWAV(2)}}
	player := &fakePlayer{}
	s := &speakServer{synthesizer: synth, playback: player}

	// The call itself must come back while chunk 1 is still PLAYING and
	// chunk 2 has not started — that is the whole point of streaming.
	res, err := s.speakChunks(context.Background(), "first second", "alan", false, chunks)
	if err != nil {
		t.Fatal(err)
	}
	got := res.Content[0].Text
	if !strings.Contains(got, "playing: true") || !strings.Contains(got, "streaming: true") ||
		!strings.Contains(got, "chunks: 2") || !strings.Contains(got, "duration_seconds: 1") ||
		!strings.Contains(got, fmt.Sprintf("audio_bytes: %d", len(wav1))) {
		t.Fatalf("unexpected streaming result: %q", got)
	}
	player.mu.Lock()
	if len(player.started) != 1 {
		player.mu.Unlock()
		t.Fatalf("second chunk started before first finished: %d", len(player.started))
	}
	close(player.waits[0])
	player.mu.Unlock()

	// The detached tail keeps strict order: chunk 2 plays only after 1 ends.
	waitFor(t, func() bool { player.mu.Lock(); defer player.mu.Unlock(); return len(player.started) == 2 })
	player.mu.Lock()
	close(player.waits[1])
	player.mu.Unlock()
}
