package main

import (
	"encoding/json"
	"fmt"
)

// runCtl sends one raw operation with a JSON object argument and prints the raw reply.
func runCtl(c *cli, args []string) error {
	pos, err := c.parse(c.flags("ctl"), args, 1, 2)
	if err != nil {
		return err
	}
	if pos[0] == "subscribe" {
		return c.subscribe()
	}
	raw := json.RawMessage(`{}`)
	if len(pos) == 2 {
		raw = json.RawMessage(pos[1])
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return fmt.Errorf("invalid: arguments must be a JSON object")
	}
	reply, err := c.call(pos[0], raw)
	if err != nil {
		return err
	}
	fmt.Println(string(reply))
	return nil
}

// runSubscribe streams daemon events until interrupted.
func runSubscribe(c *cli, args []string) error {
	if _, err := c.parse(c.flags("subscribe"), args, 0, 0); err != nil {
		return err
	}
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
