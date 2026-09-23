// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/joncbenderkh/cc-proxy/internal/auth"
	"github.com/joncbenderkh/cc-proxy/internal/prompt"
)

// errWakeClaude ends a Stop hook run whose stderr carries a prompt for
// Claude. Claude Code wakes the session when an asyncRewake hook exits
// with wakeExitCode and shows it the hook's stderr.
var errWakeClaude = errors.New("wake Claude with a prompt")

const wakeExitCode = 2

// promptPreamble introduces a prompt from the web page to Claude, which
// receives it as a system reminder rather than as a user message.
const promptPreamble = "The user sent this prompt from the cc-proxy web page. Treat it as their next message and act on it:"

const maxStopHookInput = 1 << 20

func newHookCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Commands for Claude Code hooks",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(newStopHookCommand())
	return cmd
}

func newStopHookCommand() *cobra.Command {
	var uiURL, uiTokenFile string
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop hook: wait for the next prompt from the web page",
		Long: `Run as an asyncRewake Stop hook. Once Claude has finished responding,
it waits in the background for a prompt sent from the web page, then
exits with status 2 and writes the prompt to stderr, which wakes Claude
with it. It exits 0 without a prompt when the session goes idle again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			endpoint, err := stopHookEndpoint(uiURL)
			if err != nil {
				return err
			}
			if uiTokenFile == "" {
				if uiTokenFile, err = auth.DefaultTokenFile(); err != nil {
					return fmt.Errorf("locate --ui-token-file: %w", err)
				}
			}
			token, err := auth.LoadToken(uiTokenFile)
			if err != nil {
				return fmt.Errorf("invalid --ui-token-file: %w", err)
			}
			cmd.SilenceUsage = true
			text, err := waitForPrompt(cmd.Context(), endpoint, token, cmd.InOrStdin())
			if err != nil || text == "" {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%s\n\n%s\n", promptPreamble, text)
			cmd.SilenceErrors = true
			return errWakeClaude
		},
	}
	cmd.Flags().StringVar(&uiURL, "ui-url", "http://127.0.0.1:8788", "base URL of the cc-proxy web page")
	cmd.Flags().StringVar(&uiTokenFile, "ui-token-file", "", "file holding the web page login token (default <user config dir>/cc-proxy/ui-token)")
	return cmd
}

func stopHookEndpoint(raw string) (string, error) {
	base, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid --ui-url %q: %w", raw, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", fmt.Errorf("invalid --ui-url %q: scheme must be http or https", raw)
	}
	if base.Host == "" {
		return "", fmt.Errorf("invalid --ui-url %q: missing host", raw)
	}
	return base.JoinPath("hooks", "stop").String(), nil
}

// waitForPrompt relays the Stop hook input to the web page server and
// returns the prompt a viewer sent, or "" when the wait ended without one.
func waitForPrompt(ctx context.Context, endpoint, token string, stdin io.Reader) (string, error) {
	input, err := io.ReadAll(io.LimitReader(stdin, maxStopHookInput))
	if err != nil {
		return "", fmt.Errorf("read hook input: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(input))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return "", nil
	case http.StatusOK:
		var answer prompt.HookAnswer
		if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil {
			return "", fmt.Errorf("read prompt: %w", err)
		}
		return answer.Prompt, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("%s: %s: %s", endpoint, resp.Status, bytes.TrimSpace(body))
	}
}
