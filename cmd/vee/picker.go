package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

// SessionPickerCmd is the internal subcommand that shows an interactive
// mode picker with a prompt input, rendered inside tmux display-popup.
type SessionPickerCmd struct {
	VeePath string `required:"" type:"path" name:"vee-path"`
	Port    int    `short:"p" default:"2700" name:"port"`
}

type pickerMode struct {
	name      string
	indicator string
	desc      string
}

func (cmd *SessionPickerCmd) Run(args claudeArgs) error {
	if err := initModeRegistry(); err != nil {
		return err
	}

	var modes []pickerMode
	for _, name := range modeOrder {
		mode, ok := modeRegistry[name]
		if !ok {
			continue
		}
		modes = append(modes, pickerMode{
			name:      name,
			indicator: mode.Indicator,
			desc:      mode.Description,
		})
	}

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	selected := 0
	prompt := ""
	ephemeral := false
	canEphemeral := ephemeralAvailable()

	render := func() {
		var sb strings.Builder
		sb.WriteString("\033[2J\033[H")

		sb.WriteString("\r\n  \033[1m\033[38;2;137;180;250mNew Session\033[0m\r\n\r\n")

		for i, m := range modes {
			if i == selected {
				sb.WriteString("  \033[38;2;137;180;250m>\033[0m ")
			} else {
				sb.WriteString("    ")
			}
			sb.WriteString(m.indicator)
			sb.WriteString(" \033[1m")
			sb.WriteString(m.name)
			sb.WriteString("\033[0m")
			sb.WriteString("  \033[2m")
			sb.WriteString(m.desc)
			sb.WriteString("\033[0m")
			sb.WriteString("\r\n")
		}

		// Ephemeral toggle (only shown when available)
		if canEphemeral {
			sb.WriteString("\r\n  ")
			if ephemeral {
				sb.WriteString("\033[38;2;249;226;175m📦 Ephemeral\033[0m")
			} else {
				sb.WriteString("\033[2m   Local\033[0m")
			}
			sb.WriteString("\r\n")
		}

		sb.WriteString("\r\n  \033[2mPrompt:\033[0m ")
		sb.WriteString(prompt)
		sb.WriteString("\033[s") // save cursor position

		sb.WriteString("\r\n\r\n  \033[2m")
		if canEphemeral {
			sb.WriteString("C-n/C-p select  C-e ephemeral  Enter confirm  Esc cancel")
		} else {
			sb.WriteString("C-n/C-p select mode  Enter confirm  Esc cancel")
		}
		sb.WriteString("\033[0m\r\n")

		sb.WriteString("\033[u\033[?25h") // restore cursor + show it

		fmt.Print(sb.String())
	}

	render()

	buf := make([]byte, 32)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return err
		}

		input := buf[:n]

		if len(input) == 1 {
			switch input[0] {
			case 27: // Esc
				fmt.Print("\033[?25h")
				return nil
			case 10, 13: // Enter (LF or CR)
				fmt.Print("\033[?25h")
				term.Restore(int(os.Stdin.Fd()), oldState)
				return cmd.createSession(modes[selected].name, prompt, ephemeral, args)
			case 5: // C-e
				if canEphemeral {
					ephemeral = !ephemeral
				}
			case 14: // C-n
				if selected < len(modes)-1 {
					selected++
				}
			case 16: // C-p
				if selected > 0 {
					selected--
				}
			case 127, 8: // Backspace
				if len(prompt) > 0 {
					prompt = prompt[:len(prompt)-1]
				}
			case 23: // C-w — delete last word
				i := len(prompt)
				for i > 0 && prompt[i-1] == ' ' {
					i--
				}
				for i > 0 && prompt[i-1] != ' ' {
					i--
				}
				prompt = prompt[:i]
			case 21: // C-u — clear prompt
				prompt = ""
			default:
				if input[0] >= 32 && input[0] < 127 {
					prompt += string(input[0])
				}
			}
		} else if len(input) == 3 && input[0] == 27 && input[1] == 91 {
			switch input[2] {
			case 65: // Up
				if selected > 0 {
					selected--
				}
			case 66: // Down
				if selected < len(modes)-1 {
					selected++
				}
			}
		} else if len(input) == 2 && input[0] == 27 {
			// Esc followed by another char — treat as Esc
			fmt.Print("\033[?25h")
			return nil
		}

		render()
	}
}

func (cmd *SessionPickerCmd) createSession(mode, prompt string, ephemeral bool, args claudeArgs) error {
	veeBinary, err := os.Executable()
	if err != nil {
		return err
	}

	var cmdParts []string
	cmdParts = append(cmdParts, veeBinary)
	cmdParts = append(cmdParts, "_new-pane")
	cmdParts = append(cmdParts, "--vee-path", cmd.VeePath)
	cmdParts = append(cmdParts, "--port", fmt.Sprintf("%d", cmd.Port))
	cmdParts = append(cmdParts, "--mode", mode)
	if prompt != "" {
		cmdParts = append(cmdParts, "--prompt", prompt)
	}
	if ephemeral {
		cmdParts = append(cmdParts, "--ephemeral")
	}

	if len(args) > 0 {
		cmdParts = append(cmdParts, "--")
		cmdParts = append(cmdParts, []string(args)...)
	}

	execCmd := exec.Command(cmdParts[0], cmdParts[1:]...)
	execCmd.Stdout = os.Stdout
	execCmd.Stderr = os.Stderr
	return execCmd.Run()
}
