package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ctlCmd sends one raw operation with a JSON object argument and prints the raw reply.
type ctlCmd struct {
	Op     string `arg:"" name:"OP" help:"operation to send, one of api.Ops"`
	Object string `arg:"" optional:"" name:"JSON" help:"arguments as a JSON object (default {})"`
}

func (cmd *ctlCmd) Run(c *cli) error {
	if cmd.Op == "subscribe" {
		return c.subscribe()
	}
	raw := json.RawMessage(`{}`)
	if cmd.Object != "" {
		raw = json.RawMessage(cmd.Object)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return errors.New("invalid: arguments must be a JSON object")
	}
	reply, err := c.call(cmd.Op, raw)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, string(reply))
	return nil
}

// subscribeCmd streams daemon events until interrupted.
type subscribeCmd struct{}

func (cmd *subscribeCmd) Run(c *cli) error {
	return c.subscribe()
}

// subscribe prints each event as one `{"event":…}` JSON line until interrupted or disconnected.
func (c *cli) subscribe() error {
	client, err := c.client()
	if err != nil {
		return err
	}
	events, err := client.Subscribe(c.ctx)
	if err != nil {
		return err
	}
	for {
		select {
		case <-c.ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				if c.ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("admin subscription closed")
			}
			fmt.Printf("{\"event\":%s}\n", event)
		}
	}
}
