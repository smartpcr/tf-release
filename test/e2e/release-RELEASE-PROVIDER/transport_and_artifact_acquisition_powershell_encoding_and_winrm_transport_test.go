//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/cucumber/godog"

	"github.com/smartpcr/terraform-provider-labdeploy/internal/transport"
)

// testdataDir points at the impl-owned golden fixtures committed under
// internal/transport/testdata, resolved relative to this test file.
const testdataDir = "../../../internal/transport/testdata"

// goldenScript / goldenEnv mirror the KNOWN inputs behind the committed
// encoded_command.golden fixture (see internal/transport/encoding_test.go).
// The apostrophe in O'Brien locks in PowerShell single-quote escaping and the
// secret-looking TOKEN proves secrets ride only inside the base64 blob.
const goldenScript = "Write-Output $env:GREETING\nWrite-Output 'launch'\n"

func goldenEnv() map[string]string {
	return map[string]string{
		"GREETING": "O'Brien",
		"TOKEN":    "s3cr3t-value",
	}
}

func readGolden(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(testdataDir, name))
}

// ---------------------------------------------------------------------------
// EncodedCommand exact bytes — exercises production transport.EncodePS.
// ---------------------------------------------------------------------------

type encState struct {
	got string
}

func (s *encState) givenKnownScriptAndEnv() error {
	if _, ok := goldenEnv()["GREETING"]; !ok {
		return fmt.Errorf("golden env missing GREETING")
	}
	return nil
}

func (s *encState) whenEncoded() error {
	s.got = transport.EncodePS(goldenScript, goldenEnv())
	if s.got == "" {
		return fmt.Errorf("EncodePS returned empty command line")
	}
	return nil
}

func (s *encState) thenMatchesGolden(name string) error {
	want, err := readGolden(name)
	if err != nil {
		return fmt.Errorf("read golden %s: %w", name, err)
	}
	if s.got != string(want) {
		return fmt.Errorf("encoded command mismatch\n got: %q\nwant: %q", s.got, string(want))
	}
	return nil
}

func (s *encState) thenEscapingIsCorrect() error {
	const prefix = "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand "
	if !strings.HasPrefix(s.got, prefix) {
		return fmt.Errorf("missing expected flags prefix: %q", s.got)
	}
	b64 := strings.TrimPrefix(s.got, prefix)
	// Secret hygiene: the plaintext secret must never appear on the visible
	// command line. It only survives base64-encoded inside the -EncodedCommand
	// blob (which cannot contain the literal "s3cr3t-value" — the hyphen is not
	// in the base64 alphabet), so assert against the real command line s.got.
	if strings.Contains(s.got, "s3cr3t-value") {
		return fmt.Errorf("secret value leaked onto the visible command line: %q", s.got)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("golden -EncodedCommand is not valid base64: %w", err)
	}
	if len(raw)%2 != 0 {
		return fmt.Errorf("UTF-16LE payload has odd byte length %d", len(raw))
	}
	decoded := decodeUTF16LE(raw)
	if !strings.Contains(decoded, "$env:GREETING='O''Brien';") {
		return fmt.Errorf("single-quote/semicolon escaping wrong; decoded payload:\n%s", decoded)
	}
	if !strings.Contains(decoded, "$env:TOKEN='s3cr3t-value';") {
		return fmt.Errorf("token env assignment missing semicolon form; decoded payload:\n%s", decoded)
	}
	gi := strings.Index(decoded, "$env:GREETING")
	ti := strings.Index(decoded, "$env:TOKEN")
	si := strings.Index(decoded, "Write-Output $env:GREETING")
	if gi >= ti || ti >= si {
		return fmt.Errorf("env prefix not in sorted order before script body; decoded:\n%s", decoded)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Chunk math boundaries — drives the REAL production Upload path so the chunk
// windows are those the production code (planUploadChunks + uploadChunkScript)
// actually emits, not a duplicated local planner. The observed windows are then
// asserted against the committed golden fixture.
// ---------------------------------------------------------------------------

// observedChunk is one data-write the production Upload emitted, reconstructed
// from the PowerShell scripts it sent over the in-memory channel.
type observedChunk struct {
	Offset int  `json:"Offset"`
	Len    int  `json:"Len"`
	First  bool `json:"First"`
}

// chunkPlanEntry mirrors internal/transport's golden JSON view (see
// encoding_test.go): size, count and the ordered chunk windows.
type chunkPlanEntry struct {
	Size   int             `json:"size"`
	Count  int             `json:"count"`
	Chunks []observedChunk `json:"chunks"`
}

type chunkState struct {
	sizes []int
	plan  []chunkPlanEntry
}

// captureChannel is a fake WinRM command channel that records the data-carrying
// upload writes the production Upload emits, so the e2e suite can reconstruct
// the exact chunk plan without a live server. It decodes the PowerShell
// -EncodedCommand blobs the production Exec produces and extracts each chunk's
// base64 payload length and whether it created (WriteAllBytes) or appended.
type captureChannel struct {
	writes []observedChunk
	offset int
}

func decodeUTF16LE(raw []byte) string {
	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = uint16(raw[i*2]) | uint16(raw[i*2+1])<<8
	}
	return string(utf16.Decode(u16))
}

// run interprets exactly enough of the generated scripts to let Upload succeed
// while recording the data chunk windows.
func (c *captureChannel) run(_ context.Context, command, _ string) (string, string, int, error) {
	const prefix = "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand "
	script := command
	if strings.HasPrefix(command, prefix) {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(command, prefix))
		if err != nil {
			return "", "decode: " + err.Error(), 1, nil
		}
		script = decodeUTF16LE(raw)
	}

	switch {
	case strings.Contains(script, "New-Item"): // mkdir — no data chunk
		return "", "", 0, nil
	case strings.Contains(script, "FromBase64String("):
		// A data-carrying upload write. Extract the base64 payload to learn its
		// length, and classify create-vs-append to derive the First flag.
		b64 := extractSingleQuoted(script, "FromBase64String('")
		payload, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return "", "chunk decode: " + err.Error(), 1, nil
		}
		first := strings.Contains(script, "WriteAllBytes")
		c.writes = append(c.writes, observedChunk{Offset: c.offset, Len: len(payload), First: first})
		c.offset += len(payload)
		return "", "", 0, nil
	default:
		// Empty-file creation (New-Object byte[] 0) and any other control script
		// carry no data chunk.
		return "", "", 0, nil
	}
}

