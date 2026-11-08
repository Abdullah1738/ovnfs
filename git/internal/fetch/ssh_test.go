package fetch

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestPlainWords(t *testing.T) {
	for script, want := range map[string][]string{
		"ssh -o BatchMode=yes":     {"ssh", "-o", "BatchMode=yes"},
		"  plink\t-batch ":         {"plink", "-batch"},
		"/usr/bin/ssh -p 22 -4 -x": {"/usr/bin/ssh", "-p", "22", "-4", "-x"},
		"ssh -i ~/.ssh/key":        nil,
		`ssh -o "X Y"`:             nil,
		"ssh -o 'X Y'":             nil,
		"ssh $OPTS":                nil,
		"ssh `opts`":               nil,
		"ssh | tee":                nil,
		"ssh && x":                 nil,
		"ssh; x":                   nil,
		"ssh < in":                 nil,
		"ssh > out":                nil,
		"(ssh)":                    nil,
		"ssh *":                    nil,
		"ssh ?":                    nil,
		"ssh [ab]":                 nil,
		"ssh\nx":                   nil,
		`C:\bin\ssh.exe`:           nil,
		"":                         nil,
		"   ":                      nil,
	} {
		got, ok := plainWords(script)
		if ok != (want != nil) || !slices.Equal(got, want) {
			t.Errorf("%q: %q %v, want %q", script, got, ok, want)
		}
	}
}

func TestShellCommandWithoutShell(t *testing.T) {
	env := environ{lookup: func(k string) (string, bool) {
		if k == "PATH" {
			return t.TempDir(), true // no sh here
		}
		return "", false
	}}
	cmd, err := env.shellCommand(context.Background(), "ssh -o BatchMode=yes", "host", "git-upload-pack '/r'")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ssh", "-o", "BatchMode=yes", "host", "git-upload-pack '/r'"}; !slices.Equal(cmd.Args, want) {
		t.Fatalf("args %q, want %q", cmd.Args, want)
	}
	if _, err := env.shellCommand(context.Background(), "ssh -i ~/key", "host"); err == nil || !strings.Contains(err.Error(), "sh was not found") {
		t.Fatalf("shell syntax without a shell: %v", err)
	}
}

// Observed with git 2.56 and a logging GIT_SSH_COMMAND.
func TestSSHArgs(t *testing.T) {
	ep := &endpoint{user: "u", host: "h", port: "2222", path: "~al/r.git"}
	for _, tc := range []struct {
		v       sshVariant
		want    string
		sendEnv bool
	}{
		{variantSSH, "-o SendEnv=GIT_PROTOCOL -p 2222 u@h", true},
		{variantPlink, "-P 2222 u@h", false},
		{variantPutty, "-P 2222 u@h", false},
		{variantTortoise, "-batch -P 2222 u@h", false},
	} {
		args, sendEnv, err := sshArgs(tc.v, ep)
		if err != nil || strings.Join(args, " ") != tc.want || sendEnv != tc.sendEnv {
			t.Errorf("variant %d: %q %v %v, want %q %v", tc.v, args, sendEnv, err, tc.want, tc.sendEnv)
		}
	}
	if _, _, err := sshArgs(variantSimple, ep); err == nil || !strings.Contains(err.Error(), "does not support setting port") {
		t.Errorf("simple with port: %v", err)
	}
	ep.port = ""
	if args, sendEnv, err := sshArgs(variantSimple, ep); err != nil || strings.Join(args, " ") != "u@h" || sendEnv {
		t.Errorf("simple: %q %v %v", args, sendEnv, err)
	}
}

func TestResolveSSH(t *testing.T) {
	for _, tc := range []struct {
		env     map[string]string
		cfg     string
		shell   string
		prog    string
		variant sshVariant
	}{
		{map[string]string{}, "", "", "ssh", variantSSH},
		{map[string]string{"GIT_SSH_COMMAND": "ssh -o BatchMode=yes", "GIT_SSH": "plink"}, "", "ssh -o BatchMode=yes", "", variantSSH},
		{map[string]string{"GIT_SSH": `C:\Program Files\PuTTY\plink.exe`}, "", "", `C:\Program Files\PuTTY\plink.exe`, variantPlink},
		{map[string]string{"GIT_SSH_COMMAND": `"/opt/tortoiseplink" -v`}, "", `"/opt/tortoiseplink" -v`, "", variantTortoise},
		{map[string]string{"GIT_SSH_COMMAND": "/opt/putty"}, "", "/opt/putty", "", variantAuto},
		{map[string]string{"GIT_SSH_COMMAND": "/opt/plink", "GIT_SSH_VARIANT": "ssh"}, "[ssh]\nvariant = simple\n", "/opt/plink", "", variantSSH},
		{map[string]string{}, "[core]\nsshCommand = myssh\n[ssh]\nvariant = bogus\n", "myssh", "", variantSSH},
		{map[string]string{}, "[core]\nsshCommand = myssh\n[ssh]\nvariant = auto\n", "myssh", "", variantAuto},
	} {
		g := &gitTest{t: t}
		var cfg = g.config(tc.cfg)
		p := resolveSSH(cfg, environ{lookup: func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok }})
		if p.shell != tc.shell || p.prog != tc.prog || p.variant != tc.variant {
			t.Errorf("%v %q: got %+v", tc.env, tc.cfg, p)
		}
	}
}
