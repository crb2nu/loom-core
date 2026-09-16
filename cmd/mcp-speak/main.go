// mcp-speak gives agents a voice: it synthesizes text through the homelab
// tts.lan lane (Piper voices on CPU, Orpheus voices on the Radeon VII) and
// plays the audio on the machine running this server. Fire-and-forget by
// default so a talking agent is never blocked on its own speech.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"

	"gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/env"
	"github.com/crb2nu/loom/pkg/httpclient"
	"github.com/crb2nu/loom/pkg/lifecycle"
	"github.com/crb2nu/loom/pkg/mcperror"
	"github.com/crb2nu/loom/pkg/mcpscaffold"
	"github.com/crb2nu/loom/pkg/validate"
)

var version = "0.1.0"

// Known voices by engine, mirrored from the tts-lane service. list_voices
// queries the live lane; this map only drives documentation strings.
const voiceHelp = "piper (utility): alan, amy. orpheus (expressive, GPU): tara, leah, jess, leo, dan, mia, zac, zoe."

type speakServer struct {
	ttsBase      string
	player       string
	defaultVoice string
	client       *httpclient.Client
	synthesizer  speechSynthesizer
	playback     audioPlayer
}

type speechSynthesizer interface {
	Synthesize(context.Context, string, string) ([]byte, error)
}

type audioPlayer interface {
	Start([]byte) (playback, error)
}

type playback interface {
	Wait() error
}

type laneSynthesizer struct {
	base   string
	client *httpclient.Client
}

type commandPlayer struct{ command string }

type commandPlayback struct {
	cmd  *exec.Cmd
	path string
	once sync.Once
	err  error
}

func main() {
	if err := lifecycle.RunWithSignals(context.Background(), run); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg := httpclient.DefaultConfig()
	// Orpheus synthesis of a long utterance can take tens of seconds warm.
	if cfg.Timeout < 120*time.Second {
		cfg.Timeout = 120 * time.Second
	}
	s := &speakServer{
		ttsBase:      strings.TrimRight(env.String("SPEAK_TTS_BASE", "http://tts.lan"), "/"),
		player:       env.String("SPEAK_PLAYER", "afplay"),
		defaultVoice: env.String("SPEAK_DEFAULT_VOICE", "alan"),
		client:       httpclient.New(cfg),
	}
	s.synthesizer = &laneSynthesizer{base: s.ttsBase, client: s.client}
	s.playback = &commandPlayer{command: s.player}

	srv, cleanup, err := mcpscaffold.NewServer(ctx, "mcp-speak", version,
		mcpscaffold.WithInstructions("Voice output through the homelab TTS lane. speak() synthesizes and plays audio on this machine's speakers; use it for short spoken updates, not long documents. "+voiceHelp),
	)
	if err != nil {
		return err
	}
	defer func() { _ = cleanup(ctx) }()

	srv.AddTracedTool(mcp.Tool{
		Name:        "speak",
		Description: "Synthesize text via the tts.lan lane and play it aloud on this machine. Keep utterances short and conversational. " + voiceHelp,
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"text": map[string]any{
					"type":        "string",
					"description": "What to say (1-1500 chars)",
				},
				"voice": map[string]any{
					"type":        "string",
					"description": "Voice name; picks the engine. Default from SPEAK_DEFAULT_VOICE (" + voiceHelp + ")",
				},
				"blocking": map[string]any{
					"type":        "boolean",
					"description": "Wait for playback to finish before returning (default false)",
				},
			},
			Required: []string{"text"},
		},
	}, s.handleSpeak)

	srv.AddTracedTool(mcp.Tool{
		Name:        "list_voices",
		Description: "List the voices the tts.lan lane currently serves.",
		InputSchema: mcp.InputSchema{Type: "object", Properties: map[string]any{}},
	}, s.handleListVoices)

	return srv.Run(ctx)
}

func (s *speakServer) handleSpeak(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	v := validate.NewArgs(args)
	text := v.Required("text")
	voice := v.String("voice", s.defaultVoice)
	blocking := v.Bool("blocking", false)
	if err := v.Validate(); err != nil {
		return mcp.ErrorResult(err), nil
	}
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 1500 {
		return mcp.ErrorResult(mcperror.InvalidParam("text", "must be 1-1500 characters")), nil
	}

	chunks := splitSentences(text)
	if len(chunks) == 1 {
		return s.speakSingle(ctx, text, voice, blocking)
	}
	return s.speakChunks(ctx, text, voice, blocking, chunks)
}