// extractSingleQuoted returns the text between the first single quote following
// marker and the next single quote. PowerShell base64 payloads never contain a
// quote so simple matching is sufficient here.
func extractSingleQuoted(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	end := strings.Index(rest, "'")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func (s *chunkState) givenPayloadSizes() error {
	s.sizes = []int{0, 1, 48000, 48001}
	return nil
}

// whenChunked drives the production transport.Upload for each payload size over
// an in-memory channel and records the chunk windows it actually emits.
func (s *chunkState) whenChunked() error {
	if len(s.sizes) == 0 {
		return fmt.Errorf("no payload sizes established")
	}
	ctx := context.Background()
	remote := `C:\deploy\app\payload.bin`
	s.plan = s.plan[:0]
	for _, size := range s.sizes {
		ch := &captureChannel{}
		tr := transport.NewWinRMWithRunner("host", 5986, ch.run)
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i % 251)
		}
		if err := tr.Upload(ctx, bytes.NewReader(payload), int64(size), remote); err != nil {
			return fmt.Errorf("production Upload(size=%d): %w", size, err)
		}
		// ch.writes stays nil for a zero-byte payload, which marshals to the
		// golden's "chunks": null; non-empty payloads carry the observed windows.
		s.plan = append(s.plan, chunkPlanEntry{Size: size, Count: len(ch.writes), Chunks: ch.writes})
	}
	return nil
}

func (s *chunkState) thenMatchesGolden(name string) error {
	if len(s.plan) == 0 {
		return fmt.Errorf("chunk plan was not produced by the production Upload path")
	}
	got, err := json.MarshalIndent(s.plan, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal chunk plan: %w", err)
	}
	got = append(got, '\n')
	want, err := readGolden(name)
	if err != nil {
		return fmt.Errorf("read golden %s: %w", name, err)
	}
	if string(got) != string(want) {
		return fmt.Errorf("production chunk plan mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
	return nil
}

func InitializeScenario_transport_and_artifact_acquisition_powershell_encoding_and_winrm_transport(ctx *godog.ScenarioContext) {
	enc := &encState{}
	chunk := &chunkState{}

	ctx.Step(`^the known PowerShell script and env containing "O'Brien"$`, enc.givenKnownScriptAndEnv)
	ctx.Step(`^the command is PowerShell-encoded$`, enc.whenEncoded)
	ctx.Step(`^the encoded bytes match the committed golden fixture "([^"]*)"$`, enc.thenMatchesGolden)
	ctx.Step(`^the single-quote escaping in the decoded payload is correct$`, enc.thenEscapingIsCorrect)

	ctx.Step(`^upload payloads of sizes 0, 1, 48000 and 48001 bytes$`, chunk.givenPayloadSizes)
	ctx.Step(`^the upload is chunked$`, chunk.whenChunked)
	ctx.Step(`^the chunk counts and offsets match the committed golden fixture "([^"]*)"$`, chunk.thenMatchesGolden)
}

func TestE2E_transport_and_artifact_acquisition_powershell_encoding_and_winrm_transport(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_transport_and_artifact_acquisition_powershell_encoding_and_winrm_transport,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"transport_and_artifact_acquisition_powershell_encoding_and_winrm_transport.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
