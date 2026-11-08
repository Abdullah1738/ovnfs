package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stencil-hq/ovnfs/api"
	"github.com/stencil-hq/ovnfs/checkout"
)

func TestPullSelectsAttachment(t *testing.T) {
	for _, tc := range []struct {
		name        string
		args        api.PullArgs
		attachments []string
		code        string
		message     string
	}{
		{name: "checkout required", code: "invalid", message: "needs a checkout"},
		{name: "no attachment", args: api.PullArgs{Checkout: "app"}, code: "not_found", message: "attach it first"},
		{name: "other checkout", args: api.PullArgs{Checkout: "app"}, attachments: []string{"application@a"}, code: "not_found"},
		{name: "unknown host", args: api.PullArgs{Checkout: "app", From: "missing"}, attachments: []string{"app@a"}, code: "not_found"},
		{name: "one disconnected attachment", args: api.PullArgs{Checkout: "app"}, attachments: []string{"app@a"}, code: "unavailable", message: "app@a is not connected"},
		{name: "ambiguous attachments", args: api.PullArgs{Checkout: "app"}, attachments: []string{"app@b", "app@a"}, code: "invalid", message: "a, b; choose --from HOST"},
		{name: "explicit disconnected host", args: api.PullArgs{Checkout: "app", From: "b"}, attachments: []string{"app@a", "app@b"}, code: "unavailable", message: "app@b is not connected"},
		{name: "ssh destination", args: api.PullArgs{Checkout: "app", From: "me@host"}, attachments: []string{"app@me@host", "app@other"}, code: "unavailable", message: "app@me@host is not connected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLinks(nil)
			defer l.cancel()
			for _, key := range tc.attachments {
				name, host, _ := strings.Cut(key, "@")
				l.out[key] = &outLink{checkout: name, to: host}
			}
			_, err := l.pull(context.Background(), tc.args)
			var he *checkout.Error
			if !errors.As(err, &he) || he.Code != tc.code {
				t.Fatalf("pull error = %v, want %s", err, tc.code)
			}
			if tc.message != "" && !strings.Contains(he.Message, tc.message) {
				t.Fatalf("pull error = %q, want %q", he.Message, tc.message)
			}
		})
	}
}

// TestPushReportsEachAttachment: push takes every attachment the filters
// match, in checkout and host order, and reports one that cannot push without
// failing the others.
func TestPushReportsEachAttachment(t *testing.T) {
	l := newLinks(nil)
	defer l.cancel()
	for _, key := range []string{"web@b", "app@me@b", "app@a"} {
		name, host, _ := strings.Cut(key, "@")
		l.out[key] = &outLink{checkout: name, to: host}
	}
	for _, tc := range []struct {
		args api.PushArgs
		want []string
	}{
		{args: api.PushArgs{}, want: []string{"app@a", "app@me@b", "web@b"}},
		{args: api.PushArgs{Checkout: "app"}, want: []string{"app@a", "app@me@b"}},
		{args: api.PushArgs{To: "b"}, want: []string{"web@b"}},
		{args: api.PushArgs{Checkout: "app", To: "me@b"}, want: []string{"app@me@b"}},
	} {
		r, err := l.push(context.Background(), tc.args)
		if err != nil {
			t.Fatalf("push %+v: %v", tc.args, err)
		}
		var got []string
		for _, p := range r.Pushed {
			got = append(got, p.Checkout+"@"+p.To)
			if !strings.Contains(p.Error, "not connected") {
				t.Errorf("push %s@%s error = %q, want not connected", p.Checkout, p.To, p.Error)
			}
		}
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("push %+v reached %v, want %v", tc.args, got, tc.want)
		}
	}
	_, err := l.push(context.Background(), api.PushArgs{Checkout: "app", To: "c"})
	var he *checkout.Error
	if !errors.As(err, &he) || he.Code != "not_found" || !strings.Contains(he.Message, "checkout app is not attached to c") {
		t.Fatalf("push to no attachment = %v", err)
	}
}
