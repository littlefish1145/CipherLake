package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var completionCmd = &cobra.Command{
	Use:   "completion [bash|zsh|fish|powershell]",
	Short: "Generate shell completion script",
	Long: `Generate shell completion script for cipherlakectl.

To load completions:

Bash:
  source <(cipherlakectl completion bash)

  # To load completions for each session, execute once:
  # Linux:
  cipherlakectl completion bash > /etc/bash_completion.d/cipherlakectl
  # macOS:
  cipherlakectl completion bash > /usr/local/etc/bash_completion.d/cipherlakectl

Zsh:
  # If shell completion is not already enabled in your environment,
  # you will need to enable it. You can execute the following once:
  echo "autoload -U compinit; compinit" >> ~/.zshrc

  # To load completions for each session, execute once:
  cipherlakectl completion zsh > "${fpath[1]}/_cipherlakectl"

  # You will need to start a new shell for this setup to take effect.

Fish:
  cipherlakectl completion fish | source

  # To load completions for each session, execute once:
  cipherlakectl completion fish > ~/.config/fish/completions/cipherlakectl.fish

PowerShell:
  cipherlakectl completion powershell | Out-String | Invoke-Expression

  # To load completions for every new session, run:
  cipherlakectl completion powershell > cipherlakectl.ps1
  # and source this file from your PowerShell profile.
`,
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
	RunE: func(cmd *cobra.Command, args []string) error {
		shell := args[0]
		switch shell {
		case "bash":
			return rootCmd.GenBashCompletion(os.Stdout)
		case "zsh":
			return rootCmd.GenZshCompletion(os.Stdout)
		case "fish":
			return rootCmd.GenFishCompletion(os.Stdout, true)
		case "powershell":
			return rootCmd.GenPowerShellCompletionWithDesc(os.Stdout)
		default:
			return fmt.Errorf("unsupported shell type: %s (valid: bash, zsh, fish, powershell)", shell)
		}
	},
	DisableFlagsInUseLine: true,
}

func init() {
	rootCmd.AddCommand(completionCmd)
}
