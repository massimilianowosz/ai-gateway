package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
)

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiIndigo = "\x1b[38;2;34;178;176m"
	ansiGreen  = "\x1b[38;5;114m"
	ansiRed    = "\x1b[38;5;203m"
	ansiYellow = "\x1b[38;5;220m"
	ansiBlue   = "\x1b[38;5;111m"
)

func paint(code, s string) string {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return s
	}
	return code + s + ansiReset
}

func bold(s string) string   { return paint(ansiBold, s) }
func dim(s string) string    { return paint(ansiDim, s) }
func orange(s string) string { return paint(ansiIndigo, s) }
func green(s string) string  { return paint(ansiGreen, s) }
func red(s string) string    { return paint(ansiRed, s) }
func yellow(s string) string { return paint(ansiYellow, s) }
func blue(s string) string   { return paint(ansiBlue, s) }

func printWelcome(w io.Writer, version, baseURL string, hasKey bool) {
	// Same logo used by the gateway server
	logo := orange("    ███╗   ███╗ ██████╗ ██████╗ ███████╗██╗") + "\n" +
		orange("    ████╗ ████║██╔═══██╗██╔══██╗██╔════╝██║") + "\n" +
		orange("    ██╔████╔██║██║   ██║██║  ██║█████╗  ██║") + "\n" +
		orange("    ██║╚██╔╝██║██║   ██║██║  ██║██╔══╝  ██║") + "\n" +
		orange("    ██║ ╚═╝ ██║╚██████╔╝██████╔╝███████╗███████╗") + "\n" +
		orange("    ╚═╝     ╚═╝ ╚═════╝ ╚═════╝ ╚══════╝╚══════╝") + "\n" +
		orange("          ██╗  ██╗██╗██╗   ██╗███████╗") + "\n" +
		orange("          ██║  ██║██║██║   ██║██╔════╝") + "\n" +
		orange("          ███████║██║██║   ██║█████╗") + "\n" +
		orange("          ██╔══██║██║╚██╗ ██╔╝██╔══╝") + "\n" +
		orange("          ██║  ██║██║ ╚████╔╝ ███████╗") + "\n" +
		orange("          ╚═╝  ╚═╝╚═╝  ╚═══╝  ╚══════╝")
	fmt.Fprintln(w, logo)
	fmt.Fprintf(w, "\n    %s\n\n", dim("⚡ LLM Gateway — Fast. Open. Yours."))

	// Status line
	var statusParts []string
	statusParts = append(statusParts, dim("v"+version))

	statusParts = append(statusParts, gatewayStatus(baseURL))

	if hasKey {
		statusParts = append(statusParts, green("● key loaded"))
	} else {
		statusParts = append(statusParts, yellow("● no key"))
	}

	modelNames := configuredModelNames()
	if len(modelNames) == 0 {
		statusParts = append(statusParts, yellow("0 models configured"))
	} else {
		statusParts = append(statusParts, blue(fmt.Sprintf("%d model(s) configured", len(modelNames))))
	}

	fmt.Fprintf(w, "    %s\n", strings.Join(statusParts, "  │  "))
	fmt.Fprintf(w, "    %s %s\n", dim("config:"), ConfigFilePath())
	fmt.Fprintf(w, "    %s %s\n", dim("target:"), baseURL)
	if len(modelNames) > 0 {
		modelLine := strings.Join(modelNames, ", ")
		fmt.Fprintf(w, "    %s %s\n", dim("models:"), truncateVisible(modelLine, 56))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, dim(strings.Repeat("─", 64)))
	fmt.Fprintf(w, "    %s  init · status · models · keys · teams · test · help\n", orange("›"))
	fmt.Fprintln(w, dim(strings.Repeat("─", 64)))
	fmt.Fprintf(w, "    %s\n", dim("Output"))
	fmt.Fprintln(w)
}

func gatewayStatus(baseURL string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	client := NewClient(baseURL, "")
	if _, err := client.Health(ctx); err == nil {
		ready, readyErr := client.Ready(ctx)
		if readyErr == nil && ready.Status == "ready" {
			return green("● running") + " " + dim(fmt.Sprintf("(%d live models)", ready.Models))
		}
		return yellow("● running") + " " + dim("(not ready)")
	}

	pid, err := readPID()
	if err == nil && isProcessAlive(pid) {
		return yellow("● process alive") + " " + dim(fmt.Sprintf("(pid %d, API unreachable)", pid))
	}
	return dim("○ stopped")
}

func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if s[i] == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func visibleLen(s string) int {
	return utf8.RuneCountInString(stripANSI(s))
}

func truncateVisible(s string, maxLen int) string {
	plain := stripANSI(s)
	if visibleLen(plain) <= maxLen {
		return s
	}
	if maxLen <= 1 {
		return "…"
	}
	runes := []rune(plain)
	return string(runes[:maxLen-1]) + "…"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func isTerminal() bool {
	return (isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd()))
}
