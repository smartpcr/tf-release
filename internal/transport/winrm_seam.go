package transport

import "context"

// NewWinRMWithRunner builds a WinRM transport whose command channel is fn — a
// test seam — instead of a live winrm.Client, so acceptance/e2e suites can
// exercise the real Exec/Upload/Download methods (and, in particular, the
// production upload chunk math in Upload) against an in-memory PowerShell
// channel without a live WinRM server. fn has the same signature as
// winrm.Client.RunWithContextWithString.
func NewWinRMWithRunner(host string, port int, fn func(ctx context.Context, command, stdin string) (string, string, int, error)) Transport {
	return &winrmTransport{host: host, port: port, run: fn}
}
