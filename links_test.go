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
			l := newLinks()
			defer l.cancel()
			for _, key := range tc.attachments {
				_, host, _ := strings.Cut(key, "@")
				l.out[key] = &outLink{to: host}
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