func (s *speakServer) synth() speechSynthesizer {
	if s.synthesizer != nil {
		return s.synthesizer
	}
	return &laneSynthesizer{base: s.ttsBase, client: s.client}
}

func (s *speakServer) playerImpl() audioPlayer {
	if s.playback != nil {
		return s.playback
	}
	return &commandPlayer{command: s.player}
}

func (s *speakServer) speakSingle(ctx context.Context, text, voice string, blocking bool) (*mcp.CallToolResult, error) {
	wav, err := s.synth().Synthesize(ctx, text, voice)
	if err != nil {
		return mcp.ErrorResult(err), nil
	}
	playing, err := s.playerImpl().Start(wav)
	if err != nil {
		return mcp.ErrorResult(err), nil
	}
	result := map[string]any{
		"voice":            voice,
		"chars":            len(text),
		"audio_bytes":      len(wav),
		"duration_seconds": wavDurationSeconds(wav),
	}
	if blocking {
		_ = playing.Wait()
		result["played"] = true
	} else {
		go func() { _ = playing.Wait() }()
		result["playing"] = true
	}
	return mcp.JSONResult(result)
}

func (s *speakServer) speakChunks(ctx context.Context, text, voice string, blocking bool, chunks []string) (*mcp.CallToolResult, error) {
	if blocking {
		return s.speakChunksBlocking(ctx, text, voice, chunks)
	}
	// Non-blocking contract: return as soon as the FIRST sentence is
	// audible; the rest of the pipeline detaches. Synthesizing every chunk
	// before returning would make the call block for roughly the whole
	// utterance — the exact latency this streaming path exists to remove.
	first, rest := chunks[0], chunks[1:]
	wav, err := s.synth().Synthesize(ctx, first, voice)
	if err != nil {
		return mcp.ErrorResult(err), nil
	}
	current, err := s.playerImpl().Start(wav)
	if err != nil {
		return mcp.ErrorResult(err), nil
	}
	// The tool-call ctx dies when this handler returns; the detached tail
	// must not inherit that cancellation or chunk 2+ synthesis is killed
	// mid-flight.
	tail := context.WithoutCancel(ctx)
	go func() {
		for _, chunk := range rest {
			next, err := s.synth().Synthesize(tail, chunk, voice)
			if err != nil {
				fmt.Fprintf(os.Stderr, "mcp-speak: streaming tail aborted: %v\n", err)
				_ = current.Wait()
				return
			}
			_ = current.Wait()
			if current, err = s.playerImpl().Start(next); err != nil {
				fmt.Fprintf(os.Stderr, "mcp-speak: streaming tail aborted: %v\n", err)
				return
			}
		}
		_ = current.Wait()
	}()
	return mcp.JSONResult(map[string]any{
		"voice": voice, "chars": len(text), "streaming": true,
		"chunks": len(chunks), "audio_bytes": len(wav),
		// First chunk only — the tail is still synthesizing by design.
		"duration_seconds": wavDurationSeconds(wav),
		"playing":          true,
	})
}

func (s *speakServer) speakChunksBlocking(ctx context.Context, text, voice string, chunks []string) (*mcp.CallToolResult, error) {
	var totalBytes int
	var totalDuration float64
	var current playback
	for _, chunk := range chunks {
		wav, err := s.synth().Synthesize(ctx, chunk, voice)
		if err != nil {
			if current != nil {
				_ = current.Wait()
			}
			return mcp.ErrorResult(err), nil
		}
		totalBytes += len(wav)
		totalDuration += wavDurationSeconds(wav)
		if current != nil {
			_ = current.Wait()
		}
		current, err = s.playerImpl().Start(wav)
		if err != nil {
			return mcp.ErrorResult(err), nil
		}
	}
	_ = current.Wait()
	return mcp.JSONResult(map[string]any{
		"voice": voice, "chars": len(text), "audio_bytes": totalBytes,
		"duration_seconds": totalDuration, "chunks": len(chunks),
		"played": true,
	})
}

