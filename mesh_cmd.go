package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/stencil-hq/ovnfs/api"
)

// meshUsage lists the mesh subverbs.
const meshUsage = `usage:
  ovnfs mesh setup [--relay URL|off]   start a mesh here (once) and print a single-use join token
  ovnfs mesh join TOKEN                join the mesh a token invites to
  ovnfs mesh ls                        list the members
  ovnfs mesh rm MEMBER                 remove another machine
  ovnfs mesh leave                     take this machine out of its mesh`

// runMesh dispatches `ovnfs mesh SUBVERB`; each subverb is the op mesh.SUBVERB.
func runMesh(c *cli, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(meshUsage)
		if len(args) == 0 {
			return exitCode(2)
		}
		return nil
	}
	sub, rest := args[0], args[1:]
	fs := c.flags("mesh " + sub)
	switch sub {
	case "setup":
		relay := fs.String("relay", "", "relay URL for a new mesh, or off (default: the Stencil relay)")
		if _, err := c.parse(fs, rest, 0, 0); err != nil {
			return err
		}
		inv, err := call[api.MeshInvite](c, "mesh.setup", api.MeshSetupArgs{Relay: *relay})
		if err != nil || c.json {
			return err
		}
		fmt.Println(inv.Token)
		fmt.Fprintf(os.Stderr, "on the other machine: ovnfs mesh join '%s'\n(single use, valid until %s; mesh %s, %d member(s))\n", inv.Token, inv.Expires.Local().Format("15:04"), inv.Mesh.Mesh, len(inv.Mesh.Members))
		return nil
	case "join":
		pos, err := c.parse(fs, rest, 1, 1)
		if err != nil {
			return err
		}
		m, err := call[api.Mesh](c, "mesh.join", api.MeshJoinArgs{Token: pos[0]})
		if err != nil || c.json {
			return err
		}
		printMesh(m)
		return nil
	case "ls":
		if _, err := c.parse(fs, rest, 0, 0); err != nil {
			return err
		}
		m, err := call[api.Mesh](c, "mesh.ls", struct{}{})
		if err != nil || c.json {
			return err
		}
		printMesh(m)
		return nil
	case "rm":
		pos, err := c.parse(fs, rest, 1, 1)
		if err != nil {
			return err
		}
		m, err := call[api.Mesh](c, "mesh.rm", api.MeshRmArgs{Member: pos[0]})
		if err != nil || c.json {
			return err
		}
		printMesh(m)
		return nil
	case "leave":
		if _, err := c.parse(fs, rest, 0, 0); err != nil {
			return err
		}
		if _, err := call[api.Mesh](c, "mesh.leave", struct{}{}); err != nil || c.json {
			return err
		}
		fmt.Println("left the mesh")
		return nil
	}
	return errors.New(meshUsage)
}

// printMesh shows the mesh and its members, this machine marked with *.
func printMesh(m api.Mesh) {
	if m.Mesh == "" {
		fmt.Printf("in no mesh (node %s); run `ovnfs mesh setup` or `ovnfs mesh join TOKEN`\n", abbrev(m.Node, 10))
		return
	}
	relay := "relay off"
	switch {
	case m.Relay != "" && m.RelayUp:
		relay = "relay " + m.Relay + " (up)"
	case m.Relay != "":
		relay = "relay " + m.Relay + " (down)"
	}
	fmt.Printf("mesh %s  %s\n", m.Mesh, relay)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tNODE\tWAYS")
	for _, mem := range m.Members {
		name := mem.Name
		if mem.Self {
			name += " *"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, mem.Node[:10], strings.Join(mem.Ways, ", "))
	}
	tw.Flush()
}
