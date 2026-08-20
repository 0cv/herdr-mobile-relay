package noecho

import (
	"strings"
	"testing"
)

func TestMatchRecognizesNoEchoPrompts(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "sudo",
			content: "$ sudo dnf upgrade\n[sudo] password for cv: ",
			want:    "[sudo] password for cv:",
		},
		{
			name:    "sudo styled",
			content: "\x1b[1m[sudo] password for cv:\x1b[0m\n",
			want:    "[sudo] password for cv:",
		},
		{
			name:    "ssh account",
			content: "cv@fedora's password:",
			want:    "cv@fedora's password:",
		},
		{
			name:    "bare password",
			content: "Connecting to vault\n\nPassword:\n\n\n",
			want:    "Password:",
		},
		{
			name:    "password for subject",
			content: "Password for vault:",
			want:    "Password for vault:",
		},
		{
			name:    "ssh key passphrase",
			content: "Enter passphrase for key '/home/cv/.ssh/id_ed25519':",
			want:    "Enter passphrase for key '/home/cv/.ssh/id_ed25519':",
		},
		{
			name:    "bare passphrase",
			content: "gpg: signing\nEnter passphrase:",
			want:    "Enter passphrase:",
		},
		{
			name:    "smartcard pin",
			content: "Enter PIN for 'PIV Card Holder pin (PIV_II)':",
			want:    "Enter PIN for 'PIV Card Holder pin (PIV_II)':",
		},
		{
			name:    "bare pin",
			content: "Enter PIN:",
			want:    "Enter PIN:",
		},
		{
			name:    "repeat password",
			content: "New password:\nRepeat password:",
			want:    "Repeat password:",
		},
		{
			name:    "verify password",
			content: "Verify password:",
			want:    "Verify password:",
		},
		{
			name:    "confirm password",
			content: "Confirm password:",
			want:    "Confirm password:",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt, ok := Match(tt.content)
			if !ok {
				t.Fatalf("Match(%q) did not recognize a no-echo prompt", tt.content)
			}
			if prompt != tt.want {
				t.Fatalf("prompt = %q, want %q", prompt, tt.want)
			}
		})
	}
}

func TestMatchRejectsPromptsThatAreNotSecretReads(t *testing.T) {
	longKey := strings.Repeat("d", 120)
	tests := []struct {
		name    string
		content string
	}{
		{name: "empty", content: ""},
		{name: "blank", content: "\n \n\t\n"},
		{
			name:    "over the display cap",
			content: "Enter passphrase for key '/home/cv/.ssh/" + longKey + "':",
		},
		{name: "question", content: "Do you want to change the password?"},
		{name: "yes no affordance", content: "Overwrite the stored password y/n"},
		{name: "spaced yes no affordance", content: "Reset the password  y / n"},
		{name: "policy prose", content: "the password policy requires 12 characters"},
		{name: "manager prose", content: "Password Manager:"},
		{name: "echoed value", content: "Password: hunter2"},
		{name: "unrelated prompt", content: "Enter your name:"},
		{name: "colonless pin instruction", content: "Then press the reader and Enter PIN"},
		{name: "shell prompt", content: "[sudo] password for cv:\n❯ "},
		{name: "already answered", content: "[sudo] password for cv:\nSorry, try again."},
		{name: "agent composer", content: "❯ Ask anything..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt, ok := Match(tt.content)
			if ok {
				t.Fatalf("Match(%q) reported a secret prompt %q", tt.content, prompt)
			}
			if prompt != "" {
				t.Fatalf("rejected content returned prompt %q", prompt)
			}
		})
	}
}