func (l *laneSynthesizer) Synthesize(ctx context.Context, text, voice string) ([]byte, error) {
	body := fmt.Sprintf(`{"input":%q,"voice":%q}`, text, voice)
	resp, err := l.client.Post(ctx, l.base+"/v1/audio/speech", "application/json", strings.NewReader(body))
	if err != nil {
		return nil, mcperror.WrapAPI("tts-lane", err)
	}
	defer func() { _ = resp.Body.Close() }()
	wav, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, mcperror.WrapAPI("tts-lane", err)
	}
	if resp.StatusCode != 200 {
		return nil, mcperror.APIError("tts-lane", resp.StatusCode, string(wav[:min(len(wav), 300)]))
	}
	return wav, nil
}

func (p *commandPlayer) Start(wav []byte) (playback, error) {
	f, err := os.CreateTemp("", "mcp-speak-*.wav")
	if err != nil {
		return nil, fmt.Errorf("temp file: %w", err)
	}
	path := f.Name()
	if _, err = f.Write(wav); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("write wav: %w", err)
	}
	_ = f.Close()
	cmd := exec.CommandContext(context.Background(), p.command, path)
	if err = cmd.Start(); err != nil {
		_ = os.Remove(path)
		return nil, mcperror.NotConfigured("SPEAK_PLAYER", fmt.Sprintf("player %q failed to start: %v", p.command, err))
	}
	return &commandPlayback{cmd: cmd, path: path}, nil
}

func (p *commandPlayback) Wait() error {
	p.once.Do(func() {
		p.err = p.cmd.Wait()
		_ = os.Remove(p.path)
	})
	return p.err
}

const shortSentenceLength = 40

func splitSentences(text string) []string {
	var raw []string
	start := 0
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		boundary := r == '.' || r == '!' || r == '?' || r == '\n'
		if !boundary {
			continue
		}
		j := i + 1
		for j < len(runes) && (runes[j] == '.' || runes[j] == '!' || runes[j] == '?') {
			j++
		}
		if r != '\n' && j < len(runes) && !unicode.IsSpace(runes[j]) {
			continue
		}
		if part := strings.TrimSpace(string(runes[start:j])); part != "" {
			raw = append(raw, part)
		}
		start = j
		i = j - 1
	}
	if part := strings.TrimSpace(string(runes[start:])); part != "" {
		raw = append(raw, part)
	}
	if len(raw) < 2 {
		return []string{text}
	}

	merged := make([]string, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		part := raw[i]
		if len([]rune(part)) < shortSentenceLength {
			if len(merged) > 0 {
				merged[len(merged)-1] += " " + part
				continue
			}
			if i+1 < len(raw) {
				raw[i+1] = part + " " + raw[i+1]
				continue
			}
		}
		merged = append(merged, part)
	}
	if len(merged) < 2 {
		return []string{text}
	}
	return merged
}

func (s *speakServer) handleListVoices(ctx context.Context, args map[string]any) (*mcp.CallToolResult, error) {
	resp, err := s.client.Get(ctx, s.ttsBase+"/v1/audio/voices")
	if err != nil {
		return mcp.ErrorResult(mcperror.WrapAPI("tts-lane", err)), nil
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return mcp.ErrorResult(mcperror.WrapAPI("tts-lane", err)), nil
	}
	if resp.StatusCode != 200 {
		return mcp.ErrorResult(mcperror.APIError("tts-lane", resp.StatusCode, string(payload))), nil
	}
	return mcp.TextResult(string(payload)), nil
}

// wavDurationSeconds derives playback length from a 16-bit PCM WAV header.
// Returns 0 for anything it does not recognize — duration is advisory.
func wavDurationSeconds(wav []byte) float64 {
	if len(wav) < 44 || !bytes.HasPrefix(wav, []byte("RIFF")) {
		return 0
	}
	sampleRate := binary.LittleEndian.Uint32(wav[24:28])
	byteRate := binary.LittleEndian.Uint32(wav[28:32])
	if byteRate == 0 || sampleRate == 0 {
		return 0
	}
	dataLen := len(wav) - 44
	return float64(dataLen) / float64(byteRate)
}
