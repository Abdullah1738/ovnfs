package rev

import (
	"path/filepath"
	"testing"
)

func TestTrackingMatchesGit(t *testing.T) {
	g := newGit(t)
	base := [][2]string{
		{"remote.origin.url", "https://example.com/origin.git"},
		{"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
		{"remote.fork.url", "https://example.com/fork.git"},
		{"remote.fork.fetch", "+refs/heads/*:refs/remotes/fork/*"},
		{"branch.topic.remote", "origin"},
		{"branch.topic.merge", "refs/heads/main"},
	}
	cases := []struct {
		name string
		cfg  [][2]string
	}{
		{name: "defaults"},
		{name: "current", cfg: [][2]string{{"push.default", "current"}}},
		{name: "upstream", cfg: [][2]string{{"push.default", "upstream"}}},
		{name: "tracking", cfg: [][2]string{{"push.default", "tracking"}}},
		{name: "matching", cfg: [][2]string{{"push.default", "matching"}}},
		{name: "nothing", cfg: [][2]string{{"push.default", "nothing"}}},
		{name: "simple-same-name", cfg: [][2]string{{"branch.topic.merge", "refs/heads/topic"}}},
		{name: "pushdefault", cfg: [][2]string{{"remote.pushDefault", "fork"}}},
		{name: "pushdefault-same-name", cfg: [][2]string{{"remote.pushDefault", "fork"}, {"branch.topic.merge", "refs/heads/topic"}}},
		{name: "pushdefault-current", cfg: [][2]string{{"remote.pushDefault", "fork"}, {"push.default", "current"}}},
		{name: "pushdefault-upstream", cfg: [][2]string{{"remote.pushDefault", "fork"}, {"push.default", "upstream"}}},
		{name: "pushremote", cfg: [][2]string{{"remote.pushDefault", "origin"}, {"branch.topic.pushRemote", "fork"}}},
		{name: "pushremote-url", cfg: [][2]string{{"branch.topic.pushRemote", "https://example.com/fork.git"}}},
		{name: "pushurl", cfg: [][2]string{{"remote.fork.pushurl", "ssh://example.com/fork"}, {"branch.topic.pushRemote", "ssh://example.com/fork"}}},
		{name: "push-refspec", cfg: [][2]string{{"remote.origin.push", "refs/heads/topic:refs/heads/review/topic"}, {"remote.origin.fetch", "+refs/heads/review/*:refs/remotes/origin/r/*"}}},
		{name: "push-glob", cfg: [][2]string{{"remote.origin.push", "refs/heads/*:refs/heads/mine/*"}, {"push.default", "current"}}},
		{name: "push-refspec-miss", cfg: [][2]string{{"remote.origin.push", "refs/heads/other:refs/heads/other"}}},
		{name: "dot-remote", cfg: [][2]string{{"branch.topic.remote", "."}, {"branch.topic.merge", "refs/heads/main"}}},
		{name: "negative", cfg: [][2]string{{"remote.origin.fetch", "^refs/heads/main"}}},
		{name: "negative-glob", cfg: [][2]string{{"remote.origin.fetch", "^refs/heads/ma*"}}},
		{name: "explicit-fetch", cfg: [][2]string{{"remote.second.url", "x"}, {"remote.second.fetch", "refs/heads/main:refs/remotes/second/trunk"}, {"branch.topic.remote", "second"}}},
		{name: "two-globs", cfg: [][2]string{{"remote.origin.fetch", "+refs/heads/m*:refs/remotes/origin/mm*"}}},
		{name: "no-merge", cfg: [][2]string{{"branch.other.remote", "origin"}}},
		{name: "unknown-remote", cfg: [][2]string{{"branch.other.remote", "nowhere"}, {"branch.other.merge", "refs/heads/main"}}},
		{name: "multi-merge", cfg: [][2]string{{"branch.topic.merge", "refs/heads/topic"}}},
	}
	specs := []string{"topic@{upstream}", "topic@{push}", "other@{u}", "other@{push}", "@{u}", "@{push}", "main@{push}"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "r")
			g.run(t.TempDir(), "init", "-q", "-b", "topic", dir)
			tree := g.run(dir, "mktree")
			commit := g.runInput(dir, "c\n", "commit-tree", tree)
			for _, ref := range []string{"heads/topic", "heads/main", "heads/other", "remotes/origin/main",
				"remotes/origin/topic", "remotes/origin/other", "remotes/fork/topic", "remotes/fork/main",
				"remotes/origin/review/topic", "remotes/origin/r/topic", "remotes/origin/mine/topic",
				"remotes/second/trunk", "remotes/origin/mmain", "remotes/origin/mmtopic"} {
				g.run(dir, "update-ref", "refs/"+ref, commit)
			}
			for _, kv := range append(append([][2]string{}, base...), tc.cfg...) {
				g.run(dir, "config", "--add", kv[0], kv[1])
			}
			if tc.name == "multi-merge" {
				g.run(dir, "config", "--add", "branch.topic.merge", "refs/heads/main")
			}
			src := openSource(t, g, dir)
			compareNames(t, g, dir, src, specs)
		})
	}
}

func TestParseRefspec(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Refspec
		bad  bool
	}{
		{in: "+refs/heads/*:refs/remotes/origin/*", want: Refspec{Src: "refs/heads/*", Dst: "refs/remotes/origin/*", Force: true, Pattern: true}},
		{in: "refs/heads/main", want: Refspec{Src: "refs/heads/main"}},
		{in: "^refs/heads/secret*", want: Refspec{Src: "refs/heads/secret*", Negative: true, Pattern: true}},
		{in: "main:", want: Refspec{Src: "main"}},
		{in: ":refs/heads/gone", want: Refspec{Dst: "refs/heads/gone"}},
		{in: "refs/heads/*/x:refs/remotes/*/y", want: Refspec{Src: "refs/heads/*/x", Dst: "refs/remotes/*/y", Pattern: true}},
		{in: "refs/heads/*:refs/remotes/x", bad: true},
		{in: "refs/heads/x:refs/remotes/*", bad: true},
		{in: "refs/*/*:refs/x/*", bad: true},
		{in: "^a:b", bad: true},
		{in: "^", bad: true},
		{in: "refs/heads/a..b:x", bad: true},
	} {
		got, err := ParseRefspec(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseRefspec(%q) accepted", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseRefspec(%q) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
}
