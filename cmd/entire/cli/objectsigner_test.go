package cli

import (
	"testing"

	format "github.com/go-git/go-git/v6/plumbing/format/config"
)

func TestHasCustomSSHSignProgram(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  *format.Config
		want bool
	}{
		{
			name: "nil raw config",
			raw:  nil,
			want: false,
		},
		{
			name: "empty raw config",
			raw:  format.New(),
			want: false,
		},
		{
			name: "custom program set",
			raw: func() *format.Config {
				c := format.New()
				c.Section("gpg").Subsection("ssh").SetOption("program", "/Applications/1Password.app/Contents/MacOS/op-ssh-sign")
				return c
			}(),
			want: true,
		},
		{
			name: "default ssh-keygen is not custom",
			raw: func() *format.Config {
				c := format.New()
				c.Section("gpg").Subsection("ssh").SetOption("program", "ssh-keygen")
				return c
			}(),
			want: false,
		},
		{
			name: "gpg section exists but no ssh.program",
			raw: func() *format.Config {
				c := format.New()
				c.Section("gpg").SetOption("format", "ssh")
				return c
			}(),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := hasCustomSSHSignProgram(tt.raw)
			if got != tt.want {
				t.Errorf("hasCustomSSHSignProgram() = %v, want %v", got, tt.want)
			}
		})
	}
}
