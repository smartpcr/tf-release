//go:build e2e

package e2e

import (
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

// winrmChunkRaw duplicates the impl's fixed raw-bytes-per-chunk window
// (DESIGN §8.1) so this external suite can reproduce the golden chunk plan.
const winrmChunkRaw = 48000

// uploadChunk mirrors internal/transport.uploadChunk's JSON shape (no struct
// tags → exported field names) so the marshaled plan is byte-identical to the
// impl-generated golden.
type uploadChunk struct {
	Offset int
	Len    int
	First  bool
}

type chunkPlanEntry struct {
	Size   int           `json:"size"`
	Count  int           `json:"count"`
	Chunks []uploadChunk `json:"chunks"`
}

func planUploadChunks(size int) []uploadChunk {
	if size <= 0 {
		return nil
	}
	chunks := make([]uploadChunk, 0, (size+winrmChunkRaw-1)/winrmChunkRaw)
	for off := 0; off < size; off += winrmChunkRaw {
		n := winrmChunkRaw
		if off+n > size {
			n = size - off
		}
		chunks = append(chunks, uploadChunk{Offset: off, Len: n, First: off == 0})
	}
	return chunks
}

// encState carries values between steps of the EncodedCommand scenario.
type encState struct {
	got string
}

// chunkState carries values between steps of the chunk-math scenario.
type chunkState struct {
	sizes []int
}

func readGolden(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(testdataDir, name))
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
	if strings.Contains(prefix, "s3cr3t-value") {
		return fmt.Errorf("secret value leaked onto the visible command line")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("golden -EncodedCommand is not valid base64: %w", err)
	}
	if len(raw)%2 != 0 {
		return fmt.Errorf("UTF-16LE payload has odd byte length %d", len(raw))
	}
	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = uint16(raw[i*2]) | uint16(raw[i*2+1])<<8
	}
	decoded := string(utf16.Decode(u16))
	if !strings.Contains(decoded, "$env:GREETING='O''Brien';") {
		return fmt.Errorf("single-quote/semicolon escaping wrong; decoded payload:\n%s", decoded)
	}
	if !strings.Contains(decoded, "$env:TOKEN='s3cr3t-value';") {
		return fmt.Errorf("token env assignment missing semicolon form; decoded payload:\n%s", decoded)
	}
	gi := strings.Index(decoded, "$env:GREETING")
	ti := strings.Index(decoded, "$env:TOKEN")
	si := strings.Index(decoded, "Write-Output $env:GREETING")
	if !(gi < ti && ti < si) {
		return fmt.Errorf("env prefix not in sorted order before script body; decoded:\n%s", decoded)
	}
	return nil
}

func (s *chunkState) givenPayloadSizes() error {
	s.sizes = []int{0, 1, winrmChunkRaw, winrmChunkRaw + 1}
	return nil
}

func (s *chunkState) whenChunked() error {
	if len(s.sizes) == 0 {
		return fmt.Errorf("no payload sizes established")
	}
	return nil
}

func (s *chunkState) thenMatchesGolden(name string) error {
	plan := make([]chunkPlanEntry, 0, len(s.sizes))
	for _, sz := range s.sizes {
		chunks := planUploadChunks(sz)
		plan = append(plan, chunkPlanEntry{Size: sz, Count: len(chunks), Chunks: chunks})
	}
	got, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal chunk plan: %w", err)
	}
	got = append(got, '\n')
	want, err := readGolden(name)
	if err != nil {
		return fmt.Errorf("read golden %s: %w", name, err)
	}
	if string(got) != string(want) {
		return fmt.Errorf("chunk plan mismatch\n got:\n%s\nwant:\n%s", got, want)
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
